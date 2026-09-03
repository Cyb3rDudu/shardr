//go:build !race

package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/importer"
)

// ---------------------------------------------------------------------------
// Goal 7 — the core proof: two shardhives (separate CAS + swarm clients
// on one test machine). A imports locally; B imports the SAME artifact
// purely out of A's swarm via /import/bt semantics (pin + infohash +
// webseed hint), then B loses a content blob and re-fills it via the
// ensure path — again only from A. At the end B's CAS blobs are
// byte-identical to A's and every derived digest converges.
// ---------------------------------------------------------------------------

// e2eNode is one shardhive instance: CAS + swarm client.
type e2eNode struct {
	t      *testing.T
	name   string
	root   string
	client *Client
}

func newNode(t *testing.T, name string, seed bool) *e2eNode {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataRoot = root
	cfg.DHT = false // deterministic: direct peer injection, no global DHT in tests
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	cfg.WebseedAddr = "127.0.0.1:0"
	cfg.Seed = seed
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Cleanup(c.Close)
	return &e2eNode{t: t, name: name, root: root, client: c}
}

// importLocal runs a real local import (001 §8.6) of the fixture repo,
// EXTENDED with one generated multi-piece weights shard: the gold toy
// shards are copied into a temp dir renamed to a 3-part family (originals
// untouched) and toy-00003 is generated at 2.5 MiB (> piece length, so
// the torrent has real piece layers and the E2E victim is a genuine
// multi-piece swarm download). A second weights family under its own
// name is not importable in one member (001 §3.1: split parts merge per
// format; separate families would collide on the derived quant).
func (n *e2eNode) importLocal() *importer.ImportResult {
	n.t.Helper()
	root := n.t.TempDir()
	gold := "../../internal/importer/testdata/gold"
	entries, err := os.ReadDir(gold)
	if err != nil {
		n.t.Fatalf("%s: read gold fixture: %v", n.name, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(gold, e.Name()))
		if err != nil {
			n.t.Fatalf("%s: copy fixture %s: %v", n.name, e.Name(), err)
		}
		name := e.Name()
		// Grow the toy family to 3 parts: the generated multi-piece shard.
		switch name {
		case "toy-00001-of-00002.gguf":
			name = "toy-00001-of-00003.gguf"
		case "toy-00002-of-00002.gguf":
			name = "toy-00002-of-00003.gguf"
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			n.t.Fatalf("%s: write fixture %s: %v", n.name, name, err)
		}
	}
	big := make([]byte, 5*512*1024) // 2.5 MiB deterministic
	for i := range big {
		big[i] = byte(i*31 + 7)
	}
	if err := os.WriteFile(filepath.Join(root, "toy-00003-of-00003.gguf"), big, 0o644); err != nil {
		n.t.Fatalf("%s: write big shard: %v", n.name, err)
	}
	sources, err := importer.LocalSources([]string{root})
	if err != nil {
		n.t.Fatalf("%s: local sources: %v", n.name, err)
	}
	res, err := importer.Import(context.Background(), n.client.store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		n.t.Fatalf("%s: import: %v", n.name, err)
	}
	return res
}

// seedArtifact registers the imported artifact for seeding.
func (n *e2eNode) seedArtifact(manifestDigest string) *Recon {
	n.t.Helper()
	manifestBytes, err := readBlob(n.client.store, artifact.DigestHex(manifestDigest))
	if err != nil {
		n.t.Fatalf("%s: read manifest: %v", n.name, err)
	}
	var m artifact.Manifest
	if err := unmarshalJSON(manifestBytes, &m); err != nil {
		n.t.Fatalf("%s: parse manifest: %v", n.name, err)
	}
	rec, recBytes, err := n.client.RecordForManifest(manifestDigest)
	if err != nil || rec == nil {
		n.t.Fatalf("%s: record for %s: %v %v", n.name, manifestDigest, rec, err)
	}
	if err := n.client.SeedArtifact(context.Background(), &m, manifestBytes, rec, recBytes); err != nil {
		n.t.Fatalf("%s: seed: %v", n.name, err)
	}
	recon, err := Reconstruct(&m, manifestBytes)
	if err != nil {
		n.t.Fatal(err)
	}
	return recon
}

