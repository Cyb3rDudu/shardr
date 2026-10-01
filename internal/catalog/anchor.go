package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Cyb3rDudu/shardr/internal/importer"
)

// ErrNotAnchored: no trust anchor exists for the bytes and the caller has
// not shifted trust to the catalog (Epic #65 trust chain 3). Loud by
// design — this is the rescued-model refusal.
var ErrNotAnchored = errors.New("catalog: no anchor — the model is rescued (its Hugging Face source is gone) and the only digest record is the catalog provider's own; re-run with --trust-catalog to accept that trust shift (bytes then verify against the catalog record, NOT against Hugging Face)")

// Anchor is the of-record digest set a pull verifies every byte against.
type Anchor struct {
	Repo     string
	Revision string
	// Source names whose word the digests are: "huggingface" (fetched
	// from the HF tree API at the pinned revision) or "catalog" (the
	// provider's recorded checksums, --trust-catalog only).
	Source string
	// Rescued: no HF anchor exists (HF repo or revision gone).
	Rescued bool
	// Files pins every file the anchor covers, by repo path. Live HF
	// anchors cover ALL files (lfs sha256 for LFS, git blob sha1 for the
	// rest); catalog anchors cover the recorded LFS weight files only —
	// uncovered files ride on the torrent's own piece hashes, which is
	// exactly what accepting the catalog's trust means.
	Files []AnchorFile
}

// AnchorFile is one pinned file. SHA256 is the CAS-space digest (LFS
// files); GitSHA1 additionally pins non-LFS files (sha1("blob <n>\0"+b)).
type AnchorFile struct {
	Path    string
	Size    int64
	SHA256  string // bare 64-hex, "" when HF publishes none for the file
	GitSHA1 string // 40-hex git blob id, live anchors only
}

// ResolveAnchor builds the anchor of record for a resolved listing:
//
//  1. HF tree at the pinned revision. Reachable → live anchor covering
//     every file (digests, never transports: the catalog only pointed at
//     the revision).
//  2. HF gone (repo or revision) → rescued. Without trustCatalog this
//     refuses with ErrNotAnchored. With it, the anchor becomes the
//     catalog's checksum record (LFS weight files) under a loud warning.
func ResolveAnchor(ctx context.Context, hf *importer.HFClient, pf Provider, r *Resolved, trustCatalog bool) (*Anchor, error) {
	tree, terr := hf.ListRepoTree(ctx, r.Repo, r.Revision)
	if terr == nil {
		a := &Anchor{Repo: r.Repo, Revision: r.Revision, Source: "huggingface", Files: make([]AnchorFile, 0, len(tree))}
		for _, tf := range tree {
			if tf.Path == ".gitattributes" {
				continue // repo bookkeeping, never part of a catalog torrent
			}
			a.Files = append(a.Files, AnchorFile{Path: tf.Path, Size: tf.Size, SHA256: tf.LFSOID, GitSHA1: tf.GitOID})
		}
		return a, nil
	}
	if !errors.Is(terr, importer.ErrUnknownRepo) {
		return nil, fmt.Errorf("catalog: anchor fetch for %s@%s: %w", r.Repo, r.Revision, terr)
	}
	// Rescued: HF has neither the repo nor the revision.
	if !trustCatalog {
		return nil, ErrNotAnchored
	}
	cs, cerr := pf.Checksums(ctx, r.Repo) // == *Pirateface in v1
	if cerr != nil {
		return nil, fmt.Errorf("catalog: rescued anchor (--trust-catalog) for %s: %w", r.Repo, cerr)
	}
	a := &Anchor{Repo: r.Repo, Revision: r.Revision, Source: "catalog", Rescued: true}
	for _, c := range cs {
		a.Files = append(a.Files, AnchorFile{Path: c.Path, SHA256: c.SHA256})
	}
	return a, nil
}

// Warning returns the loud trust-shift notice for rescued pulls.
func (a *Anchor) Warning() string {
	if !a.Rescued {
		return ""
	}
	return "trust shifts from Hugging Face to the catalog provider (" + a.Source + " anchor): weight files verify against the catalog-recorded SHA-256s; files without a recorded digest ride on the listed torrent's own piece hashes"
}

// ByPath indexes the anchor files.
func (a *Anchor) ByPath() map[string]AnchorFile {
	m := make(map[string]AnchorFile, len(a.Files))
	for _, f := range a.Files {
		m[f.Path] = f
	}
	return m
}

// Describe renders the anchor for CLI output (one line per file).
func (a *Anchor) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "anchor %s %s@%s (%d files)", a.Source, a.Repo, a.Revision, len(a.Files))
	if a.Rescued {
		b.WriteString(" [rescued]")
	}
	return b.String()
}
