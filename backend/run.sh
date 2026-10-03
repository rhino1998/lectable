#!/usr/bin/env bash
# Builds (optionally) and launches cmd/server, which spawns cmd/ttsworker
# itself - see backend/CLAUDE.md's "Run" section for the underlying
# commands/env vars this wraps. Override any exported var below by setting
# it in the environment before calling this script.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# Native library locations - audio.cpp (linked into ttsworker only) and
# llama.cpp (linked into server, for speaker attribution). Both binaries'
# builds need both on CGO_LDFLAGS; both need audio.cpp's on LD_LIBRARY_PATH
# at runtime for ttsworker's dynamic linking, llama.cpp's for server's own.
AUDIOCPP_DIR="${AUDIOCPP_DIR:-$HOME/audio.cpp/build-rocm7/bin}"
LLAMACPP_DIR="${LLAMACPP_DIR:-$HOME/llama.cpp-latest/build-live/bin}"
# ROCm 7.2.4, unpacked user-space (see the root Makefile): ahead of the
# system's 6.4.4 on the library path, and reached via librocdxg under WSL.
ROCM_DIR="${ROCM_DIR:-$HOME/rocm7-user/root/opt/rocm-7.2.4}"
export CGO_LDFLAGS="-L${AUDIOCPP_DIR} -L${LLAMACPP_DIR}"
# internal/store's search index needs SQLite's FTS5 compiled in.
export GOFLAGS="-tags=sqlite_fts5 ${GOFLAGS:-}"
export LD_LIBRARY_PATH="${LLAMACPP_DIR}:${AUDIOCPP_DIR}:${ROCM_DIR}/lib:${ROCM_DIR}/llvm/lib:${LD_LIBRARY_PATH:-}"
export HSA_ENABLE_DXG_DETECTION=1
# cmd/server puts this first on ttsworker's own LD_LIBRARY_PATH; left at its
# default it could load a different libaudiocpp than the one linked here.
export LECTABLE_AUDIOCPP_LIB_DIR="${LECTABLE_AUDIOCPP_LIB_DIR:-$AUDIOCPP_DIR}"

export LECTABLE_DATA_DIR="${LECTABLE_DATA_DIR:-$(pwd)/data}"
export LECTABLE_SPEAKER_LLM_GPU_LAYERS="${LECTABLE_SPEAKER_LLM_GPU_LAYERS:--1}"
# Primed system prompts as restored KV snapshots (~28% faster LLM passes,
# +1.4GB ttsworker RSS) - see llmworker.Config.PrimeAsState.
export LECTABLE_SPEAKER_LLM_PRIME_STATE="${LECTABLE_SPEAKER_LLM_PRIME_STATE:-true}"

GO_TOOLCHAIN_DIR="${GO_TOOLCHAIN_DIR:-$HOME/go-toolchains/go1.27.1/bin}"
if [ -d "$GO_TOOLCHAIN_DIR" ]; then
  export PATH="$GO_TOOLCHAIN_DIR:$PATH"
  export GOTOOLCHAIN=local
fi

BUILD=0
FOREGROUND=0
for arg in "$@"; do
  case "$arg" in
    --build) BUILD=1 ;;
    --fg | --foreground) FOREGROUND=1 ;;
    *)
      echo "usage: $0 [--build] [--fg]" >&2
      exit 1
      ;;
  esac
done

if [ "$BUILD" = "1" ]; then
  echo "building ttsworker and server..."
  go build -o ttsworker ./cmd/ttsworker
  go build -o server ./cmd/server
fi

if [ "$FOREGROUND" = "1" ]; then
  exec ./server
fi

LECTABLE_LOG_FILE="${LECTABLE_LOG_FILE:-/tmp/lectable-server.log}"
# One log per run, keeping the previous run's as $LECTABLE_LOG_FILE.1 - appending
# forever grew it past a gigabyte.
if [ -f "$LECTABLE_LOG_FILE" ]; then
  mv -f "$LECTABLE_LOG_FILE" "$LECTABLE_LOG_FILE.1"
fi
nohup ./server >"$LECTABLE_LOG_FILE" 2>&1 &
disown
echo "server started (PID $!), spawns ./ttsworker itself, logging to $LECTABLE_LOG_FILE"
