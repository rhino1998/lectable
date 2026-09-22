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
AUDIOCPP_DIR="${AUDIOCPP_DIR:-$HOME/audio.cpp/build/bin}"
LLAMACPP_DIR="${LLAMACPP_DIR:-$HOME/llama-cpp-py-sync/vendor/llama.cpp/build/bin}"
export CGO_LDFLAGS="-L${AUDIOCPP_DIR} -L${LLAMACPP_DIR}"
export LD_LIBRARY_PATH="${LLAMACPP_DIR}:${AUDIOCPP_DIR}:${LD_LIBRARY_PATH:-}"

export DATA_DIR="${DATA_DIR:-$(pwd)/data}"
export SPEAKER_LLM_GPU_LAYERS="${SPEAKER_LLM_GPU_LAYERS:--1}"

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

LOG_FILE="${LOG_FILE:-/tmp/lectable-server.log}"
nohup ./server >>"$LOG_FILE" 2>&1 &
disown
echo "server started (PID $!), spawns ./ttsworker itself, logging to $LOG_FILE"
