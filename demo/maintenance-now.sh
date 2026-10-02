#!/usr/bin/env bash
# Start the "Database upgrade" maintenance window 30s from now (10 min long) and restart vigil.
set -euo pipefail
cd "$(dirname "$0")"
if docker compose version >/dev/null 2>&1; then DC="docker compose"; else DC="podman compose"; fi
start=$(python3 -c 'import datetime as d;print((d.datetime.now(d.timezone.utc)+d.timedelta(seconds=30)).strftime("%Y-%m-%dT%H:%M:%SZ"))')
sed -i.bak -E "s|^    start: \"[^\"]*\"(.*maint-start.*)$|    start: \"$start\"\1|" vigil.yaml && rm -f vigil.yaml.bak
grep maint-start vigil.yaml
$DC restart vigil >/dev/null
echo "maintenance starts at $start (UTC) for 10 minutes — Database component; alerts for postgres are silenced."
