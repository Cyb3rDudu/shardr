package runner

import (
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/config"
)

func val(b bool) config.Value { return config.Value{Kind: config.KindBool, Bool: b} }
func ival(n int64) config.Value {
	return config.Value{Kind: config.KindInt, Int: n}
}
func sval(s string) config.Value {
	return config.Value{Kind: config.KindString, Str: s}
}

// All four layers populated: the highest layer wins per key (002 §2.2 —
// scalar replacement per key, no cross-key inference).
func TestOverlayRankHighestWins(t *testing.T) {
	ov := Overlay{
		Advisory: map[string]config.Value{"ctx_size": ival(2048), "jinja": val(true)},
		UserConfig: map[string]config.Value{
			"ctx_size": ival(4096), "n_threads": ival(8), "flash_attn": val(false),
		},
		ConfigFile: map[string]config.Value{"ctx_size": ival(8192), "n_threads": ival(4)},
		Sets:       map[string]config.Value{"ctx_size": ival(1024)},
	}
	merged, err := ov.Merge()
	if err != nil {
		t.Fatal(err)
	}
	if merged["ctx_size"].Int != 1024 {
		t.Fatalf("--set must win ctx_size: %+v", merged["ctx_size"])
	}
	if merged["n_threads"].Int != 4 {
		t.Fatalf("--config must win n_threads: %+v", merged["n_threads"])
	}
	if merged["flash_attn"].Bool {
		t.Fatalf("user config flash_attn=false must hold (higher layers silent): %+v", merged["flash_attn"])
	}
	if !merged["jinja"].Bool {
		t.Fatalf("advisory jinja must survive where no higher layer speaks: %+v", merged["jinja"])
	}
}

// An unknown key in EACH layer is an immediate loud error naming the
// layer (002 §2.2) — four probes, one per layer.
func TestOverlayUnknownKeyPerLayer(t *testing.T) {
	cases := []struct {
		name string
		ov   Overlay
		want string
	}{
		{"advisory", Overlay{Advisory: map[string]config.Value{"n_gpu_layersxx": ival(1)}}, "advisory"},
		{"user-config", Overlay{UserConfig: map[string]config.Value{"gpu_layers": ival(1)}}, "user config"},
		{"config-file", Overlay{ConfigFile: map[string]config.Value{"turbo": val(true)}}, "--config"},
		{"set", Overlay{Sets: map[string]config.Value{"horsepower": ival(9)}}, "--set"},
	}
	for _, tc := range cases {
		_, err := tc.ov.Merge()
		if err == nil {
			t.Fatalf("%s: unknown key must fail loudly", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error must name the layer %q: %v", tc.name, tc.want, err)
		}
		if !strings.Contains(err.Error(), "002 §7.1") {
			t.Fatalf("%s: error must cite the allowlist: %v", tc.name, err)
		}
	}
}

// Machine-NEUTRAL rule (002 §2.1): advisory layers ship machine-neutral
// keys only — n_gpu_layers in an advisory entry is as loud as anywhere.
func TestOverlayAdvisoryMachineNeutralOnly(t *testing.T) {
	ov := Overlay{Advisory: map[string]config.Value{"n_gpu_layers": ival(40)}}
	_, err := ov.Merge()
	if err == nil || !strings.Contains(err.Error(), "advisory") {
		t.Fatalf("advisory n_gpu_layers must be rejected with layer named: %v", err)
	}
}

// A typed mismatch is as loud as an unknown key (allowlist types).
func TestOverlayTypeMismatch(t *testing.T) {
	ov := Overlay{Sets: map[string]config.Value{"ctx_size": sval("huge")}}
	_, err := ov.Merge()
	if err == nil || !strings.Contains(err.Error(), "want int") {
		t.Fatalf("string in an int slot must fail: %v", err)
	}
}

// --set parses typed values (bool/int/string) and loud-rejects garbage.
func TestAddSetTyped(t *testing.T) {
	var ov Overlay
	if err := ov.AddSet("jinja", "true"); err != nil {
		t.Fatal(err)
	}
	if err := ov.AddSet("ctx_size", "4096"); err != nil {
		t.Fatal(err)
	}
	if err := ov.AddSet("kv_cache_type", "q8_0"); err != nil {
		t.Fatal(err)
	}
	if err := ov.AddSet("ctx_size", "big"); err == nil {
		t.Fatal("non-int in an int key must fail")
	}
	merged, err := ov.Merge()
	if err != nil {
		t.Fatal(err)
	}
	if !merged["jinja"].Bool || merged["ctx_size"].Int != 4096 || merged["kv_cache_type"].Str != "q8_0" {
		t.Fatalf("merged: %+v", merged)
	}
}

// Argv maps merged keys to llama-server flags per 002 §7.1, in
// allowlist order (deterministic), booleans presence-only, mmproj_variant
// runner-consumed (never a flag).
func TestArgvMapping(t *testing.T) {
	merged := map[string]config.Value{
		"n_gpu_layers":   ival(40),
		"ctx_size":       ival(8192),
		"flash_attn":     val(true),
		"mlock":          val(false),
		"kv_cache_type":  sval("q8_0"),
		"jinja":          val(true),
		"mmproj_variant": sval("q8_0"),
	}
	got := strings.Join(Argv(merged), " ")
	want := "-ngl 40 -c 8192 -fa --cache-type-k q8_0 --jinja"
	if got != want {
		t.Fatalf("argv:\n got %s\nwant %s", got, want)
	}
}
