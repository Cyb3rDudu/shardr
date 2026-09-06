//go:build unix

package llamalock

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ExtractAsset safely unpacks a verified tar.gz into destDir:
//
//  1. extraction happens in a FRESH unpredictable temp dir inside destDir;
//  2. every path component is opened FD-anchored with Openat(O_NOFOLLOW) —
//     a swapped symlink component is refused by the kernel (ELOOP), not by
//     a racy path check (same pattern as internal/cli/rmrf_unix.go);
//  3. entries are only ever created, never followed or overwritten;
//  4. symlink/hardlink targets must resolve INSIDE the extract root;
//  5. the temp dir is renamed into place only after full, error-free
//     processing (atomic landing; failures leave nothing behind).
//
// It returns the single top-level directory name inside destDir.
func ExtractAsset(tarPath, destDir string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(destDir, ".extract-*")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			return "", fmt.Errorf("%w (temp cleanup also failed: %v — %s may linger)", err, rmErr, tmp)
		}
		return "", err
	}
	rootFd, err := unix.Open(tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fail(fmt.Errorf("extract: open temp root: %w", err))
	}
	defer unix.Close(rootFd)

	gz, err := newGzipReader(f)
	if err != nil {
		return fail(fmt.Errorf("extract: %w", err))
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	prefix := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("extract: %w", err))
		}
		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		if err := checkEntryName(clean); err != nil {
			return fail(err)
		}
		head := strings.SplitN(clean, "/", 2)[0]
		if prefix == "" {
			prefix = head
		} else if head != prefix {
			return fail(fmt.Errorf("extract: multiple top-level entries (%q vs %q)", head, prefix))
		}
		if clean == prefix {
			continue // the top-level dir entry itself
		}
		// entries are extracted RELATIVE to the top level; the temp dir
		// is renamed onto destDir/<prefix> after full success
		if err := extractEntryAt(rootFd, strings.TrimPrefix(clean, prefix+"/"), hdr, tr, prefix); err != nil {
			return fail(err)
		}
	}
	if prefix == "" {
		return fail(errors.New("extract: empty archive"))
	}
	// Landing: POSIX cannot atomically rename over a non-empty dir. For a
	// FRESH target the rename is atomic; for an existing one we move the
	// old tree aside first (window = one rename, never a half-written
	// tree — the new dir is always complete when it appears).
	target := filepath.Join(destDir, prefix)
	if fi, err := os.Lstat(target); err == nil && fi.IsDir() {
		old := target + ".old-" + fmt.Sprintf("%d", os.Getpid())
		if err := os.Rename(target, old); err != nil {
			return fail(fmt.Errorf("extract: set aside stale %s: %w", target, err))
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Rename(old, target) // best-effort restore
			return fail(fmt.Errorf("extract: land %s: %w", target, err))
		}
		if err := os.RemoveAll(old); err != nil {
			return fail(fmt.Errorf("extract: cleanup set-aside %s: %w", old, err))
		}
		return prefix, nil
	}
	if err := os.Rename(tmp, target); err != nil {
		return fail(fmt.Errorf("extract: land %s: %w", target, err))
	}
	return prefix, nil
}

// checkEntryName rejects absolute paths, ".." and empty names.
func checkEntryName(clean string) error {
	if clean == "" || clean == "." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(clean, "/../") {
		return fmt.Errorf("extract: unsafe entry %q", clean)
	}
	return nil
}

// checkLinkTarget refuses absolute targets and escapes: the RESOLVED
// target (relative to the link's directory) must stay under the root.
func checkLinkTarget(name, target string) error {
	t := filepath.ToSlash(filepath.Clean(target))
	if strings.HasPrefix(t, "/") {
		return fmt.Errorf("extract: unsafe link %q -> %q (absolute)", name, target)
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), t)))
	if err := checkEntryName(resolved); err != nil {
		return fmt.Errorf("extract: unsafe link %q -> %q (escapes extract root)", name, target)
	}
	return nil
}

