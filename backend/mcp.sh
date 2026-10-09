#!/usr/bin/env bash
# Builds and runs cmd/lectable-mcp, the MCP server the repo's .mcp.json
# registers with Claude Code as "lectable" (stdio - nothing but the
# protocol may reach stdout, so the build reports to stderr). Rebuilding on
# every start costs a second once Go's cache is warm and means the server
# never lags the route catalog `make generate` writes.
#
# Env: LECTABLE_URL (http://localhost:8080), LECTABLE_DATA_DIR (./data),
# LECTABLE_LOG_FILE (/tmp/lectable-server.log) - the same defaults run.sh
# starts the backend with.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# shellcheck source=../env.sh
. ../env.sh
export LECTABLE_DATA_DIR="${LECTABLE_DATA_DIR:-$(pwd)/data}"
export LECTABLE_URL="${LECTABLE_URL:-http://localhost:${LECTABLE_PORT:-8080}}"

bin="${XDG_CACHE_HOME:-$HOME/.cache}/lectable/lectable-mcp"
mkdir -p "$(dirname "$bin")"
go build -o "$bin" ./cmd/lectable-mcp >&2
exec "$bin"
