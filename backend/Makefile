# Wraps every day-to-day Go command (build/test/vet/tidy) with the env vars
# ttsworker's cgo build actually needs (CGO_LDFLAGS/LD_LIBRARY_PATH pointed
# at local audio.cpp/llama.cpp builds) and the newer Go toolchain this
# module's go.mod requires (see ~/go-toolchains, used only if present) - see run.sh for the same env
# vars applied to actually launching the server, and backend/CLAUDE.md's
# "Running everything locally" section for the underlying commands this
# replaces. Override any var below on the command line, e.g.
# `make build AUDIOCPP_DIR=/some/other/path`.
AUDIOCPP_DIR ?= $(HOME)/audio.cpp/build-rocm7/bin
LLAMACPP_DIR ?= $(HOME)/llama.cpp-latest/build-live/bin
# ROCm 7.2.4, unpacked user-space (no system install; /opt/rocm is still
# 6.4.4 and on ld.so.conf, so this must come first). Under WSL it reaches the
# GPU through librocdxg, which needs HSA_ENABLE_DXG_DETECTION=1.
ROCM_DIR ?= $(HOME)/rocm7-user/root/opt/rocm-7.2.4
GO_TOOLCHAIN_DIR ?= $(HOME)/go-toolchains/go1.27.1/bin

export CGO_LDFLAGS := -L$(AUDIOCPP_DIR) -L$(LLAMACPP_DIR)
# internal/store's search index needs SQLite's FTS5 compiled in.
export GOFLAGS := -tags=sqlite_fts5 $(GOFLAGS)
export LD_LIBRARY_PATH := $(LLAMACPP_DIR):$(AUDIOCPP_DIR):$(ROCM_DIR)/lib:$(ROCM_DIR)/llvm/lib:$(LD_LIBRARY_PATH)
export HSA_ENABLE_DXG_DETECTION := 1
ifneq ($(wildcard $(GO_TOOLCHAIN_DIR)),)
export PATH := $(GO_TOOLCHAIN_DIR):$(PATH)
export GOTOOLCHAIN := local
endif

.PHONY: build test vet tidy run generate deploy rollback

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

# Regenerates the frontend's/Android client's API types from
# internal/httpapi's DTOs (cmd/apigen).
generate:
	go generate ./internal/httpapi

# Builds then launches via run.sh (which sets these same env vars itself,
# so this works even if invoked directly instead of through make).
run:
	./run.sh --build

# Rebuilds both binaries and restarts the running backend, keeping the old
# binaries as *.prev (see deploy.sh); rollback restarts on those.
deploy:
	./deploy.sh

rollback:
	./deploy.sh --rollback
