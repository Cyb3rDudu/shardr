package importer

import (
	"context"
	"io"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

func src(name, content string) Source {
	return Source{Name: name, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(newStrReader(content)), nil
	}}
}

func newStrReader(s string) *stringReader { return &stringReader{s: s} }

type stringReader struct{ s string }

func (r *stringReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

func TestClassifySplitAndQuants(t *testing.T) {
	c, err := classify([]Source{
		src("qwen3.8-27b-ud-q4_k_m-00001-of-00002.gguf", "a"),
		src("qwen3.8-27b-ud-q4_k_m-00002-of-00002.gguf", "b"),
		src("model-00001-of-00003.safetensors", "c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var found struct{ split1, split2, st bool }
	for _, e := range c.entries {
		switch {
		case e.name == "qwen3.8-27b-ud-q4_k_m-00001-of-00002.gguf":
			found.split1 = e.part == 1 && e.quant == "ud-q4_k_m"
		case e.name == "qwen3.8-27b-ud-q4_k_m-00002-of-00002.gguf":
			found.split2 = e.part == 2 && e.quant == "ud-q4_k_m"
		case e.name == "model-00001-of-00003.safetensors":
			found.st = e.kind == "weights.safetensors" && e.part == 1
		}
	}
	if !found.split1 || !found.split2 || !found.st {
		t.Fatalf("split classification wrong: %+v (entries %+v)", found, c.entries)
	}
}

func TestClassifyRolesAndTar(t *testing.T) {
	c, err := classify([]Source{
		src("mmproj-f16.gguf", "m"),
		src("imatrix.dat.gguf", "i"),
		src("model.safetensors.index.json", "{}"),
		src("adapter_model.safetensors", "x"),
		src("adapter_config.json", "{}"),
		src("tokenizer.json", "{}"),
		src("tokenizer_config.json", "{}"),
		src("vocab.json", "{}"),
		src("chat_template.jinja", "t"),
		src("modeling_toy.py", "code"),
		src("toy_processor.py", "code"),
		src("random_tool.py", "code"),
		src("README.md", "r"),
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]entry{}
	for _, e := range c.entries {
		kinds[e.name] = e
	}
	if e, ok := kinds["mmproj-f16.gguf"]; !ok || e.kind != "weights.aux" || e.role != "vision-projector" {
		t.Fatalf("mmproj: %+v", e)
	}
	if e, ok := kinds["imatrix.dat.gguf"]; !ok || e.role != "imatrix" {
		t.Fatalf("imatrix: %+v", e)
	}
	if e, ok := kinds["model.safetensors.index.json"]; !ok || e.role != "weights-index" {
		t.Fatalf("index.json: %+v", e)
	}
	if e, ok := kinds["adapter.tar"]; !ok || e.kind != "adapter" || len(e.adapterFiles) != 2 {
		t.Fatalf("adapter pair must fold into one tar: %+v", e)
	}
	if e, ok := kinds["tokenizer.tar"]; !ok || e.kind != "tokenizer" || len(e.tokenizer) != 3 {
		t.Fatalf("tokenizer set must fold into one tar: %+v", e)
	}
	if e, ok := kinds["chat_template.jinja"]; !ok || e.kind != "chat-template" {
		t.Fatalf("chat template: %+v", e)
	}
	if e, ok := kinds["modeling_toy.py"]; !ok || e.kind != "code" || e.codeRole != "modeling" {
		t.Fatalf("modeling code: %+v", e)
	}
	if e, ok := kinds["toy_processor.py"]; !ok || e.codeRole != "processor" {
		t.Fatalf("processor code: %+v", e)
	}
	if _, ok := kinds["random_tool.py"]; ok {
		t.Fatal("unrecognized .py must be skipped (default deny)")
	}
	// README skipped, adapter pair NOT skipped, unknown py skipped.
	if c.skipped != 2 {
		t.Fatalf("skipped %d want 2 (README + random_tool.py)", c.skipped)
	}
}

func TestClassifyTrainingArtifactsSkipped(t *testing.T) {
	c, _ := classify([]Source{
		src("checkpoint-1200/model.safetensors", "x"),
		src("optimizer.pt", "x"),
		src("scheduler.pt", "x"),
		src("rng_state_0.pth", "x"),
		src("trainer_state.json", "{}"),
		src("training_args.bin", "x"),
		src("logs/train.log", "x"),
		src("assets/logo.png", "x"),
		src("gen.bin.pyc", "x"),
	})
	if len(c.entries) != 0 {
		t.Fatalf("training artifacts must all be skipped, got %+v", c.entries)
	}
	if c.skipped != 9 {
		t.Fatalf("skipped %d want 9", c.skipped)
	}
}

func TestQuantChain(t *testing.T) {
	cases := []struct {
		name   string
		config string
		header string
		want   string
	}{
		{"toy-q8_0.gguf", "", "", "q8_0"},
		{"big-ud-q4_k_m-00001-of-00002.gguf", "", "", "ud-q4_k_m"},
		{"model-00001-of-00002.safetensors", `{"quantization_config":{"quant_method":"fp8"}}`, "", "fp8"},
		{"model.safetensors", `{"quantization_config":{"quant_method":"gptq","bits":4}}`, "", "raw"}, // out of vocab → raw
		{"model.safetensors", "", `{"w":{"dtype":"BF16","data_offsets":[0,400]},"b":{"dtype":"F32","data_offsets":[400,500]}}`, "bf16"},
		{"model.safetensors", "", `{"w":{"dtype":"I8","data_offsets":[0,100]}}`, "raw"},
		{"model.safetensors", "", "", "raw"},
	}
	for _, tc := range cases {
		got := DeriveQuant(tc.name, []byte(tc.config), []byte(tc.header), nil)
		if got != tc.want {
			t.Errorf("%s: quant %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestDeterministicTarOrdering(t *testing.T) {
	a := buildDeterministicTar(map[string][]byte{"b.txt": {2}, "a.txt": {1}, "c/d.txt": {3}})
	b := buildDeterministicTar(map[string][]byte{"c/d.txt": {3}, "a.txt": {1}, "b.txt": {2}})
	if string(a) != string(b) {
		t.Fatal("tar not deterministic under input reordering")
	}
}

// A pure-safetensors repo derives its quant from the dominant dtype and
// produces a full artifact with tokenizer.
func TestImportSafetensorsRepo(t *testing.T) {
	store := mustStore(t)
	header := `{"__metadata__":{},"w":{"dtype":"BF16","shape":[4,4],"data_offsets":[0,32]}}`
	// Real safetensors framing: 8-byte little-endian header length.
	var lenPrefix [8]byte
	lenPrefix[0] = byte(len(header))
	var body []byte
	body = append(body, lenPrefix[:]...)
	body = append(body, []byte(header)...)
	body = append(body, make([]byte, 32)...)
	res, err := Import(context.Background(), store, []Source{
		{Name: "model-00001-of-00002.safetensors", Open: func() (io.ReadCloser, error) {
			return io.NopCloser(newStrReader(string(body))), nil
		}},
		src("tokenizer.json", "{}"),
	}, ImportOptions{As: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Quant != "bf16" {
		t.Fatalf("safetensors artifact: %+v", res.Artifacts)
	}
}

func mustStore(t *testing.T) *storeType {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type storeType = cas.Store

func openStore(root string) (*cas.Store, error) { return cas.Open(root) }
