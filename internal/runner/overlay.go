// Package runner implements the shardr model runtime (002): overlay
// configuration merge, the llama-server subprocess lifecycle, and the
// serve instance registry. It is the engine behind `shardr run/serve/stop`.
package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/config"
)

// Layer identifies where a config value came from — errors name the
// layer so a typo is findable (002 §2: loud, with provenance).
type Layer int

const (
	LayerAdvisory   Layer = iota // manifest runtime-config entries (001 §3.1)
	LayerUserConfig              // ~/.config/shardr/config.toml (004 §7)
	LayerConfigFile              // --config <file.toml>
	LayerSet                     // --set key=value
)

func (l Layer) String() string {
	switch l {
	case LayerAdvisory:
		return "advisory defaults (manifest runtime-config)"
	case LayerUserConfig:
		return "user config (config.toml)"
	case LayerConfigFile:
		return "--config file"
	case LayerSet:
		return "--set"
	}
	return "unknown layer"
}

// Spec is one runtime configuration key's allowlist entry (002 §7.1).
type Spec struct {
	Key  string
	Flag string // llama-server flag; "" = consumed by the runner, not passed
	Kind config.Kind
}

// Allowlist is the llama runtime key→flag mapping (002 §7.1). mmproj_variant
// selects the vision-projector variant — it is a runner concern, never a
// llama-server flag.
var Allowlist = []Spec{
	{"n_gpu_layers", "-ngl", config.KindInt},
	{"ctx_size", "-c", config.KindInt},
	{"n_threads", "-t", config.KindInt},
	{"flash_attn", "-fa", config.KindBool},
	{"mlock", "--mlock", config.KindBool},
	{"kv_cache_type", "--cache-type-k", config.KindString},
	{"batch_size", "-b", config.KindInt},
	{"ubatch_size", "-ub", config.KindInt},
	{"n_parallel", "-np", config.KindInt},
	{"jinja", "--jinja", config.KindBool},
	{"mmproj_variant", "", config.KindString},
}

func specFor(key string) (Spec, bool) {
	for _, s := range Allowlist {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// kv is a typed overlay scalar with provenance.
type kv struct {
	Value config.Value
	From  Layer
}

// Overlay carries the four layers (002 §2.1) toward Merge. Layers are
// scalar maps; Merge replaces individual keys — higher wins per key.
type Overlay struct {
	// Advisory: parsed runtime-config entries of the artifact (layer 1).
	Advisory map[string]config.Value
	// UserConfig: [runtimes.llama] ∪ [models."<ref>".llama] (layer 2).
	UserConfig map[string]config.Value
	// ConfigFile: --config file, same schema as layer 2 (layer 3).
	ConfigFile map[string]config.Value
	// Sets: --set key=value, repeatable (layer 4).
	Sets map[string]config.Value

	// provenance for error messages
	setOrder []string
}

// AddSet appends one --set key=value pair (layer 4).
func (o *Overlay) AddSet(key, raw string) error {
	v, err := parseSetValue(key, raw)
	if err != nil {
		return err
	}
	if o.Sets == nil {
		o.Sets = map[string]config.Value{}
	}
	o.Sets[key] = v
	o.setOrder = append(o.setOrder, key)
	return nil
}

func parseSetValue(key, raw string) (config.Value, error) {
	// Explicit kind via allowlist; fallback: true/false → bool, digits →
	// int, else string.
	if s, ok := specFor(key); ok {
		switch s.Kind {
		case config.KindBool:
			switch raw {
			case "true", "false":
				return config.Value{Kind: config.KindBool, Bool: raw == "true"}, nil
			}
			return config.Value{}, fmt.Errorf("--set %s: want true/false, got %q", key, raw)
		case config.KindInt:
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return config.Value{}, fmt.Errorf("--set %s: want integer, got %q", key, raw)
			}
			return config.Value{Kind: config.KindInt, Int: n}, nil
		}
	}
	switch raw {
	case "true", "false":
		return config.Value{Kind: config.KindBool, Bool: raw == "true"}, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return config.Value{Kind: config.KindInt, Int: n}, nil
	}
	return config.Value{Kind: config.KindString, Str: raw}, nil
}

// Merge validates EVERY key in EVERY layer against the allowlist and
// merges scalar-per-key, highest layer wins (002 §2.2). An unknown key
// anywhere is a loud error naming layer and key — fail fast, no silent
// drops.
func (o Overlay) Merge() (map[string]config.Value, error) {
	layers := []struct {
		l Layer
		m map[string]config.Value
	}{
		{LayerAdvisory, o.Advisory},
		{LayerUserConfig, o.UserConfig},
		{LayerConfigFile, o.ConfigFile},
		{LayerSet, o.Sets},
	}
	// Validate first — an unknown key in ANY layer fails the whole merge,
	// even if a higher layer would shadow it.
	for _, l := range layers {
		if l.l == LayerAdvisory {
			// Layer 1 is publisher-provided and must stay machine-NEUTRAL
			// (002 §2.1): ctx_size/jinja travel in artifacts, machine
			// specifics (GPU layers, threads, locks) never do.
			for k := range l.m {
				if _, known := specFor(k); !known {
					continue // unknown keys hit the §7.1 check below
				}
				if k != "ctx_size" && k != "jinja" {
					return nil, fmt.Errorf("runtime key %q in %s: advisory layers are machine-neutral only (002 §2.1 — ctx_size, jinja); machine specifics belong to user overlays",
						k, l.l)
				}
			}
		}
		keys := make([]string, 0, len(l.m))
		for k := range l.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := specFor(k); !ok {
				return nil, fmt.Errorf("unknown runtime key %q in %s (002 §7.1 allowlist: %s)",
					k, l.l, allowlistKeys())
			}
			// Typed check: a bool slot fed a string is as loud as an unknown key.
			if s, _ := specFor(k); l.m[k].Kind != s.Kind {
				return nil, fmt.Errorf("runtime key %q in %s: want %s value, got %s",
					k, l.l, kindName(s.Kind), kindName(l.m[k].Kind))
			}
		}
	}
	merged := map[string]config.Value{}
	for _, l := range layers {
		for k, v := range l.m {
			merged[k] = v
		}
	}
	return merged, nil
}

