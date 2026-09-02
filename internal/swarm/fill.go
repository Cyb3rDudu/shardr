package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// ---------------------------------------------------------------------------
// Fill engine (004 §5 fetch): selective swarm fill on ensure-miss and the
// two-phase /import/bt flow (001 §8.7). Priorities: config and manifest
// first (the metadata phase), then weights. Every piece is merkle-verified
// by the engine on arrival; every file runs the CAS verify-write before it
// exists. resolve stays pure — fills run only from ensure/import.
// ---------------------------------------------------------------------------

// ErrLayersUnavailable: piece-layers acquisition failed from every hint.
var ErrLayersUnavailable = errors.New("swarm: piece-layers unavailable")

// Hints are untrusted operational source data (004 §4): trackers,
// webseeds, direct peer addresses. Bytes verify the same regardless of
// source; hints only affect where to look.
type Hints struct {
	Trackers []string
	Webseeds []string
	Peers    []string
}

// MissingFiles lists manifest files whose blobs are absent from the CAS.
func MissingFiles(store *cas.Store, m *artifact.Manifest) []string {
	var missing []string
	for _, f := range m.Files {
		if !store.Has(artifact.DigestHex(f.Digest)) {
			missing = append(missing, f.Name)
		}
	}
	return missing
}

// RecordForManifest loads the state-linked distribution record for a
// manifest (nil when unlinked — the caller decides whether that is fatal
// for its flow).
func (c *Client) RecordForManifest(manifestDigest string) (*artifact.DistributionRecord, []byte, error) {
	recDigest, err := c.store.RecordDigestForManifest(manifestDigest)
	if err != nil {
		return nil, nil, err
	}
	if recDigest == "" {
		return nil, nil, nil
	}
	f, err := c.store.Open(artifact.DigestHex(recDigest))
	if err != nil {
		return nil, nil, fmt.Errorf("swarm: linked record blob %s missing from CAS: %w", recDigest, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	var rec artifact.DistributionRecord
	if err := unmarshalJSON(b, &rec); err != nil {
		return nil, nil, fmt.Errorf("swarm: parse record %s: %w", recDigest, err)
	}
	if got := artifact.Digest(b); got != recDigest {
		return nil, nil, fmt.Errorf("swarm: record blob %s content hashes to %s (corruption; never silently rebuilt)", recDigest, got)
	}
	return &rec, b, nil
}

// Fill fetches the missing blobs of a locally-resolved artifact over the
// swarm. The record must be state-linked (it carries the piece-layers
// digest and the binding infohash — 001 §6). Blocking; cancel via ctx.
func (c *Client) Fill(ctx context.Context, m *artifact.Manifest, manifestBytes []byte, hints Hints, progress func(done, total int)) error {
	recon, err := Reconstruct(m, manifestBytes)
	if err != nil {
		return err
	}
	rec, _, err := c.RecordForManifest(recon.ManifestDigest)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("swarm: fill: no distribution record linked for manifest %s — the record pins the infohash and piece-layers identity (001 §6); link it at import time", recon.ManifestDigest)
	}
	// Goal 2 gate: binding check BEFORE announce.
	if err := recon.CheckRecord(rec); err != nil {
		return err
	}
	layers, err := c.layersFor(recon, rec)
	if err != nil {
		return err
	}

	missing := MissingFiles(c.store, m)
	if len(missing) == 0 {
		return nil
	}

	c.casSt.Register(recon.InfohashHex, recon.FileSpecs)
	spec, err := specFromRecon(recon, layers, hints.Trackers, hints.Webseeds, hints.Peers)
	if err != nil {
		return err
	}
	t, err := c.addFreshTorrent(spec, recon.InfohashHex)
	if err != nil {
		return err
	}
	<-t.GotInfo()
	setFillPriorities(t, m, recon)

	drv := c.casSt.driverFor(recon.InfohashHex)
	if drv == nil {
		return fmt.Errorf("swarm: fill: driver not open for %s", recon.InfohashHex)
	}
	// Target is TOTAL sealed files: present files count from the start, so
	// a partially-present artifact cannot satisfy the wait early.
	target := len(recon.FileSpecs)
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if progress != nil {
		go func() {
			tick := time.NewTicker(250 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-pollCtx.Done():
					return
				case <-tick.C:
					progress(drv.SealedCount(), target)
				}
			}
		}()
	}
	if err := drv.WaitSealed(ctx, target); err != nil {
		t.Drop()
		return err
	}
	t.Drop()

	// Usage = replication (004 §5): a filled artifact seeds.
	if c.cfg.Seed {
		return c.SeedArtifact(ctx, m, manifestBytes, rec, nil)
	}
	return nil
}

