package artifact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// ValidationError is a 001 document rule violation. Class carries the
// E_* vocabulary the spec vectors assert on (docs/specs/vectors/README.md)
// — production code and vectors share this one rule set.
type ValidationError struct {
	Class string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Class + ": " + e.Msg }

// Error classes (vectors vocabulary).
const (
	ClassValidation         = "E_VALIDATION"
	ClassValidationKind     = "E_VALIDATION_KIND"
	ClassValidationReserved = "E_VALIDATION_RESERVED_PATH"
	ClassValidationWeightsM = "E_VALIDATION_WEIGHTS_MIX"
	ClassValidationWeights0 = "E_VALIDATION_WEIGHTS_MISSING"
	ClassValidationCard     = "E_VALIDATION_CARDINALITY"
	ClassValidationParts    = "E_VALIDATION_PARTS"
	ClassValidationRtDup    = "E_VALIDATION_RUNTIME_DUP"
	ClassValidationOrder    = "E_VALIDATION_FILE_ORDER"
	ClassValidationQuantDup = "E_VALIDATION_QUANT_DUP"
	ClassValidationInfohash = "E_VALIDATION_INFOHASH"
)

var reInfohash = regexp.MustCompile(`^btmh:1220[0-9a-f]{64}$`)

// ValidateArtifact validates a decoded 001 document (raw canonical JSON)
// and returns its artifactType. Unknown fields are preserved on decode
// (forward compat, 001 §3.2) — not rejected.
func ValidateArtifact(raw []byte) (artifactType string, err *ValidationError) {
	var head struct {
		SchemaVersion int    `json:"schemaVersion"`
		ArtifactType  string `json:"artifactType"`
	}
	if jerr := json.Unmarshal(raw, &head); jerr != nil {
		return "", &ValidationError{ClassValidation, "not a valid 001 document: " + jerr.Error()}
	}
	if head.SchemaVersion != 1 {
		return "", &ValidationError{ClassValidation, "schemaVersion must be 1"}
	}
	switch head.ArtifactType {
	case "":
		return "", &ValidationError{ClassValidation, "artifactType missing"}
	case "model":
		var m Manifest
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			return "", &ValidationError{ClassValidation, jerr.Error()}
		}
		return head.ArtifactType, ValidateManifest(&m)
	case "model-index":
		var idx Index
		if jerr := json.Unmarshal(raw, &idx); jerr != nil {
			return "", &ValidationError{ClassValidation, jerr.Error()}
		}
		return head.ArtifactType, ValidateIndex(&idx)
	case "distribution":
		var d DistributionRecord
		if jerr := json.Unmarshal(raw, &d); jerr != nil {
			return "", &ValidationError{ClassValidation, jerr.Error()}
		}
		return head.ArtifactType, ValidateDistribution(&d)
	default:
		return "", &ValidationError{ClassValidationKind, fmt.Sprintf("unknown artifactType %q", head.ArtifactType)}
	}
}

// ValidateFileName enforces canonical artifact-relative names (001 §3.1
// rule 1, R2): '/'-separated non-empty segments, no '.', '..', no empty
// segments (covers leading '/', 'a//b', './x'), no backslash — and the
// reserved 'manifest' namespace (004 §3 places the manifest blob at
// manifest/sha256-<hex>): neither 'manifest' itself nor any 'manifest/…'
// prefix is a legal file name.
func ValidateFileName(name string) *ValidationError {
	if name == "manifest" || strings.HasPrefix(name, "manifest/") {
		return &ValidationError{ClassValidationReserved, fmt.Sprintf("file %q: the manifest/ path prefix is reserved for the embedded manifest document (001 §3.1 rule 1, ruling R2)", name)}
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.Contains(name, "\\") || strings.Contains(name, "//") {
		return &ValidationError{ClassValidation, fmt.Sprintf("file %q: invalid artifact-relative name", name)}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return &ValidationError{ClassValidation, fmt.Sprintf("file %q: invalid artifact-relative name (path segments must not be . or ..)", name)}
		}
	}
	return nil
}

