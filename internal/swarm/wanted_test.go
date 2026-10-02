package swarm

// Review-round-2 pins: the liveness mechanisms whose behavior is
// invisible to the all-files test set. (a) WaitSealedWanted must ignore
// side-effect seals of unwanted files; (b) VerifyPreSealed must flip
// engine piece states over CAS-hit ranges without any download;
// (c) wantedFiles' quant filtering is a direct table.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// (a) An unwanted file that seals FIRST (piece-overlapped neighbour or
// plain piece-aligned layout) must not satisfy a wanted-subset target —
// the old count-all semantics returned early and imported partial
// artifacts. Mutation pin: `st.sealed && wanted[path]` → `st.sealed`
// turns this red (the wait would return while model.gguf is open).
func TestWaitSealedWantedIgnoresUnwantedSeals(t *testing.T) {
	// Piece-aligned two-file layout: notes.txt owns piece 0 alone,
	// model.gguf owns pieces 1..4 — no spanning, so notes can seal while
	// model.gguf is untouched.
	files := []v1FixtureFile{
		{Path: "notes.txt", Content: foreignBytes(64 * 1024)},
		{Path: "model.gguf", Content: foreignBytes(4 * 64 * 1024)},
	}
	fx := buildV1("owner__repo", files, 64*1024)
	_, drv, ti := openFixtureStorage(t, fx, fx.anchorOf(), true)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	// Piece 0 completes → notes.txt (UNWANTED) seals.
	if err := downloadPiece(t, ti, info.Piece(0), stream); err != nil {
		t.Fatal(err)
	}
	if drv.SealedFiles()["notes.txt"] == "" {
		t.Fatal("setup: notes.txt must have sealed from piece 0")
	}
	// Wanted = model.gguf only: the wait must NOT return on notes.txt.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := drv.WaitSealedWanted(ctx, map[string]bool{"model.gguf": true}); err == nil {
		t.Fatal("an unwanted side-effect seal must not satisfy the wanted target (quant-filtered pulls would import partial artifacts)")
	}
	// Completing the wanted file's pieces unblocks the wait.
	for i := 1; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti, info.Piece(i), stream); err != nil {
			t.Fatal(err)
		}
	}
	if err := drv.WaitSealedWanted(context.Background(), map[string]bool{"model.gguf": true}); err != nil {
		t.Fatalf("wanted file sealed — wait must return: %v", err)
	}
}

// (b) VerifyPreSealed flips the ENGINE's piece states over CAS-hit
// ranges without any download: a real torrent client (no peers, no
// trackers) over the driver, whose storage reads serve the CAS blobs —
// the engine hashes them itself and the pieces report complete. This is
// what keeps partial-CAS re-pulls from re-downloading sealed ranges
// before priorities rise (ImportCatalog calls it right after GotInfo).
func TestVerifyPreSealedCompletesCASHitPieces(t *testing.T) {
	fx := testFixture()
	// Pass 1: download everything into a store we keep (simulated engine).
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := NewForeignStorage(store)
	if err := fs.Register(fx.Infohash, fx.anchorOf(), true); err != nil {
		t.Fatal(err)
	}
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	ih := hashFromHex(t, fx.Infohash)
	ti1, err := fs.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, storageTI{piece: func(p metainfo.Piece) storagePiece { return ti1.Piece(p) }}, info.Piece(i), stream); err != nil {
			t.Fatal(err)
		}
	}
	ti1.Close()

	// A real engine over the same store: every file is a CAS hit.
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT = true
	cfg.DisableIPv6 = true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0 // ephemeral (42069 default collides)
	cfg.DefaultStorage = NewForeignStorage(store)
	// (a separate driver instance registers the anchor for the engine)
	fs2 := NewForeignStorage(store)
	if err := fs2.Register(fx.Infohash, fx.anchorOf(), true); err != nil {
		t.Fatal(err)
	}
	cfg.DefaultStorage = fs2
	tc, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tr, err := tc.AddTorrent(fx.MetaInfo)
	if err != nil {
		t.Fatal(err)
	}
	<-tr.GotInfo()
	drv := fs2.driverFor(fx.Infohash)
	if drv == nil {
		t.Fatal("engine did not open the driver (registration missing?)")
	}
	for i := 0; i < info.NumPieces(); i++ {
		if tr.Piece(i).State().Complete {
			t.Fatalf("piece %d must start incomplete (engine has not hashed the CAS bytes yet)", i)
		}
	}
	drv.VerifyPreSealed(context.Background(), tr)
	for i := 0; i < info.NumPieces(); i++ {
		if !tr.Piece(i).State().Complete {
			t.Fatalf("piece %d over CAS-hit ranges must be complete after VerifyPreSealed — without it the engine re-downloads sealed bytes", i)
		}
	}
}

