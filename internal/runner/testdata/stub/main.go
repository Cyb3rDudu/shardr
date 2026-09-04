// Command stub-llama-server is the CI test double for llama-server:
// a tiny Go HTTP server implementing the contract surface the runner
// depends on (002 §5.3): --host/--port/-m/--mmproj argv, /health,
// /v1/models (readiness), /v1/chat/completions (echo). It records its
// argv to $STUB_ARGV_FILE and exits cleanly on SIGTERM/SIGINT (or
// ignores SIGTERM when $STUB_IGNORE_SIGTERM=1, to exercise the SIGKILL
// deadline path).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	host, port, model, mmproj, alias := "127.0.0.1", "0", "", "", ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			i++
			host = args[i]
		case "--port":
			i++
			port = args[i]
		case "-m", "--model":
			i++
			model = args[i]
		case "--mmproj":
			i++
			mmproj = args[i]
		case "--alias":
			i++
			alias = args[i]
		}
	}
	if f := os.Getenv("STUB_ARGV_FILE"); f != "" {
		b, _ := json.Marshal(args)
		os.WriteFile(f, b, 0o600)
	}
	if model != "" {
		if _, err := os.Stat(model); err != nil {
			fmt.Fprintf(os.Stderr, "stub-llama-server: -m %s: %v\n", model, err)
			os.Exit(1)
		}
	}

	// Exit-clean-on-signal discipline (002 §5.3): SIGTERM → clean exit.
	go func() {
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		if os.Getenv("STUB_IGNORE_SIGTERM") == "1" && s == syscall.SIGTERM {
			// Deadbeat mode: never exit — the runner must SIGKILL us.
			fmt.Fprintln(os.Stderr, "stub: ignoring SIGTERM (deadline test)")
			for {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		// The served model id (002 §6): llama-server's /v1/models shape is
		// {"models":[{"model":…}]} — the stub mirrors it so client code
		// works identically against both. --alias carries the reference;
		// fall back to the -m path when absent.
		id := alias
		if id == "" {
			id = model
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q}]}`, id, id)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Unknown request parameters → 400 (002 §6): only the known keys
		// model/messages/stream are accepted; anything else is honest 400.
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
			return
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		for k := range raw {
			switch k {
			case "model", "messages", "stream":
			default:
				http.Error(w, fmt.Sprintf(`{"error":"unknown parameter %q"}`, k), http.StatusBadRequest)
				return
			}
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(b, &body)
		content := "echo"
		for _, m := range body.Messages {
			if m.Role == "user" {
				content = m.Content
			}
		}
		suffix := ""
		if mmproj != "" {
			suffix = " [+mmproj]"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"stub","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q}}]}`,
			body.Model, "echo: "+content+suffix)
	})
	addr := host + ":" + port
	fmt.Fprintf(os.Stderr, "stub-llama-server listening on %s (model %s)\n", addr, model)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		os.Exit(1)
	}
}
