# 003 — CAS (shardhive's content-addressed store)

Status: **Draft v0**

## 1. Purpose

The content-addressed store: how blobs live on disk, how writes verify,
what "trusted" means — and the single-copy principle that makes TB-scale
model inventories manageable. Contract between shardhive's importers,
its swarm client, and its readers (the runner, other clients).

## 2. Layout

Root: `$SHARDR_CAS` if set, else `$XDG_DATA_HOME/shardr/cas`, else
`~/.local/share/shardr/cas`.

```
<root>/
  blobs/sha256/<2hex>/<62hex>    ; content-addressed blob files, mode 0444
  incoming/                      ; in-progress writes: <16hex-random>.part
  state/                         ; shardhive metadata: current indexes,
                                 ; repo namespaces, tag aliases, jobs
```

- Manifests, model configs, and piece-layers blobs are blobs — one
  storage path, one trust path.
- **2-hex sharding** bounds every directory to ≤ 1/256 of the blob
  population (mirror-scale repos reach millions of blobs).
- Directories 0755; blob files **0444**. The mode is permission hygiene
  (tamper speed-bump, race-free mmap) — the trust boundary is the
  digest and the re-hash (§4), never mode bits: an owner can chmod or
  replace files, only verification catches that. Immutability *by
  contract* plus 0444 is what makes zero-copy mmap safe (002 §4).
- `state/` holds the namespace map (`ns/name` → current index digest),
  index snapshots, and tag aliases. This is shardhive-local metadata,
  never torrented (004 §1).

## 3. Write path (verifying, atomic)

For expected digest `d` and byte stream `s`:

1. Stream `s` to `incoming/<random>.part`, computing flat SHA-256
   concurrently.
2. On EOF: digest ≠ `d` → delete the part, report error. Partial data is
   never promoted, never trusted, never seeded.
3. Match → `fsync`, `chmod 0444`, atomic `rename()` into `blobs/`.
4. Target exists → keep the existing blob (idempotent). POSIX rename
   replacing a digest-identical file is byte-equivalent; the existence
   check is an optimization, not the correctness mechanism.

Rules: blobs are never mutated in place; concurrent same-digest writers
are safe by construction (atomic rename, both wrote identical bytes or
one failed verification); on startup, stale parts (mtime > 24 h) are
removed.

## 4. Integrity semantics

- A blob is **trusted** iff it was written by the verifying write path,
  or fully re-hashed since.
- **Seed-start re-hash**: before a blob is offered to the swarm for the
  first time in a process's lifetime, it is fully re-hashed and must
  match (cached per process). Disk-level corruption must never silently
  become swarm corruption. Escape hatch `--seed-no-verify` exists,
  documented unsafe, never default.
- `shardr verify <ref|digest|--all>`: exit `0` clean, `1` mismatch,
  `2` missing. Verification is an explicit command with explicit cost —
  never an implicit background tax.
- The CAS never serves `incoming/*.part` to peers, runtimes, or readers.

## 5. Zero-copy reads

- **Local readers** (the runner, same-machine clients) receive **CAS
  paths** from shardhive (005 §3) and mmap/read directly. Immutability +
  0444 make this race-free; shared pages dedupe between processes.
- **Remote readers** stream via shardhive's range-capable blob endpoint
  (005 §3) — read-through, never a mandated copy.
- Only shardhive materializes bytes (import, swarm fetch). **Clients
  never duplicate TB-scale content.** One copy per machine.

## 6. Garbage collection (reserved)

`pin`/`unpin` + `prune` of unreachable blobs with a grace period —
specified once mirror scale demands it. Deleting content is never
designed in passing.
