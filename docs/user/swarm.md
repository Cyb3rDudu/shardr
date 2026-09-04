# Swarm & seeding

shardhive embeds a BitTorrent v2 client (spec 004). It serves two roles:
**fetch** (fill in missing blobs for a pinned/known artifact) and
**seed** (upload what you already have). Seeding is on by default.

## What "seed" means

- Every complete artifact in your store is announced to its swarm, and
  other shardhive nodes can pull its pieces from you — over both the
  peer protocol and a plain-HTTP webseed listener the daemon runs on
  `127.0.0.1:<ephemeral>`.
- **Use is replication:** the moment you import or fetch a model, your
  node becomes a mirror for everyone else who wants it. There is no
  leech mode; that is the design (community mirror, 004 §5).
- Upload can be bounded: `[swarm] upload_limit = <bytes/sec>` in
  `config.toml` (`0` = unlimited, the default). Seeding can be turned
  off entirely with `[swarm] seed = false`; the swarm client itself can
  be disabled with `enabled = false`.

## When seeding starts

- **At daemon startup**, every complete artifact in the store is joined
  synchronously — this is the `shardhive: swarm: seeding N artifact(s)`
  startup line. Blobs are re-hashed first (CAS discipline, 003 §4);
  `--seed-no-verify` / `no_seed_verify = true` skips that re-hash and
  is documented-unsafe, never default.
- **After a swarm fill** completes on this node, the newly completed
  artifact is joined immediately.
- Artifacts imported **while the daemon is already running** (local/HF)
  join on the next restart. (Until then the blobs are in the CAS and
  verified — they just are not announced yet.)

## Fetching (ensure)

`POST /v1/ensure` with a local-manifest ref fills missing blobs from
the swarm:

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/ensure \
  -d '{"ref":"shardr:///gold/swarm-toy:q8_0"}'
```

Discovery prefers the source hints recorded at import time (trackers,
webseeds, peers — untrusted operational data); without hints it is
DHT-only. Fetched bytes are digest-verified on write: a peer sending
wrong bytes fails the fill (`E_NOT_IMPORTABLE`), never enters the CAS.

## The trust model in one paragraph

Digests instead of transports: every byte is verified against its
SHA-256 content address on the way in, no matter whether it arrived
from a local disk, HF, a webseed, or a peer. The swarm decides where
bytes come from; the CAS decides whether they are the right bytes.
Imports via BT additionally require a **manifest digest pin** before
joining a swarm (pin-before-join) — see
[importing.md](importing.md#bittorrent-imports).

## Config knobs

See [config.md](config.md#swarm--shardhives-section) for the full
`[swarm]` table: `enabled`, `seed`, `upload_limit`, `dht`,
`no_seed_verify`, `webseed_addr`.
