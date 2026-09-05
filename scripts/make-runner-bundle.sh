#!/usr/bin/env bash
# Package the shardr runner bundle for one platform, REPRODUCIBLY:
# deterministic tar (sorted names, fixed mtime/uid/gid, gzip mtime 0),
# built_at = source-commit epoch. Same commit + same pinned binaries =
# same bytes, so a re-run of the release pipeline hits the idempotent
# noop instead of the no-overwrite hard-fail.
#
# The prebuilt llama runtime ships as the WHOLE extract dir (llama-server
# loads its dylibs via @loader_path — a lone binary is useless).
#
# Usage: make-runner-bundle.sh <platform> <distdir> <outdir>
#        <distdir> holds llama-dist/ (extracted prebuilt), shardr, llama-version.txt
#        version from $SHARDR_VERSION or arg 4 (defaults to short HEAD).
set -euo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"
platform=$1; dist=$2; outdir=$3; version=${4:-${SHARDR_VERSION:-$(git rev-parse --short HEAD)}}
dist=$(cd "$dist" && pwd)

lock=$(go run ./cmd/llama-lock validate)
llama_ref=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["ref"])')
llama_commit=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["commit"])')
llama_sha=$(printf '%s' "$lock" | python3 -c 'import sys,json;print(json.load(sys.stdin)["assets"]["'"$platform"'"]["sha256"])')
llama_version=$(tr '\n' ' ' < "$dist/llama-version.txt" | sed 's/  */ /g' | sed 's/ $//')

# Determinism anchor: the source commit's timestamp, not wall clock.
epoch=$(git log -1 --format=%ct)
built_at=$(git log -1 --format=%cI)

stage=$(mktemp -d); trap 'rm -rf "$stage"' EXIT
r="$stage/shardr-runner"
mkdir -p "$r/LICENSES/go" "$r/llama"
cp "$dist/shardr" "$r/"
cp -R "$dist/llama-dist/." "$r/llama/"
# upstream license ships inside the prebuilt archive
[ -f "$r/llama/LICENSE" ] && cp "$r/llama/LICENSE" "$r/LICENSES/llama.cpp"
# shardr's own license — bundled as soon as the repo ships a LICENSE file.
for l in LICENSE LICENSE.md LICENSE.txt; do
  if [ -f "$l" ]; then cp "$l" "$r/LICENSES/shardr"; break; fi
done
# Go dependency licenses straight from the module cache (stdlib go only).
while read -r mod dir; do
  case "$mod" in github.com/Cyb3rDudu/shardr*|"") continue ;; esac
  for l in LICENSE LICENSE.md LICENSE.txt COPYING COPYING.LESSER NOTICE; do
    if [ -f "$dir/$l" ]; then
      mkdir -p "$r/LICENSES/go/$mod"
      cp "$dir/$l" "$r/LICENSES/go/$mod/$l"
      break
    fi
  done
done < <(go list -m -f '{{.Path}} {{.Dir}}' all)

cat > "$r/BUILDINFO.json" <<EOF
{
  "shardr_commit": "$(git rev-parse HEAD)",
  "llama_ref": "$llama_ref",
  "llama_commit": "$llama_commit",
  "llama_asset_sha256": "$llama_sha",
  "llama_server_version": "$llama_version",
  "runtime_source": "upstream prebuilt release binaries (never self-built)",
  "platform": "$platform",
  "built_at": "$built_at"
}
EOF

mkdir -p "$outdir"
name="shardr-runner_${version}_${platform}"
SOURCE_DATE_EPOCH="$epoch" python3 - "$stage" "$outdir/$name.tar.gz" "$epoch" "$r" <<'PYEOF'
import gzip, io, os, sys, tarfile
stage, out, epoch = sys.argv[1], sys.argv[2], int(sys.argv[3])
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w") as tf:
    for root, dirs, files in os.walk(stage):
        dirs.sort()
        for f in sorted(files):
            p = os.path.join(root, f)
            arc = os.path.relpath(p, stage)
            if os.path.islink(p):
                ti = tarfile.TarInfo(arc)
                ti.type = tarfile.SYMTYPE
                ti.linkname = os.readlink(p)
                ti.mtime, ti.uid, ti.gid, ti.uname, ti.gname = epoch, 0, 0, "", ""
                tf.addfile(ti)
            else:
                ti = tf.gettarinfo(p, arcname=arc)
                ti.mtime, ti.uid, ti.gid, ti.uname, ti.gname = epoch, 0, 0, "", ""
                ti.mode = 0o755 if ti.mode & 0o111 else 0o644
                with open(p, "rb") as fh:
                    tf.addfile(ti, fh)
with open(out, "wb") as fh:
    with gzip.GzipFile(filename="", mode="wb", fileobj=fh, mtime=0) as gz:
        gz.write(buf.getvalue())
PYEOF
bash scripts/sha256sums.sh "$outdir" "$name.tar.gz"
echo ">> $outdir/$name.tar.gz"
