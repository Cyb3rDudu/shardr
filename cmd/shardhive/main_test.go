package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// withTestCAS points SHARDR_CAS at a temp root, returns it and a writer
// helper.
func withTestCAS(t *testing.T) *cas.Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv("SHARDR_CAS", root)
	s, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *cas.Store, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRunVerifyExitCodes(t *testing.T) {
	s := withTestCAS(t)
	clean := put(t, s, []byte("clean blob"))

	tests := []struct {
		name string
		arg  string
		want int
	}{
		{"clean digest", clean, 0},
		{"unknown digest", bytesToDigest([]byte("never written")), 2},
		{"all clean", "--all", 0},
	}
	for _, tt := range tests {
		if got := runVerify(tt.arg); got != tt.want {
			t.Errorf("%s: exit %d want %d", tt.name, got, tt.want)
		}
	}
}

func TestRunVerifyMutatedBlobExit1(t *testing.T) {
	s := withTestCAS(t)
	d := put(t, s, []byte("will be tampered"))
	p, err := s.BlobPath(d)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("tampered content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runVerify(d); got != 1 {
		t.Errorf("mutated blob: exit %d want 1", got)
	}
	if got := runVerify("--all"); got != 1 {
		t.Errorf("--all with mutation: exit %d want 1", got)
	}
}

func TestRunVerifyDeletedBlobExit2(t *testing.T) {
	s := withTestCAS(t)
	d := put(t, s, []byte("will be deleted"))
	// Reference the digest in state so --all can detect the deletion.
	if err := s.SetNamespace("ns/current-index", d); err != nil {
		t.Fatal(err)
	}
	p, _ := s.BlobPath(d)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := runVerify(d); got != 2 {
		t.Errorf("deleted blob: exit %d want 2", got)
	}
	if got := runVerify("--all"); got != 2 {
		t.Errorf("--all with deletion: exit %d want 2", got)
	}
}

// Mismatch outranks missing when both occur under --all.
func TestRunVerifyMismatchOutranksMissing(t *testing.T) {
	s := withTestCAS(t)
	d1 := put(t, s, []byte("mutate me"))
	d2 := put(t, s, []byte("delete me"))
	// Both lists must be non-empty to prove the ranking branch: d2 is
	// referenced by state so its deletion shows up as missing.
	if err := s.SetNamespace("ns/x", d2); err != nil {
		t.Fatal(err)
	}
	p1, _ := s.BlobPath(d1)
	os.Chmod(p1, 0o644)
	os.WriteFile(p1, []byte("mutated"), 0o644)
	p2, _ := s.BlobPath(d2)
	os.Remove(p2)
	if got := runVerify("--all"); got != 1 {
		t.Errorf("mismatch+missing: exit %d want 1", got)
	}
}

func TestRunVerifyCanonicalDigestPrefix(t *testing.T) {
	s := withTestCAS(t)
	clean := put(t, s, []byte("canonical form"))
	// Bare hex stays valid.
	if got := runVerify(clean); got != 0 {
		t.Errorf("bare hex: exit %d want 0", got)
	}
	// Canonical sha256:<hex> form is accepted and normalized.
	if got := runVerify("sha256:" + clean); got != 0 {
		t.Errorf("sha256: prefix: exit %d want 0", got)
	}
	// Canonical form of a missing digest still exits 2.
	if got := runVerify("sha256:" + bytesToDigest([]byte("missing"))); got != 2 {
		t.Errorf("sha256: prefix missing: exit %d want 2", got)
	}
	// Garbage stays a usage-level verify failure (exit 2, missing).
	if got := runVerify("sha256:not-hex"); got != 2 {
		t.Errorf("garbage digest: exit %d want 2", got)
	}
}

func bytesToDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Table test for CLI dispatch; run() must preserve exit semantics exactly.
func TestRunDispatch(t *testing.T) {
	withTestCAS(t) // isolate SHARDR_CAS even for cases that never touch the store
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"cas alone", []string{"cas"}, exitUsage},
		{"cas verify without digest", []string{"cas", "verify"}, exitUsage},
		{"cas verify --all clean", []string{"cas", "verify", "--all"}, 0},
		{"version", []string{"version"}, 0},
	}
	for _, tt := range tests {
		if got := run(tt.args); got != tt.want {
			t.Errorf("%s (args %v): exit %d want %d", tt.name, tt.args, got, tt.want)
		}
	}
}

// Sanity for layout assumption used by the deleted-blob test.
func TestBlobPathMatchesLayout(t *testing.T) {
	s := withTestCAS(t)
	d := bytesToDigest([]byte("x"))
	p, err := s.BlobPath(d)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Root, "blobs", "sha256", d[:2], d[2:])
	if p != want {
		t.Fatalf("got %s want %s", p, want)
	}
}

// --- Straggler c: cas verify accepts no uppercase hex (canonical or nothing) ---

func TestVerifyRejectsUppercaseHex(t *testing.T) {
	s := withTestCAS(t)
	lower := put(t, s, []byte("canonical case"))
	upper := strings.ToUpper(lower)

	code, out := captureStdout(t, func() int { return runVerify(upper) })
	if code != 2 {
		t.Fatalf("uppercase hex: exit %d want 2", code)
	}
	if !strings.Contains(out, "uppercase") || !strings.Contains(out, "lowercase") {
		t.Fatalf("error must point at the canonical form, got: %s", out)
	}

	// Same digest, canonical lowercase: verifies clean.
	if code, _ := captureStdout(t, func() int { return runVerify(lower) }); code != 0 {
		t.Fatalf("lowercase hex: exit %d want 0", code)
	}
	// Canonical sha256: prefix + lowercase still works.
	if code, _ := captureStdout(t, func() int { return runVerify("sha256:" + lower) }); code != 0 {
		t.Fatalf("sha256: prefix: exit %d want 0", code)
	}
}

// captureStdout runs f, returning its exit code and everything printed to
// stdout (and stderr, merged) for assertion.
func captureStdout(t *testing.T, f func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return code, string(b)
}
