package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore opens a store in a temp dir and returns it plus a helper
// computing the digest of content.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parts(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.incomingDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// --- Layout & root resolution ---

func TestLayoutDirsCreated(t *testing.T) {
	s := newTestStore(t)
	for _, d := range []string{s.blobsDir(), s.incomingDir(), s.stateDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("expected dir %s", d)
		}
	}
}

func TestResolveRootOrder(t *testing.T) {
	t.Setenv("SHARDR_CAS", "/custom/cas")
	if r, _ := ResolveRoot(); r != "/custom/cas" {
		t.Fatalf("SHARDR_CAS ignored: %s", r)
	}
	t.Setenv("SHARDR_CAS", "")
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if r, _ := ResolveRoot(); r != "/xdg/shardr/cas" {
		t.Fatalf("XDG ignored: %s", r)
	}
	t.Setenv("XDG_DATA_HOME", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "share", "shardr", "cas")
	if r, _ := ResolveRoot(); r != want {
		t.Fatalf("default wrong: %s want %s", r, want)
	}
}

func TestBlobPathShardedAndValidated(t *testing.T) {
	s := newTestStore(t)
	d := strings.Repeat("ab", 32)
	p, err := s.BlobPath(d)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.blobsDir(), "ab", strings.Repeat("ab", 31))
	if p != want {
		t.Fatalf("path %s want %s", p, want)
	}
	for _, bad := range []string{
		"", "short", strings.Repeat("z", 64), strings.Repeat("A", 64),
		"../" + strings.Repeat("a", 60), d + "x",
	} {
		if _, err := s.BlobPath(bad); err == nil {
			t.Fatalf("digest %q accepted", bad)
		}
	}
}

// --- Write path ---

func TestPutHappyPath(t *testing.T) {
	s := newTestStore(t)
	content := []byte("hello shardhive")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	p, _ := s.BlobPath(d)
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("blob not at %s: %v", p, err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("blob content %q want %q", got, content)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o444 {
		t.Fatalf("blob mode %o want 0444", fi.Mode().Perm())
	}
	if leak := parts(t, s); len(leak) != 0 {
		t.Fatalf("part leaked: %v", leak)
	}
	if !s.Has(d) {
		t.Fatal("Has=false after Put")
	}
}

func TestPutMismatchLeavesNoTrace(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(digestOf([]byte("wrong")), bytes.NewReader([]byte("other bytes"))); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("want ErrDigestMismatch, got %v", err)
	}
	if s.Has(digestOf([]byte("other bytes"))) {
		t.Fatal("unverified bytes were promoted")
	}
	if leak := parts(t, s); len(leak) != 0 {
		t.Fatalf("part leaked after mismatch: %v", leak)
	}
}

func TestPutInvalidDigestRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put("not-a-digest", bytes.NewReader(nil)); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestPutIdempotentDoubleWrite(t *testing.T) {
	s := newTestStore(t)
	content := []byte("same bytes")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	p, _ := s.BlobPath(d)
	before, _ := os.Stat(p)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("second put: %v", err)
	}
	if leak := parts(t, s); len(leak) != 0 {
		t.Fatalf("part leaked on idempotent write: %v", leak)
	}
	// One blob, still correct content.
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, content) {
		t.Fatal("idempotent write changed content")
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(before.ModTime()) {
		t.Fatal("idempotent write replaced the blob (should keep existing)")
	}
	if n := countBlobFiles(t, s); n != 1 {
		t.Fatalf("blob count %d want 1", n)
	}
}

func countBlobFiles(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	filepath.WalkDir(s.blobsDir(), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// --- Concurrency ---

func TestPutConcurrentSameDigest(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("concurrent-payload"), 1000)
	d := digestOf(content)
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Put(d, bytes.NewReader(content))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent put failed: %v", err)
		}
	}
	if leak := parts(t, s); len(leak) != 0 {
		t.Fatalf("part leaked: %v", leak)
	}
	if n := countBlobFiles(t, s); n != 1 {
		t.Fatalf("blob count %d want 1", n)
	}
	if err := s.Verify(d); err != nil {
		t.Fatalf("blob corrupt after concurrent writes: %v", err)
	}
}

// --- Stale part cleanup ---

func TestOpenCleansStaleParts(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.incomingDir(), "deadbeefdeadbeefdeadbeefdeadbeef.part")
	fresh := filepath.Join(s.incomingDir(), "0000000000000000.part")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale part survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh part removed: %v", err)
	}
}

// --- Verify (mutation tests: corruption must be caught) ---

