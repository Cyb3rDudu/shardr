# shardr

shardr is a decentralized registry and distribution service for LLMs — "Napster for LLMs".
It packages models as OCI-like artifacts, stores them in a content-addressed store,
distributes them via BitTorrent v2, and can fall back to importing from Hugging Face.
Model references follow the grammar `shardr.io/ns/name:tag`.

## Components

| Component | Kind | Purpose |
| --- | --- | --- |
| `shardr` | Runtime binary | Client/runtime (later: build/run/serve) |
| `shardhive` | Registry server | Internal registry service |
| `shardrbay` | Web index | Magnet-link web index (see `site/shardrbay/`) |

## Quickstart

Requires Go ≥ 1.23.

```sh
go build ./...
go build -o shardr ./cmd/shardr
go build -o shardhive ./cmd/shardhive
./shardr --version   # shardr 0.0.1-dev
```

## Specs

Design documents live in [`docs/specs/`](docs/specs/).
