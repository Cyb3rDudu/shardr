# Quickstart

From a fresh checkout to a chat completion. Every command below was
executed for real on macOS (Apple Silicon); outputs are the real ones,
paths generalized. Linux/x86-64 differs only in the fetched runtime
platform.

## 1. Build everything

`make all` builds both binaries and fetches the pinned llama.cpp
runtime (digest-verified, from `runtime/llama.lock` — see
[Runtime & releases](../operations/releases.md)):

```console
$ make all
>> building shardr + shardhive into bin
go build -o bin/shardr ./cmd/shardr
go build -o bin/shardhive ./cmd/shardhive
>> fetching prebuilt llama.cpp b10684 (darwin_arm64)
fetched darwin_arm64 -> bin/llama-b10684 (sha256 8310138372444cbe…)
ln -sf llama-b10684/llama-server bin/llama-server
>> bin/llama-server ready (pin: b10684)
```

(On an unauthenticated machine the GitHub release API may answer 403 —
export a token: `GITHUB_TOKEN=$(gh auth token) make all`.)

## 2. Start the daemon

```console
$ ./bin/shardhive serve
shardhive: swarm: seeding 1 artifact(s)
shardhive 0.0.1-dev listening on /var/folders/…/T/shardhive-501/shardhive.sock
```

In a second shell the CLI speaks to it over the Unix socket (the
socket permission is the access boundary — mode 0600):

```console
$ ./bin/shardr models
minicpmv/mmproj    index present    raw
```

## 3. Import a model

Local import (namespace is mandatory — files must be regular files;
filenames drive quant classification, so keep quant tokens lowercase):

```console
$ ./bin/shardr import local /tmp/toy-model --as gold/smollm2-135m-instruct
# job terminal state:
#   state done  quants [q4_k_m]  skipped 0
```

The same flow works over the API (`POST /v1/import/local`), from
Hugging Face (`shardr import hf <repo>` — revision-pinned to the
resolved commit), or as an anchored
[catalog pull](../user/catalog.md) (`shardr catalog search <terms>` →
`shardr pull <owner/repo>`). Note the operational reality of catalog
pulls: torrent metadata comes from peers, so a listing with zero
seeders online fails loudly after two minutes — see
[Troubleshooting](../operations/troubleshooting.md#partial-fills-unreachable-sources).

## 4. Check the inventory

```console
$ ./bin/shardr models
gold/smollm2-135m-instruct    index present    q4_k_m,q4km,raw
minicpmv/mmproj               index present    raw
```

## 5. Run the model

```console
$ ./bin/shardr serve shardr:///gold/smollm2-135m-instruct:q4_k_m --id docs-e2e
resolve shardr:///gold/smollm2-135m-instruct:q4_k_m
spawn <worktree>/bin/llama-server
  weights <cas>/blobs/sha256/ed/5fa30c48… (zero-copy mmap)
serving shardr:///gold/smollm2-135m-instruct:q4_k_m
  id       docs-e2e
  endpoint http://127.0.0.1:63441
```

The weights path is the CAS blob itself — llama-server mmaps it
directly, no copy (zero-copy serving). `shardr run <ref>` is the same
flow in the foreground (endpoint from llama-server's own startup log);
`shardr stop docs-e2e` stops a background instance (SIGTERM, SIGKILL
after 30 s).

## 6. Chat completion (OpenAI-compatible)

```console
$ curl -s http://127.0.0.1:63441/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"smollm2","messages":[{"role":"user","content":"Name the largest planet in our solar system. One word."}],"max_tokens":10}'
{
  "choices": [
    {
      "finish_reason": "length",
      "index": 0,
      "message": {"role": "assistant",
                  "content": "The largest planet in our solar system is Jupiter,"}
    }
  ],
  "model": "shardr:///gold/smollm2-135m-instruct:q4_k_m",
  "system_fingerprint": "b10684-cc83d7b48",
  "usage": {"completion_tokens": 10, "prompt_tokens": 42, "total_tokens": 52}
}
```

The response names the canonical reference that was served and the
runtime fingerprint (llama ref + upstream commit from the pin) — what
answered is traceable to what was stored.

## 7. Verify the store (optional, explicit)

```console
$ ./bin/shardhive cas verify --all
verify --all: 0 mismatched, 0 missing, 0 state errors
all blobs clean
```

Next: the [concept tour](concept-tour.md) for how the pieces fit
together, or the [references](../references/api.md) for the full API.
