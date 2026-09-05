// llama-lock is the single-truth CLI over runtime/llama.lock, used by
// the Makefile, the CI workflows and humans (manual updates). The pin is
// an upstream PREBUILT b-release (owner ruling 2026-09-05: never
// self-build); assets are digest-pinned per platform.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	llamalock "github.com/Cyb3rDudu/shardr/internal/llamalock"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: llama-lock <command> [flags]

commands:
  ref                     print the pinned bNNNN release
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
                          "noop <ref>" or "update <old> <new>"; --write rewrites runtime/llama.lock`)
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
	digests, err := llamalock.ReleaseAssets(ctx, ref)
	if err != nil {
		return resolved{}, err
	}
	pub, err := llamalock.ReleasePublishedAt(ctx, ref)
	if err != nil {
		return resolved{}, err
	}
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
	tarPath := filepath.Join(os.TempDir(), fmt.Sprintf("llama-asset-%s.tar.gz", platform))
	if err := llamalock.DownloadAsset(ctx, llamalock.Lock{Assets: map[string]llamalock.Asset{platform: {URL: url, SHA256: sha}}}, platform, tarPath); err != nil {
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
		fmt.Printf("provenance OK: %s -> %s (assets verified on darwin_arm64, linux_amd64)\n", lk.Ref, lk.Commit)

	case "fetch":
		fs := flag.NewFlagSet("fetch", flag.ExitOnError)
		ref := fs.String("ref", "", "canary override: exact bNNNN release (digests from the release API)")
		fs.Parse(args)
		if fs.NArg() != 2 {
			usage()
		}
		platform, dest := fs.Arg(0), fs.Arg(1)
		if err := runFetch(ctx, platform, dest, *ref); err != nil {
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
		tag, err := llamalock.NewestPinnableBRelease(ctx, time.Now())
		if err != nil {
			fatal(err)
		}
		r, err := resolveRef(ctx, tag)
		if err != nil {
			fatal(err)
		}
		printJSON(r)

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
		fs.Parse(args)
		lk, err := llamalock.Load()
		if err != nil {
			fatal(err)
		}
		var latest string
		if *ref != "" {
			if !llamalock.IsNightly(*ref) {
				fatal(fmt.Errorf("--ref %q rejected: pin accepts exact bNNNN releases only", *ref))
			}
			pub, err := llamalock.ReleasePublishedAt(ctx, *ref)
			if err != nil {
				fatal(err)
			}
			if time.Since(pub) < llamalock.MinAge {
				fatal(fmt.Errorf("--ref %q rejected: released %s, pin requires >= %s soak time", *ref, pub.Format(time.RFC3339), llamalock.MinAge))
			}
			latest = *ref
		} else {
			latest, err = llamalock.NewestPinnableBRelease(ctx, time.Now())
			if err != nil {
				fatal(err)
			}
		}
		update, err := llamalock.Decide(lk.Ref, latest)
		if err != nil {
			fatal(err)
		}
		if !update {
			fmt.Printf("noop %s\n", lk.Ref)
			return
		}
		r, err := resolveRef(ctx, latest)
		if err != nil {
			fatal(err)
		}
		if *write {
			root, err := llamalock.FindRepoRoot()
			if err != nil {
				fatal(err)
			}
			nl := llamalock.Lock{Ref: r.Ref, Commit: r.Commit, UpdatedAt: llamalock.Now()}
			nl.Assets = map[string]llamalock.Asset{}
			for _, p := range llamalock.Platforms {
				nl.Assets[p] = llamalock.Asset{Platform: p, URL: llamalock.AssetURLFor(r.Ref, p), SHA256: r.SourceSHA256[p]}
			}
			if err := os.WriteFile(filepath.Join(root, llamalock.Path), nl.Format(), 0o644); err != nil {
				fatal(err)
			}
		}
		fmt.Printf("update %s %s\n", lk.Ref, r.Ref)

	default:
		usage()
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "llama-lock: %v\n", err)
	os.Exit(1)
}
