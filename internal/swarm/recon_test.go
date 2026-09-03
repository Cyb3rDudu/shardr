package swarm

import (
	"strings"
	"testing"
)

// pinHex accepts only the canonical form; bare and uppercase digests are
// loud with the canonicality hint.
func TestPinHexCanonicalOnly(t *testing.T) {
	lower := strings.Repeat("ab", 32)
	if got, err := pinHex("sha256:" + lower); err != nil || got != lower {
		t.Fatalf("canonical: %q %v", got, err)
	}
	if _, err := pinHex(lower); err == nil || !strings.Contains(err.Error(), "sha256: prefix") {
		t.Fatalf("bare hex must be loud with hint: %v", err)
	}
	upper := strings.Repeat("AB", 32)
	if _, err := pinHex("sha256:" + upper); err == nil || !strings.Contains(err.Error(), "uppercase") {
		t.Fatalf("uppercase must be loud with hint: %v", err)
	}
	short := strings.Repeat("a", 63)
	if _, err := pinHex("sha256:" + short); err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Fatalf("short digest must be loud: %v", err)
	}
}
