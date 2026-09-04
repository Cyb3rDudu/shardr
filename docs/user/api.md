# shardhive API v1

HTTP/1.1 over a Unix domain socket (mode 0600 — the socket permission
is the access boundary; no TCP in this build). JSON in, JSON out.

```sh
S=${SHARDR_SOCKET:-${XDG_RUNTIME_DIR:-/tmp/shardhive-$(id -u)}/shardhive.sock}
curl -s --unix-socket "$S" http://localhost/v1/models
```

## Versioning

The major version is a path prefix: `/v1/…`. Requests aimed at another
major version are rejected loudly so clients can detect mismatches:

```sh
curl -s --unix-socket "$S" http://localhost/v2/resolve
# {"error":{"code":"E_UNSUPPORTED_VERSION",
#           "message":"unsupported API major version /v2/ (/v2/resolve); this shardhive supports v1",
#           "candidates":["v1"]}}            # HTTP 400
```

Unknown paths under `/v1/` are a plain 404 (`E_NOT_FOUND`).

## Error envelope

Every error — including HTTP-level failures like an unsatisfiable range
— is one JSON shape:

```json
{"error": {"code": "E_UNKNOWN_REF", "message": "…", "candidates": ["…"]}}
```

`candidates` is optional and lists known alternatives (e.g. existing
tags for a repo, same-namespace models) so callers can self-correct.
See [errors.md](errors.md) for the full class inventory.

## References at the API

The API accepts **only the canonical URI form** (000 §2):

```
shardr:///ns/name:quant          # namespace, name, quant selector
shardr:///ns/name:tag+quant      # tag-pinned (tags are per-repository)
shardr:///ns/name@sha256:<hex>   # manifest-addressing: digest pin, no selector
```

ns/name are lowercased by the parser; tags are case-sensitive and kept
verbatim. A parseable CLI short form (`ns/name:quant`) is rejected with
`E_PARSE` and the message names the canonical spelling to use:

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=gold/toy-qwen:q8_0'
# {"error":{"code":"E_PARSE",
#           "message":"references at the API must be canonical URIs; use shardr:///gold/toy-qwen:q8_0"}}   # 400
```

---

## GET /v1/resolve

Pure name → digests lookup against **local state only**. Never touches
the network.

| Query | Meaning |
| --- | --- |
| `ref` | canonical reference (required) |

Success (200):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/toy-qwen:q8_0'
# {"ref":"shardr:///gold/toy-qwen:q8_0","ns":"gold","name":"toy-qwen",
#  "quant":"q8_0",
#  "manifestDigest":"sha256:bca235eb…",
#  "indexDigest":"sha256:206ada42…",
#  "plan":"pending",
#  "planReason":"manifest document parsing lands with imports (001 §8)"}
```

`plan`/`planReason` describe the runner's execution plan and stay
`pending` until the runner slice lands. The `@sha256:<hex>` form
resolves without any state lookup (the digest *is* the answer) and
returns no `indexDigest`.

Errors: `E_BAD_REQUEST` (missing `ref`), `E_PARSE`/`E_LENGTH`/…
(reference grammar, with canonical-form hints), `E_UNKNOWN_REF` (no
local index; message says network resolver fetch is not implemented in
this build, `candidates` lists same-namespace repos), `E_UNKNOWN_REF` with
tag-scoped message + `candidates` (an unknown tag is an unknown ref:
"no tag <tag> for ns/name; tags are scoped per repository (000 §3.4)"),
`E_NO_INDEX` (index blob not in CAS), `E_INVALID_INDEX` (index fails
validation), `E_NO_MEMBER` (no index member matches the selector).

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/nope:q8_0'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"no local index for gold/nope; network resolver fetch is not implemented in this shardhive build",
#           "candidates":["gold/toy-qwen"]}}                 # 404
```

## GET /v1/open

`/resolve` plus local blob paths: which files are present and where.
Never auto-fills anything.

Success (200) — same fields as `/resolve`, plus `files` (present) and
`missing` (digests):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/open?ref=shardr:///gold/toy-qwen:q8_0'
# { … "files":[
#     {"digest":"sha256:206ada42…","path":"~/.local/share/shardr/cas/blobs/sha256/20/6ada42…","size":485},
#     {"digest":"sha256:bca235eb…","path":"…/blobs/sha256/bc/a235eb…","size":602}]}
```

`missing` appears only when digests are absent (`omitempty` — an empty
`missing` key is not emitted).

Errors: identical to `/resolve` — the same resolution path runs first:

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/open?ref=shardr:///gold/none:q8_0'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"no local index for gold/none; network resolver fetch is not implemented in this shardhive build"}}   # 404
```

## POST /v1/ensure

Make a resolved model locally complete. Body: `{"ref": "…"}`.

- All files present → terminal `done` job immediately.
- Files missing and the swarm client is enabled → asynchronous swarm
  fill job (fetch from peers/webseeds, digest-verified on write).
- Files missing and the swarm client is disabled → terminal `failed`
  job with `E_SOURCE_UNAVAILABLE` naming the config knob.

Returns 201 with the initial job; poll `GET /v1/jobs/{id}`:

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure \
  -d '{"ref":"shardr:///gold/toy-qwen:q8_0"}'
# {"id":"84a0207d94abdbed","ref":"shardr:///gold/toy-qwen:q8_0",
#  "state":"done","manifest":"sha256:bca235eb…","kind":"ensure",
#  "filesDone":2,"filesTotal":2}                             # 201
```

