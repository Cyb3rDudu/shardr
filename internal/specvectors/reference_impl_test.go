package specvectors

import (
	"fmt"
	"sort"
	"strings"
)

// ----------------------------------------------------------------------------
// Reference parser + resolver for spec 000 (References & URI Scheme).
// Test-only reference implementation. Processing order (documented in the
// vectors README): length check → scheme/authority → digest suffix →
// ns/name → selector → no-selector rule.
// ----------------------------------------------------------------------------

const refSchemePrefix = "shardr:///"

const (
	errLength       = "E_LENGTH"
	errParse        = "E_PARSE"
	errNoSelector   = "E_NO_SELECTOR"
	errDigestFormat = "E_DIGEST_FORMAT"
	errTagBanned    = "E_TAG_BANNED"
	errUnknownTag   = "E_UNKNOWN_TAG"
	errAmbiguous    = "E_AMBIGUOUS_SELECTOR"
	errNoMember     = "E_NO_MEMBER"
	errPinMismatch  = "E_PIN_MISMATCH"
)

type refError struct {
	Class      string   `json:"class"`
	Candidates []string `json:"candidates,omitempty"`
	Message    string   `json:"message,omitempty"`
}

func (e *refError) Error() string { return e.Class + ": " + e.Message }

type refParse struct {
	Canonical string `json:"canonical"`
	NS        string `json:"ns"`
	Name      string `json:"name"`
	Sel       string `json:"sel,omitempty"`
	Tag       string `json:"tag,omitempty"`
	Quant     string `json:"quant,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// parseRef parses a canonical-form reference (000 §2). Silent lowercase
// applies to ns/name; the tag is preserved verbatim.
func parseRef(input string) (*refParse, *refError) {
	if len(input) > 512 {
		return nil, &refError{Class: errLength, Message: "reference exceeds 512 bytes"}
	}
	if !strings.HasPrefix(input, refSchemePrefix) {
		return nil, &refError{Class: errParse, Message: "must start with shardr:/// (empty authority)"}
	}
	rest := input[len(refSchemePrefix):]

	// Digest suffix. '@' may not appear in ns/name/sel/digest bodies, so at
	// most one '@' is possible; anything else is a parse error.
	digest := ""
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		digest = rest[i+1:]
		rest = rest[:i]
		if digest == "" || strings.ContainsRune(digest, '@') {
			return nil, &refError{Class: errParse, Message: "empty or repeated @digest"}
		}
		if !isDigest(digest) {
			return nil, &refError{Class: errDigestFormat, Message: "digest must be sha256:<64 lowercase hex>"}
		}
	}

	// Selector: first ':' after ns/name (ns/name/sel charsets exclude ':').
	sel := ""
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		sel = rest[i+1:]
		rest = rest[:i]
		if sel == "" {
			return nil, &refError{Class: errParse, Message: "empty selector"}
		}
	}

	// ns/name split.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return nil, &refError{Class: errParse, Message: "missing ns/name separator"}
	}
	ns, name := rest[:slash], rest[slash+1:]
	if strings.ContainsRune(name, '/') {
		return nil, &refError{Class: errParse, Message: "too many path segments"}
	}
	ns, name = strings.ToLower(ns), strings.ToLower(name) // 000 §2 silent lowercase
	if !isNS(ns) {
		return nil, &refError{Class: errParse, Message: "ns must match [a-z0-9][a-z0-9._-]{0,63} after lowercasing"}
	}
	if !isName(name) {
		return nil, &refError{Class: errParse, Message: "name must match [a-z0-9][a-z0-9._-]{0,127} after lowercasing"}
	}

	p := &refParse{NS: ns, Name: name, Digest: digest}
	if sel != "" {
		if err := validateSelector(sel, p); err != nil {
			return nil, err
		}
	}
	if sel == "" && digest == "" {
		return nil, &refError{Class: errNoSelector, Message: "no default selector in the scheme (000 §2)"}
	}

	var sb strings.Builder
	sb.WriteString(refSchemePrefix)
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

// validateSelector checks sel := quant | tag["+"quant] (000 §2) including the
// tag ban (000 §3.4). Fills p.Sel/Tag/Quant.
func validateSelector(sel string, p *refParse) *refError {
	p.Sel = sel
	if tag, quant, ok := strings.Cut(sel, "+"); ok {
		if !isTag(tag) {
			return &refError{Class: errParse, Message: "tag must match [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}"}
		}
		if err := checkTagBan(tag); err != nil {
			return err
		}
		if !quantSyntaxValid(quant) {
			return &refError{Class: errParse, Message: "quant part must match 000 Appendix A"}
		}
		p.Tag, p.Quant = tag, quant
		return nil
	}
	if !quantSyntaxValid(sel) {
		// Not quant syntax. The only other legal sel form is tag+quant, so a
		// bare tag-shaped string is a parse error (bare tags never select).
		return &refError{Class: errParse, Message: "selector must be a quant or tag+quant; bare tags are not selectors"}
	}
	p.Quant = sel
	return nil
}

func checkTagBan(tag string) *refError {
	low := strings.ToLower(tag)
	if strings.HasPrefix(low, "sha256-") || strings.HasPrefix(low, "sha256:") {
		return &refError{Class: errTagBanned, Message: "tags may not start with sha256-/sha256:"}
	}
	if quantSyntaxValid(low) {
		return &refError{Class: errTagBanned, Message: "quant-shaped strings select index members, they cannot be tags"}
	}
	return nil
}

// quantSyntaxValid implements 000 Appendix A: charset [a-z0-9_-], ≤ 24 chars,
// starts with a known prefix AND contains ≥ 1 digit, OR is the reserved term
// "raw". NOTE: as literally written the prefix list rejects "raw"; the
// reserved-term clause is read as overriding the prefix clause (see defect
// note in the vectors README and issue #3).
func quantSyntaxValid(s string) bool {
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

func isDigest(s string) bool {
	const p = "sha256:"
	if !strings.HasPrefix(s, p) || len(s) != len(p)+64 {
		return false
	}
	for _, r := range s[len(p):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func isNS(s string) bool   { return isAtom(s, 64) }
func isName(s string) bool { return isAtom(s, 128) }

// isTag: [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127} — case-sensitive local alias,
// preserved verbatim (000 §2).
func isTag(s string) bool {
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

// ----------------------------------------------------------------------------
// Resolution (000 §3) against a test-provided index context.
// ----------------------------------------------------------------------------

type indexMember struct {
	Quant    string `json:"quant"`
	Manifest string `json:"manifest"`
}

type resolveContext struct {
	Members   []indexMember            `json:"members,omitempty"`
	Snapshots map[string][]indexMember `json:"snapshots,omitempty"`
}

type resolution struct {
	Quant    string `json:"quant"`
	Manifest string `json:"manifest"`
}

// resolveRef resolves the quant of a parsed reference against the context
// index: exact match first, then unique-prefix expansion (000 §3.1), then the
// @digest pin equality check (000 §3.3).
func resolveRef(p *refParse, ctx *resolveContext) (*resolution, *refError) {
	if p.Quant == "" {
		return nil, nil // manifest-addressing form (@digest, no selector): nothing to resolve
	}
	members := ctx.Members
	if p.Tag != "" {
		snap, ok := ctx.Snapshots[p.Tag]
		if !ok {
			return nil, &refError{Class: errUnknownTag, Message: "unknown tag " + p.Tag}
		}
		members = snap
	}

	// Exact match wins over prefix matches.
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
		return nil, &refError{Class: errNoMember, Message: "no index member matches " + p.Quant}
	case 1:
		for _, m := range members {
			if m.Quant == candidates[0] {
				return checkPin(p, m)
			}
		}
		panic("unreachable")
	default:
		sort.Strings(candidates)
		return nil, &refError{Class: errAmbiguous, Candidates: candidates,
			Message: fmt.Sprintf("ambiguous prefix %q", p.Quant)}
	}
}

func checkPin(p *refParse, m indexMember) (*resolution, *refError) {
	if p.Digest != "" && p.Digest != m.Manifest {
		return nil, &refError{Class: errPinMismatch,
			Message: fmt.Sprintf("pin %s does not match resolved member %s (%s)", p.Digest, m.Quant, m.Manifest)}
	}
	return &resolution{Quant: m.Quant, Manifest: m.Manifest}, nil
}

// cliShortToCanonical expands the CLI short form ns/name[:sel][@digest] to
// the canonical URI (000 §2). It is input sugar only.
func cliShortToCanonical(input string) string {
	return refSchemePrefix + input
}
