// Package swarm implements shardhive's BitTorrent v2 swarm client per
// spec 004: a CAS-backed storage driver (the swarm writes directly into
// the verifying store — no second copy), deterministic torrent
// reconstruction from manifests, the infohash binding gate against the
// distribution record, selective swarm fill on ensure, seed-by-default,
// and the /import/bt flow with a pinned manifest digest (001 §8.7).
package swarm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// ---------------------------------------------------------------------------
// CAS storage driver (004 §5): anacrolix storage.ClientImpl backed by the
// content-addressed store. Every torrent file maps to exactly one CAS
// digest (manifest entries, plus the manifest itself at
// manifest/sha256-<hex>). BitTorrent v2 files are piece-aligned (BEP 52),
// so no piece ever spans two files and every file finalizes independently:
// pieces stream into incoming/*.part, and when the last piece of a file
// completes, the part runs through the CAS verify-write (003 §3) — flat
// sha256 against the pinned digest. Unverified bytes are never promoted;
// the CAS never serves incoming/*.part (003 §4).
// ---------------------------------------------------------------------------

// FileSpec pins one torrent file to a CAS digest (bare hex).
type FileSpec struct {
	Digest string // 64 lowercase hex, no sha256: prefix
	Size   int64
}

// VerifyCache tracks per-process blob verification (003 §4 seed-start
// re-hash): a digest lands here after a verifying write or a full re-hash
// in this process. Seeding a blob requires a cache hit or an explicit
// re-hash at registration — disk corruption must never silently become
// swarm corruption.
type VerifyCache struct{ m sync.Map }

func (v *VerifyCache) MarkVerified(digest string) { v.m.Store(digest, true) }
func (v *VerifyCache) WasVerified(digest string) bool {
	ok, _ := v.m.Load(digest)
	return ok.(bool)
}

// CASStorage implements storage.ClientImpl over the CAS. Torrents are
// registered by short infohash (first 20 bytes of the v2 hash — the key
// anacrolix uses internally) with their file→digest map BEFORE being
// added; an unregistered torrent is a loud error, never an unverified
// write (fail closed: the client never stores bytes it cannot pin).
type CASStorage struct {
	store    *cas.Store
	verified *VerifyCache

	mu       sync.Mutex
	registry map[string]map[string]FileSpec // short infohash hex → path → spec
	open     map[string]*casTorrent         // short infohash hex → live driver
}

// NewCASStorage builds the driver. store may be nil (tests that only
// exercise registration).
func NewCASStorage(store *cas.Store, verified *VerifyCache) *CASStorage {
	if verified == nil {
		verified = &VerifyCache{}
	}
	return &CASStorage{
		store:    store,
		verified: verified,
		registry: map[string]map[string]FileSpec{},
		open:     map[string]*casTorrent{},
	}
}

// Register maps every file of the torrent identified by the v2 infohash
// hex (64 chars) to its CAS digest. Must be called before the torrent is
// added to the client. Re-registering the same infohash replaces the map
// (two-phase /import/bt re-add after the manifest phase).
func (c *CASStorage) Register(v2InfohashHex string, files map[string]FileSpec) error {
	if len(v2InfohashHex) != 64 {
		return fmt.Errorf("swarm: register: infohash must be 64 hex chars (v2), got %q", v2InfohashHex)
	}
	for path, spec := range files {
		if len(spec.Digest) != 64 {
			return fmt.Errorf("swarm: register: file %q digest must be bare 64-hex", path)
		}
		if spec.Size <= 0 {
			return fmt.Errorf("swarm: register: file %q size must be > 0", path)
		}
	}
	c.mu.Lock()
	c.registry[v2InfohashHex[:40]] = files
	c.mu.Unlock()
	return nil
}

// OpenTorrent implements storage.ClientImpl.
func (c *CASStorage) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	if info.HasV1() {
		return storage.TorrentImpl{}, fmt.Errorf("swarm: torrent %x carries v1 fields; shardr torrents are pure BitTorrent v2 (004 §3)", infoHash)
	}
	key := fmt.Sprintf("%x", infoHash)
	c.mu.Lock()
	files, ok := c.registry[key]
	if !ok {
		c.mu.Unlock()
		return storage.TorrentImpl{}, fmt.Errorf("swarm: torrent %x is not registered with CAS digests; refusing to store unpinned bytes", infoHash)
	}
	t := newCASTorrent(c, key, info, files)
	c.open[key] = t
	c.mu.Unlock()
	return storage.TorrentImpl{
		Piece:         t.piece,
		PieceWithHash: func(p metainfo.Piece, _ g.Option[[]byte]) storage.PieceImpl { return t.piece(p) },
		Close:         t.close,
	}, nil
}

