package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// buildTestTorrent seals a real artifact via the production math and
// returns everything the driver needs: metainfo, per-path digests, content.
func buildTestTorrent(t *testing.T, files map[string][]byte) (*metainfo.MetaInfo, map[string]FileSpec, string) {
	t.Helper()
	var afs []artifact.File
	for name, content := range files {
		sum := sha256.Sum256(content)
		root := artifact.MerkleRoot(content)
		afs = append(afs, artifact.File{
			Kind:   "weights.gguf",
			Digest: "sha256:" + hex.EncodeToString(sum[:]),
			Size:   int64(len(content)),
			Name:   name,
			BT:     artifact.BT{MerkleRoot: "sha256:" + hex.EncodeToString(root[:])},
		})
	}
	artifact.SortFiles(afs)
	manifestBytes := []byte(`{"schemaVersion":1,"artifactType":"model","files":[]}`)
	manifestSum := sha256.Sum256(manifestBytes)
	manifestHex := hex.EncodeToString(manifestSum[:])
	infoBencode, res, err := artifact.BuildTorrent(afs, manifestBytes, manifestHex, func(name string) ([]byte, error) {
		return files[name], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]string{}
	if err := bencode.Unmarshal(res.PieceLayersBencode, &layers); err != nil {
		t.Fatal(err)
	}
	specs := map[string]FileSpec{}
	for _, f := range afs {
		specs[f.Name] = FileSpec{Digest: artifact.DigestHex(f.Digest), Size: f.Size}
	}
	specs["manifest/sha256-"+manifestHex] = FileSpec{Digest: manifestHex, Size: int64(len(manifestBytes))}
	mi := &metainfo.MetaInfo{InfoBytes: infoBencode, PieceLayers: layers}
	return mi, specs, manifestHex
}

func infohashHex(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	spec, err := torrentSpecFromMetaInfo(mi)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(spec.InfoHashV2.Value[:])
}

func shortInfohashHex(t *testing.T, mi *metainfo.MetaInfo) string {
	t.Helper()
	return infohashHex(t, mi)[:40]
}

// openDriver spins the storage for mi/specs and returns the TorrentImpl.
func openDriver(t *testing.T, store *cas.Store, mi *metainfo.MetaInfo, specs map[string]FileSpec) storage.TorrentImpl {
	t.Helper()
	st := NewCASStorage(store, nil)
	if err := st.Register(infohashHex(t, mi), specs); err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	var ih metainfo.Hash
	spec, _ := torrentSpecFromMetaInfo(mi)
	copy(ih[:], spec.InfoHashV2.Value[:20])
	impl, err := st.OpenTorrent(context.Background(), &info, ih)
	if err != nil {
		t.Fatal(err)
	}
	return impl
}

// writeAllPieces drives the anacrolix writeChunk contract: PIECE-relative
// offsets (msg.Begin within the piece). The simple shape writes each whole
// piece at offset 0 of the piece — chunk sub-offsets exercise the same
// translation.
func writeAllPieces(t *testing.T, impl storage.TorrentImpl, info *metainfo.Info, path string, content []byte) {
	t.Helper()
	for i := 0; i < info.NumPieces(); i++ {
		p := impl.Piece(info.Piece(i))
		if path != "" && !pieceBelongsTo(info, i, path) {
			continue
		}
		off := int64(i) * info.PieceLength // file-relative slice of the CONTENT
		end := off + info.PieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		if _, err := p.WriteAt(content[off:end], 0); err != nil {
			t.Fatalf("write piece %d: %v", i, err)
		}
	}
}

func pieceBelongsTo(info *metainfo.Info, i int, path string) bool {
	fis := info.UpvertedFiles()
	pl := info.PieceLength
	for _, fi := range fis {
		begin := int(fi.TorrentOffset / pl)
		n := (int(fi.Length) + int(pl) - 1) / int(pl)
		if i >= begin && i < begin+n {
			return slashJoin(fi.Path) == path
		}
	}
	return false
}

// torrentSpecFromMetaInfo builds the TorrentSpec for a pure-v2 torrent
// (test-only: production builds specs from reconstructions). NOTE:
// v1.61.0 released panics on pure-v2 specs (issue #1089) — this build
// pins upstream master 4ad31c5 which fixed it; the helper exists so the
// v2-only spec shape is constructed in exactly one place.
func torrentSpecFromMetaInfo(mi *metainfo.MetaInfo) (*torrent.TorrentSpec, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, fmt.Errorf("swarm: unmarshal info: %w", err)
	}
	if info.HasV1() {
		return nil, fmt.Errorf("swarm: torrent carries v1 fields; shardr torrents are pure BTv2 (004 §3)")
	}
	var v2 g.Option[infohash_v2.T]
	v2.Set(infohash_v2.HashBytes(mi.InfoBytes))
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{
			InfoHashV2: v2,
			InfoBytes:  mi.InfoBytes,
		},
		PieceLayers: mi.PieceLayers,
		DisplayName: info.BestName(),
	}, nil
}

