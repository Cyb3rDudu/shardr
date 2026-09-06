package main

import (
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/llamalock"
)

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

// TestParseFetchArgsEqualsForm: --ref=bN must parse as a flag in any
// position (the old dead glob case + "=" exemption let it silently
// become a positional — worst case a destdir literally named
// "--ref=bN" fetching the PINNED release). Any other -x=y token is an
// unknown flag, never a positional.
func TestParseFetchArgsEqualsForm(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		wantRef string
	}{
		{[]string{"--ref=b10819", "darwin_arm64", ".llama-bin"}, "b10819"}, // leading
		{[]string{"darwin_arm64", ".llama-bin", "--ref=b10819"}, "b10819"}, // trailing
		{[]string{"darwin_arm64", "--ref=b10819", ".llama-bin"}, "b10819"}, // middle
		{[]string{"-ref=b10819", "darwin_arm64", ".llama-bin"}, "b10819"},  // single-dash equals
	} {
		p, d, r, err := parseFetchArgs(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if p != "darwin_arm64" || d != ".llama-bin" || r != tc.wantRef {
			t.Fatalf("%v: got (%q,%q,%q)", tc.args, p, d, r)
		}
	}
	for _, bad := range [][]string{
		{"darwin_arm64", ".llama-bin", "-x=y"},          // unknown flag with = must not pass as positional
		{"darwin_arm64", ".llama-bin", "--refs=b10819"}, // lookalike flag
		{"darwin_arm64", "--refs=b10819"},               // lookalike, would eat the destdir slot
		{"--ref="},                                      // empty equals value → no ref set is fine? see below
		{"darwin_arm64", "d", "--ref"},                  // missing value
	} {
		if _, _, _, err := parseFetchArgs(bad); err == nil {
			t.Errorf("%v: must be rejected", bad)
		}
	}
}

// TestLockFromPinCarriesSnapshotDigests: the rendered lock must carry
// EXACTLY the digests of the validated snapshot — if the write path
// ever substitutes other digests (e.g. a re-fetch), this goes red.
func TestLockFromPinCarriesSnapshotDigests(t *testing.T) {
	pin := llamalock.Pinnable{
		Ref:     "b10819",
		Commit:  "c0ffee",
		Digests: map[string]string{"darwin_arm64": strings.Repeat("a", 64), "linux_amd64": strings.Repeat("b", 64)},
	}
	lk := lockFromPin(pin, "2026-09-06T00:00:00Z")
	for _, p := range llamalock.Platforms {
		a := lk.Assets[p]
		if a.SHA256 != pin.Digests[p] {
			t.Errorf("%s: lock digest %s != snapshot %s", p, a.SHA256, pin.Digests[p])
		}
		if a.URL != llamalock.AssetURLFor(pin.Ref, p) {
			t.Errorf("%s: url %s", p, a.URL)
		}
	}
	if lk.Ref != pin.Ref || lk.Commit != pin.Commit || lk.UpdatedAt != "2026-09-06T00:00:00Z" {
		t.Errorf("header fields drifted: %+v", lk)
	}
	// rendered bytes must literally contain each snapshot digest (a
	// swapped digest anywhere in the write path breaks this)
	rendered := string(lk.Format())
	for _, d := range pin.Digests {
		if !strings.Contains(rendered, d) {
			t.Errorf("rendered lock missing snapshot digest %s…", d[:16])
		}
	}
	if strings.Contains(rendered, strings.Repeat("c", 64)) {
		t.Error("rendered lock contains a digest that is NOT in the snapshot")
	}
}
