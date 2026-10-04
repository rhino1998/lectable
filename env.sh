# shellcheck shell=bash
# The one place this repo's native toolchain is located: the audio.cpp and
# llama.cpp builds ttsworker links against, the ROCm they were built with,
# and the Go toolchain. Sourced by both Makefiles (as BASH_ENV, so every
# recipe gets it) and by backend/run.sh and backend/deploy.sh; source it
# yourself for a bare `go build`/`go test`:
#
#   . ./env.sh
#
# Override a location by setting it first, in the environment or on make's
# command line (`make build AUDIOCPP_DIR=/some/other/path`).

AUDIOCPP_DIR="${AUDIOCPP_DIR:-$HOME/audio.cpp/build-rocm7/bin}"
LLAMACPP_DIR="${LLAMACPP_DIR:-$HOME/llama.cpp-latest/build-live/bin}"
# ROCm 7.2.4, unpacked user-space (no system install; /opt/rocm is still
# 6.4.4 and on ld.so.conf, so this must come first on the library path).
ROCM_DIR="${ROCM_DIR:-$HOME/rocm7-user/root/opt/rocm-7.2.4}"
# The Go this module's go.mod needs, used only if it is installed there.
GO_TOOLCHAIN_DIR="${GO_TOOLCHAIN_DIR:-$HOME/go-toolchains/go1.27.1/bin}"
export AUDIOCPP_DIR LLAMACPP_DIR ROCM_DIR GO_TOOLCHAIN_DIR

export CGO_LDFLAGS="-L${AUDIOCPP_DIR} -L${LLAMACPP_DIR}"
# backend/internal/store's search index needs SQLite's FTS5 compiled in.
case " ${GOFLAGS:-} " in
  *" -tags=sqlite_fts5 "*) ;;
  *) export GOFLAGS="-tags=sqlite_fts5${GOFLAGS:+ $GOFLAGS}" ;;
esac
# Also what the linker resolves libaudiocpp's own ROCm dependencies through.
export LD_LIBRARY_PATH="${LLAMACPP_DIR}:${AUDIOCPP_DIR}:${ROCM_DIR}/lib:${ROCM_DIR}/llvm/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
# Under WSL, ROCm reaches the GPU through librocdxg, which needs this.
export HSA_ENABLE_DXG_DETECTION=1

if [ -d "$GO_TOOLCHAIN_DIR" ]; then
  export PATH="$GO_TOOLCHAIN_DIR:$PATH"
  export GOTOOLCHAIN=local
fi
