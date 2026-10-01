package swarm

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// ---------------------------------------------------------------------------
// Foreign torrent storage (Epic #65, good-citizen mode): community
// catalog torrents are FOREIGN — BitTorrent v1, upstream repo layout,
// their own piece layout — never shardr's deterministic v2 torrents
// (004 §3). They run on a dedicated engine with this CAS-backed v1
// driver: v1 pieces map onto the concatenated byte stream (pieces may
// span files), every write splits across the files it touches, and a
// file seals independently when the last piece covering it completes —
// engine SHA-1 piece check first, then the anchor gate: flat sha256
// (+ git blob sha1 for non-LFS HF files) must match the anchor of
// record, else loud failure. Sealed blobs serve seed reads straight from
// the CAS. Unpinned files (catalog-trust mode, no recorded digest) seal
// under their computed digest: the listed torrent's own piece hashes are
// then the only vouching, which is exactly what accepting the catalog's
// trust means.
//
// The shardr v2 path (CASStorage) is untouched: foreign torrents never
// enter it (fail-closed OpenTorrent there rejects v1, 004 §3).
// ---------------------------------------------------------------------------

// ForeignFile pins one foreign-torrent file to its anchor.
type ForeignFile struct {
	Path    string // torrent-relative (== repo path)
	Size    int64
	SHA256  string // bare 64-hex anchor digest; "" = unpinned
	GitSHA1 string // 40-hex git blob id (non-LFS HF anchor); "" = none
}

// foreignReg is one torrent's registration: the anchor set plus its
// strictness. Strict (live HF anchor): every torrent file must be
// covered — a file outside the anchor is unanchorable and refused.
// Trust (rescued, --trust-catalog): uncovered files ride on the
// torrent's own piece hashes.
type foreignReg struct {
	files  map[string]ForeignFile
	strict bool
}

// ForeignStorage is the v1 CAS driver, keyed by v1 infohash (40 hex).
type ForeignStorage struct {
	store *cas.Store

	mu   sync.Mutex
	reg  map[string]foreignReg // 40-hex infohash → registration
	open map[string]*foreignTorrent
}

// NewForeignStorage builds the driver.
func NewForeignStorage(store *cas.Store) *ForeignStorage {
	return &ForeignStorage{
		store: store,
		reg:   map[string]foreignReg{},
		open:  map[string]*foreignTorrent{},
	}
}

// Register pins a foreign torrent's anchor BEFORE it is added.
func (s *ForeignStorage) Register(v1InfohashHex string, files map[string]ForeignFile, strict bool) error {
	if len(v1InfohashHex) != 40 {
		return fmt.Errorf("swarm: foreign: register: infohash must be 40 hex chars (v1), got %q", v1InfohashHex)
	}
	for path, f := range files {
		if f.SHA256 != "" && len(f.SHA256) != 64 {
			return fmt.Errorf("swarm: foreign: register: file %q digest must be bare 64-hex", path)
		}
	}
	s.mu.Lock()
	s.reg[v1InfohashHex] = foreignReg{files: files, strict: strict}
	s.mu.Unlock()
	return nil
}

// OpenTorrent implements storage.ClientImpl (v1 only).
func (s *ForeignStorage) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	if info.HasV2() {
		return storage.TorrentImpl{}, fmt.Errorf("swarm: foreign: torrent %x carries v2 fields; catalog torrents are pure v1 (shardr's own v2 torrents use the native path, 004 §3)", infoHash)
	}
	key := fmt.Sprintf("%x", infoHash)
	s.mu.Lock()
	reg, ok := s.reg[key]
	s.mu.Unlock()
	if !ok {
		return storage.TorrentImpl{}, fmt.Errorf("swarm: foreign: torrent %s is not registered with anchor digests; refusing to store unpinned bytes", key)
	}
	t, err := newForeignTorrent(s, key, info, reg)
	if err != nil {
		return storage.TorrentImpl{}, err
	}
	s.mu.Lock()
	s.open[key] = t
	s.mu.Unlock()
	return storage.TorrentImpl{
		Piece:         t.piece,
		PieceWithHash: func(p metainfo.Piece, _ g.Option[[]byte]) storage.PieceImpl { return t.piece(p) },
		Close:         t.close,
	}, nil
}

// driverFor returns the live driver (nil when not open).
func (s *ForeignStorage) driverFor(v1InfohashHex string) *foreignTorrent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open[v1InfohashHex]
}

// foreignFileRange is one file's slice of the concatenated v1 stream.
type foreignFileRange struct {
	path       string
	begin, end int64 // absolute byte range [begin, end)
}