func TestVerifyClean(t *testing.T) {
	s := newTestStore(t)
	d := digestOf([]byte("intact"))
	if err := s.Put(d, bytes.NewReader([]byte("intact"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(d); err != nil {
		t.Fatalf("clean blob flagged: %v", err)
	}
}

func TestVerifyDetectsMutation(t *testing.T) {
	s := newTestStore(t)
	content := []byte("original bytes")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	// Tamper: flip bytes under the 0444 mode — an owner can always chmod.
	p, _ := s.BlobPath(d)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(content, "tampered"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(d); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mutation not detected: %v", err)
	}
}

func TestVerifyMissing(t *testing.T) {
	s := newTestStore(t)
	d := digestOf([]byte("never written"))
	if err := s.Verify(d); !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("want ErrBlobMissing, got %v", err)
	}
}

func TestVerifyAllFindsMutatedAndMissing(t *testing.T) {
	s := newTestStore(t)
	var digests []string
	for i := 0; i < 3; i++ {
		b := []byte(fmt.Sprintf("blob-%d", i))
		d := digestOf(b)
		if err := s.Put(d, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}
	// Mutate blob 0, delete blob 1 (referenced by state so --all can notice
	// the deletion), leave blob 2 clean.
	mutated := digests[0]
	deleted := digests[1]
	if err := s.SetNamespace("ns/current-index", deleted); err != nil {
		t.Fatal(err)
	}
	p, _ := s.BlobPath(mutated)
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("mutated"), 0o644)
	os.Remove(filepath.Join(s.blobsDir(), deleted[:2], deleted[2:]))

	res, err := s.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Mismatched) != 1 || res.Mismatched[0] != mutated {
		t.Fatalf("mismatched %v want [%s]", res.Mismatched, mutated)
	}
	if len(res.Missing) != 1 || res.Missing[0] != deleted {
		t.Fatalf("missing %v want [%s]", res.Missing, deleted)
	}
	if len(res.StateErrors) != 0 {
		t.Fatalf("unexpected state errors %v", res.StateErrors)
	}
	if err := s.Verify(digests[2]); err != nil {
		t.Fatalf("clean blob flagged: %v", err)
	}
}

// errSourceFailed simulates a mid-stream source failure during an import.
var errSourceFailed = errors.New("source stream failed")

// failingReader yields a few bytes, then fails on every subsequent Read.
type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial-bytes"), errSourceFailed
	}
	return 0, errSourceFailed
}

func TestPutReaderErrorCleansPart(t *testing.T) {
	s := newTestStore(t)
	if err := s.Put(digestOf([]byte("irrelevant")), &failingReader{}); !errors.Is(err, errSourceFailed) {
		t.Fatalf("want source error to surface, got %v", err)
	}
	if leak := parts(t, s); len(leak) != 0 {
		t.Fatalf("part leaked after reader failure: %v", leak)
	}
	if n := countBlobFiles(t, s); n != 0 {
		t.Fatalf("blob promoted despite reader failure: %d blobs", n)
	}
}

// --- VerifyAll: foreign files must not abort the walk ---

func TestVerifyAllSkipsForeignFiles(t *testing.T) {
	s := newTestStore(t)
	content := []byte("clean")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(s.blobsDir(), ".DS_Store"),
		filepath.Join(s.blobsDir(), "ab", "zz-not-hex"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.VerifyAll()
	if err != nil {
		t.Fatalf("foreign file aborted verification: %v", err)
	}
	if len(res.Mismatched) != 0 || len(res.Missing) != 0 {
		t.Fatalf("mismatched=%v missing=%v, want both empty", res.Mismatched, res.Missing)
	}
	if err := s.Verify(d); err != nil {
		t.Fatalf("clean blob flagged: %v", err)
	}
}

// --- Read path ---

func TestOpenReadsBlob(t *testing.T) {
	s := newTestStore(t)
	content := []byte("readable")
	d := digestOf(content)
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, len(content))
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, content) {
		t.Fatalf("read %q want %q", buf, content)
	}
}

// --- State store ---

func TestStateNamespacesRoundtrip(t *testing.T) {
	s := newTestStore(t)
	d := digestOf([]byte("index"))
	if err := s.SetNamespace("ns/models", d); err != nil {
		t.Fatal(err)
	}
	ns, err := s.Namespaces()
	if err != nil {
		t.Fatal(err)
	}
	if ns["ns/models"] != "sha256:"+d {
		t.Fatalf("namespace %v", ns)
	}
	if err := s.DeleteNamespace("ns/models"); err != nil {
		t.Fatal(err)
	}
	ns, _ = s.Namespaces()
	if _, ok := ns["ns/models"]; ok {
		t.Fatal("namespace survived delete")
	}
	// Absent state file is an empty map, not an error.
	if len(ns) != 0 {
		t.Fatalf("want empty map, got %v", ns)
	}
}

