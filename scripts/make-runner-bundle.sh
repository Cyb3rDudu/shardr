#!/usr/bin/env bash
# Package the shardr runner bundle for one platform:
#   shardr-runner_<version>_<platform>.tar.gz
#     shardr, llama-server, BUILDINFO.json, LICENSES/
# Usage: make-runner-bundle.sh <platform> <bindir-with:shardr,llama-server,llama-LICENSE> <outdir>
#        <version> from env or arg 4 (defaults to short HEAD).
set -euo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"
platform=$1; bindir=$2; outdir=$3; version=${4:-$(git rev-parse --short HEAD)}

lock=$(go run ./cmd/llama-lock validate)
llama_ref=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ref"])')
llama_commit=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["commit"])')
llama_sha=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["source_sha256"])')

stage=$(mktemp -d); trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/shardr-runner/LICENSES"
cp "$bindir/shardr" "$bindir/llama-server" "$stage/shardr-runner/"
cp "$bindir/llama-LICENSE" "$stage/shardr-runner/LICENSES/llama.cpp-MIT"
cat > "$stage/shardr-runner/BUILDINFO.json" <<EOF
{
  "shardr_commit": "$(git rev-parse HEAD)",
  "llama_ref": "$llama_ref",
  "llama_commit": "$llama_commit",
  "llama_source_sha256": "$llama_sha",
  "platform": "$platform",
  "compiler": "$(cc --version | head -1)",
  "cmake": "$(cmake --version | head -1)",
  "build_options": "-DLLAMA_SERVER=ON -DBUILD_SHARED_LIBS=OFF -DCMAKE_BUILD_TYPE=Release",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
mkdir -p "$outdir"
name="shardr-runner_${version}_${platform}"
tar -czf "$outdir/$name.tar.gz" -C "$stage" shardr-runner
bash scripts/sha256sums.sh "$outdir" "$name.tar.gz"
echo ">> $outdir/$name.tar.gz"
