# shardr

shardr is a decentralized model storage and distribution stack for LLMs —
"Napster for LLMs". Models are addressed by location-transparent
references (`shardr:///ns/name:quant`), stored in a content-addressed
store, filled from local files / Hugging Face / a BitTorrent v2 swarm,
and served through an OpenAI-compatible runtime. Usage is meant to feel
like `docker run`, for LLMs.

## Components

| Component | Kind | Purpose |
| --- | --- | --- |
| `shardr` | Model runner + CLI | Runs models (OpenAI-compatible endpoints) **and** is the CLI for shardhive (pull/import/models/verify) |
| `shardhive` | Storage daemon | CAS + imports (local / HF / BT) + BitTorrent swarm client; exposes the standardized client interface |
| `shardrbay` | Web index | Magnet-link discovery over the swarm (see `site/shardrbay/`) |

## Quickstart

Requires Go ≥ 1.23.

```sh
go build ./...
go build -o shardr ./cmd/shardr
go build -o shardhive ./cmd/shardhive
./shardr --version   # shardr 0.0.1-dev
```

## Specs

Design documents live in [`docs/specs/`](docs/specs/) — references & URI
scheme (000), artifact format (001), runner & runtime config (002), CAS
(003), swarm (004), shardhive interface (005).
