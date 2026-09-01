# 000 — References & URI Scheme

Status: **Draft v0**

## 1. Purpose

The canonical way to address models across all shardr components and
third-party clients. A reference identifies **what** you want, never
**where** it is: resolution is shardhive's job (005 §3).

## 2. URI scheme

Canonical form (RFC 3986-conformant — **empty authority**, `file:///`
precedent; a future host authority is reserved for federation with
remote shardhives and stays syntactically distinct):

```
shardr:///ns/name[:sel][@digest]
```

```
ns     := [a-z0-9][a-z0-9._-]{0,63}
name   := [a-z0-9][a-z0-9._-]{0,127}
sel    := quant | tag["+"quant]     ; quant-only is the primary form
tag    := [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}   ; case-sensitive local alias
quant  := per Appendix A
digest := "sha256:" 64*HEXLOWER
```

**CLI short form:** interactive commands additionally accept
`ns/name[:sel][@digest]` and canonicalize to the full URI internally.
The short form is input sugar only — it never appears in Modelfiles,
APIs, manifests, or shardrbay entries, which use the canonical URI.

- Total length ≤ 512 bytes; no implicit trimming; parse errors are loud.
- `ns`/`name` are silently lowercased (one entity, one namespace, no
  case twins). `tag` is preserved verbatim.
- Reserved namespaces: `library/`, `shardr/`.
- A reference without `:sel` is a **resolution error** — there is no
  default selector in the scheme. What is "current" differs per user;
  interactive-CLI defaults belong to user configuration (004 §7) and
  apply only there.

## 3. Selector semantics

1. **Quant-only (primary form):**
  `shardr:///unsloth/qwen3.8-27b-gguf:ud-q4_k_m` resolves against the
  repo's **current index** (005 §6). Matching is exact, or a **unique
  prefix** of exactly one member (`:q8` → `q8_0`); ambiguity is a loud
  error listing candidates.
2. **Tag+quant:** the tag resolves to a stored index snapshot, then the
  member is selected as above.
3. **@digest** pins the member manifest digest and MUST match the
  selection; `shardr:///ns/name@sha256:…` (no selector) addresses a
  manifest directly.
4. Tags are **local aliases** managed by the shardhive owner; imports
   create none. Tags whose lowercased form starts with `sha256-`/
   `sha256:` or matches the quant syntax (Appendix A) are invalid —
   quant-shaped strings always select index members, never point at
   content themselves.

## 4. Examples

| Reference | Verdict |
| --- | --- |
| `shardr:///unsloth/qwen3.8-27b-gguf:ud-q4_k_m` | valid, canonical primary form |
| `shardr:///unsloth/qwen3.8-27b-gguf:q8` | valid, unique prefix → `q8_0` |
| `unsloth/qwen3.8-27b-gguf:q8_0` | valid CLI short form (canonicalizes) |
| `shardr:///zai-org/glm-5.3:fp8` | valid |
| `shardr:///some/repo:raw` | valid, unquantized member |
| `shardr:///ns/name:q8_0@sha256:ab…` | valid, pinned |
| `shardr:///ns/name` | invalid — no selector |
| `shardr:///ns/name:q4` | invalid — ambiguous prefix (lists `q4_0`, `q4_1`) |

## Appendix A — Quant syntax and vocabulary

**Protocol-global and versioned with this spec.** Local configuration
MUST NOT extend or alter the vocabulary — the tag/quant boundary must be
machine-independent (the same string is the same thing on every node).

- lowercase, charset `[a-z0-9_-]`, ≤ 24 chars;
- contains ≥ 1 digit **or** is the reserved term `raw` (unquantized /
  unclassifiable import fallback);
- starts with a known prefix: `q`, `iq`, `tq`, `ud-`, `bf`, `f`, `fp`,
  `mx`.

Vocabulary (non-exhaustive; governs the tag ban — imports derive member
quants from data, whatever it is; new quant families extend this list
via a spec revision, which is a protocol version bump): `q2_k q3_k_m
q4_0 q4_1 q5_0 q5_1 q6_k q8_0 iq1_s iq1_m iq2_s iq2_xxs iq3_s iq3_xxs
iq4_xs tq1_0 tq2_0 mxfp4 bf16 f16 f32 fp8 fp16 raw` plus lineage/suffix
families (`ud-*`, `_s _m _l _xl _xxs _k_s _k_m _k_l _k_xl`).
