package specvectors

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vectorPath resolves a vectors-dir-relative path.
func vectorPath(t *testing.T, rel string) string {
	t.Helper()
	if rel == "" {
		return ""
	}
	p := filepath.Join(vectorsDir, rel)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("vector asset %s: %v", rel, err)
	}
	return p
}

// loadJSONL decodes a JSONL vector suite. Blank lines are skipped.
func loadJSONL(t *testing.T, rel string) []map[string]any {
	t.Helper()
	f, err := os.Open(vectorPath(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatalf("%s:%d: invalid JSON: %v", rel, line, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s: no vectors", rel)
	}
	return out
}

func vString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func vBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func vFloat(m map[string]any, key string) float64 {
	f, _ := m[key].(float64)
	return f
}

// expectBlock returns the nested expect object.
func expectBlock(t testing.TB, v map[string]any) map[string]any {
	t.Helper()
	e, ok := v["expect"].(map[string]any)
	if !ok {
		t.Fatalf("%s: missing expect block", vString(v, "id"))
	}
	return e
}

// assertField compares an expected string field when present in expect.
func assertField(t testing.TB, expect map[string]any, key, got string) {
	t.Helper()
	if want, ok := expect[key].(string); ok && want != got {
		t.Errorf("%s: got %s=%q, want %q", key, key, got, want)
	}
}

// assertErrorClass verifies a must-fail expectation.
func assertErrorClass(t testing.TB, expect map[string]any, errClass string, candidates []string) {
	t.Helper()
	if !vBool(expect, "ok") {
		want := vString(expect, "errorClass")
		if errClass != want {
			t.Errorf("error class: got %s, want %s", errClass, want)
		}
		wantC, _ := expect["candidates"].([]any)
		if len(wantC) > 0 {
			if len(candidates) != len(wantC) {
				t.Errorf("candidates: got %v, want %v", candidates, wantC)
			} else {
				for i, c := range wantC {
					if cs, _ := c.(string); cs != candidates[i] {
						t.Errorf("candidates[%d]: got %s, want %s", i, candidates[i], cs)
					}
				}
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Suite 000 — reference grammar (docs/specs/000-reference-grammar.md)
// ----------------------------------------------------------------------------

func TestSuite000ReferenceVectors(t *testing.T) {
	for _, v := range loadJSONL(t, "000-reference.jsonl") {
		v := v
		id := vString(v, "id")
		t.Run(id, func(t *testing.T) { checkRefVector(t, v) })
	}
}

// TestHarnessRejectsFalseGreen pins review blocker 1 of PR #8: an
// expect.ok=false vector WITHOUT context whose input parses successfully
// must fail loudly — it may not silently fall through the resolution branch.
func TestHarnessRejectsFalseGreen(t *testing.T) {
	probe := map[string]any{
		"id":    "harness-probe",
		"input": "shardr:///ns/name:q8_0", // parses fine, no context
		"expect": map[string]any{
			"ok":         false,
			"errorClass": "E_NO_MEMBER",
		},
	}
	rec := &recordingTB{}
	checkRefVector(rec, probe)
	if !rec.failed {
		t.Fatal("harness passed a must-fail vector without context (false green)")
	}
}

// recordingTB records failures instead of exiting the goroutine, so the
// harness can be exercised with synthetic vectors in regression tests.
type recordingTB struct {
	testing.TB // satisfies the interface; only the methods below are used
	failed     bool
}

func (r *recordingTB) Helper()               {}
func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Error(...any)          { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatal(...any)          { r.failed = true }
func (r *recordingTB) Logf(string, ...any)   {}
func (r *recordingTB) Log(...any)            {}

func checkRefVector(t testing.TB, v map[string]any) {
	input := vString(v, "input")
	if vString(v, "form") == "cli-short" {
		input = cliShortToCanonical(input)
	}
	expect := expectBlock(t, v)

	p, rerr := parseRef(input)
	if rerr != nil {
		// Parse-phase failure.
		if vBool(expect, "ok") {
			t.Fatalf("unexpected parse error: %v", rerr)
		}
		assertErrorClass(t, expect, rerr.Class, rerr.Candidates)
		return
	}
	assertField(t, expect, "canonical", p.Canonical)
	assertField(t, expect, "ns", p.NS)
	assertField(t, expect, "name", p.Name)
	assertField(t, expect, "sel", p.Sel)
	assertField(t, expect, "quant", p.Quant)
	assertField(t, expect, "tag", p.Tag)
	assertField(t, expect, "digest", p.Digest)

	// Blocker 1: after a successful parse, a must-fail expectation has no
	// business being green. Without a context the only place the expected
	// error could still occur is resolution — so fail immediately.
	if !vBool(expect, "ok") && v["context"] == nil {
		t.Fatalf("expected error %s, but reference parsed successfully and no context is present to resolve against", vString(expect, "errorClass"))
	}
	if raw, ok := v["context"]; ok {
		var ctx resolveContext
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &ctx); err != nil {
			t.Fatal(err)
		}
		res, rerr := resolveRef(p, &ctx)
		if rerr != nil {
			if vBool(expect, "ok") {
				t.Fatalf("unexpected resolution error: %v", rerr)
			}
			assertErrorClass(t, expect, rerr.Class, rerr.Candidates)
			return
		}
		if !vBool(expect, "ok") {
			t.Fatalf("expected error %s, but reference parsed and resolved", vString(expect, "errorClass"))
		}
		wantRes, hasRes := expect["resolved"].(map[string]any)
		if hasRes {
			assertField(t, wantRes, "quant", res.Quant)
			assertField(t, wantRes, "manifest", res.Manifest)
		}
	}
}

// ----------------------------------------------------------------------------
// Suite 001 — canonicalization (docs/specs/001-artifact-and-manifest.md)
// ----------------------------------------------------------------------------

func TestSuite001CanonicalizationVectors(t *testing.T) {
	for _, v := range loadJSONL(t, "001-canonical.jsonl") {
		v := v
		id := vString(v, "id")
		t.Run(id, func(t *testing.T) {
			expect := expectBlock(t, v)

			var raw []byte
			if inline, ok := v["inline"]; ok {
				raw, _ = json.Marshal(inline)
			} else {
				b, err := os.ReadFile(vectorPath(t, vString(v, "input")))
				if err != nil {
					t.Fatal(err)
				}
				raw = b
			}

			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("input is not valid JSON: %v", err)
			}
			_, verr := validateArtifact(doc)
			if !vBool(expect, "ok") {
				if verr == nil {
					t.Fatalf("expected validation error %s, document validated", vString(expect, "errorClass"))
				}
				assertErrorClass(t, expect, verr.class, nil)
				return
			}
			if verr != nil {
				t.Fatalf("unexpected validation error: %v", verr)
			}

			canonical, err := jcsCanonical(raw)
			if err != nil {
				t.Fatalf("JCS canonicalization failed: %v", err)
			}
			wantBytes, err := os.ReadFile(vectorPath(t, vString(v, "expectedCanonical")))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, wantBytes) {
				t.Errorf("canonical bytes differ:\n got: %q\nwant: %q", canonical, wantBytes)
			}
			gotDigest := flatSHA256(canonical)
			if want := vString(v, "expectedDigest"); want != "" && gotDigest != want {
				t.Errorf("digest: got %s, want %s", gotDigest, want)
			}
			if not := vString(v, "notDigest"); not != "" && gotDigest == not {
				t.Errorf("digest unexpectedly equal to %s", not)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Suite 004 — torrent mapping (docs/specs/004-torrent-mapping.md)
// ----------------------------------------------------------------------------

func TestSuite004TorrentVectors(t *testing.T) {
	for _, v := range loadJSONL(t, "004-torrent.jsonl") {
		v := v
		id := vString(v, "id")
		t.Run(id, func(t *testing.T) {
			expect := expectBlock(t, v)
			switch vString(v, "kind") {
			case "piece-length":
				got := ladderPieceLength(int64(vFloat(v, "totalSize")))
				if want := int64(vFloat(expect, "pieceLength")); got != want {
					t.Errorf("pieceLength: got %d, want %d", got, want)
				}

			case "merkle-root":
				var r recipeFile
				b, _ := json.Marshal(v["file"])
				if err := json.Unmarshal(b, &r); err != nil {
					t.Fatal(err)
				}
				root := merkleRoot(recipeBytes(r))
				got := "sha256:" + fmt.Sprintf("%x", root)
				assertField(t, expect, "root", got)

			case "info-dict":
				res := runInfoDict(t, v)
				assertField(t, expect, "name", res.Name)
				assertField(t, expect, "infohash", res.Infohash)
				assertField(t, expect, "pieceLayersDigest", res.PieceLayersDigest)
				if want := vFloat(expect, "pieceLength"); want != 0 && float64(res.PieceLength) != want {
					t.Errorf("pieceLength: got %d, want %.0f", res.PieceLength, want)
				}
				if want := vFloat(expect, "totalSize"); want != 0 && float64(res.TotalSize) != want {
					t.Errorf("totalSize: got %d, want %.0f", res.TotalSize, want)
				}
				if gold := vString(expect, "infoDict"); gold != "" {
					compareGold(t, gold, res.infoBencode)
				}
				if gold := vString(expect, "pieceLayers"); gold != "" {
					compareGold(t, gold, res.pieceLayersBencode)
				}

			case "verify-record":
				manifestBytes, mDoc, resolved := loadManifestCtx(t, v)
				rec := recordFromVector(t, v)
				errClass := verifyRecord(manifestBytes, mDoc, resolved, rec)
				if vBool(expect, "ok") {
					if errClass != "" {
						t.Fatalf("unexpected verification error: %s", errClass)
					}
				} else {
					assertErrorClass(t, expect, errClass, nil)
				}

			case "verify-manifest":
				_, mDoc, resolved := loadManifestCtx(t, v)
				errClass := verifyManifest(mDoc, resolved)
				if vBool(expect, "ok") {
					if errClass != "" {
						t.Fatalf("unexpected verification error: %s", errClass)
					}
				} else {
					assertErrorClass(t, expect, errClass, nil)
				}

			default:
				t.Fatalf("unknown kind %q", vString(v, "kind"))
			}
		})
	}
}

// --- shared 004 helpers ------------------------------------------------------

type torrentRecord struct {
	ManifestDigest    string
	Infohash          string
	PieceLength       int64
	PieceLayersDigest string
}

type manifestCtx struct {
	manifestBytes []byte
	mDoc          *manifestDoc
	resolved      map[string][]byte
	digestHex     string
}

func loadManifestCtx(t *testing.T, v map[string]any) ([]byte, *manifestDoc, map[string][]byte) {
	t.Helper()
	manifestBytes, err := os.ReadFile(vectorPath(t, vString(v, "manifest")))
	if err != nil {
		t.Fatal(err)
	}
	var mDoc manifestDoc
	if err := json.Unmarshal(manifestBytes, &mDoc); err != nil {
		t.Fatal(err)
	}
	sidecar := map[string]recipeFile{}
	sb, err := os.ReadFile(vectorPath(t, vString(v, "files")))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sb, &sidecar); err != nil {
		t.Fatal(err)
	}
	resolved := map[string][]byte{}
	for name, r := range sidecar {
		resolved[name] = recipeBytes(r)
	}
	// Optional mutation of one file entry (negative verify-manifest vectors).
	if mut, ok := v["mutate"].(map[string]any); ok {
		if _, hasFile := mut["file"]; hasFile {
			applyManifestMutation(t, &mDoc, mut)
		}
	}
	return manifestBytes, &mDoc, resolved
}

func applyManifestMutation(t *testing.T, mDoc *manifestDoc, mut map[string]any) {
	t.Helper()
	target, _ := mut["file"].(string)
	for i := range mDoc.Files {
		if mDoc.Files[i].Name != target {
			continue
		}
		if s, ok := mut["bt.merkleRoot"].(string); ok {
			mDoc.Files[i].BT.MerkleRoot = s
		}
		if s, ok := mut["digest"].(string); ok {
			mDoc.Files[i].Digest = s
		}
		if f, ok := mut["size"].(float64); ok {
			mDoc.Files[i].Size = int64(f)
		}
		return
	}
	t.Fatalf("mutation target file %q not found", target)
}

func recordFromVector(t *testing.T, v map[string]any) torrentRecord {
	t.Helper()
	var rec torrentRecord
	if path := vString(v, "record"); path != "" && v["record"].(string) != "" {
		b, err := os.ReadFile(vectorPath(t, path))
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			ManifestDigest string `json:"manifestDigest"`
			Torrent        struct {
				Infohash          string `json:"infohash"`
				PieceLength       int64  `json:"pieceLength"`
				PieceLayersDigest string `json:"pieceLayersDigest"`
			} `json:"torrent"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		rec = torrentRecord{d.ManifestDigest, d.Torrent.Infohash, d.Torrent.PieceLength, d.Torrent.PieceLayersDigest}
	} else if inline, ok := v["record"].(map[string]any); ok {
		rec.ManifestDigest, _ = inline["manifestDigest"].(string)
		tor, _ := inline["torrent"].(map[string]any)
		if tor != nil {
			rec.Infohash, _ = tor["infohash"].(string)
			rec.PieceLength = int64(vFloat(tor, "pieceLength"))
			rec.PieceLayersDigest, _ = tor["pieceLayersDigest"].(string)
		} else { // tolerate a flat record
			rec.Infohash, _ = inline["infohash"].(string)
			rec.PieceLength = int64(vFloat(inline, "pieceLength"))
			rec.PieceLayersDigest, _ = inline["pieceLayersDigest"].(string)
		}
	} else {
		t.Fatal("verify-record vector needs record (file or inline)")
	}
	if mut, ok := v["mutate"].(map[string]any); ok {
		if s, ok := mut["record.manifestDigest"].(string); ok {
			rec.ManifestDigest = s
		}
	}
	return rec
}

type infoDictOut struct {
	infoBencode        []byte
	pieceLayersBencode []byte
	infoDictResult
}

func runInfoDict(t *testing.T, v map[string]any) infoDictOut {
	t.Helper()
	manifestBytes, mDoc, resolved := loadManifestCtx(t, v)
	digestHex := flatSHA256(manifestBytes)
	digestHex = digestHex[len("sha256:"):]
	benc, res, err := buildInfoDict(mDoc, manifestBytes, digestHex, func(name string) []byte { return resolved[name] })
	if err != nil {
		t.Fatal(err)
	}
	return infoDictOut{benc, res.PieceLayersBencode, res}
}

// verifyRecord implements the 001 §6 verification rule: reconstruct the info
// dict from the manifest, recompute infohash and piece-layers digest, and
// compare field-by-field against the distribution record.
func verifyRecord(manifestBytes []byte, mDoc *manifestDoc, resolved map[string][]byte, rec torrentRecord) string {
	digestHex := strings.TrimPrefix(flatSHA256(manifestBytes), "sha256:")
	if rec.ManifestDigest != "sha256:"+digestHex {
		return "E_MANIFEST_DIGEST_MISMATCH"
	}
	_, res, err := buildInfoDict(mDoc, manifestBytes, digestHex, func(name string) []byte { return resolved[name] })
	if err != nil {
		return "E_INFOHASH_MISMATCH" // unresolvable recipes make the torrent unbuildable
	}
	if rec.Infohash != res.Infohash {
		return "E_INFOHASH_MISMATCH"
	}
	if rec.PieceLength != res.PieceLength {
		return "E_PIECE_LENGTH_MISMATCH"
	}
	if rec.PieceLayersDigest != res.PieceLayersDigest {
		return "E_PIECE_LAYERS_DIGEST_MISMATCH"
	}
	return ""
}

// verifyManifest recomputes every entry's size, flat digest and merkle root
// from the deterministic file recipes (001 §7.1 digest correctness).
func verifyManifest(mDoc *manifestDoc, resolved map[string][]byte) string {
	for _, f := range mDoc.Files {
		data, ok := resolved[f.Name]
		if !ok {
			return "E_SIZE_MISMATCH"
		}
		if int64(len(data)) != f.Size {
			return "E_SIZE_MISMATCH"
		}
		if got := flatSHA256(data); got != f.Digest {
			return "E_DIGEST_MISMATCH"
		}
		root := merkleRoot(data)
		if got := "sha256:" + fmt.Sprintf("%x", root); got != f.BT.MerkleRoot {
			return "E_MERKLE_ROOT_MISMATCH"
		}
	}
	return ""
}

func compareGold(t *testing.T, rel string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(vectorPath(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: bytes differ:\n got: %q\nwant: %q", rel, got, want)
	}
}

// TestSuiteHygiene pins two structural requirements from issue #3: unique
// vector ids and at least one must-fail vector per suite.
func TestSuiteHygiene(t *testing.T) {
	for _, suite := range []string{"000-reference.jsonl", "001-canonical.jsonl", "004-torrent.jsonl"} {
		var negatives int
		seen := map[string]bool{}
		for _, v := range loadJSONL(t, suite) {
			id := vString(v, "id")
			if seen[id] {
				t.Errorf("%s: duplicate vector id %s", suite, id)
			}
			seen[id] = true
			if !vBool(expectBlock(t, v), "ok") {
				negatives++
			}
		}
		if negatives == 0 {
			t.Errorf("%s: no negative (must-fail) vector", suite)
		}
		t.Logf("%s: %d vectors, %d negative", suite, len(seen), negatives)
	}
}
