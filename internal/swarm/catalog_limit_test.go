package swarm

// Good-citizen upload-limit wiring (Epic #65 ruling 2): [catalog]
// upload_limit (inheriting [swarm] upload_limit when unset) must reach
// the FOREIGN engine's rate limiter — community seeding never eats the
// node's shardr-swarm budget, and an unset config never produces a
// limiter. Mutation-proofing: dropping the wiring (or ignoring the
// config) turns this red.

import (
	"testing"

	"golang.org/x/time/rate"
)

func TestCatalogUploadLimitWiring(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	cfg.CatalogUploadLimit = 4096
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.foreignEngine(); err != nil {
		t.Fatal(err)
	}
	if c.foreignLim == nil {
		t.Fatal("[catalog] upload_limit = 4096 must produce a foreign-engine rate limiter")
	}
	if c.foreignLim.Limit() != rate.Limit(4096) {
		t.Fatalf("limiter rate: %v, want 4096", c.foreignLim.Limit())
	}
	// The v2 engine's budget is separate: no catalog limit on it (the
	// swarm UploadLimit is 0 here = unlimited).
	if c.uploadLim != nil {
		t.Fatal("catalog limit must not leak into the v2 engine budget")
	}
}

func TestCatalogUploadLimitUnlimitedByDefault(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.foreignEngine(); err != nil {
		t.Fatal(err)
	}
	if c.foreignLim != nil {
		t.Fatal("unset [catalog] upload_limit (0 = unlimited) must not construct a limiter")
	}
}
