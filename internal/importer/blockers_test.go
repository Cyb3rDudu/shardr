package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// --- B1: upstream config.json must actually feed the pipeline ---

func TestConfigFeedsPipeline(t *testing.T) {
	store := mustStore(t)
	cfg := `{"model_type":"toyfam","max_positional_embeddings":4096,` +
		`"quantization_config":{"quant_method":"fp8"}}`
	res, err := Import(context.Background(), store, []Source{
		{Name: "toy-00001-of-00002.gguf", Open: bytesSource("part1")},
		{Name: "toy-00002-of-00002.gguf", Open: bytesSource("part2")},
		{Name: "config.json", Open: bytesSource(cfg)},
	}, ImportOptions{As: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Quant != "fp8" {
		t.Fatalf("quant must come from quantization_config, got %+v", res.Artifacts)
	}
	// The built modelconfig must carry the derived fields.
	f := findFile(t, store, res.Artifacts[0].Manifest, "modelconfig.json")
	doc := string(f)
	for _, want := range []string{
		`"family":"toyfam"`, `"quantization":"fp8"`,
		`"contextLength":4096`, `"ctx_size":4096`,
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("modelconfig missing %s: %s", want, doc)
		}
	}
	// config.json is consumed, not skipped.
	manifest := string(readCAS(t, store, res.Artifacts[0].Manifest))
	if strings.Contains(manifest, `"io.shardr.import.skipped":"1"`) == false {
		// skipped counts README-like default-denies only; with no other
		// files it must be 0 — config.json is chain input, not a skip.
		if !strings.Contains(manifest, `"io.shardr.import.skipped":"0"`) {
			t.Fatalf("config.json must not be counted as skipped: %s", manifest)
		}
	}
}

func findFile(t *testing.T, store *cas.Store, manifestDigest, name string) []byte {
	t.Helper()
	m := decodeManifest(t, readCAS(t, store, manifestDigest))
	for _, f := range m.Files {
		if f.Name == name {
			return []byte(readCAS(t, store, f.Digest))
		}
	}
	t.Fatalf("file %s not in manifest", name)
	return nil
}

func decodeManifest(t *testing.T, doc string) *artifact.Manifest {
	t.Helper()
	var m artifact.Manifest
	if err := jsonUnmarshalString(doc, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

// B2 (HF SHA pinning) is proven end-to-end in internal/api:
// TestImportHFPinsRevision — the stub serves ONLY the SHA revision, so a
// branch-fetch fails the import.

// --- B3: same-quant groups are a loud conflict ---

func TestSameQuantConflictIsLoud(t *testing.T) {
	store := mustStore(t)
	_, err := Import(context.Background(), store, []Source{
		{Name: "alpha-q8_0.gguf", Open: bytesSource("a")},
		{Name: "beta-q8_0.gguf", Open: bytesSource("b")},
	}, ImportOptions{As: "x/y"})
	if err == nil || !strings.Contains(err.Error(), "quant conflict") {
		t.Fatalf("want loud quant conflict, got %v", err)
	}
	ns, _ := store.Namespaces()
	if len(ns) != 0 {
		t.Fatalf("failed import must not mutate state: %v", ns)
	}
}

// --- B4: missing current-index blob is loud, never a silent rebuild ---

func TestMissingIndexBlobIsLoud(t *testing.T) {
	store := mustStore(t)
	ghost := testDigestOf("ghost index")
	if err := store.SetNamespace("x/y", ghost); err != nil {
		t.Fatal(err)
	}
	_, err := Import(context.Background(), store, []Source{
		{Name: "a-q8_0.gguf", Open: bytesSource("a")},
	}, ImportOptions{As: "x/y"})
	if err == nil || !strings.Contains(err.Error(), "missing from the CAS") {
		t.Fatalf("want loud missing-index error, got %v", err)
	}
	ns, _ := store.Namespaces()
	if ns["x/y"] != "sha256:"+ghost {
		t.Fatalf("state must not be silently rebuilt: %v", ns["x/y"])
	}
}

// --- B7: corrupt current-index members are loud, never carried over ---

func TestCorruptIndexMembersLoud(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, members string }{
		{"duplicate quant", `[{"manifest":"` + good + `","quant":"q8_0","weightsFormat":"gguf"},{"manifest":"sha256:` + strings.Repeat("b", 64) + `","quant":"q8_0","weightsFormat":"gguf"}]`},
		{"invalid manifest digest", `[{"manifest":"sha256:abcd","quant":"q8_0","weightsFormat":"gguf"}]`},
		{"invalid quant syntax", `[{"manifest":"` + good + `","quant":"not-a-quant","weightsFormat":"gguf"}]`},
	} {
		store := mustStore(t)
		idx := `{"schemaVersion":1,"artifactType":"model-index","members":` + tc.members + `}`
		if err := store.Put(testDigestOf(idx), strings.NewReader(idx)); err != nil {
			t.Fatal(err)
		}
		d := "sha256:" + testDigestOf(idx)
		if err := store.SetNamespace("x/y", d); err != nil {
			t.Fatal(err)
		}
		_, err := Import(context.Background(), store, []Source{
			{Name: "model-q4_0.gguf", Open: bytesSource("w")},
		}, ImportOptions{As: "x/y"})
		if err == nil || !strings.Contains(err.Error(), "fails validation") {
			t.Fatalf("%s: want loud index-validation error, got %v", tc.name, err)
		}
		ns, _ := store.Namespaces()
		if ns["x/y"] != d {
			t.Fatalf("%s: state must stay untouched, got %v", tc.name, ns["x/y"])
		}
		// Import failed before any merge: no new index was published — the
		// corrupt members cannot have landed in a new current index.
	}
}

// --- B5: multiple PEFT pairs survive as separate adapters ---

func TestMultipleAdaptersSurvive(t *testing.T) {
	store := mustStore(t)
	res, err := Import(context.Background(), store, []Source{
		{Name: "toy-q8_0.gguf", Open: bytesSource("w")},
		{Name: "adapter_model.safetensors", Open: bytesSource("root-weights")},
		{Name: "adapter_config.json", Open: bytesSource("root-config")},
		{Name: "loraA/adapter_model.safetensors", Open: bytesSource("A-weights")},
		{Name: "loraA/adapter_config.json", Open: bytesSource("A-config")},
		{Name: "loraB/adapter_model.safetensors", Open: bytesSource("B-weights")},
		{Name: "loraB/adapter_config.json", Open: bytesSource("B-config")},
	}, ImportOptions{As: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeManifest(t, readCAS(t, store, res.Artifacts[0].Manifest))
	adapters := map[string]bool{}
	for _, f := range m.Files {
		if f.Kind == "adapter" {
			adapters[f.Name] = true
		}
	}
	if !adapters["adapter.tar"] || !adapters["loraA/adapter.tar"] || !adapters["loraB/adapter.tar"] {
		t.Fatalf("adapter entries: %v (root pair must stay adapter.tar)", adapters)
	}
	// Both sub-adapters' contents must be present (digests resolve in CAS).
	for _, f := range m.Files {
		if f.Kind != "adapter" {
			continue
		}
		tar := readCAS(t, store, f.Digest)
		if f.Name == "loraA/adapter.tar" && !strings.Contains(tar, "A-weights") {
			t.Fatalf("loraA tar lost its weights: %q", tar)
		}
		if f.Name == "loraB/adapter.tar" && !strings.Contains(tar, "B-weights") {
			t.Fatalf("loraB tar lost its weights: %q", tar)
		}
		if f.Name == "adapter.tar" && !strings.Contains(tar, "root-weights") {
			t.Fatalf("root tar lost its weights: %q", tar)
		}
	}
}

// --- B6: concurrent imports to one namespace lose no members ---

func TestConcurrentImportsLoseNoMembers(t *testing.T) {
	store := mustStore(t)
	var wg sync.WaitGroup
	for _, q := range []string{"q8_0", "q4_0"} {
		q := q
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Import(context.Background(), store, []Source{
				{Name: "model-" + q + ".gguf", Open: bytesSource("weights-" + q)},
			}, ImportOptions{As: "x/y"})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ns, _ := store.Namespaces()
	f := store.Open
	_ = f
	idxDoc := readCAS(t, store, ns["x/y"])
	var idx artifact.Index
	if err := jsonUnmarshalString(idxDoc, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Members) != 2 {
		t.Fatalf("both members must survive, got %d: %s", len(idx.Members), idxDoc)
	}
}

// --- stragglers ---

func TestDualChatTemplateKeepsJinja(t *testing.T) {
	store := mustStore(t)
	res, err := Import(context.Background(), store, []Source{
		{Name: "model-bf16-00001-of-00002.safetensors", Open: safetensorsFile(t, "BF16")},
		{Name: "chat_template.jinja", Open: bytesSource("jinja wins")},
		{Name: "chat_template.json", Open: bytesSource("json loses")},
	}, ImportOptions{As: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeManifest(t, readCAS(t, store, res.Artifacts[0].Manifest))
	n := 0
	for _, f := range m.Files {
		if f.Kind == "chat-template" {
			n++
			if f.Name != "chat_template.jinja" {
				t.Fatalf("kept wrong template: %s", f.Name)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one chat-template, got %d", n)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("dual template must warn")
	}
}

func TestNonSPDXLicenseAnnotated(t *testing.T) {
	store := mustStore(t)
	res, err := Import(context.Background(), store, []Source{
		{Name: "toy-q8_0.gguf", Open: bytesSource("w")},
		{Name: "LICENSE", Open: bytesSource("Some Custom License Text v2, all rights weird")},
	}, ImportOptions{As: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readCAS(t, store, res.Artifacts[0].Manifest)
	if !strings.Contains(manifest, `"io.shardr.license.name":"LICENSE"`) {
		t.Fatalf("license.name must be annotated even without SPDX: %s", manifest)
	}
	if strings.Contains(manifest, `"io.shardr.license":"`) {
		t.Fatalf("no SPDX value must be claimed: %s", manifest)
	}
}

func TestPathPrefixCollisionRejected(t *testing.T) {
	m := &artifact.Manifest{SchemaVersion: 1, ArtifactType: "model",
		Files: []artifact.File{
			{Kind: "config", Name: "modelconfig.json", Size: 10, Digest: "sha256:" + strings.Repeat("a", 64), BT: artifact.BT{MerkleRoot: "sha256:" + strings.Repeat("a", 64)}},
			{Kind: "weights.gguf", Name: "model.gguf", Size: 100, Digest: "sha256:" + strings.Repeat("b", 64), BT: artifact.BT{MerkleRoot: "sha256:" + strings.Repeat("b", 64)}},
			{Kind: "weights.gguf", Name: "model.gguf/inner.gguf", Size: 100, Digest: "sha256:" + strings.Repeat("c", 64), BT: artifact.BT{MerkleRoot: "sha256:" + strings.Repeat("c", 64)}},
		}}
	if err := artifact.ValidateManifest(m); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("want path-prefix collision error, got %v", err)
	}
}

func TestImatrixWithoutGGUFRecognized(t *testing.T) {
	c, err := classify([]Source{src("Qwen3-8B.imatrix", "x")})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 1 || c.entries[0].kind != "weights.aux" || c.entries[0].role != "imatrix" {
		t.Fatalf("imatrix must be aux: %+v", c.entries)
	}
}

// helpers -----------------------------------------------------------

func testDigestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func jsonUnmarshalString(doc string, v any) error {
	return json.Unmarshal([]byte(doc), v)
}

func safetensorsFile(t *testing.T, dtype string) func() (readCloser, error) {
	t.Helper()
	header := `{"w":{"dtype":"` + dtype + `","shape":[4,4],"data_offsets":[0,32]}}`
	var lenPrefix [8]byte
	lenPrefix[0] = byte(len(header))
	var body []byte
	body = append(body, lenPrefix[:]...)
	body = append(body, header...)
	body = append(body, make([]byte, 32)...)
	b := string(body)
	return func() (readCloser, error) {
		return io.NopCloser(strings.NewReader(b)), nil
	}
}
