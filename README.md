# vigil

Self-hosted uptime monitoring and status page. One binary, one config file, alerts you can trust.

> Uptime Kuma's simplicity + Gatus's config-as-code + Instatus's status page + agents for private infrastructure.

**Status:** Phases 0–2b done (core engine · status page, incidents, maintenance, admin UI · agents, multi-location quorum, discovery, self-monitoring, metrics, Helm · Postgres + HA failover). Not production-ready yet.

## Quick start

```bash
# binary
go build -o vigil ./cmd/vigil
SLACK_WEBHOOK=https://hooks.slack.com/... ./vigil -config examples/vigil.yaml

# docker
docker run -d -p 8080:8080 \
  -v $PWD/vigil.yaml:/etc/vigil/vigil.yaml:ro -v vigil-data:/data \
  -e SLACK_WEBHOOK=... ghcr.io/mali-harsh/vigil
```

Validate config without starting: `vigil -config vigil.yaml -check` (all errors reported at once, unknown keys rejected).

## What it monitors (Phase 0)

| Type   | Checks |
|--------|--------|
| `http` | status codes, body contains, latency (→ degraded), TLS cert expiry, custom method/headers/body |
| `tcp`  | port accepts connections |
| `tls`  | handshake + certificate expiry |
| `dns`  | resolves, optionally to expected IPs |
| `push` | heartbeat: anything that can `curl` pings `/push/<token>`; silent past `interval + grace` = DOWN |

## Why the alerts are trustworthy

```
probe result ──► state machine ──► incident ──► notification
(one sample)     (confirmation)    (lifecycle)   (async, retried, deduped)
```

- **No single-blip alerts.** DOWN needs `fail_threshold` consecutive failures; UP needs `recover_threshold` successes. Flapping never pages.
- **One alert per state change**, plus a reminder every `reminder_every` while still down — not one per minute.
- **Isolation.** Every monitor runs on its own goroutine with a hard timeout; a hanging target can't delay others (tested).
- **Atomic state.** A single engine goroutine owns state → incident → notification; the API never shows DOWN without its incident.
- **Survives restarts.** State and open incidents are persisted; a restart mid-outage closes the *same* incident and sends one recovery.
- **No silent startup noise.** First sighting of a healthy target is silent; removed/paused monitors' incidents are closed.
- **Slow channels can't stall monitoring.** Notifications are queued, retried with backoff (0s/2s/10s/30s), and drained on shutdown.

## Agents: private networks & multiple regions

`vigil agent` is the same binary in agent mode. It **only dials out** over HTTPS — no inbound ports, no VPN — so it runs in a VPC, a Kubernetes cluster, Cloud Run/ECS, or an office Pi.

```yaml
agents:
  - {name: gcp-vpc,   token: "${AGENT_GCP_TOKEN}"}
  - {name: singapore, token: "${AGENT_SG_TOKEN}"}
monitors:
  - name: Internal API          # only reachable inside the VPC
    type: http
    url: http://10.0.4.12:8282/health
    locations: [gcp-vpc]
  - name: Public website        # down only if 2 of 3 vantage points agree
    type: http
    url: https://example.com
    locations: [local, gcp-vpc, singapore]
    quorum: 2                   # default: majority
```
```bash
VIGIL_AGENT_TOKEN=... vigil agent -server https://status.example.com
```

- Each location has its own state machine; the monitor's state is derived by **quorum**. One region failing is regional noise, not an outage (the incident names the confirming location).
- **An agent going offline is never a service outage.** You get one "agent offline" alert; its locations freeze. Monitors seen *only* by that agent show "No data" publicly instead of a stale "Operational".
- A dead agent can't blind you: quorum shrinks to the locations still online — but losing the agents that *saw* an outage is not treated as a recovery.
- Agents buffer results while the server is unreachable (newest kept), and may only report monitors assigned to them; clock-skewed timestamps are replaced.

## Who watches the watcher

- `server.heartbeat: {url, interval}` — vigil pings an external dead-man's switch (healthchecks.io, or *another* vigil's `/push/<token>`) only while its engine is healthy. If vigil dies, hangs or loses its DB, the pings stop and the other side alerts.
- `/healthz` fails if the database is unreachable **or** the engine loop has stalled — wire it to your orchestrator's liveness probe.
- Static export keeps the public page up when vigil itself is down.

## Discovery: monitor what you deploy

Opt workloads in where they're defined. Discovered monitors get the same defaults, validation and alerting as configured ones (admin/API/metrics; not on the public page).

