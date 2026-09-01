package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
		{"no args", nil, 2},
		{"unknown command", []string{"frobnicate"}, 2},
		{"cas alone", []string{"cas"}, 2},
		{"cas verify without digest", []string{"cas", "verify"}, 2},
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
