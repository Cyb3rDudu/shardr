# CLI reference

Everything the `shardhive` daemon binary does today, derived from the
code, plus the `shardr` runner lifecycle. The `shardr` CLI surface
(import/pull/models/verify/status) is covered in the
[README](https://github.com/Cyb3rDudu/shardr#adding-models) and
[getting-started.md](getting-started.md).
Usage/dispatch errors and warnings go to stderr; `cas verify` prints
its per-digest outcome lines (including `FAIL …`) to stdout.

```
usage: shardhive <command> [args]

commands:
  serve [--socket <path>]     run the daemon: API v1 over a Unix socket
                              (mode 0600; socket permission is the access
                              boundary)
  cas verify <digest|--all>   re-hash blobs and check digests; digests may
                              be bare hex or canonical sha256:<hex>
                              (exit 0 clean, 1 mismatch, 2 missing)
  version                     print version

usage/dispatch errors exit 64.
```

## `shardhive version`

Prints `shardhive <version>` and exits 0. The version is `0.0.1-dev`
unless injected at build time:

```sh
go build -ldflags "-X main.version=1.2.3" -o shardhive ./cmd/shardhive
./shardhive version
# shardhive 1.2.3
```

## `shardhive serve`

Runs the daemon until SIGINT/SIGTERM (exit 0 after clean shutdown).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--socket <path>` | see below | Unix socket path for this run |
| `--seed-no-verify` | off | skip the startup re-hash before seeding (see below) |

Startup sequence:

1. Load `config.toml` (`[swarm]` section; see
   [config.md](config.md)). A malformed file or an unknown `[swarm]` key
   is a loud error and the daemon does not start — a typo must never
   quietly disable seeding.
2. If the swarm is enabled: construct the swarm client and synchronously
   seed every complete artifact in the store (startup seed). This
   re-hashes sealed blobs first (CAS discipline); artifacts imported
   after startup join the swarm on the next restart, or as soon as they
   are fetched via `/v1/ensure` on this node.
3. Open the CAS, create the socket, serve API v1.

Startup output (stderr for swarm lines, stdout for the ready line):

```
shardhive: swarm: seeding 1 artifact(s)
shardhive 0.0.1-dev listening on /run/user/1000/shardhive.sock
```

Socket path resolution, in order:

1. `--socket <path>`
2. `$SHARDR_SOCKET`
3. `$XDG_RUNTIME_DIR/shardhive.sock`
4. `/tmp/shardhive-<uid>/shardhive.sock` (directory created 0700)

The socket is chmod'd 0600 — that permission is the access boundary.

`--seed-no-verify` is the documented-unsafe escape hatch (spec 003 §4):
it skips the startup re-hash, so on-disk corruption could become swarm
corruption. It is rejected outright when the swarm is disabled (it would
be meaningless), and never default.

Exit codes: `0` clean shutdown · `1` runtime failure (config error, CAS
open failure, port conflicts) · `64` flag/dispatch misuse.

If the socket path is occupied by a live daemon, startup fails loudly
("socket … is in use by another shardhive"). A leftover socket from a
crashed daemon is removed only when nothing accepts connections on it
**and** lstat confirms it is a socket; regular files, directories, and
symlinks at the socket path are never deleted.

## `shardhive cas verify`

Re-hashes blobs and compares against their content-addressed names.

```sh
# one digest (canonical or bare hex)
./shardhive cas verify sha256:206ada423e06f7e44458ac189fd2c82339eb3d7d14c00d472e294ffdfda4522c
# OK sha256: 206ada423e06f7e44458ac189fd2c82339eb3d7d14c00d472e294ffdfda4522c   (exit 0)

# everything in the store
./shardhive cas verify --all
# verify --all: 0 mismatched, 0 missing, 0 state errors
# all blobs clean                                                              (exit 0)
```

Failure output (each failing digest gets a line):

```
./shardhive cas verify --all
# FAIL sha256: 962f85a2… (digest mismatch)
# verify --all: 1 mismatched, 0 missing, 0 state errors      (exit 1)

./shardhive cas verify sha256:0000…   # not in the store
# FAIL cas: blob missing: sha256:0000…                        (exit 2)
```

Semantics:

- `--all` walks **every** blob in `blobs/sha256/` and additionally
  checks all digests referenced by `state/` (namespaces, tags,
  distribution links) for existence — a walk alone cannot notice
  deletions. Foreign files (e.g. `.DS_Store`) are skipped, not errors.
- Mismatch outranks missing when both occur.
- State files that fail to parse are reported as `WARN state unreadable`
  on stderr and counted as `state errors`; they do not change the exit
  code (broken state is not blob corruption), but they are never silent.
- Digest arguments must be lowercase hex — either bare
  (`<64 hex>`) or canonical (`sha256:<64 hex>`). Uppercase is not
  canonicalized away: canonical or nothing.

Exit codes: `0` clean · `1` digest mismatch · `2` missing blob /
store error · `64` usage (no argument, or `cas` without `verify`).

## `shardr` runner lifecycle (run / serve / stop)

`shardr` is the client binary for the model runner (spec 002 §4). It
talks to the same daemon over the same socket.

```
shardr run <ref> [--config f.toml] [--set k=v]…      foreground run
shardr serve <ref> [--id name] [--config f.toml] [--set k=v]…
                                                     background instance
shardr stop [<id>|--all]                             stop instances
```

Short refs (`ns/name:quant`) are canonicalized before the API sees
them. Runtime keys are validated against the 002 §7.1 allowlist.

**`run`** resolves and ensures the ref against the daemon, merges the
runtime overlay (see [config.md](config.md)), spawns llama-server, and
waits. The reference is passed as CAS paths — llama-server mmaps the
weights **directly out of the content-addressed store**; no additional
copy of the model is written. Multi-part (split) GGUFs get a
per-launch scratch directory of hardlinks that is removed on every
exit path. `Ctrl-C` sends SIGTERM to llama-server; if it has not exited
within 30 s it is killed with SIGKILL:

```
ready: http://127.0.0.1:8080 (model id shardr:///qwen/test:q8_0) — Ctrl-C to stop
^C
stopping (SIGTERM; SIGKILL after 30 s)
```

**`serve`** runs the same lifecycle detached under a stable `--id`
(name defaults to a generated one), records the instance in the
runner registry, and prints the endpoint:

```
shardr serve qwen/test:q8_0 --id mainllm
# serving shardr:///qwen/test:q8_0
#   id       mainllm
#   endpoint http://127.0.0.1:8081
#   model    shardr:///qwen/test:q8_0
```

`--id` is serve-only (`run` rejects it — a foreground process has no
stable id).

**`stop`** terminates serve instances: SIGTERM with a 30 s deadline,
then SIGKILL — per `id`, or all at once with `--all`. With no
argument it lists the running instances. Before signaling, the stop
path re-verifies the target's identity (start token and served ref)
so a recycled PID is never killed by name; the identity check is
repeated before the SIGKILL escalation. Split-scratch cleanup runs on
stop as on `run` exit.

## Exit codes at a glance

| Code | Meaning |
| --- | --- |
| `0` | success (verify clean, clean shutdown) |
| `1` | verify: digest mismatch · serve: runtime error |
| `2` | verify: missing blob or store error |
| `64` | usage/dispatch error (`EX_USAGE`) |
