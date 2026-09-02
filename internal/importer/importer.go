package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// Sentinel import outcomes; the API maps them to job error classes.
var (
	ErrNotImportable = errors.New("importer: no recognized weights file (eligibility gate, 001 §8.1 — fail closed)")
	ErrNoFiles       = errors.New("importer: no input files")
)

// ImportOptions carries source provenance into annotations and the model
// config.
type ImportOptions struct {
	As         string // ns/name (required, validated by the store)
	HFRepo     string // original-case repo id, HF imports only
	HFRevision string // pinned commit SHA (or resolved branch name)
	Progress   func(done, total int)
}

// ArtifactInfo is one produced artifact with its full identity set.
type ArtifactInfo struct {
	Manifest    string
	Quant       string
	Record      string // distribution record digest
	PieceLayers string
}

// ImportResult reports what one import produced.
type ImportResult struct {
	IndexDigest string
	Members     []artifact.IndexMember // one per artifact group
	Artifacts   []ArtifactInfo
	Warnings    []string
	Skipped     int
}

// Import runs the full pipeline over the classified sources: eligibility
// gate → content ingest (verify-write into the CAS) → deterministic
// manifest/index/distribution construction → state update. The result is a
// pure function of the input bytes + options (convergence, 001 §7.5).
//
// ponytail: file contents are held in memory for digest+merkle
// computation; streaming merkle over the CAS write path is the TB-scale
// follow-up.
func Import(ctx context.Context, store *cas.Store, sources []Source, opts ImportOptions) (*ImportResult, error) {
	if len(sources) == 0 {
		return nil, ErrNoFiles
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	c, err := classify(sources)
	if err != nil {
		return nil, err
	}

	// Eligibility gate (fail closed): ≥ 1 recognized weights file.
	nWeights := 0
	for _, e := range c.entries {
		if e.kind == "weights.gguf" || e.kind == "weights.safetensors" {
			nWeights++
		}
	}
	if nWeights == 0 {
		return nil, ErrNotImportable
	}

	// ---- Group into artifacts (quant families) ----
	groups := map[string][]entry{}
	var groupOrder []string
	for _, e := range c.entries {
		if e.kind == "weights.gguf" || e.kind == "weights.safetensors" {
			g := e.group
			if _, ok := groups[g]; !ok {
				groupOrder = append(groupOrder, g)
			}
			groups[g] = append(groups[g], e)
		}
	}
	sort.Strings(groupOrder)

	// Progress totals must be consistent at job end (multi-artifact
	// included): one bump per entry ingest, one per sealed artifact, one
	// for the index — done == total on the last callback.
	total := len(c.entries) + len(groupOrder) + 1
	done := 0
	bump := func() {
		done++
		if opts.Progress != nil {
			opts.Progress(done, total)
		}
	}

	// ---- Content ingest: every entry's bytes land in the CAS first. ----
	blobs := map[string][]byte{} // manifest name → bytes (tar bytes are built here too)
	for i := range c.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := c.entries[i]
		switch e.kind {
		case "tokenizer":
			members := map[string][]byte{}
			for _, n := range e.tokenizer {
				b, err := readAll(sourceByName(sources, n))
				if err != nil {
					return nil, fmt.Errorf("tokenizer member %s: %w", n, err)
				}
				members[n] = b
			}
			blobs[e.name] = buildDeterministicTar(members)
		case "adapter":
			members := map[string][]byte{}
			for _, n := range e.adapterFiles {
				b, err := readAll(e.adapterSource[n])
				if err != nil {
					return nil, fmt.Errorf("adapter member %s: %w", n, err)
				}
				members[path_base(n)] = b // the §3.1 adapter tar is the pair (base names)
			}
			blobs[e.name] = buildDeterministicTar(members)
		default:
			b, err := readAll(e.src)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", e.name, err)
			}
			blobs[e.name] = b
		}
		bump()
	}

	// Shared, quant-agnostic companions join every artifact (§3.1.3):
	// aux, adapters. tokenizer/chat-template are GGUF-excluded (§8.5).
	var sharedAux, sharedAdapters, sharedTokenizer, sharedChat []entry
	for _, e := range c.entries {
		switch e.kind {
		case "weights.aux", "code", "runtime-config":
			sharedAux = append(sharedAux, e)
		case "adapter":
			sharedAdapters = append(sharedAdapters, e)
		case "tokenizer":
			sharedTokenizer = append(sharedTokenizer, e)
		case "chat-template":
			sharedChat = append(sharedChat, e)
		}
	}

	// B3: derived quants must be unique across groups (checked in the loop
	// below where the chain has full input).
	quantTaken := map[string]string{}

	res := &ImportResult{Warnings: c.warnings, Skipped: c.skipped}
	for _, g := range groupOrder {
		weights := groups[g]
		weightsFormat := "safetensors"
		if weights[0].kind == "weights.gguf" {
			weightsFormat = "gguf"
		}
		// Quant chain (§8.4) over the group's inputs.
		var safetensorsHeader []byte
		if weightsFormat == "safetensors" {
			safetensorsHeader = safetensorsHeaderBytes(blobs[weights[0].name])
		}
		warn := func(msg string) { res.Warnings = append(res.Warnings, msg) }
		quant := DeriveQuant(path_base(weights[0].name), c.upstreamConfig, safetensorsHeader, warn)
		if other, dup := quantTaken[quant]; dup {
			return nil, fmt.Errorf("importer: quant conflict: groups %q and %q both derive quant %q; index members are unique per quant (001 §5)", other, g, quant)
		}
		quantTaken[quant] = g

		family := deriveFamily(c.upstreamConfig, weights[0].name)
		ctxLen := int64(0)
		if cfg := parseUpstreamConfig(c.upstreamConfig); cfg != nil {
			ctxLen = cfg.MaxPositionalEmbeddings
		}

		files := []artifact.File{}
		// config first (canonical order is applied by SortFiles anyway).
		cfgBytes, err := buildModelConfig(opts, weightsFormat, quant, family, c.upstreamConfig, ctxLen, c.licenseSPDX)
		if err != nil {
			return nil, err
		}
		blobs["modelconfig.json"] = cfgBytes
		files = append(files, fileFromBytes("modelconfig.json", "config", blobs))

		for _, e := range weights {
			files = append(files, fileFromEntry(e, blobs))
		}
		for _, e := range sharedAux {
			files = append(files, fileFromEntry(e, blobs))
		}
		if weightsFormat != "gguf" { // §8.5: GGUF artifacts are self-contained
			for _, e := range sharedTokenizer {
				files = append(files, fileFromEntry(e, blobs))
			}
			for _, e := range sharedChat {
				files = append(files, fileFromEntry(e, blobs))
			}
		}
		for _, e := range sharedAdapters {
			f := fileFromEntry(e, blobs)
			f.AdapterType = "lora"
			files = append(files, f)
		}

		annotations := map[string]string{
			"io.shardr.import.specVersion": ClassificationSpecVersion,
			"io.shardr.import.skipped":     fmt.Sprintf("%d", c.skipped),
		}
		if len(res.Warnings) > 0 {
			annotations["io.shardr.import.warnings"] = strings.Join(res.Warnings, "; ")
		}
		if opts.HFRepo != "" {
			annotations["io.shardr.hf.repo"] = opts.HFRepo
			annotations["io.shardr.hf.revision"] = opts.HFRevision
		}
		if c.licenseName != "" {
			// §4: SPDX where possible, else raw upstream name only.
			annotations["io.shardr.license.name"] = c.licenseName
			if c.licenseSPDX != "" {
				annotations["io.shardr.license"] = c.licenseSPDX
			}
		}

		sealed, err := artifact.Seal(files, annotations, func(name string) ([]byte, error) {
			b, ok := blobs[name]
			if !ok {
				return nil, fmt.Errorf("no content for %s", name)
			}
			return b, nil
		})
		if err != nil {
			return nil, err
		}

		// CAS writes (verifying): content blobs, manifest, piece-layers,
		// distribution record.
		for _, f := range sealed.Manifest.Files {
			if err := store.Put(artifact.DigestHex(f.Digest), strings.NewReader(string(blobs[f.Name]))); err != nil {
				return nil, fmt.Errorf("cas put %s: %w", f.Name, err)
			}
		}
		if err := putCanonical(store, sealed.ManifestDigest, sealed.ManifestBytes); err != nil {
			return nil, err
		}
		if err := putCanonical(store, sealed.Torrent.PieceLayersDigest, sealed.Torrent.PieceLayersBencode); err != nil {
			return nil, err
		}
		if err := putCanonical(store, sealed.DistributionDigest, sealed.DistributionBytes); err != nil {
			return nil, err
		}
		bump()

		res.Members = append(res.Members, artifact.IndexMember{
			Manifest:      sealed.ManifestDigest,
			Quant:         quant,
			WeightsFormat: weightsFormat,
			Revision:      opts.HFRevision,
		})
		res.Artifacts = append(res.Artifacts, ArtifactInfo{
			Manifest:    sealed.ManifestDigest,
			Quant:       quant,
			Record:      sealed.DistributionDigest,
			PieceLayers: sealed.Torrent.PieceLayersDigest,
		})
	}

	// ---- Index merge + atomic state update (B6: under the namespace lock) ----
	mu := lockNamespace(opts.As)
	mu.Lock()
	defer mu.Unlock()
	members, err := existingMembers(store, opts.As)
	if err != nil {
		return nil, err
	}
	for _, m := range res.Members {
		members = artifact.UpsertMember(members, m)
	}
	idxBytes, idxDigest, err := artifact.SealIndex(members)
	if err != nil {
		return nil, err
	}
	// Symmetric guard: the newly merged members must pass the same rule
	// set the loaded ones did — nothing unvalidated is ever published.
	if verr := artifact.ValidateIndexMembers(members); verr != nil {
		return nil, fmt.Errorf("importer: merged index for %q fails validation: %w", opts.As, verr)
	}
	if err := putCanonical(store, idxDigest, idxBytes); err != nil {
		return nil, err
	}
	if err := store.SetNamespace(opts.As, idxDigest); err != nil {
		return nil, err
	}
	res.IndexDigest = idxDigest
	bump()
	return res, nil
}