// pieceFileStart returns the piece's byte offset within its file (0-based;
// the file-relative position of piece i's first byte).
func pieceFileStart(info *metainfo.Info, i int) int64 {
	fis := info.UpvertedFiles()
	pl := info.PieceLength
	for _, fi := range fis {
		begin := int(fi.TorrentOffset / pl)
		n := (int(fi.Length) + int(pl) - 1) / int(pl)
		if i >= begin && i < begin+n {
			return (int64(i) - int64(begin)) * pl
		}
	}
	return 0
}

// The happy path: writing all pieces of a file and marking them complete
// seals the blob into the CAS with the pinned digest.
func TestStorageSealsFileIntoCAS(t *testing.T) {
	content := deterministicBytes(3<<20, 1) // > pieceLength (1 MiB for this size)
	files := map[string][]byte{"big.gguf": content}
	mi, specs, _ := buildTestTorrent(t, files)
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	impl := openDriver(t, store, mi, specs)
	infoRef, _ := mi.UnmarshalInfo()
	info := &infoRef

	writeAllPieces(t, impl, info, "big.gguf", content)
	for i := 0; i < info.NumPieces(); i++ {
		if pieceBelongsTo(info, i, "big.gguf") {
			if err := impl.Piece(info.Piece(i)).MarkComplete(); err != nil {
				t.Fatalf("mark piece %d: %v", i, err)
			}
		}
	}
	spec := specs["big.gguf"]
	if !store.Has(spec.Digest) {
		t.Fatal("blob not sealed into CAS after all pieces completed")
	}
	f, _ := store.Open(spec.Digest)
	got, _ := io.ReadAll(f)
	if string(got) != string(content) {
		t.Fatal("sealed blob content differs")
	}
}

// The verify-write gate: piece data that passes "completion" but whose
// flat digest does not match the pinned one must never produce a blob.
func TestStorageRejectsWrongBytes(t *testing.T) {
	good := deterministicBytes(2<<20, 2)
	files := map[string][]byte{"big.gguf": good}
	mi, specs, _ := buildTestTorrent(t, files)
	store, _ := cas.Open(t.TempDir())
	impl := openDriver(t, store, mi, specs)
	infoRef, _ := mi.UnmarshalInfo()
	info := &infoRef

	evil := deterministicBytes(2<<20, 3) // same length, wrong bytes
	writeAllPieces(t, impl, info, "big.gguf", evil)
	var markErr error
	for i := 0; i < info.NumPieces(); i++ {
		if pieceBelongsTo(info, i, "big.gguf") {
			if err := impl.Piece(info.Piece(i)).MarkComplete(); err != nil && markErr == nil {
				markErr = err
			}
		}
	}
	if markErr == nil {
		t.Fatal("seal of wrong bytes must fail loudly")
	}
	if store.Has(specs["big.gguf"].Digest) {
		t.Fatal("CAS must not contain the blob after a failed verify-write")
	}
	// The part file is gone too: no unverified residue.
	entries, _ := os.ReadDir(store.Root + "/incoming")
	if len(entries) != 0 {
		t.Fatalf("incoming/ must be empty after failed seal, got %d entries", len(entries))
	}
}

// Unpinned files (two-phase import) refuse writes and report unknown
// completion — the engine must never store bytes it cannot pin.
func TestStorageRefusesUnpinnedWrites(t *testing.T) {
	content := deterministicBytes(1<<20, 4)
	files := map[string][]byte{"a.gguf": content}
	mi, specs, _ := buildTestTorrent(t, files)
	delete(specs, "a.gguf") // simulate phase-1: only manifest pinned
	store, _ := cas.Open(t.TempDir())
	impl := openDriver(t, store, mi, specs)
	infoRef, _ := mi.UnmarshalInfo()
	info := &infoRef

	for i := 0; i < info.NumPieces(); i++ {
		if pieceBelongsTo(info, i, "a.gguf") {
			p := impl.Piece(info.Piece(i))
			if _, err := p.WriteAt(content[:16], 0); err == nil {
				t.Fatal("write into unpinned file must fail")
			}
			if c := p.Completion(); c.Ok {
				t.Fatal("unpinned completion must be unknown (Ok=false)")
			}
		}
	}
}

