package specvectors

import (
	"crypto/sha256"
	"strconv"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
)

// ----------------------------------------------------------------------------
// Adapter over the production torrent math in internal/artifact (spec 004).
// The spec test vectors in docs/specs/vectors/004-torrent.jsonl and the gold
// files (info dict, piece-layers benc) run against the same code the
// importer uses, so vectors and production math cannot drift apart. The
// test-only shapes below exist to keep suites_test.go / generate_test.go
// stable.
// ----------------------------------------------------------------------------

// manifestDoc is the decoded shape of a canonical manifest (001 §3).
type manifestDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	ArtifactType  string            `json:"artifactType"`
	Files         []manifestFile    `json:"files"`
	Annotations   map[string]string `json:"annotations"`
}

type manifestFile struct {
	Kind    string `json:"kind"`
	Digest  string `json:"digest"`
	Size    int64  `json:"size"`
	Name    string `json:"name"`
	Part    *int64 `json:"part"`
	Role    string `json:"role"`
	Runtime string `json:"runtime"`
	BT      struct {
		MerkleRoot string `json:"merkleRoot"`
	} `json:"bt"`
}

// infoDictResult carries the derived torrent identity of an artifact.
type infoDictResult struct {
	Name               string // torrent name: shardr-sha256-<12 hex>
	PieceLength        int64
	TotalSize          int64  // sum of entry sizes + manifest bytes
	Infohash           string // btmh:1220<hex>
	PieceLayersBencode []byte
	PieceLayersDigest  string // sha256:<hex> of the piece-layers blob
}

func toArtifactFiles(fs []manifestFile) []artifact.File {
	out := make([]artifact.File, len(fs))
	for i, f := range fs {
		out[i] = artifact.File{
			Kind: f.Kind, Digest: f.Digest, Size: f.Size, Name: f.Name,
			Part: f.Part, Role: f.Role, Runtime: f.Runtime,
			BT: artifact.BT{MerkleRoot: f.BT.MerkleRoot},
		}
	}
	return out
}

// buildInfoDict delegates to artifact.BuildTorrent (004 §3).
func buildInfoDict(m *manifestDoc, manifestBytes []byte, digestHex string, resolve func(name string) []byte) (infoBencode []byte, res infoDictResult, err error) {
	benc, tres, err := artifact.BuildTorrent(toArtifactFiles(m.Files), manifestBytes, digestHex,
		func(name string) ([]byte, error) { return resolve(name), nil })
	if err != nil {
		return nil, res, err
	}
	return benc, infoDictResult{
		Name:               tres.Name,
		PieceLength:        tres.PieceLength,
		TotalSize:          tres.TotalSize,
		Infohash:           tres.Infohash,
		PieceLayersBencode: tres.PieceLayersBencode,
		PieceLayersDigest:  tres.PieceLayersDigest,
	}, nil
}

// merkleRoot / ladderPieceLength / pieceLayerHashes delegate to the
// production implementations.
func merkleRoot(data []byte) [sha256.Size]byte { return artifact.MerkleRoot(data) }
func ladderPieceLength(totalSize int64) int64  { return artifact.LadderPieceLength(totalSize) }
func pieceLayerHashes(data []byte, pieceLength int64) [][sha256.Size]byte {
	return artifact.PieceLayerHashes(data, pieceLength)
}

// ----------------------------------------------------------------------------
// Deterministic file content recipes (documented in vectors README.md).
// ----------------------------------------------------------------------------

// recipeFile is the decoded sidecar entry.
type recipeFile struct {
	JSON         map[string]any `json:"json"`
	Repeat       string         `json:"repeat"`
	Sha256stream string         `json:"sha256stream"`
	Size         int64          `json:"size"`
}

func recipeBytes(r recipeFile) []byte {
	switch {
	case r.JSON != nil:
		return jcsMustMarshal(r.JSON)
	case r.Repeat != "":
		out := make([]byte, r.Size)
		for i := range out {
			out[i] = r.Repeat[i%len(r.Repeat)]
		}
		return out
	case r.Sha256stream != "":
		var out []byte
		for i := 0; int64(len(out)) < r.Size; i++ {
			sum := sha256.Sum256([]byte(r.Sha256stream + ":" + strconv.Itoa(i)))
			out = append(out, sum[:]...)
		}
		return out[:r.Size]
	default:
		panic("empty recipe")
	}
}

// pow2int reports whether n is a power of two (n ≥ 1).
func pow2int(n int64) bool {
	return n > 0 && n&(n-1) == 0
}
