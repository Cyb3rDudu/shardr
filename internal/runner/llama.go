package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// LlamaPin is the llama.cpp release `make llama` builds (strand design
// decision: managed subprocess of a pinned upstream release, no cgo).
const LlamaPin = "v0.3.0"

// TerminateGrace is the supervisor duty window (002 §4/§5.3): SIGTERM,
// clean exit within 30 s, SIGKILL after.
const TerminateGrace = 30 * time.Second

// ReadyTimeout is how long the runner waits for /health + /v1/models
// before declaring the spawn failed.
const ReadyTimeout = 120 * time.Second

// Runtime is one managed llama-server subprocess (002 §4/§5.3).
type Runtime struct {
	cmd    *exec.Cmd
	binary string
	port   int
	exited chan struct{} // closed when Wait returned
	err    error
}

// ResolveBinary finds llama-server: $SHARDR_LLAMA_SERVER > bundled next
// to the shardr executable > $PATH. Without a findable binary the runner
// fails with a distinct, actionable error — llama.cpp is a pinned
// external build, never a cgo embedding (strand design decision v1).
func ResolveBinary() (string, error) {
	if p := os.Getenv("SHARDR_LLAMA_SERVER"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("E_BINARY: $SHARDR_LLAMA_SERVER=%q does not exist", p)
	}
	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), "llama-server")
		if fi, err := os.Stat(bundled); err == nil && !fi.IsDir() {
			return bundled, nil
		}
	}
	if p, err := exec.LookPath("llama-server"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("E_BINARY: no llama-server binary found — set $SHARDR_LLAMA_SERVER, run `make llama` (builds the pinned llama.cpp %s into bin/), or install llama-server on $PATH", LlamaPin)
}

// freePort reserves an ephemeral loopback port (listen :0, close). The
// race window between close and llama-server's bind is inherent and
// narrow; Spawn fails loudly if the child cannot bind.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// SpawnRequest is everything Spawn needs: binary, weights as a CAS path
// (zero-copy mmap, 002 §4 — never a copy), merged-config argv, optional
// mmproj path (aux vision projector), and stdout/stderr sinks.
type SpawnRequest struct {
	Binary         string
	Weights        string   // CAS blob path passed via -m
	Argv           []string // merged-overlay flags (Argv())
	Mmproj         string   // "" = none
	Ref            string   // canonical reference — served as the model id (002 §6)
	Detached       bool     // serve: setsid, parent does not wait
	Stdout, Stderr *os.File
}

// Spawn starts llama-server per the contract (002 §5.3): argv from the
// merged config only, weights as CAS path, 127.0.0.1 bind, auto port.
func Spawn(req SpawnRequest) (*Runtime, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("E_RUNTIME: reserve port: %w", err)
	}
	argv := append([]string{
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"-m", req.Weights,
	}, req.Argv...)
	if req.Ref != "" {
		// Model id = reference (002 §6): the served surface addresses the
		// model by its shardr:/// URI, never by a local path.
		argv = append(argv, "--alias", req.Ref)
	}
	if req.Mmproj != "" {
		argv = append(argv, "--mmproj", req.Mmproj)
	}
	cmd := exec.Command(req.Binary, argv...)
	// Fail-fast WITH reason (002 §5.6): nil sinks would swallow the
	// runtime's startup diagnostics (/dev/null) — foreground runs stream
	// them so a failed spawn says why.
	if req.Stdout == nil {
		req.Stdout = os.Stdout
	}
	if req.Stderr == nil {
		req.Stderr = os.Stderr
	}
	cmd.Stdout, cmd.Stderr = req.Stdout, req.Stderr
	if req.Detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("E_RUNTIME: spawn %s: %w", req.Binary, err)
	}
	rt := &Runtime{cmd: cmd, binary: req.Binary, port: port, exited: make(chan struct{})}
	go func() {
		rt.err = cmd.Wait()
		close(rt.exited)
	}()
	return rt, nil
}

// Port is the bound loopback port.
func (rt *Runtime) Port() int { return rt.port }

// PID is the llama-server process id.
func (rt *Runtime) PID() int { return rt.cmd.Process.Pid }

// Endpoint is the OpenAI-compatible base URL.
func (rt *Runtime) Endpoint() string { return fmt.Sprintf("http://127.0.0.1:%d", rt.port) }

// WaitReady polls /health until 200, then /v1/models until 200 —
// readiness is the first successful /v1/models (002 §5.3).
func (rt *Runtime) WaitReady(ctx context.Context) error {
	deadline := time.Now().Add(ReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rt.exited:
			return fmt.Errorf("E_RUNTIME: llama-server exited during startup: %v", rt.err)
		default:
		}
		if probeOK(ctx, rt.Endpoint()+"/health") && probeOK(ctx, rt.Endpoint()+"/v1/models") {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("E_RUNTIME: llama-server not ready within %s (health/models probes failed)", ReadyTimeout)
}

// Terminate enforces the supervisor duty: SIGTERM, clean exit within
// TerminateGrace, SIGKILL after (002 §4). Idempotent; detached children
// (serve) are terminated via TerminatePID.
func (rt *Runtime) Terminate() error {
	if rt.cmd.Process == nil {
		return nil
	}
	return TerminatePID(rt.cmd.Process.Pid)
}

// TerminatePID sends SIGTERM, waits up to TerminateGrace for the process
// to disappear, then SIGKILLs. This is the stop/stop --all path for
// detached serve instances and the exit path for foreground runs.
func TerminatePID(pid int) error {
	return terminateVerified(pid, nil)
}

// TerminateVerified is the guarded discipline (002 §4): the caller's
// verify callback is checked IMMEDIATELY BEFORE SIGTERM and — after the
// grace window expires — IMMEDIATELY BEFORE SIGKILL. The pid can die
// and be recycled inside the 30 s window; without the second check the
// SIGKILL would land on an unrelated process. A failing verification
// aborts WITHOUT any signal: false means "not provably ours anymore".
func TerminateVerified(pid int, verify func() bool) error {
	return terminateVerified(pid, verify)
}

func terminateVerified(pid int, verify func() bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil // already gone (macOS FindProcess always succeeds; kill below is the real check)
	}
	if verify != nil && !verify() {
		return fmt.Errorf("E_STATE: refusing to signal pid %d — identity verification failed before SIGTERM", pid)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		if isGone(err) {
			return nil
		}
		return fmt.Errorf("E_RUNTIME: SIGTERM %d: %w", pid, err)
	}
	deadline := time.Now().Add(TerminateGrace)
	for time.Now().Before(deadline) {
		if err := p.Signal(syscall.Signal(0)); err != nil && isGone(err) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Grace expired: re-verify identity before the kill. A pid that
	// died mid-window and was reused must never receive SIGKILL.
	if verify != nil && !verify() {
		return fmt.Errorf("E_STATE: refusing to SIGKILL pid %d — identity changed during the %s grace window (pid reuse?)", pid, TerminateGrace)
	}
	if err := p.Kill(); err != nil && !isGone(err) {
		return fmt.Errorf("E_RUNTIME: SIGKILL %d: %w", pid, err)
	}
	return nil
}

func isGone(err error) bool {
	return err == syscall.ESRCH || err == os.ErrProcessDone
}

func probeOK(ctx context.Context, url string) bool {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
