# Getting started

This walkthrough covers install, first daemon start, a local import, and
store verification. Everything shown here works in the current build.

## Installation

From source (requires Go ≥ 1.23):

```sh
git clone https://github.com/Cyb3rDudu/shardr
cd shardr
go build -o shardhive ./cmd/shardhive
./shardhive version
# shardhive 0.0.1-dev
```

The version string is injectable at build time:

```sh
go build -ldflags "-X main.version=1.2.3" -o shardhive ./cmd/shardhive
```

## Where shardhive puts things

| What | Env override | Default |
| --- | --- | --- |
| Content store (CAS) | `$SHARDR_CAS` | `$XDG_DATA_HOME/shardr/cas`, else `~/.local/share/shardr/cas` |
| Daemon socket | `$SHARDR_SOCKET` | `$XDG_RUNTIME_DIR/shardhive.sock`, else `/tmp/shardhive-<uid>/shardhive.sock` |
| Config file | `$SHARDR_CONFIG` | `$XDG_CONFIG_HOME/shardr/config.toml`, else `~/.config/shardr/config.toml` |

No config file is required: with no `config.toml` present, documented
defaults apply (see [config.md](config.md)).

## Start the daemon

```sh
./shardhive serve
# shardhive 0.0.1-dev listening on /run/user/1000/shardhive.sock
```

The socket is created with mode 0600 — the file permission **is** the
access boundary. Only your user can talk to the daemon; there is no TCP
listener in this build.

Use `--socket <path>` to choose a different path for this run, or set
`$SHARDR_SOCKET` for all clients. If the socket path is occupied by
something that is not a verifiably orphaned Unix socket (a regular file,
a directory, a symlink), the daemon refuses to start rather than delete
it.

With the swarm enabled and complete artifacts in the store, the daemon
also joins their swarms at startup:

```sh
./shardhive serve
# shardhive: swarm: seeding 1 artifact(s)
# shardhive 0.0.1-dev listening on /run/user/1000/shardhive.sock
```

## Import a local model

Point the import at a directory (or a set of files) and name the
namespace it should land in — `as` is mandatory:

```sh
S=${SHARDR_SOCKET:-${XDG_RUNTIME_DIR}/shardhive.sock}
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/path/to/model-dir"],"as":"gold/my-model"}'
# {"id":"c04e88d49859eabe","ref":"gold/my-model","state":"waiting",
#  "kind":"import-local","as":"gold/my-model","filesTotal":5}
```

Imports are asynchronous jobs. Poll until `state` is `done`:

```sh
curl -s --unix-socket "$S" http://localhost/v1/jobs/c04e88d49859eabe
# {"id":"c04e88d49859eabe", … "state":"done","filesDone":5,"filesTotal":5,
#  "result":{"manifests":["sha256:…"],"indexDigest":"sha256:…",
#            "quants":["q8_0"],"skipped":0}}
```

`quants` lists the quantization members the import produced — that is
the selector you use in references. The quant is derived per artifact:
from the filename token (e.g. `model-q8_0.gguf` → `q8_0`), else from the
upstream `config.json` `quantization_config`, else from the dominant
safetensors dtype, else `raw`.

Only **regular files** are importable — symlinks (even pointing back
inside the root), FIFOs, and devices fail the whole import with
`E_SOURCE_NOT_REGULAR`. See [importing.md](importing.md) for what the
importer recognizes and skips.

## Check the inventory

```sh
curl -s --unix-socket "$S" http://localhost/v1/models
# {"skeleton":true,
#  "note":"skeleton inventory: seed state and sizes land with later slices (005 §3)",
#  "namespaces":[{"ns":"gold","name":"my-model","indexDigest":"sha256:…",
#                 "indexPresent":true,"quants":["q8_0"]}],
#  "tags":[]}
```

And resolve the reference you just created (the API takes the canonical
URI form — see [api.md](api.md)):

```sh
curl -s --unix-socket "$S" \
  'http://localhost/v1/resolve?ref=shardr:///gold/my-model:q8_0'
# {"ref":"shardr:///gold/my-model:q8_0","ns":"gold","name":"my-model",
#  "quant":"q8_0","manifestDigest":"sha256:…","indexDigest":"sha256:…",
#  "plan":"pending", …}
```

## Verify the store

The CAS is content-addressed: every blob's name is its SHA-256. `cas
verify` re-hashes and proves it:

```sh
./shardhive cas verify --all
# verify --all: 0 mismatched, 0 missing, 0 state errors
# all blobs clean          (exit 0)
```

A single digest also works (`sha256:<hex>` or bare hex). Exit codes are
stable: `0` clean, `1` digest mismatch, `2` missing, `64` usage error.
See [cli.md](cli.md).

## Stop the daemon

`Ctrl-C` or `SIGTERM`; the daemon closes the socket and exits 0.

## Next steps

- [importing.md](importing.md) — Hugging Face and BitTorrent imports,
  eligibility rules, what gets skipped
- [swarm.md](swarm.md) — what seeding means and how to bound it
- [errors.md](errors.md) — the `E_*` error classes with causes and fixes
