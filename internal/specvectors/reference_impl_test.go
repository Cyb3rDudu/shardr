package specvectors

import (
	"github.com/Cyb3rDudu/shardr/internal/ref"
)

// ----------------------------------------------------------------------------
// Adapter over the production reference parser internal/ref (spec 000).
// The spec test vectors in docs/specs/vectors/000-reference.jsonl run
// against the same code the daemon uses, so vectors and production parser
// cannot drift apart. The test-only shapes below exist only to keep
// suites_test.go stable.
// ----------------------------------------------------------------------------

type (
	refError = ref.Error
	refParse = ref.Ref
)

// parseRef parses a canonical-form reference (000 §2).
func parseRef(input string) (*refParse, *refError) { return ref.Parse(input) }

// indexMember / resolveContext: test-provided index context (000 §3).
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
// index: current members, or the tag's snapshot members (000 §3.2).
func resolveRef(p *refParse, ctx *resolveContext) (*resolution, *refError) {
	if p.Quant == "" {
		return nil, nil // manifest-addressing form (@digest, no selector): nothing to resolve
	}
	members := ctx.Members
	if p.Tag != "" {
		snap, ok := ctx.Snapshots[p.Tag]
		if !ok {
			return nil, &refError{Class: ref.ErrUnknownTag, Message: "unknown tag " + p.Tag}
		}
		members = snap
	}
	prod := make([]ref.Member, len(members))
	for i, m := range members {
		prod[i] = ref.Member{Quant: m.Quant, Manifest: m.Manifest}
	}
	res, rerr := ref.Resolve(p, prod)
	if rerr != nil {
		return nil, rerr
	}
	if res == nil {
		return nil, nil
	}
	return &resolution{Quant: res.Quant, Manifest: res.Manifest}, nil
}

// cliShortToCanonical expands the CLI short form ns/name[:sel][@digest] to
// the canonical URI (000 §2). It is input sugar only.
func cliShortToCanonical(input string) string {
	return ref.SchemePrefix + input
}

// quantSyntaxValid delegates to the production quant syntax check (000 App. A).
func quantSyntaxValid(s string) bool { return ref.QuantSyntax(s) }
