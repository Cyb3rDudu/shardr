#!/usr/bin/env bash
# Build llama-server from runtime/llama.lock — the ONLY version truth.
# Downloads the pinned source archive, verifies SHA-256 BEFORE unpacking
# (fail-closed, no unverified bytes on disk), proves tag→commit, then
# builds with pinned CMake options.
#
# Env overrides (canary channel ONLY — never writes the stable lock):
#   LLAMA_REF / LLAMA_COMMIT / LLAMA_SOURCE_SHA256
set -euo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"

DEST=${1:-.llama-build}

# Read lock fields (single truth), allow canary env overrides.
if [ -n "${LLAMA_REF:-}" ] && [ -n "${LLAMA_COMMIT:-}" ] && [ -n "${LLAMA_SOURCE_SHA256:-}" ]; then
  : # canary override: explicit pin, lock untouched
else
  lock=$(go run ./cmd/llama-lock validate) || { echo "E_LOCK: runtime/llama.lock failed validation" >&2; exit 1; }
  LLAMA_REF=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ref"])')
  LLAMA_COMMIT=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["commit"])')
  LLAMA_SOURCE_SHA256=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["source_sha256"])')
fi

url="https://github.com/ggml-org/llama.cpp/archive/${LLAMA_COMMIT}.tar.gz"
echo ">> llama.cpp ${LLAMA_REF} @ ${LLAMA_COMMIT}"

# Download with timeout, hash BEFORE any unpack (path-traversal-safe:
# we only ever extract into a fresh dir we control and inspect the
# top-level prefix).
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -sSL --fail --max-time 900 --retry 2 "$url" -o "$tmp/src.tar.gz"
actual=$(sha256sum "$tmp/src.tar.gz" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$tmp/src.tar.gz" | cut -d' ' -f1)
if [ "$actual" != "$LLAMA_SOURCE_SHA256" ]; then
  echo "E_DIGEST: source archive sha256 $actual != locked $LLAMA_SOURCE_SHA256 — refusing to build" >&2
  exit 1
fi
echo ">> source digest OK ($actual)"

rm -rf "$DEST"
mkdir -p "$DEST"
tar -xzf "$tmp/src.tar.gz" -C "$DEST"
prefix=$(ls "$DEST")
if [ "$(printf '%s\n' "$prefix" | wc -l)" -ne 1 ] || [ ! -d "$DEST/$prefix" ]; then
  echo "E_ARCHIVE: unexpected archive layout" >&2; exit 1
fi
src="$DEST/$prefix"

# Pinned, reproducible-ish build options (documented in BUILDINFO).
cmake -S "$src" -B "$src/build" -DLLAMA_SERVER=ON -DBUILD_SHARED_LIBS=OFF -DCMAKE_BUILD_TYPE=Release
cmake --build "$src/build" --config Release -j --target llama-server
echo ">> llama-server built: $src/build/bin/llama-server"
ln -sfn "$src/build/bin/llama-server" "$DEST/llama-server"