// ValidateManifest checks the full canonical invariants of a manifest
// (001 §3.1): unique valid names, exactly one config (canonical name
// modelconfig.json), one weights format, weights present, tokenizer /
// chat-template 0..1, runtime-config ≤ 1 per runtime id, contiguous split
// parts, canonical digest/merkle-root forms, size > 0, and canonical file
// order (§3.1 rule 4). Returns a loud error naming the violated rule.
func ValidateManifest(m *Manifest) *ValidationError {
	if m.SchemaVersion != 1 || m.ArtifactType != "model" {
		return &ValidationError{ClassValidation, "manifest: schemaVersion/artifactType must be 1/model"}
	}
	if len(m.Files) == 0 {
		return &ValidationError{ClassValidation, "files must be a non-empty array"}
	}
	seen := map[string]bool{}
	weightsFormat := ""
	parts := map[string][]int64{}
	nConfig, nTokenizer, nChat := 0, 0, 0
	runtimes := map[string]int{}
	// A name that is a path PREFIX of another name would corrupt the BEP 52
	// file tree (file/dir collision at one path) — reject loudly instead of
	// building a torrent no client accepts (or panicking in insertFileTree).
	byName := map[string]File{}
	for _, f := range m.Files {
		byName[f.Name] = f
	}
	var prev *File
	for i := range m.Files {
		f := &m.Files[i]
		if verr := ValidateFileName(f.Name); verr != nil {
			return verr
		}
		if seen[f.Name] {
			return &ValidationError{ClassValidation, fmt.Sprintf("duplicate file name %q", f.Name)}
		}
		seen[f.Name] = true
		if !ref.IsDigest(f.Digest) {
			return &ValidationError{ClassValidation, fmt.Sprintf("file %q: digest must match sha256:<64 lowercase hex>", f.Name)}
		}
		if f.Size <= 0 {
			return &ValidationError{ClassValidation, fmt.Sprintf("file %q: size must be > 0", f.Name)}
		}
		if !ref.IsDigest(f.BT.MerkleRoot) {
			return &ValidationError{ClassValidation, fmt.Sprintf("file %q: bt.merkleRoot must match sha256:<64 lowercase hex>", f.Name)}
		}
		switch f.Kind {
		case "":
			return &ValidationError{ClassValidation, fmt.Sprintf("file entry missing kind")}
		case "config":
			nConfig++
			if f.Name != "modelconfig.json" {
				return &ValidationError{ClassValidation, "config entry must be named modelconfig.json"}
			}
		case "weights.gguf", "weights.safetensors":
			format := strings.TrimPrefix(f.Kind, "weights.")
			if weightsFormat == "" {
				weightsFormat = format
			} else if weightsFormat != format {
				return &ValidationError{ClassValidationWeightsM, fmt.Sprintf("one weights format per artifact, found %s and %s", weightsFormat, format)}
			}
			if f.Part != nil {
				parts[format] = append(parts[format], *f.Part)
			}
		case "tokenizer":
			nTokenizer++
		case "chat-template":
			nChat++
		case "runtime-config":
			if f.Runtime == "" {
				return &ValidationError{ClassValidation, fmt.Sprintf("runtime-config entry %q missing runtime id", f.Name)}
			}
			runtimes[f.Runtime]++
			if runtimes[f.Runtime] > 1 {
				return &ValidationError{ClassValidationRtDup, fmt.Sprintf("more than one runtime-config entry for runtime %q (001 §3.1: <= 1 per runtime id)", f.Runtime)}
			}
		case "weights.aux", "adapter", "code":
			// cardinality 0..n
		default:
			return &ValidationError{ClassValidation, fmt.Sprintf("file %q: unknown kind %q", f.Name, f.Kind)}
		}
		if prev != nil && fileOrder(prev, f) > 0 {
			return &ValidationError{ClassValidationOrder, fmt.Sprintf("files[%d] (%s) violates canonical file order", i, f.Name)}
		}
		prev = f
	}
	if nConfig != 1 {
		return &ValidationError{ClassValidation, fmt.Sprintf("exactly 1 config entry required, found %d", nConfig)}
	}
	if weightsFormat == "" {
		return &ValidationError{ClassValidationWeights0, "artifact requires at least one weights entry (001 §3.1: weights 1..n)"}
	}
	if nTokenizer > 1 {
		return &ValidationError{ClassValidationCard, fmt.Sprintf("at most 1 tokenizer entry (001 §3.1), found %d", nTokenizer)}
	}
	if nChat > 1 {
		return &ValidationError{ClassValidationCard, fmt.Sprintf("at most 1 chat-template entry (001 §3.1), found %d", nChat)}
	}
	// Path-prefix collision: "a" and "a/b" cannot coexist in the tree.
	for _, f := range m.Files {
		segs := strings.Split(f.Name, "/")
		for i := 1; i < len(segs); i++ {
			if _, ok := byName[strings.Join(segs[:i], "/")]; ok {
				return &ValidationError{ClassValidation, fmt.Sprintf("name %q collides with directory path %q (BEP 52 file tree)", f.Name, strings.Join(segs[:i], "/"))}
			}
		}
	}
	// Split parts must be contiguous 1..n.
	for format, ps := range parts {
		if len(ps) == 0 {
			continue
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
		if ps[0] != 1 || ps[len(ps)-1] != int64(len(ps)) {
			return &ValidationError{ClassValidationParts, fmt.Sprintf("%s parts must be contiguous 1..n, got %v", format, ps)}
		}
	}
	return nil
}

// fileOrder implements 001 §3.1 rule 4 (canonical file order): <0 if a
// sorts before b. Must agree with SortFiles; the digest tie-break for
// cardinality-1 kinds keeps the comparator total.
func fileOrder(a, b *File) int {
	ra, rb := kindRank(a.Kind), kindRank(b.Kind)
	if ra != rb {
		return ra - rb
	}
	switch {
	case a.Kind == "weights.gguf" || a.Kind == "weights.safetensors":
		ap, bp := derefPart(a.Part), derefPart(b.Part)
		if ap != bp {
			return int(ap - bp)
		}
		return strings.Compare(a.Digest, b.Digest)
	case a.Kind == "weights.aux":
		return strings.Compare(a.Name, b.Name)
	case a.Kind == "runtime-config":
		return strings.Compare(a.Runtime, b.Runtime)
	case a.Kind == "code":
		if c := strings.Compare(a.CodeRole, b.CodeRole); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	default: // config, tokenizer, chat-template, adapter
		return strings.Compare(a.Digest, b.Digest)
	}
}

func derefPart(p *int64) int64 {
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
		return 8
	}
}

