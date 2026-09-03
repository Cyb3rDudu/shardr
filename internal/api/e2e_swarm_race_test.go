//go:build race

package api

import "testing"

func TestEnsureFillOverAPI(t *testing.T) {
	t.Skip("swarm E2E excluded under -race: upstream data race in anacrolix hash-exchange (see PR #20); runs in the non-race suite")
}
