// Package specvectors holds the machine-checkable spec test-vector harness
// for docs/specs 000, 001 and 004 (see docs/specs/vectors/README.md for the
// vector format). Everything in this package is test-only: the reference
// implementations live in _test.go files so the vectors stay decoupled from
// any future production code. Regenerate goldfiles with:
//
//	go test ./internal/specvectors -update
package specvectors
