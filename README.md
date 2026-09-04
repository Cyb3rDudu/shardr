# shardr

shardr is a decentralized model storage and distribution stack for LLMs.
Models are addressed by location-transparent references (`shardr:///ns/name:quant`), stored in a content-addressed store, and filled from local files, Hugging Face, or a BitTorrent v2 swarm. Every byte is digest-verified on the way in, so the transport never has to be trusted.

## Components

| Component | Kind | Status | Purpose |
| --- | --- | --- | --- |
| `shardhive` | Storage daemon | **works today** | CAS + imports (local / HF / BT) + BitTorrent v2 swarm client (fetch + seed); serves API v1 over a Unix socket |
| `shardr` | Model runner | in development | OpenAI-compatible model runtime (currently a version-stub binary) |
| `shardrbay` | Web index | planned | Magnet-link discovery over the swarm |

## Quickstart

Requires Go ≥ 1.23.

```sh
go build -o shardhive ./cmd/shardhive
./shardhive serve          # daemon; API v1 on a Unix socket (mode 0600)
```

In a second shell, import a local model directory and verify the store:

```sh
S=${SHARDR_SOCKET:-${XDG_RUNTIME_DIR}/shardhive.sock}
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/path/to/model-dir"],"as":"gold/my-model"}'
# → {"id":"…","state":"waiting",…}   poll GET /v1/jobs/{id} until "done"

curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/my-model:q8_0'
./shardhive cas verify --all   # re-hash every blob; exit 0 clean
```

See [docs/user/getting-started.md](docs/user/getting-started.md) for the full walkthrough,
[docs/user/api.md](docs/user/api.md) for all endpoints, and
[docs/user/cli.md](docs/user/cli.md) for the complete CLI reference.

## Documentation

- **Using shardhive** — [getting started](docs/user/getting-started.md) ·
  [CLI reference](docs/user/cli.md) · [configuration](docs/user/config.md) ·
  [API v1](docs/user/api.md) · [importing models](docs/user/importing.md) ·
  [swarm & seeding](docs/user/swarm.md) · [error reference](docs/user/errors.md)
- **Working on the code** — [architecture](docs/dev/architecture.md) ·
  [code layout](docs/dev/layout.md) · [testing](docs/dev/testing.md) ·
  [conventions](docs/dev/conventions.md)
- **Design specs** — [docs/specs/](docs/specs/): references & URI scheme (000),
  artifact format (001), Modelfile & model contract (002), CAS (003),
  torrent mapping (004), shardhive interface (005). Specs are the source of
  truth for protocol behavior; these docs describe what the current build does.
