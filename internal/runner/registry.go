package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
)

// Instance is one registered serve entry (002 §4): a daemonized
// llama-server with a stable id, its ref, port, and pid.
type Instance struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"` // canonical shardr:/// URI
	Endpoint string `json:"endpoint"`
	PID      int    `json:"pid"`
	Started  string `json:"started"` // RFC3339
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

// List returns all registered instances, sorted by id.
func (r *Registry) List() ([]Instance, error) {
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("E_STATE: read %s: %w", r.path, err)
	}
	var list []Instance
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("E_STATE: parse %s: %w", r.path, err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// Add registers an instance; an existing id is a loud error (stable ids
// are stable).
func (r *Registry) Add(in Instance) error {
	list, err := r.List()
	if err != nil {
		return err
	}
	for _, e := range list {
		if e.ID == in.ID {
			return fmt.Errorf("E_STATE: serve id %q already running (%s) — pick another --id or stop it first", in.ID, in.Endpoint)
		}
	}
	list = append(list, in)
	return r.write(list)
}

// Remove drops an instance by id (stop path).
func (r *Registry) Remove(id string) error {
	list, err := r.List()
	if err != nil {
		return err
	}
	out := list[:0]
	for _, e := range list {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return r.write(out)
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

func (r *Registry) write(list []Instance) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("E_STATE: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("E_STATE: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, r.path)
}
