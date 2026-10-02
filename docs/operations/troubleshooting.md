# Troubleshooting

Symptom → root cause, using the `E_*` classes. The full inventory with
sources is the [error inventory](../references/errors.md); the wire
envelope is specified in the [HTTP API](../references/api.md).

## Daemon won't start / CLI can't connect

```console
$ shardr models
shardr: E_DAEMON_UNREACHABLE: shardhive daemon not reachable — is it running? (…)
```

- Is `shardhive serve` running? Check the socket:
    `ls -l ${SHARDR_SOCKET:-${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}/shardhive-$(id -u)}/shardhive.sock}`
- `shardhive` refusing the socket path (`refusing to remove regular
  file/directory/symlink at socket path …`): a wrong `SHARDR_SOCKET`
  points at something that is not a socket. The daemon never deletes
  it — unset or fix the variable.
- `socket … is in use by another shardhive`: a second daemon on the
  same socket; pick another `--socket`.

## Config errors at startup (loud, before the daemon serves)

```console
$ shardhive serve
shardhive: config ~/.config/shardr/config.toml: unknown [swarm] key "bogus" (known: enabled, seed, upload_limit, dht, no_seed_verify, webseed_addr)
```

Unknown `[swarm]`/`[catalog]` keys, wrong value types, and malformed
lines are startup failures by design — a typo must not quietly
disable seeding. See the
[configuration reference](../references/configuration.md) for the
valid keys.

## Refused imports

**`E_SOURCE_NOT_REGULAR`** — a local import path contained a symlink,
FIFO, or device (executed):

```console
$ curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
    -d '{"paths":["/tmp/evil-link.gguf"],"as":"gold/toy"}'
{"error":{"code":"E_SOURCE_NOT_REGULAR","message":"importer: import root is a hard boundary — source is not a regular file (symlink/fifo/device); refusing fail-open (E_SOURCE_NOT_REGULAR): /tmp/evil-link.gguf"}}
```

The import root is a hard boundary — resolve the symlink target or
import the real file.

**`E_NOT_ANCHORED` (catalog)** — the listing's Hugging Face source is
gone (rescued). Without `--trust-catalog` / `trustCatalog: true` the
pull refuses; with it, digests verify against the catalog provider's
own checksums (a trust shift, printed as a warning — see
[Catalog pulls](../user/catalog.md)).

**`E_SOURCE_FORBIDDEN` (HF)** — gated repo without a token, or a
nonexistent repo id when anonymous (HF answers 401/403 for both). Set
`SHARDR_HF_TOKEN` in the daemon's environment and re-check the repo
id.

## Partial fills / unreachable sources

**`E_SOURCE_UNAVAILABLE`** with "swarm client disabled" — fills,
`/import/bt`, and catalog pulls need `[swarm] enabled = true`.

**`E_SOURCE_UNAVAILABLE`** with "manifest … is not local" — the name
resolves, but no import ever brought the manifest blob in. Ensure by
importing (local/HF/catalog/bt) first.

**Catalog pull fails after two minutes with "no torrent metadata
within 2 minutes"** (executed against a live listing):

```json
{"error":{"code":"E_INTERNAL",
 "message":"swarm: catalog: no torrent metadata within 2 minutes — the swarm is unreachable (seeders online? tracker up?); a magnet alone never carries the file tree"}}
```

Torrent metadata comes from peers, not from the webseed. A listing
with zero seeders online cannot deliver its file tree — the webseed
only serves file bytes *after* metadata. Retry later or pick another
listing; byte integrity is never at stake (the anchor gates every
file).

**Known gap (tracked on issue #65, post-merge follow-up):** after the
metadata phase, a catalog job has **no stall detection** — a peer
delivering metadata but no bytes leaves the job `fetching`
indefinitely (`context.Background()` in the catalog import handler).
If a catalog job sits in `fetching` far beyond any plausible transfer
time, restart the daemon: jobs live in memory only, the CAS and state
are unaffected, and already-sealed files stay verified.

## Wrong quant / selector errors

- `E_NO_MEMBER` — the repo's index has no such member. List what
    exists: `shardr models` (executed: `gold/smollm2-135m-instruct
    index present q4_k_m,q4km,raw`).
- `E_AMBIGUOUS_SELECTOR` — the prefix matches several members;
    `candidates` lists them; spell the full quant.
- Model imported as `raw` although the filename says `Q4_K_M` — the
    quant vocabulary is lowercase-only; an uppercase token classifies
    as `raw` (see the table in the
    [URI grammar page](../references/uri-references.md#where-quant-names-come-from)).
    Rename the file to a lowercase quant form and re-import; identical
    bytes converge into the existing index.

## Integrity problems

`E_CORRUPTION` (a present blob is not valid), `E_NO_INDEX`,
`E_INVALID_INDEX` — enumerate with the re-hash (executed):

```console
$ shardhive cas verify --all
verify --all: 0 mismatched, 0 missing, 0 state errors
all blobs clean
```

Exit codes: `0` clean, `1` mismatch, `2` missing. Corrupt or missing
blobs are re-fetched by an ensure/import of the affected artifact —
nothing is ever silently rebuilt.

## Runner lifecycle failures

`shardr stop` enforces the supervisor duty (002 §4): **SIGTERM, clean
exit within 30 s, then SIGKILL** — idempotent, with pid identity
verification before signalling (executed):

```console
$ shardr stop docs-e2e
stopping docs-e2e (pid 85034)…
stopped docs-e2e
```

- **`E_STATE: refusing to stop …`** — the pid fails identity
  verification (start token or served-ref mismatch): a stale registry
  entry or pid reuse. The message names the state file; verify and
  clean it manually — the runner refuses to signal a process it
  cannot prove is its own.
- **Foreground `shardr run` hangs after Ctrl-C** — SIGTERM is sent;
  the supervisor waits up to 30 s before SIGKILL. A second Ctrl-C does
  not skip the window.
- **llama-server not found (`E_BINARY`)** — the runner resolves
  `llama-server` next to the `shardr` executable (`bin/llama-server`, a
  symlink into the pinned extract dir); the message names all remedies.
  Run `make llama` (part of `make all`), or point
  `$SHARDR_LLAMA_SERVER` at a binary, or install llama-server on
  `$PATH`.
