package swarm

// v1 torrent construction for tests: hand-built info dicts with real
// SHA-1 piece hashes over the concatenated file stream — deterministic,
// no network, no third-party tooling.

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// v1Fixture is a crafted foreign torrent: file contents in stream order
// plus its metainfo.
type v1Fixture struct {
	Name     string
	Files    []v1FixtureFile // stream order
	MetaInfo *metainfo.MetaInfo
	Infohash string // 40 hex
	Magnet   func(ws string, peers []string) string
}

type v1FixtureFile struct {
	Path    string
	Content []byte
}

// buildV1 builds a pure-v1 metainfo from ordered files and a piece
// length, with piece hashes over the concatenated stream.
func buildV1(name string, files []v1FixtureFile, pieceLength int64) *v1Fixture {
	var stream []byte
	var fis []map[string]any
	for _, f := range files {
		stream = append(stream, f.Content...)
		fis = append(fis, map[string]any{"length": int64(len(f.Content)), "path": strings.Split(f.Path, "/")})
	}
	var pieces []byte
	for off := int64(0); off < int64(len(stream)); off += pieceLength {
		end := off + pieceLength
		if end > int64(len(stream)) {
			end = int64(len(stream))
		}
		h := sha1.Sum(stream[off:end])
		pieces = append(pieces, h[:]...)
	}
	info := map[string]any{
		"name":         name,
		"piece length": pieceLength,
		"files":        fis,
		"pieces":       pieces,
	}
	mi := &metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	fx := &v1Fixture{Name: name, Files: files, MetaInfo: mi}
	ih := mi.HashInfoBytes()
	fx.Infohash = fmt.Sprintf("%x", ih)
	fx.Magnet = func(ws string, peers []string) string {
		q := url.Values{}
		q.Set("xt", "urn:btih:"+fx.Infohash)
		if ws != "" {
			q.Set("ws", ws)
		}
		for _, p := range peers {
			q.Add("x.pe", p)
		}
		return "magnet:?" + q.Encode()
	}
	return fx
}

// fixturePaths returns the file paths in stream order.
func (fx *v1Fixture) fixturePaths() []string {
	out := make([]string, len(fx.Files))
	for i, f := range fx.Files {
		out[i] = f.Path
	}
	return out
}

// sha256Hex of b.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// gitBlob of b (sha1("blob <n>\0"+b)).
func gitBlob(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// anchorOf builds the strict anchor map for the fixture (every file,
// sha256 + git blob id, sizes).
func (fx *v1Fixture) anchorOf() map[string]ForeignFile {
	m := map[string]ForeignFile{}
	for _, f := range fx.Files {
		m[f.Path] = ForeignFile{Path: f.Path, Size: int64(len(f.Content)), SHA256: sha256Hex(f.Content), GitSHA1: gitBlob(f.Content)}
	}
	return m
}

// sortedKeys of a map (test determinism).
func sortedKeys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
