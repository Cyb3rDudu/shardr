# HTTP API v1

The shardhive daemon serves HTTP/1.1 over a Unix domain socket (mode
0600 — the socket permission is the access boundary; there is no TCP
listener). JSON in, JSON out. The canonical contract is
[spec 005](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/005-shardhive-interface.md);
this page documents the implemented surface of `internal/api/server.go`,
with every example executed for real against a running daemon.

All examples assume the socket in a variable (see
[Daemon operation](../operations/daemon.md) for path resolution):

```sh
S=${SHARDR_SOCKET:-${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}/shardhive-$(id -u)}/shardhive.sock}
curl -s --unix-socket "$S" http://localhost/v1/models
```

## Versioning

The `/v1` path prefix is the client's version declaration — there is no
negotiation header. A request aimed at another major version is
rejected loudly (executed):

```sh
curl -s --unix-socket "$S" http://localhost/v2/resolve
# {"error":{"code":"E_UNSUPPORTED_VERSION",
#           "message":"unsupported API major version /v2/ (/v2/resolve); this shardhive supports v1",
#           "candidates":["v1"]}}                                # HTTP 400
```

Unknown paths under `/v1/` are a plain 404 (`E_NOT_FOUND`).

## Error envelope

Every error — including HTTP-level failures such as an unsatisfiable
range — is one JSON shape:

```json
{"error": {"code": "E_UNKNOWN_REF", "message": "…", "candidates": ["…"]}}
```

`candidates` is optional and lists known alternatives (existing
same-namespace repos, matching index members) so callers can
self-correct. The full class inventory with sources is in the
[error inventory](errors.md).

## References at the API

The API accepts **only the canonical URI form** (spec 000 §2):
`shardr:///ns/name:quant`, `shardr:///ns/name:tag+quant`, or
`shardr:///ns/name@sha256:<hex>`. A parseable CLI short form is
rejected with the canonical spelling in the message (executed):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=gold/smollm2-135m-instruct:q4_k_m'
# {"error":{"code":"E_PARSE",
#           "message":"references at the API must be canonical URIs; use shardr:///gold/smollm2-135m-instruct:q4_k_m"}}   # 400
```

URL-encoding note (executed): in a query string `+` decodes to a
space, so the tag separator in `tag+quant` must be sent as `%2B`.
Unencoded, the daemon receives `snap raw` and answers
`E_PARSE: selector must be a quant or tag+quant; bare tags are not
selectors`. Encoded correctly:

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///minicpmv/mmproj:snap%2Braw'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"no tag snap for minicpmv/mmproj; tags are scoped per repository (000 §3.4)"}}   # 404
```

## GET /v1/resolve

Pure name → digest lookup against **local state only**; never touches
the network. Query parameter `ref` (canonical reference, required).

Executed (200):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/smollm2-135m-instruct:q4_k_m'
# {"ref":"shardr:///gold/smollm2-135m-instruct:q4_k_m","ns":"gold","name":"smollm2-135m-instruct",
#  "quant":"q4_k_m",
#  "manifestDigest":"sha256:696ff8cb8d6695de89ce9a76bd07eb82941e1fea48612d3446801a1f48039825",
#  "indexDigest":"sha256:1631afafd611129f9caf6a4b33aaf7cb843e37da35ffff4e2ff4d2e765ab300f",
#  "plan":"pending",
#  "planReason":"manifest document parsing lands with imports (001 §8)"}
```

`plan` is always `"pending"` in this build — a stale constant from an
earlier slice (known legacy defect, see the code comment on
`resolveResult`); treat it as unimplemented.

The `@sha256:<hex>` form addresses a manifest directly, without index
lookup, and returns no `indexDigest` (executed):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///minicpmv/mmproj@sha256:3db690a6d2d18d85eff7206bcf65911321bf582a58b4194d33d0b78e5a5a8171'
# {"ref":"shardr:///minicpmv/mmproj@sha256:3db690a6…","ns":"minicpmv","name":"mmproj",
#  "manifestDigest":"sha256:3db690a6d2d18d85eff7206bcf65911321bf582a58b4194d33d0b78e5a5a8171",
#  "plan":"pending","planReason":"manifest document parsing lands with imports (001 §8)"}   # 200
```

