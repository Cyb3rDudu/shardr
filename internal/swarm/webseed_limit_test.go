package swarm

import (
	"context"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// The limitWriter routes body writes through the shared budget: writes
// beyond the burst block until tokens arrive, and a budget that cannot
// be satisfied within the request context is refused (x/time/rate
// semantics — it fails fast once the deadline can no longer be met).
func TestWebseedBodyIsThrottled(t *testing.T) {
	// Positive: 1000 tokens/s, burst 1 → 10 one-byte writes need ≥9ms.
	lim := rate.NewLimiter(1000, 1)
	lw := &limitWriter{w: discardWriter{}, lim: lim, ctx: context.Background()}
	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := lw.Write([]byte{1}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 9*time.Millisecond {
		t.Fatalf("10 writes at 1000/s burst 1 must take ≥9ms, took %v (no throttling)", elapsed)
	}
	// Refusal: a 1-byte budget with a 300ms deadline cannot serve a
	// second byte within the deadline.
	lim2 := rate.NewLimiter(1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	lw2 := &limitWriter{w: discardWriter{}, lim: lim2, ctx: ctx}
	if _, err := lw2.Write([]byte{1}); err != nil {
		t.Fatalf("burst write: %v", err)
	}
	if _, err := lw2.Write([]byte{2}); err == nil {
		t.Fatal("second write must not pass within the deadline")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
