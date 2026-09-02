// Package importer implements shardhive's import machinery per spec 001 §8:
// default-deny classification, the eligibility gate (fail closed), the
// quant derivation chain, deterministic artifact construction, and the
// local and Hugging Face sources. One rule set for all sources — same
// upstream bytes + same classification specVersion → byte-identical
// artifacts (import convergence, 001 §7.5).
package importer

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// ClassificationSpecVersion is the classification-ruleset version pinned
// into every import's annotations (001 §4, §7.5). Bumping it is a protocol
// decision: it deliberately breaks convergence with older imports.
const ClassificationSpecVersion = "0"

// Source is one upstream file: its artifact-relative name and a lazy
// content opener (local file, HF range stream, …).
type Source struct {
	Name string
	Open func() (io.ReadCloser, error)
}

// entry is a classified file on its way into an artifact.
type entry struct {
	name     string // manifest-relative canonical name
	kind     string // 001 §3.1 kind
	part     int64  // split part (1..n), 0 = unsplit
	role     string // weights.aux role
	codeRole string
	group    string // artifact group key (quant families)
	quant    string // derived quant candidate ("" = chain continues)
	src      Source
	// tar members (tokenizer / adapter entries)
	tokenizer     []string
	adapterFiles  []string
	adapterSource map[string]Source
}

// classification carries the full outcome: recognized entries, skipped
// count, and warnings for the manifest annotations.
type classification struct {
	entries  []entry
	skipped  int
	warnings []string
	// upstream config.json content (input to the quant chain and the model
	// config builder), if present.
	upstreamConfig []byte
	// license annotation candidates from skipped license files.
	licenseName string
	licenseSPDX string
}

var (
	splitSuffix = regexp.MustCompile(`[-_](\d{5})(-of-(\d{5}))?$`)
	// training artifacts (001 §8.2)
	checkpointDir = regexp.MustCompile(`(^|/)checkpoint-\d+(/|$)`)
	rngState      = regexp.MustCompile(`(^|/)rng_state[^/]*\.pth$`)
)

// skipRules: files never becoming manifest entries (001 §8.2). License
// files are redirected to annotations by the caller via skipLicense.
func skipRule(name string) (skip bool, license bool) {
	base := path.Base(name)
	lower := strings.ToLower(base)
	switch {
	case checkpointDir.MatchString(name),
		hasDirSeg(name, "training_state", "wandb", "runs", "logs", "assets", ".eval_results", "__pycache__", "benchmarks"),
		lower == ".gitattributes",
		lower == "generation_config.json",
		lower == "trainer_state.json",
		lower == "training_args.bin",
		lower == "optimizer.pt",
		lower == "scheduler.pt",
		rngState.MatchString(name),
		strings.HasSuffix(lower, ".pyc"),
		strings.HasPrefix(base, "README"):
		return true, false
	case lower == "config.json":
		return true, false // upstream HF config: quant-chain input, not a manifest entry
	case strings.HasPrefix(lower, "license") || strings.HasPrefix(lower, "licence") || lower == "copying":
		return true, true
	default:
		return false, false
	}
}

func hasDirSeg(name string, segs ...string) bool {
	for _, s := range strings.Split(name, "/") {
		for _, want := range segs {
			if s == want {
				return true
			}
		}
	}
	return false
}

// tokenizerSetFiles are folded into one deterministic tar (001 §8.3).
var tokenizerSetFiles = map[string]bool{
	"tokenizer.json": true, "tokenizer_config.json": true, "special_tokens_map.json": true,
	"added_tokens.json": true, "preprocessor_config.json": true, "processor_config.json": true,
}

func tokenizerSetMember(name string) bool {
	base := path.Base(name)
	if tokenizerSetFiles[base] {
		return true
	}
	for _, p := range []string{"vocab.", "merges."} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	if strings.HasSuffix(base, ".model") && base != "adapter_model.safetensors" {
		return true // sentencepiece
	}
	return false
}

var codeFilePatterns = []string{"modeling_", "tokenization_", "configuration_"}