A pre-resolved failure (unknown ref) still answers 201 — with a
terminal `failed` job:

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure \
  -d '{"ref":"shardr:///gold/none:q8_0"}'
# {"id":"61fe0aff9f21cb27","ref":"shardr:///gold/none:q8_0","state":"failed",
#  "error":{"code":"E_UNKNOWN_REF","message":"no local index for gold/none; …"},
#  "kind":"ensure"}                                              # 201
```

Job states: `waiting` → `fetching` (with `filesDone`/`filesTotal`
progress) → `done` | `failed` (with an `error` envelope). Jobs are
immutable once published — every poll returns a consistent snapshot,
and a terminal state is final, never silently retried.

Errors (as terminal job payload or, for pre-job failures, as the
response): `E_BAD_REQUEST`, ref-grammar classes, `E_UNKNOWN_REF`,
`E_SOURCE_UNAVAILABLE` (manifest not local — the message names the
import endpoints that can bring manifests in), swarm-fill outcomes
(`E_NOT_IMPORTABLE` for verification failures, `E_SOURCE_UNAVAILABLE`
for unreachable sources).

## GET /v1/jobs/{id}

Fetch one job by id (the `id` from an import/ensure response):

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/f4af8094937a22bd
# {"id":"f4af8094937a22bd","ref":"gold/toy-qwen","state":"done",
#  "manifest":"sha256:91471361…","kind":"import-local","as":"gold/toy-qwen",
#  "filesDone":5,"filesTotal":5,
#  "result":{"manifests":["sha256:91471361…"],"indexDigest":"sha256:88edcdb0…",
#            "quants":["raw"],"skipped":0}}
```

Errors: `E_NOT_FOUND` (404) for an unknown id:

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/deadbeef
# {"error":{"code":"E_NOT_FOUND","message":"no such job: deadbeef"}}   # 404
```

Note: jobs live in daemon memory — a restart empties the job list (the
CAS and state are unaffected).

## GET /v1/blob/{digest}

Read-only blob access from the CAS — zero-copy, Range support
mandatory. Digest is `sha256:<hex>` or bare hex.

```sh
curl -s --unix-socket "$S" -H 'Range: bytes=0-4' \
  http://localhost/v1/blob/sha256:206ada42…