// openUnderRoot opens a root-relative path component-wise FD-anchored
// below rootFd (every directory Openat O_NOFOLLOW|O_DIRECTORY, leaf
// O_RDONLY|O_NOFOLLOW). The returned FD is owned by the CALLER.
func openUnderRoot(rootFd int, clean string) (int, error) {
	comps := strings.Split(clean, "/")
	fd := rootFd
	for i, c := range comps {
		var flags int
		if i == len(comps)-1 {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW
		} else {
			flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW
		}
		nfd, err := unix.Openat(fd, c, flags, 0)
		if err != nil {
			if fd != rootFd {
				unix.Close(fd)
			}
			return -1, err
		}
		if fd != rootFd {
			unix.Close(fd)
		}
		fd = nfd
	}
	return fd, nil
}

// extractEntryAt writes ONE entry below rootFd, FD-anchored: every
// directory component is opened with Openat(O_NOFOLLOW|O_DIRECTORY),
// the leaf file is created with O_CREAT|O_EXCL|O_NOFOLLOW. A path whose
// component is (or becomes) a symlink is refused by the kernel.
func extractEntryAt(rootFd int, name string, hdr *tar.Header, tr *tar.Reader, prefix string) error {
	comps := strings.Split(filepath.ToSlash(name), "/")
	base := comps[len(comps)-1]
	if base == "" { // trailing-slash dir entry
		comps, base = comps[:len(comps)-1], comps[len(comps)-2]
	}
	dirFd := rootFd
	closeDir := func() {
		if dirFd != rootFd {
			unix.Close(dirFd)
			dirFd = rootFd
		}
	}
	for _, c := range comps[:len(comps)-1] {
		if err := unix.Mkdirat(dirFd, c, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			closeDir()
			return fmt.Errorf("extract: mkdir %q: %w", c, err)
		}
		nfd, err := unix.Openat(dirFd, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			closeDir()
			return fmt.Errorf("extract: open dir %q refused (symlink component?): %w", c, err)
		}
		closeDir()
		dirFd = nfd
	}
	defer func() {
		if dirFd != rootFd {
			unix.Close(dirFd)
		}
	}()
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := unix.Mkdirat(dirFd, base, uint32(hdr.Mode&0o777)); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("extract: mkdir %q: %w", base, err)
		}
	case tar.TypeReg:
		fd, err := unix.Openat(dirFd, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, uint32(hdr.Mode&0o777|0o400))
		if err != nil {
			return fmt.Errorf("extract: create %q refused (exists or symlink?): %w", base, err)
		}
		out := os.NewFile(uintptr(fd), name)
		_, werr := io.Copy(out, tr)
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("extract: write %q: %w", name, werr)
		}
	case tar.TypeSymlink:
		if err := checkLinkTarget(name, hdr.Linkname); err != nil {
			return err
		}
		if err := unix.Symlinkat(hdr.Linkname, dirFd, base); err != nil {
			return fmt.Errorf("extract: symlink %q: %w", name, err)
		}
	case tar.TypeLink:
		// TAR SEMANTICS: hardlink targets are ARCHIVE-ROOT-relative
		// INCLUDING the top-level prefix ("llama-b1/bin/llama-server"),
		// while entries are extracted prefix-stripped. Require the prefix,
		// strip it, THEN reject absolute targets and any ".." outright,
		// and open the target component-wise FD-anchored below rootFd
		// (never with a path the kernel would walk outside the temp tree).
		target := filepath.ToSlash(filepath.Clean(hdr.Linkname))
		if !strings.HasPrefix(target, prefix+"/") {
			return fmt.Errorf("extract: unsafe hardlink %q => %q (target must be archive-root-relative under %s/)", name, hdr.Linkname, prefix)
		}
		target = strings.TrimPrefix(target, prefix+"/")
		if target == "" || target == "." || strings.HasPrefix(target, "/") || target == ".." || strings.HasPrefix(target, "../") || strings.Contains(target, "/../") {
			return fmt.Errorf("extract: unsafe hardlink %q => %q (must stay under the extract root, no ..)", name, hdr.Linkname)
		}
		tgtFd, err := openUnderRoot(rootFd, target)
		if err != nil {
			return fmt.Errorf("extract: hardlink target %q not opened under root (missing/symlink/escape?): %w", hdr.Linkname, err)
		}
		unix.Close(tgtFd)
		if err := unix.Linkat(rootFd, target, dirFd, base, 0); err != nil {
			return fmt.Errorf("extract: hardlink %q: %w", name, err)
		}
	default:
		return fmt.Errorf("extract: unsupported entry type %q in %q", string(hdr.Typeflag), name)
	}
	return nil
}
