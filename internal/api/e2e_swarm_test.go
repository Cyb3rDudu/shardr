//go:build !race

package api

// Blocker #3 proof: the swarm fill runs over the REAL API — B fills from
// A via POST /v1/ensure with the source hints /import/bt persisted, not
// via a direct Client.Fill call. Two full shardhive stacks (separate CAS
// roots, swarm clients, API servers over their own sockets).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/importer"
	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

type apiE2ENode struct {
	t      *testing.T
	client *swarm.Client
	server *Server
	http   *http.Client
	pin    string // manifest digest of the shared artifact
}

func newAPIE2ENode(t *testing.T, name string) *apiE2ENode {
	t.Helper()
	cfg := swarm.DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	cfg.WebseedAddr = "127.0.0.1:0"
	c, err := swarm.New(cfg)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Cleanup(c.Close)
	// Short socket path (macOS sun_path limit).
	sockDir, err := os.MkdirTemp(os.TempDir(), "shxapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	srv, err := New(c.Store(), filepath.Join(sockDir, name+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Swarm = c
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", srv.Path())
			},
		},
		// Terminal jobs arrive within milliseconds; fills within seconds.
		Timeout: 120 * time.Second,
	}
	return &apiE2ENode{t: t, client: c, server: srv, http: hc}
}

func (n *apiE2ENode) post(path string, body any) (int, []byte) {
	n.t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := n.http.Post("http://shardhive"+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		n.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestEnsureFillOverAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E spins real swarm engines")
	}
	// A: local import (gold fixture + a generated multi-piece weights
	// file), then seed.
	a := newAPIE2ENode(t, "A")
	root := t.TempDir()
	copyTree(t, "../../internal/importer/testdata/gold", root)
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = byte(i*29 + 3)
	}
	if err := os.WriteFile(filepath.Join(root, "toy-00003-of-00003.gguf"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	sources, err := importer.LocalSources([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	res, err := importer.Import(context.Background(), a.client.Store(), sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}
	pin := res.Members[0].Manifest
	a.pin = pin
	if err := a.client.SeedArtifactFromCAS(context.Background(), pin); err != nil {
		t.Fatalf("A seed: %v", err)
	}

	// B: pinned BT import from A over the API.
	b := newAPIE2ENode(t, "B")
	// The infohash comes from A (B has no manifest yet).
	aRecon, _, _, err := swarm.ReconstructFromStore(a.client.Store(), pin)
	if err != nil {
		t.Fatal(err)
	}
	code, body := b.post("/v1/import/bt", map[string]any{
		"infohash":       aRecon.InfohashBtmh,
		"manifestDigest": pin,
		"webseeds":       []string{a.client.WebseedURL()},
		"peers":          a.client.PeerAddrs(),
	})
	if code != http.StatusCreated {
		t.Fatalf("import/bt: %d %s", code, body)
	}
	var job Job
	json.Unmarshal(body, &job)
	term := waitAPIJob(t, b, job.ID)
	if term.State != "done" {
		t.Fatalf("import/bt job: %+v (error %+v)", term, term.Error)
	}

	// B loses the multi-piece weights blob and refills via POST /v1/ensure.
	mb := readManifestBlob(t, b.client.Store(), pin)
	var m artifact.Manifest
	json.Unmarshal(mb, &m)
	victim := ""
	for _, f := range m.Files {
		if strings.HasSuffix(f.Name, "toy-00003-of-00003.gguf") {
			victim = strings.TrimPrefix(f.Digest, "sha256:")
			break
		}
	}
	if victim == "" {
		t.Fatal("big weights file not in manifest")
	}
	vp, err := b.client.Store().BlobPath(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(vp); err != nil {
		t.Fatal(err)
	}

	code, body = b.post("/v1/ensure", map[string]string{"ref": "shardr:///gold/repo@" + pin})
	if code != http.StatusCreated {
		t.Fatalf("ensure: %d %s", code, body)
	}
	json.Unmarshal(body, &job)
	if job.State == "done" {
		t.Fatal("ensure must be async while a blob is missing")
	}
	term = waitAPIJob(t, b, job.ID)
	if term.State != "done" {
		t.Fatalf("ensure job: %+v (error %+v)", term, term.Error)
	}
	if !b.client.Store().Has(victim) {
		t.Fatal("victim blob not refilled via POST /v1/ensure")
	}

	// Byte-identity against A for the refilled blob.
	fa, err := a.client.Store().Open(victim)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	fb, err := b.client.Store().Open(victim)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()
	if string(blobDigest(t, fa)) != string(blobDigest(t, fb)) {
		t.Fatal("refilled blob differs from A's")
	}
}

func waitAPIJob(t *testing.T, n *apiE2ENode, id string) Job {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("job %s never terminal", id)
		}
		resp, err := n.http.Get("http://shardhive/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var j Job
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		json.Unmarshal(raw, &j)
		if j.State == "done" || j.State == "failed" {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readManifestBlob(t *testing.T, s interface {
	Open(digest string) (*os.File, error)
}, pin string) []byte {
	t.Helper()
	f, err := s.Open(strings.TrimPrefix(pin, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return b
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func blobDigest(t *testing.T, r io.Reader) []byte {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	return h.Sum(nil)
}
