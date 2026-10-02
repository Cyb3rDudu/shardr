package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/catalog"
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

// [catalog] upload_limit resolution (Epic #65 ruling 2): set → own
// value; unset → INHERIT [swarm] upload_limit; garbage keys are loud.
// Mutation-proofing: ignoring the config turns this red.
func TestCatalogUploadLimitResolution(t *testing.T) {
	write := func(toml string) string {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nupload_limit = 4096\n"))
	if n, err := loadCatalogUploadLimit(1 << 20); err != nil || n != 4096 {
		t.Fatalf("[catalog] set: %d %v (want 4096)", n, err)
	}
	t.Setenv("SHARDR_CONFIG", write("[swarm]\nupload_limit = 250000\n"))
	if n, err := loadCatalogUploadLimit(250000); err != nil || n != 250000 {
		t.Fatalf("[catalog] unset must inherit [swarm]: %d %v (want 250000)", n, err)
	}
	t.Setenv("SHARDR_CONFIG", write(""))
	if n, err := loadCatalogUploadLimit(0); err != nil || n != 0 {
		t.Fatalf("both unset = unlimited: %d %v", n, err)
	}
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nupload_limit = -1\n"))
	if _, err := loadCatalogUploadLimit(0); err == nil {
		t.Fatal("negative upload_limit must be loud")
	}
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nseed_faster = true\n"))
	if _, err := loadCatalogUploadLimit(0); err == nil || !strings.Contains(err.Error(), "unknown [catalog] key") {
		t.Fatalf("unknown key must be loud: %v", err)
	}
}

func TestCatalogURLResolution(t *testing.T) {
	write := func(toml string) string {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Unset (absent section, absent key) → "" → provider default;
	// behavior unchanged.
	for _, toml := range []string{"", "[swarm]\nenabled = true\n", "[catalog]\nupload_limit = 5\n"} {
		t.Setenv("SHARDR_CONFIG", write(toml))
		if u, err := loadCatalogURL(); err != nil || u != "" {
			t.Fatalf("unset must stay \"\" (default): %q %v", u, err)
		}
	}
	// Valid: scheme + host, optional port — exactly that.
	for _, valid := range []string{"https://mirror.example", "https://mirror.example:8443"} {
		t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \""+valid+"\"\n"))
		if u, err := loadCatalogURL(); err != nil || u != valid {
			t.Fatalf("valid url %q: got %q %v", valid, u, err)
		}
	}
	// Invalid: loud startup error naming the rule — never a fallback.
	for _, invalid := range []string{
		"",                        // explicit empty
		"http://mirror.example",   // not https
		"https://mirror.example/", // trailing path
		"https://mirror.example/api",
		"https://mirror.example/?q=1",
		"https://mirror.example#frag",
		"https://",                    // no host
		"https://user@mirror.example", // userinfo
		"mirror.example",              // not absolute
	} {
		t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \""+invalid+"\"\n"))
		_, err := loadCatalogURL()
		if err == nil || !strings.Contains(err.Error(), "[catalog] url must be") {
			t.Fatalf("invalid url %q must be loud with reason, got %v", invalid, err)
		}
	}
	// Wrong type: loud too.
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = 5\n"))
	if _, err := loadCatalogURL(); err == nil || !strings.Contains(err.Error(), "quoted string") {
		t.Fatalf("non-string url must be loud: %v", err)
	}
	// Unknown key stays loud even next to a valid url (fail-closed set).
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \"https://mirror.example\"\nseed_faster = true\n"))
	if _, err := loadCatalogURL(); err == nil || !strings.Contains(err.Error(), "unknown [catalog] key") {
		t.Fatalf("unknown key must stay loud: %v", err)
	}
	// upload_limit behavior untouched alongside url.
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \"https://mirror.example\"\nupload_limit = 4096\n"))
	if n, err := loadCatalogUploadLimit(1 << 20); err != nil || n != 4096 {
		t.Fatalf("upload_limit beside url: %d %v", n, err)
	}
}

// TestNewCatalogProviderWiring pins the daemon chain resolveCatalogURL
// → loadCatalogURL → NewPiratefaceAt: the configured [catalog] url must
// reach the provider. The dead-loopback URL makes the proof offline —
// if the wiring ignored config and fell back to the default, Search
// would hit the live provider and not fail with "unreachable".
func TestNewCatalogProviderWiring(t *testing.T) {
	write := func(toml string) string {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// Configured dead loopback → provider pinned to it; Search fails
	// offline (connection refused), proving no fallback to the default.
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \"https://127.0.0.1:1\"\n"))
	t.Setenv("SHARDR_CATALOG_URL", "") // config must be the only override
	p, err := newCatalogProvider()
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != "https://127.0.0.1:1" {
		t.Fatalf("provider base: %q", p.BaseURL)
	}
	if _, serr := p.Search(context.Background(), "tinyllm"); serr == nil || !strings.Contains(serr.Error(), "unreachable") {
		t.Fatalf("dead loopback must answer unreachable, got %v", serr)
	}

	// Unset → the documented default, behavior unchanged.
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nupload_limit = 5\n"))
	p, err = newCatalogProvider()
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseURL != catalog.DefaultPiratefaceURL {
		t.Fatalf("unset must keep the default, got %q", p.BaseURL)
	}

	// Invalid url → loud, no provider.
	t.Setenv("SHARDR_CONFIG", write("[catalog]\nurl = \"http://insecure.example\"\n"))
	if _, err := newCatalogProvider(); err == nil || !strings.Contains(err.Error(), "[catalog] url must be") {
		t.Fatalf("invalid url must be loud: %v", err)
	}
}
