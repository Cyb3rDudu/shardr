package cas

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State files under state/ (spec 003 §2): shardhive-local metadata, never
// torrented. Maps are stored as canonical compact JSON — encoding/json sorts
// map keys, so repeated saves of equal content are byte-identical.
const (
	namespacesFile = "namespaces.json" // ns/name → current index digest
	tagsFile       = "tags.json"       // tag alias → digest
)

// Namespaces returns the namespace → current-index-digest mapping.
func (s *Store) Namespaces() (map[string]string, error) {
	return s.loadMap(namespacesFile)
}

// SetNamespace sets name's current index digest.
func (s *Store) SetNamespace(name, digest string) error {
	return s.updateMap(namespacesFile, func(m map[string]string) { m[name] = digest })
}

// DeleteNamespace removes a namespace mapping if present.
func (s *Store) DeleteNamespace(name string) error {
	return s.updateMap(namespacesFile, func(m map[string]string) { delete(m, name) })
}

// TagAliases returns the tag-alias → digest mapping.
func (s *Store) TagAliases() (map[string]string, error) {
	return s.loadMap(tagsFile)
}

// SetTagAlias points alias at digest.
func (s *Store) SetTagAlias(alias, digest string) error {
	return s.updateMap(tagsFile, func(m map[string]string) { m[alias] = digest })
}

// DeleteTagAlias removes a tag alias if present.
func (s *Store) DeleteTagAlias(alias string) error {
	return s.updateMap(tagsFile, func(m map[string]string) { delete(m, alias) })
}

func (s *Store) loadMap(name string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(s.stateDir(), name))
	if os.IsNotExist(err) {
		return map[string]string{}, nil // empty store state is normal
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("cas: parse %s: %w", name, err)
	}
	return m, nil
}

// updateMap loads, mutates, and atomically saves. Load-modify-save races
// between concurrent updates lose one write; shardhive is a single daemon
// owning state/, so a process-level lock suffices for now.
// ponytail: add file locking if state writers ever run in multiple processes.
func (s *Store) updateMap(name string, mutate func(map[string]string)) error {
	m, err := s.loadMap(name)
	if err != nil {
		return err
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.stateDir(), name), b)
}

// atomicWrite writes data to path via temp file + fsync + rename.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
