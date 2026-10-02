# vigil

Self-hosted uptime monitoring and status page. One binary, one config file, alerts you can trust.

> Uptime Kuma's simplicity + Gatus's config-as-code + Instatus's status page + agents for private infrastructure.

**Status:** Phases 0–3 done (core engine · status page, incidents, maintenance, admin UI · agents, quorum, discovery, self-monitoring, metrics, Helm · Postgres HA · protocol checks, on-call channels, escalation + ack, scoped keys + SSO, subscribers). Not production-ready yet.

## Try everything locally

```bash
cd demo && ./up.sh      # vigil + agent + fake services + mail inbox, all features on
```
Then follow [`demo/README.md`](demo/README.md): 18 scenarios (outages, escalation + ack links, agents, quorum, discovery, maintenance, subscribers, roles, HA failover).

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

## What it monitors

| Type | Proves | Config |
|---|---|---|
| `http` | status, body text, **JSON assertions**, latency (→ degraded), cert expiry | `url`, `expect: {status, body_contains, json: [{path: data.ok, equals: "true"}], max_latency, cert_min_days}` |
| `tcp` | port accepts connections | `host: db:5432` |
| `tls` | handshake + certificate expiry | `host: api:443`, `expect.cert_min_days` |
| `dns` | resolves (optionally to given IPs) | `host`, `expect.resolves_to` |
| `icmp` | ping reply (unprivileged ICMP; else needs `CAP_NET_RAW`) | `host: 10.0.0.1` |
| `postgres` | connects **and** runs `SELECT 1` (password never shown) | `url: postgres://user:pass@db:5432/app` |
| `redis` | `AUTH` + `PING` → `PONG` (catches "LOADING", wrong password) | `host`, `password`, `username`, `tls` |
| `kafka` | broker answers an ApiVersions request (not just an open port) | `host: broker:9092`, `tls` |
| `grpc` | `grpc.health.v1` → `SERVING` (h2c or TLS) | `host: svc:50051`, `service`, `tls` |
| `smtp` | `220` banner + `EHLO` | `host: mail:25`, `tls` (implicit, :465) |
| `push` | heartbeat: anything that can `curl` pings `/push/<token>` | `token`, `interval`, `grace` |

Protocol checks speak just enough of each protocol to prove the service is *answering* — tested against real Postgres 17, Redis 7 and Kafka 3.9.

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

## Alert channels

| `type` | Notes |
|---|---|
| `slack` | incoming webhook or Workflow-Builder webhook (variable `text`) |
| `discord`, `teams` | channel webhook / Teams *Workflows* webhook (adaptive card) |
| `telegram` | `bot_token`, `chat_id` |
| `email` | `to: [...]`, via `server.smtp` (STARTTLS / TLS, header-injection safe) |
| `pagerduty` | Events API v2 `routing_key` — **trigger → acknowledge → resolve** on one dedup key |
| `opsgenie` | `api_key`, `region` — create → acknowledge → close by alias |
| `webhook` | the event as JSON |

`POST /api/v1/notifiers/<name>/test` (or **Send test** in the admin UI) sends a real message and reports the channel's actual answer.

## Escalation & acknowledgement

```yaml
server:
  public_url: https://status.example.com
  ack_secret: "${VIGIL_ACK_SECRET}"     # signs one-click ack links in alerts
escalations:
  - name: prod
    steps:
      - notify: [slack]                 # immediately
      - {after: 10m, notify: [pagerduty]}
      - {after: 30m, notify: [cto-email]}
monitors:
  - {name: API, type: http, url: https://api.example.com/health, escalation: prod}
```

- Every alert carries an **Acknowledge** link. Opening it shows a confirm button — it never acks on GET, because Slack unfurlers and mail scanners open links.
- Acking (link, admin UI, or `POST /api/v1/incidents/{id}/ack`) stops further steps and reminders; everyone already paged is told who took it; PagerDuty/Opsgenie are acknowledged.
- Recovery is sent to **every** channel the outage reached. Steps fire within 5 s of becoming due.

## Access control

```yaml
server:
  api_keys:
    - {name: grafana,    token: "${GRAFANA_TOKEN}", scopes: [read]}
    - {name: deploy-bot, token: "${BOT_TOKEN}",     scopes: [incidents:write]}
  auth:                                   # SSO via oauth2-proxy / Cloudflare Access / Google IAP
    header: X-Forwarded-Email
    trusted_proxies: [10.0.0.0/8]         # the header is ignored from anywhere else
    users:
      - {email: "*@example.com", role: responder}
      - {email: lead@example.com, role: admin}
```

Scopes: `read`, `incidents:write`, `subscribers:write`, `admin`. Roles: **viewer** (read), **responder** (+ ack, declare/update incidents), **admin** (everything). The admin UI hides what a role can't do; incident updates and acks record **who** did them. Legacy `api_tokens` / `admin_tokens` keep working. In HA, a standby vouches for SSO identities with a signature bound to the request, so forged or replayed headers are rejected.

## Status-page subscribers

`status_page.subscribe: true` (needs `server.smtp` + `public_url`) adds an email sign-up form: double opt-in (confirm on POST, not GET), one-click unsubscribe (incl. RFC 8058 `List-Unsubscribe-Post` for Gmail/Yahoo), honeypot + per-IP and global rate limits, and the same response whether or not an address is already subscribed. Subscribers get every public incident update.

Webhook subscribers are added by operators: `POST /api/v1/subscribers {"kind":"webhook","address":"https://..."}` returns a secret; deliveries carry `X-Vigil-Signature: sha256=<HMAC of body>`.

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
- **Phase 3 — teams** ✅ protocol checks (Postgres/Redis/Kafka/gRPC/ICMP/SMTP, JSON assertions), PagerDuty/Opsgenie/email/Discord/Teams/Telegram, escalation + signed ack links, scoped API keys + SSO roles + audit actor, status-page subscribers
- **Next** Terraform provider (needs a monitor-write API), config hot-reload, maintenance announcements to subscribers, design pass

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
internal/acklink   signed one-click acknowledge links
internal/subscribers  status-page subscriber delivery (email, signed webhooks)
```

## Development

```bash
go test -race ./...                                            # SQLite
VIGIL_TEST_POSTGRES='postgres://user:pw@localhost:5432/postgres?sslmode=disable' \
  go test -race ./...                                          # everything again on Postgres + HA tests
```

MIT licensed.