// foreignTorrent is the per-torrent v1 driver.
type foreignTorrent struct {
	parent   *ForeignStorage
	key      string // 40-hex v1 infohash
	info     *metainfo.Info
	strict   bool
	files    []foreignFileRange // ordered by begin
	states   map[string]*foreignState
	mu       sync.Mutex
	closed   bool
	doneCond *sync.Cond
}

type foreignState struct {
	name    string
	anchor  ForeignFile
	part    *os.File
	sealed  bool
	digest  string // sealed: computed sha256 (== anchor.SHA256 when pinned)
	failed  error
	pieces  []bool // one bit per piece overlapping the file
	numDone int
}

func newForeignTorrent(parent *ForeignStorage, key string, info *metainfo.Info, reg foreignReg) (*foreignTorrent, error) {
	t := &foreignTorrent{parent: parent, key: key, info: info, strict: reg.strict}
	t.doneCond = sync.NewCond(&t.mu)
	t.states = map[string]*foreignState{}
	fis := info.UpvertedFiles()
	sort.Slice(fis, func(i, j int) bool { return fis[i].TorrentOffset < fis[j].TorrentOffset })
	pl := int64(info.PieceLength)
	var off int64
	for _, fi := range fis {
		if !canonicalTorrentPath(fi.Path) {
			return nil, fmt.Errorf("swarm: foreign: torrent file tree contains non-canonical path %q (empty/./.. segments rejected)", slashJoin(fi.Path))
		}
		path := slashJoin(fi.Path)
		if fi.Length == 0 {
			continue // v1 allows empty files; they occupy no bytes
		}
		begin, end := off, off+fi.Length
		off = end
		t.files = append(t.files, foreignFileRange{path: path, begin: begin, end: end})
		a, anchored := reg.files[path]
		if anchored && a.Size != 0 && a.Size != fi.Length {
			return nil, fmt.Errorf("swarm: foreign: anchor size mismatch for %q: anchor says %d bytes, torrent says %d — the torrent is not what the anchor describes; refusing", path, a.Size, fi.Length)
		}
		if !anchored && reg.strict {
			return nil, fmt.Errorf("swarm: foreign: torrent carries %q outside the anchor (%d pinned files) — unanchorable bytes; refusing (E_NOT_ANCHORED class)", path, len(reg.files))
		}
		n := int((end-1)/pl) - int(begin/pl) + 1
		st := &foreignState{name: path, anchor: ForeignFile{Path: path, Size: fi.Length, SHA256: a.SHA256, GitSHA1: a.GitSHA1}, pieces: make([]bool, n)}
		if a.SHA256 != "" && parent.store != nil && parent.store.Has(a.SHA256) {
			st.sealed = true // CAS hit: seed-side; engine piece checks confirm below
			st.digest = a.SHA256
		}
		t.states[path] = st
	}
	return t, nil
}

// filesInRange returns the files overlapping absolute byte range [b, e).
func (t *foreignTorrent) filesInRange(b, e int64) []foreignFileRange {
	var out []foreignFileRange
	for _, f := range t.files {
		if f.begin >= e {
			break
		}
		if f.end > b {
			out = append(out, f)
		}
	}
	return out
}

// pieceFor computes the local bit index range of piece [b,e) inside file f.
func pieceBits(f foreignFileRange, b, e, pl int64) (first, last int) {
	first = int(b / pl)
	if f.begin > b {
		first = int(f.begin / pl)
	}
	last = int((e - 1) / pl)
	if f.end < e {
		last = int((f.end - 1) / pl)
	}
	return first, last
}

// piece returns the PieceImpl for a global v1 piece index.
func (t *foreignTorrent) piece(p metainfo.Piece) storage.PieceImpl {
	return &foreignPiece{t: t, off: p.Offset(), len: p.Length()}
}

func (t *foreignTorrent) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, st := range t.states {
		if st.part != nil {
			st.part.Close()
			if !st.sealed {
				os.Remove(st.part.Name())
			}
		}
	}
	t.doneCond.Broadcast()
	t.parent.mu.Lock()
	delete(t.parent.open, t.key)
	t.parent.mu.Unlock()
	return nil
}

// SealedCount reports sealed files (progress).
func (t *foreignTorrent) SealedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, st := range t.states {
		if st.sealed {
			n++
		}
	}
	return n
}

// SealedFiles returns the sealed files' computed digests (import sources).
func (t *foreignTorrent) SealedFiles() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]string{}
	for path, st := range t.states {
		if st.sealed {
			out[path] = st.digest
		}
	}
	return out
}

