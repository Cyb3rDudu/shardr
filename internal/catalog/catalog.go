// Package catalog implements the catalog provider layer (Epic #65):
// discovery of community-listed model torrents. Catalogs are discovery
// + transport ONLY — a magnet from a catalog is never trusted by itself
// (005 §5 law); the trust anchor for bytes is either Hugging Face at the
// pinned revision, or — for rescued models under explicit --trust-
// catalog — the catalog's own recorded checksums, accepted with a loud
// warning that trust shifts from HF to the catalog provider.
package catalog

import "context"

// Provider is one catalog source. v1 ships exactly one implementation
// (pirateface); the interface is the client-side blueprint for more.
type Provider interface {
	// Name is the stable provider id (CLI/annotations).
	Name() string
	// Search runs a term query and returns the listed models.
	Search(ctx context.Context, terms string) ([]Model, error)
	// Resolve fetches one repo's listing record: magnet, transport hints,
	// pinned revision, and the catalog-recorded per-file checksums.
	Resolve(ctx context.Context, repo string) (*Resolved, error)
	// Checksums fetches the provider's recorded per-file SHA-256 record
	// for a repo (the rescued-anchor source under --trust-catalog).
	Checksums(ctx context.Context, repo string) ([]FileChecksum, error)
}

// Model is one search hit (listing, not a guarantee of availability).
type Model struct {
	Repo   string // owner/name
	Size   string // human size label as served ("26 MB")
	Seeds  int    // reported seeders
	Magnet string
}

// FileChecksum is one catalog-recorded per-file SHA-256 (the provider's
// copy of the official HF hashes for LFS weight files).
type FileChecksum struct {
	Path   string
	SHA256 string // bare 64-hex
}

// Resolved is the full listing record for one repo.
type Resolved struct {
	Repo     string
	Revision string   // 40-hex HF commit pinned by the listing's webseed
	Infohash string   // v1, 40 hex — the foreign torrent's identity
	Magnet   string   // the listed magnet (uri-escaped as served)
	Trackers []string // tr= values
	Webseeds []string // ws= values
	Size     string
	Seeds    int
}
