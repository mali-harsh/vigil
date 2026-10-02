#!/usr/bin/env bash
# Stop the demo. ./down.sh --wipe also deletes vigil's data (history, incidents).
set -euo pipefail
cd "$(dirname "$0")"
if docker compose version >/dev/null 2>&1; then DC="docker compose"; else DC="podman compose"; fi
if [ "${1:-}" = "--wipe" ]; then $DC down -v; rm -rf export/*; else $DC down; fi
