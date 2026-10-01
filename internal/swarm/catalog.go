package swarm

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/Cyb3rDudu/shardr/internal/cas"
	"github.com/Cyb3rDudu/shardr/internal/catalog"
	"github.com/Cyb3rDudu/shardr/internal/importer"
)

// ---------------------------------------------------------------------------
// Catalog pull (Epic #65): fetch a community-listed model torrent into
// the CAS under its anchor, build the native artifacts with the regular
// importer, then KEEP seeding the foreign swarm from CAS bytes
// (good-citizen mode). The foreign engine is a second torrent client:
// catalog swarms are somebody else's swarms — their piece layout, their
// trackers — with their OWN upload budget ([catalog] upload_limit,
// inheriting [swarm] upload_limit when unset), so community seeding
// never eats the node's shardr-swarm budget.
// ---------------------------------------------------------------------------

// CatalogPull is one resolved pull request.
type CatalogPull struct {
	Resolved *catalog.Resolved
	Anchor   *catalog.Anchor
	Quant    string   // "" = every group
	As       string   // override namespace ("" = lowercased repo id)
	Peers    []string // optional direct-peer hints (x.pe) — untrusted operational data
}

// CatalogResult reports what one pull produced.
type CatalogResult struct {
	Import        *importer.ImportResult
	Infohash      string // v1 hex
	SeededForeign bool   // good-citizen handle alive
	Warning       string // trust-shift notice (rescued pulls)
}

// foreignEngine lazily starts the v1 engine (one per daemon; separate
// DHT presence and upload budget from the v2 client).
func (c *Client) foreignEngine() (*torrent.Client, error) {
	c.foreignOnce.Do(func() {
		c.foreignStor = NewForeignStorage(c.store)
		tcfg := torrent.NewDefaultClientConfig()
		tcfg.DefaultStorage = c.foreignStor
		tcfg.NoDHT = !c.cfg.DHT
		tcfg.Seed = true // good-citizen mode is the point of the strand
		tcfg.DisableIPv6 = c.cfg.DisableIPv6
		if c.cfg.ListenHost != "" {
			tcfg.ListenHost = func(string) string { return c.cfg.ListenHost }
		}
		tcfg.ListenPort = 0
		if c.cfg.CatalogUploadLimit > 0 {
			c.foreignLim = uploadLimiter(c.cfg.CatalogUploadLimit)
			tcfg.UploadRateLimiter = c.foreignLim
		}
		tc, err := torrent.NewClient(tcfg)
		if err != nil {
			c.foreignErr = fmt.Errorf("swarm: catalog: foreign engine: %w", err)
			return
		}
		c.foreignTC = tc
	})
	if c.foreignErr != nil {
		return nil, c.foreignErr
	}
	if c.foreignTC == nil {
		return nil, fmt.Errorf("swarm: catalog: foreign engine not started")
	}
	return c.foreignTC, nil
}

// foreignSeeds tracks live good-citizen handles (infohash hex → entry).
type foreignSeedEntry struct {
	t     *torrent.Torrent
	files map[string]string // path → sealed digest
}

