# 002 — Runner, Modelfile & Runtime Configuration

Status: **Draft v0** — vectors pending: Modelfile grammar, LABEL duplicates, config overlays, runtime allowlist

## 1. Purpose

Three contracts in one document:

1. the **Modelfile** — declarative *content composition* recipe
   (FROM + adapters + template + system prompt),
2. the **runtime configuration model** — layered overlays (advisory
   defaults → user config → `--config` file → CLI flags),
3. the **Model Contract** — what a runtime must fulfill to execute a
   model, plus the serving API toward proxies (llama-swap) and users.

Design principle: **runtime configuration is a property of the machine
and the user, never of the artifact.** Artifacts that embedded user
runtime config would multiply digests per GPU tweak, invent a second
addressing dimension, and still not run unconfigured. Configuration
resolves at run time from overlays; the Modelfile composes content only.

## 2. Runtime configuration (layered overlays)

### 2.1 Layers (lowest → highest precedence)

| # | Source | Where | Nature |
| --- | --- | --- | --- |
| 1 | **Advisory defaults** | optional `runtime-config` entries inside any artifact (001 §3.1) | publisher/importer-provided, **machine-neutral only** (`ctx_size`, `jinja` — never `n_gpu_layers`) |
| 2 | **User config** | `~/.config/shardr/config.toml` (004 §7): `[runtimes.<id>]` global, `[models."<ref>"]` per model | local, never distributed |
| 3 | **`--config <file>`** | per-invocation TOML file, same schema as layer 2 | the natural home for per-model setups, versionable next to the models |
| 4 | **CLI flags** | `shardr run … --set <dotkey>=<value>` (repeatable) | highest precedence |

### 2.2 Merge semantics

- Flat scalar maps per runtime id; a higher layer **replaces individual
  keys** — no cross-key inference, no nesting magic.
- All keys in every layer are validated against the target runtime's
  allowlist (§7.1). An unknown key anywhere is a loud error at run time
  (fail fast; no silent drops).
- No layer provides a key → the runtime's built-in default applies.

### 2.3 Effect

`shardr run <ref>` works with **zero configuration**: advisory defaults
(if shipped) → user config (if set) → runtime built-ins. A freshly
imported `:q8_0` runs out of the box. There are no config tags and no
config-only builds.

## 3. Modelfile (content composition)

### 3.1 Principles

- **Declarative only.** No `RUN`, no shell, no filesystem reads; anything
  outside the grammar is a build error. The grammar is the security
  boundary — Modelfiles are remote content by default.
- **Content, not configuration.** A Modelfile composes a new model from
  existing content: base weights, adapters, template, system prompt. It
  carries no runtime tuning (§2 owns that). A build that would change no
  content (no `ADAPTER`, `TEMPLATE`, or `SYSTEM`) fails: *nothing to
  build — use overlays*.
- **Reproducible builds.** Same inputs (refs pinned to digests) →
  byte-identical manifest: canonical JSON (001 §3), canonical entry
  ordering (001 §3.1 rule 4), no implicit timestamps.
- **No local path escape:** `FROM`/`ADAPTER` take `shardr://` references;
  other instructions take literal values. No instruction reads the build
  host's filesystem.

### 3.2 Lexical rules

UTF-8, no BOM; case-insensitive instruction keywords (uppercase is the
mandated style); `#` comments run to end of line; line-based parsing
except triple-quoted blocks; no implicit trimming. Terminals:

- `ws` := one or more spaces or tabs
- `quoted1` := `"` … `"` — single-line JSON string (RFC 8259)
- `block3` := `"""` … `"""` — raw multi-line text, terminated by a line
  containing only `"""`; the delimiters cannot appear inside
- `rkey` := `[a-z0-9]([a-z0-9.-]*[a-z0-9])?` (reverse-DNS style
  annotation keys)
- `jsonscalar` := JSON string, number, `true`, `false`, or `null`
- `text` := any UTF-8; no filesystem or shell semantics

### 3.3 Grammar

```
modelfile  := instruction+
instruction := "FROM" ws ref                    ; exactly one, first
            | "ADAPTER" ws ref                  ; 0..n
            | "TEMPLATE" ws block3              ; 0..1
            | "SYSTEM" ws (quoted1 | block3)    ; 0..1
            | "LABEL" ws rkey ws jsonscalar     ; 0..n
ref       := shardr:// reference per 000
block3    := '"""' …text… '"""'
```

Duplicate `LABEL` keys are a build error — one instruction, one value,
loudly. (`LABEL` may appear any number of times, but never twice with
the same key.)

### 3.4 Build semantics (`shardr build`)

1. **Parse** grammar; reject content-free builds (§3.1).
2. **Resolve & pin**: `FROM`/`ADAPTER` resolved via shardhive (`FROM`
   MUST resolve to a concrete manifest; an index without quant selection
   is a loud error listing available selectors). `FROM` MAY reference
   another model image — composition layers, like docker layers. The
   base digest is recorded as `io.shardr.base.digest`.
3. **Ensure blobs** in the shardhive CAS (missing content is imported /
   swarm-fetched automatically — resolution order per 005 §3).
