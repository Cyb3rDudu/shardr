package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Cyb3rDudu/shardr/internal/api"
	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// version is injected at build time via ldflags, e.g.
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.0.1-dev"

// Exit codes: usage/dispatch errors use 64 (EX_USAGE) so they are
// distinguishable from verify semantics, which stay 0 clean / 1 mismatch /
// 2 missing (spec 003 §4).
const (
	exitUsage          = 64
	exitVerifyClean    = 0
	exitVerifyMismatch = 1
	exitVerifyMissing  = 2
)

const usage = `usage: shardhive <command> [args]

commands:
  serve [--socket <path>]     run the daemon: API v1 over a Unix socket
                              (mode 0600; socket permission is the access
                              boundary)
  cas verify <digest|--all>   re-hash blobs and check digests; digests may
                              be bare hex or canonical sha256:<hex>
                              (exit 0 clean, 1 mismatch, 2 missing)
  version                     print version

usage/dispatch errors exit 64.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches CLI args and returns the process exit code.
func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		fmt.Println("shardhive", version)
		return 0
	case "serve":
		return runServe(args[1:])
	case "cas":
		if len(args) == 2 && args[1] == "verify" {
			fmt.Fprintln(os.Stderr, "usage: shardhive cas verify <digest|--all>")
			return exitUsage
		}
		if len(args) == 3 && args[1] == "verify" {
			return runVerify(args[2])
		}
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	default:
		fmt.Fprint(os.Stderr, usage)
		return exitUsage
	}
}

// runServe starts the daemon and blocks until SIGINT/SIGTERM.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "socket path (default: $SHARDR_SOCKET, $XDG_RUNTIME_DIR/shardhive.sock, or <tmp>/shardhive-<uid>/shardhive.sock)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	store, err := cas.Open("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 1
	}
	srv, err := api.New(store, *socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 1
	}
	if err := srv.Listen(); err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 1
	}
	fmt.Println("shardhive", version, "listening on", srv.Path())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		srv.Close()
	}()
	if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 1
	}
	return 0
}

// normalizeVerifyArg accepts canonical "sha256:<hex>" digests and bare hex,
// returning bare hex (the CLI-internal form). Anything else passes through
// and fails digest validation loudly.
func normalizeVerifyArg(arg string) string {
	return strings.TrimPrefix(arg, "sha256:")
}

// runVerify implements `cas verify` exit semantics per spec 003 §4:
// 0 clean, 1 mismatch, 2 missing. Mismatch outranks missing when both occur.
// State-load errors are reported on stderr but do not change the exit code
// (they are not blob corruption; see VerifyResult.StateErrors).
func runVerify(arg string) int {
	store, err := cas.Open("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return exitVerifyMissing
	}
	if arg != "--all" {
		d := strings.ToLower(normalizeVerifyArg(arg))
		if err := store.Verify(d); err != nil {
			fmt.Println("FAIL", err)
			switch {
			case errors.Is(err, cas.ErrDigestMismatch):
				return exitVerifyMismatch
			default:
				return exitVerifyMissing
			}
		}
		fmt.Println("OK sha256:", d)
		return exitVerifyClean
	}
	res, err := store.VerifyAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return exitVerifyMissing
	}
	for _, d := range res.Mismatched {
		fmt.Println("FAIL sha256:", d, "(digest mismatch)")
	}
	for _, d := range res.Missing {
		fmt.Println("FAIL sha256:", d, "(missing)")
	}
	for _, e := range res.StateErrors {
		fmt.Fprintln(os.Stderr, "WARN state unreadable:", e)
	}
	fmt.Printf("verify --all: %d mismatched, %d missing, %d state errors\n",
		len(res.Mismatched), len(res.Missing), len(res.StateErrors))
	if len(res.Mismatched) > 0 {
		return exitVerifyMismatch
	}
	if len(res.Missing) > 0 {
		return exitVerifyMissing
	}
	fmt.Println("all blobs clean")
	return exitVerifyClean
}
