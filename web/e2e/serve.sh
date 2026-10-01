#!/bin/sh
# Starts a throwaway webpty for the browser tests: the current web build is
# embedded, the database lives in a temp directory, and nothing is shared
# with a developer's own instance.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
# WEBPTY_E2E_DATA lets a spec reach the database to damage a recording on purpose.
data=${WEBPTY_E2E_DATA:-$(mktemp -d "${TMPDIR:-/tmp}/webpty-e2e.XXXXXX")}
rm -rf "$data"
mkdir -p "$data"

cd "$root"
go run ./internal/webassets/syncdist web/dist internal/webassets/dist
go build -o "$data/webpty" ./cmd/webpty

WEBPTY_ADDRESS="127.0.0.1:${WEBPTY_E2E_PORT:-8766}" \
WEBPTY_DATABASE_PATH="$data/webpty.db" \
WEBPTY_COMMAND=/bin/sh \
WEBPTY_RECORDING_FLUSH_INTERVAL=100ms \
WEBPTY_RECORDING_QUEUE_BYTES=67108864 \
"$data/webpty" &
pid=$!
trap 'kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; rm -rf "$data"' EXIT INT TERM
wait "$pid"
