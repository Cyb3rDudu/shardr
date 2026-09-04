package runner

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Instance is one registered serve entry (002 §4): a daemonized
// llama-server with a stable id, its ref, port, and pid.
type Instance struct {
	ID         string `json:"id"`
	Ref        string `json:"ref"` // canonical shardr:/// URI
	Endpoint   string `json:"endpoint"`
	PID        int    `json:"pid"`
	Started    string `json:"started"`            // RFC3339
	StartToken string `json:"startToken"`         // process start-time identity (pid-reuse guard)
	SplitDir   string `json:"splitDir,omitempty"` // split-GGUF scratch dir (cleanup on last stop)
}

// Registry is the local serve-instance state file: $XDG_RUNTIME_DIR if
// set, else the per-uid tmp fallback (same rules as the daemon socket,
// 005 §3 — macOS has no XDG_RUNTIME_DIR, hence the explicit fallback).
type Registry struct {
	path string
}

// RegistryPath resolves the instances file location.
func RegistryPath() (string, error) {
	if p := os.Getenv("SHARDR_RUNNER_STATE"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		uid := "0"
		if u, err := userID(); err == nil {
			uid = u
		}
		dir = filepath.Join(os.TempDir(), "shardr-"+uid)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "shardr-instances.json"), nil
}

func userID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Uid, nil
}

// OpenRegistry opens (creating nothing yet) the registry at the
// resolved path.
func OpenRegistry() (*Registry, error) {
	p, err := RegistryPath()
	if err != nil {
		return nil, err
	}
	return &Registry{path: p}, nil
}

// OpenRegistryAt opens a registry at an explicit path (tests).
func OpenRegistryAt(path string) *Registry { return &Registry{path: path} }

// Path exposes the state file location (CLI output).
func (r *Registry) Path() string { return r.path }

// List returns all registered instances, sorted by id (lock-free read;
// the write path renames atomically, so a reader sees either the old or
// the new file, never a torn one).
func (r *Registry) List() ([]Instance, error) {
	return r.listLocked()
}

func (r *Registry) listLocked() ([]Instance, error) {
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("E_STATE: read %s: %w", r.path, err)
	}
	var list []Instance
	if err := json.Unmarshal(b, &list); err != nil {
		// Salvage any readable PIDs into the error so live instances stay
		// stoppable manually — a corrupt state file must not strand
		// GB-scale runtimes.
		pids := salvagePIDs(b)
		msg := fmt.Sprintf("E_STATE: parse %s: %v", r.path, err)
		if len(pids) > 0 {
			msg += fmt.Sprintf(" — possible instance pids still running: %v (kill manually if shardr is done; then remove the file)", pids)
		}
		return nil, errors.New(msg)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// Add registers an instance; an existing id is a loud error (stable ids
// are stable). The read-modify-write runs under a cross-process flock —
// two concurrent serve invocations both survive registration (the
// unlocked last-writer-wins silently dropped one before).
func (r *Registry) Add(in Instance) error {
	return r.withLock(func() error {
		list, err := r.listLocked()
		if err != nil {
			return err
		}
		for _, e := range list {
			if e.ID == in.ID {
				return fmt.Errorf("E_STATE: serve id %q already running (%s) — pick another --id or stop it first", in.ID, in.Endpoint)
			}
		}
		list = append(list, in)
		return r.writeLocked(list)
	})
}

// Remove drops an instance by id (stop path) under the same lock.
func (r *Registry) Remove(id string) error {
	return r.withLock(func() error {
		list, err := r.listLocked()
		if err != nil {
			return err
		}
		out := list[:0]
		for _, e := range list {
			if e.ID != id {
				out = append(out, e)
			}
		}
		return r.writeLocked(out)
	})
}

// withLock serializes registry mutations across processes via an flock
// sidecar file (LOCK_EX blocking). ponytail: flock file instead of a
// lock library — syscall.Flock is stdlib on every unix we ship.
// preLockHook fires BEFORE lock acquisition (test seam: a barrier here
// forces concurrent critical sections when the lock is broken, and mere
// serialization when it works — deterministic red/green for the flock).
var preLockHook func()

func (r *Registry) withLock(fn func() error) error {
	if preLockHook != nil {
		preLockHook()
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("E_STATE: %w", err)
	}
	f, err := os.OpenFile(r.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("E_STATE: open lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("E_STATE: flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// Find returns the instance with the given id.
func (r *Registry) Find(id string) (*Instance, error) {
	list, err := r.List()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("E_STATE: no serve instance %q — see `shardr status`", id)
}

func (r *Registry) writeLocked(list []Instance) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("E_STATE: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), filepath.Base(r.path)+".*")
	if err != nil {
		return fmt.Errorf("E_STATE: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("E_STATE: write %s: %w", tmp.Name(), err)
	}
	tmp.Close()
	return os.Rename(tmp.Name(), r.path)
}

var pidRe = regexp.MustCompile(`"pid":\s*(\d+)`)

// salvagePIDs scrapes raw registry bytes for pid numbers (corruption
// recovery aid).
func salvagePIDs(b []byte) []int {
	var out []int
	for _, m := range pidRe.FindAllSubmatch(b, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > 1 {
			out = append(out, n)
		}
	}
	return out
}

// ProcessStartToken returns a pid's process-start identity: Linux reads
// the kernel starttime (field 22 of /proc/<pid>/stat — unique per boot,
// so a recycled pid has a different token), darwin/BSD fall back to ps
// lstart. The token is recorded at serve time and re-derived before
// every signal: a stale registry entry whose pid was reused must NEVER
// be killable (002 §4 supervisor duty cuts both ways).
func ProcessStartToken(pid int) (string, error) {
	// The token binds pid → (boot, start): boot identity survives pid
	// reuse across reboots, start identity within a boot. lstart is only
	// second-granular on darwin — the boot prefix is what makes the
	// token load-bearing despite that.
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// Field 22 after the parenthesized comm (comm may contain spaces).
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 <= len(s) {
			fields := strings.Fields(s[i+2:])
			if len(fields) >= 20 {
				return "linux:" + linuxBootTime() + ":" + fields[19], nil // fields[0] is state (field 3); +19 = field 22
			}
		}
		return "", fmt.Errorf("E_STATE: /proc/%d/stat: unexpected layout", pid)
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("E_STATE: cannot derive start token for pid %d: %v", pid, err)
	}
	return "darwin:" + darwinBootTime() + ":" + strings.Join(strings.Fields(string(out)), " "), nil
}

// linuxBootTime reads the boot epoch from /proc/stat (btime line).
func linuxBootTime() string {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return "?"
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "btime ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "btime "))
		}
	}
	return "?"
}

// darwinBootTime reads kern.boottime (seconds since epoch).
func darwinBootTime() string {
	b, err := syscall.Sysctl("kern.boottime")
	if err != nil || len(b) < 8 {
		return "?"
	}
	var sec int64
	if err := binary.Read(strings.NewReader(b[:8]), binary.LittleEndian, &sec); err != nil {
		return "?"
	}
	return strconv.FormatInt(sec, 10)
}
