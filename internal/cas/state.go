package cas

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// State files under state/ (spec 003 §2): shardhive-local metadata, never
// torrented. Maps are stored as canonical compact JSON — encoding/json sorts
// map keys, so repeated saves of equal content are byte-identical. Digests
// are stored in the canonical protocol form "sha256:<64hex>" (000 §2); keys
// are validated on write (issue #4 hard follow-up: no garbage in state/).
const (
	namespacesFile = "namespaces.json" // ns/name → current index digest
	tagsFile       = "tags.json"       // tag alias → digest
)

// Namespaces returns the namespace → current-index-digest mapping.
func (s *Store) Namespaces() (map[string]string, error) {
	return s.loadMap(namespacesFile)
}

// SetNamespace sets the namespace's current index digest. Key must be a
// well-formed "ns/name" (000 §2), digest may be bare hex or "sha256:"-
// prefixed and is stored canonically prefixed.
func (s *Store) SetNamespace(name, digest string) error {
	if !ref.ValidNamespaceKey(name) {
		return fmt.Errorf("cas: invalid namespace key %q: want ns/name per spec 000", name)
	}
	d, rerr := ref.NormalizeDigest(digest)
	if rerr != nil {
		return fmt.Errorf("cas: invalid digest for %q: %s", name, rerr.Message)
	}
	return s.updateMap(namespacesFile, func(m map[string]string) { m[name] = d })
}

// DeleteNamespace removes a namespace mapping if present.
func (s *Store) DeleteNamespace(name string) error {
	return s.updateMap(namespacesFile, func(m map[string]string) { delete(m, name) })
}

// TagAliases returns the tag-alias → digest mapping.
func (s *Store) TagAliases() (map[string]string, error) {
	return s.loadMap(tagsFile)
}

// SetTagAlias points alias at digest. Alias must satisfy the tag shape and
// ban rules (000 §2, §3.4); digest is normalized like SetNamespace.
func (s *Store) SetTagAlias(alias, digest string) error {
	if err := ref.ValidTag(alias); err != nil {
		return fmt.Errorf("cas: invalid tag alias %q: %s", alias, err.Message)
	}
	d, rerr := ref.NormalizeDigest(digest)
	if rerr != nil {
		return fmt.Errorf("cas: invalid digest for tag %q: %s", alias, rerr.Message)
	}
	return s.updateMap(tagsFile, func(m map[string]string) { m[alias] = d })
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

// atomicWrite writes data to path via temp file + fsync + rename + parent
// dir fsync (crash durability: the rename itself survives).
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory, making prior renames/creations inside it
// durable across crashes. On Linux/macOS fsync on a directory fd is valid;
// errors other than "not supported" propagate (issue #4 hard follow-up).
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some filesystems reject directory fsync; blob durability is the
		// digest's job, this is best-effort crash hygiene.
		return fmt.Errorf("cas: fsync dir %s: %w", path, err)
	}
	return nil
}

// stateDigestHex strips the canonical "sha256:" prefix for CAS-internal
// lookups (BlobPath/Has use bare hex).
func stateDigestHex(d string) string {
	return strings.TrimPrefix(d, ref.DigestSchemePrefix)
}
