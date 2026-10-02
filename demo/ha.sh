#!/usr/bin/env bash
# HA demo helper:  ./ha.sh up | status | kill-leader | restart-leader | down
set -euo pipefail
cd "$(dirname "$0")"
if docker compose version >/dev/null 2>&1; then DC="docker compose"; else DC="podman compose"; fi
if grep -q '"credsStore": *"desktop"' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null && ! command -v docker-credential-desktop >/dev/null; then
  export DOCKER_CONFIG="$(mktemp -d)"; echo '{}' > "$DOCKER_CONFIG/config.json"
fi
DC="$DC -f compose.ha.yml"
ready() { curl -s -o /dev/null -w '%{http_code}' "http://localhost:$1/readyz" 2>/dev/null || true; }
leader() { [ "$(ready 8091)" = 200 ] && echo vigil-a && return; [ "$(ready 8092)" = 200 ] && echo vigil-b && return; echo none; }
case "${1:-status}" in
  up)
    if ! { docker image inspect vigil:demo || podman image inspect vigil:demo; } >/dev/null 2>&1 || [ "${REBUILD:-}" = 1 ]; then
      echo "building vigil:demo (one build)…"; (docker build -t vigil:demo .. 2>/dev/null || podman build -t vigil:demo ..) | tail -1
    fi
    $DC up -d
    printf "waiting for a leader"; until [ "$(leader)" != none ]; do printf "."; sleep 1; done; echo
    echo "  load balancer: http://localhost:8090   node A: :8091   node B: :8092   toy: http://localhost:9100"
    "$0" status ;;
  status)
    echo "  vigil-a /readyz=$(ready 8091)  vigil-b /readyz=$(ready 8092)  → leader: $(leader)"
    echo "  via LB: $(curl -s -o /dev/null -w '%{http_code}' http://localhost:8090/) (served by $(curl -s -D - -o /dev/null http://localhost:8090/ | awk -F': ' 'tolower($1)=="x-served-by"{print $2}' | tr -d '\r'))" ;;
  kill-leader)
    l=$(leader); [ "$l" = none ] && { echo "no leader"; exit 1; }
    echo "  kill -9 $l (simulated crash)…"; $DC kill -s KILL "$l" >/dev/null
    t0=$(date +%s); printf "  waiting for failover"; until [ "$(leader)" != none ]; do printf "."; sleep 0.5; done
    echo " new leader: $(leader) after $(( $(date +%s) - t0 ))s"; "$0" status ;;
  restart-leader)
    l=$(leader); [ "$l" = none ] && { echo "no leader"; exit 1; }
    echo "  graceful stop of ${l} (lease released on shutdown)…"; t0=$(date +%s); $DC stop "$l" >/dev/null
    printf "  waiting for handover"; until [ "$(leader)" != none ]; do printf "."; sleep 0.2; done
    echo " new leader: $(leader) after $(( $(date +%s) - t0 ))s"; $DC start "$l" >/dev/null; "$0" status ;;
  start-all) $DC up -d >/dev/null; sleep 3; "$0" status ;;
  logs) $DC logs -f vigil-a vigil-b ;;
  down) $DC down -v ;;
  *) echo "usage: $0 up | status | kill-leader | restart-leader | start-all | logs | down"; exit 2 ;;
esac
