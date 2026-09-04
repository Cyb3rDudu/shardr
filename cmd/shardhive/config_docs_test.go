package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestConfigDocsExampleParses proves the config.toml example in
// docs/user/config.md is the literal spec-004-§7 file and parses
// against the production parser with exactly the documented [swarm]
// values. The TOML block is extracted FROM the docs file so the two
// cannot drift apart.
func TestConfigDocsExampleParses(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "user", "config.md"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?s)```toml\n(.*?)```")
	m := re.FindStringSubmatch(string(doc))
	if m == nil {
		t.Fatal("config.md: no ```toml block found")
	}
	toml := m[1]
	for _, want := range []string{
		"[swarm]", "enabled = true", "seed = true", "upload_limit = 0",
		"dht = true", "[references]", "default_selector = \"\"",
		"[runtimes.llama]", "n_threads = 8", "[models.",
	} {
		if !strings.Contains(toml, want) {
			t.Fatalf("docs example drifted: %q missing from the toml block\n%s", want, toml)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHARDR_CONFIG", path)
	cfg, err := loadSwarmConfig()
	if err != nil {
		t.Fatalf("docs example must parse against the production parser: %v", err)
	}
	if !cfg.Enabled || !cfg.Seed || !cfg.DHT || cfg.UploadLimit != 0 || cfg.NoSeedVerify {
		t.Fatalf("docs example must yield the documented defaults, got %+v", cfg)
	}
}
