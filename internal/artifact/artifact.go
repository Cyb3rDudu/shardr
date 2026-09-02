package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// File is one manifest file entry (001 §3.1). Field order in the struct is
// irrelevant — canonical serialization sorts.
type File struct {
	Kind        string `json:"kind"`
	Digest      string `json:"digest"`
	Size        int64  `json:"size"`
	Name        string `json:"name"`
	Part        *int64 `json:"part,omitempty"`
	Role        string `json:"role,omitempty"`
	Variant     string `json:"variant,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	AdapterType string `json:"adapterType,omitempty"`
	CodeRole    string `json:"codeRole,omitempty"`
	BT          BT     `json:"bt"`
}

// BT pins the per-file BitTorrent v2 merkle root (001 §3.1, 004 §2).
type BT struct {
	MerkleRoot string `json:"merkleRoot"`
}

// Manifest is the model artifact document (001 §3).
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	ArtifactType  string            `json:"artifactType"`
	Files         []File            `json:"files"`
	Annotations   map[string]string `json:"annotations"`
}

// IndexMember is one quant family member of a model index (001 §5).
type IndexMember struct {
	Manifest      string `json:"manifest"`
	Quant         string `json:"quant"`
	WeightsFormat string `json:"weightsFormat"`
	Revision      string `json:"revision"`
}

// Index is the per-repo quant family grouping (001 §5). Immutable,
// content-addressed, stored in shardhive state, never torrented.
type Index struct {
	SchemaVersion int           `json:"schemaVersion"`
	ArtifactType  string        `json:"artifactType"`
	Members       []IndexMember `json:"members"`
}

// DistributionRecord binds a manifest to its torrent (001 §6).
type DistributionRecord struct {
	SchemaVersion  int     `json:"schemaVersion"`
	ArtifactType   string  `json:"artifactType"`
	ManifestDigest string  `json:"manifestDigest"`
	Torrent        Torrent `json:"torrent"`
}

// Torrent is the identity-bearing subset of the torrent binding.
type Torrent struct {
	Infohash          string `json:"infohash"`
	PieceLength       int64  `json:"pieceLength"`
	PieceLayersDigest string `json:"pieceLayersDigest"`
}

// Canonical serializes v as RFC 8785 JCS — the deterministic form all
// artifact documents are hashed over (001 §3).
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

// Digest returns "sha256:<hex>" of the flat bytes — the canonical address
// form (001 §7.1).
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Validation (001 §3.1 + R2 name reservation)
// ---------------------------------------------------------------------------

// ValidateFileName enforces canonical artifact-relative names (001 §3.1
// rule 1, R2): '/'-separated non-empty segments, no '.', '..', no empty
// segments (covers leading '/', 'a//b', './x'), unique within the caller's
// set — and the reserved 'manifest' namespace (004 §3 places the manifest
// blob at manifest/sha256-<hex>): neither 'manifest' itself nor any
// 'manifest/…' prefix is a legal file name.
func ValidateFileName(name string) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if name == "manifest" || strings.HasPrefix(name, "manifest/") {
		return fmt.Errorf("name %q is reserved (manifest/ torrent tree prefix, 004 §3)", name)
	}
	segs := strings.Split(name, "/")
	for _, s := range segs {
		if s == "" {
			return fmt.Errorf("name %q: empty path segment", name)
		}
		if s == "." || s == ".." {
			return fmt.Errorf("name %q: forbidden segment %q", name, s)
		}
	}
	return nil
}

// ValidateManifest checks the structural invariants of a built manifest:
// unique valid names, exactly one config, one weights format, contiguous
// split parts, digest and merkle-root presence on every entry. Returns a
// loud error naming the violated rule.
func ValidateManifest(m *Manifest) error {
	if m.SchemaVersion != 1 || m.ArtifactType != "model" {
		return fmt.Errorf("manifest: schemaVersion/artifactType must be 1/model")
	}
	seen := map[string]bool{}
	weightsFormat := ""
	parts := map[string][]int64{}
	nConfig, nTokenizer, nChat := 0, 0, 0
	// A name that is a path PREFIX of another name would corrupt the BEP 52
	// file tree (file/dir collision at one path) — reject loudly instead of
	// building a torrent no client accepts (or panicking in insertFileTree).
	byName := map[string]File{}
	for _, f := range m.Files {
		byName[f.Name] = f
	}
	for _, f := range m.Files {
		if err := ValidateFileName(f.Name); err != nil {
			return err
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate file name %q", f.Name)
		}
		seen[f.Name] = true
		if f.Digest == "" || f.Size < 0 || f.BT.MerkleRoot == "" {
			return fmt.Errorf("file %q: digest, size, and bt.merkleRoot are required", f.Name)
		}
		switch f.Kind {
		case "config":
			nConfig++
		case "weights.gguf":
			if weightsFormat != "" && weightsFormat != "gguf" {
				return errors.New("one weights format per artifact (gguf+safetensors mixed)")
			}
			weightsFormat = "gguf"
			if f.Part != nil {
				parts["gguf"] = append(parts["gguf"], *f.Part)
			}
		case "weights.safetensors":
			if weightsFormat != "" && weightsFormat != "safetensors" {
				return errors.New("one weights format per artifact (gguf+safetensors mixed)")
			}
			weightsFormat = "safetensors"
			if f.Part != nil {
				parts["st"] = append(parts["st"], *f.Part)
			}
		case "tokenizer":
			nTokenizer++
		case "chat-template":
			nChat++
		case "weights.aux", "adapter", "runtime-config", "code":
			// cardinality 0..n
		default:
			return fmt.Errorf("file %q: unknown kind %q", f.Name, f.Kind)
		}
	}
	if nConfig != 1 {
		return fmt.Errorf("manifest requires exactly one config, got %d", nConfig)
	}
	if weightsFormat == "" {
		return errors.New("manifest requires at least one weights file (001 §8.1)")
	}
	if nTokenizer > 1 || nChat > 1 {
		return fmt.Errorf("tokenizer/chat-template are 0..1 (got %d/%d)", nTokenizer, nChat)
	}
	// Path-prefix collision: "a" and "a/b" cannot coexist in the tree.
	for _, f := range m.Files {
		segs := strings.Split(f.Name, "/")
		for i := 1; i < len(segs); i++ {
			if _, ok := byName[strings.Join(segs[:i], "/")]; ok {
				return fmt.Errorf("name %q collides with directory path %q (BEP 52 file tree)", f.Name, strings.Join(segs[:i], "/"))
			}
		}
	}
	// Split parts must be contiguous 1..n.
	for _, ps := range parts {
		if len(ps) == 0 {
			continue
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
		if ps[0] != 1 || ps[len(ps)-1] != int64(len(ps)) {
			return fmt.Errorf("split parts must be contiguous 1..n, got %v", ps)
		}
	}
	return nil
}

// SortFiles orders entries into the canonical deterministic order
// (001 §3.1 rule 4): config, weights (by part, then digest), weights.aux
// (by name), tokenizer, chat-template, adapters (by digest), runtime-config
// (by runtime id), code (by role, then name).
func SortFiles(files []File) {
	kindRank := map[string]int{
		"config": 0, "weights.gguf": 1, "weights.safetensors": 1,
		"weights.aux": 2, "tokenizer": 3, "chat-template": 4,
		"adapter": 5, "runtime-config": 6, "code": 7,
	}
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		ra, rb := kindRank[a.Kind], kindRank[b.Kind]
		if ra != rb {
			return ra < rb
		}
		switch a.Kind {
		case "weights.gguf", "weights.safetensors":
			// Split parts 1..n; a partless single file behaves as part 0
			// and sorts first.
			pa, pb := int64(0), int64(0)
			if a.Part != nil {
				pa = *a.Part
			}
			if b.Part != nil {
				pb = *b.Part
			}
			if pa != pb {
				return pa < pb
			}
			return a.Digest < b.Digest
		case "weights.aux":
			return a.Name < b.Name
		case "adapter":
			return a.Digest < b.Digest
		case "runtime-config":
			return a.Runtime < b.Runtime
		case "code":
			if a.CodeRole != b.CodeRole {
				return a.CodeRole < b.CodeRole
			}
			return a.Name < b.Name
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// Seal: deterministic artifact construction (001 §3, §6, 004 §3)
// ---------------------------------------------------------------------------

// Resolve returns the content bytes of a file entry by manifest name.
type Resolve func(name string) ([]byte, error)

// Sealed is the full identity set of one built artifact.
type Sealed struct {
	Manifest           *Manifest
	ManifestBytes      []byte // canonical JCS
	ManifestDigest     string // sha256:<hex> of ManifestBytes
	Torrent            TorrentResult
	Distribution       *DistributionRecord
	DistributionBytes  []byte // canonical JCS
	DistributionDigest string
}

// Seal canonicalizes, validates, and torrent-binds a manifest. files must
// already carry digest, size, and bt.merkleRoot for every entry; resolve
// supplies content bytes again for the torrent math (the importer resolves
// from the CAS, so bytes are already verified). Pure function of inputs —
// same files + annotations → byte-identical output (import convergence,
// 001 §7.5).
// ponytail: resolve re-reads full files for the merkle math; streaming
// merkle over the CAS write path is the TB-scale follow-up.
func Seal(files []File, annotations map[string]string, resolve Resolve) (*Sealed, error) {
	SortFiles(files)
	m := &Manifest{SchemaVersion: 1, ArtifactType: "model", Files: files, Annotations: annotations}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	manifestBytes, err := Canonical(m)
	if err != nil {
		return nil, err
	}
	manifestDigest := Digest(manifestBytes)

	infoBenc, tres, err := BuildTorrent(files, manifestBytes, DigestHex(manifestDigest), resolve)
	if err != nil {
		return nil, err
	}
	_ = infoBenc

	dist := &DistributionRecord{
		SchemaVersion:  1,
		ArtifactType:   "distribution",
		ManifestDigest: manifestDigest,
		Torrent: Torrent{
			Infohash:          tres.Infohash,
			PieceLength:       tres.PieceLength,
			PieceLayersDigest: tres.PieceLayersDigest,
		},
	}
	distBytes, err := Canonical(dist)
	if err != nil {
		return nil, err
	}
	return &Sealed{
		Manifest:           m,
		ManifestBytes:      manifestBytes,
		ManifestDigest:     manifestDigest,
		Torrent:            tres,
		Distribution:       dist,
		DistributionBytes:  distBytes,
		DistributionDigest: Digest(distBytes),
	}, nil
}

// UpsertMember merges a member into an index (replacing any member with
// the same quant) and returns members sorted by quant — deterministic
// regardless of import order.
func UpsertMember(members []IndexMember, m IndexMember) []IndexMember {
	out := members[:0:0]
	for _, e := range members {
		if e.Quant != m.Quant {
			out = append(out, e)
		}
	}
	out = append(out, m)
	sort.Slice(out, func(i, j int) bool { return out[i].Quant < out[j].Quant })
	return out
}

// SealIndex canonicalizes an index document.
func SealIndex(members []IndexMember) (bytes []byte, digest string, err error) {
	b, err := Canonical(&Index{SchemaVersion: 1, ArtifactType: "model-index", Members: members})
	if err != nil {
		return nil, "", err
	}
	return b, Digest(b), nil
}
