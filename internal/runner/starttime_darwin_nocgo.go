//go:build darwin && !cgo

package runner

// darwinProcStart without cgo: libproc's proc_pidinfo is unavailable —
// the token chain refuses to trust a degraded identity source.
func darwinProcStart(pid int) string { return "" }
