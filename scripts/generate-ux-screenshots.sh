#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UXSHOT_GOCACHE="${GOCACHE:-${TMPDIR:-/tmp}/agentmux-uxshot-go-cache}"

cd "$ROOT/daemon"
GOCACHE="$UXSHOT_GOCACHE" go run ./cmd/uxshot -output-dir "$ROOT/docs/design/img" "$@"
