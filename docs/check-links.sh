#!/bin/sh
# docs/check-links.sh — verify relative links in the published Markdown docs.
# Succeeds silently (exit 0) when every relative link target exists and every
# anchor resolves; fails (exit 1) listing each failure as file:line -> target.
#
# The published set is derived from mkdocs.yml via docs/list-published.py (the
# same source the site build uses), so this script never keeps a second,
# hand-maintained exclusion list. Relative link targets are resolved from each
# file's own directory; absolute URLs (http/https/mailto) are never fetched
# (no network).
#
# Anchors: `mkdocs build --strict` only reports missing anchors at INFO level,
# so it does NOT fail the build on them. This script closes that gap — when a
# rendered site exists in _site/ (run `mkdocs build` first), a link of the
# form page.md#frag or #frag is checked against the `id="frag"` actually
# emitted in the corresponding built HTML page. Without _site/ the anchor
# check is skipped (file-existence checks always run).
set -e

cd "$(dirname "$0")/.."
tmp=$(mktemp)
published=$(mktemp)
trap 'rm -f "$tmp" "$published"' EXIT

script_dir=$(dirname "$0")
python3 "$script_dir/list-published.py" > "$published"

site_available=0
[ -d _site ] && site_available=1

# Map a source .md path to the HTML file MkDocs builds for it
# (use_directory_urls: true — index.md -> <dir>/index.html, else
# <dir>/<stem>/index.html). Echoes nothing for non-md paths or README.
built_html() {
	case "$1" in
	docs/*.md) ;;
	*) return 0 ;;
	esac
	case "$1" in
	*/index.md) printf '%s\n' "_site/${1#docs/}" | sed 's#index\.md$#index.html#' ;;
	*) printf '%s\n' "_site/${1#docs/}" | sed 's#\.md$#/index.html#' ;;
	esac
}

while IFS= read -r f; do
	dir=$(dirname "$f")
	grep -noE '\]\([^)]+\)' "$f" | sed 's/^\([0-9]*\):\](\(.*\))$/\1 \2/' |
	while read -r lineno target; do
		case "$target" in
		http://*|https://*|mailto:*) continue ;;
		esac
		path="${target%%#*}"
		frag=""
		case "$target" in
		*\#*) frag="${target#*\#}" ;;
		esac
		if [ -z "$path" ]; then
			# In-page anchor: validate against this file's own built page.
			[ -n "$frag" ] || continue
			src_target="$f"
		else
			src_target="$dir/$path"
			if [ ! -e "$src_target" ]; then
				echo "DEAD LINK: $f:$lineno -> $target"
				continue
			fi
		fi
		[ -n "$frag" ] || continue
		[ "$site_available" = 1 ] || continue
		html=$(built_html "$src_target")
		[ -n "$html" ] || continue
		[ -e "$html" ] || continue
		if ! grep -qF "id=\"$frag\"" "$html"; then
			echo "MISSING ANCHOR: $f:$lineno -> $target"
		fi
	done
done < "$published" > "$tmp"

if [ -s "$tmp" ]; then
	cat "$tmp" >&2
	echo "link check: FAILED" >&2
	exit 1
fi
if [ "$site_available" = 1 ]; then
	echo "link check: all relative links resolve (anchors checked against _site/)"
else
	echo "link check: all relative links resolve (anchors NOT checked — build first)"
fi
