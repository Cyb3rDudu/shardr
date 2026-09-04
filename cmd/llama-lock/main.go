// llama-lock is the single-truth CLI over runtime/llama.lock, used by
// the Makefile, the CI workflows and humans (manual updates).
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
  ref                       print the pinned stable ref
  validate [file]           fail-closed lockfile validation (default runtime/llama.lock)
  latest                    resolve the newest upstream vX.Y.Z (JSON: ref, commit, source_url, source_sha256)
  latest-nightly            resolve the newest upstream bNNNN (JSON, canary channel only)
  resolve <tag>             resolve an exact vX.Y.Z or bNNNN tag to JSON
  check-update [--write] [--ref vX.Y.Z]
                            compare upstream (or --ref) with the lock:
                            "noop <ref>" or "update <old> <new>"; --write rewrites runtime/llama.lock`)
	os.Exit(2)
}

type resolved struct {
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
	SourceURL    string `json:"source_url"`
	SourceSHA256 string `json:"source_sha256"`
}

func resolveRef(ctx context.Context, ref string) (resolved, error) {
	commit, err := llamalock.ResolveTag(ctx, ref)
	if err != nil {
		return resolved{}, err
	}
	digest, err := llamalock.ArchiveSHA256(ctx, commit)
	if err != nil {
		return resolved{}, err
	}
	return resolved{Ref: ref, Commit: commit, SourceURL: llamalock.SourceURLFor(commit), SourceSHA256: digest}, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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

	case "latest":
		tag, err := llamalock.LatestStableTag(ctx)
		if err != nil {
			fatal(err)
		}
		r, err := resolveRef(ctx, tag)
		if err != nil {
			fatal(err)
		}
		printJSON(r)

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
		ref := fs.String("ref", "", "manual override: exact vX.Y.Z tag (workflow_dispatch)")
		fs.Parse(args)
		lk, err := llamalock.Load()
		if err != nil {
			fatal(err)
		}
		var latest string
		if *ref != "" {
			if !llamalock.IsStable(*ref) {
				fatal(fmt.Errorf("--ref %q rejected: manual override accepts exact vX.Y.Z tags only", *ref))
			}
			latest = *ref
		} else {
			latest, err = llamalock.LatestStableTag(ctx)
			if err != nil {
				fatal(err)
			}
		}
		update, err := llamalock.Decide(lk.Ref, latest)
		if err != nil {
			fatal(err) // e.g. bNNNN leaked into the stable channel
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
			nl := llamalock.Lock{Ref: r.Ref, Commit: r.Commit, SourceURL: r.SourceURL, SourceSHA256: r.SourceSHA256, UpdatedAt: llamalock.Now()}
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
