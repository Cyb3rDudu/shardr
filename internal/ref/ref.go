// Package ref implements spec 000 (References & URI Scheme): parsing and
// validating shardr references, canonicalization, and quant/member
// resolution. This is the production implementation; the spec test vectors
// in internal/specvectors run against it, so parser and vectors cannot
// drift apart.
package ref

import (
	"sort"
	"strings"
)

// SchemePrefix is the canonical empty-authority URI prefix (000 §2).
const SchemePrefix = "shardr:///"

// Error classes, stable identifiers shared with the spec test vectors
// (docs/specs/vectors/000-reference.jsonl).
const (
	ErrLength          = "E_LENGTH"
	ErrParse           = "E_PARSE"
	ErrNoSelector      = "E_NO_SELECTOR"
	ErrDigestFormat    = "E_DIGEST_FORMAT"
	ErrTagBanned       = "E_TAG_BANNED"
	ErrUnknownTag      = "E_UNKNOWN_TAG"
	ErrAmbiguous       = "E_AMBIGUOUS_SELECTOR"
	ErrNoMember        = "E_NO_MEMBER"
	ErrPinMismatch     = "E_PIN_MISMATCH"
	maxRefLen          = 512
	digestHexLen       = 64
	DigestSchemePrefix = "sha256:"
)

