package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/catalog"
	"github.com/Cyb3rDudu/shardr/internal/config"
	"github.com/Cyb3rDudu/shardr/internal/importer"
)

// Pull runs /ensure for a ref and follows the job to its terminal state
// (005 §4): progress on stdout, job errors verbatim.
func Pull(ctx context.Context, c *Client, arg string, out io.Writer) error {
	canonical, err := canonicalizeOr(arg, "")
	if err != nil {
		return err
	}
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, "/v1/ensure", map[string]string{"ref": canonical}, &job); err != nil {
		return err
	}
	fmt.Fprintf(out, "ensuring %s\n  job %s\n", canonical, job.ID)
	last := ""
	term, err := c.WaitJob(ctx, job.ID, func(j Job) {
		bar := fmt.Sprintf("%d/%d", j.FilesDone, j.FilesTotal)
		if bar != last {
			fmt.Fprintf(out, "\r  %s %s", j.State, bar)
			last = bar
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	if term.State == "failed" {
		return term.Error
	}
	fmt.Fprintf(out, "done: %s local (%d files)\n", canonical, term.FilesTotal)
	return nil
}

// ImportLocal ingests local files under a required --as namespace.
func ImportLocal(ctx context.Context, c *Client, paths []string, as string, out io.Writer) error {
	if as == "" {
		return fmt.Errorf("E_BAD_REQUEST: --as is required (001 §8.6): shardr import local <paths> --as ns/name")
	}
	if len(paths) == 0 {
		return fmt.Errorf("E_BAD_REQUEST: no paths given")
	}
	return runImportJob(ctx, c, out, "/v1/import/local", map[string]any{"paths": paths, "as": as})
}

// ImportHF imports from Hugging Face (001 §8).
func ImportHF(ctx context.Context, c *Client, repo, rev string, out io.Writer) error {
	if repo == "" {
		return fmt.Errorf("E_BAD_REQUEST: no repo given")
	}
	body := map[string]any{"repo": repo}
	if rev != "" {
		body["revision"] = rev
	}
	return runImportJob(ctx, c, out, "/v1/import/hf", body)
}

// ImportBT imports via BitTorrent with a MANDATORY manifest pin — a
// magnet alone is never trusted (005 §5); the server enforces it, the
// CLI refuses earlier with the same rule.
func ImportBT(ctx context.Context, c *Client, magnetOrInfohash, manifest string, out io.Writer) error {
	if magnetOrInfohash == "" {
		return fmt.Errorf("E_BAD_REQUEST: no magnet/infohash given")
	}
	if manifest == "" {
		return fmt.Errorf("E_BAD_REQUEST: --manifest <sha256:…> is mandatory — a magnet alone is never trusted (005 §5)")
	}
	// The API takes {magnet | infohash} as DISTINCT fields (005 §3) — a
	// magnet URI in the infohash field is a 400 waiting to happen.
	field := "infohash"
	if strings.HasPrefix(magnetOrInfohash, "magnet:") {
		field = "magnet"
	}
	return runImportJob(ctx, c, out, "/v1/import/bt", map[string]any{
		field: magnetOrInfohash, "manifestDigest": manifest})
}

func runImportJob(ctx context.Context, c *Client, out io.Writer, path string, body any) error {
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, path, body, &job); err != nil {
		return err
	}
	fmt.Fprintf(out, "import started: job %s\n", job.ID)
	term, err := c.WaitJob(ctx, job.ID, func(j Job) {
		fmt.Fprintf(out, "\r  %s %d/%d", j.State, j.FilesDone, j.FilesTotal)
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	if term.State == "failed" {
		return term.Error
	}
	if term.Result != nil {
		for _, q := range term.Result.Quants {
			fmt.Fprintf(out, "  quant %s\n", q)
		}
		if term.Result.Infohash != "" {
			fmt.Fprintf(out, "  infohash %s\n", term.Result.Infohash)
		}
		for _, w := range term.Result.Warnings {
			fmt.Fprintf(out, "  warning: %s\n", w)
		}
	}
	fmt.Fprintf(out, "done\n")
	return nil
}

// catalogProvider builds the CLI's catalog provider with the SAME
// base-URL resolution as the daemon: [catalog] url from config.toml
// (validated by the shared catalog.ResolveConfigURL), else
// $SHARDR_CATALOG_URL, else the default. Without this, a mirror
// configured for the daemon would be invisible to `catalog search`
// and `pull` — the CLI pre-checks must see the provider the daemon
// will actually use. Config errors are loud (E_CONFIG), never a
// silent fallback to the default.
func catalogProvider() (*catalog.Pirateface, error) {
	f, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	u, err := catalog.ResolveConfigURL(f)
	if err != nil {
		return nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	return catalog.NewPiratefaceAt(u), nil
}

// CatalogSearch runs a provider search and renders the listing
// (repo, size, reported seeds, magnet).
func CatalogSearch(ctx context.Context, terms []string, out io.Writer) error {
	if len(terms) == 0 {
		return fmt.Errorf("E_BAD_REQUEST: catalog search needs terms")
	}
	pf, err := catalogProvider()
	if err != nil {
		return err
	}
	models, err := pf.Search(ctx, strings.Join(terms, " "))
	if err != nil {
		return err
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "no models match — try broader terms")
		return nil
	}
	for _, m := range models {
		fmt.Fprintf(out, "%s\t%s\t%d seeds\t%s\n", m.Repo, m.Size, m.Seeds, m.Magnet)
	}
	return nil
}

// CatalogPullOptions configure a catalog pull.
type CatalogPullOptions struct {
	Quant        string
	TrustCatalog bool
}

// CatalogPull pulls a catalog-listed model through the daemon: resolve
// for display, early rescued refusal (the daemon re-derives the whole
// trust path itself — the CLI is comfort, never authority), then the
// import job with its trust-shift warning rendered on top.
func CatalogPull(ctx context.Context, c *Client, repo string, opts CatalogPullOptions, out io.Writer) error {
	pf, err := catalogProvider()
	if err != nil {
		return err
	}
	resolved, err := pf.Resolve(ctx, repo)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s  %s  %d seeds  rev %s…\n", resolved.Repo, resolved.Size, resolved.Seeds, resolved.Revision[:12])
	// Early rescued check: one HF probe, fail fast before the job.
	hf := importer.NewHFClient()
	if _, terr := hf.ListRepoTree(ctx, resolved.Repo, resolved.Revision); terr != nil {
		if errors.Is(terr, importer.ErrUnknownRepo) && !opts.TrustCatalog {
			return catalog.ErrNotAnchored
		}
		if !errors.Is(terr, importer.ErrUnknownRepo) {
			return fmt.Errorf("E_SOURCE_UNAVAILABLE: anchor fetch for %s@%s: %w", resolved.Repo, resolved.Revision, terr)
		}
		fmt.Fprintln(out, "warning: rescued model — trust shifts from Hugging Face to the catalog provider (--trust-catalog given)")
	}
	body := map[string]any{"repo": repo, "trustCatalog": opts.TrustCatalog}
	if opts.Quant != "" {
		body["quant"] = opts.Quant
	}
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, "/v1/import/catalog", body, &job); err != nil {
		return err
	}
	fmt.Fprintf(out, "pull started: job %s\n", job.ID)
	last := ""
	term, err := c.WaitJob(ctx, job.ID, func(j Job) {
		bar := fmt.Sprintf("%d/%d", j.FilesDone, j.FilesTotal)
		if bar != last {
			fmt.Fprintf(out, "\r  %s %s", j.State, bar)
			last = bar
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	if term.State == "failed" {
		return term.Error
	}
	if term.Result != nil {
		for _, w := range term.Result.Warnings {
			fmt.Fprintf(out, "  warning: %s\n", w)
		}
		for _, q := range term.Result.Quants {
			fmt.Fprintf(out, "  quant %s\n", q)
		}
	}
	fmt.Fprintf(out, "done: shardr:///%s (seeding the catalog swarm — good citizen)\n", strings.ToLower(repo))
	return nil
}

// Models prints the inventory (005 §4 /models).
func Models(ctx context.Context, c *Client, out io.Writer) error {
	var inv struct {
		Namespaces []struct {
			NS           string   `json:"ns"`
			Name         string   `json:"name"`
			IndexDigest  string   `json:"indexDigest"`
			IndexPresent bool     `json:"indexPresent"`
			Quants       []string `json:"quants,omitempty"`
		} `json:"namespaces"`
		Tags []struct {
			Repo string `json:"repo"`
			Tag  string `json:"tag"`
		} `json:"tags"`
	}
	if err := c.DoJSON(ctx, http.MethodGet, "/v1/models", nil, &inv); err != nil {
		return err
	}
	if len(inv.Namespaces) == 0 && len(inv.Tags) == 0 {
		fmt.Fprintln(out, "no models imported yet — shardr import local <paths> --as ns/name")
		return nil
	}
	for _, ns := range inv.Namespaces {
		status := "index missing"
		if ns.IndexPresent {
			status = "index present"
		}
		fmt.Fprintf(out, "%s/%s\t%s\t%s\n", ns.NS, ns.Name, status, strings.Join(ns.Quants, ","))
	}
	for _, t := range inv.Tags {
		fmt.Fprintf(out, "%s:%s\ttag\n", t.Repo, t.Tag)
	}
	return nil
}

// Verify re-hashes content (003 §4) via the verify job: ref | digest | --all.
func Verify(ctx context.Context, c *Client, target string, out io.Writer) error {
	if target == "" {
		return fmt.Errorf("E_BAD_REQUEST: verify needs a ref, a digest, or --all")
	}
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, "/v1/verify", map[string]string{"target": target}, &job); err != nil {
		return err
	}
	term, err := c.WaitJob(ctx, job.ID, nil)
	if err != nil {
		return err
	}
	if term.State == "failed" {
		return term.Error
	}
	if term.Verify != nil && (len(term.Verify.Missing) > 0 || len(term.Verify.StateErrors) > 0) {
		for _, m := range term.Verify.Missing {
			fmt.Fprintf(out, "PROBLEM: %s\n", m)
		}
		for _, e := range term.Verify.StateErrors {
			fmt.Fprintf(out, "PROBLEM: %s\n", e)
		}
		return fmt.Errorf("E_VERIFY_FAILED: integrity problems found (see above)")
	}
	fmt.Fprintf(out, "ok: %s (%d blobs re-hashed)\n", job.Ref, term.FilesTotal)
	return nil
}

// Status shows one job or the recent job list (005 §4).
func Status(ctx context.Context, c *Client, jobID string, out io.Writer) error {
	if jobID != "" {
		var j Job
		if err := c.DoJSON(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &j); err != nil {
			return err
		}
		fmt.Fprintf(out, "job %s %s %s (%d/%d)\n", j.ID, j.Kind, j.State, j.FilesDone, j.FilesTotal)
		if j.Error != nil {
			fmt.Fprintf(out, "  error %s: %s\n", j.Error.Code, j.Error.Message)
		}
		return nil
	}
	var list struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.DoJSON(ctx, http.MethodGet, "/v1/jobs", nil, &list); err != nil {
		return err
	}
	if len(list.Jobs) == 0 {
		fmt.Fprintln(out, "no jobs yet")
		return nil
	}
	// Server sorts newest-first (createdAt); show the 20 most recent in
	// that order — newest at the top.
	n := len(list.Jobs)
	if n > 20 {
		n = 20
	}
	for i := 0; i < n; i++ {
		j := list.Jobs[i]
		line := fmt.Sprintf("%s %s %s", j.ID, j.Kind, j.State)
		if j.Ref != "" && j.Ref != "all" {
			line += " " + j.Ref
		}
		if j.Error != nil {
			line += fmt.Sprintf(" [%s]", j.Error.Code)
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

// canonicalizeOr canonicalizes with an optional default selector from
// the loaded user config (004 §7).
func canonicalizeOr(arg, _ string) (string, error) {
	f, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("E_CONFIG: %w", err)
	}
	sel := ""
	if sec, ok := f["references"]; ok {
		if v, ok := sec["default_selector"]; ok {
			sel = v.Str
		}
	}
	return Canonicalize(arg, sel)
}
