//go:build !linux && !darwin

package cli

import "fmt"

// removeSplitDir on unsupported platforms refuses loudly — the old
// path-based RemoveAll on registry data does not come back.
func removeSplitDir(stored string) error {
	return fmt.Errorf("E_STATE: split-dir removal unsupported on this platform — manual removal required: %s", stored)
}
