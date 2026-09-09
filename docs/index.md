# shardr

shardr is a decentralized LLM repository with sync-based distribution. It
keeps large language models available and digitally sovereign: model files
live in content-addressed storage on your own machines, synchronize over a
BitTorrent-based peer network, and are served through an OpenAI-compatible
runtime. Availability grows with every node that participates; no single
provider is required.

The system has two parts. **shardhive** is the storage daemon: a
content-addressed store (CAS) with imports from local files, Hugging Face,
and BitTorrent, plus the sync client and API v1 over a mode-0600 Unix
socket. **shardr** is the model runtime and management CLI: it runs a model
with layered runtime configuration and maps the weights directly out of the
CAS at serve time.

Content is addressed by digest and verified on every write, independent of
how a byte arrived. The reference `ns/name:quant` addresses an artifact;
the quant vocabulary is protocol-level and parses identically on every
node.

## Where to go next

| Goal | Path |
| --- | --- |
| Import and manage models | [Importing models](user/importing.md) |
| How seeding and peer synchronization work | [Swarm & seeding](user/swarm.md) |
| Configure shardhive and the model runner | [Configuration](user/config.md) |
| Work on the code (architecture, layout, conventions) | [Developer Guide](dev/architecture.md) |
| Concept tour and quickstart (in progress, Epic #41 S5) | [Get Started](get-started/quickstart.md) |

## Status

Operational end to end today: import → CAS → synchronization → serving a
real GGUF with the pinned llama.cpp runtime. Specifications live in
[`docs/specs/`](https://github.com/Cyb3rDudu/shardr/tree/main/docs/specs) in
this repository and are canonical; site pages summarize them and link to
the code tree rather than duplicating them.
