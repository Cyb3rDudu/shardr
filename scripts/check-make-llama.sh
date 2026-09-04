#!/usr/bin/env bash
# Fresh-checkout mechanics check for `make llama` (issue #26, blocker 5;
# round-3 warning 4): a fresh clone has no bin/ and the old cp failed.
# Runs the deploy step in a TEMP tree (BIN/LLAMA_BUILD_DIR env-overridden)
# — the ~1 GB clone+build stays out of the loop. REAL assertions:
#   1. the fake deploy landed in the TEMP bin/;
#   2. repo bin/ is absent or empty afterwards (any content = red);
#   3. the repo's build trees are byte-identical before/after.
set -eu
cd "$(git rev-parse --show-toplevel)"
repo_before="$(find bin .llama-build -type f -exec shasum {} + 2>/dev/null | sort | sha256sum || true)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/build/build/bin"
printf '#!/bin/sh\nfake-llama-server\n' > "$TMP/build/build/bin/llama-server"
chmod +x "$TMP/build/build/bin/llama-server"
# build-llama.sh exposes the binary as $LLAMA_BUILD_DIR/llama-server
cp "$TMP/build/build/bin/llama-server" "$TMP/build/llama-server"
BIN="$TMP/bin" LLAMA_BUILD_DIR="$TMP/build" make deploy-llama >/dev/null
test -x "$TMP/bin/llama-server"
grep -q fake-llama-server "$TMP/bin/llama-server"
if [ -d bin ] && [ -n "$(ls -A bin 2>/dev/null)" ]; then
  echo "repo bin/ is not empty after the temp-isolated check — isolation broken" >&2
  exit 1
fi
repo_after="$(find bin .llama-build -type f -exec shasum {} + 2>/dev/null | sort | sha256sum || true)"
if [ "$repo_before" != "$repo_after" ]; then
  echo "repo build artifacts changed during the check — isolation broken" >&2
  exit 1
fi
echo "fresh-checkout deploy (temp-isolated, repo untouched): OK"
