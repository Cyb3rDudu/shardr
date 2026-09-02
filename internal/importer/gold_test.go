package importer

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

var updateGold = flag.Bool("update", false, "rewrite golden import digests (only with a stated reason)")

// golden captures the deterministic identity of the fixture import. The
// digests cover every artifact document and every derived blob — a change
// in classification, ordering, tars, modelconfig, or torrent math flips
// them. Updating this file is a deliberate, reviewable act: run
// `go test ./internal/importer -update` and justify the delta in the PR.
type golden struct {
	Manifest      string `json:"manifest"`
	Index         string `json:"index"`
	Distribution  string `json:"distribution"`
	PieceLayers   string `json:"pieceLayers"`
	ManifestBytes string `json:"manifestDoc"`
	IndexBytes    string `json:"indexDoc"`
	RecordBytes   string `json:"recordDoc"`
}

const goldNS = "gold/repo"
const goldFile = "testdata/golden.json"

func importGoldFixtures(t *testing.T, root string) (*ImportResult, *golden, *cas.Store) {
	t.Helper()
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := LocalSources([]string{"testdata/gold"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(context.Background(), store, sources, ImportOptions{As: goldNS})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("fixtures must produce exactly one artifact, got %d", len(res.Artifacts))
	}
	a := res.Artifacts[0]
	g := &golden{
		Manifest:      a.Manifest,
		Index:         res.IndexDigest,
		Distribution:  a.Record,
		PieceLayers:   a.PieceLayers,
		ManifestBytes: readCAS(t, store, a.Manifest),
		IndexBytes:    readCAS(t, store, res.IndexDigest),
		RecordBytes:   readCAS(t, store, a.Record),
	}
	return res, g, store
}

func readCAS(t *testing.T, store *cas.Store, digest string) string {
	t.Helper()
	f, err := store.Open(trimDigest(digest))
	if err != nil {
		t.Fatalf("read %s: %v", digest, err)
	}
	defer f.Close()
	b, err := readFileAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func trimDigest(d string) string { return d[len("sha256:"):] }

func TestGoldenImport(t *testing.T) {
	_, g, store := importGoldFixtures(t, t.TempDir())

	if *updateGold {
		b, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("golden updated — justify the delta in the PR: %s", goldFile)
	}
	b, err := os.ReadFile(goldFile)
	if err != nil {
		t.Fatalf("golden file missing (run go test ./internal/importer -update once and commit with justification): %v", err)
	}
	var want golden
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if *g != want {
		t.Fatalf("import diverged from golden:\n got %+v\nwant %+v", *g, want)
	}
	// The golden digests must actually be present and hash-true.
	if err := store.Verify(trimDigest(g.Manifest)); err != nil {
		t.Fatalf("golden manifest fails CAS verify: %v", err)
	}
}

// TestImportConvergence is the hard convergence proof (001 §7.5): the same
// input set, imported into two FRESH CAS roots, produces byte-identical
// artifact digests — manifest, index, distribution record, piece layers.
func TestImportConvergence(t *testing.T) {
	_, g1, _ := importGoldFixtures(t, t.TempDir())
	_, g2, _ := importGoldFixtures(t, t.TempDir())
	if g1.Manifest != g2.Manifest || g1.Index != g2.Index ||
		g1.Distribution != g2.Distribution || g1.PieceLayers != g2.PieceLayers ||
		g1.ManifestBytes != g2.ManifestBytes || g1.RecordBytes != g2.RecordBytes {
		t.Fatalf("convergence broken:\n%+v\n%+v", *g1, *g2)
	}
}

// TestImportReRunIdempotent: importing again into the SAME store yields
// the same digests (index unchanged, no duplicate members).
func TestImportReRunIdempotent(t *testing.T) {
	root := t.TempDir()
	res1, _, store := importGoldFixtures(t, root)
	sources, _ := LocalSources([]string{"testdata/gold"})
	res2, err := Import(context.Background(), store, sources, ImportOptions{As: goldNS})
	if err != nil {
		t.Fatal(err)
	}
	if res1.IndexDigest != res2.IndexDigest {
		t.Fatalf("index digest changed on re-import: %s -> %s", res1.IndexDigest, res2.IndexDigest)
	}
	ns, _ := store.Namespaces()
	if ns[goldNS] != res2.IndexDigest { // IndexDigest is already sha256:-prefixed
		t.Fatalf("namespace not at current index: %v", ns[goldNS])
	}
}

// --- Eligibility gate (001 §8.1, fail closed) ---

func TestEligibilityNoWeightsFailsClosed(t *testing.T) {
	store, _ := cas.Open(t.TempDir())
	sources := []Source{
		{Name: "README.md", Open: fileOpen("testdata/gold/README.md")},
		{Name: "config.json", Open: fileOpen("testdata/gold/config.json")},
		{Name: "tokenizer.json", Open: fileOpen("testdata/gold/tokenizer.json")},
		{Name: "logo.png", Open: bytesSource("\x89PNG fakes")},
	}
	if _, err := Import(context.Background(), store, sources, ImportOptions{As: "x/y"}); err == nil ||
		!contains(err.Error(), "eligibility") {
		t.Fatalf("want eligibility failure, got %v", err)
	}
}

// .bin/.pt/.pth are NEVER weights — pickle formats are an execution
// vector (001 §8.1).
func TestEligibilityPickleFormatsNeverWeights(t *testing.T) {
	store, _ := cas.Open(t.TempDir())
	sources := []Source{
		{Name: "model.bin", Open: bytesSource("pickle bytes")},
		{Name: "pytorch_model-00001-of-00002.bin", Open: bytesSource("pickle bytes")},
		{Name: "adapter.pt", Open: bytesSource("pickle bytes")},
		{Name: "rng_state.pth", Open: bytesSource("pickle bytes")},
	}
	_, err := Import(context.Background(), store, sources, ImportOptions{As: "x/y"})
	if err == nil || !contains(err.Error(), "eligibility") {
		t.Fatalf("pickle-only repo must fail the gate, got %v", err)
	}
	// And nothing may have been written.
	ns, _ := store.Namespaces()
	if len(ns) != 0 {
		t.Fatalf("state mutated by failed import: %v", ns)
	}
}

// --- R2: reserved manifest/ names and non-canonical segments ---

func TestReservedAndNonCanonicalNamesRejected(t *testing.T) {
	for _, bad := range []string{
		"manifest", "manifest/anything", "./x", "a//b", "../x", "/lead",
		"a/./b", "a/../b", "",
	} {
		if err := validateArtifactName(bad); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
	for _, ok := range []string{"model.safetensors", "sub/dir/file.json", "a.b.c"} {
		if err := validateArtifactName(ok); err != nil {
			t.Fatalf("name %q rejected: %v", ok, err)
		}
	}
}

// The importer must refuse an input set whose artifact names hit the
// reservation: a weights file under a manifest/ path upstream collides
// with the torrent manifest tree (R2, 004 §3). Tokenizer/adapter members
// are folded into fixed tar names and cannot collide; a bare unrecognized
// 'manifest' file falls to the default-deny skip.
func TestImportRejectsReservedName(t *testing.T) {
	store, _ := cas.Open(t.TempDir())
	sources := []Source{
		{Name: "toy-q8_0.gguf", Open: bytesSource("gguf bytes")},
		{Name: "manifest/extra.gguf", Open: bytesSource("collision")},
	}
	_, err := Import(context.Background(), store, sources, ImportOptions{As: "x/y"})
	if err == nil || !contains(err.Error(), "reserved") {
		t.Fatalf("reserved name must be rejected loudly, got %v", err)
	}
}

func fileOpen(p string) func() (readCloser, error) {
	return func() (readCloser, error) { return os.Open(p) }
}

// --- test helpers ---

type readCloser = io.ReadCloser // alias: Source.Open compatibility

func readFileAll(r readCloser) ([]byte, error) { return io.ReadAll(r) }

func bytesSource(s string) func() (readCloser, error) {
	return func() (readCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// validateArtifactName is the importer-facing alias of the artifact rule
// (001 §3.1 rule 1 + R2 reservation).
func validateArtifactName(name string) error { return artifact.ValidateFileName(name) }
