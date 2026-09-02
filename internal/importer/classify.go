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
	// config.json deliberately NOT skipped: classify extracts it as the
	// quant-chain + modelconfig input (001 §8.4). It never becomes a
	// manifest entry itself — the built modelconfig.json does.
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
	var tokenizerNames, chatFiles []string
	// B5: one adapter entry per directory-scoped PEFT pair — the old global
	// tar merged members by base name and silently dropped all but one.
	type adapterPair struct {
		dir     string
		files   []string
		sources map[string]Source
	}
	adapterPairs := map[string]*adapterPair{}
	adapterAdd := func(name string, src Source) {
		dir := dirOf(name)
		ap := adapterPairs[dir]
		if ap == nil {
			ap = &adapterPair{dir: dir, sources: map[string]Source{}}
			adapterPairs[dir] = ap
		}
		ap.files = append(ap.files, name)
		ap.sources[name] = src
	}
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
			// Identity rule: ONLY the import-root config.json is the upstream
			// config (001 §8.4) — a nested foo/config.json must never overwrite
			// or shape it (a wrong/stale config would silently produce a
			// different manifest). Nested configs are default-deny skips; a
			// read error on the root config fails the import instead of
			// silently falling through the quant chain to filename/dtype/raw.
			if name != "config.json" {
				c.skipped++
				continue
			}
			b, err := readAll(src)
			if err != nil {
				return nil, fmt.Errorf("read config.json: %w", err)
			}
			c.upstreamConfig = b
			continue
		}
		switch {
		case strings.Contains(lower, "imatrix") && !strings.HasSuffix(lower, ".gguf"):
			// §8.3: imatrix is weights.aux; upstream names it e.g.
			// Qwen3-8B.imatrix — recognized with or without .gguf.
			c.entries = append(c.entries, entry{name: name, kind: "weights.aux", role: "imatrix", src: src})
		case strings.HasPrefix(lower, "mmproj") && !strings.HasSuffix(lower, ".gguf"):
			c.entries = append(c.entries, entry{name: name, kind: "weights.aux", role: "vision-projector", src: src})
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
				adapterAdd(name, src)
				continue // becomes part of the directory-scoped PEFT adapter tar
			}
			stamm, part := splitSafetensors(base)
			c.entries = append(c.entries, entry{
				name: name, kind: "weights.safetensors", part: part,
				group: "st\x00" + stamm, src: src,
			})
		case base == "model.safetensors.index.json":
			c.entries = append(c.entries, entry{name: name, kind: "weights.aux", role: "weights-index", src: src})
		case base == "adapter_config.json":
			adapterAdd(name, src)
		case base == "chat_template.jinja" || base == "chat_template.json" || base == "chat_template":
			chatFiles = append(chatFiles, name)
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
	// chat-template is 0..1 (001 §3.1); dual-template repos are the common
	// HF migration shape — keep the .jinja (richer), skip + warn the rest.
	if len(chatFiles) > 1 {
		sort.Strings(chatFiles)
		kept := ""
		for _, n := range chatFiles {
			if path.Base(n) == "chat_template.jinja" {
				kept = n
			}
		}
		if kept == "" {
			kept = chatFiles[0]
		}
		for _, n := range chatFiles {
			if n != kept {
				c.skipped++
				c.warnings = append(c.warnings, "multiple chat templates; kept "+kept+", skipped "+n)
			}
		}
		chatFiles = []string{kept}
	}
	for _, n := range chatFiles {
		c.entries = append(c.entries, entry{name: n, kind: "chat-template",
			src: sourceByName(sources, n)})
	}
	// B5: one adapter tar per directory-scoped pair; tar members are the
	// pair's two base names (the §3.1 adapter tar is the pair).
	for _, dir := range sortedKeys(adapterPairs) {
		ap := adapterPairs[dir]
		sort.Strings(ap.files)
		hasModel, hasConfig := false, false
		for _, n := range ap.files {
			switch path.Base(n) {
			case "adapter_model.safetensors":
				hasModel = true
			case "adapter_config.json":
				hasConfig = true
			}
		}
		if !hasModel || !hasConfig {
			c.skipped += len(ap.files)
			c.warnings = append(c.warnings, "incomplete PEFT pair in \""+dir+"\": skipped")
			continue
		}
		tarName := "adapter.tar"
		if dir != "" {
			tarName = dir + "/adapter.tar"
		}
		c.entries = append(c.entries, entry{name: tarName, kind: "adapter", adapterFiles: ap.files, adapterSource: ap.sources})
	}
	return c, nil
}

func dirOf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
