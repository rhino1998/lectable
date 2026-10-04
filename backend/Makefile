# Wraps every day-to-day Go command (build/test/vet/tidy). ../env.sh gives
# each recipe what ttsworker's cgo build needs (CGO_LDFLAGS/LD_LIBRARY_PATH
# pointed at the local audio.cpp/llama.cpp builds, the sqlite_fts5 tag) and
# the newer Go toolchain this module's go.mod requires; run.sh and deploy.sh
# source the same file. Override a location on the command line, e.g.
# `make build AUDIOCPP_DIR=/some/other/path`.
SHELL := /bin/bash
export BASH_ENV := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)/env.sh

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

# Builds then launches via run.sh.
run:
	./run.sh --build

# Rebuilds both binaries and restarts the running backend, keeping the old
# binaries as *.prev (see deploy.sh); rollback restarts on those.
deploy:
	./deploy.sh

rollback:
	./deploy.sh --rollback
