package main

import "testing"

// TestParseFetchArgs: the canary dispatch uses the documented TRAILING
// --ref syntax; the stdlib flag package would silently drop it (usage
// exit 2 before any download). Both orders must parse identically.
func TestParseFetchArgs(t *testing.T) {
	for _, args := range [][]string{
		{"darwin_arm64", ".llama-bin", "--ref", "b10819"},
		{"--ref", "b10819", "darwin_arm64", ".llama-bin"},
		{"darwin_arm64", ".llama-bin"},
	} {
		p, d, r, err := parseFetchArgs(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if p != "darwin_arm64" || d != ".llama-bin" {
			t.Fatalf("%v: got %q %q", args, p, d)
		}
		wantRef := ""
		if len(args) == 4 {
			wantRef = "b10819"
		}
		if r != wantRef {
			t.Fatalf("%v: ref=%q want %q", args, r, wantRef)
		}
	}
	for _, bad := range [][]string{
		{"darwin_arm64"},                 // too few positionals
		{"darwin_arm64", "d", "extra"},   // too many
		{"darwin_arm64", "d", "--ref"},   // missing value
		{"--bogus", "darwin_arm64", "d"}, // unknown flag
	} {
		if _, _, _, err := parseFetchArgs(bad); err == nil {
			t.Errorf("%v: must be rejected", bad)
		}
	}
}