# {"art                        # HTTP 206 (partial)
```

- `200` full body, `206` valid range, `416` + `E_RANGE_INVALID` for a
  range that does not overlap the blob.
- Only validated blobs (`blobs/sha256/`) are addressable — partial
  downloads (`incoming/*.part`) never are.

```sh
curl -s --unix-socket "$S" -H 'Range: bytes=999999-' \
  http://localhost/v1/blob/sha256:206ada42…
# {"error":{"code":"E_RANGE_INVALID","message":"invalid range: failed to overlap"}}   # 416
```

Errors: `E_BAD_REQUEST` (invalid digest form), `E_NOT_FOUND` (no such
blob), `E_RANGE_INVALID`.

## POST /v1/import/local

Import local files. Body:

```json
{"paths": ["/abs/path", "/abs/model-dir"], "as": "ns/name"}
```

`as` is **mandatory** (001 §8.6 — never optional) and must be a
well-formed `ns/name`. Paths may be files or directories; a directory
contributes every regular file beneath it. Everything must be a
**regular file** — symlinks, FIFOs, devices fail the whole request with
`E_SOURCE_NOT_REGULAR` (400) before any byte is read.

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/tmp/toy-model"],"as":"gold/toy-qwen"}'
# {"id":"c04e88d49859eabe","ref":"gold/toy-qwen","state":"waiting",
#  "kind":"import-local","as":"gold/toy-qwen","filesTotal":5}    # 201
```

Terminal `result` (see `/v1/jobs/{id}` above): `manifests`,
`indexDigest`, `quants`, `warnings?`, `skipped`.

Errors: `E_BAD_REQUEST` (bad body, missing `paths`/`as`, malformed
`as`), `E_SOURCE_NOT_REGULAR` (non-regular source), then importer
outcomes as terminal job errors (`E_NOT_IMPORTABLE`,
`E_INVALID_INDEX`, `E_INTERNAL`).

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/tmp/evil-link.gguf"],"as":"gold/bad"}'
# {"error":{"code":"E_SOURCE_NOT_REGULAR",
#           "message":"importer: import root is a hard boundary — source is not a regular file (symlink/fifo/device); refusing fail-open (E_SOURCE_NOT_REGULAR): /tmp/evil-link.gguf"}}   # 400
```

## POST /v1/import/hf

Import a Hugging Face repo. Body: `{"repo": "org/name", "revision":
"…"}` — `revision` optional (default branch).

The revision is **pinned**: the repo listing resolves to a commit SHA
and every file byte is fetched at that SHA, never at the mutable branch
— a branch move between listing and fetch cannot publish bytes from
commit Y under the provenance of commit X (001 §8.8).

Public repos work anonymously; set `SHARDR_HF_TOKEN` in the daemon's
environment for gated repos (sent as a Bearer token).

The namespace derives from the repo id (lowercased `org/name`); the
original-case repo id and the pinned revision are recorded in the
manifest annotations.

Note: an HF import fetches **the whole repo** (every file the listing
returns), so mind the size for multi-quant repos.

Real run (public repo, anonymous access):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/hf \
  -d '{"repo":"klosax/tinyllamas-stories-gguf"}'
# {"id":"00c4a5efa2c02d17","ref":"klosax/tinyllamas-stories-gguf",
#  "state":"waiting","kind":"import-hf",
#  "as":"klosax/tinyllamas-stories-gguf","filesTotal":6}         # 201
# … poll /v1/jobs/{id}: state "fetching" → "done"; the terminal
#   result shape is the same as local imports (manifests, indexDigest,
#   quants, skipped).
```

Errors: `E_BAD_REQUEST` (missing `repo`), then HF outcomes as 502
response-body errors or terminal job errors: `E_UNKNOWN_REF` (repo not
found), `E_SOURCE_FORBIDDEN` (gated or — when anonymous — nonexistent
repo), `E_RATE_LIMITED`, `E_SOURCE_UNAVAILABLE` (unreachable),
`E_NOT_IMPORTABLE`, `E_INTERNAL`.

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/hf \
  -d '{"repo":"some-org/does-not-exist"}'
# {"error":{"code":"E_SOURCE_FORBIDDEN",
#           "message":"importer: HF access denied (auth required or private repo)"}}   # 502
```

## POST /v1/import/bt

Pinned BitTorrent v2 import. Body:

```json
{
  "magnet": "magnet:?",                    // XOR infohash
  "infohash": "btmh:1220<hex>",            // XOR magnet
  "manifestDigest": "sha256:<hex>",        // REQUIRED — the pin
  "trackers": ["…"],                        // optional hints
  "webseeds": ["http://…"],                 // optional hints
  "peers": ["host:port"]                    // optional hints
}
```

**The pin is mandatory: a magnet alone is never accepted.** The fetch
is bound to the pinned manifest digest; every fetched byte is verified
against it on write. An infohash that delivers bytes not matching the
pin fails loudly (`E_NOT_IMPORTABLE`).

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/bt \
  -d '{"infohash":"btmh:1220a707…"}'
# {"error":{"code":"E_BAD_REQUEST",
#           "message":"missing required field: manifestDigest — the pin is mandatory; a magnet alone is never trusted (005 §5)"}}   # 400
```

A real pinned import (node B pulling from seeding node A's webseed —
both are shardhive daemons on this machine):

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/bt \
  -d '{"infohash":"btmh:1220a707…","manifestDigest":"sha256:1d5491c2…","webseeds":["http://127.0.0.1:59718"]}'
# {"id":"facb9adcab3fa66d", … ,"kind":"import-bt"}            # 201
# … poll /v1/jobs/{id} →
# {"state":"done","result":{"manifests":["sha256:1d5491c2…"],
#   "infohash":"btmh:1220a707…","skipped":0}}
```

Note what a BT import brings in: the **pinned artifact's blobs**
(manifest + files). It does not create a namespace index — resolve by
`name:quant` on the importing node needs a local (or HF) import; the
`@sha256:<manifest>` form resolves immediately.

The infohash for a pinned import comes from the publisher's
distribution record (the `torrent.infohash` field of the record blob in
the CAS) — a user-facing surface for it lands with the shardrbay/CLI
slice. Until then, the seeding node's record blob carries it.

Errors: `E_BAD_REQUEST` (bad body, missing/ambiguous magnet vs
infohash, non-canonical pin), `E_NOT_IMPLEMENTED` (501 — swarm client
disabled; the message names the `[swarm] enabled` config knob), then
swarm outcomes as terminal job errors (`E_NOT_IMPORTABLE`,
`E_SOURCE_UNAVAILABLE`).

## GET /v1/models

Local inventory from state + CAS presence:

```sh
curl -s --unix-socket "$S" http://localhost/v1/models
# {"skeleton":true,
#  "note":"skeleton inventory: seed state and sizes land with later slices (005 §3)",
#  "namespaces":[
#    {"ns":"gold","name":"toy-qwen","indexDigest":"sha256:88edcdb0…",
#     "indexPresent":true,"quants":["q8_0"]}],
#  "tags":[]}
```

`tags` entries carry the repo they are scoped to (`repo`, `tag`,
`digest`, `blobPresent`). The response is honest about being a skeleton
(`skeleton: true`, `note`): seed state and sizes are explicit
follow-ups, not silently missing fields.

Errors: `E_INTERNAL` (state unreadable).
