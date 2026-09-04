# Importing models

Imports bring upstream files into the CAS as canonical, verifiable
artifacts (spec 001 §8). One rule set for all sources — local
directories, Hugging Face repos, BitTorrent swarms — so the same
upstream bytes always produce byte-identical manifests (import
convergence).

## The pipeline (same for every source)

1. **Classification** — default deny: every upstream file is matched
   against the recognition rules below; anything unrecognized is
   **skipped and counted** (`skipped` in the job result), never an
   error. Licenses are redirected into manifest annotations.
2. **Eligibility gate** — an import with no recognized weights file
   fails closed (`E_NOT_IMPORTABLE`). A repo of only tokenizers is not
   a model.
3. **Ingest** — every file is written through the CAS verifying path:
   content-addressed by SHA-256, re-hashed on write. Tampered bytes
   never enter the store.
4. **Sealed artifacts** — deterministic manifest + model-index +
   distribution record construction. Same bytes in, same digests out.

## What the classifier recognizes

| Upstream shape | Becomes |
| --- | --- |
| `*.gguf` (not `mmproj*`/`imatrix*`) | weights.gguf entry; split suffix `-00001-of-00003` (also `_00001_of_00003`) → parts; filename quant token (dash-separated suffix run, e.g. `model-q8_0.gguf` → `q8_0`) |
| `*.safetensors` | weights.safetensors entry; standard shard numbering → parts |
| any file with `imatrix` in the name (incl. `*.gguf`) | weights.aux (imatrix) |
| any `mmproj*` file (incl. `*.gguf`) | weights.aux (vision-projector) |
| `model.safetensors.index.json` | weights.aux (weights-index) |
| `adapter_model.safetensors` + `adapter_config.json` | one adapter tar per directory (both required — incomplete pairs are skipped with a warning) |
| `tokenizer.json`, `tokenizer_config.json`, `special_tokens_map.json`, `added_tokens.json`, `preprocessor_config.json`, `processor_config.json`, `vocab.*`, `merges.*`, `*.model` (sentencepiece) | one deterministic `tokenizer.tar` |
| `chat_template.jinja` / `chat_template.json` / `chat_template` | chat-template entry (0..1; with several, the `.jinja` wins, others skipped + warned) |
| `modeling_*.py`, `tokenization_*.py`, `configuration_*.py`, `*_processor.py` | code entries (role model/tokenization/configuration/processor) |
| root `config.json` | the upstream config — feeds the quant derivation chain and the model config; never a manifest entry itself |
| license files (`LICENSE`, `COPYING`, …) | manifest annotations (SPDX detected where possible), not entries |

Skipped and counted (001 §8.2): training artifacts (`checkpoint-*/`,
`rng_state*.pth`, optimizer/scheduler state), repo housekeeping
(`README*`, `.gitattributes`, `.pyc`, `wandb/`, `runs/`, `logs/`,
`assets/`, `__pycache__/`, `benchmarks/`, `.eval_results`,
`generation_config.json`), nested `config.json` (anywhere but the
import root), `non_lora_trainables.bin` (explicit warning), and
everything unrecognized.

## How the quant is derived

Per artifact, first match wins (000 Appendix A vocabulary, no local
override):

1. **Filename token** — the longest dash-separated suffix run of the
   GGUF base name that is valid quant syntax (prefix `q`/`iq`/`tq`/
   `ud-`/`bf`/`f`/`fp`/`mx` + a digit, ≤ 24 chars): `model-q8_0.gguf`
   → `q8_0`, `Qwen3-8B-ud-q4_k_m.gguf` → `ud-q4_k_m`.
2. **Upstream config** — `quantization_config.quant_method`/`format`
   from the root `config.json`, when in vocabulary.
3. **Dominant dtype** — the safetensors dtype covering the most weight
   bytes: `BF16→bf16`, `F16→f16`, `F32→f32`.
4. Otherwise: `raw`.

## Local imports

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/local \
  -d '{"paths":["/data/qwen-gguf"],"as":"gold/qwen-gguf"}'