Errors (all executed):

- `E_BAD_REQUEST` — missing `ref` query parameter (400)
- `E_PARSE` (+ canonical-form hint) — non-canonical or malformed
  reference (400)
- `E_UNKNOWN_REF` — no local index, with same-namespace `candidates`
  (404):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///minicpmv/nope:raw'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"no local index for minicpmv/nope; network resolver fetch is not implemented in this shardhive build",
#           "candidates":["minicpmv/mmproj"]}}                  # 404
```

- `E_UNKNOWN_REF` — unknown tag, with the per-repository scoping
  message and existing tags as `candidates` (404)
- `E_AMBIGUOUS_SELECTOR` — prefix matches more than one index member,
  `candidates` lists them (400):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/smollm2-135m-instruct:q4'
# {"error":{"code":"E_AMBIGUOUS_SELECTOR","message":"ambiguous prefix q4",
#           "candidates":["q4_k_m","q4km"]}}                    # 400
```

- `E_NO_INDEX` — index blob absent from the CAS (404)
- `E_INVALID_INDEX` — index blob fails validation (500)
- `E_NO_MEMBER` — no index member matches the selector (400):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/smollm2-135m-instruct:q8_0'
# {"error":{"code":"E_NO_MEMBER","message":"no index member matches q8_0"}}   # 400
```

## GET /v1/open

`/resolve` plus the local blob inventory: which files are present with
CAS paths, which digests are missing. Never auto-fills anything.
Executed (200, one weights file, one config, index and manifest blobs):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/open?ref=shardr:///minicpmv/mmproj:raw'
# {"ref":"shardr:///minicpmv/mmproj:raw","ns":"minicpmv","name":"mmproj","quant":"raw",
#  "manifestDigest":"sha256:3db690a6…","indexDigest":"sha256:fb6ae5be…",
#  "plan":"pending","planReason":"…",
#  "files":[
#    {"digest":"sha256:fb6ae5be…","path":"<cas>/blobs/sha256/fb/6ae5be…","size":198},
#    {"digest":"sha256:3db690a6…","path":"<cas>/blobs/sha256/3d/b690a6…","size":620},
#    {"digest":"sha256:e576867a…","path":"<cas>/blobs/sha256/e5/76867a…","size":169,
#     "name":"modelconfig.json","kind":"config"},
#    {"digest":"sha256:f8a805e9…","path":"<cas>/blobs/sha256/f8/a805e9…","size":1044425152,
#     "name":"MiniCPM-V-2.6-mmproj.gguf","kind":"weights.gguf"}]}
```

File records carry the manifest metadata (`name`, `kind`, `role`,
`variant`, `part`, `runtime`) when the manifest is locally present.
`missing` appears only when digests are absent (`omitempty`).
Errors: identical to `/resolve` — the same resolution path runs first.

## POST /v1/ensure

Make a resolved model locally complete. Body `{"ref": "…"}` (canonical
reference).

- All files present → terminal `done` job immediately (executed, 201):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure \
  -d '{"ref":"shardr:///minicpmv/mmproj:raw"}'
