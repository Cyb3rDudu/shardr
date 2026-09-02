package swarm

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// ---------------------------------------------------------------------------
// Swarm client (004 §5): one anacrolix torrent.Client per shardhive
// process, CAS-backed storage, seed-by-default, and the webseed HTTP
// surface (piece-layers well-known path 004 §5(2) + BEP 19-style file
// serving so other shardhives can pull from this node over plain HTTP —
// every byte digest-verified regardless of transport).
// ---------------------------------------------------------------------------

// Config mirrors [swarm] in ~/.config/shardr/config.toml (004 §7). All
// knobs are local-node only; nothing protocol-affecting exists.
type Config struct {
	Enabled      bool  // swarm client at all (fetch + seed)
	Seed         bool  // seed complete artifacts (the community mirror)
	UploadLimit  int64 // bytes/sec, 0 = unlimited
	DHT          bool  // DHT + PEX
	NoSeedVerify bool  // escape hatch (003 §4), documented unsafe, never default
	// WebseedAddr is the webseed HTTP bind address (":0" = ephemeral).
	// Empty disables the webseed listener (peer protocol still works).
	WebseedAddr string
	// DataRoot is the CAS root; empty = cas.ResolveRoot().
	DataRoot string
	// DisableIPv6 and ListenHost keep tests deterministic.
	DisableIPv6 bool
	ListenHost  string
}

// DefaultConfig returns the documented defaults (004 §7): enabled,
// seeding, unlimited upload, DHT on.
func DefaultConfig() Config {
	return Config{Enabled: true, Seed: true, UploadLimit: 0, DHT: true, WebseedAddr: "127.0.0.1:0"}
}

// Client owns the torrent engine, the CAS storage driver, and the
// webseed listener for one shardhive process.
type Client struct {
	cfg    Config
	store  *cas.Store
	tc     *torrent.Client
	casSt  *CASStorage
	verify *VerifyCache

	wsLn     net.Listener
	wsURL    string // http://host:port — the source-hint webseed base URL
	wsSrv    *http.Server
	wsMu     sync.Mutex
	wsFiles  map[string]FileSpec // torrentName/path → spec (BEP 19 layout)
	wsLayers map[string]string   // manifest hex OR layers digest → layers digest hex

	seedsMu sync.Mutex
	seeds   map[string]*seedEntry // infohash hex → registered seed
}

type seedEntry struct {
	recon *Recon
	t     *torrent.Torrent
}

// New opens the CAS, starts the torrent engine, and binds the webseed
// listener. A nil cfg means defaults.
func New(cfg Config) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("swarm: New with Enabled=false (caller bug: do not construct a disabled client)")
	}
	root := cfg.DataRoot
	if root == "" {
		r, err := cas.ResolveRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	store, err := cas.Open(root)
	if err != nil {
		return nil, fmt.Errorf("swarm: open CAS: %w", err)
	}
	verify := &VerifyCache{}
	casSt := NewCASStorage(store, verify)

	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DefaultStorage = casSt
	tcfg.NoDHT = !cfg.DHT
	tcfg.Seed = cfg.Seed
	tcfg.DisableIPv6 = cfg.DisableIPv6
	if cfg.ListenHost != "" {
		tcfg.ListenHost = func(string) string { return cfg.ListenHost }
	}
	tcfg.ListenPort = 0 // ephemeral; NAT traversal is DHT/PEX's business
	if cfg.UploadLimit > 0 {
		// Burst covers one peer request window (config default if unset).
		tcfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.UploadLimit), 1<<16)
	}
	tc, err := torrent.NewClient(tcfg)
	if err != nil {
		return nil, fmt.Errorf("swarm: torrent client: %w", err)
	}

	c := &Client{
		cfg:      cfg,
		store:    store,
		tc:       tc,
		casSt:    casSt,
		verify:   verify,
		seeds:    map[string]*seedEntry{},
		wsFiles:  map[string]FileSpec{},
		wsLayers: map[string]string{},
	}
	if err := c.startWebseed(); err != nil {
		tc.Close()
		return nil, err
	}
	return c, nil
}

// Store exposes the CAS handle (shared with the API server).
func (c *Client) Store() *cas.Store { return c.store }

// WebseedURL returns the base URL other nodes can use as a webseed
// source hint ("" when the listener is off).
func (c *Client) WebseedURL() string { return c.wsURL }

// Close drops the engine and the webseed listener.
func (c *Client) Close() {
	if c.wsSrv != nil {
		c.wsSrv.Close()
	}
	c.tc.Close()
}

// ---------------------------------------------------------------------------
// Seeding (004 §5): seed every complete artifact. Registration runs the
// 003 §4 seed-start re-hash (full flat re-hash per blob, cached per
// process) unless the escape hatch is set, verifies the 001 §6 binding
// when a distribution record is linked, then announces.
// ---------------------------------------------------------------------------

