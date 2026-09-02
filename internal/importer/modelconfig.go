package importer

import (
	"path"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
)

// modelConfigShape is the §3.2 model config document (canonical name
// modelconfig.json). Advisory fields only; unknown upstream fields are not
// carried over — this document is built, not mirrored.
type modelConfigShape struct {
	Family        string                    `json:"family,omitempty"`
	WeightsFormat string                    `json:"weightsFormat"`
	Quantization  string                    `json:"quantization"`
	SelfContained bool                      `json:"selfContained"`
	ContextLength int64                     `json:"contextLength,omitempty"`
	License       string                    `json:"license,omitempty"`
	Source        *configSource             `json:"source,omitempty"`
	Template      *configTemplate           `json:"template"`
	System        any                       `json:"system"`
	Adapters      []any                     `json:"adapters"`
	Runtimes      map[string]map[string]any `json:"runtimes"`
}

type configSource struct {
	HF *hfSource `json:"hf,omitempty"`
}

type hfSource struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
}

type configTemplate struct {
	Override any `json:"override"`
}

// buildModelConfig derives the modelconfig.json content for one artifact
// (001 §3.2, §8.5). family: upstream model_type when present; otherwise
// the weights-file stamm — the spec does not define family derivation for
// local imports without upstream config (gap reported on #4).
func buildModelConfig(opts ImportOptions, weightsFormat, quant, family string,
	upstream []byte, contextLength int64, license string) ([]byte, error) {
	cfg := modelConfigShape{
		Family:        family,
		WeightsFormat: weightsFormat,
		Quantization:  quant,
		SelfContained: weightsFormat == "gguf", // §8.5
		License:       license,
		Template:      &configTemplate{Override: nil},
		System:        nil,
		Adapters:      []any{},
		Runtimes:      map[string]map[string]any{},
	}
	if contextLength > 0 {
		cfg.ContextLength = contextLength
		// §8.5: advisory runtime-config defaults, machine-neutral.
		cfg.Runtimes["llama"] = map[string]any{"ctx_size": contextLength}
	}
	if opts.HFRepo != "" {
		cfg.Source = &configSource{HF: &hfSource{Repo: opts.HFRepo, Revision: opts.HFRevision}}
	}
	return artifact.Canonical(cfg)
}

// deriveFamily: upstream model_type, else the stamm of the first weights
// file name (deterministic from input).
func deriveFamily(upstream []byte, firstWeightsName string) string {
	if c := parseUpstreamConfig(upstream); c != nil && c.ModelType != "" {
		return c.ModelType
	}
	base := path.Base(firstWeightsName)
	stamm, _, _ := splitGGUF(base)
	if stamm == "" {
		stamm, _ = splitSafetensors(base)
	}
	stamm = strings.TrimSuffix(stamm, "-")
	if stamm == "" {
		stamm = strings.TrimSuffix(base, path.Ext(base))
	}
	return stamm
}
