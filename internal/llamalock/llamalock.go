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
	"runtime"
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
// platforms must be pinned; any duplicate key, header or section is a
// hard error (no silent last-one-wins).
func Parse(data []byte) (Lock, error) {
	var lk Lock
	seen := map[string]bool{}
	seenSections := map[string]bool{}
	seenAssetKeys := map[string]map[string]bool{}
	platform := ""
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := assetHdrRe.FindStringSubmatch(line); m != nil {
			platform = m[1]
			if !validPlatform(platform) {
				return Lock{}, fmt.Errorf("llama.lock:%d: unknown platform %q", i+1, platform)
			}
			if seenSections[platform] {
				return Lock{}, fmt.Errorf("llama.lock:%d: duplicate section [assets.%s]", i+1, platform)
			}
			seenSections[platform] = true
			seenAssetKeys[platform] = map[string]bool{}
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
			if seenAssetKeys[platform][k] {
				return Lock{}, fmt.Errorf("llama.lock:%d: duplicate key %q in [assets.%s]", i+1, k, platform)
			}
			seenAssetKeys[platform][k] = true
			a := lk.Assets[platform]
			a.Platform = platform
			switch k {
			case "url":
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

// Decide is the pure update decision for the pin: same ref or an older
// bNNNN → no update (a downgrade needs an explicit --allow-downgrade);
// anything that is not bNNNN → hard error.
func Decide(current, latest string, allowDowngrade bool) (update bool, err error) {
	if !IsNightly(latest) {
		return false, fmt.Errorf("refusing pin update to %q: not an exact bNNNN release", latest)
	}
	if latest == current {
		return false, nil
	}
	if bnum(latest) <= bnum(current) && !allowDowngrade {
		return false, nil // older (or re-pin of same number) — not an update
	}
	return true, nil
}

// bnum extracts the numeric part of a bNNNN ref (0 if malformed).
func bnum(ref string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(ref, "b"))
	return n
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
		Name      string    `json:"name"`
		Digest    string    `json:"digest"` // "sha256:…"
		UpdatedAt time.Time `json:"updated_at"`
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
// least MinAge old and carries all platform assets. Soak age is judged
// by the ASSET updated_at (freshly re-uploaded binaries on an old
// release must not count as aged), not the release published_at.
// b-releases ship ~30/day, so the 7-day window needs a few pages of the
// newest-first list.
func NewestPinnableBRelease(ctx context.Context, now time.Time) (string, error) {
	return newestPinnable(ctx, now, func(page int) string {
		return fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", RepoSlug, page)
	})
}

// ValidatePinnableRelease proves a b-release is pinnable: exact bNNNN
// tag, complete platform asset matrix WITH digests, and the YOUNGEST
// asset updated_at at least MinAge old (soak is judged per asset — a
// freshly re-uploaded binary on an old release does not count as aged).
// BOTH the automatic selection and the manual --ref path go through
// this one function; there is no second, weaker check.
func ValidatePinnableRelease(ctx context.Context, ref string, now time.Time) error {
	if !IsNightly(ref) {
		return fmt.Errorf("%q is not an exact bNNNN release", ref)
	}
	rel, err := fetchRelease(ctx, ref)
	if err != nil {
		return err
	}
	return validatePinnableRel(rel, now)
}

// validatePinnableRel is the pure core of ValidatePinnableRelease.
func validatePinnableRel(rel Release, now time.Time) error {
	if !IsNightly(rel.TagName) {
		return fmt.Errorf("%q is not an exact bNNNN release", rel.TagName)
	}
	_, youngest, ok := releaseAssetMatrix(rel)
	if !ok {
		return fmt.Errorf("release %s: incomplete prebuilt asset matrix", rel.TagName)
	}
	if age := now.Sub(youngest); age < MinAge {
		return fmt.Errorf("release %s: youngest asset is only %s old (soak window %s) — assets updated %s", rel.TagName, age.Truncate(time.Minute), MinAge, youngest.Format(time.RFC3339))
	}
	return nil
}

func newestPinnable(ctx context.Context, now time.Time, pageURL func(int) string) (string, error) {
	for page := 1; page <= 10; page++ {
		rels, err := fetchReleases(ctx, pageURL(page))
		if err != nil {
			return "", err
		}
		if len(rels) == 0 {
			break
		}
		for _, rel := range rels {
			if err := validatePinnableRel(rel, now); err != nil {
				continue // not pinnable (wrong tag, matrix, or soak)
			}
			return rel.TagName, nil
		}
	}
	return "", fmt.Errorf("no b-release older than %s with a complete asset matrix found", MinAge)
}

// releaseAssetMatrix returns the per-platform digests of a release and
// the NEWEST asset updated_at across the required platforms (soak is
// judged by the youngest asset: one freshly re-uploaded binary makes
// the whole release unaged). ok=false if any platform asset (with
// digest) is missing.
func releaseAssetMatrix(rel Release) (digests map[string]string, oldest time.Time, ok bool) {
	digests = map[string]string{}
	for _, a := range rel.Assets {
		for _, p := range Platforms {
			if a.Name == fmt.Sprintf(AssetNames[p], rel.TagName) {
				d := strings.TrimPrefix(a.Digest, "sha256:")
				if !sha256Re.MatchString(d) {
					return nil, time.Time{}, false
				}
				digests[p] = d
				u := a.UpdatedAt.UTC()
				if oldest.IsZero() || u.After(oldest) {
					oldest = u
				}
			}
		}
	}
	return digests, oldest, len(digests) == len(Platforms)
}

func fetchReleases(ctx context.Context, url string) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("release api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release api: HTTP %d", resp.StatusCode)
	}
	var rels []Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&rels); err != nil {
		return nil, fmt.Errorf("release api: %w", err)
	}
	return rels, nil
}

// DownloadAsset fetches the pinned prebuilt archive for platform,
// streams it through SHA-256 (fail-closed on digest mismatch — no
// unverified bytes survive) into an exclusive unpredictable temp file
// and returns its path. The CALLER owns removal after extraction.
func DownloadAsset(ctx context.Context, lk Lock, platform string) (string, error) {
	a, ok := lk.Assets[platform]
	if !ok {
		return "", fmt.Errorf("no pinned asset for %s", platform)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", a.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", a.URL, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "llama-asset-*.tar.gz")
	if err != nil {
		return "", err
	}
	path := f.Name()
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAssetBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr // a failed close means broken bytes on disk
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("download %s: %w", a.URL, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != a.SHA256 {
		os.Remove(path)
		return "", fmt.Errorf("E_DIGEST: asset sha256 %s != pinned %s for %s", got, a.SHA256, platform)
	}
	return path, nil
}

// maxAssetBytes bounds asset downloads (real archives are ~10–20 MB).
const maxAssetBytes = 512 << 20

// Now returns the canonical timestamp for updated_at.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// newGzipReader is split out so extract_unix stays testable.
func newGzipReader(r io.Reader) (*gzip.Reader, error) { return gzip.NewReader(r) }

// HostPlatform maps GOOS/GOARCH onto the supported runner platforms —
// the ONE mapping truth (the Makefile just calls this).
func HostPlatform() (string, error) {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture %q (want arm64/amd64)", runtime.GOARCH)
	}
	p := runtime.GOOS + "_" + arch
	for _, x := range Platforms {
		if x == p {
			return p, nil
		}
	}
	return "", fmt.Errorf("unsupported platform %q (want %v)", p, Platforms)
}
