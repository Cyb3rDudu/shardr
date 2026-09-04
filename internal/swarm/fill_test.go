package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/cas"
)

// Engine-free tests for the fill-adjacent loaders (run under -race too —
// no anacrolix client is constructed).

// newEnginelessClient builds a Client with only the CAS wired (same
// package — field access, no torrent engine).
func newEnginelessClient(t *testing.T) *Client {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Client{store: store}
}

// RecordForManifest validates the record STRUCTURE, not just the digest
// form: a well-formed-JSON, digest-consistent but structurally invalid
// record (wrong artifactType) is a loud error, never silently consumed.
func TestRecordForManifestRejectsInvalidRecord(t *testing.T) {
	c := newEnginelessClient(t)
	// Digest-consistent JSON with a structurally invalid payload.
	recBytes := []byte(`{"schemaVersion":1,"artifactType":"not-distribution","manifestDigest":"sha256:` + strings.Repeat("ab", 32) + `","torrent":{"infohash":"btmh:1220` + strings.Repeat("cd", 32) + `","pieceLength":1048576,"pieceLayersDigest":"sha256:` + strings.Repeat("ef", 32) + `"}}`)
	sum := sha256.Sum256(recBytes)
	recHex := hex.EncodeToString(sum[:])
	if err := c.store.Put(recHex, strings.NewReader(string(recBytes))); err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetDistributionLink("sha256:"+strings.Repeat("ab", 32), "sha256:"+recHex); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.RecordForManifest("sha256:" + strings.Repeat("ab", 32)); err == nil {
		t.Fatal("structurally invalid record must be rejected loudly")
	} else if !strings.Contains(err.Error(), "fails validation") {
		t.Fatalf("error must name the validation failure: %v", err)
	}
}

// The no-hints error states the v1 acquisition contract (004 §5 v1
// bootstrap): webseed-only, manifest-digest key — not the obsolete
// sidecar/metainfo path list.
func TestLayersUnavailableNamesV1WebseedContract(t *testing.T) {
	_, err := (&Client{}).fetchLayersFromHints(context.Background(), strings.Repeat("ab", 32), Hints{})
	if !errors.Is(err, ErrLayersUnavailable) {
		t.Fatalf("must be ErrLayersUnavailable: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"webseed", "004 §5", "manifest digest hex"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message must name %q: %s", want, msg)
		}
	}
	for _, stale := range []string{"resolver envelope sidecar", ".torrent metainfo; pass"} {
		if strings.Contains(msg, stale) {
			t.Fatalf("message must not advertise later-version paths as v1 options: %s", msg)
		}
	}
}
