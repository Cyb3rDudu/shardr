#!/bin/sh
# docs/check-links.sh — verify relative links in the repository's Markdown
# documentation. Succeeds silently (exit 0) when every relative link target
# exists; fails (exit 1) listing every dead link as file:line -> target.
#
# The strict reference is `mkdocs build --strict` (which resolves intra-site
# links); this script is the cheap filesystem twin, run locally and in CI.
# Scope = the published doc set: README.md plus every MkDocs page under
# docs/ that the site renders (anything not excluded in mkdocs.yml:
# docs/specs/*, docs/_inventory.md, docs/requirements.txt, docs/mkdocs.yml,
# docs/user/getting-started.md). Relative links resolve from the file's own
# directory; anchors are ignored (file existence only). Absolute URLs
# (http/https/mailto) are not fetched — no network.
set -e

cd "$(dirname "$0")/.."
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Enumerate published sources (path style "docs/…" or "README.md").
{
	find docs -type f -name '*.md'
	echo README.md
} | grep -vE '^docs/specs/' \
	| grep -vE '(^docs/_inventory\.md$|^docs/requirements\.txt$|^docs/mkdocs\.yml$|^docs/user/getting-started\.md$)' \
	> "$tmp.src"

while IFS= read -r f; do
	dir=$(dirname "$f")
	grep -noE '\]\([^)]+\)' "$f" | sed 's/^\([0-9]*\):\](\(.*\))$/\1 \2/' |
	while read -r lineno target; do
		case "$target" in
		http://*|https://*|mailto:*) continue ;;
		esac
		path="${target%%#*}"
		[ -z "$path" ] && continue
		# Directory/fragment-only links (self-page) are always valid.
		[ -e "$dir/$path" ] || echo "DEAD LINK: $f:$lineno -> $target"
	done
done < "$tmp.src" > "$tmp"
rm -f "$tmp.src"

if [ -s "$tmp" ]; then
	cat "$tmp" >&2
	echo "link check: FAILED" >&2
	exit 1
fi
echo "link check: all relative links resolve"
