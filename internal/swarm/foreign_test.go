package swarm

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// Direct driver simulation: the engine contract (WriteAt chunks in any
// order, MarkComplete once the piece hash passed, reads for checks and
// seeding) exercised without a network. Fixture deliberately crosses
// piece→file boundaries: v1 pieces span files, the driver must split.

func openFixtureStorage(t *testing.T, fx *v1Fixture, anchor map[string]ForeignFile, strict bool) (*ForeignStorage, *foreignTorrent, storageTI) {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := NewForeignStorage(store)
	if err := fs.Register(fx.Infohash, anchor, strict); err != nil {
		t.Fatal(err)
	}
	infoV, err := fx.MetaInfo.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info := &infoV
	ih := hashFromHex(t, fx.Infohash)
	ti, err := fs.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ti.Close() })
	drv := fs.driverFor(fx.Infohash)
	if drv == nil {
		t.Fatal("driver not open")
	}
	return fs, drv, storageTI{piece: func(p metainfo.Piece) storagePiece { return ti.Piece(p) }}
}

// storageTI narrows storage.TorrentImpl (func fields, not methods) to
// an interface the simulations can call.
type storageTI struct {
	piece func(p metainfo.Piece) storagePiece
}

type storagePiece = interface {
	io.ReaderAt
	io.WriterAt
	MarkComplete() error
	MarkNotComplete() error
	Completion() storage.Completion
}

func (a storageTI) Piece(p metainfo.Piece) storagePiece { return a.piece(p) }

// downloadPiece simulates the engine writing one piece: chunks of 16KiB
// in REVERSE order (out-of-order writes are the norm), then completion.
func downloadPiece(t *testing.T, ti storageTI, p metainfo.Piece, stream []byte) error {
	t.Helper()
	pi := ti.Piece(p)
	const chunk = 16 << 10
	b := stream[p.Offset() : p.Offset()+p.Length()]
	for end := len(b); end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		if _, err := pi.WriteAt(b[start:end], int64(start)); err != nil {
			return err
		}
		end = start
	}
	return pi.MarkComplete()
}

func hashFromHex(t *testing.T, hexStr string) metainfo.Hash {
	t.Helper()
	var ih metainfo.Hash
	if _, err := hex.Decode(ih[:], []byte(hexStr)); err != nil {
		t.Fatal(err)
	}
	return ih
}

func testFixture() *v1Fixture {
	files := []v1FixtureFile{
		{Path: "config.json", Content: bytes.Repeat([]byte(`{"model_type":"toy"}`), 60)}, // ~960 B
		{Path: "model.safetensors", Content: foreignBytes(300*1024 + 123)},
		{Path: "tokenizer.json", Content: []byte(`{"tokens":[]}`)},
		// v1-legal empty file: no stream bytes, no pieces — must still
		// seal (at open, under the empty digest) or the wanted target is
		// unreachable and the pull hangs.
		{Path: "empty.txt", Content: nil},
	}
	return buildV1("owner__repo", files, 64*1024)
}

func foreignBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestForeignDriverSealsEveryFile(t *testing.T) {
	fx := testFixture()
	_, drv, ti := openFixtureStorage(t, fx, fx.anchorOf(), true)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti, info.Piece(i), stream); err != nil {
			t.Fatalf("piece %d: %v", i, err)
		}
	}
	sealed := drv.SealedFiles()
	if len(sealed) != len(fx.Files) {
		t.Fatalf("want %d sealed files, got %v", len(fx.Files), sortedKeys(sealed))
	}
	for _, f := range fx.Files {
		if sealed[f.Path] != sha256Hex(f.Content) {
			t.Fatalf("digest mismatch for %s: %s", f.Path, sealed[f.Path])
		}
	}
	// Seed reads: a read spanning the sealed file boundary (piece that
	// spans config.json → model.safetensors) must serve from CAS blobs.
	p0 := info.Piece(0)
	got := make([]byte, p0.Length())
	if _, err := ti.Piece(p0).ReadAt(got, 0); err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if !bytes.Equal(got, stream[:p0.Length()]) {
		t.Fatal("seed read across the file boundary returned wrong bytes")
	}
}

// The digest gate (mutation-proven guard): an anchor digest that does
// not match the downloaded bytes must fail the seal loudly — deleting
// the check (or weakening it to a warning) turns this red.
func TestForeignDriverRefusesAnchorMismatch(t *testing.T) {
	fx := testFixture()
	anchor := fx.anchorOf()
	tampered := anchor["model.safetensors"]
	tampered.SHA256 = strings.Repeat("f", 64) // prepared wrong digest
	anchor["model.safetensors"] = tampered
	_, drv, ti := openFixtureStorage(t, fx, anchor, true)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	var err error
	for i := 0; i < info.NumPieces(); i++ {
		if err = downloadPiece(t, ti, info.Piece(i), stream); err != nil {
			break
		}
	}
	if err == nil {
		if err = drv.WaitSealedWanted(context.Background(), wantedOf(fx)); err == nil {
			t.Fatal("tampered anchor digest must refuse the seal")
		}
	}
	if want := "anchor mismatch"; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Fatalf("error must name the anchor mismatch: %v", err)
	}
}

