BIN ?= bin

# llama.cpp version comes from runtime/llama.lock — the SINGLE version
# truth (parsed fail-closed by internal/llamalock; no second pin here).
# Lazily expanded so targets that never use the version (clean, deploy)
# don't compile Go on every make invocation.
LLAMA_VERSION = $(shell go run ./cmd/llama-lock ref)
LLAMA_SERVER := $(BIN)/llama-server

.PHONY: all llama build-llama deploy-llama check-llama-deploy clean test

all: llama

# llama fetches the PINNED prebuilt llama.cpp release binaries
# (runtime/llama.lock — single truth; owner ruling: never self-build)
# into BIN. The whole extract dir is kept: llama-server loads its dylibs
# via @loader_path, a lone binary is useless on macOS.
LLAMA_VERSION = $(shell go run ./cmd/llama-lock ref)
LLAMA_SERVER := $(BIN)/llama-server
LLAMA_PLATFORM := $(shell uname -s | tr '[:upper:]' '[:lower:]')_$(shell uname -m | tr '[:upper:]' '[:lower:]')

.PHONY: all llama fetch-llama deploy-llama check-llama-deploy clean test

all: llama

llama:
	@$(MAKE) --no-print-directory fetch-llama
	@$(MAKE) --no-print-directory deploy-llama

fetch-llama:
	@echo ">> fetching prebuilt llama.cpp $(LLAMA_VERSION) ($(LLAMA_PLATFORM))"
	go run ./cmd/llama-lock fetch $(LLAMA_PLATFORM) $(BIN)

# deploy-llama links the fetched llama-server into BIN root (ResolveBinary
# finds it next to the shardr executable). Its own target so the
# fresh-checkout mechanics test can exercise the link WITHOUT the download.
deploy-llama:
	@mkdir -p $(BIN)
	ln -sf $$(basename $$(dirname $$(ls -d $(BIN)/llama-b*/llama-server | head -1)))/llama-server $(LLAMA_SERVER)
	@echo ">> $(LLAMA_SERVER) ready (pin: $(LLAMA_VERSION))"

# check-llama-deploy: fresh-checkout mechanics in a TEMP tree (never
# touches the repo's real bin/ or .llama-build).
check-llama-deploy:
	@bash scripts/check-make-llama.sh

test:
	go test ./...

clean:
	rm -rf $(BIN)
