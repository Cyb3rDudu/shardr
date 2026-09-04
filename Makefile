BIN ?= bin
LLAMA_BUILD_DIR ?= .llama-build

# llama.cpp version comes from runtime/llama.lock — the SINGLE version
# truth (parsed fail-closed by internal/llamalock; no second pin here).
# Lazily expanded so targets that never use the version (clean, deploy)
# don't compile Go on every make invocation.
LLAMA_VERSION = $(shell go run ./cmd/llama-lock ref)
LLAMA_SERVER := $(BIN)/llama-server

.PHONY: all llama build-llama deploy-llama check-llama-deploy clean test

all: llama

# llama builds the pinned llama.cpp llama-server into BIN (adjacent to
# where `go build -o $(BIN)/ ./cmd/shardr` puts shardr — ResolveBinary
# finds it next to the executable, SHARDR_LLAMA_SERVER overrides
# everything). Recursive make keeps build → deploy strictly ordered even
# under -j. BIN and LLAMA_BUILD_DIR are env-overridable so tests never
# touch a user's real build tree.
llama:
	@$(MAKE) --no-print-directory build-llama
	@$(MAKE) --no-print-directory deploy-llama

build-llama:
	@echo ">> building llama.cpp $(LLAMA_VERSION) (this downloads and compiles ~1 GB)"
	bash scripts/build-llama.sh $(LLAMA_BUILD_DIR)

# deploy-llama copies the built binary into BIN — its own target so the
# fresh-checkout mechanics test can exercise the copy WITHOUT the ~1 GB
# clone+build. mkdir -p first: a fresh clone has no BIN and cp alone
# fails (issue #26, blocker 5).
deploy-llama:
	@mkdir -p $(dir $(LLAMA_SERVER))
	cp $(LLAMA_BUILD_DIR)/llama-server $(LLAMA_SERVER)
	@echo ">> $(LLAMA_SERVER) ready (pin: $(LLAMA_VERSION))"

# check-llama-deploy: fresh-checkout mechanics in a TEMP tree (never
# touches the repo's real bin/ or .llama-build).
check-llama-deploy:
	@bash scripts/check-make-llama.sh

test:
	go test ./...

clean:
	rm -rf $(LLAMA_BUILD_DIR) $(BIN)
