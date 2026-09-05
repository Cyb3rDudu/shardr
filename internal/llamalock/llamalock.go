// Package llamalock is the single source of truth for the llama.cpp
// runtime shardr ships. Everything (Makefile, CI workflows, runner
// diagnostics) reads runtime/llama.lock; there is no second pin.
//
// Owner ruling 2026-09-05: shardr NEVER compiles llama.cpp. The runtime
// is consumed exclusively as upstream prebuilt release binaries,
// digest-pinned per platform. Stability comes from OUR pin (exact bNNNN
// + commit + per-platform asset sha256) plus the E2E gate.
//
// The parser is fail-closed: any deviation from the exact format, any
// non-canonical value, any non-ggml-org URL is a hard error.
package llamalock

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Upstream is the only allowed source origin.
const Upstream = "github.com/ggml-org/llama.cpp"

// RepoSlug is the API path form of Upstream (no scheme).
const RepoSlug = "ggml-org/llama.cpp"

// Path is the lockfile location relative to the repo root.
const Path = "runtime/llama.lock"

// Platforms are the runner platforms shardr ships.
var Platforms = []string{"darwin_arm64", "linux_amd64"}

// MinAge is the community-breakage soak time a b-release must have
// before it may be PINNED (canary may test anything; the stable lock
// only moves to releases at least this old).
const MinAge = 7 * 24 * time.Hour

var (
	stableTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	nightlyRe   = regexp.MustCompile(`^b[0-9]+$`)
	commitRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lockLineRe  = regexp.MustCompile(`^([a-z0-9_]+) = "([^"]*)"$`)
	assetHdrRe  = regexp.MustCompile(`^\[assets\.([a-z0-9_]+)\]$`)
)

// AssetNames maps shardr platforms to upstream asset file names.
var AssetNames = map[string]string{
	"darwin_arm64": "llama-%s-bin-macos-arm64.tar.gz",
	"linux_amd64":  "llama-%s-bin-ubuntu-x64.tar.gz",
}

// Lock is the parsed runtime/llama.lock.
type Lock struct {
	Ref       string           `json:"ref"`
	Commit    string           `json:"commit"`
	Assets    map[string]Asset `json:"assets"`
	UpdatedAt string           `json:"updated_at"`
}

