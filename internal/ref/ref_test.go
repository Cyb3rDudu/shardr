package ref

import "testing"

// The full parse/resolve surface is machine-checked by the spec vectors
// (internal/specvectors runs docs/specs/vectors/000-reference.jsonl against
// this package). These tests cover only the production extras the vectors
// do not exercise.

func TestParseAnyBothForms(t *testing.T) {
	p, err := ParseAny("shardr:///ns/name:q8_0")
	if err != nil || p.Canonical != "shardr:///ns/name:q8_0" {
		t.Fatalf("canonical form: %v %s", err, p.Canonical)
	}
	p, err = ParseAny("ns/name:q8_0")
	if err != nil || p.Canonical != "shardr:///ns/name:q8_0" {
		t.Fatalf("short form: %v %s", err, p.Canonical)
	}
	// Case: ns/name lowercased in short form too.
	p, err = ParseAny("NS/Name:q8_0")
	if err != nil || p.Canonical != "shardr:///ns/name:q8_0" {
		t.Fatalf("short lowercase: %v %s", err, p.Canonical)
	}
	if _, err := ParseAny("bare-name:q8_0"); err == nil || err.Class != ErrParse {
		t.Fatalf("bare name accepted: %v", err)
	}
}

func TestNormalizeDigest(t *testing.T) {
	ok64 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, err := NormalizeDigest(ok64); err != nil || got != "sha256:"+ok64 {
		t.Fatalf("bare hex: %v %s", err, got)
	}
	if got, err := NormalizeDigest("sha256:" + ok64); err != nil || got != "sha256:"+ok64 {
		t.Fatalf("prefixed: %v %s", err, got)
	}
	for _, bad := range []string{
		"", "sha256:", "sha256:xyz", ok64[:63], ok64 + "0",
		"SHA256:" + ok64, "sha256:" + "0123456789ABCDEF" + ok64[16:],
	} {
		if _, err := NormalizeDigest(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestValidNamespaceKey(t *testing.T) {
	for _, ok := range []string{"ns/name", "unsloth/qwen3.8-27b-gguf", "a/b", "library/x"} {
		if !ValidNamespaceKey(ok) {
			t.Fatalf("rejected %q", ok)
		}
	}
	for _, bad := range []string{
		"", "name", "ns/name/extra", "/name", "ns/", "UP/name", "ns/UP",
		"ns/na me", "-lead/name", "ns/-lead",
	} {
		if ValidNamespaceKey(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestValidTag(t *testing.T) {
	for _, ok := range []string{"stable", "Stable", "_x", "nightly-2026"} {
		if err := ValidTag(ok); err != nil {
			t.Fatalf("rejected %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "q8_0", // quant-shaped → ban
		"sha256-abc", "SHA256:abc", // digest-shaped → ban
		".lead", "sp ace", "a\x00b", // shape violations
	} {
		if err := ValidTag(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
