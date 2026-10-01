package catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// DefaultPiratefaceURL is the live catalog endpoint; SHARDR_CATALOG_URL
// overrides it (mirrors, tests) — same pattern as HF_ENDPOINT.
const DefaultPiratefaceURL = "https://pirateface.co"

// Provider errors — the API layer maps them to 005 §3 classes.
var (
	ErrCatalogUnreachable = fmt.Errorf("catalog: provider unreachable")
	ErrNotListed          = fmt.Errorf("catalog: repo not listed by the provider")
	ErrBadListing         = fmt.Errorf("catalog: malformed listing")
)

// Pirateface is the pirateface.co provider (verified surface, Epic #65):
//
//	GET /s/<terms>  → text/plain, one model per line:
//	  <owner/repo>  <size label>  <N> seeds  <magnet>
//	(fields separated by exactly two spaces)
//
// Magnets are BitTorrent v1 (urn:btih, 40 hex), carry the pirateface
// tracker twice (udp + http) and one webseed
// https://pirateface.co/api/ws/<owner>/<repo>/<40-hex revision>/ whose
// path pins the HF revision. Model pages (/<owner>/<repo>) embed a
// checksum record ("<sha256>  <path>" per line, LFS weight files) in the
// server-rendered payload — fetched only for rescued pulls, where it is
// the anchor of record under --trust-catalog.
type Pirateface struct {
	BaseURL string
	HTTP    *http.Client
}

// NewPirateface builds a client with env-derived defaults.
func NewPirateface() *Pirateface {
	base := os.Getenv("SHARDR_CATALOG_URL")
	if base == "" {
		base = DefaultPiratefaceURL
	}
	return &Pirateface{BaseURL: base, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Name implements Provider.
func (p *Pirateface) Name() string { return "pirateface" }

func (p *Pirateface) get(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrCatalogUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil, ErrNotListed
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: read body: %v", ErrCatalogUnreachable, err)
	}
	return resp.StatusCode, b, nil
}

// Search implements Provider: GET /s/<terms> (QueryEscape — a "/" in the
// terms must stay inside one path segment or the request routes to a
// page, not the terminal API).
func (p *Pirateface) Search(ctx context.Context, terms string) ([]Model, error) {
	if strings.TrimSpace(terms) == "" {
		return nil, fmt.Errorf("%w: empty search terms", ErrBadListing)
	}
	_, body, err := p.get(ctx, "/s/"+url.QueryEscape(terms))
	if err != nil {
		return nil, err
	}
	var models []Model
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		m, perr := ParseSearchLine(ln)
		if perr != nil {
			// One malformed line never poisons the listing; a truncated
			// last line is a provider hiccup, not our failure. Unparsed
			// lines are skipped loudly-by-count downstream if empty.
			continue
		}
		models = append(models, *m)
	}
	return models, nil
}

// Resolve implements Provider: exact-match search for the repo, then
// decompose the magnet into identity + transport hints + pinned revision.
func (p *Pirateface) Resolve(ctx context.Context, repo string) (*Resolved, error) {
	if !ValidRepoID(repo) {
		return nil, fmt.Errorf("%w: repo id %q is not owner/name", ErrBadListing, repo)
	}
	models, err := p.Search(ctx, repo)
	if err != nil {
		return nil, err
	}
	for i := range models {
		if models[i].Repo != repo {
			continue
		}
		return FromListing(&models[i])
	}
	return nil, fmt.Errorf("%w: %s", ErrNotListed, repo)
}

// Checksums fetches the catalog's checksum record for a repo from its
// model page: the server-rendered payload embeds the record as a
// "<sha256>  <path>"-per-line string prop. LFS weight files only — the
// record is the rescued-anchor source, never the default anchor.
func (p *Pirateface) Checksums(ctx context.Context, repo string) ([]FileChecksum, error) {
	if !ValidRepoID(repo) {
		return nil, fmt.Errorf("%w: repo id %q is not owner/name", ErrBadListing, repo)
	}
	status, body, err := p.get(ctx, "/"+repo)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: model page HTTP %d for %s", ErrBadListing, status, repo)
	}
	return ParseChecksumRecord(string(body))
}