// fileFromEntry builds the manifest File for a content entry.
func fileFromEntry(e entry, blobs map[string][]byte) artifact.File {
	b := blobs[e.name]
	f := artifact.File{
		Kind:     e.kind,
		Digest:   artifact.Digest(b),
		Size:     int64(len(b)),
		Name:     e.name,
		Role:     e.role,
		CodeRole: e.codeRole,
		BT:       artifact.BT{MerkleRoot: "sha256:" + hexString(artifact.MerkleRoot(b))},
	}
	if e.part > 0 {
		p := e.part
		f.Part = &p
	}
	return f
}

func fileFromBytes(name, kind string, blobs map[string][]byte) artifact.File {
	b := blobs[name]
	return artifact.File{
		Kind:   kind,
		Digest: artifact.Digest(b),
		Size:   int64(len(b)),
		Name:   name,
		BT:     artifact.BT{MerkleRoot: "sha256:" + hexString(artifact.MerkleRoot(b))},
	}
}

func putCanonical(store *cas.Store, digest string, b []byte) error {
	return store.Put(artifact.DigestHex(digest), strings.NewReader(string(b)))
}

// namespaceLocks serializes the index read-merge-write span per namespace:
// concurrent import jobs for the same as must not lose members (the CAS
// state mutex only covers individual file writes, not the RMW chain).
// ponytail: in-process only; cross-process locking when multiple daemons
// share a CAS root.
var namespaceLocks sync.Map // nsKey → *sync.Mutex

