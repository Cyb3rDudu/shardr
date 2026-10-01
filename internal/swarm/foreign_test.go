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
		if err = drv.WaitSealed(context.Background(), len(fx.Files)); err == nil {
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