# {"id":"b47b97e5e25bc355","ref":"shardr:///minicpmv/mmproj:raw","state":"done",
#  "createdAt":"2026-10-02T12:16:31.184735Z",
#  "manifest":"sha256:3db690a6…","kind":"ensure","filesDone":2,"filesTotal":2}
```

- Files missing + swarm enabled → async fill job (`waiting` →
  `fetching` with progress → terminal).
- Files missing + swarm disabled → terminal `failed` job,
  `E_SOURCE_UNAVAILABLE` naming the config knob.
- Manifest not local → terminal `failed` job, `E_SOURCE_UNAVAILABLE`
  naming the import endpoints that can bring manifests in.
- Unresolvable ref → still 201 with a terminal `failed` job (executed):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure \
  -d '{"ref":"shardr:///minicpmv/nope:raw"}'
# {"id":"23334dca74f81f51","ref":"shardr:///minicpmv/nope:raw","state":"failed",
#  "createdAt":"2026-10-02T12:16:31.193289Z",
#  "error":{"code":"E_UNKNOWN_REF","message":"no local index for minicpmv/nope; …"},
#  "kind":"ensure"}                                              # 201
```

- `E_BAD_REQUEST` — missing `ref` field or bad JSON (executed, 400):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure -d '{}'
# {"error":{"code":"E_BAD_REQUEST","message":"missing required field: ref"}}
```

Jobs are immutable once published — every poll returns a consistent
snapshot; terminal states are final, never silently retried. Jobs live
in daemon memory; a restart empties the job list (CAS and state are
unaffected).

## GET /v1/jobs/{id} · GET /v1/jobs

One job by id; the list endpoint returns all jobs newest-first
(`shardr status` without an argument uses it). Executed terminal
import job (200):

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/cfcba1cbaa111de5
# {"id":"cfcba1cbaa111de5","ref":"gold/smollm2-135m-instruct","state":"done",
#  "createdAt":"2026-10-02T12:17:05.744655Z",
#  "manifest":"sha256:7d2ef480…","kind":"import-local","as":"gold/smollm2-135m-instruct",
#  "filesDone":1,"filesTotal":1,
#  "result":{"manifests":["sha256:7d2ef480…"],"indexDigest":"sha256:2a98f6cb…",
#            "quants":["q4km"],"skipped":0}}
```

