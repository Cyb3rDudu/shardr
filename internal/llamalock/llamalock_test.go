package llamalock

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validLock() string {
	return `ref = "b10684"
commit = "cc83d7b4824f73cfdda4dfbb47ee39804f71b328"
updated_at = "2026-09-05T15:58:13Z"

[assets.darwin_arm64]
url = "https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-macos-arm64.tar.gz"
sha256 = "8310138372444cbeb5c1123c88d27aa9bc78dd14a549c5654733eb339f665c07"

[assets.linux_amd64]
url = "https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-ubuntu-x64.tar.gz"
sha256 = "2eb35b197e220511456dfb011c118f74707735457c4d09927ae0382c6b29e7ee"
`
}

func TestParseValid(t *testing.T) {
	lk, err := Parse([]byte(validLock()))
	if err != nil {
		t.Fatal(err)
	}
	if lk.Ref != "b10684" || len(lk.Assets) != 2 {
		t.Fatalf("parsed %+v", lk)
	}
	// Format round-trip is byte-identical (canonical output).
	if got := string(lk.Format()); got != validLock() {
		t.Fatalf("round-trip mismatch:\n%s", got)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	darwinURL := "https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-macos-arm64.tar.gz"
	cases := map[string]func(string) string{
		"vX.Y.Z ref (stable carries no binaries)": func(s string) string { return strings.Replace(s, `ref = "b10684"`, `ref = "v0.4.0"`, 1) },
		"short commit": func(s string) string {
			return strings.Replace(s, "cc83d7b4824f73cfdda4dfbb47ee39804f71b328", "cc83d7b4", 1)
		},
		"uppercase commit": func(s string) string {
			return strings.Replace(s, "cc83d7b4824f73cfdda4dfbb47ee39804f71b328", strings.ToUpper("cc83d7b4824f73cfdda4dfbb47ee39804f71b328"), 1)
		},
		"wrong digest": func(s string) string {
			return strings.Replace(s, "8310138372444cbeb5c1123c88d27aa9bc78dd14a549c5654733eb339f665c07", "deadbeef", 1)
		},
		"moving ref":       func(s string) string { return strings.Replace(s, `ref = "b10684"`, `ref = "master"`, 1) },
		"missing field":    func(s string) string { return strings.Replace(s, `updated_at = "2026-09-05T15:58:13Z"`, ``, 1) },
		"missing platform": func(s string) string { return s[:strings.Index(s, "[assets.linux_amd64]")] },
		"unknown platform": func(s string) string { return strings.Replace(s, "[assets.linux_amd64]", "[assets.windows_x64]", 1) },
		"duplicate key":    func(s string) string { return s + `ref = "b1"` + "\n" },
		"unknown asset key": func(s string) string {
			return strings.Replace(s, `[assets.linux_amd64]`, "[assets.linux_amd64]", 1) + "extra = \"x\"\n"
		},
		"http url": func(s string) string {
			return strings.Replace(s, "https://github.com/ggml-org", "http://github.com/ggml-org", 1)
		},
		"foreign org": func(s string) string { return strings.Replace(s, "ggml-org/llama.cpp", "evil/llama.cpp", 1) },
		"url not canonical": func(s string) string {
			return strings.Replace(s, darwinURL, "https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-macos-x64.tar.gz", 1)
		},
		"asset url other ref": func(s string) string {
			return strings.Replace(s, darwinURL, "https://github.com/ggml-org/llama.cpp/releases/download/b10685/llama-b10684-bin-macos-arm64.tar.gz", 1)
		},
		"bad timestamp": func(s string) string {
			return strings.Replace(s, `updated_at = "2026-09-05T15:58:13Z"`, `updated_at = "yesterday"`, 1)
		},
		"garbage line": func(s string) string { return s + "totally not a lock line\n" },
		"empty":        func(string) string { return "" },
	}
	for name, mutate := range cases {
		if _, err := Parse([]byte(mutate(validLock()))); err == nil {
			t.Errorf("%s: parser accepted an invalid lockfile", name)
		}
	}
}

func TestDecidePinChannel(t *testing.T) {
	if up, err := Decide("b10684", "b10684", false); err != nil || up {
		t.Errorf("unchanged upstream must be a noop, got up=%v err=%v", up, err)
	}
	if up, err := Decide("b10684", "b10700", false); err != nil || !up {
		t.Errorf("newer b-release must update, got up=%v err=%v", up, err)
	}
	// NUMERIC comparison: b99 > b100 lexicographically but NOT numerically.
	if up, err := Decide("b100", "b99", false); err != nil || up {
		t.Errorf("lexicographic trap: b99 is older than b100, must be noop, got up=%v err=%v", up, err)
	}
	// downgrade needs an explicit opt-in
	if up, err := Decide("b10684", "b10680", false); err != nil || up {
		t.Errorf("older release without --allow-downgrade must be noop, got up=%v err=%v", up, err)
	}
	if up, err := Decide("b10684", "b10680", true); err != nil || !up {
		t.Errorf("older release with --allow-downgrade must update, got up=%v err=%v", up, err)
	}
	// vX.Y.Z may never become the pin: stable releases attach no binaries.
	if _, err := Decide("b10684", "v0.4.0", false); err == nil {
		t.Error("vX.Y.Z must be rejected for the pin")
	}
	if _, err := Decide("b10684", "master", false); err == nil {
		t.Error("moving ref must be rejected")
	}
}

func TestRefClassification(t *testing.T) {
	if !IsNightly("b10684") || IsNightly("b9999x") || IsNightly("v0.4.0") {
		t.Error("nightly classification broken")
	}
	if !IsStable("v0.4.0") || IsStable("b10684") {
		t.Error("stable classification broken")
	}
	if MinAge != 7*24*60*60*1e9 { // 7 days, nanoseconds
		t.Errorf("soak window drifted: %v", MinAge)
	}
}

// TestDownloadAssetDigestRefusal: a tampered asset (server bytes do not
// match the pinned digest) must be refused — no file on disk, hard error.
func TestDownloadAssetDigestRefusal(t *testing.T) {
	payload := []byte("tampered asset bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()
	url := srv.URL + "/asset.tar.gz"
	// snapshot the temp dir: a refused download must leave NO new file
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "llama-asset-*"))
	path, err := DownloadAsset(context.Background(), url, strings.Repeat("0", 64), "darwin_arm64")
	if err == nil {
		t.Fatal("digest mismatch must fail")
	}
	if !strings.Contains(err.Error(), "E_DIGEST") {
		t.Fatalf("expected E_DIGEST failure, got %v", err)
	}
	if path != "" {
		t.Errorf("refused asset must not return a path, got %q", path)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "llama-asset-*"))
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("refused download leaked a temp asset: before=%v after=%v", before, after)
	}
	// positive control: matching digest passes, temp path is unpredictable
	h := sha256.Sum256(payload)
	good := hex.EncodeToString(h[:])
	p1, err := DownloadAsset(context.Background(), url, good, "darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p1)
	p2, err := DownloadAsset(context.Background(), url, good, "darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p2)
	if filepath.Base(p1) == "llama-asset-darwin_arm64.tar.gz" || filepath.Dir(p1) != filepath.Dir(p2) || p1 == p2 {
		t.Errorf("temp paths must be unpredictable per download: %q vs %q", p1, p2)
	}
}

// TestSingleTruthLockfile: no second version pin may creep back into the
// Makefile or Go sources — runtime/llama.lock is the only truth, and the
// committed lockfile itself must parse.
func TestSingleTruthLockfile(t *testing.T) {
	for _, src := range []string{"../../Makefile", "../../internal/runner/llama.go"} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"LLAMA_VERSION := v", "LLAMA_VERSION ?= v", "LLAMA_VERSION = v", "LLAMA_VERSION := b", "LLAMA_VERSION ?= b", "LLAMA_VERSION = b", `LlamaPin = "v`, `LlamaPin = "b`} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains a second version pin (%q)", src, bad)
			}
		}
		if strings.Contains(string(data), "cmake ") || strings.Contains(string(data), "LLAMA_BUILD_") {
			t.Errorf("%s still self-builds llama.cpp (project decision: prebuilt only)", src)
		}
	}
	root, err := FindRepoRoot()
	if err != nil {
		t.Skipf("not run from repo: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime/llama.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("committed lockfile does not parse: %v", err)
	}
}

