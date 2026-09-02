package artifact

import (
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
