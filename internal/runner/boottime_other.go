//go:build !darwin

package runner

// darwinBootTime is unreachable on non-darwin platforms (the token
// chain reads /proc/<pid>/stat there); the shape keeps registry.go
// platform-neutral.
func darwinBootTime() string { return "?" }
