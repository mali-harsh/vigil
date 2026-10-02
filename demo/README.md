# vigil demo: try every feature locally

One command starts vigil with every feature switched on, plus fake services you can break and inboxes where alerts and email land.

```bash
cd demo
./up.sh            # first run builds the image (~2-3 min), then ~10 s
./down.sh          # stop   (./down.sh --wipe also deletes history/incidents)
```

Needs Docker or Podman with compose and about 1.5 GB free RAM. Timings are shortened for the demo: checks every 10 s, DOWN after 2 failures (~20 s), escalation after 45 s.

| Open | What |
|---|---|
| http://localhost:8080 | **Status page** |
| http://localhost:8080/admin | **Admin UI** (token `demo-admin-token-123456`) |
| http://localhost:9000 | **Toy panel**: break services; Slack / on-call / Teams alerts land here |
| http://localhost:9001 | **Office intranet**: a private network only the agent can reach |
| http://localhost:8025 | **Mailpit**: every email (alerts, subscriber mail) |

What's running: `vigil` (server, SQLite) · `agent` (on the private `office` network) · `toy` and `intranet` (fake services) · `cron` (heartbeat job) · `redis` · `postgres` · `mailpit` · `whoami` (auto-discovered from labels). `$DC` below means `docker compose` or `podman compose`, run inside `demo/`.

---

