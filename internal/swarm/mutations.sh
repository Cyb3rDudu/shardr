#!/usr/bin/env bash
# Mutation testing: every critical guard must have a test that goes RED
# when the guard is removed. Applies one mutation at a time against the
# COMMITTED state, runs the guarding test, expects failure, restores.
# Run from the repo root: bash internal/swarm/mutations.sh
set -u
cd "$(git rev-parse --show-toplevel)"
[ -z "$(git status --porcelain)" ] || { echo "worktree not clean — commit first"; exit 1; }

pass=0; fail=0
declare -a RESULTS

mutate() { # name file sed-expr test [pkg]
  local name="$1" file="$2" expr="$3" test="$4" pkg="${5:-./...}"
  # sed -i.bak is portable across BSD and GNU sed; the .bak is removed after.
  sed -i.bak "$expr" "$file" && rm -f "$file.bak"
  if ! go build ./... >/dev/null 2>&1; then
    RESULTS+=("BROKEN-BUILD $name"); fail=$((fail+1)); git checkout -- "$file"; return
  fi
  if go test "$pkg" -run "$test" -count=1 >/dev/null 2>&1; then
    RESULTS+=("NOT-RED    $name — test '$test' still green without the guard"); fail=$((fail+1))
  else
    RESULTS+=("RED        $name (test '$test' fails without guard, as required)"); pass=$((pass+1))
  fi
  git checkout -- "$file"
  [ -z "$(git status --porcelain)" ] || { echo "restore failed for $file"; exit 1; }
}

# 1. CAS verify-write gate in the storage seal.
mutate "verify-write-gate" internal/swarm/storage.go \
  's/if err := p.t.parent.store.Put(p.st.spec.Digest, part); err != nil {/if err := error(nil); err != nil {/' \
  'TestStorageRejectsWrongBytes' ./internal/swarm/

# 2. Infohash binding in CheckRecord (record vs derived).
mutate "record-infohash-binding" internal/swarm/recon.go \
  's/if rec.Torrent.Infohash != r.InfohashBtmh {/if false \&\& rec.Torrent.Infohash != r.InfohashBtmh {/' \
  'TestImportBTInfohashBindingAbortsBeforeAnnounce' ./internal/swarm/

# 3. Joined-vs-derived infohash gate in /import/bt (probe: an extra-file
# torrent whose every byte is pin-valid — only this gate fires).
mutate "import-binding-gate" internal/swarm/fill.go \
  's/if recon.InfohashHex != ihHex {/if false \&\& recon.InfohashHex != ihHex {/' \
  'TestImportBTExtraFileTorrentOnlyBindingCatches' ./internal/swarm/

# 4. Pin-mandatory in /import/bt.
mutate "pin-mandatory" internal/api/server.go \
  's/if body.ManifestDigest == "" {/if false \&\& body.ManifestDigest == "" {/' \
  'TestImportBTPinMandatory' ./internal/api/

# 5. Unpinned-file write refusal (WriteAt only — the error message anchors
# the mutation to the write path, not the Completion reader).
mutate "unpinned-write-refusal" internal/swarm/storage.go \
  's/return 0, fmt.Errorf("swarm: write into unpinned file (no CAS digest registered); the client must not request this piece")/return len(b), nil/' \
  'TestStorageRefusesUnpinnedWrites' ./internal/swarm/

# 6. Unregistered-torrent loud refusal (ok forced true; map hit falls back
# to nil spec map — the guard's error return is what the test pins).
mutate "unregistered-loud" internal/swarm/storage.go \
  's/files, ok := c.registry\[key\]/ok := true; files := map[string]FileSpec{}/' \
  'TestStorageUnregisteredTorrentIsLoud' ./internal/swarm/

# 7. Traversal-path rejection at the storage trust boundary.
mutate "traversal-reject" internal/swarm/storage.go \
  's/if !canonicalTorrentPath(fi.Path) {/if false \&\& !canonicalTorrentPath(fi.Path) {/' \
  'TestStorageRejectsTraversalPaths' ./internal/swarm/

# 8. Manifest merkle-root pin enforcement in BuildTorrent.
mutate "merkle-pin" internal/artifact/torrent.go \
  's/if pinned := DigestHex(f.BT.MerkleRoot); pinned != hex.EncodeToString(root\[:\]) {/if false {/' \
  'TestBuildTorrentRejectsLyingMerkleRoot' ./internal/artifact/

# 9. Seed-start re-hash (003 §4).
mutate "seed-rehash" internal/swarm/swarm.go \
  's/if err := c.store.Verify(spec.Digest); err != nil {/if false {/' \
  'TestSeedStartRehashRejectsCorruptedBlob' ./internal/swarm/

# 10. Fill wait target: total files, not missing count (early-return bug class).
mutate "fill-wait-target" internal/swarm/fill.go \
  's/target := len(recon.FileSpecs)/target := len(recon.FileSpecs) - 1/' \
  'TestTwoInstanceE2E' ./internal/swarm/

# 11. Unknown-key loudness in the [swarm] TOML subset (001 §8.7 namespacing:
# config must fail closed on unrecognized keys, not absorb them).
mutate "swarm-config-unknown-key" cmd/shardhive/config.go \
  's/if !swarmKeys\[key\] {/if false \&\& !swarmKeys[key] {/' \
  'TestLoadSwarmConfigUnknownKeyIsLoud' ./cmd/shardhive/

# 12. /v1/ensure with blobs missing but swarm disabled must fail loudly
# naming the config knob (anchored to the ensure path via the missing-blobs
# block, not the /import/bt handler).
# Both s.Swarm == nil guards (handleEnsure + handleImportBT) share the
# pattern; neutering either nil-derefs in the job goroutine. Both tests
# must stay green with the guards present.
mutate "ensure-swarm-off-loud" internal/api/server.go \
  's/if s.Swarm == nil {/if false \&\& s.Swarm == nil {/g' \
  'TestEnsureLocalPresence|TestImportBTPinMandatory' ./internal/api/

echo
for r in "${RESULTS[@]}"; do echo "$r"; done
echo
echo "mutation testing: $pass red (correct), $fail not-red (gaps)"
[ "$fail" -eq 0 ]
