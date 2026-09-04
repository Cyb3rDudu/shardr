package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/config"
	"github.com/Cyb3rDudu/shardr/internal/ref"
	"github.com/Cyb3rDudu/shardr/internal/runner"
)

// RunOptions are the run/serve knobs (002 §4).
type RunOptions struct {
	ID         string   // serve: stable instance id ("" = derive from ref)
	ConfigFile string   // --config <file.toml> (layer 3)
	Sets       []string // --set key=value (layer 4)
}

// openResult is the /open payload the runner consumes: the full file
// list with CAS paths after ensure (005 §3).
type openResult struct {
	Ref            string     `json:"ref"`
	ManifestDigest string     `json:"manifestDigest,omitempty"`
	Files          []openFile `json:"files,omitempty"`
	Missing        []string   `json:"missing,omitempty"`
}

type openFile struct {
	Digest  string `json:"digest"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Role    string `json:"role,omitempty"`
	Variant string `json:"variant,omitempty"`
	Part    *int64 `json:"part,omitempty"`
	Runtime string `json:"runtime,omitempty"`
}

// Run executes the foreground lifecycle (002 §4): resolve → ensure →
// merge overlays → spawn llama-server (weights as CAS path, zero-copy)
// → readiness → attach signals (Ctrl-C = SIGTERM discipline).
func Run(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer) error {
	rt, err := launch(ctx, c, arg, opts, out)
	if err != nil {
		return err
	}
	defer rt.Terminate() // one terminate path: signal or ctx, never both fire twice
	fmt.Fprintf(out, "ready: %s (model id %s) — Ctrl-C to stop\n", rt.Endpoint(), rt.Ref)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		fmt.Fprintln(out, "\nstopping (SIGTERM; SIGKILL after 30 s)")
	case <-ctx.Done():
	}
	return nil
}

// Serve executes the background lifecycle (002 §4): detached spawn,
// stable id, registry entry, endpoint output.
func Serve(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer) error {
	rt, err := launchDetached(ctx, c, arg, opts, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "serving %s\n  id       %s\n  endpoint %s\n  model    %s\n", rt.Ref, rt.ID, rt.Endpoint(), rt.Ref)
	return nil
}

// Stop terminates serve instances (002 §4): SIGTERM, clean ≤ 30 s,
// SIGKILL after — per id or --all.
func Stop(ctx context.Context, c *Client, id string, all bool, out io.Writer) error {
	reg, err := runner.OpenRegistry()
	if err != nil {
		return err
	}
	list, err := reg.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "no serve instances running")
		return nil
	}
	var targets []string
	switch {
	case all:
		for _, in := range list {
			targets = append(targets, in.ID)
		}
	case id != "":
		if _, err := reg.Find(id); err != nil {
			for _, in := range list {
				fmt.Fprintf(out, "running: %s (%s)\n", in.ID, in.Endpoint)
			}
			return err
		}
		targets = []string{id}
	default:
		return fmt.Errorf("E_BAD_REQUEST: stop needs an id or --all (running: %s)", idsOf(list))
	}
	for _, tid := range targets {
		in, err := reg.Find(tid)
		if err != nil {
			return err
		}
		// PID-reuse guard: a stale registry entry after a crash/reboot
		// could name an unrelated recycled PID — SIGTERM (+SIGKILL after
		// 30 s) would kill it. Verify identity first: the instance's
		// /v1/models must serve id == the registered ref (--alias makes
		// the real runtime do exactly that).
		if id, ok := probeModelID(ctx, in.Endpoint); !ok || id != in.Ref {
			return fmt.Errorf("E_STATE: refusing to stop %s: pid %d does not serve %s on %s (stale entry or pid reuse?) — verify and clean %s manually",
				in.ID, in.PID, in.Ref, in.Endpoint, reg.Path())
		}
		fmt.Fprintf(out, "stopping %s (pid %d)…\n", in.ID, in.PID)
		if err := runner.TerminatePID(in.PID); err != nil {
			return err
		}
		if err := reg.Remove(tid); err != nil {
			return err
		}
		fmt.Fprintf(out, "stopped %s\n", tid)
	}
	return nil
}

// probeModelID asks a serve endpoint for its model id (identity check
// before signaling; 002 §6 id = reference).
func probeModelID(ctx context.Context, endpoint string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var list struct {
		Models []struct {
			ID string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list.Models) == 0 {
		return "", false
	}
	return list.Models[0].ID, true
}

func idsOf(list []runner.Instance) string {
	var sb strings.Builder
	for i, in := range list {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(in.ID)
	}
	return sb.String()
}

// launched is the outcome of a successful launch.
type launched struct {
	*runner.Runtime
	Ref string
	ID  string
}

func launch(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer) (*launched, error) {
	l, err := prepare(ctx, c, arg, opts, out, false)
	if err != nil {
		return nil, err
	}
	if err := l.Runtime.WaitReady(ctx); err != nil {
		l.Runtime.Terminate()
		return nil, err
	}
	return l, nil
}

func launchDetached(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer) (*launched, error) {
	l, err := prepare(ctx, c, arg, opts, out, true)
	if err != nil {
		return nil, err
	}
	if err := l.Runtime.WaitReady(ctx); err != nil {
		l.Runtime.Terminate()
		return nil, err
	}
	// Register only after readiness — a dead instance never lands in the
	// registry.
	reg, err := runner.OpenRegistry()
	if err != nil {
		l.Runtime.Terminate()
		return nil, err
	}
	if err := reg.Add(runner.Instance{
		ID: l.ID, Ref: l.Ref, Endpoint: l.Runtime.Endpoint(),
		PID: l.Runtime.PID(), Started: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		l.Runtime.Terminate()
		return nil, err
	}
	return l, nil
}

// prepare does resolve → ensure → open → overlay → spawn.
func prepare(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer, detached bool) (*launched, error) {
	canonical, userCfgFile, layer3File, err := canonicalizeWithConfig(arg, opts.ConfigFile)
	if err != nil {
		return nil, err
	}
	p, rerr := ref.Parse(canonical)
	if rerr != nil {
		return nil, fmt.Errorf("%s: %s", rerr.Class, rerr.Message)
	}
	fmt.Fprintf(out, "resolve %s\n", canonical)

	// Ensure: CAS hit or fill job; then open for the file list.
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, "/v1/ensure", map[string]string{"ref": canonical}, &job); err != nil {
		return nil, err
	}
	if job.State == "waiting" || job.State == "fetching" {
		fmt.Fprintf(out, "filling %s\n", canonical)
		last := ""
		term, err := c.WaitJob(ctx, job.ID, func(j Job) {
			bar := fmt.Sprintf("%d/%d", j.FilesDone, j.FilesTotal)
			if bar != last {
				fmt.Fprintf(out, "\r  %s %s", j.State, bar)
				last = bar
			}
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(out)
		if term.State == "failed" {
			return nil, term.Error
		}
	}
	var open openResult
	if err := c.DoJSON(ctx, http.MethodGet, "/v1/open?ref="+urlEscape(canonical), nil, &open); err != nil {
		return nil, err
	}
	if len(open.Missing) > 0 {
		return nil, fmt.Errorf("E_SOURCE_UNAVAILABLE: %d files still missing after ensure: %s", len(open.Missing), strings.Join(open.Missing, ", "))
	}
	// Overlays (002 §2): advisory ← runtime-config blob via /blob; user
	// config and --config stay SEPARATE layers so unknown-key errors name
	// the true source (layer provenance, 002 §2.2).
	ov := runner.Overlay{
		UserConfig: runner.UserConfigOverlay(userCfgFile, "llama", shortRefOf(p)),
		ConfigFile: runner.ConfigFileOverlay(layer3File, "llama", shortRefOf(p)),
	}
	if open.ManifestDigest != "" {
		if adv, aerr := advisory(ctx, c, open.Files); aerr != nil {
			return nil, aerr
		} else if len(adv) > 0 {
			ov.Advisory = adv
		}
	}
	for _, s := range opts.Sets {
		kv := strings.SplitN(s, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("E_BAD_REQUEST: --set wants key=value, got %q", s)
		}
		if err := ov.AddSet(kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	merged, err := ov.Merge()
	if err != nil {
		return nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	variant := merged["mmproj_variant"].Str // "" → f16 default (002 §5.7)
	weights, parts, mmproj, warns, err := pickWeights(open.Files, variant)
	for _, w := range warns {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	if err != nil {
		return nil, err
	}
	if len(parts) > 1 {
		// Split GGUFs: llama.cpp discovers siblings by the
		// <prefix>-0000N-of-0000M.gguf naming pattern — content-addressed
		// CAS names never match. Hardlink the parts under their ORIGINAL
		// manifest names into a per-manifest scratch dir: a hardlink
		// copies zero bytes (zero-copy law intact, 002 §4).
		splitDir, serr := splitScratchDir(open.ManifestDigest)
		if serr != nil {
			return nil, serr
		}
		for _, p := range parts {
			dst := filepath.Join(splitDir, p.Name)
			// Self-healing link: remove-then-link. A leftover from an
			// earlier process whose CAS root died has nlink 1 (its
			// original was unlinked); relinking against the live CAS
			// source restores the shared-bytes state.
			os.Remove(dst)
			if err := os.Link(p.Path, dst); err != nil {
				if !errors.Is(err, syscall.EXDEV) {
					return nil, fmt.Errorf("E_RUNTIME: hardlink split part %s: %w", p.Name, err)
				}
				// Cross-device (CAS and runtime dir on different
				// filesystems): a symlink carries zero bytes just the
				// same — the runtime opens and mmaps THROUGH it into the
				// CAS blob (zero-copy law intact, 002 §4).
				if err := os.Symlink(p.Path, dst); err != nil {
					return nil, fmt.Errorf("E_RUNTIME: link split part %s: %w", p.Name, err)
				}
			}
		}
		weights.Path = filepath.Join(splitDir, parts[0].Name)
	}

	bin, err := runner.ResolveBinary()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "spawn %s\n  weights %s (zero-copy mmap)\n", bin, weights.Path)
	var stdout, stderr *os.File
	if detached {
		// Detached instances keep logs next to the registry state.
		stateDir := filepath.Dir(mustRegistryPath())
		_ = os.MkdirAll(stateDir, 0o700)
		f, ferr := os.OpenFile(filepath.Join(stateDir, "serve-"+idFor(opts.ID, p)+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if ferr != nil {
			return nil, fmt.Errorf("E_STATE: open serve log: %w", ferr)
		}
		stdout, stderr = f, f
	}
	rt, err := runner.Spawn(runner.SpawnRequest{
		Binary: bin, Weights: weights.Path, Argv: runner.Argv(merged), Mmproj: mmproj, Ref: canonical,
		Detached: detached, Stdout: stdout, Stderr: stderr,
	})
	if stdout != nil {
		stdout.Close() // the child owns its dup; our fd must not leak per Serve
	}
	if err != nil {
		return nil, err
	}
	return &launched{Runtime: rt, Ref: canonical, ID: idFor(opts.ID, p)}, nil
}

// pickWeights selects the weights.gguf file and the vision-projector aux
// (002 §5.7): variant from mmproj_variant (default f16); a multimodal
// artifact without a projector warns; unknown aux roles log and continue.
func pickWeights(files []openFile, variant string) (weights openFile, parts []openFile, mmproj string, warns []string, err error) {
	if variant == "" {
		variant = "f16" // 002 §5.7 default
	}
	anyProjector := false
	match := false
	for _, f := range files {
		switch f.Kind {
		case "weights.gguf":
			parts = append(parts, f)
		case "weights.aux":
			switch f.Role {
			case "vision-projector":
				anyProjector = true
				if f.Variant == variant {
					mmproj = f.Path
					match = true
				}
			case "":
				warns = append(warns, "aux entry without role: "+f.Name)
			default:
				warns = append(warns, "ignoring unknown aux role "+f.Role+" ("+f.Name+") — 002 §5.7")
			}
		}
	}
	if len(parts) == 0 {
		return weights, nil, "", warns, fmt.Errorf("E_NOT_IMPORTABLE: artifact has no weights.gguf file (llama runtime, 002 §5.1)")
	}
	// Split GGUFs (001 §3.1 Part field): one logical weights set in N
	// ordered parts — llama-server takes part 1 via -m and discovers the
	// siblings by its own naming convention. Contiguity was validated at
	// import; parts of a split share the CAS directory.
	partOf := func(f openFile) int64 {
		if f.Part != nil {
			return *f.Part
		}
		return 0
	}
	sort.Slice(parts, func(i, j int) bool { return partOf(parts[i]) < partOf(parts[j]) })
	weights = parts[0]
	if anyProjector && !match {
		// Multimodal artifact without the selected projector variant: a
		// warning, never an abort (002 §5.7).
		warns = append(warns, "vision-projector variant "+variant+" not found — serving text-only")
	}
	return weights, parts, mmproj, warns, nil
}

// splitScratchDir is the per-manifest hardlink home for split GGUFs
// ($XDG_RUNTIME_DIR or the per-uid tmp fallback, mirroring the registry).
func splitScratchDir(manifestDigest string) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		uid := "0"
		if u, uerr := user.Current(); uerr == nil {
			uid = u.Uid
		}
		base = filepath.Join(os.TempDir(), "shardr-"+uid)
	}
	dir := filepath.Join(base, "shardr-split", strings.TrimPrefix(manifestDigest, "sha256:"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("E_STATE: split scratch dir: %w", err)
	}
	return dir, nil
}

func advisory(ctx context.Context, c *Client, files []openFile) (map[string]config.Value, error) {
	out := map[string]config.Value{}
	for _, f := range files {
		// Only THIS runtime's advisory entries (001 §3.1 Runtime id): a
		// foreign runtime's neutral keys must not leak into the llama
		// layer, and all matching entries merge.
		if f.Kind != "runtime-config" || f.Runtime != "llama" {
			continue
		}
		resp, err := c.Do(ctx, http.MethodGet, "/v1/blob/"+strings.TrimPrefix(f.Digest, "sha256:"), nil)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("E_INTERNAL: read advisory blob: %w", err)
		}
		scalars, err := runner.ParseScalars(b)
		if err != nil {
			return nil, fmt.Errorf("E_CONFIG: advisory runtime-config %s: %w", f.Name, err)
		}
		for k, v := range scalars {
			out[k] = v
		}
	}
	return out, nil
}

func shortRefOf(p *ref.Ref) string {
	s := p.NS + "/" + p.Name + ":" + p.Quant
	if p.Tag != "" {
		s = p.NS + "/" + p.Name + ":" + p.Tag + "+" + p.Quant
	}
	return s
}

var idSanitize = regexp.MustCompile(`[^a-z0-9._-]+`)

func idFor(want string, p *ref.Ref) string {
	if want != "" {
		return want
	}
	base := p.NS + "-" + p.Name + "-" + p.Quant
	return idSanitize.ReplaceAllString(strings.ToLower(base), "-")
}

func mustRegistryPath() string {
	p, err := runner.RegistryPath()
	if err != nil {
		return "."
	}
	return p
}

func urlEscape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == ':' || r == '/' || r == '+' || r == '@' {
			sb.WriteString(fmt.Sprintf("%%%02X", r))
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// canonicalizeWithConfig canonicalizes the ref (default_selector
// comfort) and loads the merged layer-2/3 config file: user config ∪
// --config (per-key, --config wins — it is the more specific layer 3).
func canonicalizeWithConfig(arg, configFile string) (string, config.File, config.File, error) {
	userCfg, err := config.Load()
	if err != nil {
		return "", nil, nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	sel := ""
	if sec, ok := userCfg["references"]; ok {
		if v, ok := sec["default_selector"]; ok {
			sel = v.Str
		}
	}
	canonical, err := Canonicalize(arg, sel)
	if err != nil {
		return "", nil, nil, err
	}
	if configFile == "" {
		return canonical, userCfg, config.File{}, nil
	}
	b, err := os.ReadFile(configFile)
	if err != nil {
		return "", nil, nil, fmt.Errorf("E_CONFIG: read --config: %w", err)
	}
	layer3, err := config.Parse(configFile, string(b))
	if err != nil {
		return "", nil, nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	return canonical, userCfg, layer3, nil
}
