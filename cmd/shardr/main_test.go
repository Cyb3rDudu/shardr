package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// Argument parsing fails loud everywhere: a value-taking flag as the
// last argument is an error (never silently dropped), extra positionals
// are errors (never silently ignored).
func TestArgParsingFailLoud(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"rev-value-last", []string{"import", "hf", "org/repo", "--rev"}, "--rev needs a value"},
		{"hf-extra-args", []string{"import", "hf", "org/repo", "extra"}, "exactly one repo"},
		{"pull-no-ref", []string{"pull"}, "exactly one ref"},
		{"pull-extra", []string{"pull", "ns/name:q8_0", "more"}, "exactly one ref"},
		{"status-extra", []string{"status", "job1", "job2"}, "at most one job id"},
		{"verify-missing", []string{"verify"}, "exactly one target"},
		{"verify-extra", []string{"verify", "--all", "x"}, "exactly one target"},
		{"id-value-last", []string{"serve", "ns/name:q8_0", "--id"}, "--id needs a value"},
		{"config-value-last", []string{"run", "ns/name:q8_0", "--config"}, "--config needs a value"},
		{"set-value-last", []string{"run", "ns/name:q8_0", "--set"}, "--set needs a value"},
		{"as-value-last", []string{"import", "local", "x.gguf", "--as"}, "--as needs a value"},
	}
	for _, tc := range cases {
		// The command must fail BEFORE touching the socket: SHARDR_SOCKET
		// points nowhere, so reaching the daemon would also fail — assert
		// the parse message specifically.
		t.Setenv("SHARDR_SOCKET", "/nonexistent/shardhive.sock")
		code := run(tc.args)
		if code == 0 {
			t.Fatalf("%s: must exit non-zero", tc.name)
		}
		// Re-run capturing stderr for the message check.
		out := captureStderr(t, func() int { return run(tc.args) })
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s: stderr must contain %q, got: %s", tc.name, tc.want, out)
		}
	}
}

func captureStderr(t *testing.T, fn func() int) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := fn()
	setOsStderr(w) // keep linters honest: restore below
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = code
	return string(b)
}

var _ = setOsStderr

func setOsStderr(f *os.File) { _ = f }