func allowlistKeys() string {
	var sb strings.Builder
	for i, s := range Allowlist {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(s.Key)
	}
	return sb.String()
}

func kindName(k config.Kind) string {
	switch k {
	case config.KindBool:
		return "bool"
	case config.KindInt:
		return "int"
	}
	return "string"
}

// Argv builds the llama-server flag arguments from the merged config
// (002 §5.3: argv derives ONLY from the merged config via the §7.1
// mapping — weights and mmproj are appended by the spawner, not here).
// Boolean keys pass their flag when true and are omitted when false
// (llama-server default-off); mmproj_variant is runner-consumed.
func Argv(merged map[string]config.Value) []string {
	// Deterministic order: allowlist order, not map iteration.
	var argv []string
	for _, s := range Allowlist {
		v, ok := merged[s.Key]
		if !ok || s.Flag == "" {
			continue
		}
		switch s.Kind {
		case config.KindBool:
			if v.Bool {
				argv = append(argv, s.Flag)
			}
		case config.KindInt:
			argv = append(argv, s.Flag, strconv.FormatInt(v.Int, 10))
		default:
			argv = append(argv, s.Flag, v.Str)
		}
	}
	return argv
}

// AdvisoryFromManifest extracts the layer-1 advisory overlay for the
// llama runtime from the artifact's runtime-config entries (001 §3.1:
// machine-neutral only — n_gpu_layers in an advisory entry is rejected
// as loudly as anywhere else, via the same Merge validation).
// The entry's blob content must be a flat JSON object of scalars.
func AdvisoryFromManifest(m *artifact.Manifest, readBlob func(digestHex string) ([]byte, error), runtimeID string) (map[string]config.Value, error) {
	out := map[string]config.Value{}
	for _, f := range m.Files {
		if f.Kind != "runtime-config" || f.Runtime != runtimeID {
			continue
		}
		hex := strings.TrimPrefix(f.Digest, "sha256:")
		b, err := readBlob(hex)
		if err != nil {
			return nil, fmt.Errorf("advisory runtime-config blob: %w", err)
		}
		obj, err := ParseScalars(b)
		if err != nil {
			return nil, fmt.Errorf("advisory runtime-config %s: %w", f.Name, err)
		}
		for k, v := range obj {
			out[k] = v
		}
	}
	return out, nil
}
