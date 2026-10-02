# Runtime & releases

The runbook for the llama.cpp runtime and the runner releases. The
versioning policy is stated in the repository README
("[llama.cpp versioning & runner releases]"); this page describes the
implemented machinery: `runtime/llama.lock` as the single version
truth, the bot update flow, the canary channel, rollback, and what a
release bundle contains — citing the real release
`shardr-runner-5755101-llama-b10684` as evidence.

[llama.cpp versioning & runner releases]: https://github.com/Cyb3rDudu/shardr#llamacpp-versioning--runner-releases

## The lockfile is the only version truth

`runtime/llama.lock` pins, per platform (`darwin_arm64`,
`linux_amd64`), the upstream **prebuilt** llama.cpp b-release: ref,
upstream commit, asset URL, and asset SHA-256. Project decision
2026-09-05: shardr never compiles llama.cpp — the runtime is consumed
as upstream prebuilt release binaries, digest-pinned. There is no
second pin anywhere; the parser is fail-closed
(`internal/llamalock`, CLI `cmd/llama-lock`).

Local mechanics (executed):

```console
$ make all
>> building shardr + shardhive into bin
>> fetching prebuilt llama.cpp b10684 (darwin_arm64)
fetched darwin_arm64 -> bin/llama-b10684 (sha256 8310138372444cbe…)
ln -sf llama-b10684/llama-server bin/llama-server
>> bin/llama-server ready (pin: b10684)
```

The whole extract dir is kept (`bin/llama-b10684/`): llama-server
loads its dylibs via `@loader_path` — a lone binary is useless.
`ResolveBinary` finds `llama-server` next to the `shardr` executable.

The served runtime identifies itself in every chat completion
(executed): `"system_fingerprint": "b10684-cc83d7b48"` — llama ref
plus upstream commit short SHA, matching the lock.

## Daily update flow (bot: workflow A → E2E gate → merge)

`.github/workflows/llama-upstream-check.yml` runs daily (cron 17:03
UTC) and never merges, never releases:

1. **Verify the current pin** — if upstream re-uploaded assets under
   the same bNNNN (digests no longer match), the run goes red *before*
   any update decision.
2. **`llama-lock check-update --write`** — finds the newest b-release
   that is **≥ 7 days old** (community soak filter) with a complete
   prebuilt asset matrix; rewrites `runtime/llama.lock`. `noop` ends
   the run.
3. **Bot PR** — branch `bot/llama-<ref>`, commit
   `chore(llama): pin <ref> (digest-pinned prebuilt runtime)`, PR body
   with old/new pins, upstream compare link, and digest provenance.
4. **E2E gate dispatched on the bot branch** — the same
   `llama build + E2E` reusable workflow the release pipeline uses
   (workflow B): `llama-lock validate`/`verify`, digest-verified fetch,
   real runner E2E with a pinned test model
   (`unsloth/SmolLM2-135M-Instruct-GGUF` at a pinned revision +
   SHA-256), on `ubuntu-latest` and `macos-latest` (native Apple
   Silicon — a real Metal runtime test).
5. **Human merge-go** — the gate result is visible as a commit status
   on the bot commit and linked in the PR; a human merges.

## Release (workflow C, merged main only)

`release-runner.yml` triggers on pushes to `main` that touch the
lockfile or anything the bundle contains. It re-runs the same E2E
matrix on `main`, then packages per platform
(`scripts/make-runner-bundle.sh`, reproducible: deterministic tar —
sorted names, fixed mtime/uid/gid, gzip mtime 0 — `built_at` from the
source commit epoch). Re-running on the same commit hits an
idempotent noop; same release identity with *different* bytes is a
hard failure.

Real evidence — the release published from `main @ 5755101`
(`gh release view shardr-runner-5755101-llama-b10684`, 2026-10-02):

| Asset | Size |
| --- | --- |
| `shardr-runner_5755101_darwin_arm64.tar.gz` | 24 469 967 B |
| `shardr-runner_5755101_linux_amd64.tar.gz` | 29 815 127 B |
| `SHA256SUMS` | 215 B |

Tag scheme: `shardr-runner-<source-commit>-llama-<b-ref>` — the
runner binary and the runtime pin are versioned together; a tag names
exactly one content state of both.

## Canary channel (workflow D)

`llama-nightly-canary.yml` runs weekly (Mondays 04:37 UTC) and tests
the **newest** bNNNN through the same digest-verified fetch + E2E
matrix — with **no age filter** (canary exists to catch breakage
before the 7-day soak ends). It never touches `runtime/llama.lock`
and never creates a stable release.

## Rollback

The pin only moves forward: `check-update` never proposes an older
bNNNN. Rolling back is an explicit act —
`llama-lock check-update --ref <older-bNNNN>` plus
`--allow-downgrade` (the flag exists precisely to make a downgrade a
deliberate decision, not an accident). The change lands as a normal
lockfile PR through the same gate; history in `runtime/llama.lock` is
one commit per pin move.

## Bundle contents

`shardr-runner_<commit>_<platform>.tar.gz` unpacks to
`shardr-runner/`:

```
shardr-runner/
  shardr                    # the runner CLI
  llama/                    # the WHOLE upstream extract dir
                            # (llama-server + @loader_path dylibs)
  BUILDINFO.json            # shardr_commit, llama_ref, llama_commit,
                            # llama_asset_sha256, llama_server_version,
                            # runtime_source, platform, built_at
  LICENSES/
    llama.cpp               # upstream license (from the prebuilt archive)
    go/<module>/…           # Go dependency licenses from the module cache
```

`BUILDINFO.json` is the provenance record: source commit, llama ref +
commit + asset digest, and the runtime policy string
(`upstream prebuilt release binaries (never self-built)`).
