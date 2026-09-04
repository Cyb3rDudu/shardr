package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/api"
	"github.com/Cyb3rDudu/shardr/internal/cas"
	"github.com/Cyb3rDudu/shardr/internal/importer"
	"github.com/Cyb3rDudu/shardr/internal/runner"
	"syscall"
)

var runnerRegistryAt = runner.OpenRegistryAt

type runnerInstance = runner.Instance

var stubBinary string

// The integration proof (stub variant of the DoD E2E): a real daemon on
// a real socket, a real imported artifact (gold fixture), the stub
// llama-server — serve → ready → chat completion → stop --all with clean
// SIGTERM. Runs in CI without a real llama.cpp build.
func TestServeStopLifecycleAgainstDaemon(t *testing.T) {
	if stubBinary == "" {
		t.Skip("stub not built")
	}
	// Daemon: CAS + API on a short-lived socket.
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
	t.Setenv("SHARDR_LLAMA_SERVER", stubBinary)
	t.Setenv("SHARDR_CONFIG", writeCfg(t, ""))
	stateDir := t.TempDir()
	t.Setenv("SHARDR_RUNNER_STATE", filepath.Join(stateDir, "instances.json"))

	// Import the gold fixture (real artifact, split-GGUF weights).
	sources, err := importer.LocalSources([]string{"../../internal/importer/testdata/gold"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := importer.Import(context.Background(), store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}
	quant := res.Members[0].Quant
	shortRef := "gold/repo:" + quant

	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	if err := Serve(context.Background(), c, shortRef, RunOptions{}, out); err != nil {
		t.Fatalf("serve: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "endpoint http://127.0.0.1:") {
		t.Fatalf("serve output must name the endpoint:\n%s", out.String())
	}

	// Registry carries the instance with the canonical ref.
	regOut := &bytes.Buffer{}
	if err := Status(context.Background(), c, "", regOut); err != nil {
		t.Fatal(err)
	}
	// The instance answers: /v1/models (id = canonical ref, 002 §6) + a
	// chat completion.
	endpoint := serveEndpoint(out.String())
	mresp, err := http.Get(endpoint + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var mlist struct {
		Models []struct {
			ID string `json:"model"`
		} `json:"models"`
	}
	json.NewDecoder(mresp.Body).Decode(&mlist)
	mresp.Body.Close()
	canonical := "shardr:///gold/repo:" + quant
	if len(mlist.Models) == 0 || mlist.Models[0].ID != canonical {
		t.Fatalf("served model id must be the canonical reference: %+v", mlist.Models)
	}
	creq, _ := json.Marshal(map[string]any{
		"model":    "gold/repo:" + quant,
		"messages": []map[string]string{{"role": "user", "content": "hello shardr"}},
	})
	resp, err := http.Post(endpoint+"/v1/chat/completions", "application/json", bytes.NewReader(creq))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&chat)
	resp.Body.Close()
	if len(chat.Choices) == 0 || !strings.Contains(chat.Choices[0].Message.Content, "hello shardr") {
		t.Fatalf("chat echo broken: %+v", chat)
	}

	// stop --all: SIGTERM, registry empty afterwards.
	stopOut := &bytes.Buffer{}
	if err := Stop(context.Background(), c, "", true, stopOut); err != nil {
		t.Fatalf("stop: %v\n%s", err, stopOut.String())
	}
	if !strings.Contains(stopOut.String(), "stopped") {
		t.Fatalf("stop output:\n%s", stopOut.String())
	}
	// Second stop is a no-op with honest output.
	stopOut2 := &bytes.Buffer{}
	if err := Stop(context.Background(), c, "", true, stopOut2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopOut2.String(), "no serve instances") {
		t.Fatalf("second stop:\n%s", stopOut2.String())
	}
}

// Foreground run: ready line, then ctx cancel = clean SIGTERM exit.
func TestRunForegroundLifecycle(t *testing.T) {
	if stubBinary == "" {
		t.Skip("stub not built")
	}
	root := t.TempDir()
	t.Setenv("SHARDR_CAS", root)
	store, _ := cas.Open(root)
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
	t.Setenv("SHARDR_LLAMA_SERVER", stubBinary)
	t.Setenv("SHARDR_CONFIG", writeCfg(t, ""))

	sources, err := importer.LocalSources([]string{"../../internal/importer/testdata/gold"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := importer.Import(context.Background(), store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}

	c, _ := NewClient()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, c, "gold/repo:"+res.Members[0].Quant, RunOptions{}, out)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "ready: http://127.0.0.1:") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("run exited early: %v\n%s", err, out.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !strings.Contains(out.String(), "ready: http://127.0.0.1:") {
		t.Fatalf("no ready line:\n%s", out.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("run did not exit after cancel")
	}
}

// Unknown overlay keys on the REAL path (user config → Merge) are loud
// with layer provenance — CLI level, not just unit level.
func TestRunUnknownConfigKeyIsLoud(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHARDR_CAS", root)
	store, _ := cas.Open(root)
	sockDir, _ := os.MkdirTemp(os.TempDir(), "sx")
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "hive.sock")
	srv, _ := api.New(store, sock)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	t.Setenv("SHARDR_SOCKET", sock)
	t.Setenv("SHARDR_LLAMA_SERVER", stubBinary)
	t.Setenv("SHARDR_CONFIG", writeCfg(t, "[runtimes.llama]\nhorsepower = 11\n"))
	t.Setenv("SHARDR_RUNNER_STATE", filepath.Join(t.TempDir(), "instances.json"))

	sources, _ := importer.LocalSources([]string{"../../internal/importer/testdata/gold"})
	res, err := importer.Import(context.Background(), store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := NewClient()
	err = Serve(context.Background(), c, "gold/repo:"+res.Members[0].Quant, RunOptions{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "E_CONFIG") || !strings.Contains(err.Error(), "user config") {
		t.Fatalf("unknown key must be loud with layer: %v", err)
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: Run writes from its
// goroutine while the test polls String() — the race detector demands
// synchronization (caught in CI, not locally — exactly what it is for).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func serveEndpoint(serveOut string) string {
	for _, ln := range strings.Split(serveOut, "\n") {
		e := strings.TrimSpace(ln)
		e = strings.TrimPrefix(e, "endpoint ")
		if strings.HasPrefix(e, "http://127.0.0.1:") {
			return e
		}
	}
	return ""
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shardr-cli-stub-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	stubBinary = filepath.Join(dir, "stub-llama-server")
	cmd := exec.Command("go", "build", "-o", stubBinary, "../runner/testdata/stub")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build stub:", err)
		stubBinary = ""
	}
	os.Exit(m.Run())
}

// Split GGUFs serve through the ORIGINAL-name hardlink dir: llama.cpp
// discovers siblings only by the <prefix>-0000N-of-0000M.gguf pattern —
// a bare CAS path would load part 1 alone. The gold fixture is 2-part.
func TestSplitGGUFHardlinkNaming(t *testing.T) {
	if stubBinary == "" {
		t.Skip("stub not built")
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
	t.Setenv("SHARDR_LLAMA_SERVER", stubBinary)
	t.Setenv("SHARDR_CONFIG", writeCfg(t, ""))
	af := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("STUB_ARGV_FILE", af)
	t.Setenv("SHARDR_RUNNER_STATE", filepath.Join(t.TempDir(), "instances.json"))

	sources, _ := importer.LocalSources([]string{"../../internal/importer/testdata/gold"})
	res, err := importer.Import(context.Background(), store, sources, importer.ImportOptions{As: "gold/repo"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := NewClient()
	out := &syncBuffer{}
	if err := Serve(context.Background(), c, "gold/repo:"+res.Members[0].Quant, RunOptions{}, out); err != nil {
		t.Fatalf("serve: %v\n%s", err, out.String())
	}
	// The stub recorded its argv: -m must point at the hardlink dir with
	// the ORIGINAL part name, not the bare CAS hex path.
	b, err := os.ReadFile(af)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	json.Unmarshal(b, &argv)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-of-00002.gguf") {
		t.Fatalf("-m must use the original split name (pattern -00001-of-00002.gguf): %s", joined)
	}
	if !strings.Contains(joined, "shardr-split") {
		t.Fatalf("-m must come from the split scratch dir: %s", joined)
	}
	// Zero-copy: the hardlink shares bytes — every scratch entry must be
	// a link (st_nlink > 1), never a copy.
	Stop(context.Background(), c, "", true, io.Discard)
	var mPath string
	for i, a := range argv {
		if a == "-m" && i+1 < len(argv) {
			mPath = argv[i+1]
		}
	}
	splitDir := filepath.Dir(mPath)
	entries, err := os.ReadDir(splitDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("split dir must hold both parts: %v %v", entries, err)
	}
	for _, e := range entries {
		fi, err := os.Lstat(filepath.Join(splitDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.Mode().IsRegular() {
			t.Fatalf("%s must be a regular file (hardlink)", e.Name())
		}
		if nlink := fi.Sys().(*syscall.Stat_t).Nlink; nlink < 2 {
			t.Fatalf("%s is a COPY, not a hardlink (nlink=%d)", e.Name(), nlink)
		}
	}
}

// stop REFUSES to signal when the pid does not serve the registered ref
// (stale entry / pid reuse) — an unrelated process must never get
// SIGTERM+SIGKILL.
func TestStopRefusesForeignPID(t *testing.T) {
	if stubBinary == "" {
		t.Skip("stub not built")
	}
	state := filepath.Join(t.TempDir(), "instances.json")
	t.Setenv("SHARDR_RUNNER_STATE", state)
	// A live "unrelated" process (sleep) wearing the registered pid.
	sleep := exec.Command("sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleep.Process.Kill()
	reg := runnerRegistryAt(state)
	if err := reg.Add(runnerInstance{ID: "victim", Ref: "shardr:///x/y:q8_0", Endpoint: "http://127.0.0.1:1", PID: sleep.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	c, _ := NewClient()
	err := Stop(context.Background(), c, "victim", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing to stop") {
		t.Fatalf("foreign pid must be refused: %v", err)
	}
	// The sleep survived.
	if err := sleep.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("unrelated process was killed — pid-reuse guard broken")
	}
}