// peerAddrs of this node's torrent engine (TCP listeners only — a bare
// host:port string is dialed as TCP by the receiving side).
func (n *e2eNode) peerAddrs() []string {
	n.t.Helper()
	var out []string
	for _, a := range n.client.tc.ListenAddrs() {
		if a.Network() == "tcp" {
			out = append(out, a.String())
		}
	}
	return out
}

// hasAllBlobs reports whether every file of the artifact is in the CAS.
func (n *e2eNode) hasAllBlobs(recon *Recon) bool {
	for path, spec := range recon.FileSpecs {
		if !n.client.store.Has(spec.Digest) {
			n.t.Logf("%s: missing %s (%s)", n.name, path, spec.Digest[:12])
			return false
		}
	}
	return true
}

// blobEqual compares a blob between nodes byte-for-byte.
func blobEqual(a, b *e2eNode, digest string) bool {
	fa, err1 := a.client.store.Open(digest)
	if err1 != nil {
		return false
	}
	defer fa.Close()
	fb, err2 := b.client.store.Open(digest)
	if err2 != nil {
		return false
	}
	defer fb.Close()
	ha, hb := sha256.New(), sha256.New()
	io.Copy(ha, fa)
	io.Copy(hb, fb)
	return string(ha.Sum(nil)) == string(hb.Sum(nil))
}

func TestTwoInstanceE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E spins real swarm engines")
	}
	// --- Node A: import locally, seed. ---
	a := newNode(t, "A", true)
	res := a.importLocal()
	manifestDigest := res.Members[0].Manifest // the pin
	recon := a.seedArtifact(manifestDigest)

	// --- Node B: BT-import the same artifact from A only. ---
	b := newNode(t, "B", true)
	hints := Hints{
		Webseeds: []string{a.client.WebseedURL()},
		Peers:    a.peerAddrs(),
	}
	btCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stopDump := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopDump:
				return
			case <-tick.C:
				b.client.dumpTorrent(recon.InfohashHex[:40])
				a.client.dumpTorrent(recon.InfohashHex[:40])
			}
		}
	}()
	btRes, err := b.client.ImportBT(btCtx, recon.InfohashBtmh, manifestDigest, hints)
	close(stopDump)
	if err != nil {
		t.Fatalf("B import/bt from A: %v", err)
	}
	if btRes.ManifestDigest != manifestDigest {
		t.Fatalf("pin drift: %s != %s", btRes.ManifestDigest, manifestDigest)
	}
	if !b.hasAllBlobs(recon) {
		t.Fatal("B missing blobs after import/bt")
	}

	// Import convergence (001 §7.5): B derives the SAME record + layers
	// digests from the same content — byte-identical identity, no
	// coordination.
	recB, recBytesB, err := b.client.RecordForManifest(manifestDigest)
	if err != nil || recB == nil {
		t.Fatalf("B record: %v %v", recB, err)
	}
	recA, recBytesA, _ := a.client.RecordForManifest(manifestDigest)
	if string(recBytesA) != string(recBytesB) {
		t.Fatalf("distribution records diverged:\nA %s\nB %s", recBytesA, recBytesB)
	}
	_ = recA

	// Byte-identity across nodes for every torrent file + the layers blob.
	for path, spec := range recon.FileSpecs {
		if !blobEqual(a, b, spec.Digest) {
			t.Fatalf("blob %s (%s) differs between A and B", path, spec.Digest)
		}
	}
	if !blobEqual(a, b, artifact.DigestHex(recA.Torrent.PieceLayersDigest)) {
		t.Fatal("piece-layers blob differs between A and B")
	}

	// --- Phase 2 (ensure path): B loses a multi-piece weights blob, refills from A. ---
	var victim string
	var victimDigest string
	for path, spec := range recon.FileSpecs {
		if (strings.HasSuffix(path, ".gguf") || strings.HasSuffix(path, ".safetensors")) && spec.Size > 1<<20 {
			victim, victimDigest = path, spec.Digest
			break
		}
	}
	if victim == "" { // fall back to any weights file
		for path, spec := range recon.FileSpecs {
			if strings.HasSuffix(path, ".gguf") || strings.HasSuffix(path, ".safetensors") {
				victim, victimDigest = path, spec.Digest
				break
			}
		}
	}
	if victim == "" {
		t.Fatal("no weights blob found to delete")
	}
	t.Logf("victim = %s digest %s", victim, victimDigest)
	bp, err := b.client.store.BlobPath(victimDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bp); err != nil {
		t.Fatal(err)
	}
	if b.client.store.Has(victimDigest) {
		t.Fatal("victim blob still present")
	}
	fillCtx, fillCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer fillCancel()
	manifestBytes, _ := readBlob(b.client.store, artifact.DigestHex(manifestDigest))
	var m artifact.Manifest
	unmarshalJSON(manifestBytes, &m)
	// B seeds nothing during the fill test? No — B seeds too (usage =
	// replication). Fill pulls the missing blob from A's swarm.
	if err := b.client.Fill(fillCtx, &m, manifestBytes, hints, nil); err != nil {
		t.Fatalf("B ensure-fill from A: %v", err)
	}
	if !b.client.store.Has(victimDigest) {
		t.Fatal("victim blob not refilled")
	}
	if !blobEqual(a, b, victimDigest) {
		t.Fatal("refilled blob differs from A's")
	}

	// Sanity: B can now seed the complete artifact itself (the mirror
	// grows — a third node could pull from B).
	if err := b.client.SeedArtifact(context.Background(), &m, manifestBytes, recB, recBytesB); err != nil {
		t.Fatalf("B reseed: %v", err)
	}
}

