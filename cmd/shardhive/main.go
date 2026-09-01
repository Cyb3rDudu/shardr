package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// version is injected at build time via ldflags, e.g.
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.0.1-dev"

const usage = `usage: shardhive <command> [args]

commands:
  cas verify <digest|--all>   re-hash blobs and check digests
                               (exit 0 clean, 1 mismatch, 2 missing)
  version                     print version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches CLI args and returns the process exit code: 0 success,
// 2 usage/dispatch error. Verification exit semantics live in runVerify.
func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("shardhive", version)
		return 0
	case "cas":
		if len(args) == 2 && args[1] == "verify" {
			fmt.Fprintln(os.Stderr, "usage: shardhive cas verify <digest|--all>")
			return 2
		}
		if len(args) == 3 && args[1] == "verify" {
			return runVerify(args[2])
		}
		fmt.Fprint(os.Stderr, usage)
		return 2
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// runVerify implements `cas verify` exit semantics per spec 003 §4:
// 0 clean, 1 mismatch, 2 missing. Mismatch outranks missing when both occur.
func runVerify(arg string) int {
	store, err := cas.Open("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 2
	}
	if arg != "--all" {
		if err := store.Verify(strings.ToLower(arg)); err != nil {
			fmt.Println("FAIL", err)
			switch {
			case errors.Is(err, cas.ErrDigestMismatch):
				return 1
			default:
				return 2
			}
		}
		fmt.Println("OK sha256:", strings.ToLower(arg))
		return 0
	}
	mismatched, missing, err := store.VerifyAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shardhive:", err)
		return 2
	}
	for _, d := range mismatched {
		fmt.Println("FAIL sha256:", d, "(digest mismatch)")
	}
	for _, d := range missing {
		fmt.Println("FAIL sha256:", d, "(missing)")
	}
	fmt.Printf("verify --all: %d mismatched, %d missing\n", len(mismatched), len(missing))
	if len(mismatched) > 0 {
		return 1
	}
	if len(missing) > 0 {
		return 2
	}
	fmt.Println("all blobs clean")
	return 0
}