// Sealed (CAS-present) files serve reads from the blob and refuse
// writes. Multi-piece on purpose: completion is engine-verified for
// multi-piece CAS hits (single-piece ones are complete on first pull).
func TestStorageSealedFileServesReads(t *testing.T) {
	content := deterministicBytes(3<<20, 5) // 3 pieces at 1 MiB piece length
	files := map[string][]byte{"a.gguf": content}
	mi, specs, _ := buildTestTorrent(t, files)
	store, _ := cas.Open(t.TempDir())
	if err := store.Put(specs["a.gguf"].Digest, bytesReader(content)); err != nil {
		t.Fatal(err)
	}
	impl := openDriver(t, store, mi, specs)
	infoRef, _ := mi.UnmarshalInfo()
	info := &infoRef

	for i := 0; i < info.NumPieces(); i++ {
		if !pieceBelongsTo(info, i, "a.gguf") {
			continue
		}
		p := impl.Piece(info.Piece(i))
		if _, err := p.WriteAt(content[:8], 0); err == nil {
			t.Fatal("write into sealed blob refused")
		}
		c := p.Completion()
		if !c.Ok || c.Complete {
			// Complete only after the engine verified the piece — a CAS hit
			// alone is not an announcement (see Completion).
			t.Fatalf("sealed-but-unchecked completion = %+v", c)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatalf("MarkComplete on sealed piece: %v", err)
		}
		// PIECE-relative read (anacrolix contract): offset 0 of piece i is
		// the piece's first bytes in the file, not the file's first bytes.
		start := pieceFileStart(info, i)
		buf := make([]byte, 8)
		if _, err := p.ReadAt(buf, 0); err != nil || string(buf) != string(content[start:start+8]) {
			t.Fatalf("sealed read of piece %d: %v %q", i, err, buf)
		}
	}
	for i := 0; i < info.NumPieces(); i++ {
		if !pieceBelongsTo(info, i, "a.gguf") {
			continue
		}
		if c := impl.Piece(info.Piece(i)).Completion(); !c.Ok || !c.Complete {
			t.Fatalf("engine-verified completion = %+v", c)
		}
	}
}

func TestStorageUnregisteredTorrentIsLoud(t *testing.T) {
	content := deterministicBytes(1<<20, 6)
	files := map[string][]byte{"a.gguf": content}
	mi, _, _ := buildTestTorrent(t, files)
	store, _ := cas.Open(t.TempDir())
	st := NewCASStorage(store, nil)
	infoRef, _ := mi.UnmarshalInfo()
	info := &infoRef
	if _, err := st.OpenTorrent(context.Background(), info, metainfo.Hash{}); err == nil {
		t.Fatal("unregistered torrent must be a loud error")
	}
}

func deterministicBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + int(seed)*7)
	}
	return b
}

func bytesReader(b []byte) io.Reader { return &byteSliceReader{b, 0} }

type byteSliceReader struct {
	b []byte
	i int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// Crafted info dicts with traversal-shaped paths are rejected at the
// storage trust boundary — no span table entries for non-canonical paths.
func TestStorageRejectsTraversalPaths(t *testing.T) {
	content := deterministicBytes(1<<20, 7)
	files := map[string][]byte{"a.gguf": content}
	mi, specs, _ := buildTestTorrent(t, files)
	root := artifact.MerkleRoot(content)
	store, _ := cas.Open(t.TempDir())
	st := NewCASStorage(store, nil)
	if err := st.Register(infohashHex(t, mi), specs); err != nil {
		t.Fatal(err)
	}
	// Rebuild the info dict with a traversal-named file: same shape, path
	// ../escape.gguf (raw map — the typed FileTree API is awkward here).
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	evilInfo := map[string]any{
		"file tree": map[string]any{
			"..": map[string]any{"": map[string]any{"length": int64(len(content)), "pieces root": string(root[:])}},
		},
		"meta version": int64(2),
		"name":         info.Name,
		"piece length": info.PieceLength,
	}
	ib := bencodeMap(evilInfo)
	var parsed metainfo.Info
	if err := bencode.Unmarshal(ib, &parsed); err != nil {
		t.Fatal(err)
	}
	// Register under the EVIL torrent's own short hash so the only
	// remaining rejection reason can be the traversal path itself.
	v2 := infohash_v2.HashBytes(ib)
	specs["../escape.gguf"] = FileSpec{Digest: artifact.DigestHex(artifact.Digest(content)), Size: int64(len(content))}
	if err := st.Register(hex.EncodeToString(v2[:]), specs); err != nil {
		t.Fatal(err)
	}
	var ih metainfo.Hash
	copy(ih[:], v2[:20])
	if _, err := st.OpenTorrent(context.Background(), &parsed, ih); err == nil {
		t.Fatal("traversal path must be rejected at OpenTorrent")
	}
}

func bencodeMap(v any) []byte {
	b, err := bencode.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