```

Rules:

- `as` is mandatory and names the target namespace: `ns/name`.
- Paths may be files or directories; a directory contributes every
  regular file beneath it (artifact name = relative path). Duplicate
  names across paths are rejected.
- **Regular files only** — the import root is a hard boundary. Symlinks
  (even pointing back inside the root), FIFOs, devices, and sockets
  fail the whole import with `E_SOURCE_NOT_REGULAR` before any byte is
  read; nothing is written, no state is touched. The check holds at
  read time too: a file swapped for a symlink mid-import is caught, not
  followed.
- A root `config.json` that exists but cannot be read fails the import
  (loudly) — a wrong config would silently produce a wrong manifest.

## Hugging Face imports

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/hf \
  -d '{"repo":"unsloth/Qwen3-8B-GGUF","revision":"refs/pr/2"}'
```

- `revision` optional; defaults to the repo's default branch. The
  listing resolves it to a **commit SHA and every file byte is fetched
  at that SHA** — a branch moving mid-import cannot mix commits.
- Namespace = lowercased repo id (`unsloth/qwen3-8b-gguf`); the
  original repo id and pinned revision are recorded in the manifest
  annotations.
- Anonymous access works for public repos; `SHARDR_HF_TOKEN` in the
  daemon's environment unlocks gated repos.
- An HF import fetches **the whole repo** (every file the listing
  returns) — mind the size for multi-quant repos.
- Upstream URL path segments are escaped per segment — hostile repo or
  file names cannot smuggle path traversal or query syntax into HF
  requests.

## BitTorrent imports

```sh
curl -s --unix-socket "$S" -X POST http://localhost/v1/import/bt \
  -d '{"infohash":"btmh:1220…","manifestDigest":"sha256:…","webseeds":["http://peer:port"]}'
```

- **Pin-mandatory:** `manifestDigest` is required; `magnet` or
  `infohash` alone is rejected (`E_BAD_REQUEST`). The pin is the trust
  anchor — the swarm only decides *where* bytes come from, never *what*
  they are. Every fetched byte is verified against the pinned manifest
  on write; a mismatch fails the import (`E_NOT_IMPORTABLE`).
- `magnet` and `infohash` are mutually exclusive. Discovery hints:
  **at least one `webseeds` entry is required for `/v1/import/bt` in
  this build** (the piece layers are fetched over HTTP first; v1
  bootstrap, 004 §5) — trackers and `peers` are additional hints,
  recorded as untrusted operational data. DHT-only discovery applies
  to `/v1/ensure` refills of already-known artifacts, not to first
  contact.
- The daemon's swarm client must be enabled (`[swarm] enabled = true`,
  the default) — else 501 `E_NOT_IMPLEMENTED`.
- A BT import delivers the pinned artifact's blobs. It does **not**
  create a namespace index: `name:quant` references need a local/HF
  import on that node; `@sha256:<manifest>` works immediately.

The infohash to pin comes from the publisher's distribution record
(`torrent.infohash` in the record blob in their CAS).

## Eligibility — what gets rejected, and why

| Rejection | Class | Why |
| --- | --- | --- |
| No recognized weights file | `E_NOT_IMPORTABLE` | eligibility gate: not a model, fail closed |
| Symlink/FIFO/device in or at the import root | `E_SOURCE_NOT_REGULAR` | hard boundary — no followed bytes, ever |
| Unreadable root `config.json` | import error (`E_INTERNAL` job) | a wrong config would silently skew the quant chain |
| Corrupt current model-index at merge time | `E_INVALID_INDEX` | loud, state untouched — never silently rebuilt |
| HF repo not found / gated without token / rate-limited | `E_UNKNOWN_REF` / `E_SOURCE_FORBIDDEN` / `E_RATE_LIMITED` | upstream verdicts, surfaced verbatim |
| Swarm bytes ≠ pin | `E_NOT_IMPORTABLE` / `E_SOURCE_UNAVAILABLE` | fetched blobs fail the verify-write or the torrent identity does not bind to the pin → `E_NOT_IMPORTABLE`; a manifest-pin mismatch during the fetch phase → `E_SOURCE_UNAVAILABLE` |

Being **skipped** (unrecognized files, nested configs, training
artifacts) is not an error — it is counted in `result.skipped` and, where
it matters, called out in `result.warnings`.
