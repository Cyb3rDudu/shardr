package importer

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalSources expands the /v1/import/local paths[] into named sources:
// a directory contributes every regular file beneath it (artifact-relative
// name = slash-separated relative path); a plain file contributes its base
// name. Names must not collide — the manifest validation catches that.
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
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			if err := add(filepath.Base(p), p); err != nil {
				return nil, err
			}
			continue
		}
		root := p
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return add(strings.ReplaceAll(rel, string(filepath.Separator), "/"), path)
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
