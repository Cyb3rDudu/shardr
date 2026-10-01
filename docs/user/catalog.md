# Catalog pulls (pirateface.co)

A catalog pull fetches a community-listed model torrent into your CAS
and keeps seeding the swarm it came from. The first (and currently only)
provider is [pirateface.co](https://pirateface.co), a listing of
checksum-verified BitTorrent torrents for Apache-2.0/MIT Hugging Face
models.

```sh
shardr catalog search qwen 0.5b gguf
shardr pull bartowski/Qwen2.5-0.5B-Instruct-GGUF --quant raw
```

A bare `owner/repo` argument (no `:quant` selector) is a catalog pull.
A selector-bearing argument keeps the established behavior: `shardr
pull ns/name:quant` fills a ref you already have from the shardr swarm.

## What a pull does

1. **Resolve** — the listing is fetched from the provider: repo, size,
   reported seeders, magnet, and the pinned Hugging Face revision (the
   40-hex commit in the listing's webseed URL).
2. **Anchor** — expected per-file digests are fetched from Hugging Face
   at that pinned revision (tree API at the commit). The magnet is
   never trusted by itself; the catalog is discovery and transport
   only.
3. **Fetch** — the listed torrent is joined (tracker, DHT, webseed).
   Every file is verified twice: the torrent's own piece hashes on
   arrival, then the anchor gate at completion — flat SHA-256 for LFS
   files, the git blob id for the rest. A mismatch refuses the file
   loudly; nothing unverified enters the store.
4. **Import** — the sealed files go through the regular import pipeline
   (same classification, same convergence). The model lands under the
   lowercased repo id, e.g. `shardr:///bartowski/qwen2.5-0.5b-instruct-gguf:raw`.
5. **Seed back** — the torrent handle stays alive and seeds from your
   CAS bytes (good-citizen mode): same bytes, the listing's own piece
   layout. Community seeding has its own upload budget
   ([`[catalog] upload_limit`](config.md)); it never eats your
   shardr-swarm budget.

Provider facts worth knowing (verified 2026-10):

- Listing torrents are BitTorrent v1 and carry **one weight file each**
  (one torrent per quant, no companions). `--quant` selects the family;
  uppercase filenames like `Q4_K_M.gguf` derive the quant `raw`
  (the reference vocabulary is lowercase-only, 000 App. A).
- The listing's webseed redirects to `huggingface.co/<repo>/resolve/
  <revision>/<file>` — live models download straight from the HF CDN
  with the swarm as fallback.
- Torrent metadata comes from peers: a pull of a listing with **zero
  seeders online** fails after two minutes with a loud error. A magnet
  alone never carries the file tree.

## Rescued models

When the Hugging Face source of a listing is gone (removed repo or
revision — the catalog marks these *Rescued*), there is no HF anchor.
The pull refuses loudly:

```
E_NOT_ANCHORED: no anchor — the model is rescued (its Hugging Face
source is gone) and the only digest record is the catalog provider's
own; re-run with --trust-catalog to accept that trust shift …
```

With `--trust-catalog` the pull proceeds, and trust shifts from
Hugging Face to the catalog provider:

- weight files verify against the catalog-recorded SHA-256 checksums
  (the provider's copy of the official HF hashes),
- files without a recorded checksum ride on the listed torrent's own
  piece hashes — that is exactly what accepting the catalog's trust
  means.

The warning is printed on the CLI and carried on the job result. Byte
verification is never skipped in either mode — only the *source of the
expected digests* changes.

## API

The same flow is available over the daemon API:

```sh
curl --unix-socket $SHARDR_SOCKET http://shardhive/v1/import/catalog \
  -H 'Content-Type: application/json' \
  -d '{"repo":"bartowski/Qwen2.5-0.5B-Instruct-GGUF","quant":"raw"}'
```

The daemon re-resolves the listing and re-derives the anchor itself —
client-supplied magnets are never trusted. Error classes follow the
error reference: `E_NOT_ANCHORED` (rescued, no trust shift),
`E_UNKNOWN_REF` (not listed), `E_SOURCE_UNAVAILABLE` (provider or HF
unreachable).
