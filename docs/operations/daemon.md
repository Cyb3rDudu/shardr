# Daemon operation

> **In progress** — this Operations page ships with Stage S4. It will cover
> the daemon lifecycle: `serve`, socket path resolution, the 0600 access
> boundary, the CAS layout on disk, and the verify workflow — with executed
> commands.

The behavior it will describe is already specified:
[`docs/specs/003`](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/003-cas-format.md)
(the CAS) and
[`docs/specs/005`](https://github.com/Cyb3rDudu/shardr/blob/main/docs/specs/005-shardhive-interface.md)
(the daemon interface). The command surface is documented in the [User Guide
→ CLI reference](../user/cli.md).
