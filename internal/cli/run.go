package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
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
}

// Run executes the foreground lifecycle (002 §4): resolve → ensure →
// merge overlays → spawn llama-server (weights as CAS path, zero-copy)
// → readiness → attach signals (Ctrl-C = SIGTERM discipline).
func Run(ctx context.Context, c *Client, arg string, opts RunOptions, out io.Writer) error {
	rt, err := launch(ctx, c, arg, opts, out)
	if err != nil {
		return err
	}
	defer rt.Terminate()
	fmt.Fprintf(out, "ready: %s (model id %s) — Ctrl-C to stop\n", rt.Endpoint(), rt.Ref)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		fmt.Fprintln(out, "\nstopping (SIGTERM; SIGKILL after 30 s)")
	case <-ctx.Done():
	}
	return rt.Terminate()
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
	canonical, cfgFile, err := canonicalizeWithConfig(arg, opts.ConfigFile)
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
	// Overlays (002 §2): advisory ← runtime-config blob via /blob.
	ov := runner.Overlay{UserConfig: runner.UserConfigOverlay(cfgFile, "llama", shortRefOf(p))}
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
	weights, mmproj, warns, err := pickWeights(open.Files, variant)
	for _, w := range warns {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	if err != nil {
		return nil, err
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
	if err != nil {
		return nil, err
	}
	return &launched{Runtime: rt, Ref: canonical, ID: idFor(opts.ID, p)}, nil
}

// pickWeights selects the weights.gguf file and the vision-projector aux
// (002 §5.7): variant from mmproj_variant (default f16); a multimodal
// artifact without a projector warns; unknown aux roles log and continue.
func pickWeights(files []openFile, variant string) (weights openFile, mmproj string, warns []string, err error) {
	if variant == "" {
		variant = "f16" // 002 §5.7 default
	}
	anyProjector := false
	match := false
	var parts []openFile
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
		return weights, "", warns, fmt.Errorf("E_NOT_IMPORTABLE: artifact has no weights.gguf file (llama runtime, 002 §5.1)")
	}
	// Split GGUFs (001 §3.1 Part field): one logical weights set in N
	// ordered parts — llama-server takes part 1 via -m and discovers the
	// siblings by its own naming convention. Contiguity was validated at
	// import; parts of a split share the CAS directory.
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].Part == nil || parts[j].Part == nil {
			return false
		}
		return *parts[i].Part < *parts[j].Part
	})
	weights = parts[0]
	if anyProjector && !match {
		// Multimodal artifact without the selected projector variant: a
		// warning, never an abort (002 §5.7).
		warns = append(warns, "vision-projector variant "+variant+" not found — serving text-only")
	}
	return weights, mmproj, warns, nil
}

func advisory(ctx context.Context, c *Client, files []openFile) (map[string]config.Value, error) {
	for _, f := range files {
		if f.Kind != "runtime-config" {
			continue
		}
		resp, err := c.Do(ctx, http.MethodGet, "/v1/blob/"+strings.TrimPrefix(f.Digest, "sha256:"), nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("E_INTERNAL: read advisory blob: %w", err)
		}
		return runner.ParseScalars(b)
	}
	return nil, nil
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
func canonicalizeWithConfig(arg, configFile string) (string, config.File, error) {
	userCfg, err := config.Load()
	if err != nil {
		return "", nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	sel := ""
	if sec, ok := userCfg["references"]; ok {
		if v, ok := sec["default_selector"]; ok {
			sel = v.Str
		}
	}
	canonical, err := Canonicalize(arg, sel)
	if err != nil {
		return "", nil, err
	}
	if configFile == "" {
		return canonical, userCfg, nil
	}
	b, err := os.ReadFile(configFile)
	if err != nil {
		return "", nil, fmt.Errorf("E_CONFIG: read --config: %w", err)
	}
	layer3, err := config.Parse(configFile, string(b))
	if err != nil {
		return "", nil, fmt.Errorf("E_CONFIG: %w", err)
	}
	// Layer 3 replaces keys per section on top of layer 2 (002 §2.2).
	merged := config.File{}
	for sec, kv := range userCfg {
		merged[sec] = kv
	}
	for sec, kv := range layer3 {
		if merged[sec] == nil {
			merged[sec] = kv
			continue
		}
		for k, v := range kv {
			merged[sec][k] = v
		}
	}
	return canonical, merged, nil
}