// (c) wantedFiles table: quant matching, raw semantics for uppercase
// names (000 App. A: the vocabulary is lowercase-only), and both error
// branches. A real torrent with the info (no peers needed).
func TestWantedFilesQuantFiltering(t *testing.T) {
	mkTorrent := func(t *testing.T, files []v1FixtureFile) *torrent.Torrent {
		t.Helper()
		fx := buildV1("owner__repo", files, 64*1024)
		cfg := torrent.NewDefaultClientConfig()
		cfg.DataDir = t.TempDir()
		cfg.NoDHT = true
		cfg.DisableIPv6 = true
		cfg.ListenHost = func(string) string { return "127.0.0.1" }
		cfg.ListenPort = 0 // ephemeral: the fixed default (42069) collides across test clients
		tc, err := torrent.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tc.Close() })
		tr, err := tc.AddTorrent(fx.MetaInfo)
		if err != nil {
			t.Fatal(err)
		}
		<-tr.GotInfo()
		return tr
	}
	ggufRepo := []v1FixtureFile{
		{Path: "config.json", Content: []byte("{}")},
		{Path: "tokenizer.json", Content: []byte("{}")},
		{Path: "model-Q4_K_M.gguf", Content: foreignBytes(64 * 1024)}, // uppercase → raw
		{Path: "model-q5_k_m.gguf", Content: foreignBytes(64 * 1024)}, // lowercase token
	}
	tr := mkTorrent(t, ggufRepo)

	all, err := wantedFiles(tr, "")
	if err != nil || len(all) != 4 {
		t.Fatalf("no selector: want all 4 files, got %v (%v)", all, err)
	}
	q5, err := wantedFiles(tr, "q5_k_m")
	if err != nil {
		t.Fatal(err)
	}
	if !q5["config.json"] || !q5["tokenizer.json"] || !q5["model-q5_k_m.gguf"] || q5["model-Q4_K_M.gguf"] {
		t.Fatalf("--quant q5_k_m must take the matching weights plus companions: %v", q5)
	}
	raw, err := wantedFiles(tr, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if !raw["model-Q4_K_M.gguf"] || raw["model-q5_k_m.gguf"] {
		t.Fatalf("--quant raw must match only token-less (uppercase) GGUFs: %v", raw)
	}
	if _, err := wantedFiles(tr, "q9_9"); err == nil {
		t.Fatal("selector matching no listed weights file must be a loud error")
	}

	stRepo := []v1FixtureFile{
		{Path: "config.json", Content: []byte("{}")},
		{Path: "model.safetensors", Content: foreignBytes(64 * 1024)},
	}
	st := mkTorrent(t, stRepo)
	if _, err := wantedFiles(st, "raw"); err == nil || err.Error() == "" {
		t.Fatal("safetensors-only repo under a selector must refuse loudly (derive-from-bytes, pull without --quant)")
	}
}

// (NIT pin) The anchor size gate must run BEFORE the empty-file branch:
// a live anchor claiming bytes (Size > 0, non-LFS shape: git oid, no
// sha256) on an EMPTY torrent file is a metadata mismatch — refuse at
// open, never seal empty. Same for a pinned non-empty git blob id.
func TestForeignDriverEmptyFileAnchorSizeAndGitMismatch(t *testing.T) {
	fx := testFixture() // carries empty.txt
	cases := []struct {
		name   string
		mutate func(a ForeignFile) ForeignFile
	}{
		{"size>0 on empty torrent file", func(a ForeignFile) ForeignFile {
			a.Size = 123
			return a
		}},
		{"non-empty git blob pinned on empty torrent file", func(a ForeignFile) ForeignFile {
			a.GitSHA1 = fmt.Sprintf("%040x", 7)
			return a
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anchor := fx.anchorOf()
			e := anchor["empty.txt"]
			anchor["empty.txt"] = tc.mutate(e)
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fs := NewForeignStorage(store)
			if err := fs.Register(fx.Infohash, anchor, true); err != nil {
				t.Fatal(err)
			}
			infoV, _ := fx.MetaInfo.UnmarshalInfo()
			ih := hashFromHex(t, fx.Infohash)
			if _, err := fs.OpenTorrent(context.Background(), &infoV, ih); err == nil {
				t.Fatal("empty torrent file with a mismatched anchor record must refuse at open")
			}
		})
	}
}
