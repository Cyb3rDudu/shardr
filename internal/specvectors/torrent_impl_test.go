package specvectors

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ----------------------------------------------------------------------------
// Minimal bencode encoder (test-only reference; BEP 3 encoding rules).
// Supported values: map[string]any ([]byte keys are NOT valid map keys in Go,
// so raw-byte keys are represented as string holding raw bytes), []any,
// string (raw bytes), int64. Dictionary keys are emitted sorted as raw
// byte sequences, per BEP 3.
// ----------------------------------------------------------------------------

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

// ----------------------------------------------------------------------------
// BitTorrent v2 per-file merkle tree (BEP 52).
//
// Leaves are SHA-256 of each 16 KiB block; the final block may be short.
// The leaf layer is padded to a power-of-two width with 32 zero bytes and
// the tree is reduced with SHA-256(left || right). This is the "zero-hash
// chain" padding of spec 004 §3 and matches libtorrent's merkle_root() and
// anacrolix/torrent's merkle.RootWithPadHash (pad hash = 32 zero bytes).
//
// Empty input (0 blocks): root = SHA-256(""), matching the reference
// implementations.
// ----------------------------------------------------------------------------

const merkleBlockSize = 1 << 14 // 16 KiB (BEP 52)

func merkleRoot(data []byte) [sha256.Size]byte {
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

// pieceLayerHashes returns the hashes of the merkle layer in which one hash
// covers pieceLength bytes, omitting hashes that exclusively cover padding
// beyond end of file (BEP 52 "piece layers"). Only called for files larger
// than pieceLength.
func pieceLayerHashes(data []byte, pieceLength int64) [][sha256.Size]byte {
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
	// Build the full padded tree, level by level, bottom-up.
	var zero [sha256.Size]byte
	level := make([][sha256.Size]byte, width)
	copy(level, leaves)
	for i := nBlocks; i < width; i++ {
		level[i] = zero
	}
	for len(level) > blocksPerPiece {
		next := make([][sha256.Size]byte, len(level)/2)
		for i := range next {
			next[i] = sha256.Sum256(append(append([]byte{}, level[i*2][:]...), level[i*2+1][:]...))
		}
		level = next
	}
	// Emit only hashes covering at least one real block: node i covers
	// blocks [i*blocksPerPiece, (i+1)*blocksPerPiece).
	var out [][sha256.Size]byte
	for i, h := range level {
		if i*blocksPerPiece < nBlocks {
			out = append(out, h)
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Piece-length ladder (spec 004 §3) — pure function of total torrent size.
// The vectors define totalSize as the sum of all file entry sizes plus the
// manifest file itself (which is part of the torrent file tree).
// ----------------------------------------------------------------------------

func ladderPieceLength(totalSize int64) int64 {
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

// ----------------------------------------------------------------------------
// Info-dict construction (spec 004 §3) from a decoded manifest.
// ----------------------------------------------------------------------------

// manifestDoc is the decoded shape of a canonical manifest (001 §3).
type manifestDoc struct {
	SchemaVersion int64             `json:"schemaVersion"`
	ArtifactType  string            `json:"artifactType"`
	Files         []manifestFile    `json:"files"`
	Annotations   map[string]string `json:"annotations"`
}

type manifestFile struct {
	Kind    string `json:"kind"`
	Digest  string `json:"digest"`
	Size    int64  `json:"size"`
	Name    string `json:"name"`
	Part    *int64 `json:"part"`
	Role    string `json:"role"`
	Runtime string `json:"runtime"`
	BT      struct {
		MerkleRoot string `json:"merkleRoot"`
	} `json:"bt"`
}

// infoDictResult carries the derived torrent identity of an artifact.
type infoDictResult struct {
	Name               string // torrent name: shardr-sha256-<12 hex>
	PieceLength        int64
	TotalSize          int64  // sum of entry sizes + manifest bytes
	Infohash           string // btmh:1220<hex>
	PieceLayersBencode []byte
	PieceLayersDigest  string // sha256:<hex> of the piece-layers blob
}

// buildInfoDict reconstructs the BEP 52 info dict for a manifest whose
// canonical bytes are manifestBytes with digest hex digestHex (64 lowercase
// hex chars, no prefix), deriving per-file bytes via the content resolver.
func buildInfoDict(m *manifestDoc, manifestBytes []byte, digestHex string, resolve func(name string) []byte) (infoBencode []byte, res infoDictResult, err error) {
	tree := map[string]any{}
	total := int64(len(manifestBytes))
	for _, f := range m.Files {
		data := resolve(f.Name)
		if int64(len(data)) != f.Size {
			return nil, res, fmt.Errorf("file %q: recipe size %d != manifest size %d", f.Name, len(data), f.Size)
		}
		root := merkleRoot(data)
		insertFileTree(tree, strings.Split(f.Name, "/"), f.Size, root)
		total += f.Size
	}
	manifestRoot := merkleRoot(manifestBytes)
	tree["manifest"] = map[string]any{
		"sha256-" + digestHex: map[string]any{
			"": map[string]any{"length": int64(len(manifestBytes)), "pieces root": string(manifestRoot[:])},
		},
	}

	pieceLength := ladderPieceLength(total)
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

	// Piece layers (BEP 52): entries for files larger than pieceLength, keyed
	// by raw merkle root, value = concatenated layer hashes.
	layers := map[string]any{}
	addLayer := func(size int64, root [sha256.Size]byte, data []byte) {
		if size <= pieceLength {
			return
		}
		var sb strings.Builder
		for _, h := range pieceLayerHashes(data, pieceLength) {
			sb.Write(h[:])
		}
		layers[string(root[:])] = sb.String()
	}
	for _, f := range m.Files {
		addLayer(f.Size, merkleRoot(resolve(f.Name)), resolve(f.Name))
	}
	addLayer(int64(len(manifestBytes)), manifestRoot, manifestBytes)
	res.PieceLayersBencode = bencode(layers)
	layerSum := sha256.Sum256(res.PieceLayersBencode)
	res.PieceLayersDigest = "sha256:" + hex.EncodeToString(layerSum[:])
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
	leaf := map[string]any{"": map[string]any{"length": size, "pieces root": string(merkleRootBytes[:])}}
	name := path[len(path)-1]
	if existing, ok := node[name]; ok {
		_ = existing // duplicate names are a 001 validation error, checked there
		panic("duplicate file tree path: " + strings.Join(path, "/"))
	}
	node[name] = leaf
}

// ----------------------------------------------------------------------------
// Deterministic file content recipes (documented in vectors README.md).
// ----------------------------------------------------------------------------

// recipeFile is the decoded sidecar entry.
type recipeFile struct {
	JSON         map[string]any `json:"json"`
	Repeat       string         `json:"repeat"`
	Sha256stream string         `json:"sha256stream"`
	Size         int64          `json:"size"`
}

func recipeBytes(r recipeFile) []byte {
	switch {
	case r.JSON != nil:
		return jcsMustMarshal(r.JSON)
	case r.Repeat != "":
		out := make([]byte, r.Size)
		for i := range out {
			out[i] = r.Repeat[i%len(r.Repeat)]
		}
		return out
	case r.Sha256stream != "":
		var out []byte
		for i := 0; int64(len(out)) < r.Size; i++ {
			sum := sha256.Sum256([]byte(r.Sha256stream + ":" + strconv.Itoa(i)))
			out = append(out, sum[:]...)
		}
		return out[:r.Size]
	default:
		panic("empty recipe")
	}
}

// pow2int reports whether n is a power of two (n ≥ 1).
func pow2int(n int64) bool {
	return n > 0 && n&(n-1) == 0
}
