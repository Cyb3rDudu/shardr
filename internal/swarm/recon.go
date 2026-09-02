package swarm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
)

// ---------------------------------------------------------------------------
// Torrent reconstruction (004 §3) and the binding gate (001 §6, 004 §3):
// the info dict is a pure function of the manifest. A node holding only
// the manifest derives the infohash without any content; the derived
// identity MUST match the distribution record before the torrent is
// announced to anyone (Goal: digest world ↔ torrent world binding).
// ---------------------------------------------------------------------------

// ParseInfohash accepts "btmh:1220<hex>", "sha256:<hex>", bare 64-hex, or
// a full magnet URI (xt=urn:btmh:…) and returns the 64-hex v2 infohash.
func ParseInfohash(s string) (string, error) {
	if strings.HasPrefix(s, "magnet:") {
		m, err := metainfo.ParseMagnetV2Uri(s)
		if err != nil {
			return "", fmt.Errorf("swarm: parse magnet: %w", err)
		}
		if !m.V2InfoHash.Ok {
			return "", fmt.Errorf("swarm: magnet %q carries no urn:btmh v2 infohash (shardr torrents are pure BTv2, 004 §3)", s)
		}
		return hex.EncodeToString(m.V2InfoHash.Value[:]), nil
	}
	hexPart := s
	hexPart = strings.TrimPrefix(hexPart, "btmh:1220")
	hexPart = strings.TrimPrefix(hexPart, "urn:btmh:1220")
	hexPart = strings.TrimPrefix(hexPart, "sha256:")
	if len(hexPart) != 64 {
		return "", fmt.Errorf("swarm: infohash %q: want btmh:1220 + 64 hex chars", s)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("swarm: infohash %q: %w", s, err)
	}
	return strings.ToLower(hexPart), nil
}

// Sentinel errors mapped by the API layer to 005 §3 error classes.
var (
	// ErrBinding: the 001 §6 binding failed — derived identity disagrees
	// with the record or the joined torrent. Loud abort, never a warning.
	ErrBinding = errors.New("swarm: identity binding failed")
)

// Recon is a reconstructed torrent identity from a manifest alone.
type Recon struct {
	InfohashHex     string // 64 hex chars
	InfohashBtmh    string // btmh:1220<hex>
	InfoBencode     []byte
	PieceLength     int64
	TotalSize       int64
	Name            string
	ManifestDigest  string // sha256:<hex>
	ManifestHex     string
	FileSpecs       map[string]FileSpec // torrent path → CAS spec
	ManifestTorrent bool
}

// Reconstruct derives the torrent identity from a manifest (001 §7.4:
// torrent-reconstructable). No content bytes are touched.
func Reconstruct(m *artifact.Manifest, manifestBytes []byte) (*Recon, error) {
	manifestSum := sha256.Sum256(manifestBytes)
	digestHex := hex.EncodeToString(manifestSum[:])
	infoBencode, res, err := artifact.BuildTorrentFromManifest(m.Files, manifestBytes, digestHex)
	if err != nil {
		return nil, fmt.Errorf("swarm: reconstruct: %w", err)
	}
	ihHex := strings.TrimPrefix(res.Infohash, "btmh:1220")
	specs := make(map[string]FileSpec, len(m.Files)+1)
	for _, f := range m.Files {
		specs[f.Name] = FileSpec{Digest: artifact.DigestHex(f.Digest), Size: f.Size}
	}
	specs["manifest/sha256-"+digestHex] = FileSpec{Digest: digestHex, Size: int64(len(manifestBytes))}
	return &Recon{
		InfohashHex:    ihHex,
		InfohashBtmh:   res.Infohash,
		InfoBencode:    infoBencode,
		PieceLength:    res.PieceLength,
		TotalSize:      res.TotalSize,
		Name:           res.Name,
		ManifestDigest: "sha256:" + digestHex,
		ManifestHex:    digestHex,
		FileSpecs:      specs,
	}, nil
}

// CheckRecord enforces the 001 §6 / 004 §3 binding: the infohash derived
// from the manifest MUST equal distribution.torrent.infohash, and the
// piece length MUST match the ladder result implied by the record. This
// runs before any announce (seed start, fill, import) — a mismatch is a
// loud abort, never a warning.
func (r *Recon) CheckRecord(rec *artifact.DistributionRecord) error {
	if rec.ManifestDigest != r.ManifestDigest {
		return fmt.Errorf("%w: record pins manifest %s but this manifest is %s", ErrBinding, rec.ManifestDigest, r.ManifestDigest)
	}
	if rec.Torrent.Infohash != r.InfohashBtmh {
		return fmt.Errorf("%w: infohash mismatch — derived %s from manifest, record says %s (digest world and torrent world disagree; refusing to announce)", ErrBinding, r.InfohashBtmh, rec.Torrent.Infohash)
	}
	if rec.Torrent.PieceLength != r.PieceLength {
		return fmt.Errorf("%w: piece length mismatch — derived %d (ladder), record says %d", ErrBinding, r.PieceLength, rec.Torrent.PieceLength)
	}
	return nil
}

// LayersFromBlob decodes a CAS piece-layers blob (bencoded dict
// root → concatenated layer hashes, as built by artifact.BuildTorrent)
// into the map anacrolix consumes.
func LayersFromBlob(blob []byte) (map[string]string, error) {
	layers := map[string]string{}
	if err := bencode.Unmarshal(blob, &layers); err != nil {
		return nil, fmt.Errorf("swarm: decode piece-layers blob: %w", err)
	}
	return layers, nil
}

// torrentSpecFromMetaInfo builds the TorrentSpec for a pure-v2 torrent.
// NOTE: v1.61.0 released panics on pure-v2 specs (issue #1089) — this
// build pins upstream master 4ad31c5 which fixed it; the helper exists so
// the v2-only spec shape is constructed in exactly one place.
func torrentSpecFromMetaInfo(mi *metainfo.MetaInfo) (*torrent.TorrentSpec, error) {
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, fmt.Errorf("swarm: unmarshal info: %w", err)
	}
	if info.HasV1() {
		return nil, fmt.Errorf("swarm: torrent carries v1 fields; shardr torrents are pure BTv2 (004 §3)")
	}
	var v2 g.Option[infohash_v2.T]
	v2.Set(infohash_v2.HashBytes(mi.InfoBytes))
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{
			InfoHashV2: v2,
			InfoBytes:  mi.InfoBytes,
		},
		PieceLayers: mi.PieceLayers,
		DisplayName: info.BestName(),
	}, nil
}

// specFromRecon builds a TorrentSpec from a reconstruction plus acquired
// piece layers. peers are trusted direct-injection addresses (tests,
// source-hint x.pe equivalents).
func specFromRecon(r *Recon, layers map[string]string, trackers, webseeds, peerAddrs []string) (*torrent.TorrentSpec, error) {
	var v2 g.Option[infohash_v2.T]
	var ih infohash_v2.T
	if _, err := hex.Decode(ih[:], []byte(r.InfohashHex)); err != nil {
		return nil, err
	}
	v2.Set(ih)
	var tr [][]string
	if len(trackers) > 0 {
		tr = [][]string{trackers}
	}
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{
			InfoHashV2: v2,
			InfoBytes:  r.InfoBencode,
		},
		PieceLayers: layers,
		DisplayName: r.Name,
		Trackers:    tr,
		Webseeds:    webseeds,
		PeerAddrs:   peerAddrs,
	}, nil
}
