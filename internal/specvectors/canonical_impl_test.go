package specvectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// jcsCanonical returns the RFC 8785 (JCS) serialization of a parsed JSON
// document. Uses the reference Go implementation listed by RFC 8785.
func jcsCanonical(raw []byte) ([]byte, error) {
	return jcs.Transform(raw)
}

// jcsMustMarshal canonicalizes an in-memory document (Go values → JSON → JCS).
func jcsMustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out, err := jcsCanonical(raw)
	if err != nil {
		panic(err)
	}
	return out
}

func flatSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ----------------------------------------------------------------------------
// Minimal 001 document validation. Deliberately small: only what the spec
// tables state as MUSTs. Unknown fields are preserved (forward compat,
// 001 §3.2) — i.e. not rejected here.
// ----------------------------------------------------------------------------

var (
	reDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reInfohax = regexp.MustCompile(`^btmh:1220[0-9a-f]{64}$`)
)

type valError struct{ class, msg string }

func (e *valError) Error() string { return e.class + ": " + e.msg }

func valErrf(class, format string, args ...any) *valError {
	return &valError{class: class, msg: fmt.Sprintf(format, args...)}
}

// validateArtifact validates a decoded 001 document (manifest, model index or
// distribution record) and returns its artifactType. class is one of the
// E_VALIDATION* classes from the vectors README.
func validateArtifact(doc map[string]any) (artifactType string, err *valError) {
	bad := func(format string, args ...any) *valError {
		return valErrf("E_VALIDATION", format, args...)
	}
	sv, ok := doc["schemaVersion"].(float64)
	if !ok || sv != 1 {
		return "", bad("schemaVersion must be 1")
	}
	at, ok := doc["artifactType"].(string)
	if !ok {
		return "", bad("artifactType missing")
	}
	switch at {
	case "model":
		return at, validateManifest(doc)
	case "model-index":
		return at, validateIndex(doc)
	case "distribution":
		return at, validateDistribution(doc)
	default:
		return "", valErrf("E_VALIDATION_KIND", "unknown artifactType %q", at)
	}
}

func validateManifest(doc map[string]any) *valError {
	files, ok := doc["files"].([]any)
	if !ok || len(files) == 0 {
		return valErrf("E_VALIDATION", "files must be a non-empty array")
	}
	var (
		configs      int
		weightsFmt   string
		tokenizers   int
		chatTemplate int
		runtimes     = map[string]int{}
		parts        = map[string][]int64{}
		names        = map[string]bool{}
		prev         *manifestFile
	)
	for i, fi := range files {
		f, verr := decodeFileEntry(fi)
		if verr != nil {
			return verr
		}
		if names[f.Name] {
			return valErrf("E_VALIDATION", "duplicate file name %q", f.Name)
		}
		names[f.Name] = true
		switch f.Kind {
		case "config":
			configs++
			if f.Name != "modelconfig.json" {
				return valErrf("E_VALIDATION", "config entry must be named modelconfig.json")
			}
		case "tokenizer":
			tokenizers++
		case "chat-template":
			chatTemplate++
		case "runtime-config":
			if f.Runtime == "" {
				return valErrf("E_VALIDATION", "runtime-config entry %q missing runtime id", f.Name)
			}
			runtimes[f.Runtime]++
			if runtimes[f.Runtime] > 1 {
				return valErrf("E_VALIDATION_RUNTIME_DUP", "more than one runtime-config entry for runtime %q (001 §3.1: <= 1 per runtime id)", f.Runtime)
			}
		case "weights.gguf", "weights.safetensors":
			fmt := strings.TrimPrefix(f.Kind, "weights.")
			if weightsFmt == "" {
				weightsFmt = fmt
			} else if weightsFmt != fmt {
				return valErrf("E_VALIDATION_WEIGHTS_MIX", "one weights format per artifact, found %s and %s", weightsFmt, fmt)
			}
			if f.Part == nil {
				return valErrf("E_VALIDATION", "weights entry %q missing part", f.Name)
			}
			parts[fmt] = append(parts[fmt], *f.Part)
		}
		if cmp := fileOrder(prev, &f); prev != nil && cmp > 0 {
			return valErrf("E_VALIDATION_FILE_ORDER", "files[%d] (%s) violates canonical file order", i, f.Name)
		}
		prev = &f
	}
	if configs != 1 {
		return valErrf("E_VALIDATION", "exactly 1 config entry required, found %d", configs)
	}
	if weightsFmt == "" {
		return valErrf("E_VALIDATION_WEIGHTS_MISSING", "artifact requires at least one weights entry (001 §3.1: weights 1..n)")
	}
	if tokenizers > 1 {
		return valErrf("E_VALIDATION_CARDINALITY", "at most 1 tokenizer entry (001 §3.1), found %d", tokenizers)
	}
	if chatTemplate > 1 {
		return valErrf("E_VALIDATION_CARDINALITY", "at most 1 chat-template entry (001 §3.1), found %d", chatTemplate)
	}
	for fmt, ps := range parts {
		sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
		for i, p := range ps {
			if p != int64(i+1) {
				return valErrf("E_VALIDATION_PARTS", "%s parts must be contiguous 1..n, got %v", fmt, ps)
			}
		}
	}
	return nil
}

