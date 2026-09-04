# Conventions

How changes land in this repository.

## Linear history, no merge commits

`main` is a pearl chain: every branch lands by rebase/cherry-pick as
fast-forward-able commits; `git merge` (and `--no-ff`) is not used.
Rationale: readable `git log`, precise bisect, changelog automation
without merge noise, clean `git revert` continuity. Branch cohesion
lives in the issue/PR references inside commit messages, not in merge
topology. If two branches conflict, the second rebases onto the first —
conflicts are resolved, never "reconciled" with a merge commit.

## Conventional Commits

```
feat(imports): local + HF import machinery per 001 §8, convergence-proven
fix(api): contract breaks from PR #13 review + R3 tag scoping
docs(specs): R7 — v1 /import/bt webseed bootstrap
test(swarm): mutations for layers + hints persistence guards
refactor(artifact): production 001 validator with E_* classes
```

Types in use: `feat`, `fix`, `docs`, `test`, `refactor`. Scopes name
the area (`api`, `artifact`, `cas`, `imports`, `swarm`, `specs`, …).
Commit bodies explain the *why* and reference the spec sections and
issues they implement (e.g. "001 §8.6", "Refs #16").

## Worktrees, not dirty main

Parallel work happens in separate git worktrees/branches, never in a
shared checkout. Before every commit: `git status` — if the tree is not
clean (foreign dirty files, uncommitted leftovers), sort that out first
(committed, stashed, or explicitly documented as foreign); never commit
on top of someone else's dirty state.

## Review discipline

- Every code change is reviewed over the **committed range** before
  merge; review findings are fixed in follow-up commits, verified like
  the original work.
- Verification claims are proven, not asserted: CLI examples are really
  run, API shapes match the test suite, contract tests fail without the
  behavior they pin (see [testing.md](testing.md#mutation-discipline)).
- CI must be green on both runners (ubuntu, macOS) before merge.

## Specs are the protocol source of truth — gaps are reported, not decided

`docs/specs/` defines protocol behavior; code implements it. When
implementation surfaces a spec gap or divergence, it is **reported**
(a spec comment on the relevant issue/PR, or an issue) — never silently
decided in code. Spec changes land as their own reviewed branches
(`docs(specs): …`). The specs' error-class inventory and the code's
`E_*` constants are kept in sync the same way: drift is a finding, not
a fact.

## Language

Repository language is English — code, comments, commits, PRs, issues,
and docs. Specs use RFC-style MUST/SHOULD language and number their
sections; references like "001 §8.6" mean spec `001-…`, section 8.6.
