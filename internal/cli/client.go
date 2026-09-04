// Package cli implements the shardr user binary's command layer (005 §4):
// management commands against the daemon API plus the model lifecycle
// (002 §4). cmd/shardr is a thin argv dispatcher over this package.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/api"
	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// Client speaks the shardhive API over its Unix socket (005 §3): HTTP
// with a socket dialer, JSON in/out, canonical error envelopes.
type Client struct {
	http *http.Client
	base string
}

// NewClient resolves the socket exactly like the daemon (005 §3:
// $SHARDR_SOCKET → $XDG_RUNTIME_DIR/shardhive.sock → per-uid fallback)
// via the daemon's own resolver — one rule set, two binaries.
func NewClient() (*Client, error) {
	socket, err := api.DefaultSocketPath()
	if err != nil {
		return nil, fmt.Errorf("E_STATE: resolve socket path: %w", err)
	}
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
			Timeout: 0, // long-running streams (blob reads) own their deadlines
		},
		base: "http://shardhive", // dummy host — the dialer ignores it
	}, nil
}

// APIError re-exports the wire error type for command error paths.
type APIError = api.APIError

// Do issues one API request; non-2xx responses decode into *APIError
// (job errors pass through UNCHANGED — no E_INTERNAL swallowing, strand
// rule 6). Callers own closing resp.Body on nil error.
func (c *Client) Do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("E_INTERNAL: encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("E_INTERNAL: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("E_DAEMON_UNREACHABLE: shardhive daemon not reachable on %s — is it running? (%v)",
			strings.TrimPrefix(lastDialPath(c), ""), err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var env struct {
			Error *APIError `json:"error"`
		}
		if jerr := json.NewDecoder(resp.Body).Decode(&env); jerr == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("E_DAEMON_UNREACHABLE: HTTP %d (no error envelope)", resp.StatusCode)
	}
	return resp, nil
}

func lastDialPath(c *Client) string { return "" }

// DoJSON issues a request and decodes a 2xx JSON body into out.
func (c *Client) DoJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("E_INTERNAL: decode response: %w", err)
	}
	return nil
}

// Canonicalize turns a CLI ref (short or canonical, 000 §2) into the
// canonical URI the API requires. Interactive comfort: a selector-less
// ref is completed from [references].default_selector (004 §7) when the
// user configured one — humans get comfort, the API gets determinism
// (005 §2.4: the API always sees the canonical, selector-bearing URI).
func Canonicalize(input string, defaultSelector string) (string, error) {
	p, rerr := ref.ParseAny(input)
	if rerr != nil {
		// Selector-less + configured default_selector → complete and
		// re-parse (interactive comfort only, 000 §2 / 004 §7).
		if rerr.Class == "E_NO_SELECTOR" && defaultSelector != "" {
			withSel := input + ":" + defaultSelector
			p2, rerr2 := ref.ParseAny(withSel)
			if rerr2 != nil {
				return "", fmt.Errorf("%s: default_selector %q does not complete %q: %s", rerr2.Class, defaultSelector, input, rerr2.Message)
			}
			return p2.Canonical, nil
		}
		msg := rerr.Class + ": " + rerr.Message
		if len(rerr.Candidates) > 0 {
			msg += " (candidates: " + strings.Join(rerr.Candidates, ", ") + ")"
		}
		if rerr.Class == "E_NO_SELECTOR" {
			msg += " — use ns/name:quant (000 §2) or set [references].default_selector"
		}
		return "", fmt.Errorf("%s", msg)
	}
	if p.Quant == "" && p.Digest != "" {
		return p.Canonical, nil // @digest manifest-addressing: no selector needed
	}
	return p.Canonical, nil
}

// Job mirrors the daemon's job document (005 §3 /jobs/<id>).
type Job struct {
	ID         string        `json:"id"`
	Ref        string        `json:"ref"`
	State      string        `json:"state"`
	Manifest   string        `json:"manifest,omitempty"`
	Error      *APIError     `json:"error,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	FilesDone  int           `json:"filesDone,omitempty"`
	FilesTotal int           `json:"filesTotal,omitempty"`
	Result     *ImportResult `json:"result,omitempty"`
	Verify     *struct {
		Missing     []string `json:"missing,omitempty"`
		StateErrors []string `json:"stateErrors,omitempty"`
	} `json:"verify,omitempty"`
}

// ImportResult is the terminal payload of import jobs.
type ImportResult struct {
	Manifests   []string `json:"manifests"`
	IndexDigest string   `json:"indexDigest"`
	Quants      []string `json:"quants"`
	Warnings    []string `json:"warnings,omitempty"`
	Skipped     int      `json:"skipped"`
	Infohash    string   `json:"infohash,omitempty"`
}

// WaitJob polls a job to a terminal state (done|failed) and returns it.
// onTick (may be nil) observes every intermediate state.
func (c *Client) WaitJob(ctx context.Context, id string, onTick func(j Job)) (*Job, error) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		var j Job
		if err := c.DoJSON(ctx, http.MethodGet, "/v1/jobs/"+id, nil, &j); err != nil {
			return nil, err
		}
		if onTick != nil {
			onTick(j)
		}
		if j.State == "done" || j.State == "failed" {
			return &j, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