func TestStateTagAliasesRoundtrip(t *testing.T) {
	s := newTestStore(t)
	d := digestOf([]byte("blob"))
	// R3: tags are scoped per repository; the key is ns/name:tag.
	if err := s.SetTagAlias("ns/models", "stable", d); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTagAlias("other/models", "stable", d); err != nil {
		t.Fatal(err) // same tag name, different repo: independent
	}
	tags, err := s.TagAliases()
	if err != nil {
		t.Fatal(err)
	}
	if tags["ns/models:stable"] != "sha256:"+d || tags["other/models:stable"] != "sha256:"+d {
		t.Fatalf("tags %v", tags)
	}
	if err := s.DeleteTagAlias("ns/models", "stable"); err != nil {
		t.Fatal(err)
	}
	tags, _ = s.TagAliases()
	if _, ok := tags["ns/models:stable"]; ok {
		t.Fatal("tag survived delete")
	}
	if _, ok := tags["other/models:stable"]; !ok {
		t.Fatal("delete leaked across repo scope")
	}
	if err := s.DeleteTagAlias("other/models", "stable"); err != nil {
		t.Fatal(err)
	}
}

func TestStateCanonicalAndAtomic(t *testing.T) {
	s := newTestStore(t)
	// Both digest forms normalize to the canonical "sha256:" storage form.
	bare := digestOf([]byte("1"))
	if err := s.SetNamespace("ns/b", bare); err != nil { // bare hex accepted
		t.Fatal(err)
	}
	if err := s.SetNamespace("ns/a", "sha256:"+digestOf([]byte("2"))); err != nil { // prefixed accepted
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.stateDir(), namespacesFile))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ns/a":"sha256:` + digestOf([]byte("2")) + `","ns/b":"sha256:` + bare + `"}`
	if string(b) != want {
		t.Fatalf("canonical form %s want %s", b, want)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(s.stateDir())
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
	// Reload path: new Store handle on same root sees the data.
	s2, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	ns, _ := s2.Namespaces()
	if len(ns) != 2 {
		t.Fatalf("reload lost state: %v", ns)
	}
}

// Random-content stress across distinct digests, ensuring shard dirs and
// file names are derived correctly end to end.
func TestPutRandomBlobsDistinctPaths(t *testing.T) {
	s := newTestStore(t)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		b := make([]byte, rng.Intn(4096))
		rng.Read(b)
		d := digestOf(b)
		if err := s.Put(d, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		if err := s.Verify(d); err != nil {
			t.Fatalf("blob %d: %v", i, err)
		}
	}
	if n := countBlobFiles(t, s); n != 50 {
		t.Fatalf("blob count %d want 50", n)
	}
}

// --- Issue #4 hard follow-up: state validation (no garbage in state/) ---

func TestStateValidationRejectsGarbage(t *testing.T) {
	s := newTestStore(t)
	validDigest := digestOf([]byte("valid"))
	for _, bad := range []string{
		"", "name", "ns/name/extra", "/name", "ns/", "UP/name", "ns/UP",
		"ns/na me", "-lead/name",
	} {
		if err := s.SetNamespace(bad, validDigest); err == nil {
			t.Fatalf("namespace key %q accepted", bad)
		}
	}
	for _, bad := range []string{
		"", "xyz", "sha256:", "sha256:xyz", validDigest[:63], validDigest + "0",
		"SHA256:" + validDigest,
	} {
		if err := s.SetNamespace("ns/ok", bad); err == nil {
			t.Fatalf("digest %q accepted", bad)
		}
		if err := s.SetTagAlias("ns/ok", "ok-tag", bad); err == nil {
			t.Fatalf("tag digest %q accepted", bad)
		}
	}
	for _, bad := range []string{
		"", "q8_0", "sha256-abc", "SHA256:abc", ".lead", "sp ace",
	} {
		if err := s.SetTagAlias("ns/ok", bad, validDigest); err == nil {
			t.Fatalf("tag alias %q accepted", bad)
		}
		// R3: the namespace scope must validate too.
		if err := s.SetTagAlias(bad, "ok-tag", validDigest); err == nil {
			t.Fatalf("tag scope %q accepted", bad)
		}
	}
	// Nothing may have been written into state/.
	ns, _ := s.Namespaces()
	if len(ns) != 0 {
		t.Fatalf("garbage leaked into state: %v", ns)
	}
	tags, _ := s.TagAliases()
	if len(tags) != 0 {
		t.Fatalf("garbage leaked into state: %v", tags)
	}
}

