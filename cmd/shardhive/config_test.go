package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

// writeConfig writes a config file into a temp HOME/XDG and points
// SHARDR_CONFIG at it.
func writeConfig(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHARDR_CONFIG", path)
}

func TestLoadSwarmConfigDefaults(t *testing.T) {
	writeConfig(t, "")
	cfg, err := loadSwarmConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := swarm.DefaultConfig()
	if cfg != want {
		t.Fatalf("empty file must yield defaults: %+v", cfg)
	}
}

func TestLoadSwarmConfigFull(t *testing.T) {
	writeConfig(t, `# shardr config
[references]
default_selector = "q8_0"   # other component's section — not ours

[swarm]
enabled = true
seed = false
upload_limit = 1048576
dht = false
webseed_addr = "127.0.0.1:7777"

[models."unsloth/qwen3.8-27b-gguf:ud-q4_k_m"]
`)
	cfg, err := loadSwarmConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Seed || cfg.DHT {
		t.Fatalf("bools: %+v", cfg)
	}
	if cfg.UploadLimit != 1048576 {
		t.Fatalf("upload_limit: %d", cfg.UploadLimit)
	}
	if cfg.WebseedAddr != "127.0.0.1:7777" {
		t.Fatalf("webseed_addr: %q", cfg.WebseedAddr)
	}
}

// A typo in [swarm] must fail loudly — a quietly ignored key could
// disable seeding without the operator ever noticing.
func TestLoadSwarmConfigUnknownKeyIsLoud(t *testing.T) {
	writeConfig(t, "[swarm]\nseeding = false\n")
	if _, err := loadSwarmConfig(); err == nil || !strings.Contains(err.Error(), "seeding") {
		t.Fatalf("unknown key must be loud: %v", err)
	}
}

func TestLoadSwarmConfigBadValue(t *testing.T) {
	writeConfig(t, "[swarm]\nseed = maybe\n")
	if _, err := loadSwarmConfig(); err == nil {
		t.Fatal("bad bool must be loud")
	}
}

// The seed knobs map onto the engine config (DoD: [swarm] seed=false /
// upload_limit respected — the mapping test; behavior is covered by the
// swarm package tests).
func TestSeedKnobsMapToEngineConfig(t *testing.T) {
	writeConfig(t, "[swarm]\nseed = false\nupload_limit = 512\n")
	cfg, err := loadSwarmConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Seed {
		t.Fatal("seed=false must reach the engine config")
	}
	if cfg.UploadLimit != 512 {
		t.Fatalf("upload_limit must reach the engine config: %d", cfg.UploadLimit)
	}
}

// The 004 §7 example config, verbatim, must parse cleanly — inline
// comments after values, quoted section keys, all of it.
func TestLoadSwarmConfigSpecExampleLiteral(t *testing.T) {
	writeConfig(t, `[swarm]
enabled = true            # shardhive swarm client (fetch + seed)
seed = true               # seed complete artifacts (the community mirror)
upload_limit = 0          # bytes/sec, 0 = unlimited
dht = true                # DHT + PEX

[references]
# Interactive-CLI comfort ONLY: applied when a human types a
# selector-less ref. Never applied in Modelfiles, the API, manifests,
# or shardrbay entries — those always require an explicit selector.
default_selector = ""

[runtimes.llama]          # overlay layer 2 (002 §2)
n_threads = 8

[models."unsloth/qwen3.8-27b-gguf:ud-q4_k_m"]   # per-model overlay
[models."unsloth/qwen3.8-27b-gguf:ud-q4_k_m".llama]
n_gpu_layers = 40
`)
	cfg, err := loadSwarmConfig()
	if err != nil {
		t.Fatalf("spec example must parse: %v", err)
	}
	want := swarm.DefaultConfig()
	if cfg != want {
		t.Fatalf("spec example must yield the documented defaults: %+v", cfg)
	}
}

// Inline comments are stripped outside strings; a # inside a quoted
// string value survives.
func TestStripCommentSemantics(t *testing.T) {
	if got := stripComment(`true            # shardhive swarm client`); got != "true" {
		t.Fatalf("inline bool comment: %q", got)
	}
	if got := stripComment(`"a#b"  # trailing`); got != `"a#b"` {
		t.Fatalf("quoted hash survives: %q", got)
	}
	if got := stripComment("127.0.0.1:0"); got != "127.0.0.1:0" {
		t.Fatalf("no comment untouched: %q", got)
	}
}
