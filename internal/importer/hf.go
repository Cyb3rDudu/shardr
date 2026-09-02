package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// HF source errors — mapped to job error classes by the API layer.
var (
	ErrRateLimited   = errors.New("importer: HF rate limited (HTTP 429)")
	ErrForbidden     = errors.New("importer: HF access denied (auth required or private repo)")
	ErrUnknownRepo   = errors.New("importer: unknown HF repo")
	ErrHFUnreachable = errors.New("importer: HF source unreachable")
)

// DefaultHFBaseURL is the public HF endpoint; HF_ENDPOINT overrides it
// (mirrors, tests).
const DefaultHFBaseURL = "https://huggingface.co"

// HFClient reads model repos from huggingface.co: file listings via the
// API, file bytes via the range-capable resolve endpoints. Token from
// SHARDR_HF_TOKEN when set; anonymous access works for public repos.
// Stdlib only.
type HFClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHFClient builds a client with the env-derived defaults.
func NewHFClient() *HFClient {
	base := os.Getenv("HF_ENDPOINT")
	if base == "" {
		base = DefaultHFBaseURL
	}
	return &HFClient{BaseURL: base, Token: os.Getenv("SHARDR_HF_TOKEN"),
		HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *HFClient) do(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHFUnreachable, err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		resp.Body.Close()
		return nil, ErrRateLimited
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, ErrForbidden
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrUnknownRepo
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrHFUnreachable, resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// RepoInfo is the listing result: files (upstream-relative names) plus the
// pinned commit SHA for annotations.
type RepoInfo struct {
	Files     []string
	CommitSHA string
}

// ListRepo fetches /api/models/{repo}/revision/{revision} (revision
// defaults to "main"). The response's sha pins the import.
func (c *HFClient) ListRepo(ctx context.Context, repo, revision string) (*RepoInfo, error) {
	if revision == "" {
		revision = "main"
	}
	if !validHFRepoID(repo) {
		return nil, fmt.Errorf("invalid repo id %q", repo)
	}
	resp, err := c.do(ctx, c.BaseURL+"/api/models/"+repo+"/revision/"+revision)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var doc struct {
		SHA      string `json:"sha"`
		Siblings []struct {
			RFilename string `json:"rfilename"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("hf api decode: %w", err)
	}
	info := &RepoInfo{CommitSHA: doc.SHA}
	for _, s := range doc.Siblings {
		if s.RFilename != "" {
			info.Files = append(info.Files, s.RFilename)
		}
	}
	if len(info.Files) == 0 {
		return nil, fmt.Errorf("hf repo %s@%s: empty file listing", repo, revision)
	}
	return info, nil
}

// OpenFile streams one file: GET /{repo}/resolve/{revision}/{path}. The
// endpoint is range-capable; this slice streams sequentially (resume via
// Range is CAS-part follow-up work). Redirects (CDN) are followed by the
// stdlib client; bytes are verified by the CAS write path regardless of
// transport.
func (c *HFClient) OpenFile(ctx context.Context, repo, revision, path string) (io.ReadCloser, error) {
	if revision == "" {
		revision = "main"
	}
	resp, err := c.do(ctx, c.BaseURL+"/"+repo+"/resolve/"+revision+"/"+path)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// validHFRepoID: namespace/name with a permissive but bounded charset.
func validHFRepoID(repo string) bool {
	if len(repo) < 3 || len(repo) > 200 || strings.Contains(repo, "..") {
		return false
	}
	for _, r := range repo {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '/' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return strings.Count(repo, "/") == 1
}
