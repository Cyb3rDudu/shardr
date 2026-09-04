#!/usr/bin/env bash
# Mutationssicherung for the runner strand's core guards (issue #5 DoD).
# Every mutation removes ONE guard from committed state and proves the
# matching test goes RED. Run from internal/runner or repo root:
#   bash internal/runner/mutations.sh
set -u
cd "$(git rev-parse --show-toplevel)"
pass=0; fail=0

mut() { # name file perl-expr test pkg [extra env]
  local name="$1" file="$2" expr="$3" test="$4" pkg="$5"
  cp "$file" /tmp/mut.bak
  perl -0pi -e "$expr" "$file"
  if go test "$pkg" -run "$test" -timeout 120s >/tmp/mut.log 2>&1; then
    echo "NOT-RED  $name"; fail=$((fail+1))
  else
    echo "RED      $name ($test)"; pass=$((pass+1))
  fi
  cp /tmp/mut.bak "$file"
}

# 1. Unknown-key rejection (002 §2.2): allowlist check gone → per-layer
#    probe red.
mut "unknown-key-reject" internal/runner/overlay.go \
  's/\t\t\tif _, ok := specFor\(k\); !ok \{.*?\n\t\t\t\treturn nil, fmt\.Errorf\("unknown runtime key %q in %s \(002 §7\.1 allowlist: %s\)",\n\t\t\t\t\tk, l\.l, allowlistKeys\(\)\)\n\t\t\t\}\n//s' \
  'TestOverlayUnknownKeyPerLayer' ./internal/runner/

# 2. Advisory machine-neutrality (002 §2.1).
mut "advisory-neutrality" internal/runner/overlay.go \
  's/\t\tif l\.l == LayerAdvisory \{.*?\n\t\t\}\n//s' \
  'TestOverlayAdvisoryMachineNeutralOnly' ./internal/runner/

# 3. Merge precedence: highest layer must replace per key — neuter the
#    sequential overwrite (apply layers in reverse).
mut "merge-precedence" internal/runner/overlay.go \
  's/\tfor _, l := range layers \{\n\t\tfor k, v := range l\.m \{\n\t\t\tmerged\[k\] = v\n\t\t\}\n\t\}/\tfor _, l := range layers {\n\t\tfor k, v := range l.m {\n\t\t\tif _, seen := merged[k]; !seen {\n\t\t\t\tmerged[k] = v\n\t\t\t}\n\t\t}\n\t}/s' \
  'TestOverlayRankHighestWins' ./internal/runner/

# 4. SIGTERM grace deadline (002 §4): kill gone → deadbeat process never
#    dies within the window.
mut "sigterm-deadline" internal/runner/llama.go \
  's/\tif err := p\.Kill\(\); err != nil && !isGone\(err\) \{\n\t\treturn fmt\.Errorf\("E_RUNTIME: SIGKILL %d: %w", pid, err\)\n\t\}\n\treturn nil/\treturn nil/s' \
  'TestTerminateDeadlineSIGKILL' ./internal/runner/

# 5. Zero-copy: -m must be the CAS path — swap in a copy-on-spawn would
#    change argv; assert the argv check directly (weights path leaked to
#    a temp copy).
mut "zero-copy-argv" internal/runner/llama.go \
  's/"-m", req\.Weights,/"-m", req.Weights + ".runner-copy",/' \
  'TestZeroCopyWeightsPath' ./internal/runner/

# 6. CLI pin passthrough (005 §5): guard removed → magnet-only import
#    reaches the daemon.
mut "cli-pin-guard" internal/cli/commands.go \
  's/\tif manifest == "" \{\n\t\treturn fmt\.Errorf\("E_BAD_REQUEST: --manifest <sha256:…> is mandatory — a magnet alone is never trusted \(005 §5\)"\)\n\t\}\n//s' \
  'TestImportBTPinMandatory' ./internal/cli/

# 7. Short-form canonicalization: API must see ONLY canonical URIs —
#    neuter Canonicalize to pass-through.
mut "canonicalize-wire" internal/cli/client.go \
  's/func Canonicalize\(input string, defaultSelector string\) \(string, error\) \{/func Canonicalize(input string, defaultSelector string) (string, error) {\n\tif true { return input, nil }/' \
  'TestShortFormCanonicalizesOnTheWire' ./internal/cli/

# 8. Server pin enforcement survives the CLI: the API guard removed →
#    CLI refusal alone must not be the only barrier.
mut "api-pin-guard" internal/api/server.go \
  's/\tif err := swarm\.ValidatePin\(body\.ManifestDigest\); err != nil \{.*?\n\t\treturn\n\t\}\n//s' \
  'TestImportBTBadPinIs400NoJob' ./internal/api/

# 9. Loopback bind (002 §5.3): 0.0.0.0 instead of 127.0.0.1.
mut "loopback-bind" internal/runner/llama.go \
  's/"--host", "127\.0\.0\.1",/"--host", "0.0.0.0",/' \
  'TestZeroCopyWeightsPath' ./internal/runner/

# 10. Model id = reference (002 §6): alias dropped.
mut "alias-ref" internal/runner/llama.go \
  's/\tif req\.Ref != "" \{.*?\n\t\targv = append\(argv, "--alias", req\.Ref\)\n\t\}\n//s' \
  'TestServeStopLifecycleAgainstDaemon' ./internal/cli/

echo "mutations: $pass red, $fail not-red"
[ "$fail" -eq 0 ] && [ "$pass" -ge 9 ]