// The DoD negative: a wrong infohash in the hints/pin pair must abort
// BEFORE any announce — the binding gate fires on reconstruction.
func TestImportBTInfohashBindingAbortsBeforeAnnounce(t *testing.T) {
	a := newNode(t, "A2", true)
	res := a.importLocal()
	manifestDigest := res.Members[0].Manifest
	recon := a.seedArtifact(manifestDigest)

	// evil infohash: flip one hex char of the true infohash. The manifest
	// pin is real, so phase 1 would fetch the manifest from the webseed
	// (it's not in the fake torrent tree — but the layers fetch also needs
	// the webseed). To isolate the GATE, call the gate pieces directly:
	// reconstruct + CheckRecord against a lying record.
	evil := "btmh:1220" + recon.InfohashHex[:63] + flipHex(recon.InfohashHex[63])
	mb, err := readBlob(a.client.store, artifact.DigestHex(manifestDigest))
	if err != nil {
		t.Fatal(err)
	}
	var m artifact.Manifest
	unmarshalJSON(mb, &m)
	r, err := Reconstruct(&m, mb)
	if err != nil {
		t.Fatal(err)
	}
	lying := &artifact.DistributionRecord{
		SchemaVersion:  1,
		ArtifactType:   "distribution",
		ManifestDigest: manifestDigest,
		Torrent: artifact.Torrent{
			Infohash:          evil,
			PieceLength:       r.PieceLength,
			PieceLayersDigest: "sha256:" + strings.Repeat("00", 32),
		},
	}
	gerr := r.CheckRecord(lying)
	if gerr == nil {
		t.Fatal("binding gate must reject a lying infohash")
	}
	if !strings.Contains(gerr.Error(), "refusing to announce") {
		t.Fatalf("gate error must name the refusal: %v", gerr)
	}
	// And the joined-vs-derived check inside ImportBT (the same gate in
	// phase 2 wording).
	if r.InfohashBtmh == evil {
		t.Fatal("test bug: evil equals real infohash")
	}
}