// casTorrent is the per-torrent driver instance.
type casTorrent struct {
	parent *CASStorage
	key    string
	info   *metainfo.Info

	spans []fileSpan // ordered by first piece index

	mu       sync.Mutex
	files    map[string]*fileState // by slash-joined path
	closed   bool
	doneCond *sync.Cond // any-file progress/seal/failure wakeup
}

// fileSpan maps a contiguous piece range to one torrent file.
type fileSpan struct {
	path       string // slash-joined torrent-relative path
	begin, end int    // global piece index range [begin, end)
}

// fileState is the download/seal state of one file.
type fileState struct {
	spec     FileSpec
	partPath string

	sealed   bool // promoted into the CAS
	failed   error
	part     *os.File
	numDone  int // pieces completed (unsealed only)
	pieces   []bool
	doneCond *sync.Cond // broadcasts seal/failure and per-piece completion
}

func newCASTorrent(parent *CASStorage, key string, info *metainfo.Info, files map[string]FileSpec) *casTorrent {
	t := &casTorrent{parent: parent, key: key, info: info}
	t.doneCond = sync.NewCond(&t.mu)
	fis := info.UpvertedFiles()
	sort.Slice(fis, func(i, j int) bool { return fis[i].TorrentOffset < fis[j].TorrentOffset })
	pl := info.PieceLength
	t.files = make(map[string]*fileState, len(fis))
	for _, fi := range fis {
		path := filepath.Join(fi.Path...) // POSIX join; torrent paths are /-separated
		if fi.Length == 0 {
			continue // occupies no pieces (shardr manifests have no empty files; see 001 §3.1)
		}
		if fi.TorrentOffset%pl != 0 {
			// Guarded at OpenTorrent: pure v2 offsets are piece-aligned by
			// construction (BEP 52); reaching this means a non-v2 tree.
			panic(fmt.Sprintf("swarm: file %q offset %d not piece-aligned (piece length %d)", path, fi.TorrentOffset, pl))
		}
		begin := int(fi.TorrentOffset / pl)
		n := (int(fi.Length) + int(pl) - 1) / int(pl)
		t.spans = append(t.spans, fileSpan{path: path, begin: begin, end: begin + n})

		spec, ok := files[path]
		st := &fileState{spec: spec, numDone: 0}
		if ok && parent.store != nil && parent.store.Has(spec.Digest) {
			st.sealed = true // CAS hit: seed-side, no part needed
		} else if ok {
			st.pieces = make([]bool, n)
		} else {
			// Not pinned yet (two-phase /import/bt manifest phase): pieces
			// exist in the torrent but must never be requested or written.
			st.spec = FileSpec{Digest: "", Size: fi.Length}
		}
		st.spec.Size = fi.Length
		t.files[path] = st
	}
	return t
}

// piece returns the PieceImpl for a global piece index.
func (t *casTorrent) piece(p metainfo.Piece) storage.PieceImpl {
	idx := p.Index()
	span := t.spanFor(idx)
	return &casPiece{t: t, st: t.files[span.path], local: idx - span.begin}
}

func (t *casTorrent) spanFor(idx int) fileSpan {
	for _, s := range t.spans {
		if idx >= s.begin && idx < s.end {
			return s
		}
	}
	panic(fmt.Sprintf("swarm: piece %d outside all file spans (torrent %s)", idx, t.key))
}

// close drops the driver: unfinished parts are deleted (no cross-restart
// resume in v1; the CAS ages out any leftovers), sealed files are already
// in the store.
func (t *casTorrent) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, st := range t.files {
		if st.part != nil {
			st.part.Close()
			if !st.sealed {
				os.Remove(st.part.Name())
			}
		}
		st.doneCond.Broadcast()
	}
	t.parent.mu.Lock()
	delete(t.parent.open, t.key)
	t.parent.mu.Unlock()
	return nil
}

// SealedCount reports how many files are promoted into the CAS (progress
// for fill jobs).
func (t *casTorrent) SealedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, st := range t.files {
		if st.sealed {
			n++
		}
	}
	return n
}