4. **Compile config**: base config; `template.override` ← `TEMPLATE`;
   `system` ← `SYSTEM`; `adapters` = base adapters + declared (order
   preserved). Serialize canonically (001 §3.2).
5. **Assemble entries**: base entries + adapter entries (+ template/
   system handling), canonical order (001 §3.1 rule 4). Advisory
   `runtime-config` entries are **inherited unchanged** — builds do not
   tune.
6. **Emit manifest** (canonical bytes) and record it in shardhive.

The output **model image** is a normal artifact (001) whose content was
composed from a Modelfile. `shardr run` accepts any artifact; the
Modelfile is never required at run time.

## 4. Lifecycle (`run` / `serve` / `stop`) — and zero-copy

- `shardr run <ref>` — foreground: resolve `shardr://` ref via shardhive
  (005) → `ensure` (CAS hit, else import/swarm fill, in that order) →
  merge overlays (§2.2) → spawn runtime → attach signals.
- `shardr serve <ref> [--id <name>]` — daemonized instance with a stable
  id; registers its OpenAI endpoint.
- `shardr stop [<id>|--all]` — SIGTERM, clean exit within 30 s, SIGKILL
  after (supervisor duty, §5 spawn interface).
- **Zero-copy is law.** The runner never materializes model copies.
  Weights are accessed **directly from the CAS** — shardhive hands the
  runner the resolved blob paths (005 §3), and the runtime mmaps them.
  CAS blobs are immutable by contract and mode 0444 — which is
  permission hygiene that keeps mmap race-free, not a security boundary
  of its own; trust always comes from the digest and re-hashing (003 §4).
  llama.cpp loads GGUF via mmap anyway. Multiple served models share
  the same pages; swarm seeding reads the same files. **One copy per
  machine, owned by shardhive.** KB-scale metadata (manifests,
  configs) may be cached by clients; TB-scale blobs never.
- llama-swap (or any OpenAI client) fronts the served endpoints; routing
  by model id (§6).

## 5. Model Contract (runtime interface)

A runtime `R` is **shardr-compliant** iff:

1. **Format declaration**: `R` declares accepted weight formats (v1:
   `llama` → `gguf`).
2. **Config consumption**: only merged-overlay keys from its validated
   allowlist (§7.1); keys neither overlaid nor defaulted fall back to
   runtime built-ins.
3. **Spawn interface**: arguments derived **only** from the merged
   config via the key→flag mapping (§7.1); weights passed as **CAS paths
   for direct mmap** (zero-copy, §4); binds `127.0.0.1` (v1), port
   auto-selected; `/health` liveness; readiness = first 200 on
   `GET /v1/models`; clean SIGTERM exit within 30 s.
4. **Template resolution order**: explicit model-config template →
   chat-template entry (001) → format-internal (GGUF-embedded) → runtime
   default. First hit wins.
5. **No side effects**: CAS blobs opened read-only, mmap-only.
6. **Fail fast**: impossible configurations terminate non-zero with the
   reason on stderr. No silent degradation.
7. **Aux consumption** (001 §3.1): `vision-projector` → `--mmproj`
   (default variant `f16`, key `mmproj_variant`); `mtp` reserved;
   `imatrix` never consumed at runtime; `weights-index` for safetensors
   runtimes; unknown roles ignored with a log line.
8. **Code-entry trust gate**: code entries (001 §3.1) are **never
   executed** without explicit opt-in (`trust_custom_code = true` /
   `--trust-custom-code`), default off, with a loud warning naming the
   artifact and present code roles. A remote artifact carrying
   executable files is remote code execution by design — the format
   carries the bytes, execution stays a local, explicit decision.
   Runtimes that cannot execute code at all (`llama`, native GGUF only)
   ignore code entries entirely.

## 6. Serving API contract

- `GET /v1/models` — the served model, `id` = reference (000).
- `POST /v1/chat/completions` (+ SSE streaming via `stream: true`).
- `POST /v1/completions` (+ SSE streaming).
- Unknown request parameters → HTTP 400. Silent param dropping produces
  configuration illusions; honest errors keep proxies and clients
  debuggable.

## 7. Runtime registry

### 7.1 `llama` (v1 target: llama-server-impl embedded)

Key allowlist v1 (overlay-validated; mapped to llama-server flags):

| Key | Type | Flag |
| --- | --- | --- |
| `n_gpu_layers` | int | `-ngl` |
| `ctx_size` | int | `-c` |
| `n_threads` | int | `-t` |
| `flash_attn` | bool | `-fa` |
| `mlock` | bool | `--mlock` |
| `kv_cache_type` | string | `--cache-type-k` |
| `batch_size` | int | `-b` |
| `ubatch_size` | int | `-ub` |
| `n_parallel` | int | `-np` |
| `jinja` | bool | `--jinja` |
| `mmproj_variant` | string | (aux selection, §5 aux consumption) |

### 7.2 Reserved ids

| Id | Status | Formats | Notes |
| --- | --- | --- | --- |
| `mlx` | reserved | safetensors (MLX) | weights-index; code behind trust gate |
| `vllm` | reserved | safetensors | weights-index; code behind trust gate |

Runtime ids are lowercase and dot-free — they survive as dot-key
prefixes in overlay layers 2–4.