// --- Issue #4 hard follow-up: VerifyAll surfaces state-load errors ---

func TestVerifyAllReportsStateErrors(t *testing.T) {
	s := newTestStore(t)
	// A clean blob that must still verify despite corrupt state.
	if err := s.Put(digestOf([]byte("clean")), strings.NewReader("clean")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.stateDir(), namespacesFile),
		[]byte("{{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.VerifyAll()
	if err != nil {
		t.Fatalf("corrupt state aborted blob verification: %v", err)
	}
	if len(res.Mismatched) != 0 {
		t.Fatalf("mismatched %v", res.Mismatched)
	}
	if len(res.StateErrors) != 1 || !strings.Contains(res.StateErrors[0], "namespaces") {
		t.Fatalf("state errors %v, want one mentioning namespaces", res.StateErrors)
	}
	// Corrupt state must not silently mask missing references: with no
	// readable refs the missing list is empty, but StateErrors says why.
	if len(res.Missing) != 0 {
		t.Fatalf("missing %v", res.Missing)
	}
}

// --- Straggler b: state load-mutate-save must be atomic in-process ---

func TestStateConcurrentUpdatesNoLostWrite(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("ns/r%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.SetNamespace(key, digestOf([]byte(key))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ns, _ := s.Namespaces()
	if len(ns) != n {
		t.Fatalf("lost updates: %d/%d namespaces survived", len(ns), n)
	}
}

// VerifyAll covers the swarm state files: a corrupt distribution.json is
// a StateError; a link pointing at a missing record blob is Missing.
func TestVerifyAllSwarmState(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Healthy link: record blob present.
	recDigest := putTestBlob(t, s, []byte(`{"schemaVersion":1}`))
	manDigest := putTestBlob(t, s, []byte(`{"artifactType":"model"}`))
	if err := s.SetDistributionLink("sha256:"+manDigest, "sha256:"+recDigest); err != nil {
		t.Fatal(err)
	}
	res, err := s.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StateErrors) != 0 || len(res.Missing) != 0 {
		t.Fatalf("healthy state: %+v", res)
	}

	// Case A — manifest PRESENT, record blob missing: Missing is exactly
	// the record digest (not the manifest, not both).
	s2, _ := Open(t.TempDir())
	if err := s2.Put(manDigest, bytes.NewReader([]byte(`{"artifactType":"model"}`))); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetDistributionLink("sha256:"+manDigest, "sha256:"+recDigest); err != nil {
		t.Fatal(err)
	}
	res, err = s2.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != recDigest {
		t.Fatalf("case A (record missing): Missing must be exactly [%s], got %v", recDigest, res.Missing)
	}

	// Case B — record blob PRESENT, manifest missing: Missing is exactly
	// the manifest digest.
	s3, _ := Open(t.TempDir())
	if err := s3.Put(recDigest, bytes.NewReader([]byte(`{"schemaVersion":1}`))); err != nil {
		t.Fatal(err)
	}
	if err := s3.SetDistributionLink("sha256:"+manDigest, "sha256:"+recDigest); err != nil {
		t.Fatal(err)
	}
	res, err = s3.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != manDigest {
		t.Fatalf("case B (manifest missing): Missing must be exactly [%s], got %v", manDigest, res.Missing)
	}

	// Corrupt distribution.json → StateErrors (loud, never silently rebuilt).
	s4, _ := Open(t.TempDir())
	if err := os.MkdirAll(filepath.Join(s4.Root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s4.Root, "state", "distribution.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = s4.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StateErrors) == 0 || !strings.Contains(res.StateErrors[0], "distribution") {
		t.Fatalf("corrupt distribution.json must be a StateError, got %+v", res.StateErrors)
	}

	// Corrupt swarm-hints.json → StateErrors.
	s5, _ := Open(t.TempDir())
	if err := os.MkdirAll(filepath.Join(s5.Root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s5.Root, "state", "swarm-hints.json"), []byte("[broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = s5.VerifyAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.StateErrors {
		if strings.Contains(e, "swarm-hints") {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt swarm-hints.json must be a StateError, got %+v", res.StateErrors)
	}
}

func putTestBlob(t *testing.T, s *Store, content []byte) string {
	t.Helper()
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	if err := s.Put(d, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return d
}
