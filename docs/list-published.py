#!/usr/bin/env python3
"""Print the published documentation file set, one repo-relative path per line.

Derived from the MkDocs configuration (mkdocs.yml) via MkDocs' own loader, so
it is the same source the site build uses: a page is in the set when MkDocs
includes it after `exclude_docs`. README.md (the repository shop window) is
appended, since it links into the docs tree and is link-checked.

This keeps gates that must act on "published docs only" — the generalizability
grep in .github/workflows/docs.yml and docs/check-links.sh — from hand-
maintaining a second exclusion list that drifts from mkdocs.yml.

Falls back to a plain filesystem walk (docs/**/*.md minus the known excluded
paths) when MkDocs is not importable, so the callers still work without the
docs toolchain installed.
"""
import os
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXCLUDED_FALLBACK = (
    "docs/_inventory.md",
    "docs/user/getting-started.md",
    "docs/list-published.py",
    "docs/check-links.sh",
)


def via_mkdocs() -> list[str]:
    from mkdocs.config import load_config
    from mkdocs.structure.files import get_files

    cwd = os.getcwd()
    try:
        os.chdir(REPO_ROOT)
        config = load_config()
        files = get_files(config)
    finally:
        os.chdir(cwd)

    out = []
    for f in files:
        if f.src_uri.endswith(".md") and f.inclusion.is_included():
            out.append(os.path.join("docs", f.src_uri))
    return sorted(out)


def via_walk() -> list[str]:
    out = []
    for dirpath, _dirs, filenames in os.walk(os.path.join(REPO_ROOT, "docs")):
        for name in filenames:
            if not name.endswith(".md"):
                continue
            path = os.path.relpath(os.path.join(dirpath, name), REPO_ROOT)
            if path.startswith("docs/specs/"):
                continue
            if path in EXCLUDED_FALLBACK:
                continue
            out.append(path)
    return sorted(out)


def main() -> int:
    try:
        paths = via_mkdocs()
    except Exception:
        paths = via_walk()
    paths.append("README.md")
    for path in paths:
        print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main())