// Error is a loud reference failure with optional candidate list (005 §2:
// errors carry reasons and candidates, never silent fallbacks).
type Error struct {
	Class      string   `json:"class"`
	Message    string   `json:"message,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

func (e *Error) Error() string { return e.Class + ": " + e.Message }

// Ref is a parsed reference. Digest is the canonical "sha256:<64hex>" pin
// or empty.
type Ref struct {
	Canonical string
	NS        string
	Name      string
	Sel       string // raw selector (quant or tag+quant), empty for @digest form
	Tag       string // verbatim, case-sensitive
	Quant     string
	Digest    string
}

// Parse parses the canonical URI form (000 §2). Processing order per the
// vectors README: length → scheme/authority → digest suffix → ns/name →
// selector → no-selector rule. ns/name are silently lowercased; the tag is
// preserved verbatim.
func Parse(input string) (*Ref, *Error) {
	if len(input) > maxRefLen {
		return nil, &Error{Class: ErrLength, Message: "reference exceeds 512 bytes"}
	}
	if !strings.HasPrefix(input, SchemePrefix) {
		return nil, &Error{Class: ErrParse, Message: "must start with shardr:/// (empty authority)"}
	}
	rest := input[len(SchemePrefix):]

	// Digest suffix. '@' cannot appear in ns/name/sel/digest bodies, so at
	// most one '@' is possible; anything else is a parse error.
	digest := ""
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		digest = rest[i+1:]
		rest = rest[:i]
		if digest == "" || strings.ContainsRune(digest, '@') {
			return nil, &Error{Class: ErrParse, Message: "empty or repeated @digest"}
		}
		if !IsDigest(digest) {
			return nil, &Error{Class: ErrDigestFormat, Message: "digest must be sha256:<64 lowercase hex>"}
		}
	}

	// Selector: first ':' after ns/name (ns/name/sel charsets exclude ':').
	sel := ""
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		sel = rest[i+1:]
		rest = rest[:i]
		if sel == "" {
			return nil, &Error{Class: ErrParse, Message: "empty selector"}
		}
	}

	// ns/name split.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return nil, &Error{Class: ErrParse, Message: "missing ns/name separator"}
	}
	ns, name := rest[:slash], rest[slash+1:]
	if strings.ContainsRune(name, '/') {
		return nil, &Error{Class: ErrParse, Message: "too many path segments"}
	}
	ns, name = strings.ToLower(ns), strings.ToLower(name) // 000 §2 silent lowercase
	if !ValidNS(ns) {
		return nil, &Error{Class: ErrParse, Message: "ns must match [a-z0-9][a-z0-9._-]{0,63} after lowercasing"}
	}
	if !ValidName(name) {
		return nil, &Error{Class: ErrParse, Message: "name must match [a-z0-9][a-z0-9._-]{0,127} after lowercasing"}
	}

	p := &Ref{NS: ns, Name: name, Digest: digest}
	if sel != "" {
		if err := validateSelector(sel, p); err != nil {
			return nil, err
		}
	}
	if sel == "" && digest == "" {
		return nil, &Error{Class: ErrNoSelector, Message: "no default selector in the scheme (000 §2)"}
	}

	var sb strings.Builder
	sb.WriteString(SchemePrefix)
	sb.WriteString(ns)
	sb.WriteByte('/')
	sb.WriteString(name)
	if sel != "" {
		sb.WriteByte(':')
		sb.WriteString(sel)
	}
	if digest != "" {
		sb.WriteByte('@')
		sb.WriteString(digest)
	}
	p.Canonical = sb.String()
	return p, nil
}

// ParseShort parses the CLI short form ns/name[:sel][@digest] by
// canonicalizing to the full URI (000 §2). Input sugar only.
func ParseShort(input string) (*Ref, *Error) {
	return Parse(SchemePrefix + input)
}

// ParseAny accepts either the canonical URI or the CLI short form. Used at
// API boundaries (005 §3 accepts short refs) and by interactive callers.
func ParseAny(input string) (*Ref, *Error) {
	if strings.HasPrefix(input, SchemePrefix) {
		return Parse(input)
	}
	return ParseShort(input)
}

// validateSelector checks sel := quant | tag["+"quant] (000 §2) including
// the tag ban (000 §3.4). Fills p.Sel/Tag/Quant.
func validateSelector(sel string, p *Ref) *Error {
	p.Sel = sel
	if tag, quant, ok := strings.Cut(sel, "+"); ok {
		if !validTagShape(tag) {
			return &Error{Class: ErrParse, Message: "tag must match [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}"}
		}
		if err := TagBan(tag); err != nil {
			return err
		}
		if !QuantSyntax(quant) {
			return &Error{Class: ErrParse, Message: "quant part must match 000 Appendix A"}
		}
		p.Tag, p.Quant = tag, quant
		return nil
	}
	if !QuantSyntax(sel) {
		// Not quant syntax; the only other legal sel form is tag+quant, so a
		// bare tag-shaped string is a parse error (bare tags never select).
		return &Error{Class: ErrParse, Message: "selector must be a quant or tag+quant; bare tags are not selectors"}
	}
	p.Quant = sel
	return nil
}

// TagBan enforces 000 §3.4: tags whose lowercased form starts with
// sha256-/sha256: or matches the quant syntax are invalid.
func TagBan(tag string) *Error {
	low := strings.ToLower(tag)
	if strings.HasPrefix(low, "sha256-") || strings.HasPrefix(low, "sha256:") {
		return &Error{Class: ErrTagBanned, Message: "tags may not start with sha256-/sha256:"}
	}
	if QuantSyntax(low) {
		return &Error{Class: ErrTagBanned, Message: "quant-shaped strings select index members, they cannot be tags"}
	}
	return nil
}

// QuantSyntax implements 000 Appendix A: charset [a-z0-9_-], ≤ 24 chars,
// starts with a known prefix AND contains ≥ 1 digit, OR is the reserved
// term "raw" (reserved-term clause overrides the prefix clause — defect
// note in the vectors README, issue #3).
func QuantSyntax(s string) bool {
	if s == "raw" {
		return true
	}
	if len(s) == 0 || len(s) > 24 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	prefixes := []string{"q", "iq", "tq", "ud-", "bf", "f", "fp", "mx"}
	prefixOK := false
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			prefixOK = true
			break
		}
	}
	if !prefixOK {
		return false
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// IsDigest reports whether s is a canonical digest: "sha256:" + 64
// lowercase hex chars.
func IsDigest(s string) bool {
	if !strings.HasPrefix(s, DigestSchemePrefix) || len(s) != len(DigestSchemePrefix)+digestHexLen {
		return false
	}
	for _, r := range s[len(DigestSchemePrefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// NormalizeDigest canonicalizes "sha256:<hex>" or bare 64-hexlower input to
// the "sha256:<hex>" protocol form (000 §2). Everything else is rejected —
// no silent fallback, no uppercase.
func NormalizeDigest(s string) (string, *Error) {
	if strings.HasPrefix(s, DigestSchemePrefix) {
		if !IsDigest(s) {
			return "", &Error{Class: ErrDigestFormat, Message: "sha256: digest must be 64 lowercase hex chars"}
		}
		return s, nil
	}
	cand := DigestSchemePrefix + s
	if !IsDigest(cand) {
		return "", &Error{Class: ErrDigestFormat, Message: "digest must be sha256:<64 lowercase hex> or bare 64-hex"}
	}
	return cand, nil
}

func ValidNS(s string) bool   { return isAtom(s, 64) }
func ValidName(s string) bool { return isAtom(s, 128) }

// ValidNamespaceKey reports whether s is a well-formed "ns/name" — the
// State-store key shape for namespaces (003 §2, 000 §2).
func ValidNamespaceKey(s string) bool {
	ns, name, ok := strings.Cut(s, "/")
	if !ok || strings.ContainsRune(name, '/') {
		return false
	}
	return ValidNS(ns) && ValidName(name)
}

// ValidTag reports whether s is a valid tag alias key: the shape
// [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127} plus the tag ban (000 §3.4).
func ValidTag(s string) *Error {
	if !validTagShape(s) {
		return &Error{Class: ErrParse, Message: "tag must match [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}"}
	}
	return TagBan(s)
}

// validTagShape: [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127} — case-sensitive,
// preserved verbatim (000 §2).
func validTagShape(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	first := s[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9' || first == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func isAtom(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	first := s[0]
	if !(first >= 'a' && first <= 'z' || first >= '0' && first <= '9') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Member is one entry of a model index (001): a servable unit's quant and
// its manifest digest.
type Member struct {
	Quant    string `json:"quant"`
	Manifest string `json:"manifest"`
}

// Resolution is the outcome of selecting a member from an index.
type Resolution struct {
	Quant    string `json:"quant"`
	Manifest string `json:"manifest"`
}

// Resolve selects the reference's quant from members per 000 §3: exact
// match first, then unique-prefix expansion, then the @digest pin equality
// check. A Ref without a quant (@digest manifest-addressing form) resolves
// to nil — there is nothing to select.
func Resolve(p *Ref, members []Member) (*Resolution, *Error) {
	if p.Quant == "" {
		return nil, nil
	}
	for _, m := range members {
		if m.Quant == p.Quant {
			return checkPin(p, m)
		}
	}
	var candidates []string
	for _, m := range members {
		if strings.HasPrefix(m.Quant, p.Quant) {
			candidates = append(candidates, m.Quant)
		}
	}
	switch len(candidates) {
	case 0:
		return nil, &Error{Class: ErrNoMember, Message: "no index member matches " + p.Quant}
	case 1:
		for _, m := range members {
			if m.Quant == candidates[0] {
				return checkPin(p, m)
			}
		}
		panic("unreachable")
	default:
		sort.Strings(candidates)
		return nil, &Error{Class: ErrAmbiguous, Candidates: candidates,
			Message: "ambiguous prefix " + p.Quant}
	}
}

func checkPin(p *Ref, m Member) (*Resolution, *Error) {
	if p.Digest != "" && p.Digest != m.Manifest {
		return nil, &Error{Class: ErrPinMismatch,
			Message: "pin " + p.Digest + " does not match resolved member " + m.Quant + " (" + m.Manifest + ")"}
	}
	return &Resolution{Quant: m.Quant, Manifest: m.Manifest}, nil
}