// SeedArtifact registers a complete artifact for seeding. manifestBytes
// are the canonical JCS bytes. record, when non-nil, is checked against
// the derived identity first (Goal 2: the gate runs before announce).
func (c *Client) SeedArtifact(ctx context.Context, m *artifact.Manifest, manifestBytes []byte, rec *artifact.DistributionRecord, recordBytes []byte) error {
	if !c.cfg.Seed {
		return nil
	}
	recon, err := Reconstruct(m, manifestBytes)
	if err != nil {
		return err
	}
	if rec != nil {
		if err := recon.CheckRecord(rec); err != nil {
			return err
		}
	}
	// Seed-start re-hash (003 §4): every blob fully re-hashed once per
	// process unless already verified in-process (e.g. just fetched).
	for path, spec := range recon.FileSpecs {
		if path == "manifest/sha256-"+recon.ManifestHex {
			continue // covered by the record binding + canonical bytes
		}
		if !c.store.Has(spec.Digest) {
			return fmt.Errorf("swarm: seed: artifact incomplete — blob %s (%s) missing", spec.Digest, path)
		}
		if c.cfg.NoSeedVerify || c.verify.WasVerified(spec.Digest) {
			continue
		}
		if err := c.store.Verify(spec.Digest); err != nil {
			return fmt.Errorf("swarm: seed-start re-hash failed for %s (%s): %w — disk corruption must never become swarm corruption (003 §4)", spec.Digest, path, err)
		}
		c.verify.MarkVerified(spec.Digest)
	}

	c.casSt.Register(recon.InfohashHex, recon.FileSpecs)

	layers, err := c.layersFor(recon, rec)
	if err != nil {
		return err
	}
	spec, err := specFromRecon(recon, layers, nil, nil, nil)
	if err != nil {
		return err
	}
	t, _, err := c.tc.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("swarm: add seed torrent: %w", err)
	}
	// Metadata phase is local: we built the info ourselves.
	<-t.GotInfo()

	c.seedsMu.Lock()
	c.seeds[recon.InfohashHex] = &seedEntry{recon: recon, t: t}
	c.seedsMu.Unlock()

	// Webseed registration: BEP 19 layout <name>/<path> + layers paths.
	c.wsMu.Lock()
	for path, fs := range recon.FileSpecs {
		c.wsFiles[recon.Name+"/"+path] = fs
	}
	layersKey := ""
	if rec != nil {
		layersKey = artifact.DigestHex(rec.Torrent.PieceLayersDigest)
	}
	if layersKey != "" {
		c.wsLayers[recon.ManifestHex] = layersKey
		c.wsLayers[layersKey] = layersKey
	}
	c.wsMu.Unlock()
	if recordBytes != nil && rec != nil {
		// The record itself is servable too (small, useful for peers).
		recDigest := artifact.DigestHex(artifact.Digest(recordBytes))
		c.wsMu.Lock()
		c.wsFiles["records/sha256-"+recDigest] = FileSpec{Digest: recDigest, Size: int64(len(recordBytes))}
		c.wsMu.Unlock()
	}
	return nil
}

// layersFor loads the piece-layers blob for a reconstruction: from the
// CAS via the record's pieceLayersDigest, else from the manifest-derived
// lookup (state links manifest→record; the caller passes the linked
// record when it has one — without any layers the fetch path cannot
// verify big files, so missing layers are loud here).
func (c *Client) layersFor(recon *Recon, rec *artifact.DistributionRecord) (map[string]string, error) {
	digest := ""
	if rec != nil {
		digest = artifact.DigestHex(rec.Torrent.PieceLayersDigest)
	} else {
		c.wsMu.Lock()
		digest = c.wsLayers[recon.ManifestHex]
		c.wsMu.Unlock()
	}
	if digest == "" {
		// No file exceeds the piece length? The layers map is legitimately
		// empty — an empty bencoded dict decodes fine.
		return map[string]string{}, nil
	}
	if !c.store.Has(digest) {
		return nil, fmt.Errorf("swarm: piece-layers blob %s not in CAS (record-linked artifacts carry it; refusing half-verifiable swarm)", digest)
	}
	f, err := c.store.Open(digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blob, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return LayersFromBlob(blob)
}

// ---------------------------------------------------------------------------
// Webseed HTTP surface: /bt/piece-layers/<digest> (004 §5(2); digest =
// piece-layers digest, or manifest hex via the state link — PR comment
// filed on the ambiguity) and BEP 19-style <torrent-name>/<file-path>
// serving straight from CAS blobs.
// ---------------------------------------------------------------------------

func (c *Client) startWebseed() error {
	if c.cfg.WebseedAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", c.cfg.WebseedAddr)
	if err != nil {
		return fmt.Errorf("swarm: webseed listen: %w", err)
	}
	c.wsLn = ln
	c.wsURL = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/bt/piece-layers/", c.handlePieceLayers)
	mux.HandleFunc("/", c.handleWebseedFile)
	c.wsSrv = &http.Server{Handler: mux}
	go c.wsSrv.Serve(ln)
	return nil
}

func (c *Client) handlePieceLayers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bt/piece-layers/")
	c.wsMu.Lock()
	digest := c.wsLayers[key]
	c.wsMu.Unlock()
	if digest == "" {
		// Also accept the layers digest directly (self-describing CAS path).
		if len(key) == 64 {
			digest = key
		} else {
			http.Error(w, "unknown piece-layers key", http.StatusNotFound)
			return
		}
	}
	c.serveBlob(w, r, digest)
}

func (c *Client) handleWebseedFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	c.wsMu.Lock()
	spec, ok := c.wsFiles[strings.TrimPrefix(r.URL.Path, "/")]
	c.wsMu.Unlock()
	if !ok {
		http.Error(w, "not seeded here", http.StatusNotFound)
		return
	}
	c.serveBlob(w, r, spec.Digest)
}

func (c *Client) serveBlob(w http.ResponseWriter, r *http.Request, digest string) {
	if !c.cfg.Seed {
		http.Error(w, "seeding disabled", http.StatusServiceUnavailable)
		return
	}
	f, err := c.store.Open(digest)
	if err != nil {
		http.Error(w, "blob missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f) // range-capable (BEP 19 requirement)
}