// TestExtractAssetTraversalSafe: archives with absolute paths, "..",
// or escaping symlinks must be rejected before anything lands outside
// the extract dir.
func TestExtractAssetTraversalSafe(t *testing.T) {
	build := func(entries map[string]string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for name, link := range entries {
			if link != "" {
				hdr := &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: link, Mode: 0o777}
				tw.WriteHeader(hdr)
			} else {
				hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len("x"))}
				tw.WriteHeader(hdr)
				tw.Write([]byte("x"))
			}
		}
		tw.Close()
		gz.Close()
		path := filepath.Join(t.TempDir(), "a.tar.gz")
		os.WriteFile(path, buf.Bytes(), 0o644)
		return path
	}
	dest := t.TempDir()
	for name, entries := range map[string]map[string]string{
		"absolute path": {"llama-x/../../evil": ""},
		"parent escape": {"llama-x/ok": "", "../evil": ""},
		"abs symlink":   {"llama-x/link": "/etc/passwd"},
		"two top dirs":  {"a/f": "", "b/f": ""},
	} {
		if _, err := ExtractAsset(build(entries), dest); err == nil {
			t.Errorf("%s: extract accepted unsafe archive", name)
		}
	}
	// sane archive extracts and reports its root
	root, err := ExtractAsset(build(map[string]string{"llama-b1/llama-server": "", "llama-b1/LICENSE": ""}), dest)
	if err != nil || root != "llama-b1" {
		t.Fatalf("sane archive failed: root=%q err=%v", root, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "llama-b1", "llama-server")); err != nil {
		t.Fatal(err)
	}
}