## 1. Status page tour
Open **:8080**.
- **Components and groups:** Payments is a group; its status is the worst child's.
- **90-day bars** fill in as data arrives (hover a bar for that day's uptime).
- **Private monitors are hidden:** `github-cert`, `ping-1.1.1.1`, `api-2-locations` and `docker:whoami` exist (see /admin) but aren't on the page, and targets and errors never are.
- **Badge:** http://localhost:8080/badge/website.svg · **JSON:** http://localhost:8080/api/status

## 2. An outage and its alert
On **:9000**, click **💥 503** on `api`.
- After ~20 s: a 🔴 message in the toy's **#slack** inbox; status page shows **Website: Outage** with an "Investigating" incident; /admin lists the incident.
- Click **✅ healthy**: after ~20 s you get a 🟢 *RECOVERED* message and the incident moves to *Past incidents*.

## 3. A blip doesn't page anyone
Click **💥 503** on `api`, then **✅ healthy** within ~8 s. No alert: one failed check isn't confirmation (`fail_threshold: 2`). In /admin you may see "confirming 1".

## 4. Degraded (slow) vs down
Click **🐢 slow 2.5s** on `api` (its limit is `max_latency: 1500ms`). You get 🟡 *DEGRADED*, not down; the page shows *Degraded performance*. Click healthy to recover.

## 5. Escalation and one-click acknowledge
1. Break `api` (💥 503) and **wait about 70 s without doing anything**.
2. First 🔴 **#slack**; 45 s later the escalation fires: **#oncall** webhook, plus an email to `oncall@demo.test` in **Mailpit** titled `[escalation step 2]`.
3. In a 🔴 message, click the **Acknowledge: http://localhost:8080/ack/…** link. A confirm page opens. Opening it did **not** acknowledge (link scanners open links). Type your name and click **Acknowledge**.
4. All channels already paged get 👀 *acknowledged by …*. No more steps or reminders.
5. Click healthy: 🟢 recovery goes to **slack + oncall + email**, everyone the outage reached.

Without step 3, reminders repeat every 2 min. You can also acknowledge from /admin → Incidents → **Acknowledge**.

## 6. JSON assertions and a second channel
Break **payments** (💥 503).
- `payments` (HTTP status) goes down and alerts **#slack and #teams** (that monitor's `notify` list).
- `payments-json` keeps returning HTTP 200, but its body becomes `{"status":"degraded"}`. The JSON assertion `status == ok` fails, so it's DOWN too. A 200 can still be broken.

## 7. Two locations, quorum
`api-2-locations` checks `api` from **local** *and* the **office** agent and needs **2 of 2** to agree. In /admin → that monitor, see both locations and their latency. The incident's cause says which location confirmed it (`[office] …`).

## 8. Agents and private networks
`intranet` lives only on the `office` network: vigil can't reach it, the agent can (outbound connection only).
- On **:9001**, break `intranet`. It goes DOWN via the agent (/admin shows "via office").
- Kill the agent: `$DC stop agent`. After ~90 s you get ⚠️ *agent office is OFFLINE*. **No** "intranet down" alert, and publicly *Office intranet* shows **No data** (not a false "Operational"). Agent loss is never reported as an outage.
- `$DC start agent` brings ✅ *back online*.

## 9. Heartbeats (cron jobs)
The `cron` container pings `/push/<token>` every 20 s.
- `$DC stop cron`: after ~45 s (30 s interval + 15 s grace), **Nightly jobs** goes DOWN with "no heartbeat for …".
- `$DC start cron`: recovers on the next ping.

## 10. Protocol checks: real reasons, not just "port open"
Stop a dependency and read the error in /admin:
```bash
$DC stop redis      # Cache    → DOWN "dial tcp…" (then $DC start redis)
$DC stop postgres   # Database → DOWN (connect error; the password is never shown)
$DC stop mailpit    # Email    → DOWN (SMTP banner)
```
Also running: TLS cert expiry (`github-cert`), DNS (`cloudflare-dns`), ICMP (`ping-1.1.1.1`).

## 11. Docker discovery
`whoami` isn't in `vigil.yaml`. It opted in with labels (`vigil.enable=true`, `vigil.url=…`) and appears in /admin as `docker:whoami`.
- `$DC stop whoami`: stays monitored and goes **DOWN** (a stopped container must alert, not vanish).
- `$DC rm -sf whoami`: deleted, so the monitor is removed and its incident closed.
- `$DC up -d whoami`: rediscovered within 15 s.

## 12. Maintenance windows
```bash
./maintenance-now.sh     # "Database upgrade" starts in 30 s, lasts 10 min
```
The status page shows **Maintenance in progress** and Database turns blue. Now `$DC stop postgres`: **no alert**, and those minutes don't count against uptime. If postgres is still down when the window ends, you get the alert then.

## 13. Declaring incidents and posting updates
/admin → **Incidents** → *Declare an incident* (pick components and impact). Post updates: Investigating → Identified → Monitoring → Resolved. The status page shows the timeline. The admin view records **who** posted each update.

## 14. Status-page subscribers
1. Status page → bottom → **Get updates**: enter any email.
2. **Mailpit**: "Confirm your subscription" → click → **Confirm** button. (Just opening the link doesn't subscribe.)
3. Break `api` or declare an incident. The subscriber gets an email per update, with an **Unsubscribe** link (and one-click unsubscribe headers).

Webhook subscribers (signed payloads):
```bash
curl -s -H 'Authorization: Bearer demo-admin-token-123456' -d '{"kind":"webhook","address":"http://toy:9000/hook/subscriber"}' localhost:8080/api/v1/subscribers
```
The next incident update appears in the toy inbox as **#subscriber**.

## 15. Access control
- **Keys:** sign in to /admin with `demo-read-token-1234567` (read only: no forms or buttons) vs `demo-admin-token-123456` (everything). `demo-bot-token-12345678` can only manage incidents.
- **SSO roles** (simulated by the demo's `X-Demo-User` header):
```bash
for u in viewer@demo.test dev@demo.test admin@demo.test stranger@evil.test; do
  printf "%-20s read=%s declare=%s test-notifier=%s\n" $u \
    $(curl -s -o /dev/null -w '%{http_code}' -H "X-Demo-User: $u" localhost:8080/api/v1/monitors) \
    $(curl -s -o /dev/null -w '%{http_code}' -H "X-Demo-User: $u" -d '{"title":"t","components":["website"],"impact":"none","message":"m"}' localhost:8080/api/v1/incidents) \
    $(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Demo-User: $u" localhost:8080/api/v1/notifiers/slack/test)
done
```
Expected: viewer `200 403 403` · dev (domain wildcard → responder) `200 201 403` · admin `200 201 200` · stranger `401 401 401`.

## 16. API, metrics, test notifications
```bash
K='Authorization: Bearer demo-read-token-1234567'
curl -s -H "$K" localhost:8080/api/v1/monitors | head -c 600; echo
curl -s -H "$K" localhost:8080/metrics | grep -E '^vigil_(monitor_up|agent_up|leader)'
```
/admin → **Notifiers** → **Send test**: a real test message, and you see whether the channel actually accepted it.

## 17. "Who watches the watcher"
- **Dead-man's switch:** vigil pings the toy every 15 s ("Dead-man's switch" on :9000). `$DC stop vigil` and the "last ping" age keeps growing. In real life that external service alerts you.
- **Static export:** `demo/export/index.html` is rewritten every minute. Stop vigil, wait 5 min, open the file: a "has not updated since…" banner appears.

## 18. Restart safety
Break `api`, wait for 🔴, then `$DC restart vigil`. Same incident, no duplicate 🔴; fix `api` and you get exactly one 🟢.

---

## High availability (optional)
Two vigil nodes on Postgres behind nginx:
```bash
./down.sh                 # free memory first (small VMs)
./ha.sh up                # LB http://localhost:8090 · nodes :8091 / :8092 · toy http://localhost:9100
./ha.sh status            # which node leads (/readyz)
./ha.sh kill-leader       # kill -9 the leader → the other takes over in ~5 s (lease 6 s)
./ha.sh start-all
./ha.sh restart-leader    # graceful: lease released → handover in ~1 s
./ha.sh down
```
Through the LB, a crash costs ~3 s of `502 Retry-After`, a graceful restart ~0.2 s. The standby proxies to the leader, so nginx can include both nodes. `nginx-ha.conf.template` shows the `resolve` setting you need in front of containers.

## Troubleshooting
- **`docker-credential-desktop` not found:** a leftover Docker Desktop setting; the scripts work around it automatically without touching `~/.docker/config.json`.
- **Discovery "permission denied" on Podman:** SELinux. The compose file sets `label=disable` for vigil (demo only).
- **ICMP "not permitted":** the compose file sets `net.ipv4.ping_group_range`; in production, set it on the host or grant `NET_RAW`.
- **Build killed (out of memory):** stop other stacks or give the VM ≥ 3 GB (`podman machine set --memory 3072`).
- **Logs:** `$DC logs -f vigil agent`