// classify maps upstream file names to artifact entries per 001 §8.3.
// Default deny: anything unrecognized is skipped and counted. The
// classification is a pure function of the (sorted) name set.
func classify(sources []Source) (*classification, error) {
	c := &classification{}
	var tokenizerNames, adapterNames []string
	adapterTarSources := map[string]Source{}
	for _, src := range sources {
		name := src.Name
		base := path.Base(name)
		lower := strings.ToLower(base)
		if skip, isLicense := skipRule(name); skip {
			c.skipped++
			if isLicense && c.licenseSPDX == "" {
				// License text feeds the annotations; the file itself never
				// becomes a manifest entry (001 §8.2).
				if b, err := readAll(src); err == nil {
					c.licenseName, c.licenseSPDX = extractLicense(path.Base(name), b)
				}
			}
			continue
		}
		if path.Base(name) == "config.json" {
			if b, err := readAll(src); err == nil {
				c.upstreamConfig = b
			}
			continue
		}
		switch {
		case strings.HasSuffix(lower, ".gguf"):
			kind, role := "weights.gguf", ""
			if strings.HasPrefix(lower, "mmproj") {
				kind, role = "weights.aux", "vision-projector"
			} else if strings.HasPrefix(lower, "imatrix") {
				kind, role = "weights.aux", "imatrix"
			}
			if kind == "weights.gguf" {
				stamm, part, quant := splitGGUF(base)
				c.entries = append(c.entries, entry{
					name: name, kind: kind, part: part, quant: quant,
					group: quant + "\x00" + stamm, src: src,
				})
			} else {
				c.entries = append(c.entries, entry{name: name, kind: kind, role: role, src: src})
			}
		case strings.HasSuffix(lower, ".safetensors"):
			if base == "adapter_model.safetensors" {
				adapterNames = append(adapterNames, name)
				adapterTarSources[name] = src
				continue // becomes part of the PEFT adapter tar
			}
			stamm, part := splitSafetensors(base)
			c.entries = append(c.entries, entry{
				name: name, kind: "weights.safetensors", part: part,
				group: "st\x00" + stamm, src: src,
			})
		case base == "model.safetensors.index.json":
			c.entries = append(c.entries, entry{name: name, kind: "weights.aux", role: "weights-index", src: src})
		case base == "adapter_config.json":
			adapterTarSources[name] = src
			adapterNames = append(adapterNames, name)
		case base == "chat_template.jinja" || base == "chat_template.json" || base == "chat_template":
			c.entries = append(c.entries, entry{name: name, kind: "chat-template", src: src})
		case tokenizerSetMember(name):
			tokenizerNames = append(tokenizerNames, name)
		case strings.HasSuffix(lower, ".py"):
			role := ""
			for _, p := range codeFilePatterns {
				if strings.HasPrefix(base, p) {
					role = strings.TrimSuffix(p, "_")
					break
				}
			}
			if strings.Contains(base, "_processor") {
				role = "processor"
			}
			if role == "" {
				c.skipped++
				continue
			}
			c.entries = append(c.entries, entry{name: name, kind: "code", codeRole: role, src: src})
		case base == "non_lora_trainables.bin":
			c.skipped++
			c.warnings = append(c.warnings, "skipped non_lora_trainables.bin (001 §8.8)")
		default:
			c.skipped++ // default deny
		}
	}
	if len(tokenizerNames) > 0 {
		sort.Strings(tokenizerNames)
		c.entries = append(c.entries, entry{name: "tokenizer.tar", kind: "tokenizer", tokenizer: tokenizerNames})
	}
	if len(adapterNames) > 0 {
		pair := false
		for _, n := range adapterNames {
			if path.Base(n) == "adapter_model.safetensors" {
				pair = true
			}
		}
		if !pair {
			c.warnings = append(c.warnings, "adapter_config.json without adapter_model.safetensors: skipped")
			c.skipped += len(adapterNames)
		} else {
			sort.Strings(adapterNames)
			c.entries = append(c.entries, entry{name: "adapter.tar", kind: "adapter", adapterFiles: adapterNames, adapterSource: adapterTarSources})
		}
	}
	return c, nil
}

// splitGGUF strips the split suffix, extracts the quant token, and returns
// (stamm, part, quant). part is 0 for unsplit files.
func splitGGUF(base string) (string, int64, string) {
	n := strings.TrimSuffix(base, ".gguf")
	part := int64(0)
	if m := splitSuffix.FindStringSubmatch(n); m != nil {
		fmt.Sscanf(m[1], "%d", &part)
		n = n[:len(n)-len(m[0])]
	}
	// Longest suffix token run matching quant syntax (ud-q4_k_m etc.).
	tokens := strings.Split(n, "-")
	for take := len(tokens); take > 0; take-- {
		cand := strings.Join(tokens[len(tokens)-take:], "-")
		if ref.QuantSyntax(cand) {
			return strings.Join(tokens[:len(tokens)-take], "-"), part, cand
		}
	}
	return n, part, ""
}

// splitSafetensors strips standard shard numbering → (stamm, part).
func splitSafetensors(base string) (string, int64) {
	n := strings.TrimSuffix(base, ".safetensors")
	part := int64(0)
	if m := splitSuffix.FindStringSubmatch(n); m != nil {
		fmt.Sscanf(m[1], "%d", &part)
		n = n[:len(n)-len(m[0])]
	}
	return n, part
}

func readAll(src Source) ([]byte, error) {
	r, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

var spdxRe = regexp.MustCompile(`SPDX-License-Identifier:\s*(\S+)`)

// extractLicense pulls an SPDX identifier when present; the file name is
// kept as the raw upstream name otherwise (001 §4).
func extractLicense(name string, b []byte) (nameOut, spdx string) {
	if len(b) > 64*1024 {
		return name, "" // huge blobs are not license text
	}
	if m := spdxRe.FindSubmatch(b); m != nil {
		return name, string(m[1])
	}
	return name, ""
}
