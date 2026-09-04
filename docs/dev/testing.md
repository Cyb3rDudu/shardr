# Testing

## The three modes (what CI runs)

CI (both ubuntu and macOS runners) executes, in order:

```sh
go build ./...
go vet ./...
go test ./...                 # 1. full suite, plain
go test -race -short ./...    # 2. race detector + -short
```

Locally while developing, the same three plus format checking:

```sh
gofmt -l .                    # must print nothing
go test ./...
go test -race ./internal/api/ ./internal/importer/   # targeted race pass
```

Why the modes split:

- **Plain `go test ./...`** runs everything, including the slow E2E
  swarm tests (two-instance torrent round trips on localhost).
- **`-race -short`** runs the whole tree under the race detector with
  the long E2E tests skipped — race coverage for every package's
  concurrency (CAS handles, job publication, swarm listeners) without
  paying the full E2E runtime twice.
- **`go vet` / `gofmt`** are the hygiene gate; both must be clean.

## Spec vectors (`internal/specvectors`)

The JSONL suites in `docs/specs/vectors/` (reference grammar 000,
canonical artifact 001, torrent mapping 004) run **against the
production packages** — `internal/ref`, `internal/artifact` — not
against test-local re-implementations. Parser/validator and vectors
cannot drift apart: a rule change fails the vectors until the vectors
say it should pass. Golden files in `internal/importer/testdata`
(gold_test.go) pin import convergence the same way: identical bytes in,
identical digests out, across the classifier and artifact construction.

## Mutation discipline

A test proves a fix only if it fails without the fix. The established
practice for contract-critical tests:

1. write the test against the fixed code (green),
2. revert the fix (or re-introduce the old behavior) in a scratch copy,
3. watch the test go red,
4. restore, watch it go green again.

Mutations are made **only against committed states** — never against
uncommitted working trees — and the red/green evidence is recorded in
the PR. If you cannot make a test fail by breaking the behavior it
claims to pin, the test is decoration; fix it or delete the claim.

## Test style

- Tests live beside the code (`*_test.go`, same package) — they read
  internal state; that is the point.
- E2E tests spin real daemons/clients on ephemeral ports and temp
  directories; nothing touches a developer's real `$SHARDR_CAS`.
- CAS and importer tests use `t.TempDir()` stores; the CAS layout is
  exercised through the public store API, not by poking files.
- Error-contract tests assert the `E_*` class (and often `candidates`),
  not full message text — messages carry reasons, classes carry
  contracts.
