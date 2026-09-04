# Error reference

Every error — API responses and CLI-visible failures — uses stable
`E_*` classes. API errors come in the envelope
`{"error":{"code","message","candidates"?}}` (see
[api.md](api.md#error-envelope)).

## Reference grammar (000) — from `ref.Parse`, shared with all clients

| Class | Meaning | Typical cause → fix |
| --- | --- | --- |
| `E_LENGTH` | reference exceeds 512 bytes | over-long tag/quant → shorten |
| `E_PARSE` | malformed reference (scheme, ns/name, selector syntax) | typo; short form at the API (`ns/name:q`) → use the canonical URI (`shardr:///ns/name:q`); the message names the canonical spelling when it can |
| `E_NO_SELECTOR` | selector required but absent | API/Modelfile refs need `:quant` (or a tag) — interactive-CLI default selectors never apply here |
| `E_DIGEST_FORMAT` | `@sha256:<hex>` pin is not 64 lowercase hex | wrong digest → use the canonical form |
| `E_TAG_BANNED` | tag uses reserved characters/space rules | rename the tag |
| `E_UNKNOWN_TAG` | tag does not exist for this repository | `candidates` lists the repo's existing tags → pick one |
| `E_AMBIGUOUS_SELECTOR` | selector matches several index members | be more specific (tag+quant) |
| `E_NO_MEMBER` | no index member matches the selector | wrong quant for this model → check `/v1/models` for the members that exist |
| `E_PIN_MISMATCH` | pinned digest does not match what the selector resolves to | stale pin → re-resolve and re-pin |

## API surface (005 §3 inventory + importer outcomes)

| Class | HTTP | Meaning | Typical cause → fix |
| --- | --- | --- | --- |
| `E_BAD_REQUEST` | 400 | malformed body/params | bad JSON; missing `ref`/`paths`/`as`/`repo`; `magnet`+`infohash` both given → fix the request |
| `E_SOURCE_NOT_REGULAR` | 400 | import source is not a regular file | symlink/FIFO/device in or at the import root → import real files (the boundary is the point) |
| `E_NOT_FOUND` | 404 | unknown endpoint or blob/job id | wrong path/digest/id |
| `E_UNKNOWN_REF` | 404 | no local index for the repo (or HF repo not found) | model never imported here; resolver fetch not implemented in this build → import it (`/v1/import/*`); `candidates` lists same-namespace repos |
| `E_NO_INDEX` | 404 | index blob not present in the CAS | metadata fetch is ensure's job in a later slice → the importing node's index is absent |
| `E_INVALID_INDEX` | 500 | model-index blob fails validation | corruption — never silently rebuilt; restore the blob or re-import; `shardhive cas verify --all` shows the state |
| `E_RANGE_INVALID` | 416 | blob range does not overlap | `Range` beyond EOF → fix the range |
| `E_UNSUPPORTED_VERSION` | 400 | another API major version | client speaks `/v2/` → speak `/v1/` (`candidates` names the supported version) |
| `E_NOT_IMPLEMENTED` | 501 | reserved endpoint/slice | swarm disabled for `/v1/import/bt` → set `[swarm] enabled = true` (message says so) |
| `E_SOURCE_UNAVAILABLE` | 502/503/job | source not reachable / not implemented | HF unreachable; manifest not local for ensure; swarm has no peers → check network/config/hints |
| `E_SOURCE_FORBIDDEN` | 502/job | HF repo gated | set `SHARDR_HF_TOKEN` in the daemon's environment |
| `E_RATE_LIMITED` | 502/job | HF rate limit | retry later / set a token |
| `E_NOT_IMPORTABLE` | job | import fails the 001 §8 rule set or verification | no recognized weights (eligibility gate); swarm bytes ≠ pin; structurally invalid distribution record → fix the source or the pin |
| `E_INTERNAL` | 500/job | daemon bug — never a user-input verdict | report it; `cas verify` rules out disk corruption |

Notes:

- Classes with "job" in the HTTP column surface inside an asynchronous
  job's `error` field (`state: "failed"`), not as the POST response.
- `candidates`, when present, lists self-correction options (existing
  tags, same-namespace models, supported API version).
- Errors are terminal and loud — nothing retries silently.
