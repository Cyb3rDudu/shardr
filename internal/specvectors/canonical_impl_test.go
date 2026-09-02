package specvectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
)

// jcsCanonical returns the RFC 8785 (JCS) serialization of a parsed JSON
// document. Uses the reference Go implementation listed by RFC 8785.
func jcsCanonical(raw []byte) ([]byte, error) {
	return jcs.Transform(raw)
}

// jcsMustMarshal canonicalizes an in-memory document (Go values → JSON → JCS).
func jcsMustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out, err := jcsCanonical(raw)
	if err != nil {
		panic(err)
	}
	return out
}

func flatSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ----------------------------------------------------------------------------
// 001 document validation runs against the PRODUCTION validator in
// internal/artifact (like torrent_impl_test against the torrent math):
// the canonical validation the importer enforces is the same rule set the
// spec vectors assert on — vectors and production cannot drift apart.
// ----------------------------------------------------------------------------

// validateArtifact delegates to artifact.ValidateArtifact and returns its
// ValidationError (Class carries the E_* vocabulary from the vectors
// README). The harness (suites_test.go) keeps its (string, *valError)
// shape via the shim below.
type valError = artifact.ValidationError

func validateArtifact(raw []byte) (artifactType string, err *valError) {
	return artifact.ValidateArtifact(raw)
}
