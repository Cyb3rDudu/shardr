package importer

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrSourceNotRegular is the import-boundary violation: something in or at
// the import root is not a regular file (symlink, FIFO, device, socket).
// The import root is a hard boundary — a symlink out of it must never leak
// foreign bytes into the CAS, so detection fails closed before any read.
var ErrSourceNotRegular = errors.New("importer: import root is a hard boundary — source is not a regular file (symlink/fifo/device); refusing fail-open (E_SOURCE_NOT_REGULAR)")

// LocalSources expands the /v1/import/local paths[] into named sources:
// a directory contributes every regular file beneath it (artifact-relative
// name = slash-separated relative path); a plain file contributes its base
// name. Names must not collide — the manifest validation catches that.
//
// Boundary rule: lstat everywhere (never stat-following), and ONLY
// Mode().IsRegular() is admitted. Symlinks — even pointing back inside the
// root — FIFOs, devices and sockets fail the whole expansion with
// ErrSourceNotRegular: nothing is read, no state is touched.
func LocalSources(paths []string) ([]Source, error) {
	var out []Source
	seen := map[string]string{}
	add := func(name, abs string) error {
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("duplicate file name %q from both %s and %s", name, prev, abs)
		}
		seen[name] = abs
		p := abs
		out = append(out, Source{Name: name, Open: func() (io.ReadCloser, error) {
			return os.Open(p)
		}})
		return nil
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		switch {
		case fi.Mode().IsDir():
			if err := walkRoot(p, add); err != nil {
				return nil, err
			}
		case fi.Mode().IsRegular():
			if err := add(filepath.Base(p), p); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: %s", ErrSourceNotRegular, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// walkRoot descends the import root collecting regular files. WalkDir
// already reports entries with lstat semantics (symlinked directories are
// yielded as entries, never descended), so the IsRegular check below is
// the single admission gate.
func walkRoot(root string, add func(name, abs string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s", ErrSourceNotRegular, path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return add(strings.ReplaceAll(rel, string(filepath.Separator), "/"), path)
	})
}
