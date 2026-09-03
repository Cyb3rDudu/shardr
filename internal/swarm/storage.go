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
	"strings"
	"sync"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
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
	_, ok := v.m.Load(digest)
	return ok
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
		// Size is informational; the info dict is authoritative at
		// OpenTorrent (phase-1 imports register the manifest before its
		// size is known).
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
	t, err := newCASTorrent(c, key, info, files)
	if err != nil {
		c.mu.Unlock()
		return storage.TorrentImpl{}, err
	}
	c.open[key] = t
	c.mu.Unlock()
	return storage.TorrentImpl{
		Piece:         t.piece,
		PieceWithHash: func(p metainfo.Piece, _ g.Option[[]byte]) storage.PieceImpl { return t.piece(p) },
		Close:         t.close,
	}, nil
}

// driverFor returns the live driver for a registered torrent (nil when
// not open).
func (c *CASStorage) driverFor(v2InfohashHex string) *casTorrent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open[normalizeKey(v2InfohashHex)]
}

func normalizeKey(v2InfohashHex string) string {
	if len(v2InfohashHex) > 40 {
		return v2InfohashHex[:40]
	}
	return v2InfohashHex
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
	name string // torrent-relative path (never empty; used in errors)

	spec     FileSpec
	partPath string

	sealed  bool // promoted into the CAS
	failed  error
	part    *os.File
	numDone int // pieces completed (unsealed only)
	pieces  []bool
}

