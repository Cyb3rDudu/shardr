//go:build !darwin

package runner

// darwinProcStart is unreachable off darwin (the token chain reads
// /proc/<pid>/stat there); the shape keeps registry.go
// platform-neutral.
func darwinProcStart(pid int) string { return "" }
