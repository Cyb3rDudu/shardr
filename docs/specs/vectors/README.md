# Spec Test Vectors

Machine-checkable test vectors for the shardr specs. The specs in
`docs/specs/` stay **Draft v0** until these vectors exist and pass —
normativity without vectors is declaration. Every vector carries its
expected output (bytes, digests, parse results or error classes); the
harness in `internal/specvectors/` runs all suites as plain Go tests:

```sh
go test ./...
```

CI runs the same command on both runners (ubuntu-latest, macos-latest).

## Layout

| Path | Suite | Spec |
| --- | --- | --- |
| `000-reference.jsonl` | Reference grammar: parse, canonicalization, resolution | 000 |
| `001-canonical.jsonl` | Document canonicalization (JCS) + validation | 001 |
| `004-torrent.jsonl` | Piece ladder, merkle trees, info dict, record verification | 004 |
| `004-example-files.json` | Deterministic byte recipes for the example artifact | 004 |
| `gold/` | Goldfiles: byte-exact expected outputs | 001, 004 |

## Format conventions

One JSON object per line (JSONL); blank lines are ignored. Common fields:

- `id` — unique, stable vector id (`ref-`, `can-`, `tv-` prefixes)
- `desc` — what the vector pins down
- `expect.ok` — `true` = must succeed, `false` = must fail with
  `expect.errorClass` (and optionally `expect.candidates`)
- absent expected fields are not compared

Error classes are **stable API**: harnesses in other languages map them to
their own error types but must produce the same class for the same input.

## Suite 000 — reference grammar

Per-vector fields: `input`, optional `form: "cli-short"` (the harness expands
`ns/name[:sel][@digest]` to `shardr:///…` first), optional `context`
(index members and/or tag snapshots for resolution), `expect`.

Expected-success fields: `canonical` (the canonicalized string, ns/name
lowercased, selector preserved as typed), `ns`, `name`, `sel`, `quant`,
`tag`, `digest`, and `resolved {quant, manifest}` when a `context` is given.

Error classes:

| Class | Meaning |
| --- | --- |
| `E_LENGTH` | input exceeds 512 bytes (checked before grammar) |
| `E_PARSE` | grammar violation (scheme, authority, charsets, lengths, sel form, bare tag, …) |
| `E_NO_SELECTOR` | neither `:sel` nor `@digest` present (no default selector) |
| `E_DIGEST_FORMAT` | `@digest` is not `sha256:` + 64 lowercase hex |
| `E_TAG_BANNED` | tag starts with `sha256-`/`sha256:` (lowercased) or matches quant syntax |
| `E_UNKNOWN_TAG` | tag does not resolve to a stored snapshot |
| `E_AMBIGUOUS_SELECTOR` | prefix matches > 1 member; `candidates` lists them sorted |
| `E_NO_MEMBER` | selector matches no index member |
| `E_PIN_MISMATCH` | `@digest` does not match the resolved member |

Processing order: length → scheme/empty authority → `@digest` suffix →
ns/name (case-fold then validate) → selector (quant / tag+quant, tag ban) →
no-selector rule. Resolution (only with `context`): exact match wins over
prefix; unique prefix expands; ambiguity is loud; pin must match the
resolved member. The tag form always carries `+quant` — a bare tag is not
a selector (ruling R1, spec 000 §2); pinned by `ref-0117`/`ref-0118`.

## Suite 001 — canonicalization

Pipeline per vector: decode input JSON → validate against the 001 MUSTs →
canonicalize with RFC 8785 (JCS) → compare byte-exact against the goldfile
→ compare flat SHA-256 against `expectedDigest`.

Inputs: `input` (path to a differently-serialized form of the document —
reversed key order at every level, pretty or compact) or `inline`
(embedded document). Goldfiles are **byte-exact**: no trailing
newline, JCS serialization only.

Validation error classes: `E_VALIDATION` plus the specific
`E_VALIDATION_WEIGHTS_MIX`, `E_VALIDATION_WEIGHTS_MISSING`,
`E_VALIDATION_FILE_ORDER`, `E_VALIDATION_PARTS`,
`E_VALIDATION_QUANT_DUP`, `E_VALIDATION_INFOHASH`,
`E_VALIDATION_KIND`, `E_VALIDATION_RESERVED_PATH` (ruling R2),
`E_VALIDATION_RUNTIME_DUP`, `E_VALIDATION_CARDINALITY`
(see `canonical_impl_test.go` for the rule set; unknown fields are
preserved, not rejected — forward compatibility, 001 §3.2).

JCS provenance: the reference Go implementation from the RFC 8785 authors
(`github.com/cyberphone/json-canonicalization`, package
`go/src/webpki.org/jsoncanonicalizer`) generates the goldfiles. Expected
bytes are committed data (goldfile principle) — the suite runners never
regenerate them.

## Suite 004 — torrent mapping

Vector `kind`s:

- `piece-length` — ladder boundary cases at exactly 1/8/64/512 GiB and
  +1 byte each (004 §3). Pure function of total size.
- `merkle-root` — 16 KiB-leaf per-file merkle trees incl. the zero-hash
  padding chain (3, 60, 193 blocks → padded to 4, 64, 256).
- `info-dict` — full info-dict reconstruction from the example manifest:
  torrent name, file tree (entry names + `manifest/sha256-<hex>`), ladder
  piece length, bencoded bytes, infohash, piece-layers blob and digest.
- `verify-record` — the 001 §6 binding: reconstructed infohash and
  piece-layers digest must equal the distribution record's fields.