// FromListing decomposes a search hit into the full resolved record.
func FromListing(m *Model) (*Resolved, error) {
	mag, err := metainfo.ParseMagnetUri(m.Magnet)
	if err != nil {
		return nil, fmt.Errorf("%w: magnet for %s: %v", ErrBadListing, m.Repo, err)
	}
	ih := mag.InfoHash.HexString()
	if ih == "" || len(ih) != 40 {
		return nil, fmt.Errorf("%w: magnet for %s carries no v1 infohash", ErrBadListing, m.Repo)
	}
	r := &Resolved{
		Repo:     m.Repo,
		Infohash: strings.ToLower(ih),
		Magnet:   m.Magnet,
		Trackers: mag.Trackers,
		Webseeds: mag.Params["ws"],
		Size:     m.Size,
		Seeds:    m.Seeds,
	}
	// The pinned revision is the 40-hex tail of the webseed path — the
	// only revision statement a catalog listing makes.
	for _, ws := range r.Webseeds {
		if rev, ok := revisionFromWebseed(ws); ok {
			r.Revision = rev
			break
		}
	}
	if r.Revision == "" {
		return nil, fmt.Errorf("%w: magnet for %s carries no webseed with a pinned 40-hex revision — no anchor is derivable", ErrBadListing, m.Repo)
	}
	return r, nil
}

func revisionFromWebseed(ws string) (string, bool) {
	u, err := url.Parse(ws)
	if err != nil || u.Path == "" {
		return "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) == 0 {
		return "", false
	}
	rev := segs[len(segs)-1]
	if len(rev) != 40 {
		return "", false
	}
	for _, c := range rev {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return rev, true
}

// ParseSearchLine parses one /s/ result line:
// "<owner/repo>  <size label>  <N> seeds  <magnet>".
func ParseSearchLine(ln string) (*Model, error) {
	fields := strings.Split(ln, "  ")
	if len(fields) != 4 {
		return nil, fmt.Errorf("%w: line %q: want 4 double-space-separated fields, got %d", ErrBadListing, ln, len(fields))
	}
	repo, size, seeds, magnet := fields[0], fields[1], fields[2], fields[3]
	if !ValidRepoID(repo) {
		return nil, fmt.Errorf("%w: line %q: field 0 is not owner/name", ErrBadListing, ln)
	}
	seeds = strings.TrimSuffix(strings.TrimSuffix(seeds, "s"), " seed")
	n, err := strconv.Atoi(strings.TrimSpace(seeds))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%w: line %q: field 2 is not a seed count", ErrBadListing, ln)
	}
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:") {
		return nil, fmt.Errorf("%w: line %q: field 3 is not a magnet URI", ErrBadListing, ln)
	}
	return &Model{Repo: repo, Size: size, Seeds: n, Magnet: magnet}, nil
}

// checksumRecordRe matches the checksum record prop in the model page's
// server-rendered payload. The payload is JSON-in-JS: quotes arrive as
// \" and record lines are separated by a literal \n escape.
var checksumRecordRe = regexp.MustCompile(`\\"manifest\\":\\"((?:[a-f0-9]{64}  [^\\"\n]+)(?:\\n[a-f0-9]{64}  [^\\"\n]+)*)\\"`)

// ParseChecksumRecord extracts the checksum record from a model page.
func ParseChecksumRecord(html string) ([]FileChecksum, error) {
	m := checksumRecordRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("%w: no checksum record on the model page", ErrBadListing)
	}
	var out []FileChecksum
	for _, ln := range strings.Split(m[1], `\n`) {
		parts := strings.SplitN(ln, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != 64 || parts[1] == "" {
			return nil, fmt.Errorf("%w: checksum line %q is not <sha256>  <path>", ErrBadListing, ln)
		}
		out = append(out, FileChecksum{Path: parts[1], SHA256: parts[0]})
	}
	return out, nil
}

// ValidRepoID: namespace/name, bounded charset (mirrors the HF repo id
// rules of the importer — catalog repos ARE HF repo ids).
func ValidRepoID(repo string) bool {
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
