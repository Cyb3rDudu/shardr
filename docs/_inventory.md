# Documentation inventory & migration map

Inventory for the documentation-site strand (Epic #41, stages S1+S2). Every
existing documentation artifact of this repository has a recorded fate here:
**migrated + revised**, **dissolved** (content lives elsewhere, the site
does not own it), or **canonical** (never moved — the site summarizes and
links). The canonical source is named per entry.

The site source of truth is `docs/` (the MkDocs `docs_dir`). Pages in
`docs/user/`, `docs/dev/` and any future stage directory are rendered by
MkDocs; anything the site must not publish — `docs/specs/*`, this file,
`requirements.txt`, `mkdocs.yml`, `check-links.sh`, `list-published.py` —
is excluded in `mkdocs.yml` (`exclude_docs`), never deleted.

## Fate table

| Source artifact | Fate | Destination in the site | Rework needed |
| --- | --- | --- | --- |
| root `README.md` | **Stays the shop window** — not migrated into the tree, only gains one prominent site link (`https://cyb3rdudu.github.io/shardr/`) | — (external link in README) | Written separately (Epic #24); S1 only appends the site link. |
| `docs/user/getting-started.md` | **Dissolved** — the first-run/quickstart walkthrough is superseded by the future Get Started → Quickstart page (Stage S5). Not published in S1/S2. | [Get Started → Quickstart](get-started/quickstart.md) (Stage S5) | Its install + daemon + import examples are covered again in S5 with freshly executed outputs. |
| `docs/user/api.md` | **Migrated + revised** (stays the API reference) | [User Guide → API — HTTP v1](user/api.md) | Paths already placeholders; generalized, kept under the same path for README stability. |
| `docs/user/cli.md` | **Migrated + revised** | [User Guide → CLI reference](user/cli.md) | — |
| `docs/user/config.md` | **Migrated + revised** | [User Guide → Configuration](user/config.md) | Machine-specific user-config introspection generalized. |
| `docs/user/errors.md` | **Migrated** | [User Guide → Error reference](user/errors.md) | — |
| `docs/user/importing.md` | **Migrated + revised** | [User Guide → Importing models](user/importing.md) | — |
| `docs/user/swarm.md` | **Migrated + revised** | [User Guide → Swarm & seeding](user/swarm.md) | — |
| `docs/dev/architecture.md` | **Migrated + revised** | [Developer Guide → Architecture overview](dev/architecture.md) | — |
| `docs/dev/conventions.md` | **Migrated + revised** | [Developer Guide → Conventions](dev/conventions.md) | — |
| `docs/dev/layout.md` | **Migrated + revised** | [Developer Guide → Code layout](dev/layout.md) | — |
| `docs/dev/testing.md` | **Migrated + revised** | [Developer Guide → Testing](dev/testing.md) | — |
| `docs/specs/000–005` + `docs/specs/README.md` + `docs/specs/vectors/*` | **Canonical — remain untouched** in the repo, not published as site pages. | Excluded from the site (`exclude_docs: specs/`); the site pages summarize and link to the spec files. | None. |
| `docs/check-links.sh` + `docs/list-published.py` | **Migrated + extended** — the link/anchor checker run by both CI and the local gate, plus its published-set helper. | `docs/check-links.sh`, `docs/list-published.py` (same files) | Reworked: the checker walks the published set derived from the MkDocs config and validates anchors against the built HTML; the previous hard-coded README/user/dev/specs list is gone. |
| root `AGENT.md` | **Stays local/unpublished** (host-specific developer instructions, in `.git/info/exclude`) | — | None. |
| `site/` (product placeholder `site/shardrbay/`) | **Unrelated to the docs site** — remains a repo placeholder. The MkDocs output is redirected to the gitignored `_site/` so `mkdocs build` never clears this tracked placeholder. | — | None. |

## Mapping notes

- **Site dir collision.** MkDocs' default output dir is `site/`, which this
  repo already tracks for the planned `shardrbay` web index. `mkdocs.yml`
  therefore sets `site_dir: _site` (gitignored). The docs GitHub Actions
  workflow uploads `_site/`.
- **Read-only artifact policy.** None of the above dissolved destinations
  is moved or renamed in a way that breaks the README's existing links to
  `docs/user/` and `docs/dev/`; those directories stay put and the nav maps
  labels onto the existing paths.

## Vendored assets

The mermaid script the site runs is a committed file, not executable
third-party code fetched at page view. Provenance:

| Asset | Version | SHA-256 |
| --- | --- | --- |
| `docs/assets/mermaid.min.js` | mermaid 10.9.8 | `8d607d7ef1d077a8aa202e18e62212bfa992c68bfeabc5cf45d51a128fe6675d` |

Boundary: the MkDocs Material JS bundle carries internal fallback loaders
that reference `unpkg.com` for `resize-observer-polyfill` and for mermaid.
Both are conditional — Material only loads its own mermaid when the global
`mermaid` is undefined, and this configuration always defines it from the
vendored file — so those URLs are not fetched in this setup. The Material
bundle itself is not vendored (it is the theme's own build artifact).

## Link guarantee

Every nav entry in `mkdocs.yml` points at a file that exists in `docs/`.
Pages that later stages (S3 References, S4 Operations, S5 Get Started) will
fill are committed now as short "in progress" stubs, so the nav is complete.

The link gate is two-part:

- `mkdocs build --strict` fails on broken intra-site links, but it reports
  missing anchors only at INFO level and therefore does **not** fail on them.
- `docs/check-links.sh` therefore also validates anchors: after a build it
  matches every `page.md#frag` / `#frag` link against the `id="frag"`
  actually emitted in the built HTML under `_site/`. Without a build the
  anchor check is skipped (printed as such).
