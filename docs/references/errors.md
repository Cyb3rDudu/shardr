# Error inventory

Every `E_*` error class emitted by the codebase, derived from
`grep -rn '"E_' --include='*.go' internal/ cmd/` on the current tree,
with source, typical cause, and remedy. Wire errors are always the
envelope `{"error":{"code","message","candidates"?}}` (see
[HTTP API v1](api.md)); CLI errors print as `shardr: E_CODE: message`.

## Reference grammar — `internal/ref/ref.go`

| Code | Meaning | Typical cause | Remedy |
| --- | --- | --- | --- |
| `E_LENGTH` | reference exceeds 512 bytes | pasted digest list, wrong paste | shorten; one ref per operation |
| `E_PARSE` | malformed reference | missing `shardr:///`, bad ns/name shape, bare tag as selector, short form at the API | the message names the exact defect; at the API use the canonical URI (the message carries it) |
| `E_NO_SELECTOR` | reference without `:sel` and without `@digest` | `shardr:///ns/name` — there is no default selector in the scheme | add a quant (`:q8_0`), `tag+quant`, or pin with `@sha256:…`; interactively, set `[references] default_selector` |
| `E_DIGEST_FORMAT` | digest is not `sha256:` + 64 lowercase hex | truncated/uppercase digest | use the canonical digest form |
| `E_TAG_BANNED` | tag starts `sha256-`/`sha256:` or is quant-shaped | trying to smuggle content addressing through a tag | quant-shaped strings select index members; use `@digest` to pin |
| `E_AMBIGUOUS_SELECTOR` | quant prefix matches several members | `:q4` with `q4_0` and `q4_1` present | `candidates` lists them; spell the full quant |
| `E_NO_MEMBER` | no index member matches the selector | quant not imported (e.g. `:q8_0` on a q4-only repo) | check `shardr models` / `/v1/models` for present members |
| `E_PIN_MISMATCH` | `@digest` disagrees with the selected member | index moved after pinning | drop the pin or update it to the current manifest digest |
| `E_UNKNOWN_TAG` | defined; not currently emitted — unknown tags answer `E_UNKNOWN_REF` | — | — |

## API wire classes — `internal/api/server.go`

| Code | Meaning | Typical cause | Remedy |
| --- | --- | --- | --- |
| `E_BAD_REQUEST` | malformed request (missing field, bad JSON, invalid digest param) | client bug; missing `ref`/`as`/`manifestDigest` | the message names the missing field |
| `E_INVALID_REF` | defined; reserved — reference parse errors carry the grammar classes above | — | — |
| `E_UNKNOWN_REF` | no local index (resolver fetch not implemented in this build); unknown tag (tag-scoped message); catalog repo not listed | typo; not imported yet; tag from another repo | `candidates` lists same-namespace repos / existing tags; import first |
| `E_NO_INDEX` | namespace state points at an index blob absent from the CAS | deleted blob, interrupted migration — corruption, never silently rebuilt | re-import the repo; investigate store integrity (`shardhive cas verify --all`) |
| `E_INVALID_INDEX` | index blob fails validation | corrupt or foreign index blob | same as `E_NO_INDEX` — explicit repair, no silent rebuild |
| `E_SOURCE_UNAVAILABLE` | source disabled, not implemented, or unreachable | swarm disabled, HF/catalog down, manifest not local, listing torrent without seeders | the message names the knob or the unreachable source; for swarm fills set `[swarm] enabled = true` |
| `E_NOT_IMPLEMENTED` | endpoint reserved for a later slice | `/import/bt` on a swarm-disabled daemon | enable the swarm or wait for the slice named in the message |
| `E_NOT_FOUND` | no such blob / job / endpoint | wrong id, wrong digest, unknown path | — |
| `E_UNSUPPORTED_VERSION` | request aimed at another `/vN` | client/daemon version skew | `candidates` carries the supported version (`v1`) |
| `E_RANGE_INVALID` | range does not overlap the blob (HTTP 416) | `Range: bytes=<past-end>-` | request a range within `size` from `/v1/open` |
| `E_INTERNAL` | daemon bug — never a user-input verdict | actual defects; also the metadata-timeout class on catalog pulls | report it; include the message |
| `E_CORRUPTION` | CAS corruption: a present blob is not what it should be | bit rot, manual tampering | `shardhive cas verify` to enumerate; re-import affected blobs |
| `E_NOT_IMPORTABLE` | import fails the 001 §8 rule set or post-import verification; magnet/manifest binding failure | unimportable content, pin that never satisfies, fetched bytes fail verify-write | terminal for the job — fix the source/pin, never a retry |
| `E_NOT_ANCHORED` | catalog model is rescued (HF source gone) and `trustCatalog` not set (HTTP 422) | provider listing without a live HF anchor | accept the trust shift explicitly (`--trust-catalog` / `trustCatalog: true`) or skip the model |
| `E_VERIFY_FAILED` | verify job found mismatch/missing blobs | corruption, deleted blobs | job carries `verify.mismatched`/`missing`/`stateErrors`; re-fetch affected content |
| `E_SOURCE_NOT_REGULAR` | local import source is not a regular file | symlink, FIFO, device in `paths` | hard boundary — import real files only |
| `E_RATE_LIMITED` | Hugging Face rate limit | too many requests | retry later; set `SHARDR_HF_TOKEN` (authenticated quota is higher) |
| `E_SOURCE_FORBIDDEN` | HF access denied | gated repo without token, or nonexistent repo when anonymous | set `SHARDR_HF_TOKEN` in the daemon's environment; check the repo id |

