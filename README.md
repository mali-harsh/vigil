# vigil

Self-hosted uptime monitoring and status page. One binary, one config file, alerts you can trust.

> Uptime Kuma's simplicity + Gatus's config-as-code + Instatus's status page + agents for private infrastructure.

**Status:** Phases 0–1 done (core engine, status page, incidents, maintenance, admin UI). Not production-ready yet.

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

## Runs anywhere, monitors anything

vigil is one static binary (no CGO) with a SQLite file — it runs wherever a process can run.

| Where you run vigil | How |
|---|---|
| Any VM / bare metal | binary + systemd unit |
| Docker / Compose | single container, `/data` volume |
| Kubernetes | Deployment + PVC (Helm chart — Phase 2) |
| Serverless-ish | Cloud Run / ECS / Fly with a volume or Postgres |

| What you monitor | How (planned providers marked) |
|---|---|
| Anything with a URL/port | `http` / `tcp` / `tls` / `dns` in YAML |
| Cron jobs, batch, backups, serverless | `push` heartbeat |
| Docker containers | **Phase 2:** auto-discovery from labels (`vigil.enable=true`, `vigil.url=...`), Traefik-style |
| Kubernetes workloads | **Phase 2:** discovery from Service/Ingress annotations; agent runs in-cluster |
| Private networks (VPC, on-prem) | **Phase 2:** `vigil-agent` dials *out* to the server — no inbound firewall, no VPN |
| Multi-region | **Phase 2:** agents in several regions; DOWN only on quorum |

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
- **Phase 2 — everywhere** `vigil-agent` (private + multi-region), Docker & Kubernetes discovery, Postgres + HA leader election, self-heartbeat ("who watches the watcher"), Prometheus `/metrics`, Helm chart
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
internal/store     SQLite persistence, versioned migrations, daily rollups
internal/notify    Slack / webhook senders, async retrying dispatcher
internal/api       public + read/write API, push endpoint, admin UI
```

## Development

```bash
go test -race ./...
```

MIT licensed.
