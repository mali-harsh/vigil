"""vigil demo "toy": fake services you can break, plus an inbox for alerts.

  GET  /                         control panel + alert inbox (open in a browser)
  GET  /svc/<name>/health        200 "healthy" or the code you set (and optional delay)
  GET  /svc/<name>/json          {"status": "ok"|"degraded"} — for JSON assertions
  POST /svc/<name>/set           code=503&delay=0  (buttons on the panel)
  POST /hook/<channel>           receives alerts (Slack-style {"text"} or any JSON)
  GET  /ping/<name>              dead-man's switch target (vigil pings this)

Standard library only. Not for production — it's a test prop.
"""
import html
import json
import os
import re
import sys
import threading
import time
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

SERVICES = [s for s in os.environ.get("SERVICES", "api,payments").split(",") if s]
TITLE = os.environ.get("TITLE", "vigil demo — toy services")
lock = threading.Lock()
state = {s: {"code": 200, "delay": 0} for s in SERVICES}
inbox = []  # newest first
pings = {}

LINK = re.compile(r"(https?://[^\s\"'<>\\]+)")


def linkify(text):
    out, last = [], 0
    for m in LINK.finditer(text):
        out.append(html.escape(text[last:m.start()]))
        u = m.group(1)
        out.append(f'<a href="{html.escape(u)}" target="_blank">{html.escape(u)}</a>')
        last = m.end()
    out.append(html.escape(text[last:]))
    return "".join(out)


PAGE = """<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="4"><title>{title}</title>
<style>
body{{font:14px/1.5 system-ui,sans-serif;margin:0;background:#0b0d12;color:#e6e9ef}}
main{{max-width:1000px;margin:0 auto;padding:24px 16px}}h1{{font-size:20px;margin:0 0 4px}}h2{{font-size:13px;text-transform:uppercase;letter-spacing:.06em;color:#8b95a7;margin:24px 0 8px}}
p.sub{{color:#8b95a7;margin:0 0 16px}}.grid{{display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:12px}}
.card{{background:#141821;border:1px solid #232835;border-radius:10px;padding:14px}}.name{{font-weight:600;font-size:15px}}
.st{{font-size:12px;padding:2px 8px;border-radius:99px;margin-left:6px}}.ok{{background:#14532d;color:#86efac}}.bad{{background:#7f1d1d;color:#fca5a5}}.slow{{background:#78350f;color:#fcd34d}}
form{{display:inline}}button{{font:inherit;font-size:12px;border:1px solid #333a4a;background:#1d2230;color:#e6e9ef;border-radius:6px;padding:4px 9px;margin:6px 4px 0 0;cursor:pointer}}
button:hover{{background:#2a3142}}.msg{{border-left:3px solid #4f46e5;padding:8px 12px;margin:0 0 8px;background:#141821;border-radius:0 8px 8px 0;white-space:pre-wrap;word-break:break-word}}
.msg .meta{{color:#8b95a7;font-size:12px}}.ch-slack{{border-color:#a855f7}}.ch-oncall{{border-color:#ef4444}}.ch-teams{{border-color:#3b82f6}}a{{color:#93c5fd}}
code{{background:#1d2230;padding:1px 5px;border-radius:4px}}</style></head><body><main>
<h1>{title}</h1><p class="sub">Break or slow a service, then watch vigil react: status page <a href="http://localhost:8080" target="_blank">localhost:8080</a> ·
admin <a href="http://localhost:8080/admin" target="_blank">/admin</a> · email <a href="http://localhost:8025" target="_blank">Mailpit :8025</a>. Page auto-refreshes.</p>
<h2>Services</h2><div class="grid">{cards}</div>
<h2>Dead-man's switch (vigil → here)</h2><div class="card">{pings}</div>
<h2>Alerts received ({n})</h2>{msgs}
</main></body></html>"""


def card(name, s):
    if s["code"] != 200:
        badge = f'<span class="st bad">{s["code"]}</span>'
    elif s["delay"]:
        badge = f'<span class="st slow">slow {s["delay"]}ms</span>'
    else:
        badge = '<span class="st ok">healthy</span>'
    btn = lambda label, code, delay: (f'<form method="post" action="/svc/{name}/set?code={code}&delay={delay}">'
                                      f'<button>{label}</button></form>')
    return (f'<div class="card"><span class="name">{name}</span>{badge}<br>'
            + btn("✅ healthy", 200, 0) + btn("💥 503", 503, 0) + btn("🔥 500", 500, 0) + btn("🐢 slow 2.5s", 200, 2500)
            + "</div>")


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, body, ctype="text/plain; charset=utf-8"):
        b = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urlparse(self.path)
        parts = u.path.strip("/").split("/")
        if u.path == "/":
            with lock:
                cards = "".join(card(n, s) for n, s in state.items()) or "<p>(no services here)</p>"
                now = time.time()
                p = "".join(f"<div><code>{html.escape(k)}</code> last ping {int(now - t)}s ago</div>" for k, t in pings.items()) or "no pings yet"
                msgs = "".join(
                    f'<div class="msg ch-{html.escape(m["ch"])}"><div class="meta">{m["at"]} · #{html.escape(m["ch"])}</div>{linkify(m["text"])}</div>'
                    for m in inbox[:60]) or "<p>none yet — break a service above.</p>"
                return self.send(200, PAGE.format(title=html.escape(TITLE), cards=cards, pings=p, msgs=msgs, n=len(inbox)), "text/html; charset=utf-8")
        if len(parts) == 3 and parts[0] == "svc" and parts[1] in state:
            with lock:
                s = dict(state[parts[1]])
            if parts[2] == "health":
                time.sleep(s["delay"] / 1000)
                return self.send(s["code"], "healthy" if s["code"] == 200 else "broken")
            if parts[2] == "json":
                return self.send(200, json.dumps({"status": "ok" if s["code"] == 200 else "degraded", "service": parts[1]}), "application/json")
        if len(parts) == 2 and parts[0] == "ping":
            with lock:
                pings[parts[1]] = time.time()
            return self.send(200, "ok")
        if u.path == "/api/inbox":
            with lock:
                return self.send(200, json.dumps(inbox), "application/json")
        self.send(404, "not found")

    def do_POST(self):
        u = urlparse(self.path)
        parts = u.path.strip("/").split("/")
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        if len(parts) == 3 and parts[0] == "svc" and parts[2] == "set" and parts[1] in state:
            q = parse_qs(u.query)
            with lock:
                state[parts[1]] = {"code": int(q.get("code", ["200"])[0]), "delay": int(q.get("delay", ["0"])[0])}
            self.send_response(303)
            self.send_header("Location", "/")
            self.end_headers()
            return
        if len(parts) == 2 and parts[0] == "hook":
            try:
                d = json.loads(body or b"{}")
                text = d.get("text") or json.dumps(d, indent=2)
            except Exception:
                text = body.decode(errors="replace")
            with lock:
                inbox.insert(0, {"at": datetime.now().strftime("%H:%M:%S"), "ch": parts[1], "text": text})
                del inbox[200:]
            print(f"[{parts[1]}] {text.splitlines()[0] if text else ''}", flush=True)
            return self.send(200, "ok")
        if len(parts) == 2 and parts[0] == "ping":
            with lock:
                pings[parts[1]] = time.time()
            return self.send(200, "ok")
        self.send(404, "not found")


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9000
    print(f"toy on :{port} services={SERVICES}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), H).serve_forever()
