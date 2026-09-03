# 004 — Swarm (BitTorrent v2) & Configuration

Status: **Candidate v1 (surface)** — machine-checked by `docs/specs/vectors/004-torrent.jsonl`

## 1. Purpose

How artifacts map onto BitTorrent v2 swarms: torrent construction,
verification, magnets and shardrbay's role, and shardhive's swarm
client. The swarm turns individual shardhives into one common
repository: every shardhive fills from it automatically and seeds what
it holds. BitTorrent is meant to be invisible — `shardr run` just works,
and where the bytes came from is shardhive's business.

## 2. Principles

1. **One torrent per artifact** (per manifest digest). One swarm per
   model version; clients fetch selectively (BTv2 file selection).
2. **Dual hash spaces**: the canonical digest everywhere is flat
   `sha256:` (001); BTv2 merkle roots are pinned per file entry. Shared
   math (SHA-256), separate address spaces — standard verification
   everywhere, streaming piece verification in the swarm.
3. **Determinism is law**: the torrent is a pure function of the
   manifest. Same manifest → same infohash worldwide → swarm convergence
   without coordination (import convergence, 001 §7.5).
4. **Original filenames in the torrent tree**: file paths are the
   entries' `name` values (001 §3.1 rule 1) — third-party BitTorrent
   clients download real, usable model files and can seed them
   unmodified. The community mirror grows beyond the shardr install
   base.

## 3. Torrent construction (deterministic)

For an artifact with manifest digest `D`:

- **info dict** (BEP 52, `meta version = 2`): `name =
  shardr-sha256-<first 12 hex of D>`; file tree = every file entry at
  its `name` path, plus the manifest itself at `manifest/sha256-<hex>`;
  per-file roots from entry annotations; `piece length` per ladder.
- **Piece-length ladder** (pure function of total size): ≤ 1 GiB →
  1 MiB; ≤ 8 GiB → 4 MiB; ≤ 64 GiB → 16 MiB; ≤ 512 GiB → 64 MiB;
  > 512 GiB → 256 MiB (16 KiB leaves per BEP 52). Keeps piece counts in
  the 10²–10⁴ band — frontier checkpoints are multi-TB. A fixed ladder
  (not publisher choice) preserves determinism.
- **Merkle computation** (binding): 16 KiB leaves, SHA-256; internal
  nodes = SHA-256(left‖right); incomplete levels padded per the BEP 52
  zero-hash chain. Entry `bt.merkleRoot` MUST match. Piece layers (files
  with piece length > 16 KiB) form one bencoded blob.
- **Verification against the distribution record** (001 §6): the client
  reconstructs the info dict from the manifest, computes the infohash,
  and MUST find it equal to `distribution.torrent.infohash`; the
  piece-layers blob MUST hash to `distribution.torrent.pieceLayersDigest`.
  This is the binding between the digest world and the torrent world —
  it lives outside the manifest by construction.

All BT inputs are computed **at import time** (001 §8) and written to
the distribution record — every imported artifact is swarm-ready the
moment it exists.

## 4. shardrbay (discovery index)

shardrbay entries carry:

- the reference, manifest digest, and **distribution record** (digest +
  content),
- **source hints**: trackers, webseeds, mirrors — untrusted operational
  data, explicitly outside the identity-bearing record (001 §6),
- **verified sidecars** (each digest-checked against the record/manifest):
  the manifest document, the model config, the **piece-layers blob**, and
  optionally a `.torrent` metainfo file,
- size, file count, license, seed/leech counts, first-seen.

Trust position, precisely: shardrbay is **not** in the byte-integrity
trust path — every byte is digest-verified regardless of source. In v1
it **is** in the name-authenticity trust path (005 §6), unless the
client uses `@digest` or local pins. It can be stale or wrong;
correctness of bytes is unaffected, name freshness is not.

## 5. Swarm client (shardhive, day one)

- Library: `anacrolix/torrent` (pure Go, in production since 2014; BTv2,
  DHT, PEX, uTP, WebSeeds, holepunching; streaming readers; pluggable
  storage). shardhive uses a **CAS-backed storage driver** so the swarm
  writes directly into the verifying store — no second copy.
- **Piece-layers acquisition** (verified against
  `distribution.torrent.pieceLayersDigest`, always): (1) resolver
  envelope sidecar (§4), (2) a webseed well-known path
  (`/bt/piece-layers/<digest>`), (3) a `.torrent` metainfo file (BEP 52
  carries the piece layers within), (4) peer metadata exchange. The blob
  is KB-to-MB scale; any source works because the digest gates it.

  **v1 bootstrap constraint (ruling 1b, 2026-09-02):** `/import/bt`
  acquires the manifest and piece layers over the **webseed channel
  before any swarm join**, pin-checked against the pinned manifest
  digest — identity is untrusted until the pin is satisfied, and an
  infohash-only join has a metadata window where BTv2 multi-piece
  content is hashless. Sources (3) and (4) — `.torrent` metainfo and
  peer metadata exchange — remain valid acquisition paths for later
  versions (still digest-gated); v1 requires at least one webseed hint.
  Bulk data always flows over the peer protocol (the upstream webseed
  download planner panics on v2 padding chunks).
- **Fetch** (on `ensure` miss): reconstruct the torrent from the
  manifest + distribution record → join (DHT + PEX + source-hint
  trackers) → set selective priorities for missing blobs (config and
  manifest first — the metadata phase) → verify every piece (merkle) on
  arrival → CAS verify-write (003 §3).
- **Seed**: default **on** — shardhive seeds every complete artifact it
  holds. Usage is replication; this is the community mirror.
  Configurable off, rate-limited.
- **HTTP fallback**: HF resolve endpoints (range-capable) serve as byte
  sources for HF imports; any source-hint webseed likewise. Webseed
  bytes verify exactly like peer bytes — no transport is ever trusted,
  only digests are. There is no mandatory central origin; availability
  comes from community seeders plus upstream HF.
- Streaming serve-before-complete is not v1 — the CAS verify-write gate
  keeps the serve path simple and trusted.

## 6. End-to-end (`shardr run <ref>`)

1. Resolve `shardr:///` ref via shardhive (005 §3) → manifest.
2. Verify invariants (001 §7), including the distribution-record check
   (§3).
3. Ensure blobs: CAS hit → done; else swarm fill (§5), automatically,
   within the same command.
4. The runner mmaps from the CAS (zero-copy, 002 §4).

## 7. Configuration (v1, minimal — one file, documented defaults)

`~/.config/shardr/config.toml`. Model keys use the scheme-less path
form `ns/name:quant` (CLI short form, 000 §2):

```toml
[swarm]
enabled = true            # shardhive swarm client (fetch + seed)
seed = true               # seed complete artifacts (the community mirror)
upload_limit = 0          # bytes/sec, 0 = unlimited
dht = true                # DHT + PEX

[references]
# Interactive-CLI comfort ONLY: applied when a human types a
# selector-less ref. Never applied in Modelfiles, the API, manifests,
# or shardrbay entries — those always require an explicit selector.
default_selector = ""

[runtimes.llama]          # overlay layer 2 (002 §2)
n_threads = 8

[models."unsloth/qwen3.8-27b-gguf:ud-q4_k_m"]   # per-model overlay
[models."unsloth/qwen3.8-27b-gguf:ud-q4_k_m".llama]
n_gpu_layers = 40
```

The quant vocabulary is protocol-global (000 Appendix A) and has **no
local override** — the reference grammar must parse identically on every
node. The config surface stays minimal: local-node knobs only (limits,
on/off); protocol-affecting knobs do not exist.
