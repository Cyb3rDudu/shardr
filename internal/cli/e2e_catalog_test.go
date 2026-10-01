package cli

// The REAL catalog E2E (Epic #65 DoD): pull the smallest listed runnable
// model version through the REAL daemon against the LIVE pirateface
// catalog + Hugging Face anchor, over the real swarm/webseed transport,
// then prove `shardr run` serves it with a real llama-server chat
// completion. Disabled unless explicitly requested — it needs network,
// ~330 MB, and a llama-server:
//
//	SHARDR_CATALOG_E2E=1 \
//	SHARDR_LLAMA_SERVER=/path/to/llama-server \
//	go test ./internal/cli -run TestCatalogRealE2E -v
//
// Verified provider fact (2026-10-02): pirateface listing torrents carry
// exactly ONE weight file each (per-quant torrents, no companions) — the
// bartowski/Qwen2.5-0.5B-Instruct-GGUF listing is the Q4_K_M gguf alone
// (397,808,192 bytes). Its uppercase name derives quant "raw" (000 App.
// A: the vocabulary is lowercase-only), so --quant raw is the matching
// selector.

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
	"github.com/Cyb3rDudu/shardr/internal/catalog"
	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

func TestCatalogRealE2E(t *testing.T) {
	if os.Getenv("SHARDR_CATALOG_E2E") != "1" {
		t.Skip("real catalog E2E disabled (set SHARDR_CATALOG_E2E=1, SHARDR_LLAMA_SERVER; needs network + ~400 MB)")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("SHARDR_LLAMA_SERVER") == "" {
		t.Fatal("SHARDR_LLAMA_SERVER must point at a real llama-server")
	}

	// The real daemon stack: swarm client (DHT on — the magnet's tracker
	// and DHT provide the peers) + API server + real catalog provider.
	cfg := swarm.DefaultConfig()
	cfg.DataRoot = t.TempDir()
	cfg.ListenHost = "127.0.0.1"
	cfg.DisableIPv6 = true // local listeners only; DHT + tracker stay real
	sc, err := swarm.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	sockDir, _ := os.MkdirTemp(os.TempDir(), "sxcfg")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	srv, err := api.New(sc.Store(), filepath.Join(sockDir, "hive.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Swarm = sc
	srv.Catalog = catalog.NewPirateface()
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	t.Setenv("SHARDR_SOCKET", srv.Path())
	t.Setenv("SHARDR_CONFIG", writeCfg(t, ""))
	t.Setenv("SHARDR_RUNNER_STATE", filepath.Join(t.TempDir(), "instances.json"))

	c, _ := NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// ---- The anchored catalog pull (live provider, live HF anchor).
	repo := "bartowski/Qwen2.5-0.5B-Instruct-GGUF"
	pullOut := &bytes.Buffer{}
	start := time.Now()
	if err := CatalogPull(ctx, c, repo, CatalogPullOptions{Quant: "raw"}, pullOut); err != nil {
		t.Fatalf("catalog pull: %v\n%s", err, pullOut.String())
	}
	t.Logf("pull took %s:\n%s", time.Since(start).Round(time.Second), pullOut.String())

	// ---- Resolve the imported quant (the derived ref form).
	var inv struct {
		Namespaces []struct {
			NS     string   `json:"ns"`
			Name   string   `json:"name"`
			Quants []string `json:"quants"`
		} `json:"namespaces"`
	}
	if err := c.DoJSON(ctx, http.MethodGet, "/v1/models", nil, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Namespaces) == 0 {
		t.Fatal("no models after the pull")
	}
	ns := inv.Namespaces[0]
	if ns.NS+"/"+ns.Name != strings.ToLower(repo) {
		t.Fatalf("namespace: %s/%s, want %s", ns.NS, ns.Name, strings.ToLower(repo))
	}
	if len(ns.Quants) != 1 {
		t.Fatalf("--quant raw must import exactly one quant, got %v", ns.Quants)
	}
	shortRef := ns.NS + "/" + ns.Name + ":" + ns.Quants[0]
	t.Logf("imported %s", shortRef)

	// ---- Run it with the real runtime (chat + timings, like the
	// real-binary E2E — a degraded backend cannot fake positive tok/s).
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Run(runCtx, c, shortRef, RunOptions{}, out) }()
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
	creq, _ := json.Marshal(map[string]any{
		"model":      "shardr:///" + shortRef,
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
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Timings struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	json.NewDecoder(cresp.Body).Decode(&chat)
	if len(chat.Choices) == 0 || strings.TrimSpace(chat.Choices[0].Message.Content) == "" {
		t.Fatal("empty completion")
	}
	fmt.Printf("chat answered: %.80s…\n", chat.Choices[0].Message.Content)
	if chat.Timings.PromptPerSecond <= 0 || chat.Timings.PredictedPerSecond <= 0 {
		t.Fatalf("missing positive timings (prompt %.2f / predicted %.2f tok/s) — not a real-runtime proof",
			chat.Timings.PromptPerSecond, chat.Timings.PredictedPerSecond)
	}

	// Proof artifact (same convention as the real-binary E2E).
	if p := os.Getenv("SHARDR_E2E_PROOF"); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("create proof dir: %v", err)
		}
		proof := fmt.Sprintf("catalog real E2E (TestCatalogRealE2E)\nrepo:       %s\nquant:      %s (raw filter)\npull:       %s\nref:        %s\nchat:       HTTP %d\nanswer:     %.120s\ntok/s:      prompt %.1f / predicted %.1f\n",
			repo, ns.Quants[0], time.Since(start).Round(time.Second), shortRef, cresp.StatusCode,
			chat.Choices[0].Message.Content, chat.Timings.PromptPerSecond, chat.Timings.PredictedPerSecond)
		if err := os.WriteFile(p, []byte(proof), 0o644); err != nil {
			t.Fatalf("write proof artifact: %v", err)
		}
	}

	runCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("no clean exit within the 30 s grace")
	}
}
