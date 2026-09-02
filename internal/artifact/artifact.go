package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

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
// Seal: deterministic artifact construction (001 §3, §6, 004 §3)
// The canonical validation lives in validate.go — the same rule set the
// spec vectors run against (internal/specvectors).
// ---------------------------------------------------------------------------

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
