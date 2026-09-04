// Package llamalock is the single source of truth for the llama.cpp
// version shardr builds and ships. Everything (Makefile, CI workflows,
// runner diagnostics) reads runtime/llama.lock; there is no second pin.
//
// The parser is fail-closed: any deviation from the exact five-field
// format, any non-canonical value, any non-ggml-org URL is a hard error.
package llamalock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Upstream is the only allowed source origin (rule: HTTPS to the
// expected GitHub organisation, nothing else).
const Upstream = "github.com/ggml-org/llama.cpp"

// Path is the lockfile location relative to the repo root.
const Path = "runtime/llama.lock"

var (
	stableTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	nightlyRe   = regexp.MustCompile(`^b[0-9]+$`)
	commitRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lockLineRe  = regexp.MustCompile(`^([a-z0-9_]+) = "([^"]*)"$`)
)

// Lock is the parsed runtime/llama.lock.
type Lock struct {
	Ref          string
	Commit       string
	SourceURL    string
	SourceSHA256 string
	UpdatedAt    string
}

// SourceURLFor is the canonical archive URL for a commit.
func SourceURLFor(commit string) string {
	return "https://" + Upstream + "/archive/" + commit + ".tar.gz"
}

// Parse validates and parses lockfile bytes. Fail-closed: every field
// must be present, exactly once, in canonical form.
func Parse(data []byte) (Lock, error) {
	var lk Lock
	seen := map[string]bool{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := lockLineRe.FindStringSubmatch(line)
		if m == nil {
			return Lock{}, fmt.Errorf("llama.lock:%d: malformed line %q", i+1, line)
		}
		k, v := m[1], m[2]
		if seen[k] {
			return Lock{}, fmt.Errorf("llama.lock:%d: duplicate key %q", i+1, k)
		}
		seen[k] = true
		switch k {
		case "ref":
			if !stableTagRe.MatchString(v) {
				return Lock{}, fmt.Errorf("llama.lock: ref %q is not an exact vX.Y.Z stable tag (nightly bNNNN never enters the stable lock)", v)
			}
			lk.Ref = v
		case "commit":
			if !commitRe.MatchString(v) {
				return Lock{}, fmt.Errorf("llama.lock: commit %q is not a full 40-hex lowercase SHA", v)
			}
			lk.Commit = v
		case "source_url":
			if v != SourceURLFor(lk.Commit) {
				return Lock{}, fmt.Errorf("llama.lock: source_url %q does not match the canonical ggml-org archive URL for commit %s", v, lk.Commit)
			}
			lk.SourceURL = v
		case "source_sha256":
			if !sha256Re.MatchString(v) {
				return Lock{}, fmt.Errorf("llama.lock: source_sha256 %q is not 64 lowercase hex", v)
			}
			lk.SourceSHA256 = v
		case "updated_at":
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return Lock{}, fmt.Errorf("llama.lock: updated_at %q is not RFC3339", v)
			}
			lk.UpdatedAt = v
		default:
			return Lock{}, fmt.Errorf("llama.lock:%d: unknown key %q", i+1, k)
		}
	}
	for _, k := range []string{"ref", "commit", "source_url", "source_sha256", "updated_at"} {
		if !seen[k] {
			return Lock{}, fmt.Errorf("llama.lock: missing required key %q", k)
		}
	}
	return lk, nil
}

// Format renders a Lock back to canonical lockfile bytes.
func (lk Lock) Format() []byte {
	return []byte(fmt.Sprintf("ref = %q\ncommit = %q\nsource_url = %q\nsource_sha256 = %q\nupdated_at = %q\n",
		lk.Ref, lk.Commit, lk.SourceURL, lk.SourceSHA256, lk.UpdatedAt))
}

// FindRepoRoot walks up from cwd until a go.mod + runtime/llama.lock pair
// exists. Error if none (builds must run from a repo).
func FindRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, Path)); err == nil {
				return dir, nil
			}
			return "", fmt.Errorf("found go.mod at %s but no %s", dir, Path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("runtime/llama.lock not found — run from the shardr repo")
		}
		dir = parent
	}
}

// Load finds and validates runtime/llama.lock from the repo root.
func Load() (Lock, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return Lock{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		return Lock{}, err
	}
	return Parse(data)
}

// RefOf is the convenience accessor (runner diagnostics, Makefile).
func RefOf() string {
	lk, err := Load()
	if err != nil {
		return "unknown"
	}
	return lk.Ref
}

// IsStable reports whether ref is an exact vX.Y.Z release tag.
func IsStable(ref string) bool { return stableTagRe.MatchString(ref) }

