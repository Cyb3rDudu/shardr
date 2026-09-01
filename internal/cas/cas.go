// Package cas implements shardhive's content-addressed store as specified
// in docs/specs/003: blob layout, the verifying atomic write path, and
// integrity checking. Trust comes from the digest, never from mode bits.
package cas

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sentinel verification results. Callers map these to exit codes:
// nil → 0, ErrDigestMismatch → 1, ErrBlobMissing → 2.
var (
	ErrDigestMismatch = errors.New("cas: digest mismatch")
	ErrBlobMissing    = errors.New("cas: blob missing")
)

const (
	stalePartAge = 24 * time.Hour
	blobMode     = 0o444
	dirMode      = 0o755
)

// Store is a handle on a CAS root directory. It is safe for concurrent use.
type Store struct {
	Root string
}

// ResolveRoot returns the CAS root per spec 003 §2:
// $SHARDR_CAS, else $XDG_DATA_HOME/shardr/cas, else ~/.local/share/shardr/cas.
func ResolveRoot() (string, error) {
	if r := os.Getenv("SHARDR_CAS"); r != "" {
		return filepath.Abs(r)
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "shardr", "cas"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "shardr", "cas"), nil
}

// Open resolves root (root == "" → ResolveRoot), creates the layout, and
// removes stale incoming parts (mtime > 24 h) per spec 003 §3.
func Open(root string) (*Store, error) {
	if root == "" {
		var err error
		root, err = ResolveRoot()
		if err != nil {
			return nil, err
		}
	}
	s := &Store{Root: root}
	for _, d := range []string{s.blobsDir(), s.incomingDir(), s.stateDir()} {
		if err := os.MkdirAll(d, dirMode); err != nil {
			return nil, err
		}
	}
	if err := s.cleanStaleParts(time.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) blobsDir() string    { return filepath.Join(s.Root, "blobs", "sha256") }
func (s *Store) incomingDir() string { return filepath.Join(s.Root, "incoming") }
func (s *Store) stateDir() string    { return filepath.Join(s.Root, "state") }

// cleanStaleParts removes incoming/*.part files older than maxAge.
func (s *Store) cleanStaleParts(now time.Time) error {
	entries, err := os.ReadDir(s.incomingDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // raced with a concurrent writer's cleanup
			}
			return err
		}
		if now.Sub(info.ModTime()) > stalePartAge {
			if err := os.Remove(filepath.Join(s.incomingDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// validateDigest enforces the canonical form: 64 lowercase hex chars.
func validateDigest(d string) error {
	if len(d) != sha256.Size*2 {
		return fmt.Errorf("cas: invalid digest %q: want %d hex chars", d, sha256.Size*2)
	}
	if _, err := hex.DecodeString(d); err != nil {
		return fmt.Errorf("cas: invalid digest %q: %w", d, err)
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("cas: invalid digest %q: uppercase hex not allowed (digests are canonical lowercase sha256:<hex>)", d)
		}
	}
	return nil
}

// BlobPath returns the CAS path for a digest (does not check existence).
// Digest validation makes path traversal via crafted digests impossible.
func (s *Store) BlobPath(digest string) (string, error) {
	if err := validateDigest(digest); err != nil {
		return "", err
	}
	return filepath.Join(s.blobsDir(), digest[:2], digest[2:]), nil
}

// Has reports whether the blob for digest exists.
func (s *Store) Has(digest string) bool {
	p, err := s.BlobPath(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Open returns a read-only file handle for the blob.
func (s *Store) Open(digest string) (*os.File, error) {
	p, err := s.BlobPath(digest)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Put streams r into the CAS, verifying it against expectedDigest per spec
// 003 §3: write to incoming/<rand>.part while hashing, fsync, chmod 0444,
// atomic rename. Digest mismatch → part deleted, ErrDigestMismatch, and the
// blob is never created. Existing target → idempotent no-op.
func (s *Store) Put(expectedDigest string, r io.Reader) error {
	target, err := s.BlobPath(expectedDigest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.incomingDir(), dirMode); err != nil {
		return err
	}
	part, err := s.newPart()
	if err != nil {
		return err
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(part, h), r); err != nil {
		part.Close()
		os.Remove(part.Name())
		return err
	}
	if err := part.Sync(); err != nil {
		part.Close()
		os.Remove(part.Name())
		return err
	}
	if err := part.Close(); err != nil {
		os.Remove(part.Name())
		return err
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedDigest {
		os.Remove(part.Name()) // never promote unverified bytes
		return fmt.Errorf("%w: got sha256:%s", ErrDigestMismatch, got)
	}

	if _, err := os.Stat(target); err == nil {
		os.Remove(part.Name()) // idempotent: existing blob wins
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		os.Remove(part.Name())
		return err
	}
	if err := os.Chmod(part.Name(), blobMode); err != nil {
		os.Remove(part.Name())
		return err
	}
	// POSIX rename is atomic; a concurrent writer of the same digest either
	// wrote identical bytes or never reached this point. Replacing a 0444
	// target is legal — only directory write permission matters.
	if err := os.Rename(part.Name(), target); err != nil {
		os.Remove(part.Name())
		return err
	}
	// Mode metadata was set (chmod before rename) and the file itself was
	// fsynced pre-rename; syncing the parent directory makes the completed
	// rename + entry durable across crashes (issue #4 hard follow-up).
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

// newPart creates the temp file incoming/<16hex-random>.part.
func (s *Store) newPart() (*os.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	name := filepath.Join(s.incomingDir(), hex.EncodeToString(b[:])+".part")
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// Verify re-hashes the blob for digest and compares (spec 003 §4).
// Returns ErrBlobMissing or ErrDigestMismatch; nil means clean.
func (s *Store) Verify(digest string) error {
	f, err := s.Open(digest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: sha256:%s", ErrBlobMissing, digest)
		}
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%w: sha256:%s hashes to sha256:%s", ErrDigestMismatch, digest, got)
	}
	return nil
}

// VerifyResult is the outcome of a full-store verification. StateErrors
// surfaces state-load failures (e.g. corrupt JSON): a broken state file
// must not abort blob verification, but it also must not silently vanish —
// corrupt state can mask missing references (issue #4 hard follow-up).
type VerifyResult struct {
	Mismatched  []string
	Missing     []string
	StateErrors []string
}

// VerifyAll re-hashes every blob in the store and additionally checks every
// digest referenced by state/ (namespaces, tag aliases) for existence: a walk
// alone cannot notice deletions — "missing" is only decidable against an
// expectation, and state/ is the record of what should exist. Foreign files
// under blobs/sha256/ (e.g. .DS_Store, AppleDouble junk) are skipped, not
// errors — they are not blobs and must not mask verification of real ones.
func (s *Store) VerifyAll() (VerifyResult, error) {
	var res VerifyResult
	err := filepath.WalkDir(s.blobsDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		digest := filepath.Base(filepath.Dir(path)) + d.Name()
		if validateDigest(digest) != nil {
			return nil // not a blob by name: skip
		}
		if vErr := s.Verify(digest); vErr != nil {
			if errors.Is(vErr, ErrDigestMismatch) {
				res.Mismatched = append(res.Mismatched, digest)
			} else {
				return vErr
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	ns, nsErr := s.Namespaces()
	tags, tagsErr := s.TagAliases()
	if nsErr != nil {
		res.StateErrors = append(res.StateErrors, "namespaces: "+nsErr.Error())
	}
	if tagsErr != nil {
		res.StateErrors = append(res.StateErrors, "tags: "+tagsErr.Error())
	}
	refs := map[string]bool{}
	for _, m := range []map[string]string{ns, tags} {
		for _, digest := range m {
			refs[stateDigestHex(digest)] = true
		}
	}
	for digest := range refs {
		if !s.Has(digest) {
			res.Missing = append(res.Missing, digest)
		}
	}
	sort.Strings(res.Missing)
	return res, nil
}
