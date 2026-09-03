# 005 — shardhive Interface, Resolution & CLI

Status: **Draft v0** — vectors pending: resolve/ensure/open/blob, resolver envelope, version negotiation, error classes

## 1. Purpose

shardhive is the daemon that owns the CAS, the imports (local / HF / BT),
and the swarm client (003, 001 §8, 004). This spec defines the
**standardized interface** between shardhive and its clients, the
**name-resolution model** (§6), and the client contract: **`shardr` is
both the model runner (002) and the CLI for shardhive** (docker/dockerd
split: one daemon, one user-facing binary).

The interface has three versioned parts: the **URI scheme** (000), the
**API** (§3–§5), and the **artifact format** (001).

## 2. Design rules

1. **References, not locations.** Clients know
   `shardr:///ns/name:quant`; they never know (or care) where bytes
   live.
2. **Zero-copy is law.** The API never moves TB-scale blobs into client
   storage. Local clients receive **CAS paths** (mmap-ready); remote
   clients receive **range-read streams**. One copy per machine, owned
   by shardhive (003 §5). KB-scale metadata may be cached freely.
3. **Fail loud.** Unknown references, ambiguous selectors, unknown
   overlay keys, eligibility-gated imports — errors carry reasons and
   candidate lists, never silent fallbacks.
4. **No implicit selector.** The API always requires an explicit
   selector; the interactive CLI may apply the user's
   `default_selector` (004 §7) — humans get comfort, machines get
   determinism.

## 3. API (local IPC, versioned)

**Transport v1: Unix domain socket**, mode 0600, owned by the shardhive
user — the socket permission is the access boundary; no local process
outside the owner can reach the API at all. HTTP-over-loopback is an
**opt-in** deployment (remote or containerized clients) and then
REQUIRES a bearer token plus `Origin`/DNS-rebinding hardening. JSON
in/out; errors are `{"error": {"code", "message", "candidates"?}}`.

**Socket path resolution (v1 rule):** `$SHARDR_SOCKET` if set, else
`$XDG_RUNTIME_DIR/shardhive.sock`, else a per-uid fallback
`${TMPDIR:-/tmp}/shardhive-<uid>/shardhive.sock` (mode 0700 dir; macOS has
no `XDG_RUNTIME_DIR`, hence the explicit fallback).

**Version reporting:** the `/vN` path prefix IS the client's version
declaration — there is no separate negotiation header in v1.

**Error classes (v1 inventory):** `E_PARSE` (malformed reference),
`E_UNKNOWN_REF` (no local index and no resolver hit), `E_NO_INDEX`
(namespace points at a missing index blob — corruption, never silently
rebuilt), `E_INVALID_INDEX` (index fails validation), `E_SOURCE_NOT_REGULAR`
(local import source is not a regular file — hard boundary),
`E_SOURCE_UNAVAILABLE` (reserved source not implemented in this build),
`E_NOT_IMPLEMENTED` (endpoint reserved for a later slice),
`E_NOT_IMPORTABLE` (the requested import fails the 001 §8 rule set or
post-import verification — unimportable content, a manifest pin that
never satisfies, or a distribution-record/identity binding failure;
terminal for the job, never silently retried), `E_RANGE_INVALID`
(blob range does not overlap), `E_UNSUPPORTED_VERSION`, `E_BAD_REQUEST`,
`E_NOT_FOUND`, `E_INTERNAL` (daemon bug — never a user-input verdict).

**@digest in `?ref=`:** allowed — a digest-bearing reference resolves the
manifest directly and bypasses index lookup.