// Asset is one platform's pinned prebuilt binary archive.
type Asset struct {
	Platform string `json:"-"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

// AssetURLFor is the canonical download URL for a ref/platform.
func AssetURLFor(ref, platform string) string {
	return "https://github.com/ggml-org/llama.cpp/releases/download/" + ref + "/" + fmt.Sprintf(AssetNames[platform], ref)
}

// Parse validates and parses lockfile bytes. Fail-closed: every field
// must be present, exactly once, in canonical form; exactly the known
// platforms must be pinned.
func Parse(data []byte) (Lock, error) {
	var lk Lock
	seen := map[string]bool{}
	platform := ""
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := assetHdrRe.FindStringSubmatch(line); m != nil {
			platform = m[1]
			continue
		}
		m := lockLineRe.FindStringSubmatch(line)
		if m == nil {
			return Lock{}, fmt.Errorf("llama.lock:%d: malformed line %q", i+1, line)
		}
		k, v := m[1], m[2]
		if platform != "" {
			if lk.Assets == nil {
				lk.Assets = map[string]Asset{}
			}
			a := lk.Assets[platform]
			a.Platform = platform
			switch k {
			case "url":
				if !validPlatform(platform) {
					return Lock{}, fmt.Errorf("llama.lock:%d: unknown platform %q", i+1, platform)
				}
				if v != AssetURLFor(lk.Ref, platform) {
					return Lock{}, fmt.Errorf("llama.lock:%d: asset url %q is not the canonical ggml-org release URL for %s/%s", i+1, v, lk.Ref, platform)
				}
				a.URL = v
			case "sha256":
				if !sha256Re.MatchString(v) {
					return Lock{}, fmt.Errorf("llama.lock:%d: asset sha256 %q is not 64 lowercase hex", i+1, v)
				}
				a.SHA256 = v
			default:
				return Lock{}, fmt.Errorf("llama.lock:%d: unknown asset key %q", i+1, k)
			}
			lk.Assets[platform] = a
			continue
		}
		if seen[k] {
			return Lock{}, fmt.Errorf("llama.lock:%d: duplicate key %q", i+1, k)
		}
		seen[k] = true
		switch k {
		case "ref":
			if !IsNightly(v) {
				return Lock{}, fmt.Errorf("llama.lock: ref %q is not an exact bNNNN release (the pin is a prebuilt b-release; vX.Y.Z carries no binaries)", v)
			}
			lk.Ref = v
		case "commit":
			if !commitRe.MatchString(v) {
				return Lock{}, fmt.Errorf("llama.lock: commit %q is not a full 40-hex lowercase SHA", v)
			}
			lk.Commit = v
		case "updated_at":
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return Lock{}, fmt.Errorf("llama.lock: updated_at %q is not RFC3339", v)
			}
			lk.UpdatedAt = v
		default:
			return Lock{}, fmt.Errorf("llama.lock:%d: unknown key %q", i+1, k)
		}
	}
	for _, k := range []string{"ref", "commit", "updated_at"} {
		if !seen[k] {
			return Lock{}, fmt.Errorf("llama.lock: missing required key %q", k)
		}
	}
	if len(lk.Assets) != len(Platforms) {
		return Lock{}, fmt.Errorf("llama.lock: need exactly %d asset sections, got %d", len(Platforms), len(lk.Assets))
	}
	for _, p := range Platforms {
		a, ok := lk.Assets[p]
		if !ok || a.URL == "" || a.SHA256 == "" {
			return Lock{}, fmt.Errorf("llama.lock: asset section for %s must set url and sha256", p)
		}
	}
	return lk, nil
}

func validPlatform(p string) bool {
	for _, x := range Platforms {
		if x == p {
			return true
		}
	}
	return false
}

// Format renders a Lock back to canonical lockfile bytes.
func (lk Lock) Format() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "ref = %q\ncommit = %q\nupdated_at = %q\n", lk.Ref, lk.Commit, lk.UpdatedAt)
	for _, p := range Platforms {
		a := lk.Assets[p]
		fmt.Fprintf(&b, "\n[assets.%s]\nurl = %q\nsha256 = %q\n", p, a.URL, a.SHA256)
	}
	return []byte(b.String())
}

// FindRepoRoot walks up from cwd until a go.mod + runtime/llama.lock pair
// exists.
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

// IsNightly reports whether ref is a bNNNN release tag.
func IsNightly(ref string) bool { return nightlyRe.MatchString(ref) }

// IsStable reports whether ref is an exact vX.Y.Z tag (never pinnable:
// stable releases attach no binaries).
func IsStable(ref string) bool { return stableTagRe.MatchString(ref) }

// Decide is the pure update decision for the pin: same ref → no update;
// anything that is not bNNNN → hard error.
func Decide(current, latest string) (update bool, err error) {
	if !IsNightly(latest) {
		return false, fmt.Errorf("refusing pin update to %q: not an exact bNNNN release", latest)
	}
	if latest == current {
		return false, nil
	}
	return true, nil
}

// LSRemote runs git ls-remote against upstream (tag→commit proof).
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

// ResolveTag proves that ref points at commit (direct or peeled ^{}).
func ResolveTag(ctx context.Context, ref string) (commit string, err error) {
	if !IsNightly(ref) && !IsStable(ref) {
		return "", fmt.Errorf("ref %q is neither bNNNN nor vX.Y.Z", ref)
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
	commit = direct
	if peeled != "" {
		commit = peeled
	}
	if !commitRe.MatchString(commit) {
		return "", fmt.Errorf("tag %s does not resolve to a full commit (direct=%q peeled=%q)", ref, direct, peeled)
	}
	return commit, nil
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
		if n, err := strconv.Atoi(ref[1:]); err == nil && n > bestN {
			best, bestN = ref, n
		}
	}
	if best == "" {
		return "", errors.New("no bNNNN tags found upstream")
	}
	return best, nil
}

// Release is the slice of the GitHub release API llamalock needs.
type Release struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name   string `json:"name"`
		Digest string `json:"digest"` // "sha256:…"
	} `json:"assets"`
}

func fetchRelease(ctx context.Context, ref string) (Release, error) {
	url := "https://api.github.com/repos/" + RepoSlug + "/releases/tags/" + ref
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("release api %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("release api %s: HTTP %d", ref, resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("release api %s: %w", ref, err)
	}
	return rel, nil
}

// ReleaseAssets returns the API-provided sha256 digest per shardr
// platform for a b-release (independent truth to cross-check the lock
// and the download against).
func ReleaseAssets(ctx context.Context, ref string) (map[string]string, error) {
	rel, err := fetchRelease(ctx, ref)
	if err != nil {
		return nil, err
	}
	digests := map[string]string{}
	for _, a := range rel.Assets {
		for _, p := range Platforms {
			if a.Name == fmt.Sprintf(AssetNames[p], ref) {
				d := strings.TrimPrefix(a.Digest, "sha256:")
				if !sha256Re.MatchString(d) {
					return nil, fmt.Errorf("release %s: asset %s has no sha256 digest", ref, a.Name)
				}
				digests[p] = d
			}
		}
	}
	if len(digests) != len(Platforms) {
		return nil, fmt.Errorf("release %s: missing prebuilt assets (got %d/%d)", ref, len(digests), len(Platforms))
	}
	return digests, nil
}

// ReleasePublishedAt returns the release timestamp (for the ≥7-day age
// filter on pinning).
func ReleasePublishedAt(ctx context.Context, ref string) (time.Time, error) {
	rel, err := fetchRelease(ctx, ref)
	if err != nil {
		return time.Time{}, err
	}
	return rel.PublishedAt.UTC(), nil
}

// NewestPinnableBRelease returns the newest bNNNN release that is at
// least MinAge old (community soak time) and carries all platform
// assets. b-releases ship ~30/day, so the 7-day window needs a few
// pages of the newest-first release list.
func NewestPinnableBRelease(ctx context.Context, now time.Time) (string, error) {
	for page := 1; page <= 10; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", RepoSlug, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return "", fmt.Errorf("release api: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("release api: HTTP %d", resp.StatusCode)
		}
		var rels []Release
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rels)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("release api: %w", err)
		}
		if len(rels) == 0 {
			break
		}
		for _, rel := range rels {
			if !IsNightly(rel.TagName) {
				continue
			}
			if now.Sub(rel.PublishedAt.UTC()) < MinAge {
				continue // too fresh to pin
			}
			digests := map[string]string{}
			for _, a := range rel.Assets {
				for _, p := range Platforms {
					if a.Name == fmt.Sprintf(AssetNames[p], rel.TagName) {
						digests[p] = strings.TrimPrefix(a.Digest, "sha256:")
					}
				}
			}
			if len(digests) == len(Platforms) {
				return rel.TagName, nil
			}
		}
	}
	return "", fmt.Errorf("no b-release older than %s with a complete asset matrix found", MinAge)
}

// DownloadAsset fetches the pinned prebuilt archive for platform,
// streams it through SHA-256 (fail-closed on digest mismatch — no
// unverified bytes on disk), writes it to dest and returns the extract
// root directory name inside the archive.
func DownloadAsset(ctx context.Context, lk Lock, platform, dest string) error {
	a, ok := lk.Assets[platform]
	if !ok {
		return fmt.Errorf("no pinned asset for %s", platform)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", a.URL, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAssetBytes+1)); err != nil {
		f.Close()
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	f.Close()
	got := hex.EncodeToString(h.Sum(nil))
	if got != a.SHA256 {
		os.Remove(dest)
		return fmt.Errorf("E_DIGEST: asset sha256 %s != pinned %s for %s", got, a.SHA256, platform)
	}
	return nil
}

// maxAssetBytes bounds asset downloads (real archives are ~10–20 MB).
const maxAssetBytes = 512 << 20

// Now returns the canonical timestamp for updated_at.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// ExtractAsset safely unpacks a verified tar.gz: every entry must live
// under exactly one top-level directory, no absolute paths, no "..",
// no symlink escaping its directory. Returns the extract root dir name.
func ExtractAsset(tarPath, destDir string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("extract: %w", err)
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
			return "", fmt.Errorf("extract: %w", err)
		}
		clean := filepath.ToSlash(filepath.Clean(hdr.Name))
		if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") || clean == ".." {
			return "", fmt.Errorf("extract: unsafe entry %q", hdr.Name)
		}
		head := strings.SplitN(clean, "/", 2)[0]
		if prefix == "" {
			prefix = head
		} else if head != prefix {
			return "", fmt.Errorf("extract: multiple top-level entries (%q vs %q)", head, prefix)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			target := filepath.ToSlash(filepath.Clean(hdr.Linkname))
			if strings.HasPrefix(target, "/") {
				return "", fmt.Errorf("extract: unsafe link %q -> %q", hdr.Name, hdr.Linkname)
			}
		}
		target := filepath.Join(destDir, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("extract: unsupported entry type %q in %q", string(hdr.Typeflag), hdr.Name)
		}
	}
	if prefix == "" {
		return "", errors.New("extract: empty archive")
	}
	return prefix, nil
}
