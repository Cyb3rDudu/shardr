package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/config"
)

var stubPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shardr-stub-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	stubPath = filepath.Join(dir, "stub-llama-server")
	cmd := exec.Command("go", "build", "-o", stubPath, "./testdata/stub")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build stub:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func weightsFile(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "weights.gguf")
	if err := os.WriteFile(p, bytes.Repeat([]byte{0xAA}, int(size)), 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

func argvFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "argv.json")
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	json.Unmarshal(b, &args)
	return args
}

// Lifecycle contract (002 §5.3): spawn with weights as CAS path, health,
// readiness = first 200 /v1/models, clean SIGTERM exit well within 30 s.
func TestLifecycleSpawnReadyTerminate(t *testing.T) {
	w := weightsFile(t, 4096)
	t.Setenv("STUB_ARGV_FILE", argvFile(t))
	rt, err := Spawn(SpawnRequest{
		Binary: stubPath, Weights: w,
		Stdout: os.Stderr, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Readiness contract (002 §5.3): /v1/models lists the model with the
	// id derived from -m (the stub's contract surface; the real runtime
	// maps it to the reference).
	resp, err := http.Get(rt.Endpoint() + "/v1/models")
	if err != nil {
		rt.Terminate()
		t.Fatal(err)
	}
	var models struct {
		Models []struct {
			ID string `json:"model"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	if len(models.Models) != 1 || models.Models[0].ID != w {
		t.Fatalf("/v1/models must serve the -m model: %+v", models.Models)
	}
	// Chat answers.
	cresp, err := http.Post(rt.Endpoint()+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		rt.Terminate()
		t.Fatal(err)
	}
	cresp.Body.Close()
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("chat completion: %d", cresp.StatusCode)
	}
	// SIGTERM discipline: exits cleanly within the grace window.
	start := time.Now()
	if err := rt.Terminate(); err != nil {
		t.Fatal(err)
	}
	<-rt.exited
	if d := time.Since(start); d > TerminateGrace {
		t.Fatalf("clean exit took %s (> %s)", d, TerminateGrace)
	}
}

// Zero-copy is law (002 §4): the runner passes the CAS path via -m; no
// copy of the weights ever appears. Proof: argv carries the CAS path
// AND the source file's directory gains nothing.
func TestZeroCopyWeightsPath(t *testing.T) {
	w := weightsFile(t, 1<<20) // 1 MiB marker file
	dirBefore := dirUsage(t, filepath.Dir(w))
	af := argvFile(t)
	t.Setenv("STUB_ARGV_FILE", af)
	rt, err := Spawn(SpawnRequest{
		Binary: stubPath, Weights: w, Argv: []string{"-c", "4096"},
		Stdout: os.Stderr, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dirAfter := dirUsage(t, filepath.Dir(w)); dirAfter != dirBefore {
		t.Fatalf("disk usage changed around the weights: %d → %d (a copy happened?)", dirBefore, dirAfter)
	}
	args := readArgv(t, af)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-m "+w) {
		t.Fatalf("argv must pass the CAS path via -m: %s", joined)
	}
	if !strings.Contains(joined, "127.0.0.1") || !strings.Contains(joined, "--host") {
		t.Fatalf("argv must bind loopback: %s", joined)
	}
}

func dirUsage(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// SIGTERM deadline (002 §4 supervisor duty): a process that ignores
// SIGTERM is SIGKILLed after the grace window — never waited forever.
func TestTerminateDeadlineSIGKILL(t *testing.T) {
	restore := shortGrace(t, 1500*time.Millisecond)
	defer restore()
	w := weightsFile(t, 128)
	t.Setenv("STUB_IGNORE_SIGTERM", "1") // deadbeat mode BEFORE spawn (inherited env)
	rt, err := Spawn(SpawnRequest{
		Binary: stubPath, Weights: w,
		Stdout: os.Stderr, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.WaitReady(context.Background()); err != nil {
		rt.Terminate()
		t.Fatal(err)
	}
	start := time.Now()
	if err := TerminatePID(rt.PID()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > TerminateGrace+5*time.Second {
		t.Fatalf("SIGKILL enforcement took %s — deadline broken", d)
	}
	select {
	case <-rt.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process still alive after TerminatePID returned")
	}
}

// A spawned-but-dead child (bad weights path) surfaces as a loud
// startup error, not a hang.
func TestSpawnBadWeightsFailsFast(t *testing.T) {
	rt, err := Spawn(SpawnRequest{
		Binary: stubPath, Weights: filepath.Join(t.TempDir(), "missing.gguf"),
		Stdout: os.Stderr, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err == nil {
		t.Fatal("startup against a missing weights file must fail loudly")
	}
}

// Registry: add/list/find/remove, duplicate id loud.
func TestRegistryRoundtrip(t *testing.T) {
	reg := OpenRegistryAt(filepath.Join(t.TempDir(), "instances.json"))
	if err := reg.Add(Instance{ID: "a", Ref: "shardr:///x/y:q8_0", Endpoint: "http://127.0.0.1:1", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Instance{ID: "a", Ref: "shardr:///x/y:q8_0", Endpoint: "http://127.0.0.1:2", PID: 2}); err == nil {
		t.Fatal("duplicate id must be loud")
	}
	if err := reg.Add(Instance{ID: "b", Ref: "shardr:///x/z:q8_0", Endpoint: "http://127.0.0.1:3", PID: 3}); err != nil {
		t.Fatal(err)
	}
	list, err := reg.List()
	if err != nil || len(list) != 2 || list[0].ID != "a" {
		t.Fatalf("list: %v %v", list, err)
	}
	in, err := reg.Find("b")
	if err != nil || in.PID != 3 {
		t.Fatalf("find: %+v %v", in, err)
	}
	if _, err := reg.Find("nope"); err == nil {
		t.Fatal("unknown id must be loud")
	}
	if err := reg.Remove("a"); err != nil {
		t.Fatal(err)
	}
	list, _ = reg.List()
	if len(list) != 1 || list[0].ID != "b" {
		t.Fatalf("remove: %+v", list)
	}
}

// Binary resolution order: $SHARDR_LLAMA_SERVER wins over everything;
// a set-but-missing path is a loud error with the install hint.
func TestResolveBinaryPrecedence(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "llama-server")
	os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("SHARDR_LLAMA_SERVER", fake)
	got, err := ResolveBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != fake {
		t.Fatalf("env must win: %s", got)
	}
	t.Setenv("SHARDR_LLAMA_SERVER", filepath.Join(t.TempDir(), "missing"))
	if _, err := ResolveBinary(); err == nil || !strings.Contains(err.Error(), "SHARDR_LLAMA_SERVER") {
		t.Fatalf("missing env binary must be loud with the variable named: %v", err)
	}
	t.Setenv("SHARDR_LLAMA_SERVER", "")
	t.Setenv("PATH", t.TempDir()) // no llama-server anywhere (machine may have one)
	if _, err := ResolveBinary(); err == nil || !strings.Contains(err.Error(), "make llama") {
		t.Fatalf("no-binary error must carry the install hint: %v", err)
	}
}

// Advisory parsing: flat JSON scalars; garbage is loud.
func TestParseScalars(t *testing.T) {
	v, err := ParseScalars([]byte(`{"ctx_size": 4096, "jinja": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if v["ctx_size"].Int != 4096 || !v["jinja"].Bool {
		t.Fatalf("%+v", v)
	}
	if _, err := ParseScalars([]byte(`{"a": [1]}`)); err == nil {
		t.Fatal("arrays must be loud")
	}
	if _, err := ParseScalars([]byte(`[]`)); err == nil {
		t.Fatal("non-object must be loud")
	}
}

// UserConfigOverlay: per-model table beats [runtimes.llama]; both feed
// layer 2.
func TestUserConfigOverlay(t *testing.T) {
	f := config.File{
		"runtimes.llama":                 {"n_threads": {Kind: config.KindInt, Int: 8}},
		`models."ns/name:q8_0".llama`:    {"n_threads": {Kind: config.KindInt, Int: 4}},
		`models."other/repo:q8_0".llama`: {"ctx_size": {Kind: config.KindInt, Int: 111}},
	}
	got := UserConfigOverlay(f, "llama", "ns/name:q8_0")
	if got["n_threads"].Int != 4 {
		t.Fatalf("per-model table must win: %+v", got)
	}
	got2 := UserConfigOverlay(f, "llama", "third/repo:q8_0")
	if got2["n_threads"].Int != 8 {
		t.Fatalf("runtime-global must apply: %+v", got2)
	}
}

// C4: identity is re-checked IMMEDIATELY BEFORE SIGKILL. A verifier that
// flips to false during the grace window (pid died and was recycled)
// must abort WITHOUT the kill — the deadbeat process survives and the
// error names the refusal.
func TestTerminateVerifiedRefusesKillOnIdentityFlip(t *testing.T) {
	restore := shortGrace(t, 1500*time.Millisecond)
	defer restore()
	w := weightsFile(t, 128)
	t.Setenv("STUB_IGNORE_SIGTERM", "1") // deadbeat: ignores SIGTERM
	rt, err := Spawn(SpawnRequest{Binary: stubPath, Weights: w, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := rt.PID()
	termCalls := 0
	killCalls := 0
	verifyTerm := func() bool { termCalls++; return true }
	verifyKill := func() bool { killCalls++; return false } // identity flipped mid-window
	err = TerminateVerified(pid, verifyTerm, verifyKill)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("identity flip during grace must abort with a refusal: %v", err)
	}
	if termCalls != 1 || killCalls != 1 {
		t.Fatalf("verifiers must each run exactly once (TERM %d, KILL %d)", termCalls, killCalls)
	}
	// The deadbeat process must STILL be alive — no SIGKILL landed.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("SIGKILL landed despite failed identity verification")
	}
	rt.Terminate() // cleanup without verifier
}

// C4: identity checked before SIGTERM — a failing first check never
// signals at all.
func TestTerminateVerifiedRefusesBeforeSIGTERM(t *testing.T) {
	w := weightsFile(t, 128)
	rt, err := Spawn(SpawnRequest{Binary: stubPath, Weights: w, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := rt.PID()
	err = TerminateVerified(pid, func() bool { return false }, nil)
	if err == nil || !strings.Contains(err.Error(), "before SIGTERM") {
		t.Fatalf("first-check failure must refuse with the named stage: %v", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("process was signaled despite failed verification")
	}
}

// Boot identity is part of the token (pid reuse across reboots).
func TestStartTokenCarriesBootIdentity(t *testing.T) {
	tok, err := ProcessStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tok, ":") {
		t.Fatalf("token must be prefixed by platform+boot identity: %q", tok)
	}
	// Same pid twice → same token (stable within a boot).
	tok2, _ := ProcessStartToken(os.Getpid())
	if tok != tok2 {
		t.Fatalf("token must be stable: %q vs %q", tok, tok2)
	}
}

// B2 token precision (darwin source): two children spawned within the
// SAME wall-clock second must carry DISTINCT tokens — the old ps lstart
// token was second-granular and collapsed them. Also: self token stable.
func TestStartTokenPrecision(t *testing.T) {
	self, err := ProcessStartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(self, ".") {
		t.Fatalf("darwin token must be microsecond-precise (sec.usec): %q", self)
	}
	again, _ := ProcessStartToken(os.Getpid())
	if self != again {
		t.Fatalf("token must be stable: %q vs %q", self, again)
	}
	spawn := func() *exec.Cmd {
		cmd := exec.Command("sleep", "5")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	// Distinctness is the DARWIN source's property (proc_pidinfo,
	// microsecond resolution — the round-3 order targets exactly that).
	// Linux /proc starttime has 10 ms jiffy granularity: two rapid
	// spawns legitimately share a tick; the GUARD compares the same pid
	// against itself (recycle within the same jiffy AND boot is
	// practically excluded), so the coarser resolution stays sound.
	if runtime.GOOS != "darwin" {
		t.Skip("distinctness assertion targets the darwin microsecond source (linux jiffies are 10 ms by design)")
	}
	a, b := spawn(), spawn() // same wall-clock second
	defer a.Process.Kill()
	defer b.Process.Kill()
	ta, err := ProcessStartToken(a.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := ProcessStartToken(b.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if ta == tb {
		t.Fatalf("same-second children must have distinct start tokens (precision broken): %q", ta)
	}
}

// B2 zombie contract: a process that closed its listener after SIGTERM
// and hangs in shutdown must receive its SIGKILL WITHIN the grace —
// the pre-kill identity check is endpoint-free by design. Short grace
// via the test hook keeps this fast.
func TestZombieGetsSIGKILLWithinGrace(t *testing.T) {
	w := weightsFile(t, 128)
	t.Setenv("STUB_ZOMBIE", "1")
	restore := shortGrace(t, 1500*time.Millisecond)
	defer restore()
	rt, err := Spawn(SpawnRequest{Binary: stubPath, Weights: w, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Terminate()
	if err := rt.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	pid := rt.PID()
	token, err := ProcessStartToken(pid)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	// verifyTerm like Stop: token + /v1/models probe against the live
	// endpoint (still up pre-TERM). verifyKill: endpoint-free token —
	// the zombie's listener is CLOSED by the time it runs.
	endpoint := rt.Endpoint()
	err = TerminateVerified(pid,
		func() bool { return StartIdentityOK(pid, token) && probeID(endpoint) },
		func() bool { return StartIdentityOK(pid, token) },
	)
	if err != nil {
		t.Fatalf("zombie must be killed, not refused: %v", err)
	}
	d := time.Since(start)
	if d > TerminateGrace+2*time.Second {
		t.Fatalf("kill took %s — grace window broken", d)
	}
	// SIGKILL is asynchronous — poll for reaping instead of a single
	// check that races the kernel.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // reaped
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("zombie still alive after TerminateVerified + SIGKILL")
}

// probeID mirrors the endpoint factor of Stop's pre-TERM check.
func probeID(endpoint string) bool {
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, endpoint+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// shortGrace narrows TerminateGrace for one test (round-3 warning 5:
// termination tests ran ~30 s each; production default stays 30 s).
func shortGrace(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := TerminateGrace
	TerminateGrace = d
	return func() { TerminateGrace = old }
}
