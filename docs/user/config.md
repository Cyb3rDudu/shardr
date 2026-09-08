# Configuration

One file, `config.toml`, all defaults documented. Location:

1. `$SHARDR_CONFIG` if set
2. `$XDG_CONFIG_HOME/shardr/config.toml`
3. `~/.config/shardr/config.toml`

No file = documented defaults. A file that is present but malformed, or
that contains an unknown `[swarm]` key, is a **loud error** — shardhive
refuses to start rather than silently ignore a typo'd knob.

The config surface is deliberately minimal: local-node knobs only.
There are no protocol-affecting knobs — the quant vocabulary (000
Appendix A) has no local override because the reference grammar must
parse identically on every node.

## Full example (spec 004 §7)

This exact file parses 1:1 against the production parser:

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

## `[swarm]` — shardhive's section

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | swarm client at all (fetch + seed) |
| `seed` | bool | `true` | seed complete artifacts to other peers |
| `upload_limit` | int | `0` | bytes/sec, `0` = unlimited; must be ≥ 0 |
| `dht` | bool | `true` | DHT + PEX peer discovery |
| `no_seed_verify` | bool | `false` | skip the seed-start re-hash (003 §4) — documented unsafe, never default; equivalent to `serve --seed-no-verify` |
| `webseed_addr` | string | `"127.0.0.1:0"` | webseed HTTP bind address (`:0` = ephemeral port); `""` disables the webseed listener (peer protocol still works) |

Rules the parser enforces:

- Sections, `key = value` with bool/int/string, `#` comments on their
  own line or inline after a value (double-quoted strings may contain
  `#`). No arrays, no nested tables beyond dotted section names — the
  config surface does not need them.
- Values: booleans must be literal `true`/`false`; `upload_limit` must
  be a decimal integer ≥ 0; strings are double-quoted.
- Keys inside `[swarm]` that are not in the table above are errors.
- Keys inside other sections are **not** validated by shardhive — those
  sections belong to other components.

## `[references]` — CLI-only comfort

`default_selector` applies **only** when a human types a selector-less
reference at the interactive CLI. It is never applied to API requests,
Modelfiles, manifests, or shardrbay entries — those always require an
explicit selector. There is no interactive CLI in this build yet; the
section is reserved and parsed harmlessly.

## `[runtimes.*]` and `[models.*]` — runner overlay (layer 2)

These sections belong to the model runner (`shardr run` / `shardr
serve`, spec 002 §2). Model keys use the scheme-less short form
`ns/name:quant` (000 §2); `[runtimes.llama]` applies globally.
shardhive ignores them — they are consumed at run time.

The runner merges four layers, lowest → highest precedence:

1. **Advisory defaults** — optional `runtime-config` entries inside
   the artifact (publisher-provided; machine-neutral keys only)
2. **User config** — `~/.config/shardr/config.toml` (`[runtimes.llama]`,
   per-model `[models."ns/name:quant"]`)
3. **`--config <file>`** — per-invocation TOML file, same schema as
   layer 2
4. **`--set key=value`** — CLI flags (repeatable), highest precedence

A higher layer replaces individual keys; no cross-key inference. Keys
are validated against the 002 §7.1 allowlist (`n_gpu_layers`,
`ctx_size`, `n_threads`, `flash_attn`, `mlock`, `kv_cache_type`,
`batch_size`, `ubatch_size`, `n_parallel`, `jinja`, `mmproj_variant`).
An unknown key in any layer is a loud error that names the layer it
came from — nothing is dropped silently. A key absent from every
layer falls back to the runtime built-in.

**Bool keys are tri-state**: absent = inherit (runtime default
applies), `true` = pass the flag, `false` = omit the flag — which
means the runtime default applies, **not "off"**. An overlay cannot
explicitly disable a runtime-default-ON flag in v1.

```sh
shardr run qwen/test:q8_0 --set llama.n_gpu_layers=40 --set llama.ctx_size=32768
```
