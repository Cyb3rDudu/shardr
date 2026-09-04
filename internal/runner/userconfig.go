package runner

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/config"
)

// parseJSONScalars decodes a flat JSON object of bool/int/string scalars
// (runtime-config advisory entries, 001 §3.1). Anything else is loud.
func ParseScalars(b []byte) (map[string]config.Value, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("not a flat JSON object: %w", err)
	}
	out := make(map[string]config.Value, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case bool:
			out[k] = config.Value{Kind: config.KindBool, Bool: t}
		case float64:
			out[k] = config.Value{Kind: config.KindInt, Int: int64(t)}
		case string:
			out[k] = config.Value{Kind: config.KindString, Str: t}
		default:
			return nil, fmt.Errorf("key %q: want bool/int/string scalar", k)
		}
	}
	return out, nil
}

// UserConfigOverlay extracts layer 2 for one runtime from a parsed
// config file: [runtimes.llama] global, then [models."<short-ref>".llama]
// per-model — the per-model table wins per key (004 §7 shows both;
// both are layer 2, the model table is the more specific site).
// Unknown keys surface later in Merge (single validation site, 002 §2.2).
func UserConfigOverlay(f config.File, runtimeID, shortRef string) map[string]config.Value {
	out := map[string]config.Value{}
	if sec, ok := f["runtimes."+runtimeID]; ok {
		for k, v := range sec {
			out[k] = v
		}
	}
	if sec, ok := f[`models."`+shortRef+`".`+runtimeID]; ok {
		for k, v := range sec {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ConfigFileOverlay extracts layer 3 from a --config file: same schema
// as layer 2 ([runtimes.llama] ∪ [models."<short-ref>".llama]).
func ConfigFileOverlay(f config.File, runtimeID, shortRef string) map[string]config.Value {
	return UserConfigOverlay(f, runtimeID, shortRef)
}

// DefaultSelector reads [references].default_selector (004 §7) —
// interactive-CLI comfort ONLY (000 §2): applied when a human types a
// selector-less ref, never in Modelfiles, the API, manifests, or
// shardrbay entries.
func DefaultSelector(f config.File) string {
	if sec, ok := f["references"]; ok {
		if v, ok := sec["default_selector"]; ok && strings.TrimSpace(v.Str) != "" {
			return v.Str
		}
	}
	return ""
}
