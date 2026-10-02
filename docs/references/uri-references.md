# URI & reference grammar

The canonical way to address models across all shardr components and
third-party clients. The contract is
[spec 000](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/000-reference-grammar.md)
(status: Candidate v1, machine-checked by
`docs/specs/vectors/000-reference.jsonl`); this page summarizes it and
shows real resolution behavior from a running shardhive.

## Canonical form

```
shardr:///ns/name[:sel][@digest]

ns     := [a-z0-9][a-z0-9._-]{0,63}
name   := [a-z0-9][a-z0-9._-]{0,127}
sel    := quant | tag "+" quant
tag    := [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}    ; case-sensitive local alias
quant  := per spec 000 Appendix A
digest := "sha256:" 64*HEXLOWER
```

A reference identifies **what** you want, never where it is —
resolution is shardhive's job (spec 005 §6). The scheme has an empty
authority (RFC 3986 `file:///` precedent); a host authority is
reserved for future federation and stays syntactically distinct.

Rules that matter in practice:

- Total length ≤ 512 bytes; no implicit trimming; parse errors are loud.
- `ns`/`name` are silently lowercased (one entity, one namespace, no
  case twins). `tag` is preserved verbatim.
- A quant is **always** required — the runner addresses a servable
  unit (weights in a concrete quantization), never a bare family. A
  reference without `:sel` is a resolution error; there is no default
  selector in the scheme. (The interactive CLI may apply the user's
  `[references] default_selector` — human comfort only, never in
  Modelfiles, the API, manifests, or shardrbay entries.)
- Reserved namespaces: `library/`, `shardr/`.

## CLI short form

Interactive commands additionally accept `ns/name[:sel][@digest]` and
canonicalize internally. The short form is input sugar only — it never
appears in Modelfiles, APIs, manifests, or shardrbay entries. The API
rejects it with the canonical spelling in the message (executed):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=gold/smollm2-135m-instruct:q4_k_m'
# {"error":{"code":"E_PARSE",
#           "message":"references at the API must be canonical URIs; use shardr:///gold/smollm2-135m-instruct:q4_k_m"}}
```

## Selector semantics

1. **Quant-only (primary form):** resolves against the repo's current
   index. Exact match, or a **unique prefix** of exactly one member;
   ambiguity is a loud error listing the candidates. Executed with an
   index holding `q4_k_m` and `q4km`:

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///gold/smollm2-135m-instruct:q4'
# {"error":{"code":"E_AMBIGUOUS_SELECTOR","message":"ambiguous prefix q4",
#           "candidates":["q4_k_m","q4km"]}}
```

2. **Tag+quant:** the tag resolves to a stored index snapshot, then
   the member is selected as above. Tags are **scoped per repository**
   — the state key is `ns/name:tag`; a tag from another repository
   never resolves (executed):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///minicpmv/mmproj:snap%2Braw'
# {"error":{"code":"E_UNKNOWN_REF",
#           "message":"no tag snap for minicpmv/mmproj; tags are scoped per repository (000 §3.4)"}}
```

3. **@digest** pins the member manifest digest and must match the
   selection; `shardr:///ns/name@sha256:…` (no selector) addresses a
   manifest directly, bypassing index lookup (executed — note the
   absent `indexDigest`):

```sh
curl -s --unix-socket "$S" 'http://localhost/v1/resolve?ref=shardr:///minicpmv/mmproj@sha256:3db690a6d2d18d85eff7206bcf65911321bf582a58b4194d33d0b78e5a5a8171'
# {"ref":"shardr:///minicpmv/mmproj@sha256:3db690a6…","ns":"minicpmv","name":"mmproj",
#  "manifestDigest":"sha256:3db690a6d2d18d85eff7206bcf65911321bf582a58b4194d33d0b78e5a5a8171",
#  "plan":"pending","planReason":"…"}
```

Tags are local aliases managed by the shardhive owner; imports create
none. Tags whose lowercased form starts with `sha256-`/`sha256:` or
matches the quant syntax are invalid — quant-shaped strings always
select index members.

## Where quant names come from

Imports derive member quants from data (the classifier), not from a
fixed list. Observed on a real daemon:

| Imported file | Derived member quant |
| --- | --- |
| `smollm2-135m-instruct-q4_k_m.gguf` | `q4_k_m` |
| `smollm2-q4km.gguf` | `q4km` |
| `SmolLM2-135M-Instruct-Q4_K_M.gguf` (uppercase) | `raw` |

The quant vocabulary is lowercase-only (spec 000 Appendix A); an
uppercase quant token like `Q4_K_M` does not match it and the file
classifies as `raw` (the unquantized/unclassifiable fallback).
Identical bytes imported under different names converge into one index
— the `/v1/models` output above shows one repo with all three member
quants.

## Verdict table (from spec 000 §4)

| Reference | Verdict |
| --- | --- |
| `shardr:///unsloth/qwen3.8-27b-gguf:ud-q4_k_m` | valid, canonical primary form |
| `shardr:///unsloth/qwen3.8-27b-gguf:q8` | valid, unique prefix → `q8_0` |
| `unsloth/qwen3.8-27b-gguf:q8_0` | valid CLI short form (canonicalizes) |
| `shardr:///ns/name:q8_0@sha256:ab…` | valid, pinned |
| `shardr:///ns/name` | invalid — no selector |
| `shardr:///ns/name:stable` | invalid — tag without quant |
| `shardr:///ns/name:q4` | invalid — ambiguous prefix (lists `q4_0`, `q4_1`) |

The machine-checked corpus including grammar edge cases is
`docs/specs/vectors/000-reference.jsonl` in the repository.
