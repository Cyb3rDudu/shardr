//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// removeSplitDir is the FUSED validate-and-delete (round-4 blocker 2):
// the stored path is shape-checked as pure string, then deletion runs
// FD-ANCHORED from the scratch root — every component is opened with
// Openat(O_NOFOLLOW|O_DIRECTORY), leaves are removed via Unlinkat.
// There is no separate validation whose result could age between check
// and act: a component swapped to a symlink in ANY window is refused by
// the kernel (ELOOP) at open time. The launch dir itself is removed
// children-first; symlinks INSIDE it are unlinked as links (never
// followed). No path-based RemoveAll touches registry data anymore.
func removeSplitDir(stored string) error {
	if stored == "" {
		return fmt.Errorf("E_STATE: empty split dir")
	}
	root := splitRoot()
	rel, err := filepath.Rel(root, stored)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("E_STATE: split dir %q is outside the scratch root — left in place, remove manually if stale", stored)
	}
	segs := strings.Split(rel, string(filepath.Separator))
	if len(segs) != 2 || !isHex64(segs[0]) || segs[1] == "" {
		return fmt.Errorf("E_STATE: split dir %q violates the <manifest-hex>/<launch> shape — left in place, remove manually if stale", stored)
	}
	rootFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("E_STATE: open scratch root %s: %w", root, err)
	}
	defer unix.Close(rootFd)
	hexFd, err := unix.Openat(rootFd, segs[0], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("E_STATE: split component %q refused (symlink or not a directory?) — nothing deleted: %w", segs[0], err)
	}
	defer unix.Close(hexFd)
	if err := emptyDirAt(hexFd, segs[1]); err != nil {
		return err
	}
	if err := unix.Unlinkat(hexFd, segs[1], unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("E_STATE: rmdir launch %q: %w", segs[1], err)
	}
	// Best-effort: drop the now-maybe-empty hex parent so scratch trees
	// do not linger after the last launch of a manifest dies. Other
	// live launches keep it alive (ENOTEMPTY → fine, not an error).
	if err := unix.Unlinkat(rootFd, segs[0], unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOTEMPTY) {
		return fmt.Errorf("E_STATE: rmdir hex %q: %w", segs[0], err)
	}
	return nil
}

// emptyDirAt opens name under parentFd (FD-anchored, O_NOFOLLOW — a
// symlink here refuses the whole deletion) and recursively EMPTIES it;
// removing the dir itself stays with the caller (single-owner removal,
// no double-rmdir). os.File.ReadDir operates on the SAME fd via
// getdents — no path is ever resolved against mutable tree state.
func emptyDirAt(parentFd int, name string) error {
	fd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("E_STATE: open %q for removal refused (symlink or gone?): %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("E_STATE: read %q: %w", name, err)
	}
	for _, e := range entries {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		if e.IsDir() {
			if err := emptyDirAt(fd, e.Name()); err != nil {
				return err
			}
			if err := unix.Unlinkat(fd, e.Name(), unix.AT_REMOVEDIR); err != nil {
				return fmt.Errorf("E_STATE: rmdir %q: %w", e.Name(), err)
			}
			continue
		}
		if err := unix.Unlinkat(fd, e.Name(), 0); err != nil {
			return fmt.Errorf("E_STATE: unlink %q: %w", e.Name(), err)
		}
	}
	return nil
}
