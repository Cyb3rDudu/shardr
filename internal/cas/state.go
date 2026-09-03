package cas

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// State files under state/ (spec 003 §2): shardhive-local metadata, never
// torrented. Maps are stored as canonical compact JSON — encoding/json sorts
// map keys, so repeated saves of equal content are byte-identical. Digests
// are stored in the canonical protocol form "sha256:<64hex>" (000 §2); keys
// are validated on write (issue #4 hard follow-up: no garbage in state/).
const (
	namespacesFile   = "namespaces.json"   // ns/name → current index digest
	tagsFile         = "tags.json"         // tag alias → digest
	distributionFile = "distribution.json" // manifest digest → distribution record digest (swarm binding link)
	hintsFile        = "swarm-hints.json"  // manifest digest → source-hint JSON (untrusted operational data, 004 §4)
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

// TagAliases returns the scoped tag mapping. Keys are "ns/name:tag"
// (R3, 000 §3.4: tags are scoped per repository).
func (s *Store) TagAliases() (map[string]string, error) {
	return s.loadMap(tagsFile)
}

// DistributionLinks returns the manifest-digest → distribution-record
// digest mapping (the swarm binding link: given a local manifest, which
// record blob pins its torrent identity — 001 §6). Keys and values are
// canonical "sha256:<hex>".
func (s *Store) DistributionLinks() (map[string]string, error) {
	return s.loadMap(distributionFile)
}

// SetDistributionLink links a manifest digest to its distribution record
// digest. Both must be canonical digests; pass empty record to unlink.
func (s *Store) SetDistributionLink(manifestDigest, recordDigest string) error {
	md, merr := ref.NormalizeDigest(manifestDigest)
	if merr != nil {
		return fmt.Errorf("cas: distribution link: manifest digest: %s", merr.Message)
	}
	if recordDigest == "" {
		return s.updateMap(distributionFile, func(m map[string]string) { delete(m, md) })
	}
	rd, rerr := ref.NormalizeDigest(recordDigest)
	if rerr != nil {
		return fmt.Errorf("cas: distribution link: record digest: %s", rerr.Message)
	}
	return s.updateMap(distributionFile, func(m map[string]string) { m[md] = rd })
}

// HintsFor returns the persisted source-hint JSON for a manifest (""
// when none). Hints are untrusted operational data (trackers, webseeds,
// peers) recorded at import so later ensure-fills reuse them; they never
// participate in identity.
func (s *Store) HintsFor(manifestDigest string) (string, error) {
	md, merr := ref.NormalizeDigest(manifestDigest)
	if merr != nil {
		return "", fmt.Errorf("cas: hints: %s", merr.Message)
	}
	m, err := s.loadMap(hintsFile)
	if err != nil {
		return "", err
	}
	return m[md], nil
}

// SetHints persists source-hint JSON for a manifest ("" removes).
func (s *Store) SetHints(manifestDigest, hintsJSON string) error {
	md, merr := ref.NormalizeDigest(manifestDigest)
	if merr != nil {
		return fmt.Errorf("cas: hints: %s", merr.Message)
	}
	if hintsJSON == "" {
		return s.updateMap(hintsFile, func(m map[string]string) { delete(m, md) })
	}
	return s.updateMap(hintsFile, func(m map[string]string) { m[md] = hintsJSON })
}

// RecordDigestForManifest resolves the linked distribution record digest
// for a manifest ("" when unlinked).
func (s *Store) RecordDigestForManifest(manifestDigest string) (string, error) {
	md, merr := ref.NormalizeDigest(manifestDigest)
	if merr != nil {
		return "", fmt.Errorf("cas: distribution link: %s", merr.Message)
	}
	links, lerr := s.loadMap(distributionFile)
	if lerr != nil {
		return "", lerr
	}
	return links[md], nil
}

// SetTagAlias points the repository-scoped tag (nsName:tag) at digest.
// nsName must be a well-formed "ns/name", tag must satisfy the tag shape
// and ban rules (000 §2, §3.4); digest is normalized like SetNamespace.
func (s *Store) SetTagAlias(nsName, tag, digest string) error {
	if !ref.ValidNamespaceKey(nsName) {
		return fmt.Errorf("cas: invalid namespace key %q: want ns/name per spec 000", nsName)
	}
	if err := ref.ValidTag(tag); err != nil {
		return fmt.Errorf("cas: invalid tag alias %q: %s", tag, err.Message)
	}
	d, rerr := ref.NormalizeDigest(digest)
	if rerr != nil {
		return fmt.Errorf("cas: invalid digest for tag %q: %s", tag, rerr.Message)
	}
	key := nsName + ":" + tag
	return s.updateMap(tagsFile, func(m map[string]string) { m[key] = d })
}

// DeleteTagAlias removes a repository-scoped tag alias if present.
func (s *Store) DeleteTagAlias(nsName, tag string) error {
	return s.updateMap(tagsFile, func(m map[string]string) { delete(m, nsName+":"+tag) })
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

// stateMu serializes the load-mutate-save cycle in updateMap. The
// ponytail note about a single daemon writer is no longer the whole truth
// (the API serves concurrent ensures), so the chain is atomic in-process.
// ponytail: file locking still needed only if multiple processes write.
var stateMu sync.Mutex

// updateMap loads, mutates, and atomically saves under stateMu, making the
// load-mutate-save chain atomic against concurrent updates within this
// process.
func (s *Store) updateMap(name string, mutate func(map[string]string)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
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
