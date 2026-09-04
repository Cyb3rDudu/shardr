#!/usr/bin/env bash
# Publish (or verify) the runner release. Idempotent by identity:
#   shardr-runner-<shardr-version>-llama-<llama-ref>
# If the release already exists with the SAME SHA256SUMS bytes → noop (exit 0).
# Same identity with DIFFERENT bytes → hard error (exit 1, nothing touched).
# Usage: publish-release.sh <distdir>   (expects *.tar.gz + SHA256SUMS in distdir)
set -euo pipefail
dist=$1
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
version=${SHARDR_VERSION:-$(git rev-parse --short HEAD)}
llama_ref=$(go run ./cmd/llama-lock ref)
rel="shardr-runner-${version}-llama-${llama_ref}"

cd "$dist"
bash "$root/scripts/sha256sums.sh" . > SHA256SUMS

if gh release view "$rel" >/dev/null 2>&1; then
  gh release download "$rel" -p SHA256SUMS -O /tmp/existing-sums.$$ 2>/dev/null || true
  if [ -f /tmp/existing-sums.$$ ] && cmp -s /tmp/existing-sums.$$ SHA256SUMS; then
    echo ">> release $rel already published with identical bytes — idempotent noop"
    rm -f /tmp/existing-sums.$$
    exit 0
  fi
  echo "E_RELEASE: $rel already exists with DIFFERENT bytes — refusing to overwrite" >&2
  rm -f /tmp/existing-sums.$$
  exit 1
fi

gh release create "$rel" --title "$rel" --notes-preamble "Runner bundle: shardr $(git rev-parse --short HEAD), llama.cpp $llama_ref (commit pinned in runtime/llama.lock). Checksums in SHA256SUMS; provenance in each archive's BUILDINFO.json." ./*.tar.gz SHA256SUMS
echo ">> published $rel"
