#!/usr/bin/env bash
# Start the vigil demo stack (Docker or Podman).
set -euo pipefail
cd "$(dirname "$0")"
if docker compose version >/dev/null 2>&1; then DC="docker compose"; elif podman compose version >/dev/null 2>&1; then DC="podman compose"; else
  echo "need docker compose or podman compose"; exit 1; fi
# Podman on macOS: the container API socket lives inside the VM at /run/podman/podman.sock
if [ -z "${DOCKER_SOCK:-}" ] && command -v podman >/dev/null && podman machine inspect >/dev/null 2>&1; then export DOCKER_SOCK=/run/podman/podman.sock; fi
# A leftover Docker Desktop config ("credsStore": "desktop") breaks public
# image pulls when Docker Desktop isn't installed: use a clean config for the
# demo only (your ~/.docker/config.json is not touched). All images are public.
if grep -q '"credsStore": *"desktop"' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null && ! command -v docker-credential-desktop >/dev/null; then
  export DOCKER_CONFIG="$(mktemp -d)"; echo '{}' > "$DOCKER_CONFIG/config.json"
fi
mkdir -p export
$DC up -d --build
printf "waiting for vigil"
for _ in $(seq 1 60); do curl -fs http://localhost:8080/healthz >/dev/null 2>&1 && break; printf "."; sleep 1; done; echo
cat <<TXT

  vigil demo is up.

  Status page     http://localhost:8080
  Admin UI        http://localhost:8080/admin      token: demo-admin-token-123456
  Break things    http://localhost:9000            (alerts show up here too)
  Office intranet http://localhost:9001            (private network, seen only via the agent)
  Email inbox     http://localhost:8025            (Mailpit: alert + subscriber emails)

  Walkthrough:    demo/README.md
  Logs:           $DC logs -f vigil
  Stop:           ./down.sh
TXT
