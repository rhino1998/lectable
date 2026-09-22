# Wraps every day-to-day Go command (build/test/vet/tidy) with the env vars
# ttsworker's cgo build actually needs (CGO_LDFLAGS/LD_LIBRARY_PATH pointed
# at local audio.cpp/llama.cpp builds) and the newer Go toolchain this
# module's go.mod requires (go-duckdb/v2's own dependency chain needs
# go >= 1.24; see ~/go-toolchains, used only if present) - see run.sh for the same env
# vars applied to actually launching the server, and backend/CLAUDE.md's
# "Running everything locally" section for the underlying commands this
# replaces. Override any var below on the command line, e.g.
# `make build AUDIOCPP_DIR=/some/other/path`.
AUDIOCPP_DIR ?= $(HOME)/audio.cpp/build/bin
LLAMACPP_DIR ?= $(HOME)/llama-cpp-py-sync/vendor/llama.cpp/build/bin
GO_TOOLCHAIN_DIR ?= $(HOME)/go-toolchains/go1.27.1/bin

export CGO_LDFLAGS := -L$(AUDIOCPP_DIR) -L$(LLAMACPP_DIR)
export LD_LIBRARY_PATH := $(LLAMACPP_DIR):$(AUDIOCPP_DIR):$(LD_LIBRARY_PATH)
ifneq ($(wildcard $(GO_TOOLCHAIN_DIR)),)
export PATH := $(GO_TOOLCHAIN_DIR):$(PATH)
export GOTOOLCHAIN := local
endif

.PHONY: build test vet tidy run

# build only - never launches the server (see run.sh's own --build, which
# does launch it afterward; use `make run` for that instead).
build:
	go build -o ttsworker ./cmd/ttsworker
	go build -o server ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Builds then launches via run.sh (which sets these same env vars itself,
# so this works even if invoked directly instead of through make).
run:
	./run.sh --build