| Endpoint | Method | Purpose |
| --- | --- | --- |
| `/resolve?ref=…` | GET | **pure**: reference → digest + plan. Always: manifest digest, distribution record, source hints. Additionally, iff the metadata documents are locally available or arrived digest-verified from the resolver envelope (§6): manifest document, model config, file list with CAS paths, missing-file list. Otherwise those fields are `pending` until `ensure` completes its metadata phase |
| `/ensure` | POST `{ref}` | start fill (import/swarm) for missing content; returns job id |
| `/open?ref=…` | GET | local-reader convenience: resolution result with **CAS paths** for present files (zero-copy handles); missing files listed, never auto-filled |
| `/jobs/<id>` | GET | job status: state (`waiting`/`fetching`/`seeding`/`failed`/`done`), per-file progress, errors. Terminal states are `done` and `failed`; a terminal job is immutable and every later read returns a copy |
| `/blob/<digest>` | GET | **range-capable** byte stream (206 support mandatory) for remote/foreign clients — read-through, never a mandated copy |
| `/import/local` | POST `{paths[], as}` | ingest local files; namespace `as` is **required** (001 §8.6) |
| `/import/hf` | POST `{repo, revision?}` | HF import (001 §8) |
| `/import/bt` | POST `{magnet \| infohash, manifestDigest}` | BT import with pinned manifest (001 §8.7) |
| `/models` | GET | inventory: known repos, members, quants, sizes, seed state |
| `/events` | GET (SSE) | progress/lifecycle events (reserved v1.1) |

`resolve` answers "what is this name"; `ensure` makes it local; `open`
hands out handles. Side effects live exclusively in `ensure` and the
import endpoints.

## 4. CLI (`shardr`, the reference client)

`shardr` speaks the API above; every management concern is a subcommand
of the same binary that runs models. Interactive commands accept CLI
short refs (`ns/name:quant`) and canonicalize (000 §2):

| Command | Maps to | Purpose |
| --- | --- | --- |
| `shardr pull <ref>` | `/ensure` | fill the CAS (import/swarm), no run |
| `shardr import local <paths> --as ns/name` | `/import/local` | ingest local model files |
| `shardr import hf <repo> [--rev <sha>]` | `/import/hf` | HF import |
| `shardr import bt <magnet> --manifest <digest>` | `/import/bt` | BT import with pinned manifest |
| `shardr models` | `/models` | inventory listing |
| `shardr verify <ref\|digest\|--all>` | verify job | integrity re-hash (003 §4) |
| `shardr build [-f Modelfile]` | (002 §3.4) | compose a model image |
| `shardr run/serve/stop …` | (002 §4) | model lifecycle |
| `shardr status [job]` | `/jobs/<id>` | fill/import progress |

Third-party clients use the same API directly; the CLI is a client, not
a privilege.

## 5. Trust and verification

- All digests are verified by shardhive (verify-write, 003 §3); clients
  MAY re-verify using digests from `resolve` — the data is there.
- `/import/bt` MUST pin a manifest digest; the fetched manifest file
  must hash to it or the import fails. **A magnet alone is never
  trusted — the manifest is.**
- Socket permissions (v1) are the access boundary; explicit auth tokens
  are a later, versioned addition for the HTTP deployment mode.

## 6. Name resolution & trust model

Resolution chain, in order: **local state** (current index / tag alias)
→ **configured resolvers** → resolution failure (loud).

- **Configured resolvers**: an ordered list in user config (default:
  shardrbay). A resolver answers with a **resolver envelope**:
  `{ref → indexDigest/manifestDigest, distribution record, source hints,
  verified sidecars}` — the sidecars (manifest document, model config,
  piece-layers blob, `.torrent` metainfo) are optional per field and
  each is digest-checked on arrival; shardrbay is expected to always
  ship manifest + piece-layers. shardrbay is a **trusted resolver** —
  explicitly configured, not magically trustless.
- **Bytes are digest-verified** end to end: manifests, distribution
  records, sidecars, blobs. Integrity is total.
- **Name authenticity in v1 is honest and limited**: a digest proves
  "these bytes match this hash", not "this digest really is
  `unsloth/qwen…:q8_0`". Binding names to digests rests on the
  configured resolver (trust-on-first-use of its answers), or on a
  **local pin** (`@digest` in configs/Modelfiles), which is the only
  unconditionally trustworthy form.
- **Reserved v2 protocol surface**: signed namespace records (Ed25519
  key per namespace; signed index records exchanged via DHT or resolvers)
  to upgrade name authenticity from TOFU to cryptographic. Not part of
  this spec version.

## 7. Versioning

- URI scheme, API, and artifact format carry a shared major version.
  Additive changes bump minors; breaking changes bump majors and run side
  by side.
- Clients report their supported version; shardhive rejects unsupported
  majors loudly.