// ValidateIndex checks a model-index document (001 §5): schemaVersion 1,
// artifactType model-index, and member rules via ValidateIndexMembers.
func ValidateIndex(idx *Index) *ValidationError {
	if idx.SchemaVersion != 1 || idx.ArtifactType != "model-index" {
		return &ValidationError{ClassValidation, "index: schemaVersion/artifactType must be 1/model-index"}
	}
	return ValidateIndexMembers(idx.Members)
}

// ValidateIndexMembers is the central member rule set (001 §5): non-empty,
// unique quants, quant syntax per 000 Appendix A, canonical sha256:<hex>
// manifest digests. The importer's index merge and the API's index reader
// share this — a corrupt index is loud everywhere, never silently carried.
func ValidateIndexMembers(members []IndexMember) *ValidationError {
	if len(members) == 0 {
		return &ValidationError{ClassValidation, "members must be a non-empty array"}
	}
	seen := map[string]bool{}
	for i, m := range members {
		if !ref.IsDigest(m.Manifest) {
			return &ValidationError{ClassValidation, fmt.Sprintf("member %d manifest must match sha256:<64 hex>", i)}
		}
		if !ref.QuantSyntax(m.Quant) {
			return &ValidationError{ClassValidation, fmt.Sprintf("member %d quant %q is not valid quant syntax (000 Appendix A)", i, m.Quant)}
		}
		if seen[m.Quant] {
			return &ValidationError{ClassValidationQuantDup, fmt.Sprintf("duplicate member quant %q", m.Quant)}
		}
		seen[m.Quant] = true
	}
	return nil
}

// ValidateDistribution checks a distribution record (001 §6): canonical
// manifest digest, btmh v2 infohash, power-of-two piece length ≥ 16 KiB,
// canonical piece-layers digest.
func ValidateDistribution(d *DistributionRecord) *ValidationError {
	if d.SchemaVersion != 1 || d.ArtifactType != "distribution" {
		return &ValidationError{ClassValidation, "distribution: schemaVersion/artifactType must be 1/distribution"}
	}
	if !ref.IsDigest(d.ManifestDigest) {
		return &ValidationError{ClassValidation, "manifestDigest must match sha256:<64 hex>"}
	}
	if !reInfohash.MatchString(d.Torrent.Infohash) {
		return &ValidationError{ClassValidationInfohash, "torrent.infohash must match btmh:1220<64 hex>"}
	}
	pl := d.Torrent.PieceLength
	if pl < 16384 || pl&(pl-1) != 0 {
		return &ValidationError{ClassValidation, "torrent.pieceLength must be a power of two ≥ 16384"}
	}
	if !ref.IsDigest(d.Torrent.PieceLayersDigest) {
		return &ValidationError{ClassValidation, "torrent.pieceLayersDigest must match sha256:<64 hex>"}
	}
	return nil
}
