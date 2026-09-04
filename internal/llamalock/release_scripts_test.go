package llamalock

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func sh(t *testing.T, dir string, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSHA256SUMSByteExact: the generated SHA256SUMS is exactly
// "<hex>  <name>\n" per file, sorted — byte-identical on any platform.
func TestSHA256SUMSByteExact(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "b.tar.gz"), filepath.Join(dir, "a.tar.gz")
	os.WriteFile(a, []byte("hello"), 0o644)
	os.WriteFile(b, []byte("world!"), 0o644)
	out, err := sh(t, dir, repoRoot(t)+"/scripts/sha256sums.sh", ".")
	if err != nil {
		t.Fatal(err, out)
	}
	want := "711e9609339e92b03ddc0a211827dba421f38f9ed8b9d806e1ffdd8c15ffa03d  a.tar.gz\n" +
		"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824  b.tar.gz\n"
	if out != want {
		t.Fatalf("SHA256SUMS not byte-exact:\ngot:  %q\nwant: %q", out, want)
	}
}

// TestPublishReleaseNoOverwrite: an existing release identity with
// different bytes must hard-fail; identical bytes are an idempotent noop.
func TestPublishReleaseNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n# fake gh\nexit 0\n"), 0o755)

	tarball := filepath.Join(dir, "shardr-runner_test_darwin_arm64.tar.gz")
	os.WriteFile(tarball, []byte("bundle-bytes-v1"), 0o644)

	// Case 1: release exists + different bytes → exit 1.
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncase \"$1 $2\" in \"release view\") exit 0;; \"release download\") echo old > \"$7\"; exit 0;; *) exit 0;; esac\n"), 0o755)
	if out, err := sh(t, dir, repoRoot(t)+"/scripts/publish-release.sh", "."); err == nil {
		t.Fatalf("existing identity with different bytes must fail, got success: %s", out)
	}

	// Case 2: release exists + identical bytes → idempotent noop (exit 0).
	// The fake gh serves back exactly the freshly generated SHA256SUMS.
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncase \"$1 $2\" in \"release view\") exit 0;; \"release download\") cp ./SHA256SUMS \"$7\"; exit 0;; *) exit 0;; esac\n"), 0o755)
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+":"+path)
	if out, err := sh(t, dir, repoRoot(t)+"/scripts/publish-release.sh", "."); err != nil {
		t.Fatalf("identical bytes must be an idempotent noop, got: %v\n%s", err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := FindRepoRoot()
	if err != nil {
		t.Skipf("not run from repo: %v", err)
	}
	return root
}
