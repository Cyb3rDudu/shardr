#!/usr/bin/env bash
# Fresh-checkout mechanics check for `make llama` (issue #26, blocker 5):
# on a fresh clone bin/ does not exist and the old cp failed with
# "cp: bin/llama-server: No such file or directory". This script fakes
# the built binary and runs ONLY the deploy step — the ~1 GB clone+build
# stays out of the loop. Exit 0 = the deploy works from nothing.
set -eu
cd "$(git rev-parse --show-toplevel)"
rm -rf bin .llama-build
mkdir -p .llama-build/build/bin
printf '#!/bin/sh
fake-llama-server
' > .llama-build/build/bin/llama-server
chmod +x .llama-build/build/bin/llama-server
make deploy-llama >/dev/null
test -x bin/llama-server
grep -q fake-llama-server bin/llama-server
rm -rf bin .llama-build
echo "fresh-checkout deploy: OK"
