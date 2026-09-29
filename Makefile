# Repo-wide lint/test entry points. `make check` runs every component's
# linter and tests (`make lint` / `make test` for just one half); each
# lint-*/test-* target also works on its own.
#
# golangci-lint (v2) is expected on PATH or in $(go env GOPATH)/bin:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
# (from backend/, so the module's newer Go toolchain builds it). Config is the
# shared root .golangci.yml.

GO_MODULES := backend audiocpp-go llamacpp-go lac
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)
ANDROID_SDK_ROOT ?= /home/rhino/android-sdk-toolchains
# Local audio.cpp/llama.cpp builds for the cgo modules' tests - same defaults
# as backend/Makefile; override on the command line.
AUDIOCPP_DIR ?= $(HOME)/audio.cpp/build-rocm7/bin
LLAMACPP_DIR ?= $(HOME)/llama.cpp-latest/build-live/bin
# ROCm 7.2.4, unpacked user-space (no system install; /opt/rocm is still
# 6.4.4 and on ld.so.conf, so this must come first). Under WSL it reaches the
# GPU through librocdxg, which needs HSA_ENABLE_DXG_DETECTION=1.
ROCM_DIR ?= $(HOME)/rocm7-user/root/opt/rocm-7.2.4
export CGO_LDFLAGS := -L$(AUDIOCPP_DIR) -L$(LLAMACPP_DIR)
export LD_LIBRARY_PATH := $(LLAMACPP_DIR):$(AUDIOCPP_DIR):$(ROCM_DIR)/lib:$(ROCM_DIR)/llvm/lib:$(LD_LIBRARY_PATH)
export HSA_ENABLE_DXG_DETECTION := 1

.PHONY: check lint lint-go lint-frontend lint-android test test-go test-frontend test-android \
	deploy deploy-backend install-android

check: lint test

lint: lint-go lint-frontend lint-android

lint-go:
	@for m in $(GO_MODULES); do echo "== golangci-lint $$m"; (cd $$m && $(GOLANGCI_LINT) run ./...) || exit 1; done

lint-frontend:
	cd frontend && npm run lint && npm run format:check

lint-android:
	cd android && ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT) ./gradlew -q lintDebug

test: test-go test-frontend test-android

test-go:
	@for m in $(GO_MODULES); do echo "== go test $$m"; (cd $$m && go test ./...) || exit 1; done

test-frontend:
	cd frontend && npm test

test-android:
	cd android && ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT) ./gradlew -q testDebugUnitTest

# Rebuild + restart the backend (backend/deploy.sh), then build + install the
# Android app on the connected device (android/install.sh). The web frontend
# needs nothing: its Vite dev server picks up changes on reload.
deploy: deploy-backend install-android

deploy-backend:
	$(MAKE) -C backend deploy

install-android:
	cd android && ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT) ./install.sh
