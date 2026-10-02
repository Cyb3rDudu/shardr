# Swarm & sync operation

What synchronization means operationally for a shardhive node. The
canonical contract is
[spec 004](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/004-torrent-mapping.md);
user-facing defaults are in
[Swarm & seeding](../user/swarm.md), configuration keys in the
[configuration reference](../references/configuration.md#swarm-the-swarm-client-daemon).

## What the swarm is

Every artifact in the CAS is mapped to a BitTorrent v2 torrent
(distribution record in `state/distribution.json`). A node with the
swarm enabled (the default) seeds every locally-complete artifact and
can fill missing blobs of artifacts it has references for. "Usage is
replication": a node that merely *uses* a model — the runner mmaps the
same CAS bytes the torrent client reads — is already a seeder; there
is no separate upload copy of the data.

Operational consequences:

- **Serving is seeding.** `shardr run` does not upload anything by
  itself, but the daemon reads blobs for the runner from the same
  files the swarm offers to peers. One copy per machine (003 §5).
- **Seed-start re-hash.** Before a blob is first offered to the swarm
  in a process's lifetime, it is fully re-hashed (cached per process).
  Daemon startup with a large store therefore costs one full read
  pass; the escape hatch `--seed-no-verify` skips it and is documented
  unsafe (003 §4).
- **Seed-by-default across restarts.** Each `serve` start rejoins the
  swarms of every complete artifact in state (synchronous; the
  message is printed once — executed):

```console
$ shardhive serve
shardhive: swarm: seeding 1 artifact(s)
shardhive 0.0.1-dev listening on /var/folders/…/T/shardhive-501/shardhive.sock
```

## Configuration bounds

`[swarm]` keys (validated fail-closed by the daemon; defaults:
enabled, seeding, DHT on, unlimited upload):

| Key | Default | Operational meaning |
| --- | --- | --- |
| `enabled` | `true` | master switch; off disables fills, `/import/bt`, and catalog pulls loudly |
| `seed` | `true` | offer complete artifacts to peers |
| `upload_limit` | `0` (unlimited) | bytes/second, shared budget across the torrent client **and** the webseed listener — per node, not per transport |
| `dht` | `true` | DHT peer discovery |
| `no_seed_verify` | `false` | skip the seed-start re-hash (unsafe) |
| `webseed_addr` | `127.0.0.1:0` | webseed HTTP bind; the ephemeral port is announced to peers as a source hint |

Upload limiting is a good-citizen budget: it bounds how much store
bandwidth the swarm may consume, it does not throttle the runner.

## Two swarms, two budgets

A node participates in two distinct swarm populations:

1. **The shardr swarm** (spec 004): BitTorrent v2 torrents of your own
   imported artifacts, budget `[swarm] upload_limit`.
2. **Community catalog swarms** (pirateface pulls): foreign
   BitTorrent v1 torrents the node joined by pulling a listing; after
   the pull the handle stays alive and seeds from CAS bytes
   (good-citizen mode), budget `[catalog] upload_limit` — which
   **inherits** `[swarm] upload_limit` when unset. The budgets are
   separate so community seeding never starves (or hides behind) your
   own swarm traffic.

The catalog side is documented in [Catalog pulls](../user/catalog.md);
this page deliberately does not duplicate it.

## Fills

`POST /v1/ensure` (CLI: `shardr pull ns/name:quant`) starts a fill
job when the manifest is local but blobs are missing: source hints
recorded at import time (trackers, webseeds, peers — untrusted
operational data, 004 §4) plus DHT discovery. Fetched bytes go through
the verifying write path like any other write; a mismatch refuses the
blob (`E_NOT_IMPORTABLE`). Without hints, discovery is DHT-only.
Progress is per-file on the job (`filesDone`/`filesTotal`); terminal
states are final.

## Environment notes (observed)

- On networks without UPnP, the torrent client logs a port-mapping
  warning at startup (`AddPortMapping: 500`) and continues — peer
  connectivity falls back to outbound connections and the tracker/DHT.
  Harmless for operation; NAT port forwarding improves inbound peer
  reachability.