// IsNightly reports whether ref is a bNNNN nightly tag.
func IsNightly(ref string) bool { return nightlyRe.MatchString(ref) }

// Decide is the pure update decision for the STABLE channel:
// same ref → no update; a bNNNN ref → hard error (canary channel only).
func Decide(current, latest string) (update bool, err error) {
	if !IsStable(latest) {
		return false, fmt.Errorf("refusing stable-lock update to %q: not an exact vX.Y.Z tag", latest)
	}
	if latest == current {
		return false, nil
	}
	return true, nil
}

// LSRemote runs git ls-remote against upstream and returns its stdout.
// git is the standard tool for tag→commit resolution (deref ^{} included)
// and is present on every build host we use.
func LSRemote(ctx context.Context, patterns ...string) (string, error) {
	args := append([]string{"ls-remote", "https://" + Upstream}, patterns...)
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git ls-remote: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git ls-remote: %w", err)
	}
	return string(out), nil
}

// ResolveTag proves that ref points at commit: it reads both the tag ref
// and its peeled ^{} form. Annotated tags deref to the commit; lightweight
// tags are the commit. Anything else (moving tag, missing) is an error.
func ResolveTag(ctx context.Context, ref string) (commit string, err error) {
	if !IsStable(ref) && !IsNightly(ref) {
		return "", fmt.Errorf("ref %q is neither vX.Y.Z nor bNNNN", ref)
	}
	out, err := LSRemote(ctx, "refs/tags/"+ref, "refs/tags/"+ref+"^{}")
	if err != nil {
		return "", err
	}
	direct, peeled := "", ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[1] {
		case "refs/tags/" + ref:
			direct = f[0]
		case "refs/tags/" + ref + "^{}":
			peeled = f[0]
		}
	}
	if peeled != "" {
		commit = peeled // annotated tag: the commit it points at
	} else {
		commit = direct // lightweight tag: the commit itself
	}
	if !commitRe.MatchString(commit) {
		return "", fmt.Errorf("tag %s does not resolve to a full commit (direct=%q peeled=%q)", ref, direct, peeled)
	}
	return commit, nil
}

// LatestStableTag returns the newest vX.Y.Z tag upstream.
func LatestStableTag(ctx context.Context) (string, error) {
	out, err := LSRemote(ctx, "refs/tags/v*")
	if err != nil {
		return "", err
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasSuffix(f[1], "^{}") {
			continue
		}
		ref := strings.TrimPrefix(f[1], "refs/tags/")
		if IsStable(ref) {
			tags = append(tags, ref)
		}
	}
	if len(tags) == 0 {
		return "", errors.New("no vX.Y.Z tags found upstream")
	}
	sort.Slice(tags, func(i, j int) bool { return tagLess(tags[i], tags[j]) })
	return tags[len(tags)-1], nil
}

// LatestNightlyTag returns the newest bNNNN tag upstream.
func LatestNightlyTag(ctx context.Context) (string, error) {
	out, err := LSRemote(ctx, "refs/tags/b*")
	if err != nil {
		return "", err
	}
	best, bestN := "", -1
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasSuffix(f[1], "^{}") || !strings.HasPrefix(f[1], "refs/tags/b") {
			continue
		}
		ref := strings.TrimPrefix(f[1], "refs/tags/")
		if !IsNightly(ref) {
			continue
		}
		n, err := strconv.Atoi(ref[1:])
		if err == nil && n > bestN {
			best, bestN = ref, n
		}
	}
	if best == "" {
		return "", errors.New("no bNNNN tags found upstream")
	}
	return best, nil
}

// tagLess compares two stable tags numerically per component.
func tagLess(a, b string) bool {
	pa, pb := tagParts(a), tagParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func tagParts(t string) [3]int {
	var p [3]int
	fmt.Sscanf(t, "v%d.%d.%d", &p[0], &p[1], &p[2])
	return p
}

// maxArchiveBytes bounds source downloads (llama.cpp tarballs are well
// below this; growth beyond it means something is wrong — fail closed).
const maxArchiveBytes = 300 << 20

// ArchiveSHA256 downloads the source archive for commit over HTTPS and
// streams it through SHA-256 (never touches disk unverified). The
// digest is returned, not trusted: callers compare against the lock.
func ArchiveSHA256(ctx context.Context, commit string) (string, error) {
	if !commitRe.MatchString(commit) {
		return "", fmt.Errorf("commit %q is not a full 40-hex SHA", commit)
	}
	url := SourceURLFor(commit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, maxArchiveBytes+1)); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Now returns the canonical timestamp for updated_at.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