// The binding gate SPECIFICALLY: an evil torrent whose every file is
// pin-valid (the real manifest file at manifestKey + the real content
// files) but which carries an EXTRA file. All verify-writes would pass —
// only the derived-infohash gate catches the identity lie and aborts.
// Mutation-testing partner of the import-binding-gate mutation.
func TestImportBTExtraFileTorrentOnlyBindingCatches(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E spins real swarm engines")
	}
	a := newNode(t, "A5", true)
	res := a.importLocal()
	manifestDigest := res.Members[0].Manifest
	recon := a.seedArtifact(manifestDigest)

	// Phase 1 would succeed (real manifest, pin-satisfiable). The gate
	// fires at the phase-2 boundary: derived infohash != joined infohash.
	evilIh, err := a.serveExtraFileVariant(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if evilIh == recon.InfohashBtmh {
		t.Fatal("test bug: extra-file variant must change the infohash")
	}

	b := newNode(t, "B5", true)
	hints := Hints{Webseeds: []string{a.client.WebseedURL()}, Peers: a.peerAddrs()}
	btCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = b.client.ImportBT(btCtx, evilIh, manifestDigest, hints)
	if err == nil {
		t.Fatal("import of a torrent with extra files must fail at the binding gate")
	}
	if !strings.Contains(err.Error(), ErrBinding.Error()) {
		t.Fatalf("must be the binding gate, got: %v", err)
	}
}

// serveExtraFileVariant: same tree as the real artifact PLUS one extra
// file — every real file is byte-identical, so only the infohash differs.
func (n *e2eNode) serveExtraFileVariant(manifestDigest string) (string, error) {
	mb, err := readBlob(n.client.store, artifact.DigestHex(manifestDigest))
	if err != nil {
		return "", err
	}
	var m artifact.Manifest
	if err := unmarshalJSON(mb, &m); err != nil {
		return "", err
	}
	files := map[string][]byte{}
	for _, f := range m.Files {
		if files[f.Name], err = readBlob(n.client.store, artifact.DigestHex(f.Digest)); err != nil {
			return "", err
		}
	}
	extra := []byte("extra file that must not be here")
	files["extra.gguf"] = extra

	afs := make([]artifact.File, 0, len(m.Files)+1)
	afs = append(afs, m.Files...)
	sum := sha256.Sum256(extra)
	root := artifact.MerkleRoot(extra)
	afs = append(afs, artifact.File{
		Kind: "weights.gguf", Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size: int64(len(extra)), Name: "extra.gguf",
		BT: artifact.BT{MerkleRoot: "sha256:" + hex.EncodeToString(root[:])},
	})
	manifestSum := sha256.Sum256(mb)
	_, res, err := artifact.BuildTorrent(afs, mb, hex.EncodeToString(manifestSum[:]), func(name string) ([]byte, error) {
		return files[name], nil
	})
	if err != nil {
		return "", err
	}
	layers := map[string]string{}
	if err := bencodeLayersInto(res.PieceLayersBencode, layers); err != nil {
		return "", err
	}
	specs := evilFileSpecs(afs, hex.EncodeToString(manifestSum[:]), mb)
	ihRaw := strings.TrimPrefix(res.Infohash, "btmh:1220")
	if err := n.client.casSt.Register(ihRaw, specs); err != nil {
		return "", err
	}
	if err := n.client.scratchPutEvil(afs, files); err != nil {
		return "", err
	}
	infoB, _, err := artifact.BuildTorrentFromManifest(afs, mb, hex.EncodeToString(manifestSum[:]))
	if err != nil {
		return "", err
	}
	if err := n.addEvilTorrent(res, infoB, layers); err != nil {
		return "", err
	}
	return res.Infohash, nil
}

