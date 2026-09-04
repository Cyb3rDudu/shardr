BIN := bin

# Pinned llama.cpp release (002 strand design: managed subprocess, no cgo).
LLAMA_VERSION := v0.3.0
LLAMA_SERVER := $(BIN)/llama-server

.PHONY: all llama build-llama deploy-llama clean test

all: llama

# llama builds the pinned llama.cpp llama-server into bin/ (adjacent to
# where `go build -o bin/ ./cmd/shardr` puts shardr — ResolveBinary finds
# it next to the executable, SHARDR_LLAMA_SERVER overrides everything).
# Recursive make keeps build → deploy strictly ordered even under -j.
llama:
	@$(MAKE) --no-print-directory build-llama
	@$(MAKE) --no-print-directory deploy-llama

build-llama:
	@echo ">> building llama.cpp $(LLAMA_VERSION) (this downloads and compiles ~1 GB)"
	rm -rf .llama-build
	git clone --depth 1 --branch $(LLAMA_VERSION) https://github.com/ggml-org/llama.cpp .llama-build
	cmake -S .llama-build -B .llama-build/build -DLLAMA_SERVER=ON -DBUILD_SHARED_LIBS=OFF
	cmake --build .llama-build/build --config Release -j --target llama-server

# deploy-llama copies the built binary into BIN — its own target so a
# fresh-checkout mechanics test can exercise the copy WITHOUT the ~1 GB
# clone+build (scripts/check-make-llama.sh). mkdir -p first: a fresh
# clone has no bin/ and cp alone fails (issue #26, blocker 5).
deploy-llama:
	@mkdir -p $(dir $(LLAMA_SERVER))
	cp .llama-build/build/bin/llama-server $(LLAMA_SERVER)
	@echo ">> $(LLAMA_SERVER) ready (pin: $(LLAMA_VERSION))"

test:
	go test ./...

clean:
	rm -rf .llama-build $(BIN)