// ---- B1.3 mandatory extraction test vectors (mutation-guarded) ----

// buildTar builds a gzipped tar from entry specs ("n:file" file, "n->t"
// symlink to t, "n=>t" hardlink to t).
func buildTar(t *testing.T, entries [3]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e == "" {
			continue
		}
		var hdr *tar.Header
		switch {
		case strings.Contains(e, "->"):
			p := strings.SplitN(e, "->", 2)
			hdr = &tar.Header{Name: p[0], Typeflag: tar.TypeSymlink, Linkname: p[1], Mode: 0o777}
		case strings.Contains(e, "=>"):
			p := strings.SplitN(e, "=>", 2)
			hdr = &tar.Header{Name: p[0], Typeflag: tar.TypeLink, Linkname: p[1], Mode: 0o644}
		default:
			hdr = &tar.Header{Name: e, Typeflag: tar.TypeReg, Mode: 0o755, Size: 1}
			tw.WriteHeader(hdr)
			tw.Write([]byte("x"))
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	os.WriteFile(path, buf.Bytes(), 0o644)
	return path
}

func TestExtractAssetMandatoryVectors(t *testing.T) {
	mustFail := func(name string, entries [3]string) {
		t.Helper()
		dest := t.TempDir()
		if _, err := ExtractAsset(buildTar(t, entries), dest); err == nil {
			t.Errorf("%s: extractor accepted unsafe archive", name)
		}
		// nothing may leak into the destination besides temp cleanup
		files, _ := os.ReadDir(dest)
		for _, f := range files {
			if strings.HasPrefix(f.Name(), ".extract-") {
				t.Errorf("%s: temp dir leaked: %s", name, f.Name())
			}
		}
	}
	// relative symlink escape
	mustFail("relative symlink escape", [3]string{"llama-x/ok", "llama-x/link->../../victim", ""})
	// regular file whose path RUNS THROUGH a symlink parent (link -> ..)
	mustFail("file under symlink parent", [3]string{"llama-x/link->..", "llama-x/link/file", ""})
	// valid interior symlink, but a later FILE entry runs through it —
	// the kernel (Openat O_NOFOLLOW) must refuse the write, not a path check
	mustFail("file written through interior symlink", [3]string{"llama-x/d/keep", "llama-x/s->d", "llama-x/s/f"})
	// hardlink escape
	mustFail("hardlink escape", [3]string{"llama-x/ok", "llama-x/hard=>../../victim", ""})
	// absolute link target
	mustFail("absolute symlink", [3]string{"llama-x/link->/etc/passwd", "", ""})
	// multi top-level stays red
	mustFail("multi top-level", [3]string{"a/f", "b/f", ""})

	// prepared symlink in the TARGET dir must not be followed: extraction
	// lands atomically via rename, so a pre-existing target with content
	// is replaced wholesale — prove no write crosses the swap.
	dest := t.TempDir()
	os.MkdirAll(filepath.Join(dest, "llama-x"), 0o755)
	os.Symlink("../../outside", filepath.Join(dest, "llama-x", "link"))
	if _, err := ExtractAsset(buildTar(t, [3]string{"llama-x/f", "", ""}), dest); err != nil {
		t.Fatalf("replace of prepared target failed: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(dest, "llama-x", "link")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("pre-existing symlink survived a full-dir replacement — extraction not atomic")
	}

	// sane archive: extracts, reports root, links stay relative
	root, err := ExtractAsset(buildTar(t, [3]string{"llama-b1/llama-server", "llama-b1/lib.dylib->lib.1.dylib", ""}), t.TempDir())
	if err != nil || root != "llama-b1" {
		t.Fatalf("sane archive failed: root=%q err=%v", root, err)
	}
}

// ---- B4 parser duplicate negatives ----

func TestParseRejectsDuplicates(t *testing.T) {
	base := validLock()
	dupSha := strings.Replace(base, `[assets.linux_amd64]`, "[assets.linux_amd64]\nsha256 = \""+strings.Repeat("a", 64)+"\"", 1)
	dupURL := strings.Replace(base, `[assets.linux_amd64]`, "[assets.linux_amd64]\nurl = \"https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-ubuntu-x64.tar.gz\"", 1)
	dupSection := base + "\n[assets.darwin_arm64]\nurl = \"https://github.com/ggml-org/llama.cpp/releases/download/b10684/llama-b10684-bin-macos-arm64.tar.gz\"\nsha256 = \"" + strings.Repeat("8", 64) + "\"\n"
	for name, doc := range map[string]string{
		"duplicate sha256":    dupSha,
		"duplicate url":       dupURL,
		"duplicate section":   dupSection,
		"duplicate top-level": strings.Replace(base, `updated_at = "2026-09-05T15:58:13Z"`, `updated_at = "2026-09-05T15:58:13Z"`+"\n"+`updated_at = "2026-09-05T16:00:00Z"`, 1),
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: parser accepted duplicate", name)
		}
	}
}

// ---- deterministic pagination / age-filter / asset-matrix tests ----

func relJSON(t time.Time, tag string, assets map[string][2]string) string {
	type asset struct {
		Name      string    `json:"name"`
		Digest    string    `json:"digest"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	rel := struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []asset   `json:"assets"`
	}{tag, t, nil}
	for p, a := range assets {
		rel.Assets = append(rel.Assets, asset{Name: fmt.Sprintf(AssetNames[p], tag), Digest: "sha256:" + a[1], UpdatedAt: parseTime(a[0])})
	}
	one, _ := json.Marshal(rel)
	return "[" + string(one) + "]"
}

func parseTime(s string) time.Time {
	tm, _ := time.Parse(time.RFC3339, s)
	return tm
}

func TestNewestPinnableMatrix(t *testing.T) {
	now := parseTime("2026-09-05T12:00:00Z")
	old8d := "2026-08-28T00:00:00Z"
	freshUpload := "2026-09-04T00:00:00Z" // old release, FRESH asset upload
	mkDigest := func() string { return strings.Repeat("d", 64) }

	pages := map[int]string{}
	serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		if body, ok := pages[page]; ok {
			w.Write([]byte(body))
			return
		}
		w.Write([]byte("[]"))
	}))
	defer serve.Close()
	url := func(p int) string { return fmt.Sprintf("%s/releases?per_page=100&page=%d", serve.URL, p) }

	full := func(updated string) map[string][2]string {
		return map[string][2]string{"darwin_arm64": {updated, mkDigest()}, "linux_amd64": {updated, mkDigest()}}
	}

	cases := []struct {
		name  string
		pages map[int]string
		want  string // "" = error expected
	}{
		{"newest is fresh, page-2 release aged", map[int]string{
			1: relJSON(now.Add(-1*24*time.Hour), "b10900", full("2026-09-04T00:00:00Z")),
			2: relJSON(parseTime(old8d), "b10684", full(old8d)),
		}, "b10684"},
		{"pagination: fresh on pages 1-2, eligible on page 3", map[int]string{
			1: relJSON(now.Add(-24*time.Hour), "b10900", full("2026-09-04T00:00:00Z")),
			2: relJSON(now.Add(-2*24*time.Hour), "b10800", full("2026-09-03T00:00:00Z")),
			3: relJSON(parseTime(old8d), "b10600", full(old8d)),
		}, "b10600"},
		{"incomplete asset matrix skipped", map[int]string{
			1: relJSON(parseTime(old8d), "b10690", map[string][2]string{"darwin_arm64": {old8d, mkDigest()}}),
			2: relJSON(parseTime(old8d), "b10685", full(old8d)),
		}, "b10685"},
		{"old release with FRESHLY re-uploaded asset is NOT aged", map[int]string{
			1: relJSON(parseTime("2026-01-01T00:00:00Z"), "b10000", map[string][2]string{
				"darwin_arm64": {old8d, mkDigest()},
				"linux_amd64":  {freshUpload, mkDigest()}, // re-upload yesterday
			}),
			2: relJSON(parseTime(old8d), "b10684", full(old8d)),
		}, "b10684"},
		{"nothing eligible anywhere", map[int]string{
			1: relJSON(parseTime(old8d), "b10684", map[string][2]string{"darwin_arm64": {old8d, mkDigest()}}),
		}, ""},
		{"vX.Y.Z releases ignored even when aged", map[int]string{
			1: relJSON(parseTime("2026-01-01T00:00:00Z"), "v0.4.0", full("2026-01-01T00:00:00Z")),
			2: relJSON(parseTime(old8d), "b10684", full(old8d)),
		}, "b10684"},
	}
	for _, tc := range cases {
		pages = tc.pages
		got, err := newestPinnable(context.Background(), now, url)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: expected error, got %q", tc.name, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q err=%v, want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestExtractHardlinkRootEscapePoC: tar hardlink targets are
// ARCHIVE-ROOT-relative. "d/hard => ../victim" must be refused — the
// old symlink-style check accepted it (d/../victim "inside root") while
// Openat(rootFd, "../victim") actually reached OUTSIDE the temp root.
func TestExtractHardlinkRootEscapePoC(t *testing.T) {
	dest := t.TempDir()
	// victim OUTSIDE the extract temp root, inside dest
	os.WriteFile(filepath.Join(dest, "victim"), []byte("secret"), 0o644)
	if _, err := ExtractAsset(buildTar(t, [3]string{"llama-x/victim", "llama-x/d/hard=>../victim", ""}), dest); err == nil {
		t.Fatal("hardlink root-escape PoC accepted")
	}
	// the outside victim must be untouched (never opened as link source)
	b, err := os.ReadFile(filepath.Join(dest, "victim"))
	if err != nil || string(b) != "secret" {
		t.Fatalf("outside victim disturbed: %q %v", b, err)
	}
	// no landed tree, no leaked temp dirs
	files, _ := os.ReadDir(dest)
	for _, f := range files {
		if f.Name() == "llama-x" || strings.HasPrefix(f.Name(), ".extract-") || strings.HasPrefix(f.Name(), ".extract") {
			t.Errorf("leaked artifact: %s", f.Name())
		}
	}
}

// TestExtractHardlinkTarConvention: GNU tar writes hardlink targets
// ARCHIVE-ROOT-relative INCLUDING the top-level prefix ("llama-b1/bin/
// llama-server") while entries are extracted prefix-stripped — the
// extractor must strip the prefix and link to the already-extracted file.
func TestExtractHardlinkTarConvention(t *testing.T) {
	dest := t.TempDir()
	root, err := ExtractAsset(buildTar(t, [3]string{
		"llama-b1/bin/llama-server",
		"llama-b1/bin/llama-cli=>llama-b1/bin/llama-server",
		"",
	}), dest)
	if err != nil {
		t.Fatalf("tar-convention hardlink must extract: %v", err)
	}
	if root != "llama-b1" {
		t.Fatalf("root = %q, want llama-b1", root)
	}
	fi1, err1 := os.Stat(filepath.Join(dest, "llama-b1", "bin", "llama-server"))
	fi2, err2 := os.Stat(filepath.Join(dest, "llama-b1", "bin", "llama-cli"))
	if err1 != nil || err2 != nil {
		t.Fatalf("hardlink/stat failed: %v %v", err1, err2)
	}
	if !os.SameFile(fi1, fi2) {
		t.Error("llama-cli must be a hardlink (same inode) as llama-server")
	}
	// and the same convention outside the prefix stays refused
	for bad, entries := range map[string][3]string{
		"no prefix":       {"llama-b1/bin/ok", "llama-b1/bin/h=>bin/ok", ""},
		"parent escape":   {"llama-b1/victim", "llama-b1/d/hard=>../victim", ""},
		"cross top-level": {"b1/f", "b1/h=>b2/f", ""},
	} {
		if _, err := ExtractAsset(buildTar(t, entries), t.TempDir()); err == nil {
			t.Errorf("%s: extractor accepted unsafe hardlink target", bad)
		}
	}
}

// TestValidatePinnableRelFreshAsset: an OLD release timestamp with a
// FRESHLY re-uploaded asset must NOT be pinnable — this is exactly the
// hole the manual --ref path used to leave open.
func TestValidatePinnableRelFreshAsset(t *testing.T) {
	now := parseTime("2026-09-05T12:00:00Z")
	d := strings.Repeat("d", 64)
	mk := func(darwinAge, linuxAge string) Release {
		return Release{TagName: "b10000", PublishedAt: parseTime("2026-01-01T00:00:00Z"), Assets: []struct {
			Name      string    `json:"name"`
			Digest    string    `json:"digest"`
			UpdatedAt time.Time `json:"updated_at"`
		}{
			{Name: "llama-b10000-bin-macos-arm64.tar.gz", Digest: "sha256:" + d, UpdatedAt: parseTime(darwinAge)},
			{Name: "llama-b10000-bin-ubuntu-x64.tar.gz", Digest: "sha256:" + d, UpdatedAt: parseTime(linuxAge)},
		}}
	}
	if err := validatePinnableRel(mk("2026-08-20T00:00:00Z", "2026-08-25T00:00:00Z"), now); err != nil {
		t.Errorf("fully aged release must be pinnable: %v", err)
	}
	// old release published_at, one asset uploaded MINUTES ago → refuse
	if err := validatePinnableRel(mk("2026-08-20T00:00:00Z", "2026-09-05T11:55:00Z"), now); err == nil {
		t.Error("freshly re-uploaded asset must NOT be pinnable (per-asset soak)")
	}
	// missing platform asset → refuse
	r := mk("2026-08-20T00:00:00Z", "2026-08-25T00:00:00Z")
	r.Assets = r.Assets[:1]
	if err := validatePinnableRel(r, now); err == nil {
		t.Error("incomplete asset matrix must not be pinnable")
	}
}

// TestGitignoreCoversBin: build artifacts from `make llama` must never
// show up as committable (hygiene tripwire for the 27.5 MB accident).
func TestGitignoreCoversBin(t *testing.T) {
	root, err := FindRepoRoot()
	if err != nil {
		t.Skipf("not run from repo: %v", err)
	}
	gi, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{"bin/", ".llama-bin/"} {
		if !strings.Contains(string(gi), need) {
			t.Errorf(".gitignore must cover %q (make llama artifacts must stay untracked)", need)
		}
	}
}