```yaml
discovery:
  docker:     {enabled: true}                       # mount /var/run/docker.sock:ro
  kubernetes: {enabled: true, namespaces: [prod]}   # in-cluster SA, get/list services
```
```yaml
# Docker labels                         # Kubernetes Service annotations
vigil.enable: "true"                    vigil.dev/enable: "true"
vigil.url: http://api:8080/health       vigil.dev/path: /health   # → http://<svc>.<ns>.svc:<port>/health
vigil.interval: 30s                     vigil.dev/interval: 30s
```
Keys: `enable, name, type, url, host, path (k8s), interval, timeout, expect-status, body-contains, max-latency, notify, locations`. A **stopped** container stays monitored (so it alerts); a deleted one is removed and its incident closed.

## High availability (Postgres)

Run two or more servers against one Postgres. Exactly one — the **leader** — probes, alerts and writes; the others stand by and **proxy every request to the leader**, so a load balancer can simply include all nodes.

```yaml
server:
  database: {driver: postgres, url: "${DATABASE_URL}"}   # postgres://user:pass@host/db?sslmode=require
  ha: {lease_ttl: 15s}
```

- **Election** is a lease row in Postgres (works through PgBouncer / RDS Proxy / Cloud SQL proxy, unlike advisory locks). All expiry checks use the database clock, so node clock skew doesn't matter; an epoch fences stale leaders.
- **No split brain:** the leader steps down if it can't renew for ⅔·TTL; a standby may take over only after the full TTL has expired — the old leader is always stopped ≥ TTL/3 before the new one starts, even when partitioned.
- **Failover:** crash → ~TTL + TTL/3 (≈20 s at 15 s); graceful restart/rollout → ~TTL/3, because the lease is released on shutdown. The new leader rebuilds state from the database — same incident, no duplicate alert (tested with `kill -9` mid-outage).
- **Database down:** the leader steps down rather than act without state; when Postgres returns, exactly one node leads again. Keep the static export + external heartbeat for that case.
- `/healthz` is 200 on every healthy node; `/readyz` is 200 only on the leader; `vigil_leader{node}` in `/metrics`.
- Standbys reach the leader at its `ha.advertise_url` (default: `VIGIL_ADVERTISE_URL`, else `http://<bound or primary IP>:<port>`). The Helm chart sets it to the pod IP.
- During the failover gap requests get `502/503 + Retry-After` — make cron heartbeats retry: `curl -fsS --retry 3 --retry-delay 5 https://status.example.com/push/<token>`. Agents buffer and retry on their own.
- Terminate TLS at your load balancer for HA (built-in Let's Encrypt is per node).

```bash
helm install vigil deploy/helm/vigil --set ha.enabled=true --set existingSecret=vigil-secrets -f values.yaml
```

## Metrics

`GET /metrics` (Prometheus text, behind `api_tokens` if set): `vigil_monitor_up`, `vigil_monitor_state`, `vigil_check_latency_seconds{location}`, `vigil_check_results_total`, `vigil_agent_up`, `vigil_notifications_total{result=sent|failed|dropped}`, `vigil_heartbeat_pings_total`, `vigil_incidents_open`, `vigil_build_info`.

## Deploy

| Target | Files |
|---|---|
| Docker / Compose | `deploy/compose/docker-compose.yml` (read-only, caps dropped, Docker discovery example) |
| Kubernetes | `deploy/helm/vigil` — `mode: server` (StatefulSet + PVC, optional Ingress, discovery RBAC) or `mode: agent` |
| VM / bare metal | `deploy/systemd/vigil.service`, `vigil-agent.service` (hardened units) |

```bash
helm install vigil deploy/helm/vigil -f my-values.yaml                 # server
helm install vigil-agent deploy/helm/vigil --set mode=agent \
  --set agent.server=https://status.example.com --set existingSecret=vigil-agent   # agent
```
Images: tag `vX.Y.Z` → GitHub Actions publishes `ghcr.io/mali-harsh/vigil:X.Y.Z` (amd64 + arm64).

## Runs anywhere, monitors anything

vigil is one static binary (no CGO) with a SQLite file — it runs wherever a process can run.

| Where you run vigil | How |
|---|---|
| Any VM / bare metal | binary + systemd unit |
| Docker / Compose | single container, `/data` volume |
| Kubernetes | Helm chart (`deploy/helm/vigil`) |
| Serverless-ish | Cloud Run / ECS / Fly with a volume or Postgres |

| What you monitor | How (planned providers marked) |
|---|---|
| Anything with a URL/port | `http` / `tcp` / `tls` / `dns` in YAML |
| Cron jobs, batch, backups, serverless | `push` heartbeat |
| Docker containers | label discovery (`vigil.enable=true`) |
| Kubernetes workloads | Service annotation discovery; agent or server in-cluster |
| Private networks (VPC, on-prem) | `vigil agent` dials *out* — no inbound firewall, no VPN |
| Multi-region | agents in several regions; DOWN only on quorum |

## Status page

- **Components & groups** — only monitors placed in a component are public; targets, IPs and raw errors never appear (tested).
- **90-day bars** from daily rollups that outlive raw-result retention; a day's uptime is its weakest monitor's.
- **Incidents** — automatic on outage (titled by component, e.g. "API outage"), plus manual ones with an impact; public updates Investigating → Identified → Monitoring → Resolved.
- **Maintenance windows** — one-off or daily/weekly, wall-clock stable across DST; alerts silenced, excluded from uptime, shown on the page. Still broken when the window ends → alerted then.
- **Static export** (`status_page.export_dir`) — `index.html` + `status.json` written atomically every minute. Host the folder anywhere; if vigil dies the page stays up and shows a "not updated since…" warning.
- **Badges** — `/badge/<component>.svg`, `/badge/overall.svg`.
- **Automatic HTTPS** — `server.tls.domains` + `email`.

## Admin UI

Set `server.admin_tokens`, open `/admin`, sign in with the token. Monitors (problems first), per-monitor latency chart and results, incidents with update/declare forms. The session cookie is an HMAC of the token (never the token itself), HttpOnly + SameSite=Strict, and cross-origin form posts are rejected.

## API

| Endpoint | |
|---|---|
| `GET /` | status page (public) |
| `GET /api/status` | status page as JSON (public, CORS-open) |
| `GET /badge/{component}.svg` | status badge (public) |
| `GET /healthz` | liveness (DB reachable) |
| `GET\|POST\|HEAD /push/{token}` | heartbeat |
| `GET /api/v1/monitors` | read · state, last result, 24h/7d uptime |
| `GET /api/v1/monitors/{id}/results?since=1h&limit=500` | read · raw results |
| `GET /api/v1/incidents[/{id}]` | read · incidents (with updates) |
| `POST /api/v1/incidents` | admin · `{title, components, impact, message}` |
| `POST /api/v1/incidents/{id}/updates` | admin · `{status, message, title?}` |

Read endpoints need `Authorization: Bearer <api or admin token>` when `server.api_tokens` is set; writes always need an admin token.

## Roadmap

- **Phase 0 — core** ✅ scheduler, checks, state machine, incidents, Slack/webhook, SQLite, API, YAML
- **Phase 1 — status page** ✅ components & groups, 90-day bars, incident updates, maintenance windows, auto-HTTPS, static export, badges, admin UI
- **Phase 2 — everywhere** ✅ agents (private + multi-region quorum), Docker & Kubernetes discovery, dead-man's heartbeat + engine liveness, Prometheus `/metrics`, Helm chart, compose, systemd, CI/release
- **Phase 2b — HA** ✅ Postgres backend (whole test suite runs on both), lease-based leader election with fencing, proxying standbys, Helm HA mode (PDB, anti-affinity)
- **Phase 3 — teams** subscribers, escalation/on-call, RBAC & scoped keys, more checks (Postgres/Redis/Kafka/gRPC/ICMP), Terraform provider

## Layout

```
cmd/vigil          entrypoint, wiring, graceful shutdown
internal/config    YAML + ${ENV} expansion + validation
internal/check     single probes (no alerting logic)
internal/monitor   pure state machine (exhaustively unit-tested)
internal/scheduler per-monitor loops, heartbeat watchers
internal/engine    single writer: results → state → incidents → notifications
internal/maintenance  pure window math (one-off/daily/weekly, DST-safe)
internal/statuspage   public model, HTML/JSON/badge rendering, static export
internal/store     SQLite / Postgres persistence, versioned migrations, daily rollups, leader lease
internal/ha        leader election (lease renew / step-down / handover)
internal/notify    Slack / webhook senders, async retrying dispatcher
internal/api       public + read/write API, agent API, metrics, push endpoint, admin UI
internal/agent     agent client: poll assignments, run probes, batch + retry results
internal/discovery Docker labels / Kubernetes annotations → monitors
internal/metrics   dependency-free Prometheus text exposition
```

## Development

```bash
go test -race ./...                                            # SQLite
VIGIL_TEST_POSTGRES='postgres://user:pw@localhost:5432/postgres?sslmode=disable' \
  go test -race ./...                                          # everything again on Postgres + HA tests
```

MIT licensed.
