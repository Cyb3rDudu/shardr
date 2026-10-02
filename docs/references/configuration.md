# Configuration reference

The `config.toml` file and the overlay chain, as implemented — every
key below verified against the code (`internal/config`,
`cmd/shardhive/config.go`, `internal/runner/`). The config format is
specified in spec 004 §7; the runner overlay contract in spec 002 §2
and §7.1 (both canonical, linked from here).

## File location and format

Path resolution: `$SHARDR_CONFIG`, else `$XDG_CONFIG_HOME/shardr/config.toml`,
else `~/.config/shardr/config.toml`. No file = documented defaults;
everything below is optional.

The parser (`internal/config`) accepts a documented minimal subset:
sections (including quoted dotted headers like
`[models."ns/name:quant".llama]`), `key = value` with bool / integer /
`"quoted string"`, and `#` comments on their own line or inline after
a value (outside quoted strings). No arrays, no nested tables. Anything
the parser does not understand is a loud error — a typo never quietly
disables a subsystem.

Each section belongs to one component, and each component validates its
own keys — an unknown key in a known section is a loud startup error
(executed):

```console
$ shardhive serve
shardhive: config ~/.config/shardr/config.toml: unknown [swarm] key "bogus" (known: enabled, seed, upload_limit, dht, no_seed_verify, webseed_addr)
```

## `[swarm]` — the swarm client (daemon)

Read by `shardhive serve` (`cmd/shardhive/config.go`). Defaults from
`swarm.DefaultConfig()`: enabled, seeding, DHT on, unlimited upload.

| Key | Type | Default | Effect |
| --- | --- | --- | --- |
| `enabled` | bool | `true` | swarm client on/off. Off disables swarm fills, `/import/bt` (loud `E_NOT_IMPLEMENTED`), and catalog pulls (`E_SOURCE_UNAVAILABLE`) |
| `seed` | bool | `true` | seed locally-complete artifacts to the swarm (usage is replication) |
| `upload_limit` | int | `0` | upload budget in bytes/second, shared by the torrent client and the webseed listener; `0` = unlimited. Also the fallback budget for `[catalog]` |
| `dht` | bool | `true` | DHT peer discovery |
| `no_seed_verify` | bool | `false` | skip the seed-start re-hash (003 §4 escape hatch, documented unsafe; the `--seed-no-verify` serve flag sets it too) |
| `webseed_addr` | string | `"127.0.0.1:0"` | bind address of the node's webseed HTTP listener (`:0` = ephemeral port) |

## `[catalog]` — community-catalog pulls (daemon)

| Key | Type | Default | Effect |
| --- | --- | --- | --- |
| `upload_limit` | int | inherit `[swarm] upload_limit` | separate good-citizen seeding budget for catalog swarms (bytes/second, `0` = unlimited). When unset, the `[swarm]` value applies — the two budgets never eat each other |
| `url` | string | provider default (`https://pirateface.co`) | catalog provider base URL (mirror/testing), honored by the daemon **and** the CLI catalog commands. Validated fail-closed at startup: absolute `https://` with host, no path/query/fragment/userinfo (scheme + host, optional port). The URL flows to every provider-derived endpoint (listings, model pages, checksums); magnet webseeds stay content-driven (they come from the listing itself) |

Unknown `[catalog]` keys are a loud error, same as `[swarm]`.

Base-URL precedence: `[catalog] url` wins over `$SHARDR_CATALOG_URL`,
which wins over the built-in default — config is the explicit knob,
the env var is the CLI/test override.

## `[references]` — CLI comfort only

| Key | Type | Default | Effect |
| --- | --- | --- | --- |
| `default_selector` | string | — | quant applied when a human types a selector-less ref interactively. Never applies to Modelfiles, the API, manifests, or shardrbay entries (000 §2) |

## Runner overlay — `[runtimes.llama]` and `[models."<short-ref>".llama]`

The runner (`shardr run` / `shardr serve`) builds its llama-server
flags from four layers; **higher wins per key**, and every key in every
layer is validated against the allowlist below — an unknown key is a
loud error at run time, with provenance of the layer it came from
(spec 002 §2).

| Layer | Source |
| --- | --- |
| 1 · advisory | manifest runtime-config entries of the artifact (001 §3.1) |
| 2 · user config | `config.toml`: `[runtimes.llama]` global, `[models."<ns/name:quant>".llama]` per-model — the per-model table is the more specific site |
| 3 · `--config <file>` | same schema as layer 2, from an explicit file |
| 4 · `--set key=value` | repeatable command-line overrides |

Allowlist (spec 002 §7.1, as mapped in `internal/runner/overlay.go`;
valid in layers 2–4 — the **advisory layer accepts only `ctx_size` and
`jinja`**, publisher content must stay machine-neutral per 002 §2.1):

| Key | Type | llama-server flag |
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
| `mmproj_variant` | string | (runner-internal: vision-projector variant selection, not a server flag) |

**Bool keys are tri-state** (spec 002 §7.1): an absent bool **inherits**
the runtime built-in default; `true` passes the flag; `false` omits the
flag — which means the runtime default applies, not "off". In v1 an
overlay cannot explicitly disable a runtime-default-ON flag; overlays
enable and tune, they do not fight the runtime's defaults.

Example shape (verified parseable by the parser tests):

```toml
[swarm]
enabled = true
upload_limit = 1048576          # 1 MiB/s, shared budget

[catalog]
upload_limit = 524288           # catalog seeding gets its own 512 KiB/s
url = "https://pirateface.co"    # provider base URL; unset = the same default

[references]
default_selector = "q4_k_m"

[runtimes.llama]
n_gpu_layers = 40               # global
jinja = true

[models."gold/smollm2-135m-instruct:q4_k_m".llama]
ctx_size = 8192                 # per-model override, wins per key
```

## Environment variables

User-relevant variables across the components (daemon, CLI, runner,
build/fetch tooling):

| Variable | Consumer | Effect |
| --- | --- | --- |
| `SHARDR_CONFIG` | config loader | config.toml path override |
| `SHARDR_SOCKET` | daemon + CLI | Unix socket path override |
| `XDG_RUNTIME_DIR` | daemon + CLI | socket + serve-registry + split-scratch location |
| `SHARDR_CAS` | CAS | store root override |
| `XDG_DATA_HOME` | CAS | default store root base (`…/shardr/cas`) |
| `SHARDR_HF_TOKEN` | importer | Hugging Face bearer token for gated repos |
| `SHARDR_CATALOG_URL` | catalog provider | base-URL override when `[catalog] url` is unset (config wins; env is the CLI/test fallback) |
| `HF_ENDPOINT` | importer | Hugging Face API endpoint override (mirrors) |
| `SHARDR_LLAMA_SERVER` | runner | llama-server binary override (else: next to the `shardr` executable, then `$PATH`) |
| `GITHUB_TOKEN` | llama-lock fetch / workflows | GitHub release-API token — without it the prebuilt fetch can rate-limit (403) |