func lockNamespace(nsKey string) *sync.Mutex {
	m, _ := namespaceLocks.LoadOrStore(nsKey, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// existingMembers loads the namespace's current index from state+CAS, if any.
// Member validation runs through the central index validator in
// internal/artifact — the same rules the API's index reader and the spec
// vectors enforce. A corrupt index is a loud import error; its members are
// never silently carried into the new current index.
func existingMembers(store *cas.Store, nsKey string) ([]artifact.IndexMember, error) {
	ns, err := store.Namespaces()
	if err != nil {
		return nil, err
	}
	d, ok := ns[nsKey]
	if !ok {
		return nil, nil
	}
	hex := artifact.DigestHex(d)
	if !store.Has(hex) {
		// B4: consistent with the API's E_NO_INDEX — a namespace pointing
		// at a missing index blob is corruption, never silently rebuilt.
		return nil, fmt.Errorf("importer: current index %s for %q is missing from the CAS; refusing to rebuild silently (run `shardhive cas verify --all` and repair state)", d, nsKey)
	}
	f, err := store.Open(hex)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var idx artifact.Index
	if err := jsonDecode(f, &idx); err != nil {
		return nil, fmt.Errorf("current index %s: %w", d, err)
	}
	if verr := artifact.ValidateIndex(&idx); verr != nil {
		return nil, fmt.Errorf("current index %s for %q fails validation; refusing to carry corrupt members into the new index (run `shardhive cas verify --all` and repair state): %w", d, nsKey, verr)
	}
	return idx.Members, nil
}

func sourceByName(sources []Source, name string) Source {
	for _, s := range sources {
		if s.Name == name {
			return s
		}
	}
	return Source{Name: name, Open: func() (io.ReadCloser, error) {
		return nil, fmt.Errorf("no source for %s", name)
	}}
}

// safetensorsHeaderBytes extracts the JSON header: 8-byte little-endian
// length prefix, then the header document (rust safetensors format).
func safetensorsHeaderBytes(b []byte) []byte {
	if len(b) < 8 {
		return nil
	}
	n := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
	if n == 0 || n > uint64(len(b))-8 || n > 100<<20 {
		return nil
	}
	return b[8 : 8+n]
}

func path_base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func hexString(b [32]byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0xf]
	}
	return string(out)
}

func jsonDecode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