## Artifact validation — `internal/artifact/validate.go`

Structural 001 rule violations. They surface as the `message` of
`E_INVALID_INDEX` (index validation) or `E_NOT_IMPORTABLE` (manifest
validation in an import job) — the class names the exact broken rule:

| Code | Rule violated |
| --- | --- |
| `E_VALIDATION` | generic 001 document rule (bad JSON shape, wrong `schemaVersion`, missing `artifactType`, unknown file kind, config-name/cardinality rules, path collisions) |
| `E_VALIDATION_KIND` | unknown `artifactType` |
| `E_VALIDATION_RESERVED_PATH` | `manifest/` path prefix is reserved for the embedded manifest document (001 §3.1 rule 1, ruling R2) |
| `E_VALIDATION_WEIGHTS_MIX` | more than one weights format in an artifact (`weights.gguf` + `weights.safetensors`) |
| `E_VALIDATION_WEIGHTS_MISSING` | artifact requires at least one weights entry (001 §3.1) |
| `E_VALIDATION_CARDINALITY` | tokenizer/chat-template entries exceed their 0..1 cardinality |
| `E_VALIDATION_PARTS` | split-GGUF parts not contiguous `1..n` (duplicates/gaps caught element-wise) |
| `E_VALIDATION_RUNTIME_DUP` | more than one runtime-config entry for the same runtime id (001 §3.1: ≤ 1) |
| `E_VALIDATION_FILE_ORDER` | manifest files violate the canonical file order |
| `E_VALIDATION_QUANT_DUP` | duplicate quant among index members |
| `E_VALIDATION_INFOHASH` | distribution-record infohash rule failure |

All are terminal verdicts on the content — the content does not meet
the format; no retry changes that.

## CLI — `internal/cli/` (client side)

| Code | Meaning | Typical cause | Remedy |
| --- | --- | --- | --- |
| `E_DAEMON_UNREACHABLE` | cannot connect to the daemon socket | shardhive not running, wrong `SHARDR_SOCKET` | start `shardhive serve`; check socket path |
| `E_BAD_RESPONSE` | HTTP status without an error envelope | non-shardhive listener on the socket | check what owns the socket path |
| `E_CANCELLED` | job wait aborted | Ctrl-C during a poll | job continues server-side; re-check with `shardr status` |
| `E_STATE` | client-side state problems: socket resolution, serve-registry/split-scratch issues, pid identity verification failures | stale registry entries, pid reuse, leftover scratch dirs | the message names the file to inspect and clean manually |
| `E_CONFIG` | CLI-side config problems (`--config` read/parse, advisory runtime-config entries) | malformed TOML/JSON overlay file | fix the named file/key |
| `E_RUNTIME` | runner subprocess/scratch problems | split-part linking failures, scratch cleanup | the message names the path; usually disk/permission state |
| `E_BINARY` | llama-server binary not found (`internal/runner/llama.go`) — the first-run failure when the pinned runtime was never fetched; also fires for a bad `$SHARDR_LLAMA_SERVER` override | fresh checkout without `make llama`; typo in the override | the message names all three remedies: `make llama`, `$SHARDR_LLAMA_SERVER`, or llama-server on `$PATH` |

## Runtime tooling — `internal/llamalock`, `cmd/llama-lock`

Surfaced by `make llama` and the lockfile workflows, not the daemon
API:

| Code | Meaning | Typical cause | Remedy |
| --- | --- | --- | --- |
| `E_DIGEST` | fetched runtime asset does not hash to the pinned SHA-256 (`llamalock.go`) | upstream re-uploaded assets under the same bNNNN, corrupted download | re-fetch; if it persists, upstream moved — the pin must be re-derived (never force past it) |
| `E_PROVENANCE` | release-API digest or tag commit disagrees with the lock (`cmd/llama-lock`) | tag moved upstream, release re-packed | the lockfile PR flow re-pins deliberately; never auto-accept |

Daemon-side config failures (unknown `[swarm]`/`[catalog]` key, wrong
value type) are loud startup errors on stderr, not `E_` classes
(executed):

```console
$ shardhive serve
shardhive: config ~/.config/shardr/config.toml: unknown [swarm] key "bogus" (known: enabled, seed, upload_limit, dht, no_seed_verify, webseed_addr)
```

For symptom→cause triage see [Troubleshooting](../operations/troubleshooting.md).