func newCASTorrent(parent *CASStorage, key string, info *metainfo.Info, files map[string]FileSpec) (*casTorrent, error) {
	t := &casTorrent{parent: parent, key: key, info: info}
	t.doneCond = sync.NewCond(&t.mu)
	fis := info.UpvertedFiles()
	sort.Slice(fis, func(i, j int) bool { return fis[i].TorrentOffset < fis[j].TorrentOffset })
	pl := info.PieceLength
	t.files = make(map[string]*fileState, len(fis))
	for _, fi := range fis {
		// Trust boundary: torrent metadata is untrusted input. Paths are
		// map keys only (never filesystem paths), but non-canonical segments
		// ("", ".", "..", absolute) are rejected loudly — a crafted info dict
		// must not smuggle traversal-shaped keys past the spec's canonical
		// name rules (001 §3.1 rule 1).
		if !canonicalTorrentPath(fi.Path) {
			return nil, fmt.Errorf("swarm: torrent file tree contains non-canonical path %q (empty/./.. segments rejected, 001 §3.1)", slashJoin(fi.Path))
		}
		path := slashJoin(fi.Path)
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
		st := &fileState{name: path, spec: spec, numDone: 0}
		if ok && parent.store != nil && parent.store.Has(spec.Digest) {
			st.sealed = true // CAS hit: seed-side, no part needed
			st.pieces = make([]bool, n)
			if n == 1 {
				// Single-piece files have no piece layer, so no pre-info
				// setV2Hash completion pull: their first pull is at onSetInfo,
				// after the piece request order exists. A CAS hit can be
				// complete immediately — it also keeps sub-chunk files (the
				// 1.4 KB manifest) out of the webseed planner, which panics
				// on padding chunks of pending short pieces (anacrolix master).
				st.pieces[0] = true
				st.numDone = 1
			} else {
				// Multi-piece files are verified piece-by-piece by engine
				// checks (see Completion).
			}
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
	return t, nil
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
		t.doneCond.Broadcast()
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

// VerifyPresent drives anacrolix piece checks over every sealed
// multi-piece file (single-piece CAS hits are complete on their first
// pull at onSetInfo; multi-piece ones report incomplete until the engine
// itself hashes the blob — see Completion). A piece that fails its check
// means the sealed blob no longer matches the info dict's merkle root:
// corruption, loud.
func (t *casTorrent) VerifyPresent(ctx context.Context, tr *torrent.Torrent) error {
	for _, s := range t.spans {
		t.mu.Lock()
		st := t.files[s.path]
		sealed, multi := st != nil && st.sealed, s.end-s.begin > 1
		t.mu.Unlock()
		if !sealed || !multi {
			continue
		}
		for i := s.begin; i < s.end; i++ {
			if err := tr.Piece(i).VerifyDataContext(ctx); err != nil {
				return fmt.Errorf("swarm: verify present piece %d of %q: %w", i, s.path, err)
			}
			if !tr.Piece(i).State().Complete {
				return fmt.Errorf("swarm: sealed blob for %q fails its piece check at piece %d (blob corrupt or layers mismatch)", s.path, i)
			}
		}
	}
	return nil
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
		if done >= n {
			return nil
		}
		if t.closed {
			return fmt.Errorf("swarm: driver closed before %d files sealed (got %d)", n, done)
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

func (p *casPiece) ReadAt(b []byte, off int64) (int, error) {
	// anacrolix passes PIECE-relative offsets (writeChunk sends msg.Begin,
	// the offset within the piece) — translate to the file offset.
	off += int64(p.local) * p.t.info.PieceLength
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
		return 0, fmt.Errorf("swarm: read of unstarted piece in file %q (not downloading)", p.st.name)
	}
	return part.ReadAt(b, off)
}

func (p *casPiece) WriteAt(b []byte, off int64) (int, error) {
	// anacrolix passes PIECE-relative offsets — translate to the file
	// offset (same contract as ReadAt above).
	off += int64(p.local) * p.t.info.PieceLength
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
		// Pre-sealed (CAS hit): an engine piece check hashed the blob and
		// it matched. Record the verification so Completion flips only
		// after the engine confirmed every piece (see Completion for why
		// the CAS hit alone must not count as complete).
		if !p.st.pieces[p.local] {
			p.st.pieces[p.local] = true
			p.st.numDone++
			if p.st.numDone == len(p.st.pieces) {
				p.t.parent.verified.MarkVerified(p.st.spec.Digest)
			}
			p.t.doneCond.Broadcast()
		}
		return nil
	}
	if p.st.failed != nil {
		return p.st.failed
	}
	if p.st.pieces == nil {
		// Defensive: pieces is nil only for unpinned files (digest ""),
		// which Completion already reports unknown — completion must be
		// tracked nowhere else.
		return fmt.Errorf("swarm: piece completion for file %q tracked nowhere (unpinned state)", p.st.name)
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
		// Sealed never un-seals, but a failed engine piece check means the
		// blob no longer matches the merkle root: retract the piece so it
		// is not announced. The file stays sealed locally; the fill-side
		// verify (003 §4) is the loud gate for operator visibility.
		if p.st.pieces != nil && p.st.pieces[p.local] {
			p.st.pieces[p.local] = false
			p.st.numDone--
		}
		return nil
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
	// Complete is per-piece truth: pieces[local] is set only by the engine
	// itself (piece receipt for downloads, piece check for CAS hits) —
	// never trust a CAS hit without the engine hashing it (003 §4), and
	// never report a verified piece incomplete again (a concurrent
	// completion re-pull would retract it and restart the download).
	// Reporting Ok=true also keeps queueInitialPieceCheck away: verification
	// of present files is driven explicitly via VerifyPresent.
	return storage.Completion{Ok: true, Complete: p.st.pieces != nil && p.st.pieces[p.local]}
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

// canonicalTorrentPath enforces canonical /-separated segments on a
// torrent file-tree path (001 §3.1 rule 1 analogue for untrusted metadata).
func canonicalTorrentPath(segs []string) bool {
	if len(segs) == 0 {
		return false
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\\") {
			return false
		}
	}
	return true
}

// slashJoin renders a torrent file path; identical semantics to the test
// helper (single source of truth lives here, tests reuse it).
func slashJoin(segs []string) string { return strings.Join(segs, "/") }
