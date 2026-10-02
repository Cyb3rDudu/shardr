# Daemon operation

Running shardhive: lifecycle, socket, the access boundary, what lives
on disk, and the verify workflow. The canonical contracts are
[spec 003](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/003-cas-format.md)
(the CAS) and
[spec 005](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/005-shardhive-interface.md)
(the interface); commands are documented in the
[CLI reference](../user/cli.md).

## Serve

```console
$ shardhive serve
shardhive: swarm: seeding 1 artifact(s)
shardhive 0.0.1-dev listening on /var/folders/…/T/shardhive-501/shardhive.sock
```

`serve` blocks until SIGINT/SIGTERM. At startup it loads
`config.toml` (unknown keys in `[swarm]`/`[catalog]` are loud startup
errors — a typo never silently disables seeding), opens the CAS, and —
when the swarm is enabled — synchronously rejoins the swarms of every
complete artifact in state (seed-by-default across restarts, 004 §5).
The seed-start re-hash makes startup cost proportional to stored
bytes; `--seed-no-verify` skips it and is documented unsafe.

Flags: `--socket <path>` (else the resolution below),
`--seed-no-verify`.

## Socket path resolution

`$SHARDR_SOCKET`, else `$XDG_RUNTIME_DIR/shardhive.sock`, else
`${TMPDIR:-/tmp}/shardhive-<uid>/shardhive.sock` (the fallback
directory is created 0700; macOS has no `XDG_RUNTIME_DIR`, hence the
explicit fallback). Executed on macOS, no overrides set:

```console
$ shardhive serve
shardhive 0.0.1-dev listening on /var/folders/…/T/shardhive-501/shardhive.sock

$ ls -l "$TMPDIR/shardhive-501/shardhive.sock"
srw-------  1 dudu  staff 0 … shardhive.sock
```

## The 0600 boundary

The socket is chmod'd **0600** after listen — the socket permission is
the access boundary (005 §3): no local process outside the owning user
can reach the API at all. There is no TCP listener for the management
API. (The swarm client's peer/webseed listeners are separate and serve
only digest-verified CAS bytes; see
[swarm operation](swarm.md).)

Startup refuses to reuse a socket path that is alive (another
shardhive answers) or that is a regular file, directory, or symlink —
a wrong `SHARDR_SOCKET` must fail loudly, not delete user data. Only a
verifiably orphaned socket (no listener + `lstat` says socket) is
replaced.

## CAS layout on disk

Root: `$SHARDR_CAS`, else `$XDG_DATA_HOME/shardr/cas`, else
`~/.local/share/shardr/cas` (003 §2). Executed from the store used
throughout this documentation:

```console
$ ls ~/.local/share/shardr/cas
blobs    incoming    state

$ ls -l ~/.local/share/shardr/cas/blobs/sha256/ed/
-r--r--r--  1 dudu  staff  105454144 … 5fa30c487b282ec156c29062f1222e5c20875a944ac98289dbd242e947f747

$ ls ~/.local/share/shardr/cas/state
distribution.json    namespaces.json
```

- `blobs/sha256/<2-hex>/<62-hex>` — content-addressed blobs, mode
  0444, immutable by contract. The 2-hex sharding bounds each
  directory to ≤ 1/256 of the blob population.
- `incoming/` — in-progress writes (`<random>.part`), never served to
  peers, runtimes, or readers; stale parts (mtime > 24 h) are removed
  at startup.
- `state/` — shardhive-local metadata: current namespace indexes
  (`namespaces.json`), distribution records
  (`distribution.json`), tag aliases. Never torrented.

The write path is verifying and atomic: stream to `incoming/`,
concurrently SHA-256; digest mismatch → delete the part; match →
fsync, chmod 0444, atomic `rename()` into `blobs/`. Partial data is
never promoted, never trusted, never seeded.

## Verify workflow

Integrity is an explicit command with explicit cost, never a
background tax (003 §4). Two surfaces, same re-hash:

CLI, per digest or whole store — exit `0` clean, `1` mismatch,
`2` missing (both executed):

```console
$ shardhive cas verify sha256:ed5fa30c487b282ec156c29062f1222e5c20875a944ac98289dbd242e947f747
OK sha256: ed5fa30c487b282ec156c29062f1222e5c20875a944ac98289dbd242e947f747

$ shardhive cas verify --all
verify --all: 0 mismatched, 0 missing, 0 state errors
all blobs clean
```

API, as an async job (`POST /v1/verify` with a ref, a digest, or
`"all"`; see the [API reference](../references/api.md#post-v1verify)).

## Trust model in one paragraph

A blob is trusted iff it was written by the verifying write path or
fully re-hashed since. Before a blob is first offered to the swarm in
a process's lifetime it is re-hashed and must match — disk corruption
must never silently become swarm corruption. Verification catches
what permission bits cannot: an owner can chmod or replace files; only
the digest and the re-hash detect that.
