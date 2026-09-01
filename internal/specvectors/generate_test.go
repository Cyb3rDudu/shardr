package specvectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate goldfiles under docs/specs/vectors/gold and print derived constants")

const vectorsDir = "../../docs/specs/vectors"
const goldDir = vectorsDir + "/gold"

// TestGenerateGoldfiles regenerates the 001 canonical goldfiles and the 004
// torrent goldfiles from the deterministic example artifact. Run:
//
//	go test ./internal/specvectors -update -run TestGenerateGoldfiles -v
//
// The printed constants are pasted into the JSONL vector files by hand —
// expected values in the vectors are committed data, never recomputed by the
// suite runners.
func TestGenerateGoldfiles(t *testing.T) {
	if !*update {
		t.Skip("pass -update to regenerate goldfiles")
	}

	// 1. Deterministic example files (004-example-files.json sidecar).
	sidecar := map[string]recipeFile{}
	raw, err := os.ReadFile(vectorsDir + "/004-example-files.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatal(err)
	}
	resolved := map[string][]byte{}
	for name, r := range sidecar {
		resolved[name] = recipeBytes(r)
	}

	// 2. Manifest from the derived file digests/sizes/merkle roots.
	type bt struct {
		MerkleRoot string `json:"merkleRoot"`
	}
	file := func(kind, name string, size int64, digest, root string, part *int64, role string) map[string]any {
		m := map[string]any{
			"kind":   kind,
			"digest": digest,
			"size":   size,
			"name":   name,
			"bt":     bt{MerkleRoot: root},
		}
		if part != nil {
			m["part"] = *part
		}
		if role != "" {
			m["role"] = role
		}
		return m
	}
	entry := func(kind, name string, part *int64, role string) map[string]any {
		data := resolved[name]
		sum := sha256.Sum256(data)
		root := merkleRoot(data)
		return file(kind, name, int64(len(data)),
			"sha256:"+hex.EncodeToString(sum[:]),
			"sha256:"+hex.EncodeToString(root[:]), part, role)
	}
	p := func(n int64) *int64 { return &n }
	manifest := map[string]any{
		"schemaVersion": 1,
		"artifactType":  "model",
		"files": []any{
			entry("config", "modelconfig.json", nil, ""),
			entry("weights.gguf", "qwen3.8-27b-q8_00001.gguf", p(1), ""),
			entry("weights.gguf", "qwen3.8-27b-q8_00002.gguf", p(2), ""),
			entry("weights.aux", "imatrix.gguf", nil, "imatrix"),
		},
		"annotations": map[string]any{
			"io.shardr.hf.repo":            "unsloth/Qwen3.8-27B-GGUF",
			"io.shardr.hf.revision":        "4ca7207fa5502f4a0f8b3d1e2c3d4e5f60718293",
			"io.shardr.import.specVersion": "0",
			"io.shardr.license":            "apache-2.0",
		},
	}
	manifestCanonical := jcsMustMarshal(manifest)
	manifestDigestHex := fmt.Sprintf("%x", sha256.Sum256(manifestCanonical))
	writeGold(t, "manifest-01.canonical.json", manifestCanonical)
	// Input serializations deliberately violate every JCS rule the reference
	// implementation must normalize: reversed key order at every level,
	// whitespace, and (for input2) compact encoding.
	writeGold(t, "manifest-01.input.json", []byte(emitReversed(manifest, "\t", "\n")))
	writeGold(t, "manifest-01.input2.json", []byte(emitReversed(manifest, "", "")))

	// 3. Model index with the real manifest digest + two fabricated ones.
	fake := func(label string) string {
		sum := sha256.Sum256([]byte(label))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	index := map[string]any{
		"schemaVersion": 1,
		"artifactType":  "model-index",
		"members": []any{
			map[string]any{"manifest": "sha256:" + manifestDigestHex, "quant": "q8_0", "weightsFormat": "gguf", "revision": "4ca7207fa5502f4a0f8b3d1e2c3d4e5f60718293"},
			map[string]any{"manifest": fake("shardr-vector/001/fake-manifest/q4_0"), "quant": "q4_0", "weightsFormat": "gguf", "revision": "4ca7207fa5502f4a0f8b3d1e2c3d4e5f60718293"},
			map[string]any{"manifest": fake("shardr-vector/001/fake-manifest/q4_1"), "quant": "q4_1", "weightsFormat": "gguf", "revision": "4ca7207fa5502f4a0f8b3d1e2c3d4e5f60718293"},
		},
	}
	indexCanonical := jcsMustMarshal(index)
	writeGold(t, "index-01.canonical.json", indexCanonical)
	writeGold(t, "index-01.input.json", []byte(emitReversed(index, "\t", "\n")))

	// 4. Info dict + piece layers from the manifest.
	var mDoc manifestDoc
	if err := json.Unmarshal(manifestCanonical, &mDoc); err != nil {
		t.Fatal(err)
	}
	infoBenc, res, err := buildInfoDict(&mDoc, manifestCanonical, manifestDigestHex, func(name string) []byte { return resolved[name] })
	if err != nil {
		t.Fatal(err)
	}
	writeGold(t, "manifest-01.info-dict.benc", infoBenc)
	writeGold(t, "manifest-01.piece-layers.benc", res.PieceLayersBencode)

	// 5. Distribution record.
	dist := map[string]any{
		"schemaVersion":  1,
		"artifactType":   "distribution",
		"manifestDigest": "sha256:" + manifestDigestHex,
		"torrent": map[string]any{
			"infohash":          res.Infohash,
			"pieceLength":       res.PieceLength,
			"pieceLayersDigest": res.PieceLayersDigest,
		},
	}
	distCanonical := jcsMustMarshal(dist)
	writeGold(t, "distribution-01.canonical.json", distCanonical)
	writeGold(t, "distribution-01.input.json", []byte(emitReversed(dist, "\t", "\n")))

	// 6. Print every constant for the JSONL vectors.
	fmt.Println("=== derived constants (paste into JSONL vectors) ===")
	fmt.Println("manifest-01.digest      = sha256:" + manifestDigestHex)
	fmt.Println("index-01.digest         = " + flatSHA256(indexCanonical))
	fmt.Println("distribution-01.digest  = " + flatSHA256(distCanonical))
	fmt.Println("torrent.name            = " + res.Name)
	fmt.Println("torrent.pieceLength     =", res.PieceLength)
	fmt.Println("torrent.totalSize       =", res.TotalSize)
	fmt.Println("torrent.infohash        = " + res.Infohash)
	fmt.Println("torrent.pieceLayersDigest = " + res.PieceLayersDigest)
	for _, name := range []string{"modelconfig.json", "qwen3.8-27b-q8_00001.gguf", "qwen3.8-27b-q8_00002.gguf", "imatrix.gguf"} {
		data := resolved[name]
		root := merkleRoot(data)
		fmt.Printf("file %-28s size=%-8d digest=%s root=%s\n", name, len(data), flatSHA256(data), "sha256:"+hex.EncodeToString(root[:]))
	}

	// Standalone merkle roots for suite 004.
	fmt.Println("--- standalone merkle roots ---")
	merkleKV := map[string][]byte{
		"empty":        {},
		"hello-5":      []byte("hello"),
		"A-16384":      recipeBytes(recipeFile{Repeat: "A", Size: 16384}),
		"A-16389":      recipeBytes(recipeFile{Repeat: "A", Size: 16389}),
		"zeros-32768":  recipeBytes(recipeFile{Repeat: "\x00", Size: 32768}),
		"zeros-49152":  recipeBytes(recipeFile{Repeat: "\x00", Size: 49152}),
		"stream-49152": recipeBytes(recipeFile{Sha256stream: "shardr-vector/004/merkle", Size: 49152}),
	}
	for _, k := range []string{"empty", "hello-5", "A-16384", "A-16389", "zeros-32768", "zeros-49152", "stream-49152"} {
		root := merkleRoot(merkleKV[k])
		fmt.Printf("merkle %-12s = sha256:%s\n", k, hex.EncodeToString(root[:]))
	}
	// 193-block file piece-layer hashes (1 MiB pieces) for layer inspection.
	lh := pieceLayerHashes(resolved["qwen3.8-27b-q8_00001.gguf"], 1<<20)
	for i, h := range lh {
		fmt.Printf("weights-1 pieceLayer[%d] = sha256:%s\n", i, hex.EncodeToString(h[:]))
	}
}

func writeGold(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(goldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goldDir, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote gold/%s (%d bytes)", name, len(content))
}

// emitReversed serializes a decoded JSON document with object keys in
// REVERSE sorted order at every level (and the requested whitespace), so the
// 001 suite proves canonicalization is independent of input key order —
// something encoding/json cannot produce (it always sorts map keys).
func emitReversed(v any, indent, newline string) string {
	var sb strings.Builder
	emitRev(&sb, v, indent, newline, 0)
	return sb.String()
}

func emitRev(sb *strings.Builder, v any, indent, newline string, depth int) {
	pad := strings.Repeat(indent, depth)
	inner := strings.Repeat(indent, depth+1)
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			if newline != "" {
				sb.WriteString(newline + inner)
			}
			kb, _ := json.Marshal(k)
			sb.Write(kb)
			sb.WriteByte(':')
			if newline != "" {
				sb.WriteByte(' ')
			}
			emitRev(sb, t[k], indent, newline, depth+1)
		}
		if newline != "" && len(keys) > 0 {
			sb.WriteString(newline + pad)
		}
		sb.WriteByte('}')
	case []any:
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			if newline != "" {
				sb.WriteString(newline + inner)
			}
			emitRev(sb, e, indent, newline, depth+1)
		}
		if newline != "" && len(t) > 0 {
			sb.WriteString(newline + pad)
		}
		sb.WriteByte(']')
	default:
		b, _ := json.Marshal(v)
		sb.Write(b)
	}
}
