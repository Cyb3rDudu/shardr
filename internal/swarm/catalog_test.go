package swarm

// The full catalog pull loop over real torrent clients on localhost:
// a foreign (v1) seeder serves the torrent; a shardr swarm client pulls
// it anchored (strict HF-anchor mode) into its CAS, builds the native
// artifact, and KEEPS seeding the foreign swarm; then the original
// seeder DIES and a second fresh node pulls the same torrent purely
// from the first node's good-citizen seed — the give-back proof.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/Cyb3rDudu/shardr/internal/catalog"
)

func TestCatalogPullLoopSeedsForeignSwarm(t *testing.T) {
	fx := testFixture()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// ---- The foreign seeder: a plain BitTorrent client with the repo
	// files in its data dir (the pirateface seeder stand-in).
	dir := t.TempDir()
	for _, f := range fx.Files {
		p := filepath.Join(dir, fx.Name, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scfg := torrent.NewDefaultClientConfig()
	scfg.DataDir = dir
	scfg.NoDHT = true
	scfg.ListenPort = 0 // ephemeral: anacrolix's fixed default (42069) collides when test binaries run in parallel
	scfg.DisableIPv6 = true
	scfg.ListenHost = func(string) string { return "127.0.0.1" }
	scfg.Seed = true
	seeder, err := torrent.NewClient(scfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := seeder.AddTorrent(fx.MetaInfo)
	if err != nil {
		t.Fatal(err)
	}
	<-st.GotInfo()
	if err := st.VerifyDataContext(ctx); err != nil {
		t.Fatal(err)
	}
	var seederAddr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range seeder.ListenAddrs() {
			if a.Network() == "tcp" {
				seederAddr = a.String()
			}
		}
		if seederAddr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if seederAddr == "" {
		t.Fatal("seeder never listened")
	}

	resolved := &catalog.Resolved{
		Repo:     "owner/repo",
		Revision: strings.Repeat("b", 40),
		Infohash: fx.Infohash,
		Magnet:   fx.Magnet("", []string{seederAddr}),
		Trackers: nil,
		Webseeds: nil,
	}
	anchorFiles := make([]catalog.AnchorFile, 0, len(fx.Files))
	anchorByPath := map[string]ForeignFile{}
	for _, f := range fx.Files {
		anchorFiles = append(anchorFiles, catalog.AnchorFile{
			Path: f.Path, Size: int64(len(f.Content)),
			SHA256: sha256Hex(f.Content), GitSHA1: gitBlob(f.Content),
		})
		anchorByPath[f.Path] = ForeignFile{Path: f.Path, Size: int64(len(f.Content)), SHA256: sha256Hex(f.Content), GitSHA1: gitBlob(f.Content)}
	}
	anchor := &catalog.Anchor{Repo: "owner/repo", Revision: strings.Repeat("b", 40), Source: "huggingface", Files: anchorFiles}

	// ---- The pull: shardr swarm client, anchored import.
	cfg := DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	cfg.Seed = true
	nodeA, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeA.Close()
	resA, err := nodeA.ImportCatalog(ctx, &CatalogPull{Resolved: resolved, Anchor: anchor, Peers: []string{seederAddr}}, nil)
	if err != nil {
		t.Fatalf("import catalog: %v", err)
	}
	if resA.Import == nil || len(resA.Import.Members) == 0 {
		t.Fatalf("no artifacts: %+v", resA.Import)
	}
	if !resA.SeededForeign {
		t.Fatal("good-citizen handle must be alive after the import")
	}
	// Anchor convergence: every fixture file's CAS digest is the anchor digest.
	for path, spec := range anchorByPath {
		if !nodeA.store.Has(spec.SHA256) {
			t.Fatalf("anchor digest for %s missing from CAS", path)
		}
	}
	if len(nodeA.ForeignPeerAddrs()) == 0 {
		t.Fatal("foreign engine must be listening (seeding)")
	}

	// ---- Good-citizen proof: the original seeder dies; node B pulls
	// the same torrent purely from node A's foreign seed.
	seeder.Close()
	foreignAddr := nodeA.ForeignPeerAddrs()[0]
	resolvedB := &catalog.Resolved{
		Repo: "owner/repo", Revision: strings.Repeat("b", 40), Infohash: fx.Infohash,
		Magnet: fx.Magnet("", []string{foreignAddr}),
	}
	cfgB := DefaultConfig()
	cfgB.DataRoot = t.TempDir()
	cfgB.DHT = false
	cfgB.DisableIPv6 = true
	cfgB.ListenHost = "127.0.0.1"
	cfgB.Seed = true
	nodeB, err := New(cfgB)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeB.Close()
	resB, err := nodeB.ImportCatalog(ctx, &CatalogPull{Resolved: resolvedB, Anchor: anchor, Peers: []string{foreignAddr}}, nil)
	if err != nil {
		t.Fatalf("good-citizen re-pull from node A failed: %v", err)
	}
	// Convergence: both nodes built the identical index for the repo.
	if resB.Import.IndexDigest != resA.Import.IndexDigest {
		t.Fatalf("index divergence: A %s vs B %s", resA.Import.IndexDigest, resB.Import.IndexDigest)
	}
}