// ImportCatalog runs the anchored pull. Blocking; cancel via ctx.
func (c *Client) ImportCatalog(ctx context.Context, pull *CatalogPull, progress func(done, total int)) (*CatalogResult, error) {
	r := pull.Resolved
	if r == nil || pull.Anchor == nil {
		return nil, fmt.Errorf("swarm: catalog: pull needs a resolved listing and an anchor (caller bug)")
	}
	engine, err := c.foreignEngine()
	if err != nil {
		return nil, err
	}

	// Registration BEFORE the join: the anchor is the registration —
	// the engine may not store a byte it cannot pin (fail closed).
	anchorFiles := map[string]ForeignFile{}
	for _, f := range pull.Anchor.Files {
		anchorFiles[f.Path] = ForeignFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256, GitSHA1: f.GitSHA1}
	}
	if err := c.foreignStor.Register(r.Infohash, anchorFiles, !pull.Anchor.Rescued); err != nil {
		return nil, err
	}

	spec, err := torrent.TorrentSpecFromMagnetUri(r.Magnet)
	if err != nil {
		return nil, fmt.Errorf("swarm: catalog: magnet: %w", err)
	}
	spec.Webseeds = normalizeWebseeds(r.Webseeds)
	spec.PeerAddrs = pull.Peers
	spec.Storage = c.foreignStor
	t, _, err := engine.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("swarm: catalog: add foreign torrent: %w", err)
	}

	// Metadata phase: the info dict arrives from peers (or a webseed
	// peer); no infohash-only trust exists here — the anchor checks at
	// open (file tree vs anchor) plus the per-file digest gate on seal
	// are the trust, this join is just transport.
	infoCtx, cancelInfo := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelInfo()
	select {
	case <-t.GotInfo():
	case <-infoCtx.Done():
		dropTorrent(t)
		return nil, fmt.Errorf("swarm: catalog: no torrent metadata within 2 minutes — the swarm is unreachable (seeders online? tracker up?); a magnet alone never carries the file tree")
	}
	info := t.Info()
	if info.HasV2() {
		dropTorrent(t)
		return nil, fmt.Errorf("swarm: catalog: %s listed a v2 torrent; the catalog provider ships v1 (strand assumption broken — file this)", r.Repo)
	}

	// Wanted set: --quant filters GGUF weights to one quant family;
	// everything else (companions, configs, tokenizer) always comes.
	// v1 pieces can span a wanted and an unwanted file — those unwanted
	// files complete as a side effect and seal harmlessly (CAS dedupe).
	wanted, err := wantedFiles(t, pull.Quant)
	if err != nil {
		dropTorrent(t)
		return nil, err
	}
	for _, f := range t.Files() {
		path := strings.TrimPrefix(f.Path(), info.Name+"/")
		if _, ok := wanted[path]; ok {
			f.SetPriority(torrent.PiecePriorityNormal)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}

	drv := c.foreignStor.driverFor(r.Infohash)
	if drv == nil {
		dropTorrent(t)
		return nil, fmt.Errorf("swarm: catalog: foreign driver not open for %s (anchor open failed?)", r.Infohash)
	}
	target := len(wanted)
	if progress != nil {
		go pollProgress(ctx, drv, target, progress)
	}
	if err := drv.WaitSealed(ctx, target); err != nil {
		dropTorrent(t)
		return nil, err
	}

	// Sealed bytes must pass the engine's own piece checks before this
	// node announces them (seed-start proof, 003 §4).
	if err := drv.VerifySealed(ctx, t); err != nil {
		dropTorrent(t)
		return nil, err
	}

	// Native artifacts from the sealed (anchor-verified) CAS blobs via
	// the regular importer — same classification, same convergence.
	as := pull.As
	if as == "" {
		as = strings.ToLower(r.Repo)
	}
	sealed := drv.SealedFiles()
	paths := make([]string, 0, len(sealed))
	for p := range sealed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	sources := make([]importer.Source, 0, len(paths))
	for _, p := range paths {
		digest := sealed[p]
		sources = append(sources, importer.Source{Name: p, Open: func() (io.ReadCloser, error) {
			return openBlob(c.store, digest)
		}})
	}
	imp, err := importer.Import(ctx, c.store, sources, importer.ImportOptions{
		As: as, HFRepo: r.Repo, HFRevision: r.Revision,
	})
	if err != nil {
		return nil, err
	}

	// Good-citizen mode (Epic #65 ruling 2): the foreign handle STAYS
	// alive and seeds from CAS bytes under its own piece layout. No
	// dropTorrent — the point is giving back to the swarm we took from.
	c.foreignMu.Lock()
	c.foreignSeeds[r.Infohash] = &foreignSeedEntry{t: t, files: sealed}
	c.foreignMu.Unlock()

	return &CatalogResult{
		Import:        imp,
		Infohash:      r.Infohash,
		SeededForeign: true,
		Warning:       pull.Anchor.Warning(),
	}, nil
}

// openBlob opens a CAS blob as a ReadCloser (import sources).
func openBlob(store *cas.Store, digest string) (io.ReadCloser, error) {
	f, err := store.Open(digest)
	if err != nil {
		return nil, fmt.Errorf("catalog: open sealed blob %s: %w", digest, err)
	}
	return f, nil
}

func pollProgress(ctx context.Context, drv *foreignTorrent, target int, progress func(done, total int)) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			progress(drv.SealedCount(), target)
		}
	}
}

// wantedFiles computes the download set: with a quant selector, only the
// GGUF weights files whose filename token matches plus every non-weights
// file; without, everything.
func wantedFiles(t *torrent.Torrent, quant string) (map[string]bool, error) {
	info := t.Info()
	wanted := map[string]bool{}
	weightsTotal, weightsMatched := 0, 0
	for _, f := range info.UpvertedFiles() {
		path := slashJoin(f.Path)
		base := path
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		lower := strings.ToLower(base)
		isWeights := strings.HasSuffix(lower, ".gguf") || strings.HasSuffix(lower, ".safetensors")
		if !isWeights || quant == "" {
			wanted[path] = true
			continue
		}
		weightsTotal++
		if importer.QuantFromFilename(base) == strings.ToLower(quant) {
			wanted[path] = true
			weightsMatched++
		}
	}
	if quant != "" && weightsMatched == 0 {
		if weightsTotal == 0 {
			return nil, fmt.Errorf("swarm: catalog: --quant %q: repo has no GGUF weights to filter (safetensors groups derive their quant from the bytes; pull without --quant)", quant)
		}
		return nil, fmt.Errorf("swarm: catalog: --quant %q matches no weights file (%d GGUF files listed)", quant, weightsTotal)
	}
	return wanted, nil
}
