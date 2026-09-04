package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockDaemon is a capture server on a unix socket: it records the last
// request (path + decoded JSON body) and answers from a per-path stub.
type mockDaemon struct {
	listener net.Listener
	srv      *http.Server

	mu       chan struct{}
	lastPath string
	lastBody map[string]any
}

func newMockDaemon(t *testing.T, handler http.HandlerFunc) *mockDaemon {
	t.Helper()
	// ponytail: short socket path — macOS sun_path caps at 104 bytes
	dir, err := os.MkdirTemp("/tmp", "sx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockDaemon{listener: l, mu: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m.lastBody)
		m.lastPath = r.URL.Path + "?" + r.URL.RawQuery
		handler(w, r)
	})
	m.srv = &http.Server{Handler: mux}
	go m.srv.Serve(l)
	t.Cleanup(func() { m.srv.Close() })
	t.Setenv("SHARDR_SOCKET", sock)
	return m
}

func jobAnswer(w http.ResponseWriter, id, state string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id, "state": state, "kind": "ensure"})
}

// The CLI accepts ns/name:quant short refs and the API sees ONLY the
// canonical URI (000 §2: short form is input sugar, never on the wire).
func TestShortFormCanonicalizesOnTheWire(t *testing.T) {
	var seenPath, seenRef string
	var m *mockDaemon
	m = newMockDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ensure" {
			if m.lastBody != nil {
				seenRef, _ = m.lastBody["ref"].(string)
			}
			seenPath = r.URL.Path
			jobAnswer(w, "j1", "done")
			return
		}
		if r.URL.Path == "/v1/jobs/j1" {
			jobAnswer(w, "j1", "done")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	_ = m
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"unsloth/qwen3.8-27b-gguf:ud-q4_k_m",
		"shardr:///unsloth/qwen3.8-27b-gguf:ud-q4_k_m",
		"ns/name:tag+q8_0",
	} {
		if err := Pull(context.Background(), c, in, io.Discard); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if seenPath != "/v1/ensure" {
			t.Fatalf("ensure not called: %s", seenPath)
		}
		switch in {
		case "unsloth/qwen3.8-27b-gguf:ud-q4_k_m":
			if seenRef != "shardr:///unsloth/qwen3.8-27b-gguf:ud-q4_k_m" {
				t.Fatalf("short form leaked to the API: %q", seenRef)
			}
		case "ns/name:tag+q8_0":
			if seenRef != "shardr:///ns/name:tag+q8_0" {
				t.Fatalf("tag short form leaked: %q", seenRef)
			}
		}
	}
}

// A selector-less ref without a configured default_selector is a loud
// CLI error — the API is never asked (000 §2: no implicit selector).
func TestNoSelectorIsLoud(t *testing.T) {
	called := false
	newMockDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path == "/v1/ensure" || r.URL.Path == "/v1/jobs/j1" {
			jobAnswer(w, "j1", "done")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	c, _ := NewClient()
	err := Pull(context.Background(), c, "ns/name", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "E_NO_SELECTOR") {
		t.Fatalf("selector-less ref must be loud: %v", err)
	}
	if called {
		t.Fatal("API must not be asked for a selector-less ref")
	}
	// With a default_selector configured, the human gets the comfort.
	t.Setenv("SHARDR_CONFIG", writeCfg(t, "[references]\ndefault_selector = \"q8_0\"\n"))
	if err := Pull(context.Background(), c, "ns/name", io.Discard); err != nil {
		t.Fatalf("default_selector must complete the ref: %v", err)
	}
}

// Import bt refuses a missing pin BEFORE the daemon is asked (005 §5: a
// magnet alone is never trusted) and passes the pin through verbatim.
func TestImportBTPinMandatory(t *testing.T) {
	var gotBody map[string]any
	var m *mockDaemon
	m = newMockDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/import/bt" {
			gotBody = m.lastBody
			jobAnswer(w, "j9", "done")
			return
		}
		if r.URL.Path == "/v1/jobs/j9" {
			jobAnswer(w, "j9", "done")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	_ = m
	c, _ := NewClient()
	err := ImportBT(context.Background(), c, "magnet:?xt=urn:btih:abc", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--manifest") {
		t.Fatalf("magnet without pin must be refused with the flag named: %v", err)
	}
	pin := "sha256:" + strings.Repeat("ab", 32)
	if err := ImportBT(context.Background(), c, "magnet:?xt=urn:btih:abc", pin, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotBody["manifestDigest"] != pin {
		t.Fatalf("pin must pass through verbatim: %+v", gotBody)
	}
}

// Job errors pass through UNCHANGED — no code rewriting, no swallowing.
func TestJobErrorPassthrough(t *testing.T) {
	newMockDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ensure" {
			jobAnswer(w, "j1", "done")
			return
		}
		if r.URL.Path == "/v1/jobs/j1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id": "j1", "state": "failed",
				"error": map[string]any{"code": "E_NOT_IMPORTABLE", "message": "record fails validation: X"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	c, _ := NewClient()
	err := Pull(context.Background(), c, "ns/name:q8_0", io.Discard)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("must be an APIError: %T %v", err, err)
	}
	if apiErr.Code != "E_NOT_IMPORTABLE" || !strings.Contains(apiErr.Message, "record fails validation") {
		t.Fatalf("verbatim passthrough broken: %+v", apiErr)
	}
}

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