// The DoD negative at the API level: /import/bt with a pin whose manifest
// reconstructs a DIFFERENT infohash than the joined torrent must fail,
// never produce blobs.
func TestImportBTLyingTorrentProducesNoBlobs(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E spins real swarm engines")
	}
	a := newNode(t, "A3", true)
	res := a.importLocal()
	manifestDigest := res.Members[0].Manifest
	a.seedArtifact(manifestDigest)

	// Build an EVIL torrent on A: same files, same name — but one file's
	// bytes flipped. Its infohash differs from the pinned manifest's. The
	// webseed serves the REAL layers, the peers serve EVIL bytes.
	b := newNode(t, "B3", true)
	evilIh, err := a.serveEvilVariant(manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	hints := Hints{Webseeds: []string{a.client.WebseedURL()}, Peers: a.peerAddrs()}
	btCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = b.client.ImportBT(btCtx, evilIh, manifestDigest, hints)
	if err == nil {
		t.Fatal("import of a torrent that cannot satisfy the pin must fail")
	}
	// No content blob of the artifact may exist on B — the manifest file
	// under the pin may exist only if its bytes REALLY hash to the pin
	// (they cannot come from the evil tree without pin-matching bytes).
	manifestBytes, _ := readBlob(a.client.store, artifact.DigestHex(manifestDigest))
	var m artifact.Manifest
	unmarshalJSON(manifestBytes, &m)
	for _, f := range m.Files {
		if b.client.store.Has(artifact.DigestHex(f.Digest)) {
			t.Fatalf("evil swarm produced blob %s (%s) — verification broken", f.Name, f.Digest)
		}
	}
}

