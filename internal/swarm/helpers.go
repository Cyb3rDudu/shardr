package swarm

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
)

// Small shared helpers (kept out of the flow files for readability).

type infohashOf struct{ v infohash_v2.T }

func (h *infohashOf) fromHex(s string) error {
	_, err := hex.Decode(h.v[:], []byte(s))
	return err
}

func (h *infohashOf) opt() g.Option[infohash_v2.T] {
	var o g.Option[infohash_v2.T]
	o.Set(h.v)
	return o
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

func unmarshalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

// bencodeLayers re-encodes a layers map deterministically (sorted keys,
// same codec the artifact package used to build it).
func bencodeLayers(layers map[string]string) ([]byte, error) {
	return bencode.Marshal(layers)
}

// bencodeLayersInto decodes a layers blob into an existing map.
func bencodeLayersInto(blob []byte, into map[string]string) error {
	return bencode.Unmarshal(blob, &into)
}

// specWithInfohash builds a TorrentSpec carrying explicit infohash bytes.
func specWithInfohash(btmh string, infoBencode []byte, layers map[string]string) (*torrent.TorrentSpec, error) {
	var ih [32]byte
	raw := strings.TrimPrefix(btmh, "btmh:1220")
	if _, err := hex.Decode(ih[:], []byte(raw)); err != nil {
		return nil, err
	}
	var o g.Option[infohash_v2.T]
	o.Set(ih)
	return &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{InfoHashV2: o, InfoBytes: infoBencode},
		PieceLayers:    layers,
	}, nil
}

// scratchPutEvil stores variant content under its TRUE digests (content
// addressing is digest-honest even for adversarial torrents — the CAS is
// a blob store, not a truth authority; the pin is).
func (c *Client) scratchPutEvil(afs []artifact.File, files map[string][]byte, mb []byte) error {
	for _, f := range afs {
		if err := c.store.Put(artifact.DigestHex(f.Digest), strings.NewReader(string(files[f.Name]))); err != nil {
			return err
		}
	}
	return nil
}

var _ = fmt.Sprintf
