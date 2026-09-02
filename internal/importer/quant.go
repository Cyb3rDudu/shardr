package importer

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// Quant derivation chain (001 §8.4): filename token → upstream config.json
// quantization_config → dominant safetensors dtype → "raw". Every derived
// value must satisfy the 000 Appendix A quant syntax; an out-of-vocabulary
// candidate (e.g. quant_method "gptq") is skipped with a warning rather
// than silently entering the protocol vocabulary. (Spec gap reported:
// the prefix list does not cover all quant_method values.)

// QuantFromFilename extracts the longest quant-syntax token from a GGUF
// weights file name (after stripping split suffixes).
func QuantFromFilename(name string) string { return quantToken(name) }

func quantToken(base string) string {
	_, _, q := splitGGUF(base)
	return q
}

// upstreamConfigShape is the subset of a HF config.json the chain reads.
type upstreamConfigShape struct {
	ModelType               string `json:"model_type"`
	MaxPositionalEmbeddings int64  `json:"max_positional_embeddings"`
	QuantizationConfig      *struct {
		QuantMethod string `json:"quant_method"`
		Format      string `json:"format"`
	} `json:"quantization_config"`
}

func parseUpstreamConfig(b []byte) *upstreamConfigShape {
	if len(b) == 0 {
		return nil
	}
	var c upstreamConfigShape
	if err := json.Unmarshal(b, &c); err != nil {
		return nil
	}
	return &c
}

// QuantFromUpstreamConfig reads quantization_config.quant_method/format
// (lowercased) from an upstream config.json.
func QuantFromUpstreamConfig(configJSON []byte) string {
	c := parseUpstreamConfig(configJSON)
	if c == nil || c.QuantizationConfig == nil {
		return ""
	}
	for _, v := range []string{c.QuantizationConfig.Format, c.QuantizationConfig.QuantMethod} {
		if v == "" {
			continue
		}
		v = strings.ToLower(v)
		if ref.QuantSyntax(v) {
			return v
		}
	}
	return ""
}

// safetensorsHeaderShape reads data_offsets to weight bytes by dtype.
type safetensorsHeaderShape map[string]struct {
	Dtype       string  `json:"dtype"`
	DataOffsets []int64 `json:"data_offsets"`
}

// DominantDtype returns the quant-syntax-compatible quant for the dtype
// covering the most bytes in a safetensors header (BF16→bf16, F16→f16,
// F32→f32). Non-mappable dtypes return "".
func DominantDtype(safetensorsHeader []byte) string {
	var h safetensorsHeaderShape
	if err := json.Unmarshal(safetensorsHeader, &h); err != nil {
		return ""
	}
	bytes := map[string]int64{}
	var names []string
	for k, t := range h {
		if k == "__metadata__" || len(t.DataOffsets) != 2 {
			continue
		}
		dt := strings.ToUpper(t.Dtype)
		if _, ok := bytes[dt]; !ok {
			names = append(names, dt)
		}
		bytes[dt] += t.DataOffsets[1] - t.DataOffsets[0]
	}
	sort.Slice(names, func(i, j int) bool {
		if bytes[names[i]] != bytes[names[j]] {
			return bytes[names[i]] > bytes[names[j]]
		}
		return names[i] < names[j]
	})
	for _, dt := range names {
		switch dt {
		case "BF16":
			return "bf16"
		case "F16":
			return "f16"
		case "F32":
			return "f32"
		}
	}
	return ""
}

// DeriveQuant runs the full chain over the group's inputs.
func DeriveQuant(filename string, upstreamConfig, safetensorsHeader []byte, warn func(string)) string {
	if q := quantToken(filename); q != "" {
		return q
	}
	if q := QuantFromUpstreamConfig(upstreamConfig); q != "" {
		return q
	}
	if q := DominantDtype(safetensorsHeader); q != "" {
		return q
	}
	if c := parseUpstreamConfig(upstreamConfig); c != nil && c.QuantizationConfig != nil {
		// A quantization_config existed but its value is out of vocabulary:
		// note it, then fall through to raw (spec gap: prefix list coverage).
		qc := c.QuantizationConfig
		v := qc.Format
		if v == "" {
			v = qc.QuantMethod
		}
		if v != "" && warn != nil {
			warn("quantization_config " + strings.ToLower(v) + " is outside the quant vocabulary (000 App. A); falling back")
		}
	}
	return "raw"
}