func decodeFileEntry(fi any) (manifestFile, *valError) {
	m, ok := fi.(map[string]any)
	if !ok {
		return manifestFile{}, valErrf("E_VALIDATION", "file entry must be an object")
	}
	var f manifestFile
	f.Kind, _ = m["kind"].(string)
	f.Digest, _ = m["digest"].(string)
	f.Name, _ = m["name"].(string)
	f.Role, _ = m["role"].(string)
	f.Runtime, _ = m["runtime"].(string)
	if p, ok := m["part"].(float64); ok {
		f.Part = new(int64)
		*f.Part = int64(p)
	}
	bt, _ := m["bt"].(map[string]any)
	f.BT.MerkleRoot, _ = bt["merkleRoot"].(string)

	if f.Kind == "" {
		return f, valErrf("E_VALIDATION", "file entry missing kind")
	}
	if !reDigest.MatchString(f.Digest) {
		return f, valErrf("E_VALIDATION", "file %q: digest must match sha256:<64 lowercase hex>", f.Name)
	}
	if _, ok := m["size"]; !ok {
		return f, valErrf("E_VALIDATION", "file %q: missing size", f.Name)
	}
	if s, ok := m["size"].(float64); !ok {
		return f, valErrf("E_VALIDATION", "file %q: size must be a number", f.Name)
	} else {
		f.Size = int64(s)
	}
	if f.Size <= 0 {
		return f, valErrf("E_VALIDATION", "file %q: size must be > 0", f.Name)
	}
	if f.Name == "manifest" || strings.HasPrefix(f.Name, "manifest/") {
		return f, valErrf("E_VALIDATION_RESERVED_PATH", "file %q: the manifest/ path prefix is reserved for the embedded manifest document (001 §3.1 rule 1, ruling R2)", f.Name)
	}
	if f.Name == "" || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "..") || strings.Contains(f.Name, "\\") || strings.Contains(f.Name, "//") || strings.HasSuffix(f.Name, "/") {
		return f, valErrf("E_VALIDATION", "file %q: invalid artifact-relative name", f.Name)
	}
	for _, seg := range strings.Split(f.Name, "/") {
		if seg == "." || seg == ".." {
			return f, valErrf("E_VALIDATION", "file %q: invalid artifact-relative name (path segments must not be . or ..)", f.Name)
		}
	}
	if !reDigest.MatchString(f.BT.MerkleRoot) {
		return f, valErrf("E_VALIDATION", "file %q: bt.merkleRoot must match sha256:<64 lowercase hex>", f.Name)
	}
	return f, nil
}

// fileOrder implements 001 §3.1 rule 4 (canonical file order). Returns <0 if
// a sorts before b.
func fileOrder(a, b *manifestFile) int {
	if a == nil || b == nil {
		return 0
	}
	ra, rb := kindRank(a.Kind), kindRank(b.Kind)
	if ra != rb {
		return ra - rb
	}
	switch {
	case a.Kind == "weights.gguf" || a.Kind == "weights.safetensors":
		if ap, bp := deref(a.Part), deref(b.Part); ap != bp {
			return int(ap - bp)
		}
		return strings.Compare(a.Digest, b.Digest)
	case a.Kind == "weights.aux":
		return strings.Compare(a.Name, b.Name)
	case a.Kind == "runtime-config": // by runtime id (001 §3.1 rule 4)
		return strings.Compare(a.Runtime, b.Runtime)
	case a.Kind == "code": // role, then name (001 §3.1 rule 4)
		if c := strings.Compare(a.Role, b.Role); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	default:
		return strings.Compare(a.Digest, b.Digest)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func kindRank(kind string) int {
	switch kind {
	case "config":
		return 0
	case "weights.gguf", "weights.safetensors":
		return 1
	case "weights.aux":
		return 2
	case "tokenizer":
		return 3
	case "chat-template":
		return 4
	case "adapter":
		return 5
	case "runtime-config":
		return 6
	case "code":
		return 7
	default:
		return 8 // unknown kinds sort last; rejecting them is 002/runtime business
	}
}

func validateIndex(doc map[string]any) *valError {
	members, ok := doc["members"].([]any)
	if !ok || len(members) == 0 {
		return valErrf("E_VALIDATION", "members must be a non-empty array")
	}
	seen := map[string]bool{}
	for i, mi := range members {
		m, ok := mi.(map[string]any)
		if !ok {
			return valErrf("E_VALIDATION", "member %d must be an object", i)
		}
		d, _ := m["manifest"].(string)
		if !reDigest.MatchString(d) {
			return valErrf("E_VALIDATION", "member %d manifest must match sha256:<64 hex>", i)
		}
		q, _ := m["quant"].(string)
		if !quantSyntaxValid(q) {
			return valErrf("E_VALIDATION", "member %d quant %q is not valid quant syntax (000 Appendix A)", i, q)
		}
		if seen[q] {
			return valErrf("E_VALIDATION_QUANT_DUP", "duplicate member quant %q", q)
		}
		seen[q] = true
	}
	return nil
}

func validateDistribution(doc map[string]any) *valError {
	md, _ := doc["manifestDigest"].(string)
	if !reDigest.MatchString(md) {
		return valErrf("E_VALIDATION", "manifestDigest must match sha256:<64 hex>")
	}
	t, ok := doc["torrent"].(map[string]any)
	if !ok {
		return valErrf("E_VALIDATION", "torrent block missing")
	}
	ih, _ := t["infohash"].(string)
	if !reInfohax.MatchString(ih) {
		return valErrf("E_VALIDATION_INFOHASH", "torrent.infohash must match btmh:1220<64 hex>")
	}
	pl, ok := t["pieceLength"].(float64)
	if !ok || pl < 16384 || !pow2int(int64(pl)) {
		return valErrf("E_VALIDATION", "torrent.pieceLength must be a power of two ≥ 16384")
	}
	ld, _ := t["pieceLayersDigest"].(string)
	if !reDigest.MatchString(ld) {
		return valErrf("E_VALIDATION", "torrent.pieceLayersDigest must match sha256:<64 hex>")
	}
	return nil
}
