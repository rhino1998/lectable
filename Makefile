# Repo-wide lint entry points. `make lint` runs every component's linter;
# each lint-* target also works on its own.
#
# golangci-lint (v2) is expected on PATH or in $(go env GOPATH)/bin:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
# (from backend/, so the module's newer Go toolchain builds it). Config is the
# shared root .golangci.yml.

GO_MODULES := backend audiocpp-go llamacpp-go
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)
ANDROID_SDK_ROOT ?= /home/rhino/android-sdk-toolchains

.PHONY: lint lint-go lint-frontend lint-android deploy deploy-backend install-android

lint: lint-go lint-frontend lint-android

lint-go:
	@for m in $(GO_MODULES); do echo "== golangci-lint $$m"; (cd $$m && $(GOLANGCI_LINT) run ./...) || exit 1; done

lint-frontend:
	cd frontend && npm run lint

lint-android:
	cd android && ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT) ./gradlew -q lintDebug

# Rebuild + restart the backend (backend/deploy.sh), then build + install the
# Android app on the connected device (android/install.sh). The web frontend
# needs nothing: its Vite dev server picks up changes on reload.
deploy: deploy-backend install-android

deploy-backend:
	$(MAKE) -C backend deploy

install-android:
	cd android && ANDROID_SDK_ROOT=$(ANDROID_SDK_ROOT) ./install.sh
