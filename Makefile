BIN ?= bin
LLAMA_BUILD_DIR ?= .llama-build

# Pinned llama.cpp release (002 strand design: managed subprocess, no cgo).
LLAMA_VERSION := v0.3.0
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
	rm -rf $(LLAMA_BUILD_DIR)
	git clone --depth 1 --branch $(LLAMA_VERSION) https://github.com/ggml-org/llama.cpp $(LLAMA_BUILD_DIR)
	cmake -S $(LLAMA_BUILD_DIR) -B $(LLAMA_BUILD_DIR)/build -DLLAMA_SERVER=ON -DBUILD_SHARED_LIBS=OFF
	cmake --build $(LLAMA_BUILD_DIR)/build --config Release -j --target llama-server

# deploy-llama copies the built binary into BIN — its own target so the
# fresh-checkout mechanics test can exercise the copy WITHOUT the ~1 GB
# clone+build. mkdir -p first: a fresh clone has no BIN and cp alone
# fails (issue #26, blocker 5).
deploy-llama:
	@mkdir -p $(dir $(LLAMA_SERVER))
	cp $(LLAMA_BUILD_DIR)/build/bin/llama-server $(LLAMA_SERVER)
	@echo ">> $(LLAMA_SERVER) ready (pin: $(LLAMA_VERSION))"

# check-llama-deploy: fresh-checkout mechanics in a TEMP tree (never
# touches the repo's real bin/ or .llama-build).
check-llama-deploy:
	@bash scripts/check-make-llama.sh

test:
	go test ./...

clean:
	rm -rf $(LLAMA_BUILD_DIR) $(BIN)
