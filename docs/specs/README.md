# Spec Index

| Spec | Surface | Status | Vectors |
| --- | --- | --- | --- |
| 000 | References & URI scheme | **Candidate v1 (surface)** | `vectors/000-reference.jsonl` |
| 001 | Artifact format (canonical documents) | **Candidate v1 (surface)** | `vectors/001-canonical.jsonl` |
| 002 | Runner, Modelfile & runtime config | Draft v0 | — |
| 003 | CAS | Draft v0 | — |
| 004 | Swarm (torrent mapping) | **Candidate v1 (surface)** | `vectors/004-torrent.jsonl` |
| 005 | shardhive interface, resolution & CLI | Draft v0 | — |

Candidate v1 currently covers protocol-critical wire/addressing/distribution
surfaces: references, canonical artifact documents, torrent mapping. Runtime
contracts, CAS daemon behavior, and interface remain Draft v0.

A surface advances to Candidate v1 only with machine-checkable vectors for
the places implementors would otherwise get creative; existing code tests
for a surface prove an implementation slice, not the specification as an
interoperable contract.