// WaitSealed blocks until n files are sealed, the driver closes, or an
// anchor verification fails.
func (t *foreignTorrent) WaitSealed(ctx context.Context, n int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done := 0
		for _, st := range t.states {
			if st.failed != nil {
				return fmt.Errorf("swarm: foreign: anchor verification failed: %w", st.failed)
			}
			if st.sealed {
				done++
			}
		}
		if done >= n {
			return nil
		}
		if t.closed {
			return fmt.Errorf("swarm: foreign: driver closed before %d files sealed (got %d)", n, done)
		}
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				t.mu.Lock()
				t.doneCond.Broadcast()
				t.mu.Unlock()
			case <-stop:
			}
		}()
		t.doneCond.Wait()
		close(stop)
	}
}

// VerifySealed drives anacrolix piece checks over every piece that
// overlaps a sealed file (seed-start proof: the engine itself must hash
// the bytes before this node announces them — a CAS hit is never
// trusted without the engine verifying, 003 §4).
func (t *foreignTorrent) VerifySealed(ctx context.Context, tr *torrent.Torrent) error {
	total := t.info.TotalLength()
	pl := int64(t.info.PieceLength)
	t.mu.Lock()
	var pieces []int
	for i := 0; i < t.info.NumPieces(); i++ {
		b := int64(i) * pl
		e := b + pl
		if e > total {
			e = total
		}
		for _, f := range t.files {
			if f.begin >= e {
				break
			}
			if f.end > b && t.states[f.path].sealed {
				pieces = append(pieces, i)
				break
			}
		}
	}
	t.mu.Unlock()
	for _, i := range pieces {
		if err := tr.Piece(i).VerifyDataContext(ctx); err != nil {
			return fmt.Errorf("swarm: foreign: verify piece %d: %w", i, err)
		}
		if !tr.Piece(i).State().Complete {
			return fmt.Errorf("swarm: foreign: sealed bytes of a file under piece %d fail their piece check (corrupt or anchor mismatch)", i)
		}
	}
	return nil
}

// foreignPiece is the per-piece view over the concatenated v1 stream.
type foreignPiece struct {
	t   *foreignTorrent
	off int64 // absolute offset of the piece in the stream
	len int64
}

// writeAtLocked writes b at absolute stream offset abs, splitting
// across the files it spans. Caller holds t.mu.
func (p *foreignPiece) writeAtLocked(abs int64, b []byte) error {
	for len(b) > 0 {
		fs := p.t.filesInRange(abs, abs+int64(len(b)))
		if len(fs) == 0 {
			return fmt.Errorf("swarm: foreign: write past the file tree at offset %d", abs)
		}
		f := fs[0]
		n := int64(len(b))
		if f.end-abs < n {
			n = f.end - abs
		}
		st := p.t.states[f.path]
		if st.failed != nil {
			return st.failed
		}
		if st.sealed {
			return fmt.Errorf("swarm: foreign: write into sealed blob %s refused (CAS is immutable)", st.digest)
		}
		if st.part == nil {
			part, err := p.t.parent.newPart("foreign")
			if err != nil {
				return err
			}
			st.part = part
		}
		if _, err := st.part.WriteAt(b[:n], abs-f.begin); err != nil {
			return err
		}
		abs += n
		b = b[n:]
	}
	return nil
}

