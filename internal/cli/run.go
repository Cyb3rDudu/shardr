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
	// Per-launch scratch cleanup on every foreground exit path.
	defer func() {
		if rt.SplitDir != "" {
			removeSplitDir(rt.SplitDir)
		}
	}()
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
		// PID-reuse guard, TWO factors: a stale registry entry after a
		// crash/reboot could name an unrelated recycled PID — SIGTERM
		// (+SIGKILL after 30 s) would kill it. (1) Start token: the live
		// process's start identity must match the recorded one (a
		// recycled pid never does). (2) Endpoint: /v1/models must serve
		// the registered ref (--alias makes the real runtime do exactly
		// that). Either mismatch → refuse and instruct manual cleanup.
		if !identityOK(ctx, *in) {
			return fmt.Errorf("E_STATE: refusing to stop %s: pid %d fails identity verification (start token or served ref mismatch — stale entry or pid reuse?) — verify and clean %s manually",
				in.ID, in.PID, reg.Path())
		}
		fmt.Fprintf(out, "stopping %s (pid %d)…\n", in.ID, in.PID)
		// TerminateVerified re-checks identity immediately before SIGTERM
		// AND before SIGKILL (the 30 s window can recycle a dead pid).
		if err := runner.TerminateVerified(in.PID, func() bool { return identityOK(ctx, *in) }); err != nil {
			return err
		}
		// Split-scratch cleanup (per-launch dir — only THIS instance's
		// links live there). The stored path is VALIDATED against the
		// recomputed scratch root and expected shape before any removal:
		// a corrupted or tampered registry must never turn stop into a
		// recursive delete of an arbitrary directory.
		if in.SplitDir != "" && !removeSplitDir(in.SplitDir) {
			fmt.Fprintf(out, "warning: split dir %q failed validation — left in place, remove manually if stale\n", in.SplitDir)
		}
		if err := reg.Remove(tid); err != nil {
			return err
		}
		fmt.Fprintf(out, "stopped %s\n", tid)
	}
	return nil
}

// identityOK is the two-factor guard (start token + served ref),
// evaluated LIVE — used before stop and re-checked before every signal.
func identityOK(ctx context.Context, in runner.Instance) bool {
	liveToken, terr := runner.ProcessStartToken(in.PID)
	if terr != nil || in.StartToken == "" || liveToken != in.StartToken {
		return false
	}
	id, ok := probeModelID(ctx, in.Endpoint)
	return ok && id == in.Ref
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
	Ref      string
	ID       string
	SplitDir string
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
		if l.SplitDir != "" {
			removeSplitDir(l.SplitDir)
		}
		return nil, err
	}
	// Start token at serve time: pid-reuse identity for every later stop
	// (the endpoint probe alone cannot distinguish a recycled pid that
	// happens to serve the same ref).
	token, terr := runner.ProcessStartToken(l.Runtime.PID())
	if terr != nil {
		l.Runtime.Terminate()
		if l.SplitDir != "" {
			removeSplitDir(l.SplitDir)
		}
		return nil, terr
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
		StartToken: token, SplitDir: l.SplitDir,
	}); err != nil {
		l.Runtime.Terminate()
		if l.SplitDir != "" {
			removeSplitDir(l.SplitDir)
		}
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

	// Ensure: CAS hit or fill job; then open for the file list. Terminal
	// states resolve HERE — a synchronous failed surfaces its job error
	// immediately (never masked by a confusing /open failure), done
	// proceeds; every non-terminal state (waiting/fetching/seeding, 005
	// §3) is polled to its terminal state.
	var job Job
	if err := c.DoJSON(ctx, http.MethodPost, "/v1/ensure", map[string]string{"ref": canonical}, &job); err != nil {
		return nil, err
	}
	term := &job
	switch job.State {
	case "failed":
		return nil, job.Error
	case "done":
	default: // waiting | fetching | seeding | anything future non-terminal
		fmt.Fprintf(out, "filling %s\n", canonical)
		last := ""
		var werr error
		term, werr = c.WaitJob(ctx, job.ID, func(j Job) {
			bar := fmt.Sprintf("%d/%d", j.FilesDone, j.FilesTotal)
			if bar != last {
				fmt.Fprintf(out, "\r  %s %s", j.State, bar)
				last = bar
			}
		})
		if werr != nil {
			return nil, werr
		}
		fmt.Fprintln(out)
		if term.State == "failed" {
			return nil, term.Error
		}
	}
	_ = term
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
	splitDirUsed := ""                      // set when a split scratch dir was linked
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
		// CAS names never match. Link the parts under their ORIGINAL
		// manifest names into a PER-LAUNCH scratch dir: a hardlink or
		// symlink copies zero bytes (zero-copy law intact, 002 §4).
		splitDir, serr := splitScratchDir(open.ManifestDigest)
		if serr != nil {
			return nil, serr
		}
		// Error paths from here on must not leak the scratch dir.
		ok := false
		defer func() {
			if !ok {
				os.RemoveAll(splitDir)
			}
		}()
		for _, p := range parts {
			dst := filepath.Join(splitDir, p.Name)
			// Manifest names may be nested ("dir/part.gguf") — the
			// parent must exist before linking.
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return nil, fmt.Errorf("E_RUNTIME: split part dir %s: %w", p.Name, err)
			}
			if err := linkPart(p.Path, dst); err != nil {
				return nil, fmt.Errorf("E_RUNTIME: link split part %s: %w", p.Name, err)
			}
		}
		weights.Path = filepath.Join(splitDir, parts[0].Name)
		splitDirUsed = splitDir
		ok = true
	}

	bin, err := runner.ResolveBinary()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "spawn %s\n  weights %s (zero-copy mmap)\n", bin, weights.Path)
	_ = splitDirUsed
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
	return &launched{Runtime: rt, Ref: canonical, ID: idFor(opts.ID, p), SplitDir: splitDirUsed}, nil
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

