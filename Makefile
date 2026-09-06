BIN ?= bin

# llama.cpp pin lives in runtime/llama.lock — the SINGLE version truth
# (fail-closed parser in internal/llamalock; no second pin anywhere).
# Runtime = upstream PREBUILT b-release binaries (owner ruling
# 2026-09-05: never self-build). The whole extract dir is kept:
# llama-server loads its dylibs via @loader_path.
LLAMA_VERSION = $(shell go run ./cmd/llama-lock ref)
LLAMA_PLATFORM = $(shell go run ./cmd/llama-lock platform)
LLAMA_SERVER := $(BIN)/llama-server

.PHONY: all llama fetch-llama deploy-llama check-llama-deploy clean test

all: llama

llama:
	@$(MAKE) --no-print-directory fetch-llama
	@$(MAKE) --no-print-directory deploy-llama

fetch-llama:
	@echo ">> fetching prebuilt llama.cpp $(LLAMA_VERSION) ($(LLAMA_PLATFORM))"
	go run ./cmd/llama-lock fetch $(LLAMA_PLATFORM) $(BIN)

# deploy-llama links the fetched llama-server into BIN root (ResolveBinary
# finds it next to the shardr executable). Exact pinned path, no globs:
# a missing fetch is a loud error, never a silent wildcard match. Its own
# target so the fresh-checkout mechanics test can exercise the link
# WITHOUT the download.
deploy-llama:
	@mkdir -p $(BIN)
	@test -x "$(BIN)/llama-$(LLAMA_VERSION)/llama-server" || { echo "E_DEPLOY: $(BIN)/llama-$(LLAMA_VERSION)/llama-server missing — run 'make fetch-llama' first (pin: $(LLAMA_VERSION))" >&2; exit 1; }
	ln -sf llama-$(LLAMA_VERSION)/llama-server $(LLAMA_SERVER)
	@echo ">> $(LLAMA_SERVER) ready (pin: $(LLAMA_VERSION))"

# check-llama-deploy: fresh-checkout mechanics in a TEMP tree (never
# touches the repo's real bin/ — a developer-run `make llama` may
# legitimately populate it; isolation is proven by the byte-identical
# before/after check below).
check-llama-deploy:
	@bash scripts/check-make-llama.sh

test:
	go test ./...

clean:
	rm -rf $(BIN) .llama-bin
