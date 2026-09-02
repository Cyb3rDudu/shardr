package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func dig(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func part(n int64) *int64 { return &n }

// Duplicate parts ([1,2,2,4]) must be rejected by the element-wise
// contiguity check — an ends-only check would pass them (001 §3.1).
func TestPartsDuplicateRejected(t *testing.T) {
	m := &Manifest{SchemaVersion: 1, ArtifactType: "model", Files: []File{
		{Kind: "config", Name: "modelconfig.json", Size: 10, Digest: dig('a'), BT: BT{MerkleRoot: dig('a')}},
		{Kind: "weights.gguf", Name: "m-1.gguf", Size: 100, Digest: dig('b'), Part: part(1), BT: BT{MerkleRoot: dig('b')}},
		{Kind: "weights.gguf", Name: "m-2.gguf", Size: 100, Digest: dig('c'), Part: part(2), BT: BT{MerkleRoot: dig('c')}},
		{Kind: "weights.gguf", Name: "m-2b.gguf", Size: 100, Digest: dig('d'), Part: part(2), BT: BT{MerkleRoot: dig('d')}},
		{Kind: "weights.gguf", Name: "m-4.gguf", Size: 100, Digest: dig('e'), Part: part(4), BT: BT{MerkleRoot: dig('e')}},
	}}
	if verr := ValidateManifest(m); verr == nil || verr.Class != ClassValidationParts {
		t.Fatalf("duplicate parts must be rejected as E_VALIDATION_PARTS, got %v", verr)
	}
	// The canonical classes surface on the wire errors.
	if got := (&ValidationError{Class: ClassValidationParts, Msg: "x"}).Error(); got != "E_VALIDATION_PARTS: x" {
		t.Fatalf("Error(): %q", got)
	}
}

// BuildTorrent must reject a manifest whose pinned bt.merkleRoot does not
// match the content's computed root — a manifest lying about its own
// torrent geometry (defense in depth before the infohash gate).
func TestBuildTorrentRejectsLyingMerkleRoot(t *testing.T) {
	content := bytes.Repeat([]byte{7}, 1<<20)
	sum := sha256.Sum256(content)
	root := MerkleRoot(content)
	lyingRoot := sha256.Sum256([]byte("not the real root"))
	files := []File{{
		Kind: "weights.gguf", Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size: int64(len(content)), Name: "big.gguf",
		BT: BT{MerkleRoot: "sha256:" + hex.EncodeToString(lyingRoot[:])},
	}}
	mb := []byte(`{}`)
	msum := sha256.Sum256(mb)
	_, _, err := BuildTorrent(files, mb, hex.EncodeToString(msum[:]), func(string) ([]byte, error) { return content, nil })
	if err == nil || !strings.Contains(err.Error(), "pinned bt.merkleRoot") {
		t.Fatalf("lying merkle root must be loud: %v", err)
	}
	_ = root
}