// Git-blob gate: the secondary anchor for non-LFS files must also fail
// loudly on its own.
func TestForeignDriverRefusesGitBlobMismatch(t *testing.T) {
	fx := testFixture()
	anchor := fx.anchorOf()
	cfgA := anchor["config.json"]
	cfgA.GitSHA1 = fmt.Sprintf("%040x", 1) // prepared wrong git id
	anchor["config.json"] = cfgA
	_, _, ti := openFixtureStorage(t, fx, anchor, true)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	if err := downloadPiece(t, ti, info.Piece(0), stream); err == nil {
		t.Fatal("wrong git blob id must refuse the seal")
	}
}

// Strict mode: a torrent file outside the anchor is refused at open —
// live anchors cover everything or the pull does not happen.
func TestForeignDriverStrictRefusesUnanchoredFile(t *testing.T) {
	fx := testFixture()
	anchor := fx.anchorOf()
	delete(anchor, "tokenizer.json")
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := NewForeignStorage(store)
	if err := fs.Register(fx.Infohash, anchor, true); err != nil {
		t.Fatal(err)
	}
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	ih := hashFromHex(t, fx.Infohash)
	if _, err := fs.OpenTorrent(context.Background(), info, ih); err == nil {
		t.Fatal("strict mode must refuse a torrent file outside the anchor")
	}
}

// Trust mode: uncovered files (catalog checksum record covers LFS
// weights only) ride on the torrent's piece hashes and seal under their
// computed digest.
func TestForeignDriverTrustModeAllowsUnpinned(t *testing.T) {
	fx := testFixture()
	anchor := map[string]ForeignFile{}
	f := fx.Files[1] // the weights file only
	anchor[f.Path] = ForeignFile{Path: f.Path, Size: int64(len(f.Content)), SHA256: sha256Hex(f.Content)}
	_, drv, ti := openFixtureStorage(t, fx, anchor, false)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti, info.Piece(i), stream); err != nil {
			t.Fatalf("piece %d: %v", i, err)
		}
	}
	sealed := drv.SealedFiles()
	if len(sealed) != len(fx.Files) {
		t.Fatalf("trust mode must seal every file (unpinned under computed digests), got %v", sortedKeys(sealed))
	}
	if sealed["tokenizer.json"] != sha256Hex(fx.Files[2].Content) {
		t.Fatal("unpinned file must seal under its computed sha256")
	}
}

// CAS hits: re-opening with the blobs already present seals at open and
// serves reads; completion flips only through MarkComplete (the engine
// hashing the bytes — never trust a CAS hit without it).
func TestForeignDriverCASHitSeedsAndCompletesViaEngine(t *testing.T) {
	fx := testFixture()
	// First pass: download everything into a store we keep.
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
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	ih := hashFromHex(t, fx.Infohash)
	ti, err := fs.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, storageTI{piece: func(p metainfo.Piece) storagePiece { return ti.Piece(p) }}, info.Piece(i), stream); err != nil {
			t.Fatal(err)
		}
	}
	ti.Close()

	// Second open on the same store: CAS hits everywhere.
	fs2 := NewForeignStorage(store)
	if err := fs2.Register(fx.Infohash, fx.anchorOf(), true); err != nil {
		t.Fatal(err)
	}
	ti2, err := fs2.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	defer ti2.Close()
	ti2a := storageTI{piece: func(p metainfo.Piece) storagePiece { return ti2.Piece(p) }}
	drv2 := fs2.driverFor(fx.Infohash)
	if got := len(drv2.SealedFiles()); got != len(fx.Files) {
		t.Fatalf("CAS hits must be sealed at open, got %d", got)
	}
	p0 := info.Piece(0)
	if c := ti2a.Piece(p0).Completion(); c.Ok && c.Complete {
		t.Fatal("CAS hit must not count as complete before the engine hashes it")
	}
	got := make([]byte, p0.Length())
	if _, err := ti2a.Piece(p0).ReadAt(got, 0); err != nil {
		t.Fatalf("seed read from CAS hit: %v", err)
	}
	if !bytes.Equal(got, stream[:p0.Length()]) {
		t.Fatal("CAS-hit seed read returned wrong bytes")
	}
	if err := ti2a.Piece(p0).MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if c := ti2a.Piece(p0).Completion(); !c.Ok || !c.Complete {
		t.Fatal("engine-verified piece must report complete after MarkComplete")
	}
}

// wantedOf indexes the fixture paths (the wanted set).
func wantedOf(fx *v1Fixture) map[string]bool {
	m := make(map[string]bool, len(fx.Files))
	for _, f := range fx.Files {
		m[f.Path] = true
	}
	return m
}

