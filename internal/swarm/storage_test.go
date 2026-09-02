package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

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

func writeAllPieces(t *testing.T, impl storage.TorrentImpl, info *metainfo.Info, path string, content []byte) {
	t.Helper()
	for i := 0; i < info.NumPieces(); i++ {
		p := impl.Piece(info.Piece(i))
		off := int64(i) * info.PieceLength
		end := off + info.PieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		if path != "" && !pieceBelongsTo(info, i, path) {
			continue
		}
		if _, err := p.WriteAt(content[off:end], off-info.PieceLength*int64(pieceFileIndex(info, i))); err != nil {
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

func pieceFileIndex(info *metainfo.Info, i int) int {
	fis := info.UpvertedFiles()
	pl := info.PieceLength
	for _, fi := range fis {
		begin := int(fi.TorrentOffset / pl)
		n := (int(fi.Length) + int(pl) - 1) / int(pl)
		if i >= begin && i < begin+n {
			return begin
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

// Sealed (CAS-present) files serve reads from the blob and refuse writes.
func TestStorageSealedFileServesReads(t *testing.T) {
	content := deterministicBytes(1<<20, 5)
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
		if !c.Ok || !c.Complete {
			t.Fatalf("sealed completion = %+v", c)
		}
		buf := make([]byte, 8)
		if _, err := p.ReadAt(buf, 0); err != nil || string(buf) != string(content[:8]) {
			t.Fatalf("sealed read: %v %q", err, buf)
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
