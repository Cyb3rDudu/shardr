package llamalock

import (
	"os"
	"strings"
	"testing"
)

func validLock() string {
	return `ref = "v0.3.0"
commit = "c1d0e7a004015f23bc0233470b747b596f29b264"
source_url = "https://github.com/ggml-org/llama.cpp/archive/c1d0e7a004015f23bc0233470b747b596f29b264.tar.gz"
source_sha256 = "e381b23a9aba7e1615ef8d4713bc2f8d4777255a5b1124d633f0af280a2d5415"
updated_at = "2026-09-04T18:30:00Z"
`
}

func TestParseValid(t *testing.T) {
	lk, err := Parse([]byte(validLock()))
	if err != nil {
		t.Fatal(err)
	}
	if lk.Ref != "v0.3.0" || lk.Commit != "c1d0e7a004015f23bc0233470b747b596f29b264" {
		t.Fatalf("parsed %+v", lk)
	}
	// Format round-trip is byte-identical (canonical output).
	if got := string(lk.Format()); got != validLock() {
		t.Fatalf("round-trip mismatch:\n%s", got)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	cases := map[string]func(string) string{
		"short sha": func(s string) string {
			return strings.Replace(s, "c1d0e7a004015f23bc0233470b747b596f29b264", "c1d0e7a", 1)
		},
		"uppercase sha": func(s string) string {
			return strings.Replace(s, "c1d0e7a004015f23bc0233470b747b596f29b264", strings.ToUpper("c1d0e7a004015f23bc0233470b747b596f29b264"), 1)
		},
		"wrong digest": func(s string) string {
			return strings.Replace(s, "e381b23a9aba7e1615ef8d4713bc2f8d4777255a5b1124d633f0af280a2d5415", "deadbeef", 1)
		},
		"nightly ref":    func(s string) string { return strings.Replace(s, `ref = "v0.3.0"`, `ref = "b9999"`, 1) },
		"ref not semver": func(s string) string { return strings.Replace(s, `ref = "v0.3.0"`, `ref = "latest"`, 1) },
		"missing field":  func(s string) string { return strings.Replace(s, `updated_at = "2026-09-04T18:30:00Z"`, ``, 1) },
		"duplicate key":  func(s string) string { return s + `ref = "v0.4.0"` + "\n" },
		"unknown key":    func(s string) string { return s + `extra = "x"` + "\n" },
		"http url": func(s string) string {
			return strings.Replace(s, "https://github.com/ggml-org", "http://github.com/ggml-org", 1)
		},
		"foreign org":    func(s string) string { return strings.Replace(s, "ggml-org/llama.cpp", "evil/llama.cpp", 1) },
		"url not commit": func(s string) string { return strings.Replace(s, "/archive/c1d0e7a0", "/archive/ffffe7a0", 1) },
		"bad timestamp": func(s string) string {
			return strings.Replace(s, `updated_at = "2026-09-04T18:30:00Z"`, `updated_at = "yesterday"`, 1)
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

func TestDecideStableChannel(t *testing.T) {
	if up, err := Decide("v0.3.0", "v0.3.0"); err != nil || up {
		t.Errorf("unchanged upstream must be a noop, got up=%v err=%v", up, err)
	}
	if up, err := Decide("v0.3.0", "v0.4.0"); err != nil || !up {
		t.Errorf("newer stable must update, got up=%v err=%v", up, err)
	}
	// bNNNN may never update the stable lock (canary channel is separate).
	if _, err := Decide("v0.3.0", "b9999"); err == nil {
		t.Error("bNNNN must be rejected for the stable lock")
	}
	if _, err := Decide("v0.3.0", "main"); err == nil {
		t.Error("moving ref must be rejected")
	}
}

func TestTagClassificationAndOrder(t *testing.T) {
	for _, ok := range []string{"v0.0.1", "v10.20.30"} {
		if !IsStable(ok) {
			t.Errorf("%s should be stable", ok)
		}
	}
	for _, no := range []string{"v0.3", "v0.3.0-rc1", "b4711", "latest", "V0.3.0"} {
		if IsStable(no) {
			t.Errorf("%s should not be stable", no)
		}
	}
	if !tagLess("v0.9.0", "v0.10.0") {
		t.Error("numeric comparison broken: v0.10.0 > v0.9.0")
	}
	if !IsNightly("b9999") || IsNightly("b9999x") {
		t.Error("nightly classification broken")
	}
}

// TestSingleTruthLockfile: no second version pin may creep back into the
// Makefile or Go sources — runtime/llama.lock is the only truth.
func TestSingleTruthLockfile(t *testing.T) {
	for _, src := range []string{"../../Makefile", "../../internal/runner/llama.go"} {
		data := readFileT(t, src)
		for _, bad := range []string{"LLAMA_VERSION := v", "LLAMA_VERSION ?= v", `LlamaPin = "v`} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s contains a second version pin (%q)", src, bad)
			}
		}
	}
	// The committed lockfile itself must parse (fail-closed check on the
	// real bytes, not just their existence).
	root, err := FindRepoRoot()
	if err != nil {
		t.Skipf("not run from repo: %v", err)
	}
	lockBytes, err := os.ReadFile(root + "/runtime/llama.lock")
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	if _, err := Parse(lockBytes); err != nil {
		t.Fatalf("committed runtime/llama.lock is invalid: %v", err)
	}
}

func readFileT(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("cannot read %s: %v", p, err)
	}
	return data
}
