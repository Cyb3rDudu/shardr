package api

// /v1/import/catalog over the real API: a stub catalog provider, a stub
// HF tree anchor, a REAL foreign seeder, a REAL swarm client — the full
// daemon path (resolve → anchor → foreign pull → native import), plus
// the two refusal guards (rescued without the flag; provider unlisted).

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/Cyb3rDudu/shardr/internal/catalog"
	"github.com/Cyb3rDudu/shardr/internal/importer"
	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

// catalogE2E bundles the doubles a catalog pull needs.
type catalogE2E struct {
	h         *harness
	pfStub    *httptest.Server
	hfStub    *httptest.Server
	seeder    *torrent.Client
	seederAdr atomic.Value // string
	fx        v1FixtureLike
}

// v1FixtureLike is the subset of swarm's test fixture reused here (the
// fixture builder lives in the swarm package tests; this copy keeps the
// api package self-contained with identical bytes).
type v1FixtureLike struct {
	Name     string
	Infohash string
	Magnet   string
	Files    []v1FileLike
}
type v1FileLike struct {
	Path, SHA256, GitSHA1 string
	Content               []byte
}

// v1 fixture helpers (the swarm package's builder is test-local; this
// is the same construction, self-contained).
func foreignDeterministic(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func sha256HexOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func gitBlobOf(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

type fc struct {
	Path    string
	Content []byte
}

// buildV1Of builds the info dict + magnet (no trackers; peers injected
// per request) for ordered path/content pairs.
func buildV1Of(name string, files []fc, pieceLength int64) (infohash, magnet string) {
	var stream []byte
	var fis []map[string]any
	for _, f := range files {
		stream = append(stream, f.Content...)
		fis = append(fis, map[string]any{"length": int64(len(f.Content)), "path": strings.Split(f.Path, "/")})
	}
	var pieces []byte
	for off := int64(0); off < int64(len(stream)); off += pieceLength {
		end := off + pieceLength
		if end > int64(len(stream)) {
			end = int64(len(stream))
		}
		h := sha1.Sum(stream[off:end])
		pieces = append(pieces, h[:]...)
	}
	info := map[string]any{"name": name, "piece length": pieceLength, "files": fis, "pieces": pieces}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	ih := fmt.Sprintf("%x", mi.HashInfoBytes())
	q := url.Values{}
	q.Set("xt", "urn:btih:"+ih)
	q.Set("ws", "https://pirateface.co/api/ws/owner/repo/"+strings.Repeat("b", 40)+"/")
	return ih, "magnet:?" + q.Encode()
}

func metainfoOf(t *testing.T, name string, files []v1FileLike, pieceLength int64) *metainfo.MetaInfo {
	t.Helper()
	var stream []byte
	var fis []map[string]any
	for _, f := range files {
		stream = append(stream, f.Content...)
		fis = append(fis, map[string]any{"length": int64(len(f.Content)), "path": strings.Split(f.Path, "/")})
	}
	var pieces []byte
	for off := int64(0); off < int64(len(stream)); off += pieceLength {
		end := off + pieceLength
		if end > int64(len(stream)) {
			end = int64(len(stream))
		}
		h := sha1.Sum(stream[off:end])
		pieces = append(pieces, h[:]...)
	}
	info := map[string]any{"name": name, "piece length": pieceLength, "files": fis, "pieces": pieces}
	return &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
}

func (h *harness) startCatalogE2E(t *testing.T) *catalogE2E {
	t.Helper()
	e := &catalogE2E{h: h}

	// The fixture repo: importable (weights + config + tokenizer), piece
	// boundaries crossing files, deterministic bytes.
	files := []fc{
		{"config.json", []byte(`{"model_type":"toy","max_positional_embeddings":64}`)},
		{"model.safetensors", foreignDeterministic(150*1024 + 77)},
		{"tokenizer.json", []byte(`{"version":"1.0","model":{"type":"BPE"},"vocab":{"a":0,"b":1}}`)},
		{"tokenizer_config.json", []byte(`{"model_max_length":64,"tokenizer_class":"ToyTokenizer"}`)},
	}
	ih, mag := buildV1Of("owner__repo", files, 64*1024)
	fl := &v1FixtureLike{Name: "owner__repo", Infohash: ih, Magnet: mag}
	for _, f := range files {
		fl.Files = append(fl.Files, v1FileLike{Path: f.Path, Content: f.Content, SHA256: sha256HexOf(f.Content), GitSHA1: gitBlobOf(f.Content)})
	}
	e.fx = *fl

	// A real foreign seeder (plain BitTorrent client, file storage).
	dir := t.TempDir()
	for _, f := range e.fx.Files {
		p := filepath.Join(dir, e.fx.Name, f.Path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, f.Content, 0o644)
	}
	mi := metainfoOf(t, e.fx.Name, e.fx.Files, 64*1024)
	scfg := torrent.NewDefaultClientConfig()
	scfg.DataDir = dir
	scfg.NoDHT = true
	scfg.DisableIPv6 = true
	scfg.ListenHost = func(string) string { return "127.0.0.1" }
	scfg.Seed = true
	seeder, err := torrent.NewClient(scfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := seeder.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	<-st.GotInfo()
	if err := st.VerifyDataContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seeder.Close() })
	e.seeder = seeder
	for _, a := range seeder.ListenAddrs() {
		if a.Network() == "tcp" {
			e.seederAdr.Store(a.String())
		}
	}

	// Stub pirateface: /s/<repo> returns the listing line; the model page
	// carries the checksum record.
	rev := strings.Repeat("b", 40)
	e.pfStub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.EscapedPath(), "/s/") {
			magnet := "magnet:?xt=urn:btih:" + e.fx.Infohash +
				"&ws=" + url.QueryEscape("https://pirateface.co/api/ws/owner/repo/"+rev+"/")
			fmt.Fprintf(w, "owner/repo  150 KB  1 seeds  %s\n", magnet)
			return
		}
		if r.URL.Path == "/owner/repo" {
			var rec strings.Builder
			for i, f := range e.fx.Files {
				if i > 0 {
					rec.WriteString(`\n`)
				}
				fmt.Fprintf(&rec, `%s  %s`, f.SHA256, f.Path)
			}
			fmt.Fprintf(w, `x{\"manifest\":\"%s\"}`, rec.String())
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(e.pfStub.Close)

	// Stub HF: tree at the pinned revision (anchor of record).
	e.hfStub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/owner/repo/tree/"+rev) {
			var entries []map[string]any
			for _, f := range e.fx.Files {
				e := map[string]any{"type": "file", "path": f.Path, "oid": f.GitSHA1, "size": len(f.Content)}
				entries = append(entries, e)
			}
			json.NewEncoder(w).Encode(entries)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(e.hfStub.Close)

	h.server.Catalog = &catalog.Pirateface{BaseURL: e.pfStub.URL, HTTP: e.pfStub.Client()}
	h.server.HF = &importer.HFClient{BaseURL: e.hfStub.URL, HTTP: e.hfStub.Client()}

	// A REAL swarm client behind the API server.
	cfg := swarm.DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.DHT = false
	cfg.DisableIPv6 = true
	cfg.ListenHost = "127.0.0.1"
	cfg.WebseedAddr = "127.0.0.1:0"
	sc, err := swarm.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sc.Close)
	oldStore := h.store
	h.store = sc.Store()
	h.server.Swarm = sc
	h.server.store = sc.Store()
	_ = oldStore
	return e
}

