#!/usr/bin/env bash
# Portable SHA256SUMS generation: byte-identical output on Linux and macOS
# ("<hex>  <name>", POSIX sha256sum format, sorted by name, LF endings).
# Usage: sha256sums.sh <dir> [names...]   (no names = all regular files, recursive, relative paths)
set -euo pipefail
dir=$1; shift
if command -v sha256sum >/dev/null 2>&1; then
  hash() { sha256sum "$1" | cut -d' ' -f1; }
else
  hash() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi
cd "$dir"
if [ $# -gt 0 ]; then
  for f in "$@"; do printf '%s  %s\n' "$(hash "$f")" "$f"; done
else
  find . -type f ! -name SHA256SUMS | sed 's|^\./||' | LC_ALL=C sort | while read -r f; do
    printf '%s  %s\n' "$(hash "$f")" "$f"
  done
fi