func (p *foreignPiece) WriteAt(b []byte, off int64) (int, error) {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	if err := p.writeAtLocked(p.off+off, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *foreignPiece) ReadAt(b []byte, off int64) (int, error) {
	abs := p.off + off
	read := 0
	for read < len(b) {
		fs := p.t.filesInRange(abs+int64(read), abs+int64(len(b)))
		if len(fs) == 0 {
			break
		}
		f := fs[0]
		n := int64(len(b) - read)
		if f.end-(abs+int64(read)) < n {
			n = f.end - (abs + int64(read))
		}
		if n <= 0 {
			break
		}
		p.t.mu.Lock()
		st := p.t.states[f.path]
		var src io.ReaderAt
		switch {
		case st.sealed:
			fl, err := p.t.parent.store.Open(st.digest)
			if err != nil {
				p.t.mu.Unlock()
				return read, fmt.Errorf("swarm: foreign: open sealed blob %s: %w", st.digest, err)
			}
			defer fl.Close()
			src = fl
		case st.part != nil:
			src = st.part
		default:
			p.t.mu.Unlock()
			return read, fmt.Errorf("swarm: foreign: read of unstarted bytes in %q at offset %d (not downloading)", f.path, abs+int64(read))
		}
		p.t.mu.Unlock()
		m, err := src.ReadAt(b[read:int64(read)+n], abs+int64(read)-f.begin)
		read += m
		if err != nil {
			return read, err
		}
	}
	if read == 0 {
		return 0, fmt.Errorf("swarm: foreign: empty read at offset %d", abs)
	}
	return read, nil
}

func (p *foreignPiece) MarkComplete() error {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	pl := int64(p.t.info.PieceLength)
	b, e := p.off, p.off+p.len
	for _, f := range p.t.filesInRange(b, e) {
		st := p.t.states[f.path]
		first, last := pieceBits(f, b, e, pl)
		for i := first; i <= last; i++ {
			bit := i - int(f.begin/pl)
			if bit < 0 || bit >= len(st.pieces) || st.pieces[bit] {
				continue
			}
			st.pieces[bit] = true
			st.numDone++
			if st.numDone == len(st.pieces) && !st.sealed {
				if err := p.sealLocked(st); err != nil {
					st.failed = err
					p.t.doneCond.Broadcast()
					return err
				}
			}
		}
	}
	p.t.doneCond.Broadcast()
	return nil
}

// sealLocked finalizes a fully piece-covered file: the anchor gate
// (flat sha256, and the git blob sha1 when the anchor carries one —
// both must match the anchor of record), then the CAS verify-write.
// A mismatch is a loud failure: the downloaded bytes are not what the
// anchor describes. Unpinned files (catalog trust) seal under their
// computed digest — the torrent's piece hashes vouch for them.
func (p *foreignPiece) sealLocked(st *foreignState) error {
	if st.part == nil {
		return fmt.Errorf("swarm: foreign: seal with no part for %q", st.name)
	}
	if _, err := st.part.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stat, err := st.part.Stat()
	if err != nil {
		return err
	}
	if stat.Size() != st.anchor.Size {
		return fmt.Errorf("size mismatch for %q: downloaded %d bytes, expected %d", st.name, stat.Size(), st.anchor.Size)
	}
	h256 := sha256.New()
	h1 := sha1.New()
	h1.Write([]byte(fmt.Sprintf("blob %d\x00", stat.Size()))) // git blob header
	buf := make([]byte, 1<<20)
	for {
		n, rerr := st.part.Read(buf)
		if n > 0 {
			h256.Write(buf[:n])
			h1.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	got := hex.EncodeToString(h256.Sum(nil))
	if st.anchor.SHA256 != "" && got != st.anchor.SHA256 {
		st.part.Close()
		os.Remove(st.part.Name())
		st.part = nil
		return fmt.Errorf("anchor mismatch for %q: downloaded bytes hash to sha256 %s, anchor pins %s — evil or wrong torrent; refusing", st.name, got, st.anchor.SHA256)
	}
	if st.anchor.GitSHA1 != "" {
		if g1 := hex.EncodeToString(h1.Sum(nil)); g1 != st.anchor.GitSHA1 {
			st.part.Close()
			os.Remove(st.part.Name())
			st.part = nil
			return fmt.Errorf("anchor mismatch for %q: downloaded bytes are git blob %s, anchor pins %s — evil or wrong torrent; refusing", st.name, g1, st.anchor.GitSHA1)
		}
	}
	if _, err := st.part.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := p.t.parent.store.Put(got, st.part); err != nil {
		return err
	}
	st.part.Close()
	os.Remove(st.part.Name())
	st.part = nil
	st.sealed = true
	st.digest = got
	p.t.doneCond.Broadcast()
	return nil
}

func (p *foreignPiece) MarkNotComplete() error {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	pl := int64(p.t.info.PieceLength)
	b, e := p.off, p.off+p.len
	for _, f := range p.t.filesInRange(b, e) {
		st := p.t.states[f.path]
		first, last := pieceBits(f, b, e, pl)
		for i := first; i <= last; i++ {
			bit := i - int(f.begin/pl)
			if bit < 0 || bit >= len(st.pieces) || !st.pieces[bit] {
				continue
			}
			st.pieces[bit] = false
			st.numDone--
		}
	}
	return nil
}

// Completion is per-piece truth across every file the piece spans: the
// bit map is only ever set by the engine itself (piece receipt or piece
// check), never by a CAS hit alone.
func (p *foreignPiece) Completion() storage.Completion {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	for _, st := range p.t.states {
		if st.failed != nil {
			return storage.Completion{Ok: true, Complete: false, Err: st.failed}
		}
	}
	pl := int64(p.t.info.PieceLength)
	b, e := p.off, p.off+p.len
	for _, f := range p.t.filesInRange(b, e) {
		st := p.t.states[f.path]
		first, last := pieceBits(f, b, e, pl)
		for i := first; i <= last; i++ {
			bit := i - int(f.begin/pl)
			if bit < 0 || bit >= len(st.pieces) || !st.pieces[bit] {
				return storage.Completion{Ok: true, Complete: false}
			}
		}
	}
	return storage.Completion{Ok: true, Complete: true}
}

// newPart creates the incoming part (CAS naming scheme so 003 §4 stale
// cleaning covers foreign parts too).
func (s *ForeignStorage) newPart(prefix string) (*os.File, error) {
	dir := filepath.Join(s.store.Root, "incoming")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, prefix+"-*.part")
}
