package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Inline comments are stripped outside strings; a # inside a quoted
// string value survives; sections keep verbatim quoted headers.
func TestParseSemantics(t *testing.T) {
	f, err := parseT(t, `
[swarm]
enabled = true            # inline comment survives parsing
webseed_addr = "127.0.0.1:0"  # trailing

[models."ns/name:q8_0".llama]
n_gpu_layers = 40 # per-model overlay
jinja = true # per-model bool
`)
	if err != nil {
		t.Fatal(err)
	}
	if !f["swarm"]["enabled"].Bool {
		t.Fatal("inline bool comment stripped")
	}
	if got := f["swarm"]["webseed_addr"].Str; got != "127.0.0.1:0" {
		t.Fatalf("quoted string: %q", got)
	}
	sec, ok := f[`models."ns/name:q8_0".llama`]
	if !ok {
		t.Fatalf("verbatim quoted section header: %+v", f)
	}
	if sec["n_gpu_layers"].Int != 40 {
		t.Fatalf("per-model overlay int: %+v", sec["n_gpu_layers"])
	}
}

func TestParseQuotedHashSurvives(t *testing.T) {
	f, err := parseT(t, "[swarm]\nwebseed_addr = \"a#b\"  # trailing\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := f["swarm"]["webseed_addr"].Str; got != "a#b" {
		t.Fatalf("quoted hash survives: %q", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := parseT(t, "[swarm]\nenabled = maybe\n"); err == nil {
		t.Fatal("non-bool must be loud")
	}
	if _, err := parseT(t, "[swarm]\nno equals sign\n"); err == nil {
		t.Fatal("malformed line must be loud")
	}
	if _, err := parseT(t, "[swarm]\nenabled = true\nenabled = false\n"); err == nil {
		t.Fatal("duplicate key must be loud")
	}
}

func parseT(t *testing.T, content string) (File, error) {
	t.Helper()
	path := write(t, content)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(path, string(data))
}
