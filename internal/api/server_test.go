package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/cas"
	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// harness spins up a real server on a Unix socket and hands out an HTTP
// client wired to it, so every test exercises the actual wire protocol.
type harness struct {
	t      *testing.T
	store  *cas.Store
	server *Server
	client *http.Client
	base   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	t.Setenv("SHARDR_CAS", root)
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the socket path short: macOS sun_path is ~104 bytes and
	// nested Go test temp dirs blow past it.
	sockDir, err := os.MkdirTemp(os.TempDir(), "shx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "hive.sock")
	srv, err := New(store, sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve() // returns on Close
	t.Cleanup(func() { srv.Close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return &harness{t: t, store: store, server: srv, client: client, base: "http://shardhive"}
}

func (h *harness) do(method, path string, body io.Reader) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.base+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (h *harness) get(path string) (int, []byte) { return h.do(http.MethodGet, path, nil) }

func (h *harness) postJSON(path string, v any) (int, []byte) {
	b, _ := json.Marshal(v)
	return h.do(http.MethodPost, path, bytes.NewReader(b))
}

// putHashed writes content into the CAS and returns its bare-hex digest.
func (h *harness) putHashed(content []byte) string {
	h.t.Helper()
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	if err := h.store.Put(d, bytes.NewReader(content)); err != nil {
		h.t.Fatal(err)
	}
	return d
}

// --- Transport: socket mode 0600 is the access boundary ---

func TestSocketMode0600(t *testing.T) {
	h := newHarness(t)
	fi, err := os.Stat(h.server.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o want 0600", fi.Mode().Perm())
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	h := newHarness(t) // owns the socket
	srv2, err := New(h.store, h.server.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv2.Listen(); err == nil {
		srv2.Close()
		t.Fatal("second daemon accepted a live socket")
	}
}

// --- Blob inventory snapshot: proves /resolve and /open are pure ---

func blobSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	base := filepath.Join(root, "blobs", "sha256")
	filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			m[path] = fmt.Sprintf("%d:%s", d.Type(), sha256Sum(b))
		}
		return nil
	})
	return m
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func testDigestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func requireEqualMaps(t *testing.T, before, after map[string]string, ctx string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: blob set changed %d -> %d entries", ctx, len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s: blob %s mutated", ctx, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Fatalf("%s: new blob %s appeared", ctx, k)
		}
	}
}

// seedRepo writes a model-index blob with three members, registers it as
// the namespace's current index, and returns the index digest (bare hex).
// q8_0's manifest is the digest of q8ManifestContent (caller decides
// whether that blob is actually present); q4_0/q4_1 use fixed digests that
// start absent.
func seedRepo(t *testing.T, h *harness, nsKey, q8ManifestContent string) string {
	t.Helper()
	q8 := testDigestOf([]byte(q8ManifestContent))
	index := `{"artifactType":"model-index","schemaVersion":1,"members":[` +
		`{"quant":"q8_0","manifest":"sha256:` + q8 + `","weightsFormat":"gguf"},` +
		`{"quant":"q4_0","manifest":"sha256:4081c843632a7f131bb8003ad639f3a16fa5bf1d7a6c3c05d16c11e818c23b63","weightsFormat":"gguf"},` +
		`{"quant":"q4_1","manifest":"sha256:` + testDigestOf([]byte("q4_1-manifest-bytes")) + `","weightsFormat":"gguf"}]}`
	d := h.putHashed([]byte(index))
	if err := h.store.SetNamespace(nsKey, d); err != nil {
		t.Fatal(err)
	}
	return d
}

// --- /v1/resolve ---

func TestResolvePureNoCASMutation(t *testing.T) {
	h := newHarness(t)
	q8 := "sha256:" + testDigestOf([]byte("q8-manifest-bytes"))
	seedRepo(t, h, "unsloth/qwen3.8-27b-gguf", "q8-manifest-bytes")
	before := blobSnapshot(t, h.store.Root)

	cases := []struct {
		ref   string
		quant string
	}{
		{"shardr:///unsloth/qwen3.8-27b-gguf:q8_0", "q8_0"}, // canonical URI only
		{"shardr:///unsloth/qwen3.8-27b-gguf:q8", "q8_0"},   // unique prefix
	}
	for _, c := range cases {
		code, body := h.get("/v1/resolve?ref=" + url.QueryEscape(c.ref))
		if code != http.StatusOK {
			t.Fatalf("ref %s: status %d body %s", c.ref, code, body)
		}
		var res resolveResult
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
		if res.ManifestDigest != q8 {
			t.Fatalf("ref %s: manifest %s", c.ref, res.ManifestDigest)
		}
		if res.Plan != "pending" {
			t.Fatalf("plan %s want pending", res.Plan)
		}
	}
	requireEqualMaps(t, before, blobSnapshot(t, h.store.Root), "resolve")
}

func TestResolveErrors(t *testing.T) {
	h := newHarness(t)
	seedRepo(t, h, "unsloth/qwen3.8-27b-gguf", "q8-manifest-bytes")
	seedRepo(t, h, "unsloth/other-model", "other-bytes")

	// Unknown ref: loud, with same-namespace candidates, no network.
	code, body := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///unsloth/missing:q8_0"))
	if code != http.StatusNotFound {
		t.Fatalf("status %d body %s", code, body)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != ErrUnknownRef {
		t.Fatalf("code %s", e.Error.Code)
	}
	wantC := []string{"unsloth/qwen3.8-27b-gguf", "unsloth/other-model"}
	sort.Strings(e.Error.Candidates)
	sort.Strings(wantC)
	if strings.Join(e.Error.Candidates, ",") != strings.Join(wantC, ",") {
		t.Fatalf("candidates %v want %v", e.Error.Candidates, wantC)
	}

	// Ambiguous prefix lists candidates (000 §3.1 via API).
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///unsloth/qwen3.8-27b-gguf:q4"))
	if code != http.StatusBadRequest {
		t.Fatalf("ambiguous: status %d body %s", code, body)
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != "E_AMBIGUOUS_SELECTOR" || len(e.Error.Candidates) != 2 ||
		e.Error.Candidates[0] != "q4_0" || e.Error.Candidates[1] != "q4_1" {
		t.Fatalf("ambiguous payload: %+v", e.Error)
	}

	// No selector is a resolution error (000 §2).
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///unsloth/qwen3.8-27b-gguf"))
	if code != http.StatusBadRequest {
		t.Fatalf("no-selector: status %d body %s", code, body)
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ref.ErrNoSelector {
		t.Fatalf("no-selector code %s", e.Error.Code)
	}

	// Missing ref parameter.
	code, body = h.get("/v1/resolve")
	if code != http.StatusBadRequest {
		t.Fatalf("no param: status %d", code)
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrBadRequest {
		t.Fatalf("no param code %s", e.Error.Code)
	}
}

func TestResolveNoIndexBlob(t *testing.T) {
	h := newHarness(t)
	// Namespace points at a digest that is not in the CAS.
	ghost := "sha256:" + testDigestOf([]byte("ghost index"))
	if err := h.store.SetNamespace("ns/ghost", strings.TrimPrefix(ghost, "sha256:")); err != nil {
		t.Fatal(err)
	}
	code, body := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///ns/ghost:q8_0"))
	if code != http.StatusNotFound {
		t.Fatalf("status %d body %s", code, body)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrNoIndex {
		t.Fatalf("code %s want E_NO_INDEX", e.Error.Code)
	}
}

// --- /v1/ensure + /v1/jobs ---

func TestEnsureLocalPresence(t *testing.T) {
	h := newHarness(t)
	manifestContent := `{"artifactType":"model-manifest"}`
	seedRepo(t, h, "ns/models", manifestContent)
	manifestHex := testDigestOf([]byte(manifestContent))

	// Missing manifest → failed with E_SOURCE_UNAVAILABLE naming reserved sources.
	code, body := h.postJSON("/v1/ensure", map[string]string{"ref": "shardr:///ns/models:q8_0"})
	if code != http.StatusCreated {
		t.Fatalf("status %d body %s", code, body)
	}
	var job Job
	json.Unmarshal(body, &job)
	if job.State != "failed" || job.Error == nil || job.Error.Code != ErrSourceUnavail {
		t.Fatalf("job %+v", job)
	}
	if !strings.Contains(job.Error.Message, "/v1/import/") {
		t.Fatalf("error must name reserved sources: %s", job.Error.Message)
	}
	// Job is retrievable and terminal.
	code, body = h.get("/v1/jobs/" + job.ID)
	if code != http.StatusOK {
		t.Fatalf("jobs: status %d", code)
	}
	var got Job
	json.Unmarshal(body, &got)
	if got.State != "failed" {
		t.Fatalf("job state %s", got.State)
	}

	// Unknown ref → failed with the resolution error, never a network fetch.
	code, body = h.postJSON("/v1/ensure", map[string]string{"ref": "shardr:///ns/nope:q8_0"})
	if code != http.StatusCreated {
		t.Fatalf("unknown ref: status %d body %s", code, body)
	}
	json.Unmarshal(body, &job)
	if job.State != "failed" || job.Error.Code != ErrUnknownRef {
		t.Fatalf("job %+v", job)
	}

	// Present manifest → done (idempotent local hit).
	if err := h.store.Put(manifestHex, strings.NewReader(manifestContent)); err != nil {
		t.Fatal(err)
	}
	code, body = h.postJSON("/v1/ensure", map[string]string{"ref": "shardr:///ns/models:q8_0"})
	if code != http.StatusCreated {
		t.Fatalf("present: status %d body %s", code, body)
	}
	json.Unmarshal(body, &job)
	if job.State != "done" || job.Manifest != "sha256:"+manifestHex {
		t.Fatalf("job %+v", job)
	}

	// Unknown job id → 404.
	code, _ = h.get("/v1/jobs/doesnotexist")
	if code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", code)
	}
}

// --- /v1/open ---

func TestOpenListsPathsAndMissing(t *testing.T) {
	h := newHarness(t)
	q8 := "q8-manifest-body"
	indexD := seedRepo(t, h, "ns/models", q8) // index blob present
	// q4_0/q4_1 manifests absent; q8_0 manifest present.
	q8Hex := testDigestOf([]byte(q8))
	if err := h.store.Put(q8Hex, strings.NewReader(q8)); err != nil {
		t.Fatal(err)
	}

	code, body := h.get("/v1/open?ref=" + url.QueryEscape("shardr:///ns/models:q8_0"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	var res resolveResult
	json.Unmarshal(body, &res)
	if len(res.Files) != 2 {
		t.Fatalf("files %+v", res.Files)
	}
	for _, f := range res.Files {
		if !strings.HasPrefix(f.Path, h.store.Root) || !strings.Contains(f.Path, "blobs/sha256/") {
			t.Fatalf("file path %s is not a CAS path", f.Path)
		}
		if f.Size == 0 {
			t.Fatalf("file %s has no size", f.Digest)
		}
	}
	// Both index and manifest present → nothing missing, nothing auto-filled.
	if len(res.Missing) != 0 {
		t.Fatalf("missing %+v", res.Missing)
	}

	// Absent manifest: listed as missing, never fetched.
	code, body = h.get("/v1/open?ref=" + url.QueryEscape("shardr:///ns/models:q4_0"))
	if code != http.StatusOK {
		t.Fatalf("q4_0: status %d body %s", code, body)
	}
	json.Unmarshal(body, &res)
	if len(res.Files) != 1 || res.Files[0].Digest != "sha256:"+indexD {
		t.Fatalf("files %+v", res.Files)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "sha256:4081c843632a7f131bb8003ad639f3a16fa5bf1d7a6c3c05d16c11e818c23b63" {
		t.Fatalf("missing %+v", res.Missing)
	}
}

// --- /v1/blob: range math on a real blob ---

func TestBlobRangeSemantics(t *testing.T) {
	h := newHarness(t)
	content := []byte("0123456789abcdefghij") // 20 bytes
	d := h.putHashed(content)
	hex := strings.TrimPrefix(d, "sha256:")

	// Full body → 200.
	code, body := h.get("/v1/blob/" + hex)
	if code != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("full: %d %q", code, body)
	}

	// Valid range → 206 with exact bytes.
	req, _ := http.NewRequest(http.MethodGet, h.base+"/v1/blob/"+hex, nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(b) != "2345" {
		t.Fatalf("range 2-5: %d %q", resp.StatusCode, b)
	}

	// Open-ended range → 206 to EOF.
	req, _ = http.NewRequest(http.MethodGet, h.base+"/v1/blob/"+hex, nil)
	req.Header.Set("Range", "bytes=15-")
	resp, err = h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(b) != "fghij" {
		t.Fatalf("range 15-: %d %q", resp.StatusCode, b)
	}

	// Range beyond EOF → 416.
	req, _ = http.NewRequest(http.MethodGet, h.base+"/v1/blob/"+hex, nil)
	req.Header.Set("Range", "bytes=100-200")
	resp, err = h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range 100-200: %d want 416", resp.StatusCode)
	}

	// Invalid digest and missing blob → loud 404s, error envelope.
	code, body = h.get("/v1/blob/" + strings.Repeat("z", 64))
	if code != http.StatusBadRequest {
		t.Fatalf("invalid digest: %d", code)
	}
	code, body = h.get("/v1/blob/" + testDigestOf([]byte("never written")))
	if code != http.StatusNotFound {
		t.Fatalf("missing blob: %d", code)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrNotFound {
		t.Fatalf("missing blob code %s", e.Error.Code)
	}
}

// --- Version negotiation ---

func TestVersionNegotiation(t *testing.T) {
	h := newHarness(t)
	code, body := h.get("/v2/resolve?ref=x")
	if code != http.StatusBadRequest {
		t.Fatalf("v2: status %d", code)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != ErrUnsupportedVersion {
		t.Fatalf("v2 code %s", e.Error.Code)
	}
	if len(e.Error.Candidates) != 1 || e.Error.Candidates[0] != "v1" {
		t.Fatalf("supported version not advertised: %+v", e.Error)
	}
	// Unknown v1 endpoint → plain 404 in error form.
	code, body = h.get("/v1/nope")
	if code != http.StatusNotFound {
		t.Fatalf("v1 unknown: %d", code)
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrNotFound {
		t.Fatalf("v1 unknown code %s", e.Error.Code)
	}
}

// --- Reserved imports ---

func TestImportsReserved(t *testing.T) {
	h := newHarness(t)
	for _, ep := range []string{"/v1/import/local", "/v1/import/hf", "/v1/import/bt"} {
		code, body := h.postJSON(ep, map[string]any{})
		if code != http.StatusNotImplemented {
			t.Fatalf("%s: status %d body %s", ep, code, body)
		}
		var e struct {
			Error APIError `json:"error"`
		}
		json.Unmarshal(body, &e)
		if e.Error.Code != ErrNotImplemented {
			t.Fatalf("%s: code %s", ep, e.Error.Code)
		}
	}
}

// --- /v1/models ---

func TestModelsInventory(t *testing.T) {
	h := newHarness(t)
	present := seedRepo(t, h, "ns/models", "any") // index blob in CAS
	ghost := testDigestOf([]byte("ghost"))
	if err := h.store.SetNamespace("ns/ghost", ghost); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTagAlias("ns/models", "stable", strings.TrimPrefix(present, "sha256:")); err != nil {
		t.Fatal(err)
	}
	code, body := h.get("/v1/models")
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	var inv struct {
		Skeleton   bool `json:"skeleton"`
		Namespaces []struct {
			Name         string   `json:"name"`
			IndexPresent bool     `json:"indexPresent"`
			Quants       []string `json:"quants"`
		} `json:"namespaces"`
		Tags []struct {
			Repo        string `json:"repo"`
			Tag         string `json:"tag"`
			BlobPresent bool   `json:"blobPresent"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatal(err)
	}
	if !inv.Skeleton {
		t.Fatal("inventory must be marked as skeleton")
	}
	if len(inv.Namespaces) != 2 || len(inv.Tags) != 1 {
		t.Fatalf("inventory %+v", inv)
	}
	byName := map[string]bool{}
	for _, n := range inv.Namespaces {
		byName[n.Name] = n.IndexPresent
	}
	if !byName["models"] || byName["ghost"] {
		t.Fatalf("presence flags wrong: %+v", inv.Namespaces)
	}
	if !inv.Tags[0].BlobPresent {
		t.Fatalf("tag presence wrong")
	}
}

// --- Tag-based resolution through the API ---

func TestResolveViaTag(t *testing.T) {
	h := newHarness(t)
	indexD := seedRepo(t, h, "ns/models", "q8-manifest-bytes")
	if err := h.store.SetTagAlias("ns/models", "Stable", strings.TrimPrefix(indexD, "sha256:")); err != nil {
		t.Fatal(err)
	}
	// Tag snapshot resolves like the current index; tag is case-sensitive.
	code, body := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///ns/models:Stable+q8_0"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	var res resolveResult
	json.Unmarshal(body, &res)
	if res.Tag != "Stable" || res.ManifestDigest != "sha256:"+testDigestOf([]byte("q8-manifest-bytes")) {
		t.Fatalf("res %+v", res)
	}
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///ns/models:miss+q8_0"))
	if code != http.StatusNotFound { // unknown tag == unknown ref (R3 scoping)
		t.Fatalf("unknown tag: %d", code)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrUnknownRef {
		t.Fatalf("unknown tag code %s", e.Error.Code)
	}
}

// --- Fix 1: short refs are banished from the API (canonical form only) ---

func TestAPIRejectsShortRefsWithCanonicalHint(t *testing.T) {
	h := newHarness(t)
	seedRepo(t, h, "ns/models", "x")

	// resolve via query param
	code, body := h.get("/v1/resolve?ref=ns/models:q8_0")
	if code != http.StatusBadRequest {
		t.Fatalf("short ref accepted: %d %s", code, body)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	json.Unmarshal(body, &e)
	if !strings.Contains(e.Error.Message, "shardr:///ns/models:q8_0") {
		t.Fatalf("message must name the canonical form, got: %s", e.Error.Message)
	}

	// ensure via JSON body
	code, body = h.postJSON("/v1/ensure", map[string]string{"ref": "ns/models:q8_0"})
	if code != http.StatusBadRequest {
		t.Fatalf("short ref in ensure accepted: %d %s", code, body)
	}
	json.Unmarshal(body, &e)
	if !strings.Contains(e.Error.Message, "shardr:///ns/models:q8_0") {
		t.Fatalf("ensure message must name the canonical form, got: %s", e.Error.Message)
	}

	// Unparseable garbage does not get a canonical hint.
	code, body = h.get("/v1/resolve?ref=garbage")
	if code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", code)
	}
	json.Unmarshal(body, &e)
	if strings.Contains(e.Error.Message, "use shardr:///") {
		t.Fatalf("garbage must not get a canonical hint: %s", e.Error.Message)
	}
}

// --- Fix 2: socket path protection (never delete non-sockets) ---

func TestListenProtectsNonSocketPaths(t *testing.T) {
	store, _ := cas.Open(t.TempDir())

	// Short dir: nested Go test temp dirs exceed macOS's ~104-byte sun_path.
	dir, err := os.MkdirTemp(os.TempDir(), "shxfix")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	filePath := filepath.Join(dir, "precious.sock")
	const payload = "not a socket — user data"
	os.WriteFile(filePath, []byte(payload), 0o644)
	srv, err := New(store, filePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err == nil {
		srv.Close()
		t.Fatal("Listen must refuse to replace a regular file")
	}
	b, _ := os.ReadFile(filePath)
	if string(b) != payload {
		t.Fatalf("regular file damaged: %q", b)
	}

	// Directory at the socket path.
	dirPath := filepath.Join(dir, "dir.sock")
	os.Mkdir(dirPath, 0o755)
	srv, err = New(store, dirPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err == nil {
		srv.Close()
		t.Fatal("Listen must refuse to replace a directory")
	}
	if fi, err := os.Stat(dirPath); err != nil || !fi.IsDir() {
		t.Fatalf("directory damaged: %v", err)
	}

	// Symlink at the socket path.
	target := filepath.Join(dir, "target")
	os.WriteFile(target, nil, 0o644)
	linkPath := filepath.Join(dir, "link.sock")
	os.Symlink(target, linkPath)
	srv, err = New(store, linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err == nil {
		srv.Close()
		t.Fatal("Listen must refuse to replace a symlink")
	}
	if fi, err := os.Lstat(linkPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink damaged: %v", err)
	}

	// Genuinely orphaned socket (dead listener): replaced cleanly.
	sockPath := filepath.Join(dir, "orphan.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // socket file remains, nothing accepts on it
	srv, err = New(store, sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("orphaned socket must be replaceable: %v", err)
	}
	srv.Close()
}

// --- Fix 3: 416 responses stay in the JSON error envelope ---

func TestBlob416ErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	d := h.putHashed([]byte("0123456789"))

	req, _ := http.NewRequest(http.MethodGet, h.base+"/v1/blob/"+d, nil)
	req.Header.Set("Range", "bytes=99999-")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status %d want 416", resp.StatusCode)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("416 body is not the JSON error envelope: %v", err)
	}
	if e.Error.Code != ErrRangeInvalid || e.Error.Message == "" {
		t.Fatalf("envelope payload: %+v", e.Error)
	}
	// Syntactically broken ranges must also be enveloped, not plain text.
	req2, _ := http.NewRequest(http.MethodGet, h.base+"/v1/blob/"+d, nil)
	req2.Header.Set("Range", "bytes=zzz")
	resp2, err := h.client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("bad syntax: status %d", resp2.StatusCode)
	}
	if err := json.NewDecoder(resp2.Body).Decode(&e); err != nil {
		t.Fatalf("416 (bad syntax) body is not JSON: %v", err)
	}
	// Success responses must remain raw bytes (no envelope wrapping).
	code, body := h.get("/v1/blob/" + d)
	if code != http.StatusOK || string(body) != "0123456789" {
		t.Fatalf("full body broken: %d %q", code, body)
	}
}

// --- Fix 4: job map — terminal before publish, race-free ---

func TestJobsConcurrentEnsureAndPoll(t *testing.T) {
	h := newHarness(t)
	seedRepo(t, h, "ns/models", "manifest-bytes")

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				_, body := h.postJSON("/v1/ensure", map[string]string{"ref": "shardr:///ns/models:q8_0"})
				var job Job
				if err := json.Unmarshal(body, &job); err != nil || job.ID == "" {
					continue
				}
				// Poll the job while other ensures are still publishing and
				// mutating — under the race detector this catches a Job
				// mutated after publication.
				for p := 0; p < 5; p++ {
					if code, _ := h.get("/v1/jobs/" + job.ID); code != http.StatusOK {
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// --- Fix 5: index member validation (001) ---

func TestIndexMemberValidation(t *testing.T) {
	validManifest := "sha256:" + testDigestOf([]byte("m"))
	bad := []struct {
		name  string
		index string
	}{
		{"wrong schemaVersion", `{"artifactType":"model-index","schemaVersion":2,"members":[{"quant":"q8_0","manifest":"` + validManifest + `"}]}`},
		{"missing schemaVersion", `{"artifactType":"model-index","members":[{"quant":"q8_0","manifest":"` + validManifest + `"}]}`},
		{"empty members", `{"artifactType":"model-index","schemaVersion":1,"members":[]}`},
		{"quant syntax", `{"artifactType":"model-index","schemaVersion":1,"members":[{"quant":"bogus-tag","manifest":"` + validManifest + `"}]}`},
		{"bare hex manifest", `{"artifactType":"model-index","schemaVersion":1,"members":[{"quant":"q8_0","manifest":"` + strings.TrimPrefix(validManifest, "sha256:") + `"}]}`},
		{"short manifest", `{"artifactType":"model-index","schemaVersion":1,"members":[{"quant":"q8_0","manifest":"sha256:abcd"}]}`},
		{"duplicate quant", `{"artifactType":"model-index","schemaVersion":1,"members":[{"quant":"q8_0","manifest":"` + validManifest + `"},{"quant":"q8_0","manifest":"` + validManifest + `"}]}`},
	}
	for _, tc := range bad {
		h := newHarness(t)
		d := h.putHashed([]byte(tc.index))
		if err := h.store.SetNamespace("ns/models", d); err != nil {
			t.Fatal(err)
		}
		code, body := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///ns/models:q8_0"))
		if code != http.StatusInternalServerError {
			t.Fatalf("%s: status %d body %s", tc.name, code, body)
		}
		var e struct {
			Error APIError `json:"error"`
		}
		json.Unmarshal(body, &e)
		if e.Error.Code != ErrInvalidIndex {
			t.Fatalf("%s: code %s want E_INVALID_INDEX", tc.name, e.Error.Code)
		}
	}
	// Sanity: the valid shape still resolves.
	h := newHarness(t)
	d := h.putHashed([]byte(`{"artifactType":"model-index","schemaVersion":1,"members":[{"quant":"q8_0","manifest":"` + validManifest + `"}]}`))
	h.store.SetNamespace("ns/models", d)
	if code, _ := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///ns/models:q8_0")); code != http.StatusOK {
		t.Fatalf("valid index rejected")
	}
}

// --- Fix 6 (R3): tags scoped per repository ---

func TestTagsScopedPerRepo(t *testing.T) {
	h := newHarness(t)
	idxA := seedRepo(t, h, "a/models", "repo-a-manifest")
	idxB := seedRepo(t, h, "b/models", "repo-b-manifest")
	if idxA == idxB {
		t.Fatal("fixtures must differ")
	}
	// Same tag name in two repos: independent aliases (000 §3.4, R3).
	if err := h.store.SetTagAlias("a/models", "snap", idxA); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTagAlias("b/models", "snap", idxB); err != nil {
		t.Fatal(err)
	}

	manA := "sha256:" + testDigestOf([]byte("repo-a-manifest"))
	manB := "sha256:" + testDigestOf([]byte("repo-b-manifest"))
	code, body := h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///a/models:snap+q8_0"))
	if code != http.StatusOK {
		t.Fatalf("a/models: %d %s", code, body)
	}
	var res resolveResult
	json.Unmarshal(body, &res)
	if res.ManifestDigest != manA {
		t.Fatalf("a/models tag leaked: %s want %s", res.ManifestDigest, manA)
	}
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///b/models:snap+q8_0"))
	if code != http.StatusOK {
		t.Fatalf("b/models: %d %s", code, body)
	}
	json.Unmarshal(body, &res)
	if res.ManifestDigest != manB {
		t.Fatalf("b/models tag leaked: %s want %s", res.ManifestDigest, manB)
	}

	// A repo without that tag fails loudly as unknown-ref (not silent, not
	// cross-repo leakage), with the repo's real tags as candidates.
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///c/models:snap+q8_0"))
	if code != http.StatusNotFound {
		t.Fatalf("foreign repo: %d %s", code, body)
	}
	var e struct {
		Error APIError `json:"error"`
	}
	json.Unmarshal(body, &e)
	if e.Error.Code != ErrUnknownRef {
		t.Fatalf("foreign repo code %s want E_UNKNOWN_REF", e.Error.Code)
	}
	// Candidates list this repo's tags (a/models has snap).
	code, body = h.get("/v1/resolve?ref=" + url.QueryEscape("shardr:///a/models:miss+q8_0"))
	json.Unmarshal(body, &e)
	if len(e.Error.Candidates) != 1 || e.Error.Candidates[0] != "snap" {
		t.Fatalf("candidates %v want [snap]", e.Error.Candidates)
	}
}

// --- Straggler a: /models is a marked skeleton with quants when readable ---

func TestModelsSkeletonWithQuants(t *testing.T) {
	h := newHarness(t)
	seedRepo(t, h, "ns/models", "bytes") // index with q8_0/q4_0/q4_1
	if err := h.store.SetNamespace("ns/ghost", testDigestOf([]byte("ghost"))); err != nil {
		t.Fatal(err)
	}
	_, body := h.get("/v1/models")
	var inv struct {
		Skeleton   bool   `json:"skeleton"`
		Note       string `json:"note"`
		Namespaces []struct {
			Name         string   `json:"name"`
			IndexPresent bool     `json:"indexPresent"`
			Quants       []string `json:"quants"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatal(err)
	}
	if !inv.Skeleton || inv.Note == "" {
		t.Fatalf("skeleton not marked: %s", body)
	}
	byName := map[string][]string{}
	for _, n := range inv.Namespaces {
		byName[n.Name] = n.Quants
	}
	if got := byName["models"]; len(got) != 3 || got[0] != "q4_0" || got[1] != "q4_1" || got[2] != "q8_0" {
		t.Fatalf("quants for models: %v", got)
	}
	if got := byName["ghost"]; got != nil {
		t.Fatalf("ghost must have no quants: %v", got)
	}
}
