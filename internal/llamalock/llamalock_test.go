package llamalock

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if up, err := Decide("b10684", "b10684"); err != nil || up {
		t.Errorf("unchanged upstream must be a noop, got up=%v err=%v", up, err)
	}
	if up, err := Decide("b10684", "b10700"); err != nil || !up {
		t.Errorf("newer b-release must update, got up=%v err=%v", up, err)
	}
	// vX.Y.Z may never become the pin: stable releases attach no binaries.
	if _, err := Decide("b10684", "v0.4.0"); err == nil {
		t.Error("vX.Y.Z must be rejected for the pin")
	}
	if _, err := Decide("b10684", "master"); err == nil {
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
	lk := Lock{Assets: map[string]Asset{
		"darwin_arm64": {Platform: "darwin_arm64", URL: srv.URL + "/asset.tar.gz", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
	}}
	dest := filepath.Join(t.TempDir(), "asset.tar.gz")
	err := DownloadAsset(context.Background(), lk, "darwin_arm64", dest)
	if err == nil {
		t.Fatal("digest mismatch must fail")
	}
	if !strings.Contains(err.Error(), "E_DIGEST") {
		t.Fatalf("expected E_DIGEST failure, got %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("refused asset must not remain on disk")
	}
	// positive control: matching digest passes
	h := sha256.Sum256(payload)
	a := lk.Assets["darwin_arm64"]
	a.SHA256 = hex.EncodeToString(h[:])
	lk.Assets["darwin_arm64"] = a
	if err := DownloadAsset(context.Background(), lk, "darwin_arm64", dest); err != nil {
		t.Fatal(err)
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
		for _, bad := range []string{"LLAMA_VERSION := v", "LLAMA_VERSION ?= v", "LLAMA_VERSION := b", `LlamaPin = "v`, `LlamaPin = "b`} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains a second version pin (%q)", src, bad)
			}
		}
		if strings.Contains(string(data), "cmake ") || strings.Contains(string(data), "LLAMA_BUILD_") {
			t.Errorf("%s still self-builds llama.cpp (owner ruling: prebuilt only)", src)
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
