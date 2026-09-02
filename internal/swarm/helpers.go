package swarm

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/bencode"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"
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

var _ = fmt.Sprintf
