package runner

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// The flock makes the read-modify-write transactional across processes —
// and across goroutines. Deterministic proof: a two-party barrier at
// pre-Lock forces BOTH Adds past the gate before either writes. With the
// lock, they serialize and both land; with a broken/no-op lock, both
// read the same baseline and the second write drops the first entry.
func TestRegistryConcurrentAddDeterministic(t *testing.T) {
	reg := OpenRegistryAt(filepath.Join(t.TempDir(), "instances.json"))
	const n = 2
	var barrier sync.WaitGroup
	barrier.Add(n)
	preLockHook = func() {
		barrier.Done()
		barrier.Wait() // both inside the pre-lock zone simultaneously
	}
	defer func() { preLockHook = nil }()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = reg.Add(Instance{ID: fmt.Sprintf("r%d", i), Endpoint: "http://127.0.0.1:1", PID: i + 1})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	list, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("both concurrent registrations must survive (flock broken?), got %+v", list)
	}
}
