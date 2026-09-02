package importer

import (
	"archive/tar"
	"bytes"
	"sort"
	"time"
)

// buildDeterministicTar packs the named contents into a tar with sorted
// entry names, mtime 0, uid/gid 0, mode 0644 — byte-identical for the
// same input set in any order (001 §3.1 tokenizer/adapter tars, §8.3).
func buildDeterministicTar(contents map[string][]byte) []byte {
	names := make([]string, 0, len(contents))
	for n := range contents {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		hdr := &tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(contents[n])),
			ModTime: timeZero(), Typeflag: tar.TypeReg,
			Format: tar.FormatPAX,
			Uid:    0, Gid: 0,
		}
		tw.WriteHeader(hdr)
		tw.Write(contents[n])
	}
	tw.Close()
	return buf.Bytes()
}

func timeZero() time.Time { return time.Unix(0, 0).UTC() }
