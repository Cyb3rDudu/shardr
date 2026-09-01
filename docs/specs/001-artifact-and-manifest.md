# 001 — Artifact Format

Status: **Draft v0**

## 1. Purpose

Defines what a shardr **model artifact** is: the immutable unit that
shardhive stores, the swarm distributes (004), and runtimes execute
(002). Native JSON manifests, deterministically serialized, content-
addressed by flat SHA-256. Standard container-registry formats are not
adopted — models are multi-GB, torrent-distributed, quant-indexed
content, and the format is designed for exactly that.

## 2. Artifact kinds

- **model artifact** — blobs + manifest (§3), identity = manifest digest;
- **model index** — per-quant family grouping (§5);
- **distribution record** — torrent binding for an artifact (§6).

All three are content-addressed JSON documents in the CAS.

## 3. Manifest (native JSON, canonical)

```json
{
  "schemaVersion": 1,
  "artifactType": "model",
  "files": [
    {
      "kind": "config",
      "digest": "sha256:…",
      "size": 1234,
      "name": "modelconfig.json",
      "bt": { "merkleRoot": "…" }
    },
    {
      "kind": "weights.gguf",
      "digest": "sha256:…",
      "size": 17500000000,
      "name": "qwen3.8-27b-q8_00001.gguf",
      "part": 1,
      "bt": { "merkleRoot": "…" }
    }
  ],
  "annotations": {
    "io.shardr.hf.repo": "unsloth/Qwen3.8-27B-GGUF",
    "io.shardr.hf.revision": "4ca7207…"
  }
}
```

**Canonicalization:** RFC 8785 (JSON Canonicalization Scheme) by
reference. Conformance test vectors (manifests, indexes, distribution
records) accompany this spec before it advances to Candidate v1 —
byte-stable hashing across implementations is a protocol requirement,
not an implementation detail.

**The manifest contains no torrent-level hashes of itself** — no
infohash, no piece-layers digest. The torrent binds to the manifest via
the separate distribution record (§6); embedding self-referential
torrent data here would create a fixed-point problem.

### 3.1 File kinds

| Kind | Card. | Notes |
| --- | --- | --- |
| `config` | exactly 1 | the model config document (§3.2), canonical name `modelconfig.json` — a normal torrented blob |
| `weights.gguf` | 1..n | one file per blob; split shards carry `part` (contiguous 1..n) |
| `weights.safetensors` | 1..n | standard shard numbering → `part` |
| `weights.aux` | 0..n | companions; `role` ∈ {`vision-projector`, `mtp`, `imatrix`, `weights-index`, …} + optional `variant` |
| `tokenizer` | 0..1 | deterministic uncompressed tar (sorted names, mtime 0, uid/gid 0, mode 0644) |
| `chat-template` | 0..1 | single text blob |
| `adapter` | 0..n | deterministic tar (`adapter_model.safetensors` + `adapter_config.json`); `adapterType` ∈ {`lora`} |
| `runtime-config` | 0..n | **advisory defaults only**, machine-neutral; `runtime` ∈ {`llama`, `mlx`, `vllm`}; ≤ 1 per runtime id |
| `code` | 0..n | `codeRole` ∈ {`modeling`, `configuration`, `tokenizer`, `processor`, `other`} — data only, execution gated (002) |

Every file entry MUST carry `kind`, `digest`, `size`, `name`, and (for
all torrented blobs) `bt.merkleRoot`. Rules:

1. `name` = artifact-relative canonical filename, `/`-separated, no
   leading `/`, no `..`, unique. Torrent file trees are built from these
   names (004 §3) — original filenames make torrents directly usable and
   seedable by third-party BitTorrent clients.
2. One weights format per artifact (no gguf+safetensors mixing).
3. Aux attachment: **quant-specific** companions join only the matching
   member; **quant-agnostic** companions (mmproj projectors, imatrix,
   `model.safetensors.index.json`) join every member. CAS dedup makes
   per-member attachment free, and every member torrent seeds them.
4. Canonical file order (determinism): config, weights (by `part`, then
   digest), weights.aux (by `name`), tokenizer, chat-template, adapters
   (digest), runtime-config (runtime id), code (role, then name).

### 3.2 Model config (`config` file entry)

JSON, canonical, ≤ 256 KiB:

```json
{
  "family": "qwen3", "weightsFormat": "gguf", "quantization": "q8_0",
  "selfContained": false, "contextLength": 262144,
  "license": "apache-2.0",
  "source": { "hf": { "repo": "…", "revision": "…" } },
  "template": { "override": null }, "system": null,
  "adapters": [], "runtimes": { "llama": { "ctx_size": 262144 } }
}
```

`runtimes` holds **advisory defaults** only (002 §2). Unknown fields are
preserved on round-trip (forward compatibility).

## 4. Annotations

| Annotation | Requirement | Content |
| --- | --- | --- |
| `io.shardr.hf.repo` / `io.shardr.hf.revision` | on HF imports | provenance (original-case repo id; pinned commit SHA) |
| `io.shardr.license` (+ `io.shardr.license.name`) | optional | SPDX where possible, else raw string + upstream name |
| `io.shardr.import.specVersion` | on imports | classification-ruleset version (convergence invariant §7.5) |
| `io.shardr.import.skipped` / `io.shardr.import.warnings` | on imports | counts / noted anomalies from classification |

## 5. Model index (quant families)