// WaitSealed blocks until n files are sealed, the driver closes, or any
// file fails its verify-write. Returns the failure if one occurred.
func (t *casTorrent) WaitSealed(ctx context.Context, n int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, failed := 0, error(nil)
		for _, st := range t.files {
			switch {
			case st.failed != nil:
				if failed == nil {
					failed = st.failed
				}
			case st.sealed:
				done++
			}
		}
		if failed != nil {
			return fmt.Errorf("swarm: verify-write failed: %w", failed)
		}
		if done >= n || t.closed {
			return nil
		}
		// Context cancellation must wake the cond — a watchdog broadcast.
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

// casPiece is the per-piece view: ReaderAt/WriterAt against the file's
// part (downloading) or sealed CAS blob (seeding).
type casPiece struct {
	t     *casTorrent
	st    *fileState
	local int
}

func (p *casPiece) bounds() (off int64, length int64) {
	off = int64(p.local) * p.t.info.PieceLength
	length = p.t.info.PieceLength
	if rem := p.st.spec.Size - off; rem < length {
		length = rem
	}
	return
}

func (p *casPiece) ReadAt(b []byte, off int64) (int, error) {
	p.t.mu.Lock()
	sealed, part, blob := p.st.sealed, p.st.part, p.st.spec.Digest
	p.t.mu.Unlock()
	if sealed {
		f, err := p.t.parent.store.Open(blob)
		if err != nil {
			return 0, fmt.Errorf("swarm: open sealed blob %s: %w", blob, err)
		}
		defer f.Close()
		return f.ReadAt(b, off)
	}
	if part == nil {
		return 0, fmt.Errorf("swarm: read of unstarted piece (file %s not downloading)", p.st.spec.Digest)
	}
	return part.ReadAt(b, off)
}

func (p *casPiece) WriteAt(b []byte, off int64) (int, error) {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	if p.st.sealed {
		return 0, fmt.Errorf("swarm: write into sealed blob %s refused (CAS is immutable)", p.st.spec.Digest)
	}
	if p.st.spec.Digest == "" {
		return 0, fmt.Errorf("swarm: write into unpinned file (no CAS digest registered); the client must not request this piece")
	}
	if p.st.failed != nil {
		return 0, p.st.failed
	}
	if p.st.part == nil {
		part, err := p.t.parent.newPart(p.st.spec.Digest)
		if err != nil {
			return 0, err
		}
		p.st.part = part
		p.st.partPath = part.Name()
	}
	return p.st.part.WriteAt(b, off)
}

// MarkComplete records the piece and, for the last one, finalizes the
// file: the part streams through the CAS verify-write (003 §3) — flat
// sha256 must equal the pinned digest or the part is deleted and the file
// fails loudly. Only called for pieces anacrolix already merkle-verified;
// the flat digest is the independent second gate.
func (p *casPiece) MarkComplete() error {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	if p.st.sealed {
		return nil // idempotent (resume/seal races)
	}
	if p.st.failed != nil {
		return p.st.failed
	}
	if !p.st.pieces[p.local] {
		p.st.pieces[p.local] = true
		p.st.numDone++
		p.t.doneCond.Broadcast()
	}
	if p.st.numDone < len(p.st.pieces) {
		return nil
	}
	// Last piece: seal. Done under the torrent lock — a concurrent
	// ReadAt of the same file would race the part→blob transition; the
	// seal is a bounded one-shot (stream + hash + rename), and fill jobs
	// wait on doneCond rather than hammering reads of incomplete files.
	if err := p.sealLocked(); err != nil {
		p.st.failed = err
		p.t.doneCond.Broadcast()
		return err
	}
	return nil
}

func (p *casPiece) sealLocked() error {
	part := p.st.part
	if part == nil {
		return fmt.Errorf("swarm: seal with no part for digest %s", p.st.spec.Digest)
	}
	if _, err := part.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := p.t.parent.store.Put(p.st.spec.Digest, part); err != nil {
		part.Close()
		os.Remove(part.Name())
		return err
	}
	part.Close()
	os.Remove(part.Name())
	p.st.part = nil
	p.st.sealed = true
	p.t.parent.verified.MarkVerified(p.st.spec.Digest)
	p.t.doneCond.Broadcast()
	return nil
}

func (p *casPiece) MarkNotComplete() error {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	if p.st.sealed {
		return nil // sealed files never un-seal
	}
	if p.st.pieces != nil && p.st.pieces[p.local] {
		p.st.pieces[p.local] = false
		p.st.numDone--
	}
	return nil
}

func (p *casPiece) Completion() storage.Completion {
	p.t.mu.Lock()
	defer p.t.mu.Unlock()
	if p.st.spec.Digest == "" {
		return storage.Completion{Ok: false} // unpinned file: unknown by design
	}
	if p.st.failed != nil {
		return storage.Completion{Ok: true, Complete: false, Err: p.st.failed}
	}
	return storage.Completion{Ok: true, Complete: p.st.sealed}
}

// newPart creates the incoming part for a downloading file (reuses the
// CAS naming scheme so 003 §4 stale-part cleaning covers swarm parts).
func (c *CASStorage) newPart(digest string) (*os.File, error) {
	dir := filepath.Join(c.store.Root, "incoming")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, digest[:16]+"-*.part")
}
