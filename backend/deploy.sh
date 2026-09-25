#!/usr/bin/env bash
# Rebuilds both binaries and restarts the running backend: build first (a
# failed build leaves the running server untouched), keep the previous
# binaries as *.prev for rollback, stop the server running from this
# directory, relaunch it via run.sh, wait until it answers, and re-pause the
# job queue if it was paused (the queue lives in memory, so queued work is
# lost across a restart either way).
#
#   ./deploy.sh            # build + restart
#   ./deploy.sh --rollback # restart on the *.prev binaries instead
#
# Env: PORT (8080), LOG_FILE (/tmp/lectable-server.log), plus everything
# run.sh reads (AUDIOCPP_DIR, LLAMACPP_DIR, DATA_DIR, ...).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
HERE="$(pwd)"
PORT="${PORT:-8080}"
BASE="http://localhost:${PORT}"
LOG_FILE="${LOG_FILE:-/tmp/lectable-server.log}"
export LOG_FILE

ROLLBACK=0
case "${1:-}" in
  "") ;;
  --rollback) ROLLBACK=1 ;;
  *)
    echo "usage: $0 [--rollback]" >&2
    exit 1
    ;;
esac

# The server running from this directory (not some other checkout's).
running_pid() {
  local pid
  for pid in $(pgrep -x server || true); do
    if [ "$(readlink "/proc/$pid/cwd" 2>/dev/null)" = "$HERE" ]; then
      echo "$pid"
      return
    fi
  done
}

was_paused=false
if body=$(curl -fsS "$BASE/api/jobs" 2>/dev/null); then
  was_paused=$(python3 -c 'import json,sys; print(str(json.load(sys.stdin).get("paused", False)).lower())' <<<"$body")
fi

if [ "$ROLLBACK" = "1" ]; then
  for bin in server ttsworker; do
    [ -f "$bin.prev" ] || { echo "no $bin.prev to roll back to" >&2; exit 1; }
  done
  cp server server.failed 2>/dev/null || true
  cp server.prev server
  cp ttsworker.prev ttsworker
  echo "rolled back to the previous binaries"
else
  # Same env/toolchain as run.sh's --build, without launching anything yet.
  AUDIOCPP_DIR="${AUDIOCPP_DIR:-$HOME/audio.cpp/build/bin}"
  LLAMACPP_DIR="${LLAMACPP_DIR:-$HOME/llama-cpp-py-sync/vendor/llama.cpp/build/bin}"
  export CGO_LDFLAGS="-L${AUDIOCPP_DIR} -L${LLAMACPP_DIR}"
  GO_TOOLCHAIN_DIR="${GO_TOOLCHAIN_DIR:-$HOME/go-toolchains/go1.27.1/bin}"
  if [ -d "$GO_TOOLCHAIN_DIR" ]; then
    export PATH="$GO_TOOLCHAIN_DIR:$PATH"
    export GOTOOLCHAIN=local
  fi
  echo "building ttsworker and server..."
  go build -o ttsworker.new ./cmd/ttsworker
  go build -o server.new ./cmd/server
  for bin in server ttsworker; do
    [ -f "$bin" ] && cp "$bin" "$bin.prev"
    mv "$bin.new" "$bin"
  done
fi

pid=$(running_pid)
if [ -n "$pid" ]; then
  echo "stopping server (PID $pid)..."
  kill "$pid"
  for _ in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "server didn't exit in 30s; killing it"
    kill -9 "$pid"
  fi
fi

setsid ./run.sh </dev/null
echo "waiting for $BASE ..."
for _ in $(seq 1 90); do
  if curl -fsS -o /dev/null "$BASE/api/instance" 2>/dev/null; then
    if [ "$was_paused" = "true" ]; then
      curl -fsS -o /dev/null -X POST "$BASE/api/jobs/pause"
      echo "re-paused the job queue"
    fi
    echo "backend up (PID $(running_pid))"
    exit 0
  fi
  sleep 1
done
echo "backend didn't come up in 90s - last log lines:" >&2
tail -20 "$LOG_FILE" >&2
echo "roll back with: $0 --rollback" >&2
exit 1
