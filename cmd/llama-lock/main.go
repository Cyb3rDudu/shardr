// llama-lock is the single-truth CLI over runtime/llama.lock, used by
// the Makefile, the CI workflows and humans (manual updates). The pin is
// an upstream PREBUILT b-release (project decision 2026-09-05: never
// self-build); assets are digest-pinned per platform.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	llamalock "github.com/Cyb3rDudu/shardr/internal/llamalock"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: llama-lock <command> [flags]

commands:
  ref                     print the pinned bNNNN release
  platform                print the shardr platform for this host (goos_goarch)
  validate [file]         fail-closed lockfile validation (default runtime/llama.lock)
  verify                  provenance proof: tag -> locked commit AND the GitHub
                          release API digests match the pinned asset sha256s
  latest-nightly          newest bNNNN release with a complete asset matrix (JSON)
  latest-pinnable         newest bNNNN release at least 7 days old (JSON) — pin candidate
  resolve <bNNNN>         resolve a release to JSON (ref, commit, assets, published_at)
  fetch <platform> <destdir> [--ref bNNNN]
                          download the pinned prebuilt asset (digest-verified,
                          traversal-safe extract); --ref fetches a canary release
                          with digests straight from the release API
  check-update [--write] [--ref bNNNN]
                          compare latest-pinnable (or --ref) with the lock:
                          "noop <ref>" or "update <old> <new>"; --write rewrites
                          runtime/llama.lock; --allow-downgrade explicitly pins
                          an OLDER bNNNN (rollback)`)
	os.Exit(2)
}

type resolved struct {
	Ref          string            `json:"ref"`
	Commit       string            `json:"commit"`
	PublishedAt  string            `json:"published_at"`
	SourceSHA256 map[string]string `json:"asset_sha256"`
	Assets       map[string]string `json:"asset_url"`
}

func resolveRef(ctx context.Context, ref string) (resolved, error) {
	commit, err := llamalock.ResolveTag(ctx, ref)
	if err != nil {
		return resolved{}, err
	}
	// ONE release-API request serves both the digests and the timestamp
	// (the old shape fetched the release twice).
	rel, err := llamalock.FetchRelease(ctx, ref)
	if err != nil {
		return resolved{}, err
	}
	digests, err := rel.AssetDigests()
	if err != nil {
		return resolved{}, err
	}
	pub := rel.PublishedAt
	urls := map[string]string{}
	for p := range digests {
		urls[p] = llamalock.AssetURLFor(ref, p)
	}
	return resolved{Ref: ref, Commit: commit, PublishedAt: pub.Format(time.RFC3339), SourceSHA256: digests, Assets: urls}, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runFetch(ctx context.Context, platform, dest, refOverride string) error {
	var url, sha string
	if refOverride != "" {
		if !llamalock.IsNightly(refOverride) {
			return fmt.Errorf("--ref %q is not a bNNNN release", refOverride)
		}
		digests, err := llamalock.ReleaseAssets(ctx, refOverride)
		if err != nil {
			return err
		}
		url, sha = llamalock.AssetURLFor(refOverride, platform), digests[platform]
	} else {
		lk, err := llamalock.Load()
		if err != nil {
			return err
		}
		a, ok := lk.Assets[platform]
		if !ok {
			return fmt.Errorf("no pinned asset for %s", platform)
		}
		// Cross-check the pin against the live release API: a re-uploaded
		// asset with different bytes fails here, before anything runs.
		digests, err := llamalock.ReleaseAssets(ctx, lk.Ref)
		if err != nil {
			return err
		}
		if digests[platform] != a.SHA256 {
			return fmt.Errorf("E_PROVENANCE: release-API digest for %s is %s, lock pins %s", platform, digests[platform], a.SHA256)
		}
		url, sha = a.URL, a.SHA256
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tarPath, err := llamalock.DownloadAsset(ctx, url, sha, platform)
	if err != nil {
		return err
	}
	defer os.Remove(tarPath)
	prefix, err := llamalock.ExtractAsset(tarPath, dest)
	if err != nil {
		return err
	}
	fmt.Printf("fetched %s -> %s/%s (sha256 %s)\n", platform, dest, prefix, sha[:16]+"…")
	return nil
}

// parseFetchArgs accepts --ref BEFORE or AFTER the two positional args
// (the standard Go flag package stops at the first positional, which
// silently broke the documented `fetch <platform> <destdir> --ref bN`
// syntax — canary dispatches used exactly that order).
func parseFetchArgs(args []string) (platform, dest, ref string, err error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ref", "-ref":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("--ref needs a value")
			}
			i++
			ref = args[i]
		case "--ref=*", "-ref=*":
			ref = strings.TrimPrefix(args[i], "--ref=")
		default:
			if strings.HasPrefix(args[i], "-") && args[i] != "-" && !strings.Contains(args[i], "=") {
				return "", "", "", fmt.Errorf("unknown flag %q", args[i])
			}
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 2 {
		return "", "", "", fmt.Errorf("fetch wants exactly <platform> <destdir> (+ optional --ref bNNNN), got %d positional args", len(pos))
	}
	return pos[0], pos[1], ref, nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "ref":
		lk, err := llamalock.Load()
		if err != nil {
			fatal(err)
		}
		fmt.Println(lk.Ref)

	case "platform":
		p, err := llamalock.HostPlatform()
		if err != nil {
			fatal(err)
		}
		fmt.Println(p)

	case "validate":
		path := llamalock.Path
		if len(args) == 1 {
			path = args[0]
		} else if len(args) > 1 {
			usage()
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		lk, err := llamalock.Parse(data)
		if err != nil {
			fatal(err)
		}
		printJSON(lk)

	case "verify":
		if len(args) != 0 {
			usage()
		}
		lk, err := llamalock.Load()
		if err != nil {
			fatal(err)
		}
		commit, err := llamalock.ResolveTag(ctx, lk.Ref)
		if err != nil {
			fatal(err)
		}
		if commit != lk.Commit {
			fatal(fmt.Errorf("E_PROVENANCE: tag %s now points at %s, lock pins %s — tag moved upstream", lk.Ref, commit, lk.Commit))
		}
		digests, err := llamalock.ReleaseAssets(ctx, lk.Ref)
		if err != nil {
			fatal(err)
		}
		for _, p := range llamalock.Platforms {
			if digests[p] != lk.Assets[p].SHA256 {
				fatal(fmt.Errorf("E_PROVENANCE: release-API digest for %s is %s, lock pins %s", p, digests[p], lk.Assets[p].SHA256))
			}
		}
		fmt.Printf("provenance OK: %s -> %s (assets verified on %s)\n", lk.Ref, lk.Commit, strings.Join(llamalock.Platforms, ", "))

	case "fetch":
		platform, dest, ref, err := parseFetchArgs(args)
		if err != nil {
			fatal(err)
		}
		if err := runFetch(ctx, platform, dest, ref); err != nil {
			fatal(err)
		}

	case "latest-nightly":
		tag, err := llamalock.LatestNightlyTag(ctx)
		if err != nil {
			fatal(err)
		}
		r, err := resolveRef(ctx, tag)
		if err != nil {
			fatal(err)
		}
		printJSON(r)

	case "latest-pinnable":
		p, err := llamalock.NewestPinnableBRelease(ctx, time.Now())
		if err != nil {
			fatal(err)
		}
		printJSON(pinnableToResolved(p))

	case "resolve":
		fs := flag.NewFlagSet("resolve", flag.ExitOnError)
		fs.Parse(args)
		if fs.NArg() != 1 {
			usage()
		}
		r, err := resolveRef(ctx, fs.Arg(0))
		if err != nil {
			fatal(err)
		}
		printJSON(r)

	case "check-update":
		fs := flag.NewFlagSet("check-update", flag.ExitOnError)
		write := fs.Bool("write", false, "rewrite runtime/llama.lock on change")
		ref := fs.String("ref", "", "manual override: exact bNNNN release, must be >=7 days old")
		allowDown := fs.Bool("allow-downgrade", false, "explicit rollback: allow pinning an OLDER bNNNN")
		fs.Parse(args)
		lk, err := llamalock.Load()
		if err != nil {
			fatal(err)
		}
		// Both paths validate the release ONCE and write exactly the
		// validated snapshot's digests — a second fetch between soak
		// check and lock write would be a TOCTOU hole (swapped asset
		// digests entering the pin unsoaked).
		var pin llamalock.Pinnable
		if *ref != "" {
			p, err := llamalock.ValidatePinnableRelease(ctx, *ref, time.Now())
			if err != nil {
				fatal(fmt.Errorf("--ref rejected: %w", err))
			}
			pin = p
		} else {
			p, err := llamalock.NewestPinnableBRelease(ctx, time.Now())
			if err != nil {
				fatal(err)
			}
			pin = p
		}
		update, err := llamalock.Decide(lk.Ref, pin.Ref, *allowDown)
		if err != nil {
			fatal(err)
		}
		if !update {
			fmt.Printf("noop %s\n", lk.Ref)
			return
		}
		if *write {
			root, err := llamalock.FindRepoRoot()
			if err != nil {
				fatal(err)
			}
			nl := llamalock.Lock{Ref: pin.Ref, Commit: pin.Commit, UpdatedAt: llamalock.Now()}
			nl.Assets = map[string]llamalock.Asset{}
			for _, p := range llamalock.Platforms {
				nl.Assets[p] = llamalock.Asset{Platform: p, URL: llamalock.AssetURLFor(pin.Ref, p), SHA256: pin.Digests[p]}
			}
			if err := os.WriteFile(filepath.Join(root, llamalock.Path), nl.Format(), 0o644); err != nil {
				fatal(err)
			}
		}
		fmt.Printf("update %s %s\n", lk.Ref, pin.Ref)

	default:
		usage()
	}
}

// pinnableToResolved renders a validated snapshot in the resolved-JSON shape.
func pinnableToResolved(p llamalock.Pinnable) resolved {
	urls := map[string]string{}
	for plat := range p.Digests {
		urls[plat] = llamalock.AssetURLFor(p.Ref, plat)
	}
	return resolved{Ref: p.Ref, Commit: p.Commit, SourceSHA256: p.Digests, Assets: urls}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "llama-lock: %v\n", err)
	os.Exit(1)
}
