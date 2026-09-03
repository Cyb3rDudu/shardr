// Package artifact implements the shardr artifact formats per spec 001:
// model manifests, model indexes, distribution records — all canonical
// (RFC 8785 JCS) JSON documents, content-addressed by flat SHA-256 — plus
// the BitTorrent v2 binding math per spec 004 (bencode, per-file merkle
// trees with BEP 52 zero-hash padding, the piece-length ladder, infohash
// and piece-layers construction).
//
// The implementations are ports of the machine-checked reference code in
// internal/specvectors: the spec test vectors (docss/specs/vectors) run
// against THIS package, so vectors and production math cannot drift.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Minimal bencode encoder (BEP 3). Supported values: int, int64, string
// (raw bytes), []byte, []any, map[string]any. Dictionary keys are emitted
// sorted as raw byte sequences.
// ---------------------------------------------------------------------------

func bencode(v any) []byte {
	var b strings.Builder
	bencodeValue(&b, v)
	return []byte(b.String())
}

func bencodeValue(b *strings.Builder, v any) {
	switch t := v.(type) {
	case int:
		bencodeValue(b, int64(t))
	case int64:
		fmt.Fprintf(b, "i%de", t)
	case string: // raw bytes
		fmt.Fprintf(b, "%d:%s", len(t), t)
	case []byte:
		fmt.Fprintf(b, "%d:%s", len(t), string(t))
	case []any:
		b.WriteByte('l')
		for _, e := range t {
			bencodeValue(b, e)
		}
		b.WriteByte('e')
	case map[string]any:
		b.WriteByte('d')
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			fmt.Fprintf(b, "%d:%s", len(k), k)
			bencodeValue(b, t[k])
		}
		b.WriteByte('e')
	default:
		panic(fmt.Sprintf("bencode: unsupported type %T", v))
	}
}

// ---------------------------------------------------------------------------
// BitTorrent v2 per-file merkle tree (BEP 52), spec 004 §3.
//
// Leaves are SHA-256 of each 16 KiB block; the final block may be short.
// The leaf layer is padded to a power-of-two width with 32 zero bytes and
// the tree is reduced with SHA-256(left || right) — the "zero-hash chain"
// padding, matching libtorrent's merkle_root() and anacrolix/torrent's
// merkle.RootWithPadHash (pad hash = 32 zero bytes). Empty input (0
// blocks): root = SHA-256("").
// ---------------------------------------------------------------------------

const merkleBlockSize = 1 << 14 // 16 KiB (BEP 52)

// MerkleRoot returns the BEP 52 merkle root of data.
func MerkleRoot(data []byte) [sha256.Size]byte {
	leaves := merkleLeaves(data)
	if len(leaves) == 0 {
		return sha256.Sum256(nil)
	}
	width := nextPow2(len(leaves))
	var zero [sha256.Size]byte
	for i := len(leaves); i < width; i++ {
		leaves = append(leaves, zero)
	}
	for len(leaves) > 1 {
		next := make([][sha256.Size]byte, len(leaves)/2)
		for i := range next {
			next[i] = sha256.Sum256(append(append([]byte{}, leaves[i*2][:]...), leaves[i*2+1][:]...))
		}
		leaves = next
	}
	return leaves[0]
}

func merkleLeaves(data []byte) [][sha256.Size]byte {
	var leaves [][sha256.Size]byte
	for off := 0; off < len(data); off += merkleBlockSize {
		end := off + merkleBlockSize
		if end > len(data) {
			end = len(data)
		}
		leaves = append(leaves, sha256.Sum256(data[off:end]))
	}
	return leaves
}