func TestImportCatalogOverSocket(t *testing.T) {
	h := newHarness(t)
	e := h.startCatalogE2E(t)
	peer, _ := e.seederAdr.Load().(string)
	if peer == "" {
		t.Fatal("seeder not listening")
	}

	code, body := h.postJSON("/v1/import/catalog", map[string]any{
		"repo": "owner/repo", "peers": []string{peer},
	})
	if code != http.StatusCreated {
		t.Fatalf("import catalog: %d %s", code, body)
	}
	var job Job
	json.Unmarshal(body, &job)
	if job.Kind != "import-catalog" {
		t.Fatalf("kind: %+v", job)
	}
	term := waitJob(t, h, job.ID)
	if term.State != "done" {
		t.Fatalf("terminal: %+v (error %+v)", term, term.Error)
	}
	if len(term.Result.Quants) == 0 {
		t.Fatalf("no quants: %+v", term.Result)
	}
	// The artifact resolves under the lowercased repo namespace.
	q := term.Result.Quants[0]
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///owner/repo:"+q))
	if code != http.StatusOK {
		t.Fatalf("resolve: %d %s", code, body)
	}
	// Anchor convergence: every fixture digest is in the CAS.
	for _, f := range e.fx.Files {
		if !h.store.Has(f.SHA256) {
			t.Fatalf("anchor digest for %s missing from CAS", f.Path)
		}
	}
}

// The rescued refusal over the API (mutation guard): HF 404 without
// trustCatalog → 422 E_NOT_ANCHORED and NO job exists.
func TestImportCatalogRescuedRefused(t *testing.T) {
	h := newHarness(t)
	e := h.startCatalogE2E(t)
	// Flip the HF stub to 404: the source is gone.
	e.hfStub.Close()
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(gone.Close)
	h.server.HF = &importer.HFClient{BaseURL: gone.URL, HTTP: gone.Client()}

	code, body := h.postJSON("/v1/import/catalog", map[string]any{"repo": "owner/repo"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("rescued pull must be refused with 422, got %d %s", code, body)
	}
	var env struct {
		Error *APIError `json:"error"`
	}
	json.Unmarshal(body, &env)
	if env.Error == nil || env.Error.Code != "E_NOT_ANCHORED" {
		t.Fatalf("error class must be E_NOT_ANCHORED: %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "--trust-catalog") {
		t.Fatalf("refusal must name the escape hatch: %+v", env.Error)
	}
	if n := len(h.server.jobsSnapshot()); n != 0 {
		t.Fatalf("refused pull must not leave a job behind, found %d", n)
	}
}

func TestImportCatalogUnlistedRepo(t *testing.T) {
	h := newHarness(t)
	h.startCatalogE2E(t)
	code, body := h.postJSON("/v1/import/catalog", map[string]any{"repo": "who/what"})
	if code != http.StatusBadGateway {
		t.Fatalf("unlisted repo: %d %s", code, body)
	}
	if !strings.Contains(string(body), "E_UNKNOWN_REF") {
		t.Fatalf("unlisted must map to E_UNKNOWN_REF: %s", body)
	}
}

func TestImportCatalogRequiresCatalog(t *testing.T) {
	h := newHarness(t)
	code, _ := h.postJSON("/v1/import/catalog", map[string]any{"repo": "a/b"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("catalog disabled must be 503, got %d", code)
	}
}

// jobsSnapshot for the no-job assertion.
func (s *Server) jobsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.jobs {
		ids = append(ids, id)
	}
	return ids
}