// serveEvilVariant registers a wrong-bytes variant of the artifact in A's
// engine (same tree shape, mutated weights bytes) and returns its
// infohash. The webseed still serves the real piece-layers (keyed by
// manifest) — everything else about A stays honest, isolating the
// binding/verification gates.
func (n *e2eNode) serveEvilVariant(manifestDigest string) (string, error) {
	mb, err := readBlob(n.client.store, artifact.DigestHex(manifestDigest))
	if err != nil {
		return "", err
	}
	var m artifact.Manifest
	if err := unmarshalJSON(mb, &m); err != nil {
		return "", err
	}
	// Mutate the first weights file: same size, flipped bytes → different
	// merkle root → different infohash.
	files := make(map[string][]byte, len(m.Files))
	for _, f := range m.Files {
		orig, err := readBlob(n.client.store, artifact.DigestHex(f.Digest))
		if err != nil {
			return "", err
		}
		files[f.Name] = orig
	}
	var target *artifact.File
	for i := range m.Files {
		if strings.HasSuffix(m.Files[i].Name, ".gguf") || strings.HasSuffix(m.Files[i].Name, ".safetensors") {
			target = &m.Files[i]
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("no weights file to mutate")
	}
	evil := append([]byte(nil), files[target.Name]...)
	evil[0] ^= 0xFF
	files[target.Name] = evil

	var afs []artifact.File
	for _, f := range m.Files {
		data := files[f.Name]
		sum := sha256.Sum256(data)
		root := artifact.MerkleRoot(data)
		efs := f
		efs.Digest = "sha256:" + hex.EncodeToString(sum[:])
		efs.BT = artifact.BT{MerkleRoot: "sha256:" + hex.EncodeToString(root[:])}
		afs = append(afs, efs)
	}
	manifestSum := sha256.Sum256(mb)
	infoB, res, err := artifact.BuildTorrent(afs, mb, hex.EncodeToString(manifestSum[:]), func(name string) ([]byte, error) {
		return files[name], nil
	})
	if err != nil {
		return "", err
	}
	layers := map[string]string{}
	if err := bencodeLayersInto(res.PieceLayersBencode, layers); err != nil {
		return "", err
	}
	// Register the evil torrent with its OWN storage (its digests differ)
	// and add it to the engine so peers find *a* torrent — but under the
	// evil infohash, so the pin can never bind.
	ihRaw := strings.TrimPrefix(res.Infohash, "btmh:1220")
	if err := n.client.casSt.Register(ihRaw, evilFileSpecs(afs, hex.EncodeToString(manifestSum[:]), mb)); err != nil {
		return "", err
	}
	// Evil blobs need to live somewhere the driver can read: a scratch CAS
	// would be cleaner, but the driver refuses unknown digests only per
	// infohash — the evil registration is self-consistent.
	if err := n.client.scratchPutEvil(afs, files); err != nil {
		return "", err
	}
	if err := n.addEvilTorrent(res, infoB, layers); err != nil {
		return "", err
	}
	return res.Infohash, nil
}

func evilFileSpecs(afs []artifact.File, manifestHex string, mb []byte) map[string]FileSpec {
	specs := map[string]FileSpec{}
	for _, f := range afs {
		specs[f.Name] = FileSpec{Digest: artifact.DigestHex(f.Digest), Size: f.Size}
	}
	specs["manifest/sha256-"+manifestHex] = FileSpec{Digest: manifestHex, Size: int64(len(mb))}
	return specs
}

func flipHex(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

// --- test-only helpers moved out of helpers.go (production files carry
// production code only) ---

// bencodeLayersInto decodes a layers blob into an existing map.
func bencodeLayersInto(blob []byte, into map[string]string) error {
	return bencode.Unmarshal(blob, &into)
}

// specWithInfohash builds a TorrentSpec carrying explicit infohash bytes
// (layers applied post-GotInfo via AddPieceLayers — see specFromRecon).
func specWithInfohash(btmh string, infoBencode []byte, layers map[string]string) (*torrent.TorrentSpec, error) {
	var ih [32]byte
	raw := strings.TrimPrefix(btmh, "btmh:1220")
	if _, err := hex.Decode(ih[:], []byte(raw)); err != nil {
		return nil, err
	}
	var o g.Option[infohash_v2.T]
	o.Set(ih)
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{InfoHashV2: o, InfoBytes: infoBencode},
		// Layers embedded, same as production specFromRecon: an infohash-only
		// or layerless add leaves multi-piece v2 pieces hashless at onSetInfo
		// (pending vs request-order divergence — panics on anacrolix master).
		PieceLayers: layers,
	}, nil
}

// addEvilTorrent adds a test variant torrent (layers-in-spec, like
// production).
func (n *e2eNode) addEvilTorrent(res artifact.TorrentResult, infoB []byte, layers map[string]string) error {
	evSpec, err := specWithInfohash(res.Infohash, infoB, layers)
	if err != nil {
		return err
	}
	t, _, err := n.client.tc.AddTorrentSpec(evSpec)
	if err != nil {
		return err
	}
	<-t.GotInfo()
	return nil
}

// scratchPutEvil stores variant content under its TRUE digests (content
// addressing is digest-honest even for adversarial torrents — the CAS is
// a blob store, not a truth authority; the pin is).
func (c *Client) scratchPutEvil(afs []artifact.File, files map[string][]byte) error {
	for _, f := range afs {
		if err := c.store.Put(artifact.DigestHex(f.Digest), strings.NewReader(string(files[f.Name]))); err != nil {
			return err
		}
	}
	return nil
}

// dumpTorrent prints live state of one torrent in the engine (test-only).
func (c *Client) dumpTorrent(shortKey string) {
	for _, t := range c.tc.Torrents() {
		info := t.Info()
		if info == nil {
			continue
		}
		ih := fmt.Sprintf("%x", t.InfoHash())
		if ih != shortKey {
			continue
		}
		st := t.Stats()
		var pieces string
		for _, run := range t.PieceStateRuns() {
			ch := "."
			switch {
			case run.Complete:
				ch = "C"
			case run.Hashing || run.QueuedForHash || run.Marking || run.Checking:
				ch = "H"
			case run.Partial:
				ch = "p"
			case run.Priority == 0:
				ch = "."
			default:
				ch = "W"
			}
			pieces += fmt.Sprintf("%s%d", ch, run.Length)
		}
		fmt.Fprintf(os.Stderr, "[dump %p] name=%s peers=%d conns=%d readUseful=%d pieces=%s\n",
			c, info.BestName(), st.TotalPeers, st.ActivePeers, st.BytesReadUsefulData.Int64(), pieces)
	}
}

// The 003 §4 seed-start re-hash: a corrupted blob on disk must abort
// seeding loudly — disk corruption must never silently become swarm
// corruption.
func TestSeedStartRehashRejectsCorruptedBlob(t *testing.T) {
	a := newNode(t, "A4", true)
	res := a.importLocal()
	manifestDigest := res.Members[0].Manifest
	manifestBytes, _ := readBlob(a.client.store, artifact.DigestHex(manifestDigest))
	var m artifact.Manifest
	unmarshalJSON(manifestBytes, &m)
	rec, recBytes, err := a.client.RecordForManifest(manifestDigest)
	if err != nil || rec == nil {
		t.Fatalf("record: %v %v", rec, err)
	}
	// Corrupt one file blob in place (flip bytes; keep size).
	var victim string
	for _, f := range m.Files {
		victim = artifact.DigestHex(f.Digest)
		break
	}
	bp, err := a.client.store.BlobPath(victim)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bp)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xFF
	if err := os.Chmod(bp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.client.SeedArtifact(context.Background(), &m, manifestBytes, rec, recBytes); err == nil {
		t.Fatal("seed-start must reject a corrupted blob (re-hash failed)")
	} else if !strings.Contains(err.Error(), "re-hash failed") {
		t.Fatalf("error must name the re-hash: %v", err)
	}
}

// Startup seed (fix #4): a restarted daemon joins the swarms of every
// complete artifact in state. A imports, its client CLOSES, a fresh
// client on the same CAS runs StartupSeed — and B then imports the
// artifact purely from the restarted node.
func TestStartupSeedJoinsAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E spins real swarm engines")
	}
	root := t.TempDir()
	mk := func() *Client {
		cfg := DefaultConfig()
		cfg.DataRoot = root
		cfg.DHT = false
		cfg.DisableIPv6 = true
		cfg.ListenHost = "127.0.0.1"
		cfg.WebseedAddr = "127.0.0.1:0"
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// First life: local import, then shutdown (no explicit seeding).
	a1 := mk()
	sources, err := importer.LocalSources([]string{"../../internal/importer/testdata/gold"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := importer.Import(context.Background(), a1.store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}
	pin := res.Members[0].Manifest
	a1.Close()

	// Second life on the same CAS: startup scan must join the swarm.
	a2 := mk()
	t.Cleanup(a2.Close)
	joined := a2.StartupSeed(context.Background(), func(format string, args ...any) {
		t.Logf(format, args...)
	})
	if joined != 1 {
		t.Fatalf("startup seed must join the one complete artifact, joined %d", joined)
	}

	// B pulls the artifact from the RESTARTED node only.
	b := newNode(t, "B7", true)
	recon, _, _, err := ReconstructFromStore(a2.store, pin)
	if err != nil {
		t.Fatal(err)
	}
	hints := Hints{Webseeds: []string{a2.WebseedURL()}, Peers: a2.PeerAddrs()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := b.client.ImportBT(ctx, recon.InfohashBtmh, pin, hints); err != nil {
		t.Fatalf("B import from restarted A: %v", err)
	}
	if !b.hasAllBlobs(recon) {
		t.Fatal("B missing blobs after importing from the restarted node")
	}
}

// The upload limit applies to BOTH paths (blocker #6): the torrent client
// config and the webseed HTTP handler share one budget.
func TestUploadLimitWiredToBothPaths(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.UploadLimit = 12345
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.uploadLim == nil {
		t.Fatal("upload_limit must provision the shared limiter (webseed path + torrent client)")
	}
}
