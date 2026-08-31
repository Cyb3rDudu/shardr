# 003 — CAS Format

Purpose: define the content-addressed storage format: chunking, hashing, dedup, and on-disk layout.

## Open Questions

- Chunk size strategy (fixed vs content-defined)?
- Hash algorithm (blake3? sha256?)?
- Garbage collection of unreferenced chunks?