func nextPow2(n int) int {
	if n < 1 {
		return 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// PieceLayerHashes returns the hashes of the merkle layer in which one
// hash covers pieceLength bytes, omitting hashes that exclusively cover
// padding beyond end of file (BEP 52 "piece layers"). Only valid for
// files larger than pieceLength.
func PieceLayerHashes(data []byte, pieceLength int64) [][sha256.Size]byte {
	leaves := merkleLeaves(data)
	nBlocks := len(leaves)
	width := nextPow2(nBlocks)
	if width == 0 {
		width = 1
	}
	blocksPerPiece := int(pieceLength) / merkleBlockSize
	if blocksPerPiece < 1 || blocksPerPiece&(blocksPerPiece-1) != 0 {
		panic("pieceLength must be a power-of-two multiple of 16 KiB")
	}
	// The piece layer is the tree layer whose entries each cover
	// blocksPerPiece blocks: its width is width/blocksPerPiece (both powers
	// of two). Stopping at len(level)==blocksPerPiece would instead emit
	// the layer whose entries cover blocksPerPiece/width blocks — hashes
	// that no BTv2 client accepts (caught by anacrolix's root check).
	target := width / blocksPerPiece
	if target < 1 {
		target = 1 // file <= one piece: not piece-layer territory, callers skip it
	}
	var zero [sha256.Size]byte
	level := make([][sha256.Size]byte, width)
	copy(level, leaves)
	for i := nBlocks; i < width; i++ {
		level[i] = zero
	}
	for len(level) > target {
		next := make([][sha256.Size]byte, len(level)/2)
		for i := range next {
			next[i] = sha256.Sum256(append(append([]byte{}, level[i*2][:]...), level[i*2+1][:]...))
		}
		level = next
	}
	var out [][sha256.Size]byte
	for i, h := range level {
		if i*blocksPerPiece < nBlocks {
			out = append(out, h)
		}
	}
	return out
}

// LadderPieceLength is the piece-length ladder (spec 004 §3), a pure
// function of the total torrent size (all file entry sizes plus the
// manifest file itself, which is part of the torrent file tree).
func LadderPieceLength(totalSize int64) int64 {
	const (
		KiB, MiB, GiB = 1 << 10, 1 << 20, 1 << 30
	)
	switch {
	case totalSize <= 1*GiB:
		return 1 * MiB
	case totalSize <= 8*GiB:
		return 4 * MiB
	case totalSize <= 64*GiB:
		return 16 * MiB
	case totalSize <= 512*GiB:
		return 64 * MiB
	default:
		return 256 * MiB
	}
}

// ---------------------------------------------------------------------------
// Info-dict construction (spec 004 §3): deterministic torrent identity
// from a manifest. The manifest blob itself is part of the file tree at
// manifest/sha256-<hex>.
// ---------------------------------------------------------------------------

// DigestHex strips the canonical sha256: prefix (64 lowercase hex in,
// bare hex out).
func DigestHex(d string) string { return strings.TrimPrefix(d, "sha256:") }

// BuildTorrent derives the torrent identity of a manifest whose canonical
// bytes are manifestBytes with digest hex digestHex (bare hex). resolve
// returns the content bytes of a file entry by manifest name. It returns
// the bencoded info dict plus the derived identity fields.
//
// Every computed per-file merkle root MUST equal the entry's pinned
// bt.merkleRoot (001 §3.1): a manifest that lies about its own torrent
// geometry is rejected here, not just downstream at the infohash gate.
func BuildTorrent(files []File, manifestBytes []byte, digestHex string, resolve func(name string) ([]byte, error)) (infoBencode []byte, res TorrentResult, err error) {
	tree := map[string]any{}
	total := int64(len(manifestBytes))
	roots := map[string][sha256.Size]byte{}
	for _, f := range files {
		data, err := resolve(f.Name)
		if err != nil {
			return nil, res, err
		}
		if int64(len(data)) != f.Size {
			return nil, res, fmt.Errorf("file %q: content size %d != manifest size %d", f.Name, len(data), f.Size)
		}
		root := MerkleRoot(data)
		if pinned := DigestHex(f.BT.MerkleRoot); pinned != hex.EncodeToString(root[:]) {
			return nil, res, fmt.Errorf("file %q: computed merkle root sha256:%x != pinned bt.merkleRoot %s (manifest lies about its torrent geometry)", f.Name, root, f.BT.MerkleRoot)
		}
		roots[f.Name] = root
		insertFileTree(tree, strings.Split(f.Name, "/"), f.Size, root)
		total += f.Size
	}
	manifestRoot := MerkleRoot(manifestBytes)
	roots["manifest/sha256-"+digestHex] = manifestRoot
	tree["manifest"] = map[string]any{
		"sha256-" + digestHex: map[string]any{
			"": map[string]any{"length": int64(len(manifestBytes)), "pieces root": string(manifestRoot[:])},
		},
	}

	pieceLength := LadderPieceLength(total)
	res.TotalSize = total
	res.PieceLength = pieceLength
	res.Name = "shardr-sha256-" + digestHex[:12]

	info := map[string]any{
		"file tree":    tree,
		"meta version": int64(2),
		"name":         res.Name,
		"piece length": pieceLength,
	}
	infoBencode = bencode(info)
	sum := sha256.Sum256(infoBencode)
	res.Infohash = "btmh:1220" + hex.EncodeToString(sum[:])

	// Piece layers (BEP 52): entries for files larger than pieceLength,
	// keyed by raw merkle root, value = concatenated layer hashes. Only the
	// content path can compute them; BuildTorrentFromManifest leaves them
	// empty and the caller supplies the layers blob from the CAS (004 §5).
	layers := map[string]any{}
	addLayer := func(size int64, root [sha256.Size]byte, data []byte) {
		if size <= pieceLength {
			return
		}
		var sb strings.Builder
		for _, h := range PieceLayerHashes(data, pieceLength) {
			sb.Write(h[:])
		}
		layers[string(root[:])] = sb.String()
	}
	for _, f := range files {
		data, err := resolve(f.Name)
		if err != nil {
			return nil, res, err
		}
		addLayer(f.Size, roots[f.Name], data)
	}
	addLayer(int64(len(manifestBytes)), manifestRoot, manifestBytes)
	res.PieceLayersBencode = bencode(layers)
	layerSum := sha256.Sum256(res.PieceLayersBencode)
	res.PieceLayersDigest = "sha256:" + hex.EncodeToString(layerSum[:])
	return infoBencode, res, nil
}

// BuildTorrentFromManifest reconstructs the torrent identity from a
// manifest ALONE (004 §3, 001 §7.4): file tree from the pinned
// bt.merkleRoot entries, piece length from the ladder. No content bytes
// are needed — this is how a node that holds the manifest but not the
// blobs derives the infohash to join the swarm (and how the 001 §6
// binding check is evaluated without local content). The piece-layers
// blob is NOT derived (it needs content); the caller supplies it from
// the CAS or a source hint, keyed by root exactly like the layers map.
func BuildTorrentFromManifest(files []File, manifestBytes []byte, digestHex string) (infoBencode []byte, res TorrentResult, err error) {
	tree := map[string]any{}
	total := int64(len(manifestBytes))
	manifestRoot := MerkleRoot(manifestBytes)
	for _, f := range files {
		pinned := DigestHex(f.BT.MerkleRoot)
		var root [sha256.Size]byte
		if _, err := hex.Decode(root[:], []byte(pinned)); err != nil {
			return nil, res, fmt.Errorf("file %q: pinned bt.merkleRoot %q is not 64 hex chars", f.Name, f.BT.MerkleRoot)
		}
		insertFileTree(tree, strings.Split(f.Name, "/"), f.Size, root)
		total += f.Size
	}
	tree["manifest"] = map[string]any{
		"sha256-" + digestHex: map[string]any{
			"": map[string]any{"length": int64(len(manifestBytes)), "pieces root": string(manifestRoot[:])},
		},
	}
	pieceLength := LadderPieceLength(total)
	res.TotalSize = total
	res.PieceLength = pieceLength
	res.Name = "shardr-sha256-" + digestHex[:12]
	info := map[string]any{
		"file tree":    tree,
		"meta version": int64(2),
		"name":         res.Name,
		"piece length": pieceLength,
	}
	infoBencode = bencode(info)
	sum := sha256.Sum256(infoBencode)
	res.Infohash = "btmh:1220" + hex.EncodeToString(sum[:])
	return infoBencode, res, nil
}

func insertFileTree(root map[string]any, path []string, size int64, merkleRootBytes [sha256.Size]byte) {
	node := root
	for _, seg := range path[:len(path)-1] {
		next, ok := node[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[seg] = next
		}
		node = next
	}
	name := path[len(path)-1]
	if _, ok := node[name]; ok {
		panic("duplicate file tree path: " + strings.Join(path, "/"))
	}
	node[name] = map[string]any{"": map[string]any{"length": size, "pieces root": string(merkleRootBytes[:])}}
}

// TorrentResult carries the derived torrent identity of an artifact.
type TorrentResult struct {
	Name               string // torrent name: shardr-sha256-<12 hex>
	PieceLength        int64
	TotalSize          int64 // sum of entry sizes + manifest bytes
	Infohash           string
	PieceLayersBencode []byte
	PieceLayersDigest  string // sha256:<hex> of the piece-layers blob
}