// splitRoot resolves the scratch ROOT ($XDG_RUNTIME_DIR or the per-uid
// tmp fallback, mirroring the registry rules). All scratch state lives
// under <root>/shardr-split/ — the ONLY tree stop ever removes.
func splitRoot() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		uid := "0"
		if u, uerr := user.Current(); uerr == nil {
			uid = u.Uid
		}
		base = filepath.Join(os.TempDir(), "shardr-"+uid)
	}
	return filepath.Join(base, "shardr-split")
}

// splitScratchDir is the PER-LAUNCH hardlink home for split GGUFs:
// <root>/shardr-split/<manifest-hex>/<launch-id>. Every spawn gets its
// own directory — two processes serving the same model never share
// link state, so cleanup of one launch cannot touch another (the
// shared per-manifest dir was a live-reproduced cross-process hole).
func splitScratchDir(manifestDigest string) (string, error) {
	launch := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	dir := filepath.Join(splitRoot(), strings.TrimPrefix(manifestDigest, "sha256:"), launch)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("E_STATE: split scratch dir: %w", err)
	}
	return dir, nil
}

// validSplitDir recomputes the scratch root and validates a STORED
// SplitDir from the persistent registry before anything removes it: it
// must be a relative descendant of the root with the exact expected
// shape <root>/<64-hex>/<non-empty-launch>. A corrupted or tampered
// registry entry then names an unremovable path — never a recursive
// delete of an arbitrary directory.
func validSplitDir(stored string) bool {
	if stored == "" {
		return false
	}
	rel, err := filepath.Rel(splitRoot(), stored)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	segs := strings.Split(rel, string(filepath.Separator))
	if len(segs) != 2 || !isHex64(segs[0]) || segs[1] == "" || strings.Contains(segs[1], string(filepath.Separator)) {
		return false
	}
	fi, err := os.Lstat(stored)
	return err == nil && fi.IsDir()
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// removeSplitDir removes a stored split dir after validation; returns
// whether removal happened (invalid/foreign paths are left untouched
// and reported).
func removeSplitDir(stored string) bool {
	if !validSplitDir(stored) {
		return false
	}
	os.RemoveAll(stored)
	return true
}

// linkPart places one split part under its original name: hardlink
// preferred (shared bytes), symlink on EXDEV (CAS and scratch on
// different filesystems — zero bytes either way). An existing entry is
// only accepted when it verifiably points at the SAME CAS source
// (hardlink: same inode via os.SameFile; symlink: exact target) —
// anything else is replaced.
func linkPart(srcPath, dst string) error {
	os.Remove(dst)
	if err := os.Link(srcPath, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EEXIST) && !errors.Is(err, syscall.EXDEV) {
		return err
	} else if errors.Is(err, syscall.EXDEV) {
		return os.Symlink(srcPath, dst)
	}
	// EEXIST (raced re-creation between our Remove and Link): accept the
	// existing entry only with proven identity.
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, rerr := os.Readlink(dst); rerr == nil && target == srcPath {
				return nil
			}
		} else if sfi, serr := os.Stat(srcPath); serr == nil {
			if dfi, derr := os.Stat(dst); derr == nil && os.SameFile(dfi, sfi) {
				return nil
			}
		}
	}
	os.Remove(dst)
	if err := os.Link(srcPath, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return os.Symlink(srcPath, dst)
		}
		return err
	}
	return nil
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
