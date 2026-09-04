#!/bin/sh
# docs/check-links.sh — verify relative links in the repo's Markdown
# docs. Fails (exit 1) listing every dead link as file:line -> target.
# Absolute URLs are not fetched (no network); only intra-repo relative
# links are checked, resolved against each file's directory. Anchors
# are ignored (file existence only).
set -e

cd "$(dirname "$0")/.."
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

for f in README.md docs/user/*.md docs/dev/*.md docs/specs/README.md; do
	dir=$(dirname "$f")
	grep -noE '\]\([^)]+\)' "$f" | sed 's/^\([0-9]*\):\](\(.*\))$/\1 \2/' |
	while read -r lineno target; do
		case "$target" in
		http://*|https://*|mailto:*) continue ;;
		esac
		path="${target%%#*}"
		[ -z "$path" ] && continue
		if [ ! -e "$dir/$path" ]; then
			echo "DEAD LINK: $f:$lineno -> $target"
		fi
	done
done > "$tmp"

if [ -s "$tmp" ]; then
	cat "$tmp" >&2
	echo "link check: FAILED" >&2
	exit 1
fi
echo "link check: all relative links resolve"
