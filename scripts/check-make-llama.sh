#!/usr/bin/env bash
# Fresh-checkout mechanics check for `make llama` (issue #26, blocker 5):
# a fresh clone has no bin/ and the old cp failed. This script runs the
# deploy step in a TEMP tree (BIN and LLAMA_BUILD_DIR env-overridden) —
# the repo's real build artifacts are never touched, and the ~1 GB
# clone+build stays out of the loop. Exit 0 = deploy works from nothing.
set -eu
cd "$(git rev-parse --show-toplevel)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/build/build/bin"
printf '#!/bin/sh\nfake-llama-server\n' > "$TMP/build/build/bin/llama-server"
chmod +x "$TMP/build/build/bin/llama-server"
BIN="$TMP/bin" LLAMA_BUILD_DIR="$TMP/build" make deploy-llama >/dev/null
test -x "$TMP/bin/llama-server"
grep -q fake-llama-server "$TMP/bin/llama-server"
test ! -e bin -o -z "$(ls -A bin 2>/dev/null | grep -v llama-server || true)" || true
echo "fresh-checkout deploy (temp-isolated): OK"
