#!/usr/bin/env bash
# Builds (optionally) and launches cmd/server, which spawns cmd/ttsworker
# itself - see backend/CLAUDE.md's "Run" section for the underlying
# commands/env vars this wraps. Override any exported var below by setting
# it in the environment before calling this script.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# Native library locations (audio.cpp and llama.cpp, both linked into
# ttsworker only), ROCm and the Go toolchain.
# shellcheck source=../env.sh
. ../env.sh
# cmd/server puts this first on ttsworker's own LD_LIBRARY_PATH; left at its
# default it could load a different libaudiocpp than the one linked here.
export LECTABLE_AUDIOCPP_LIB_DIR="${LECTABLE_AUDIOCPP_LIB_DIR:-$AUDIOCPP_DIR}"

export LECTABLE_DATA_DIR="${LECTABLE_DATA_DIR:-$(pwd)/data}"
export LECTABLE_SPEAKER_LLM_GPU_LAYERS="${LECTABLE_SPEAKER_LLM_GPU_LAYERS:--1}"
# Primed system prompts as restored KV snapshots (~28% faster LLM passes,
# +1.4GB ttsworker RSS) - see llmworker.Config.PrimeAsState.
export LECTABLE_SPEAKER_LLM_PRIME_STATE="${LECTABLE_SPEAKER_LLM_PRIME_STATE:-true}"

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