- `verify-manifest` — every manifest entry's flat digest, size and
  merkle root recompute from the deterministic file bytes.

Verification error classes: `E_MANIFEST_DIGEST_MISMATCH`,
`E_INFOHASH_MISMATCH`, `E_PIECE_LENGTH_MISMATCH`,
`E_PIECE_LAYERS_DIGEST_MISMATCH`, `E_DIGEST_MISMATCH`,
`E_SIZE_MISMATCH`, `E_MERKLE_ROOT_MISMATCH`.

### Deterministic file recipes (`004-example-files.json`)

| Recipe | Bytes |
| --- | --- |
| `{"json": <doc>}` | JCS serialization of the embedded JSON document |
| `{"repeat": "<s>", "size": N}` | cyclic repetition of `s`, truncated to N |
| `{"sha256stream": "<label>", "size": N}` | concatenation of `SHA-256(label + ":" + i)` digests for i = 0,1,2,…, truncated to N |

### Merkle provenance

Padding rule: the leaf layer is padded to a power-of-two width with
32 zero bytes; internal nodes are `SHA-256(left ‖ right)`; the last block
may be short and is hashed as-is; empty input hashes to `SHA-256("")`.
This matches BEP 52 as implemented by libtorrent (`merkle_root()` with the
chained zero-hash pad) and anacrolix/torrent (`merkle.RootWithPadHash`),
and is what spec 004 §3 calls the "BEP 52 zero-hash chain". Known-answer
anchors: the empty and 5-byte vectors equal the well-known SHA-256 test
values of `""` and `"hello"`.

Piece layers follow BEP 52: one entry per file **larger than** the piece
length, keyed by the raw 32-byte merkle root, value = concatenated hashes
of the layer where one hash covers `pieceLength` bytes; hashes covering
only padding beyond EOF are omitted.

### Example artifact cross-links

`manifest-01` (goldfile) is the single example: its canonical bytes hash to
digest D, which feeds the index member, the distribution record's
`manifestDigest`, the torrent name (`shardr-sha256-<12 hex of D>`) and the
in-tree manifest path (`manifest/sha256-<hex of D>`). Suite 004's
`tv-verify-01` verifies against the very `gold/distribution-01.canonical.json`
committed for suite 001 — the two suites cannot drift apart.

## Regenerating goldfiles

```sh
go test ./internal/specvectors -update -run TestGenerateGoldfiles -v
```

prints all derived constants (digests, infohash, merkle roots) and rewrites
`gold/`. Expected values inside the JSONL files are hand-pasted from that
output — committed data, never recomputed by the runners. Regenerating and
diffing against the committed goldfiles must be a no-op unless a spec
decision changed (in which case: spec defect first, then vectors).

## Spec defects found by these vectors (specs NOT patched)

Reported on issue #3; the vectors encode the stated resolution so the
behavior stays pinned either way. D10 and D11 were resolved by rulings
during PR #8 review and are applied as spec commits in this PR:

- **D10** (000 §2): `sel := quant | tag["+"quant]` read as written made a
  bare tag a valid selector, contradicting §3.2 ("Tag+quant") and the
  resolution model. **Resolved by ruling R1** — spec grammar is now
  `sel := quant | tag "+" quant` (tag form always carries `+quant`).
  Pinned by `ref-0117`, `ref-0118`.
- **D11** (001 §3.1 / 004 §3): a file entry named `manifest/...` would
  collide with the embedded manifest document in the torrent file tree.
  **Resolved by ruling R2** — the `manifest/` path prefix is reserved for
  the embedded manifest; file entries must not occupy it. Pinned by
  `can-0117` (`E_VALIDATION_RESERVED_PATH`).

- **D1** (000 App. A): the quant prefix rule rejects the reserved term
  `raw` (no known prefix starts with `r`), contradicting 000 §4 which lists
  `shardr:///some/repo:raw` as valid. Vectors: `raw` is a valid quant.
- **D2** (000 §2): grammar defines ns/name as lowercase-only charsets while
  §2 also mandates silent lowercasing — needs explicit "parsers case-fold
  input, canonical form is lowercase". Vectors: fold, then validate.
- **D3** (000 §2): reserved namespaces `library/`, `shardr/` have no
  defined semantics (usage? creation? resolution?). Vectors pin only:
  syntactically valid parse.
- **D4** (000 §2/§3): undefined whether prefix expansion rewrites the
  canonical string. Vectors: canonical preserves the selector as typed;
  resolution is a separate step with its own output.
- **D5** (000 §2): the 512-byte length limit is unreachable for
  grammar-valid references (max ≈ 300 bytes); only observable if length is
  checked before grammar. Vectors: length check first.
- **D6** (001 §3.1 rule 4): "canonical file order" is phrased as a
  construction rule; for byte-stable digests across implementations it must
  be a validation MUST (JCS does not reorder arrays). Vectors: out-of-order
  `files` arrays fail with `E_VALIDATION_FILE_ORDER`.
- **D7** (001 §3 / 004 §2): the string format of `bt.merkleRoot` is
  unpinned. Vectors: `sha256:` + 64 lowercase hex (raw 32 bytes appear only
  inside the bencoded torrent structures).
- **D8** (004 §3): "total size" for the piece ladder does not say whether
  the manifest file itself (part of the torrent file tree) counts. Vectors:
  totalSize = Σ entry sizes + manifest bytes.
- **D9** (004 §3 / BEP 52): merkle root of an **empty** file is unpinned
  (BEP 52 makes `pieces root` optional for empty files). Vectors pin
  `SHA-256("")` (= anacrolix convention); the example artifact contains no
  empty files.
