// Command shardr is the user-facing binary (005 §1): the model runner
// (002) and the shardhive management CLI — docker/dockerd split; this
// binary is a client of the daemon API like any other.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Cyb3rDudu/shardr/internal/cli"
)

var version = "0.1.0-dev"

const usage = `shardr — model runner & shardhive CLI (005 §4)

Usage:
  shardr pull <ref>                    fill the CAS (import/swarm), no run
  shardr import local <paths> --as ns/name
  shardr import hf <repo> [--rev <sha>]
  shardr import bt <magnet> --manifest <sha256:…>
  shardr models                        inventory
  shardr verify <ref|digest|--all>     integrity re-hash
  shardr status [job]                  job / recent jobs
  shardr run <ref> [--config f.toml] [--set k=v]…
                                       foreground model (Ctrl-C = SIGTERM)
  shardr serve <ref> [--id name] [--config f.toml] [--set k=v]…
                                       background instance, stable id
  shardr stop [<id>|--all]             SIGTERM ≤ 30 s, then SIGKILL

Short refs (ns/name:quant) canonicalize internally; the API only ever
sees the canonical shardr:/// URI (000 §2). Runtime keys are validated
against the 002 §7.1 allowlist — unknown keys fail loudly.

Version %s
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Printf(usage, version)
		return 0
	}
	if args[0] == "version" {
		fmt.Println("shardr", version)
		return 0
	}
	ctx := context.Background()
	switch args[0] {
	case "pull":
		return withClient(func(c *cli.Client) error {
			if len(args) != 2 {
				return fmt.Errorf("E_BAD_REQUEST: pull needs exactly one ref")
			}
			return cli.Pull(ctx, c, args[1], os.Stdout)
		})
	case "import":
		return cmdImport(ctx, args[1:])
	case "models":
		return withClient(func(c *cli.Client) error { return cli.Models(ctx, c, os.Stdout) })
	case "verify":
		return withClient(func(c *cli.Client) error {
			if len(args) != 2 {
				return fmt.Errorf("E_BAD_REQUEST: verify needs a ref, a digest, or --all")
			}
			return cli.Verify(ctx, c, args[1], os.Stdout)
		})
	case "status":
		return withClient(func(c *cli.Client) error {
			job := ""
			if len(args) == 2 {
				job = args[1]
			}
			return cli.Status(ctx, c, job, os.Stdout)
		})
	case "run":
		return cmdLifecycle(ctx, args[1:], false)
	case "serve":
		return cmdLifecycle(ctx, args[1:], true)
	case "stop":
		return cmdStop(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "shardr: E_BAD_REQUEST: unknown command %q\n", args[0])
		fmt.Fprint(os.Stderr, "run `shardr help` for usage\n")
		return 2
	}
}

func withClient(fn func(*cli.Client) error) int {
	c, err := cli.NewClient()
	if err != nil {
		return fail(err)
	}
	return fail(fn(c))
}

func fail(err error) int {
	if err == nil {
		return 0
	}
	// Every CLI error carries a code + human cause (strand rule 6);
	// API/job errors pass through unchanged.
	fmt.Fprintf(os.Stderr, "shardr: %s\n", err.Error())
	return 1
}

func cmdImport(ctx context.Context, args []string) int {
	if len(args) < 1 {
		return fail(fmt.Errorf("E_BAD_REQUEST: import needs a source: local | hf | bt"))
	}
	switch args[0] {
	case "local":
		var as string
		var paths []string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--as":
				i++
				if i < len(args) {
					as = args[i]
				}
			default:
				paths = append(paths, args[i])
			}
		}
		return withClient(func(c *cli.Client) error { return cli.ImportLocal(ctx, c, paths, as, os.Stdout) })
	case "hf":
		var repo, rev string
		positional := args[1:]
		for i := 0; i < len(positional); i++ {
			if positional[i] == "--rev" && i+1 < len(positional) {
				rev = positional[i+1]
				positional = append(positional[:i], positional[i+1:]...)
				break
			}
		}
		if len(positional) > 0 {
			repo = positional[0]
		}
		return withClient(func(c *cli.Client) error { return cli.ImportHF(ctx, c, repo, rev, os.Stdout) })
	case "bt":
		var src, manifest string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--manifest":
				i++
				if i < len(args) {
					manifest = args[i]
				}
			default:
				src = args[i]
			}
		}
		return withClient(func(c *cli.Client) error { return cli.ImportBT(ctx, c, src, manifest, os.Stdout) })
	default:
		return fail(fmt.Errorf("E_BAD_REQUEST: unknown import source %q (local | hf | bt)", args[0]))
	}
}

func cmdLifecycle(ctx context.Context, args []string, serve bool) int {
	var refArg string
	var opts cli.RunOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			i++
			if i < len(args) {
				opts.ID = args[i]
			}
		case "--config":
			i++
			if i < len(args) {
				opts.ConfigFile = args[i]
			}
		case "--set":
			i++
			if i < len(args) {
				opts.Sets = append(opts.Sets, args[i])
			}
		default:
			if refArg != "" {
				return fail(fmt.Errorf("E_BAD_REQUEST: exactly one ref expected, got %q and %q", refArg, args[i]))
			}
			refArg = args[i]
		}
	}
	if refArg == "" {
		return fail(fmt.Errorf("E_BAD_REQUEST: %s needs a ref", map[bool]string{true: "serve", false: "run"}[serve]))
	}
	return withClient(func(c *cli.Client) error {
		if serve {
			return cli.Serve(ctx, c, refArg, opts, os.Stdout)
		}
		return cli.Run(ctx, c, refArg, opts, os.Stdout)
	})
}

func cmdStop(ctx context.Context, args []string) int {
	var id string
	all := false
	for _, a := range args {
		if a == "--all" {
			all = true
			continue
		}
		id = a
	}
	if !all && id == "" {
		// No argument: list running instances (comfort, not error).
		return withClient(func(c *cli.Client) error {
			return cli.Stop(ctx, c, "", false, os.Stdout)
		})
	}
	return withClient(func(c *cli.Client) error { return cli.Stop(ctx, c, id, all, os.Stdout) })
}