// setFillPriorities: config and manifest first (metadata phase), then
// everything missing. Present files are already complete in the driver —
// no bytes are re-fetched. anacrolix File.Path() includes the torrent
// name prefix; suffix-match the torrent-relative keys.
func setFillPriorities(t *torrent.Torrent, m *artifact.Manifest, recon *Recon) {
	manifestKey := "manifest/sha256-" + recon.ManifestHex
	for _, f := range t.Files() {
		path := f.Path()
		switch {
		case strings.HasSuffix(path, manifestKey) || strings.HasSuffix(path, "modelconfig.json"):
			f.SetPriority(torrent.PiecePriorityHigh)
		default:
			f.SetPriority(torrent.PiecePriorityNormal)
		}
	}
	_ = m
}

func countPresent(store *cas.Store, r *Recon) int {
	n := 0
	for _, spec := range r.FileSpecs {
		if store.Has(spec.Digest) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// /import/bt (001 §8.7, 005 §5): magnet or infohash PLUS a pinned
// manifest digest. The pin is the trust anchor — a magnet alone is never
// enough. Two phases: (1) join, acquire piece layers + the manifest file,
// flat-hash it against the pin; (2) derive identity from the manifest,
// check the infohash binding, register all file digests, fetch the rest.
// ---------------------------------------------------------------------------

// ImportResult is the terminal payload of a BT import.
type ImportResult struct {
	ManifestDigest string // sha256:<hex> (== the pin)
	Infohash       string // btmh:1220<hex>
	RecordDigest   string // sha256:<hex> of the derived distribution record
	Files          int
}

// ImportBT runs the pinned BT import. Blocking.
func (c *Client) ImportBT(ctx context.Context, magnetOrInfohash, manifestDigest string, hints Hints) (*ImportResult, error) {
	pinHex, err := pinHex(manifestDigest)
	if err != nil {
		return nil, err
	}
	ihHex, err := ParseInfohash(magnetOrInfohash)
	if err != nil {
		return nil, err
	}
	manifestKey := "manifest/sha256-" + pinHex

	// ---- Phase 1: join, get metadata + layers + manifest file ----
	layers, lerr := c.fetchLayersFromHints(ctx, pinHex, hints)
	if lerr != nil {
		return nil, lerr
	}
	c.casSt.Register(ihHex, map[string]FileSpec{
		manifestKey: {Digest: pinHex, Size: -1}, // size fixed at OpenTorrent from the info dict
	})
	spec := &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{ /* infohash only */ },
		DisplayName:    "shardr-bt-import",
		Trackers:       tiersOf(hints.Trackers),
		Webseeds:       normalizeWebseeds(hints.Webseeds),
		PeerAddrs:      hints.Peers,
	}
	var ih infohashOf
	if err := ih.fromHex(ihHex); err != nil {
		return nil, err
	}
	spec.InfoHashV2 = ih.opt()
	t1, _, err := c.tc.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("swarm: import: join torrent: %w", err)
	}
	select {
	case <-t1.GotInfo():
	case <-ctx.Done():
		dropTorrent(t1)
		return nil, ctx.Err()
	}
	if errs := t1.AddPieceLayers(layers); len(errs) > 0 {
		dropTorrent(t1)
		return nil, fmt.Errorf("swarm: import: piece layers rejected against the info dict's merkle roots: %v (evil or mismatched layers source)", errs)
	}
	// Manifest file only: the pin gates it. File.Path() carries the
	// torrent-name prefix — suffix-match.
	for _, f := range t1.Files() {
		if strings.HasSuffix(f.Path(), manifestKey) {
			f.SetPriority(torrent.PiecePriorityNow)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	drv1 := c.casSt.driverFor(ihHex)
	if drv1 == nil {
		dropTorrent(t1)
		return nil, fmt.Errorf("swarm: import: phase-1 driver missing")
	}
	if err := drv1.WaitSealed(ctx, 1); err != nil {
		dropTorrent(t1)
		return nil, fmt.Errorf("swarm: import: manifest acquisition failed: %w (the pin was never satisfied)", err)
	}
	dropTorrent(t1) // phase 1 done: manifest sealed under the pin digest

	// ---- Phase 2: identity from the manifest, binding, full fetch ----
	manifestBytes, err := readBlob(c.store, pinHex)
	if err != nil {
		return nil, err
	}
	var m artifact.Manifest
	if err := unmarshalJSON(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("swarm: import: parse pinned manifest: %w", err)
	}
	if err := artifact.ValidateManifest(&m); err != nil {
		return nil, fmt.Errorf("swarm: import: pinned manifest fails validation: %w", err)
	}
	recon, err := Reconstruct(&m, manifestBytes)
	if err != nil {
		return nil, err
	}
	// 004 §3 binding gate: the torrent we joined must be the torrent the
	// manifest describes. Mismatch = loud abort (never a warning).
	if recon.InfohashHex != ihHex {
		return nil, fmt.Errorf("%w: joined %s but manifest %s reconstructs to %s (torrent world and digest world disagree; aborting)", ErrBinding, ihHex, recon.ManifestDigest, recon.InfohashHex)
	}

	// Store the layers blob content-addressed + link the derived record.
	layersDigest, err := putLayers(c.store, layers, recon)
	if err != nil {
		return nil, err
	}
	rec, recBytes, err := derivedRecord(recon, layersDigest)
	if err != nil {
		return nil, err
	}
	if err := c.store.Put(artifact.DigestHex(artifact.Digest(recBytes)), strings.NewReader(string(recBytes))); err != nil {
		return nil, fmt.Errorf("swarm: import: store record: %w", err)
	}
	if err := c.store.SetDistributionLink(recon.ManifestDigest, artifact.Digest(recBytes)); err != nil {
		return nil, err
	}

	c.casSt.Register(ihHex, recon.FileSpecs) // replace: all files pinned now
	spec2, err := specFromRecon(recon, layers, hints.Trackers, hints.Webseeds, hints.Peers)
	if err != nil {
		return nil, err
	}
	t2, _, err := c.tc.AddTorrentSpec(spec2)
	if err != nil {
		return nil, fmt.Errorf("swarm: import: re-add full torrent: %w", err)
	}
	<-t2.GotInfo()
	setFillPriorities(t2, &m, recon)
	drv2 := c.casSt.driverFor(ihHex)
	if drv2 == nil {
		dropTorrent(t2)
		return nil, fmt.Errorf("swarm: import: phase-2 driver missing")
	}
	if err := drv2.WaitSealed(ctx, len(recon.FileSpecs)); err != nil {
		dropTorrent(t2)
		return nil, err
	}
	dropTorrent(t2)

	if c.cfg.Seed {
		if err := c.SeedArtifact(ctx, &m, manifestBytes, rec, recBytes); err != nil {
			return nil, err
		}
	}
	return &ImportResult{
		ManifestDigest: recon.ManifestDigest,
		Infohash:       recon.InfohashBtmh,
		RecordDigest:   artifact.Digest(recBytes),
		Files:          len(m.Files),
	}, nil
}

// fetchLayersFromHints acquires the piece-layers blob via the webseed
// well-known path (004 §5(2)): GET <webseed>/bt/piece-layers/<manifest
// digest hex> from any hint until one serves a decodable blob. The blob
// is merkle-root-verified by the engine at add time and digest-pinned at
// import completion — an evil source wastes bytes, never corrupts.
func (c *Client) fetchLayersFromHints(ctx context.Context, manifestHex string, hints Hints) (map[string]string, error) {
	if len(hints.Webseeds) == 0 {
		// Without layers only files ≤ piece length can verify; shardr
		// weights are bigger. Loud, with the acquisition paths named.
		return nil, fmt.Errorf("%w: no webseed source hints (004 §5 paths: resolver envelope sidecar, webseed /bt/piece-layers/<digest>, .torrent metainfo; pass a webseed hint)", ErrLayersUnavailable)
	}
	var lastErr error
	for _, base := range hints.Webseeds {
		url := strings.TrimRight(base, "/") + "/bt/piece-layers/" + manifestHex
		blob, err := httpGet(ctx, url, 8<<20)
		if err != nil {
			lastErr = err
			continue
		}
		layers, err := LayersFromBlob(blob)
		if err != nil {
			lastErr = err
			continue
		}
		return layers, nil
	}
	return nil, fmt.Errorf("%w: all webseed hints failed: %w", ErrLayersUnavailable, lastErr)
}

func httpGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// putLayers pins the acquired layers blob into the CAS under its flat
// digest when the record names it (identity check: derived == acquired).
func putLayers(store *cas.Store, layers map[string]string, recon *Recon) (string, error) {
	blob, err := bencodeLayers(layers)
	if err != nil {
		return "", err
	}
	digest := artifact.Digest(blob)
	if err := store.Put(artifact.DigestHex(digest), strings.NewReader(string(blob))); err != nil {
		return "", fmt.Errorf("swarm: import: store layers: %w", err)
	}
	return digest, nil
}

func derivedRecord(recon *Recon, layersDigest string) (*artifact.DistributionRecord, []byte, error) {
	rec := &artifact.DistributionRecord{
		SchemaVersion:  1,
		ArtifactType:   "distribution",
		ManifestDigest: recon.ManifestDigest,
		Torrent: artifact.Torrent{
			Infohash:          recon.InfohashBtmh,
			PieceLength:       recon.PieceLength,
			PieceLayersDigest: layersDigest,
		},
	}
	b, err := artifact.Canonical(rec)
	if err != nil {
		return nil, nil, err
	}
	return rec, b, nil
}

func readBlob(store *cas.Store, hexDigest string) ([]byte, error) {
	f, err := store.Open(hexDigest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func pinHex(manifestDigest string) (string, error) {
	s := strings.TrimPrefix(manifestDigest, "sha256:")
	if len(s) != 64 {
		return "", fmt.Errorf("swarm: import: manifestDigest must be canonical sha256:<64hex>, got %q", manifestDigest)
	}
	if _, err := hexDecode(s); err != nil {
		return "", fmt.Errorf("swarm: import: bad manifestDigest %q: %w", manifestDigest, err)
	}
	return strings.ToLower(s), nil
}

// addFreshTorrent drops any live torrent with the same infohash (its
// storage driver may hold stale sealed state — e.g. a blob deleted after
// seal) and adds a fresh one whose driver reflects current CAS presence.
func (c *Client) addFreshTorrent(spec *torrent.TorrentSpec, ihHex string) (*torrent.Torrent, error) {
	short := ihHex[:40]
	for _, t := range c.tc.Torrents() {
		if t.Info() != nil && fmt.Sprintf("%x", t.InfoHash()) == short {
			dropTorrent(t)
		}
	}
	t, _, err := c.tc.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("swarm: add torrent: %w", err)
	}
	return t, nil
}

// dropTorrent waits for the drop to finish — anacrolix drops are async,
// and a re-add of the same infohash racing the drop returns the stale
// torrent with its old storage state.
func dropTorrent(t *torrent.Torrent) {
	t.Drop()
	select {
	case <-t.Closed():
	case <-time.After(10 * time.Second):
		// Never blocks the flow forever; the re-add dedupe guards correctness.
	}
}

func tiersOf(trackers []string) [][]string {
	if len(trackers) == 0 {
		return nil
	}
	return [][]string{trackers}
}