Unknown id (executed, 404):

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/deadbeef
# {"error":{"code":"E_NOT_FOUND","message":"no such job: deadbeef"}}
```

## GET /v1/blob/{digest}

Read-only blob access from the CAS — zero-copy, Range support
mandatory. Digest is `sha256:<hex>` or bare hex; only validated blobs
(`blobs/sha256/`) are addressable — partial downloads
(`incoming/*.part`) never are.

Valid range → 206 (executed):

```sh
curl -s --unix-socket "$S" -H 'Range: bytes=0-15' \
  http://localhost/v1/blob/sha256:fb6ae5be9c12290aa6b5fbe85966b19032005db6a47dfc0eb27fad864e7d8549
# {"artifactType"                                              # HTTP 206
```

Non-overlapping range → 416 in the error envelope (executed):

```sh
curl -s --unix-socket "$S" -H 'Range: bytes=99999-' \
  http://localhost/v1/blob/sha256:fb6ae5be9c12290aa6b5fbe85966b19032005db6a47dfc0eb27fad864e7d8549
# {"error":{"code":"E_RANGE_INVALID","message":"invalid range: failed to overlap"}}   # 416
```

Unknown blob → `E_NOT_FOUND` (404); malformed digest →
`E_BAD_REQUEST` (400). Both executed against a running daemon.

## POST /v1/import/local

Ingest local files. Body:

```json
{"paths": ["/abs/path", "/abs/model-dir"], "as": "ns/name"}
```

`as` is **required** (001 §8.6 — never optional) and must be a
well-formed `ns/name`. Paths may be files or directories; a directory
contributes every regular file beneath it. Every source must be a
regular file — symlinks, FIFOs, and devices fail the whole request
with `E_SOURCE_NOT_REGULAR` before any byte is read.

Executed (201, then terminal `done`):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/tmp/toy-model"],"as":"gold/smollm2-135m-instruct"}'
# {"id":"…","ref":"gold/smollm2-135m-instruct","state":"waiting",
#  "kind":"import-local","as":"gold/smollm2-135m-instruct","filesTotal":1}
```

Executed refusal cases (both 400):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/tmp/smollm2-q4km.gguf"]}'
# {"error":{"code":"E_BAD_REQUEST",
#           "message":"missing required field: as (namespace, 001 §8.6 — never optional)"}}

curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/tmp/evil-link.gguf"],"as":"gold/toy"}'
# {"error":{"code":"E_SOURCE_NOT_REGULAR",
#           "message":"importer: import root is a hard boundary — source is not a regular file (symlink/fifo/device); refusing fail-open (E_SOURCE_NOT_REGULAR): /tmp/evil-link.gguf"}}
```

Terminal `result`: `manifests`, `indexDigest`, `quants`, `warnings?`,
`skipped`. Further terminal errors: `E_NOT_IMPORTABLE`,
`E_INVALID_INDEX`, `E_INTERNAL`.

## POST /v1/import/hf

Import a Hugging Face repo. Body `{"repo": "org/name", "revision":
"…"}` — `revision` optional (default branch). The revision is pinned:
the listing resolves to a commit SHA and every byte is fetched at that
SHA (001 §8.8). Anonymous access works for public repos;
`SHARDR_HF_TOKEN` in the daemon's environment enables gated repos.
The namespace derives from the lowercased repo id; original case and
pinned revision land in the manifest annotations. An HF import fetches
the whole repo listing — mind multi-quant repo sizes.

Executed error (repo does not exist / gated without token, 502):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/hf \
  -d '{"repo":"some-org/does-not-exist"}'
# {"error":{"code":"E_SOURCE_FORBIDDEN",
#           "message":"importer: HF access denied (auth required or private repo)"}}
```

Further error classes: `E_BAD_REQUEST` (missing `repo`),
`E_UNKNOWN_REF`, `E_RATE_LIMITED`, `E_SOURCE_UNAVAILABLE`,
`E_NOT_IMPORTABLE`. Success answers 201 with an `import-hf` job; the
terminal `result` shape is the same as local imports.

## POST /v1/import/bt

Pinned BitTorrent v2 import. Body:

```json
{
  "magnet": "magnet:?",            // XOR infohash
  "infohash": "btmh:1220<hex>",    // XOR magnet
  "manifestDigest": "sha256:<hex>", // REQUIRED — the pin
  "trackers": ["…"],                // optional hints
  "webseeds": ["http://…"],         // optional hints
  "peers": ["host:port"]            // optional hints
}
```

**The pin is mandatory: a magnet alone is never accepted.** Executed
(400):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/bt \
  -d '{"infohash":"btmh:1220a707dead"}'
# {"error":{"code":"E_BAD_REQUEST",
#           "message":"missing required field: manifestDigest — the pin is mandatory; a magnet alone is never trusted (005 §5)"}}
```

Executed (swarm-disabled daemon, 501):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/bt \
  -d '{"infohash":"btmh:1220a707dead","manifestDigest":"sha256:bbbb…"}'
# {"error":{"code":"E_NOT_IMPLEMENTED",
#           "message":"swarm client disabled — set [swarm] enabled = true in ~/.config/shardr/config.toml (004 §7)"}}
```

`magnet` and `infohash` together are `E_BAD_REQUEST` (mutually
exclusive, executed). A BT import brings in the pinned artifact's
blobs only — it does not create a namespace index; resolve by
`name:quant` needs a local or HF import, while `@sha256:<manifest>`
resolves immediately. Terminal job errors: `E_NOT_IMPORTABLE`
(verification/binding failures), `E_SOURCE_UNAVAILABLE`.

## POST /v1/import/catalog

Anchored pull of a community-listed model torrent (provider:
pirateface.co). Body:

```json
{
  "repo": "owner/name",       // REQUIRED — as listed by the provider
  "quant": "q4_k_m",          // optional family selector
  "trustCatalog": true,       // REQUIRED for rescued models
  "as": "ns/name",            // optional namespace override
  "peers": ["host:port"]      // optional direct-peer hints
}
```

The daemon owns the whole trust path: it resolves the listing from the
provider itself (never trusting caller-supplied magnets), derives the
anchor from the Hugging Face tree at the pinned revision, runs the
foreign-swarm import, and keeps seeding the swarm it came from
(good-citizen mode, `[catalog] upload_limit` budget — see
[Catalog pulls](../user/catalog.md)).

Executed (repo not listed, 400):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/catalog \
  -d '{"repo":"some-org/does-not-exist"}'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"catalog: repo not listed by the provider: some-org/does-not-exist"}}
```

Executed (swarm disabled, 503):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/catalog -d '{"repo":"x/y"}'
# {"error":{"code":"E_SOURCE_UNAVAILABLE",
#           "message":"swarm client disabled ([swarm] enabled) — catalog pulls need the swarm"}}
```

A **rescued** model (HF source gone) refuses without `trustCatalog`
with `E_NOT_ANCHORED` (422); with it, the anchor becomes the catalog's
own recorded checksums and the job result carries the trust-shift
warning. Terminal job errors include the executed metadata-timeout
case — a listing whose torrent has no seeders online (executed, job
payload after the CLI pull of the same repo):

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/e51d505f1104bc0f
# {"id":"e51d505f1104bc0f","ref":"HuggingFaceTB/SmolLM2-135M-Instruct","state":"failed",
#  "error":{"code":"E_INTERNAL",
#           "message":"swarm: catalog: no torrent metadata within 2 minutes — the swarm is unreachable (seeders online? tracker up?); a magnet alone never carries the file tree"},
#  "kind":"import-catalog","as":"huggingfacetb/smollm2-135m-instruct",
#  "filesDone":24,"filesTotal":24}
```

## POST /v1/verify

Integrity re-hash (003 §4) as an async job. Body `{"target": "…"}`
where target is a canonical ref, a digest, or `"all"` (the CLI spells
it `--all`; the API accepts both). A ref target resolves first (loud
on unknown refs); a manifest target verifies the manifest blob and
every file blob it lists; `"all"` re-hashes the entire CAS.

Executed by-ref (201, terminal `done`):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/verify \
  -d '{"target":"shardr:///gold/smollm2-135m-instruct:q4_k_m"}'
# {"id":"e6b5726d17ec83b9", …, "kind":"verify",
#  "target":"sha256:696ff8cb…","filesDone":3,"filesTotal":3,"state":"fetching"}
# → terminal: state "done", filesDone 3/3
```

Executed `"all"` (terminal `done`, 20/20 blobs; a failure would carry
`E_VERIFY_FAILED` and a `verify` payload with `mismatched`/`missing`/
`stateErrors` lists):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/verify -d '{"target":"all"}'
# → {"state":"done","filesDone":20,"filesTotal":20,"verify":{}}
```

## GET /v1/models

Local inventory from state + CAS presence (executed, 200):

```sh
curl -s --unix-socket "$S" http://localhost/v1/models
# {"skeleton":true,
#  "note":"skeleton inventory: seed state and sizes land with later slices (005 §3)",
#  "namespaces":[
#    {"ns":"gold","name":"smollm2-135m-instruct",
#     "indexDigest":"sha256:1631afaf…","indexPresent":true,
#     "quants":["q4_k_m","q4km","raw"]},
#    {"ns":"minicpmv","name":"mmproj",
#     "indexDigest":"sha256:fb6ae5be…","indexPresent":true,"quants":["raw"]}],
#  "tags":[]}
```

`namespaces[].quants` comes from the current index when present and
valid; `tags[]` entries carry their scoping repo (`repo`, `tag`,
`digest`, `blobPresent`). The response is explicit about being a
skeleton (`skeleton: true`) — seed state and sizes are follow-ups, not
silently missing fields. Error: `E_INTERNAL` (state unreadable).