// Empty torrent files (v1-legal) seal at open under the empty digest and
// count toward the wanted target — before the fix they had no state at
// all and WaitSealedWanted hung forever.
func TestForeignDriverEmptyFileSealsAtOpen(t *testing.T) {
	fx := testFixture()
	_, drv, ti := openFixtureStorage(t, fx, fx.anchorOf(), true)
	infoV, _ := fx.MetaInfo.UnmarshalInfo()
	info := &infoV
	if got := drv.SealedFiles()["empty.txt"]; got != emptyContentSHA256 {
		t.Fatalf("empty file must be sealed at open under the empty digest, got %q", got)
	}
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti, info.Piece(i), stream); err != nil {
			t.Fatalf("piece %d: %v", i, err)
		}
	}
	if err := drv.WaitSealedWanted(context.Background(), wantedOf(fx)); err != nil {
		t.Fatalf("empty file must not block the target: %v", err)
	}
}

// An anchor pinning an EMPTY torrent file with any other digest is an
// anchor/torrent mismatch: refuse at open (strict semantics intact).
func TestForeignDriverEmptyFileAnchorMismatch(t *testing.T) {
	fx := testFixture()
	anchor := fx.anchorOf()
	e := anchor["empty.txt"]
	e.SHA256 = strings.Repeat("f", 64)
	anchor["empty.txt"] = e
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
		t.Fatal("anchor digest mismatch on an empty file must refuse at open")
	}
}

// Anchor size mismatch at open: the torrent is not what the anchor
// describes — refuse before a single byte moves.
func TestForeignDriverRefusesAnchorSizeMismatch(t *testing.T) {
	fx := testFixture()
	anchor := fx.anchorOf()
	a := anchor["config.json"]
	a.Size++ // anchor says 961 bytes, torrent says 960
	anchor["config.json"] = a
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
		t.Fatal("anchor size mismatch must refuse at open")
	}
}

// Partial-CAS re-pull liveness (the C1a hang): after a first full pass,
// a second registration where one file is a CAS hit (sealed at open)
// and the others are NOT must still complete — the engine re-downloads
// pieces that overlap the sealed range, and those duplicate writes into
// the sealed file are dropped+acked (never an error that would disable
// the engine's download), with verification reading the CAS bytes.
func TestForeignDriverPartialCASRepullSurvivesSealedWrites(t *testing.T) {
	fx := testFixture()
	// Pass 1: full download into a store we keep.
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
	ti1a := storageTI{piece: func(p metainfo.Piece) storagePiece { return ti1.Piece(p) }}
	var stream []byte
	for _, f := range fx.Files {
		stream = append(stream, f.Content...)
	}
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti1a, info.Piece(i), stream); err != nil {
			t.Fatal(err)
		}
	}
	ti1.Close()

	// Pass 2: trust-mode anchor covering ONLY the weights file — it is a
	// CAS hit (sealed at open), the companions re-download.
	weights := fx.Files[1]
	fs2 := NewForeignStorage(store)
	anchor2 := map[string]ForeignFile{
		weights.Path: {Path: weights.Path, Size: int64(len(weights.Content)), SHA256: sha256Hex(weights.Content)},
	}
	if err := fs2.Register(fx.Infohash, anchor2, false); err != nil {
		t.Fatal(err)
	}
	ti2, err := fs2.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	defer ti2.Close()
	ti2a := storageTI{piece: func(p metainfo.Piece) storagePiece { return ti2.Piece(p) }}
	drv2 := fs2.driverFor(fx.Infohash)
	if got := drv2.SealedFiles()[weights.Path]; got != sha256Hex(weights.Content) {
		t.Fatalf("weights file must be a sealed CAS hit, got %q", got)
	}
	// Full re-download simulation: writes overlapping the sealed range
	// must be ACKED (dropped), everything else downloads normally.
	for i := 0; i < info.NumPieces(); i++ {
		if err := downloadPiece(t, ti2a, info.Piece(i), stream); err != nil {
			t.Fatalf("piece %d overlapping sealed ranges must be droppable, got: %v", i, err)
		}
	}
	if err := drv2.WaitSealedWanted(context.Background(), wantedOf(fx)); err != nil {
		t.Fatalf("partial-CAS re-pull must complete: %v", err)
	}
	sealed := drv2.SealedFiles()
	if sealed[weights.Path] != sha256Hex(weights.Content) {
		t.Fatalf("sealed digest must be unchanged by dropped writes: %s", sealed[weights.Path])
	}
	if sealed["config.json"] != sha256Hex(fx.Files[0].Content) {
		t.Fatal("unpinned companion must seal under its computed digest")
	}
	// Seed read across the sealed/unsealed boundary still serves the
	// CAS bytes for the sealed side.
	p0 := info.Piece(0)
	got := make([]byte, p0.Length())
	if _, err := ti2a.Piece(p0).ReadAt(got, 0); err != nil {
		t.Fatalf("seed read after re-pull: %v", err)
	}
	if !bytes.Equal(got, stream[:p0.Length()]) {
		t.Fatal("seed read after re-pull returned wrong bytes")
	}
}
