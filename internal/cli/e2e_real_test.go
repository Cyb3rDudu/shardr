package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/api"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// The real-binary E2E (DoD): a REAL llama-server + a REAL GGUF, enabled
// only when the environment provides both. CI runs the stub variant
// (integration_test.go); this gate exists so the proof is reproducible:
//
//	SHARDR_E2E_REAL=1 \
//	SHARDR_LLAMA_SERVER=/path/to/llama-server \
//	SHARDR_E2E_MODEL=/path/to/model.gguf \
//	go test ./internal/cli -run TestRealBinaryE2E -v
//
// Proof log of the strand's run (Qwen3.5-9B-Q6_K, 7.7 GB, llama-server
// from PATH): import 49 s → run ready → /v1/models id = shardr:///… →
// chat completion 200 → SIGTERM exit in 1 s. See .e2e-proofs/.
func TestRealBinaryE2E(t *testing.T) {
	if os.Getenv("SHARDR_E2E_REAL") != "1" {
		t.Skip("real-binary E2E disabled (set SHARDR_E2E_REAL=1, SHARDR_LLAMA_SERVER, SHARDR_E2E_MODEL)")
	}
	model := os.Getenv("SHARDR_E2E_MODEL")
	if model == "" {
		t.Fatal("SHARDR_E2E_MODEL must point at a real .gguf")
	}
	root := t.TempDir()
	t.Setenv("SHARDR_CAS", root)
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	sockDir, _ := os.MkdirTemp(os.TempDir(), "sx")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "hive.sock")
	srv, err := api.New(store, sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	t.Setenv("SHARDR_SOCKET", sock)
	t.Setenv("SHARDR_CONFIG", writeCfg(t, ""))
	t.Setenv("SHARDR_RUNNER_STATE", filepath.Join(t.TempDir(), "instances.json"))

	c, _ := NewClient()
	imp := &bytes.Buffer{}
	if err := ImportLocal(context.Background(), c, []string{model}, "e2e/real", imp); err != nil {
		t.Fatalf("import: %v\n%s", err, imp.String())
	}

	// Derive the quant the importer assigned (raw for uppercase suffixes
	// per 000 App. A — lowercase-only vocabulary).
	var inv struct {
		Namespaces []struct {
			Quants []string `json:"quants"`
		} `json:"namespaces"`
	}
	if err := c.DoJSON(context.Background(), http.MethodGet, "/v1/models", nil, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Namespaces) == 0 || len(inv.Namespaces[0].Quants) == 0 {
		t.Fatal("no quants after import")
	}
	shortRef := "e2e/real:" + inv.Namespaces[0].Quants[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, shortRef, RunOptions{}, out) }()
	endpoint := ""
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if e := runEndpoint(out.String()); e != "" {
			endpoint = e
			break
		}
		select {
		case err := <-done:
			t.Fatalf("run exited early: %v\n%s", err, out.String())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if endpoint == "" {
		t.Fatalf("no endpoint:\n%s", out.String())
	}

	// Model id = canonical reference (002 §6).
	resp, err := http.Get(endpoint + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Models []struct {
			ID string `json:"model"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	canonical := "shardr:///e2e/real:" + inv.Namespaces[0].Quants[0]
	if len(models.Models) == 0 || models.Models[0].ID != canonical {
		t.Fatalf("served id must be the reference: %+v", models.Models)
	}

	// A real chat completion.
	creq, _ := json.Marshal(map[string]any{
		"model":      canonical,
		"messages":   []map[string]string{{"role": "user", "content": "Say OK."}},
		"max_tokens": 48,
	})
	cresp, err := http.Post(endpoint+"/v1/chat/completions", "application/json", bytes.NewReader(creq))
	if err != nil {
		t.Fatal(err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		body := make([]byte, 512)
		n, _ := cresp.Body.Read(body)
		t.Fatalf("chat: %d %s", cresp.StatusCode, body[:n])
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
		Model string `json:"model"`
	}
	json.NewDecoder(cresp.Body).Decode(&chat)
	if len(chat.Choices) == 0 || (chat.Choices[0].Message.Content == "" && chat.Choices[0].Message.ReasoningContent == "") {
		t.Fatal("empty completion")
	}
	answer := strings.TrimSpace(chat.Choices[0].Message.Content + chat.Choices[0].Message.ReasoningContent)
	fmt.Printf("chat answered: %.80s…\n", answer)

	// Proof artifact: EVERYTHING the test asserts must be visible in the
	// artifact (chat status, served model id, response, tok/s) — a green
	// check without recorded evidence is unfalsifiable.
	if p := os.Getenv("SHARDR_E2E_PROOF"); p != "" {
		proof := fmt.Sprintf("e2e real-binary proof (TestRealBinaryE2E)\nref:        %s\nendpoint:   %s\nmodel id:   %s\nchat:       HTTP %d\nanswer:     %.120s\ntok/s:      prompt %.1f / predicted %.1f\nexit:       ",
			shortRef, endpoint, models.Models[0].ID, cresp.StatusCode, answer, chat.Timings.PromptPerSecond, chat.Timings.PredictedPerSecond)
		_ = proof
		os.WriteFile(p+".partial", []byte(proof), 0o644)
		t.Cleanup(func() { os.Remove(p + ".partial") })
	}

	// Clean SIGTERM exit (ctx cancel = the runner's signal path).
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("no clean exit within the 30 s grace")
	}
	fmt.Printf("clean exit after %s\n", time.Since(start).Round(time.Millisecond))
	if p := os.Getenv("SHARDR_E2E_PROOF"); p != "" {
		if b, err := os.ReadFile(p + ".partial"); err == nil {
			os.WriteFile(p, append(b, []byte(fmt.Sprintf("clean after %s\n", time.Since(start).Round(time.Millisecond)))...), 0o644)
		}
	}
}

// runEndpoint extracts the endpoint from the FOREGROUND run output
// ("ready: http://127.0.0.1:N (model id …) — Ctrl-C to stop") — the
// serve format ("  endpoint http://…") is serveEndpoint's job.
func runEndpoint(runOut string) string {
	for _, ln := range strings.Split(runOut, "\n") {
		e := strings.TrimSpace(ln)
		if rest, ok := strings.CutPrefix(e, "ready: "); ok {
			if url, _, found := strings.Cut(rest, " "); found && strings.HasPrefix(url, "http://127.0.0.1:") {
				return url
			}
		}
	}
	return ""
}