```json
{
  "schemaVersion": 1,
  "artifactType": "model-index",
  "members": [
    { "manifest": "sha256:…", "quant": "q8_0",
      "weightsFormat": "gguf", "revision": "4ca7207…" }
  ]
}
```

- Member `quant` values are unique; members point at model manifests,
  never at other indexes.
- Every repo has a **current index** (the latest import), updated
  atomically in shardhive state; quant-only selectors resolve against it
  (005 §6).
- Indexes are content-addressed documents: immutable, digest-identified,
  stored in shardhive state, never torrented — they are name-resolution
  metadata above the swarm's per-artifact torrents.

## 6. Distribution record

Binds a manifest to its torrent — outside the manifest itself (§3):

```json
{
  "schemaVersion": 1,
  "artifactType": "distribution",
  "manifestDigest": "sha256:…",
  "torrent": {
    "infohash": "btmh:1220…",
    "pieceLength": 16777216,
    "pieceLayersDigest": "sha256:…"
  }
}
```

- Content-addressed like every artifact document; its own digest is what
  shardrbay entries and peers exchange.
- **Trackers and webseeds are deliberately absent**: they are operational
  data that changes without changing artifact or torrent identity. They
  travel as untrusted **source hints** in shardrbay entries or a plain
  envelope, never inside an identity-bearing record.
- Verification rule: given the manifest, a client reconstructs the info
  dict (004 §3), computes the infohash, and MUST find it equal to
  `torrent.infohash`; the piece-layers blob hashes to
  `torrent.pieceLayersDigest`.

## 7. Invariants

1. **Digest correctness**: every digest is flat SHA-256 of the referenced
   bytes; a mismatch anywhere is a fatal integrity event, never a cache
   miss.
2. **Immutability**: digest-addressed content never changes.
3. **Kind-driven interpretation**: clients reject artifacts whose
   required kinds (for their runtime) are unknown.
4. **Torrent-reconstructable**: everything 004 needs to build the info
   dict — file names, sizes, merkle roots, piece-length input — is
   pinned in the manifest. The binding back-reference lives in the
   distribution record, never in the manifest.
5. **Import convergence**: same upstream revision + same
   `io.shardr.import.specVersion` → byte-identical manifests → identical
   infohashes → the same swarm, regardless of who imported. This is what
   makes a community mirror possible without coordination.

## 8. Imports (shardhive capability)

Three sources — **local**, **HF**, **BT** — one rule set:

1. **Eligibility gate (fail closed)**: after skip rules, ≥ 1 recognized
   weights file (`.gguf` / `.safetensors`) or ≥ 1 torrent to fetch.
   `.pt`/`.pth`/`.bin` are **never** weights — pickle-based formats are
   an execution vector, and shardr does not execute.
2. **Default-deny classification**: a file becomes a manifest entry only
   on a recognized pattern; everything else is skipped and counted.
   Skipped by rule: README, `.gitattributes`, license files (→
   annotations), `generation_config.json`, `assets/`, `.eval_results/`,
   benchmarks, metric curves, `__pycache__/`, `*.pyc`, and training
   artifacts (`checkpoint-<digits>/`, `training_state/`, `wandb/`,
   `runs/`, `logs/`, `optimizer.pt`, `scheduler.pt`, `rng_state*.pth`,
   `trainer_state.json`, `training_args.bin`).
3. **Classification table**: `*.gguf` (split pattern → `part`) →
   `weights.gguf`; `*.safetensors` → `weights.safetensors`; PEFT pair
   (`adapter_model.safetensors` + `adapter_config.json`) → `adapter`
   tar; `chat_template.jinja` / `chat_template.json` / embedded
   `chat_template` → `chat-template`; tokenizer set (`tokenizer.json`,
   `tokenizer_config.json`, `vocab.*`, `merges.*`,
   `special_tokens_map.json`, `added_tokens.json`, `*.model`,
   `preprocessor_config.json`, `processor_config.json`) → `tokenizer`
   tar; mmproj / imatrix / `model.safetensors.index.json` →
   `weights.aux`; custom-code `*.py` (`modeling_*`, `tokenization_*`,
   `configuration_*`, `*_processor*`) → `code`.
4. **Quant derivation chain**: filename token → `config.json`
   `quantization_config` (`quant_method`/`format`, lowercased —
   in-checkpoint quantization like fp8/mxfp4 carries no filename token)
   → dominant dtype from safetensors stats → `raw`.
5. GGUF imports set `selfContained: true` and emit no tokenizer/template
   entries — the runtime reads both from the GGUF itself. Advisory
   `runtime-config` is derived machine-neutral: `ctx_size` ←
   `context_length`, `jinja` ← template present.
6. **Local import** requires an explicit namespace (`--as ns/name`;
   never optional) and derives quants from filenames with the same
   chain.
7. **BT import** pins a manifest digest up front (from shardrbay, a
   peer, or any out-of-band source). The manifest is a file **inside**
   the torrent (`manifest/sha256-<hex>`): fetch it, flat-hash it, and
   require equality with the pinned digest, then parse it. Torrent
   metainfo alone is never sufficient to reconstruct a manifest — it
   lacks kinds, flat digests, config semantics, and provenance.
   Distribution-record fields (infohash, piece length, piece-layers
   digest) are verified against the fetched manifest per §6.
8. HF revision is pinned in annotations; anomalies (e.g. skipped
   `non_lora_trainables.bin`) are recorded in `io.shardr.import.warnings`.

The mirror mirrors **runnable model content**, not every upstream byte —
full byte-mirroring is explicitly out of scope.
