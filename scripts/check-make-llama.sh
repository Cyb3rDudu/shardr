#!/usr/bin/env bash
# Fresh-checkout mechanics check for `make llama` (issue #26, blocker 5;
# round-3 warning 4): a fresh clone has no bin/ and the old cp failed.
# Runs the deploy step in a TEMP tree (BIN/LLAMA_VERSION overridden via
# make command line) — the prebuilt download stays out of the loop.
# REAL assertions:
#   1. the fake deploy landed in the TEMP bin/ (symlink resolves + greps);
#   2. the repo's build trees are byte-identical before/after.
# Hashing is portable (sha256sum on Linux, shasum on macOS) — a missing
# hasher on either platform must fail the check, not pass it vacuously.
set -eu
cd "$(git rev-parse --show-toplevel)"

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"
  else shasum -a 256 "$1"
  fi
}
hash_tree() { # "<sha>  <path>" for every file under the given roots
  find "$@" -type f 2>/dev/null | LC_ALL=C sort | while read -r f; do hash_file "$f"; done
}

repo_before="$(hash_tree bin .llama-bin)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin/llama-bTEST0"
printf '#!/bin/sh\nfake-llama-server\n' > "$TMP/bin/llama-bTEST0/llama-server"
chmod +x "$TMP/bin/llama-bTEST0/llama-server"
make BIN="$TMP/bin" LLAMA_VERSION=bTEST0 deploy-llama >/dev/null
test -x "$TMP/bin/llama-server"
# the deployed link must resolve (relative targets resolve next to the
# link) and point at the fake binary
tgt="$(readlink "$TMP/bin/llama-server")"
case "$tgt" in
  /*) resolved="$tgt" ;;
  *) resolved="$TMP/bin/$tgt" ;;
esac
grep -q fake-llama-server "$resolved"
# (isolation is proven by the byte-identical before/after check below —
# a developer-run `make llama` may legitimately populate bin/)
repo_after="$(hash_tree bin .llama-bin)"
if [ "$repo_before" != "$repo_after" ]; then
  echo "repo build artifacts changed during the check — isolation broken" >&2
  exit 1
fi
echo "fresh-checkout deploy (temp-isolated, repo untouched): OK"
