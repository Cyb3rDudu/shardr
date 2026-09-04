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
				return fmt.Errorf("E_BAD_REQUEST: pull needs exactly one ref, got %d arguments", len(args)-1)
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
				return fmt.Errorf("E_BAD_REQUEST: verify needs exactly one target (ref, digest, or --all)")
			}
			return cli.Verify(ctx, c, args[1], os.Stdout)
		})
	case "status":
		if len(args) > 2 {
			return fail(fmt.Errorf("E_BAD_REQUEST: status takes at most one job id, got %d extra arguments", len(args)-2))
		}
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
				if i+1 >= len(args) {
					return fail(fmt.Errorf("E_BAD_REQUEST: --as needs a value (it is the last argument)"))
				}
				i++
				as = args[i]
			default:
				paths = append(paths, args[i])
			}
		}
		return withClient(func(c *cli.Client) error { return cli.ImportLocal(ctx, c, paths, as, os.Stdout) })
	case "hf":
		var repo, rev string
		positional := args[1:]
		for i := 0; i < len(positional); i++ {
			if positional[i] == "--rev" {
				if i+1 >= len(positional) {
					return fail(fmt.Errorf("E_BAD_REQUEST: --rev needs a value (it is the last argument)"))
				}
				rev = positional[i+1]
				positional = append(append([]string{}, positional[:i]...), positional[i+2:]...)
				break
			}
		}
		if len(positional) > 1 {
			return fail(fmt.Errorf("E_BAD_REQUEST: import hf takes exactly one repo, got %d extra arguments", len(positional)-1))
		}
		if len(positional) == 1 {
			repo = positional[0]
		}
		return withClient(func(c *cli.Client) error { return cli.ImportHF(ctx, c, repo, rev, os.Stdout) })
	case "bt":
		var src, manifest string
		var positionals []string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--manifest":
				if i+1 >= len(args) {
					return fail(fmt.Errorf("E_BAD_REQUEST: --manifest needs a value (it is the last argument)"))
				}
				i++
				manifest = args[i]
			default:
				positionals = append(positionals, args[i])
			}
		}
		if len(positionals) > 1 {
			return fail(fmt.Errorf("E_BAD_REQUEST: import bt takes exactly one magnet/infohash, got %d", len(positionals)))
		}
		if len(positionals) == 1 {
			src = positionals[0]
		}
		return withClient(func(c *cli.Client) error { return cli.ImportBT(ctx, c, src, manifest, os.Stdout) })
	default:
		return fail(fmt.Errorf("E_BAD_REQUEST: unknown import source %q (local | hf | bt)", args[0]))
	}
}

func cmdLifecycle(ctx context.Context, args []string, serve bool) int {
	var refArg string
	var opts cli.RunOptions
	flagValue := func(name string, i int) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("E_BAD_REQUEST: %s needs a value (it is the last argument)", name)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			if !serve {
				return fail(fmt.Errorf("E_BAD_REQUEST: --id is serve-only (foreground run has no stable id)"))
			}
			v, err := flagValue(args[i], i)
			if err != nil {
				return fail(err)
			}
			opts.ID = v
			i++
		case "--config":
			v, err := flagValue(args[i], i)
			if err != nil {
				return fail(err)
			}
			opts.ConfigFile = v
			i++
		case "--set":
			v, err := flagValue(args[i], i)
			if err != nil {
				return fail(err)
			}
			opts.Sets = append(opts.Sets, v)
			i++
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
	var ids []string
	all := false
	for _, a := range args {
		if a == "--all" {
			all = true
			continue
		}
		ids = append(ids, a)
	}
	if all && len(ids) > 0 {
		return fail(fmt.Errorf("E_BAD_REQUEST: stop --all takes no id (got %q)", ids[0]))
	}
	if len(ids) > 1 {
		return fail(fmt.Errorf("E_BAD_REQUEST: stop takes at most one id, got %d", len(ids)))
	}
	id := ""
	if len(ids) == 1 {
		id = ids[0]
	}
	if !all && id == "" {
		// No argument: list running instances (comfort, not error).
		return withClient(func(c *cli.Client) error {
			return cli.Stop(ctx, c, "", false, os.Stdout)
		})
	}
	return withClient(func(c *cli.Client) error { return cli.Stop(ctx, c, id, all, os.Stdout) })
}
