package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mali-harsh/vigil/internal/acklink"
	"github.com/mali-harsh/vigil/internal/config"
)

func authCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server:
  public_url: https://status.example.com
  ack_secret: "0123456789abcdef0123456789abcdef"
  database: {driver: postgres, url: "postgres://u:p@db/vigil"}
  api_keys:
    - {name: grafana, token: "grafana-token-0123456789", scopes: [read]}
    - {name: deploy-bot, token: "deploybot-token-0123456789", scopes: [incidents:write]}
  auth:
    header: X-Forwarded-Email
    trusted_proxies: [127.0.0.1/32]
    users:
      - {email: harsh@acme.io, role: admin}
      - {email: "*@acme.io", role: responder}
      - {email: intern@acme.io, role: viewer}
status_page: {components: [{name: Website, monitors: [web]}]}
monitors: [{name: web, type: http, url: "https://example.com"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func req(method, path, remote string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestPrincipals(t *testing.T) {
	a := NewAuthn(authCfg(t))
	cases := []struct {
		name   string
		r      *http.Request
		want   string // principal name ("" = anonymous)
		scope  string
		allows bool
	}{
		{"read key reads", req("GET", "/", "203.0.113.9:1", map[string]string{"Authorization": "Bearer grafana-token-0123456789"}), "grafana", "read", true},
		{"read key can't write", req("GET", "/", "203.0.113.9:1", map[string]string{"Authorization": "Bearer grafana-token-0123456789"}), "grafana", "incidents:write", false},
		{"bot writes incidents", req("GET", "/", "203.0.113.9:1", map[string]string{"Authorization": "Bearer deploybot-token-0123456789"}), "deploy-bot", "incidents:write", true},
		{"bot isn't admin", req("GET", "/", "203.0.113.9:1", map[string]string{"Authorization": "Bearer deploybot-token-0123456789"}), "deploy-bot", "admin", false},
		{"sso admin", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "Harsh@ACME.io"}), "harsh@acme.io", "admin", true},
		{"sso domain wildcard → responder", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "dev@acme.io"}), "dev@acme.io", "incidents:write", true},
		{"highest matching role wins", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "intern@acme.io"}), "intern@acme.io", "incidents:write", true},
		{"IAP prefix stripped", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "accounts.google.com:harsh@acme.io"}), "harsh@acme.io", "admin", true},
		{"unknown domain: nobody", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "eve@evil.com"}), "", "read", false},
		{"header from untrusted IP ignored", req("GET", "/", "203.0.113.9:1", map[string]string{"X-Forwarded-Email": "harsh@acme.io"}), "", "read", false},
		{"lookalike domain", req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "eve@notacme.io"}), "", "read", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := a.Principal(c.r)
			got := ""
			if p != nil {
				got = p.Name
			}
			if got != c.want || p.Can(c.scope) != c.allows {
				t.Fatalf("principal=%q can(%s)=%v; want %q %v", got, c.scope, p.Can(c.scope), c.want, c.allows)
			}
		})
	}
}

func TestForwardedIdentity(t *testing.T) {
	a := NewAuthn(authCfg(t))
	// standby received the request from the trusted SSO proxy and signs it
	in := req("GET", "/admin", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "harsh@acme.io"})
	a.SignForward(in)
	// the leader sees it coming from the standby's IP (not a trusted proxy)
	at := req("GET", "/admin", "10.0.0.2:9", map[string]string{fwdIdentity: in.Header.Get(fwdIdentity), fwdSig: in.Header.Get(fwdSig)})
	if p := a.Principal(at); p == nil || p.Name != "harsh@acme.io" {
		t.Fatalf("signed forward rejected: %+v", p)
	}
	// a client forging the forward headers gets nothing
	forged := req("GET", "/admin", "203.0.113.9:1", map[string]string{fwdIdentity: "harsh@acme.io", fwdSig: "1700000000.deadbeef"})
	if p := a.Principal(forged); p != nil {
		t.Fatalf("forged forward accepted: %+v", p)
	}
	// a client sending forward headers THROUGH a standby has them stripped
	sneaky := req("GET", "/admin", "203.0.113.9:1", map[string]string{fwdIdentity: "harsh@acme.io", fwdSig: in.Header.Get(fwdSig)})
	a.SignForward(sneaky)
	if sneaky.Header.Get(fwdIdentity) != "" {
		t.Fatal("standby forwarded a client-supplied identity")
	}
	// a valid signature replayed against another endpoint is rejected
	replay := req("POST", "/admin/incidents", "10.0.0.2:9", map[string]string{fwdIdentity: in.Header.Get(fwdIdentity), fwdSig: in.Header.Get(fwdSig)})
	if p := a.Principal(replay); p != nil {
		t.Fatal("signature replayed on another method/path accepted")
	}
	// another deployment (different database) can't mint identities for this one
	other := authCfg(t)
	other.Server.Database.URL = "postgres://u:p@otherdb/vigil"
	b := NewAuthn(other)
	in2 := req("GET", "/", "127.0.0.1:5", map[string]string{"X-Forwarded-Email": "harsh@acme.io"})
	b.SignForward(in2)
	at2 := req("GET", "/", "10.0.0.2:9", map[string]string{fwdIdentity: in2.Header.Get(fwdIdentity), fwdSig: in2.Header.Get(fwdSig)})
	if p := a.Principal(at2); p != nil {
		t.Fatal("identity signed by another deployment accepted")
	}
}

func TestAckLinkFlow(t *testing.T) {
	srv, st := setup(t, func(c *config.Config) {
		c.Server.PublicURL, c.Server.AckSecret = "https://status.example.com", "0123456789abcdef0123456789abcdef"
	})
	inc, _ := st.ActiveIncident(context.Background(), "api")
	token := strings.TrimPrefix(acklink.URL("https://status.example.com", "0123456789abcdef0123456789abcdef", inc.ID), "https://status.example.com/ack/")

	// GET (what Slack unfurl / mail scanners do) shows a form and acknowledges NOTHING
	resp, body := get(t, srv, "/ack/"+token, "")
	if resp.StatusCode != 200 || !strings.Contains(body, "Acknowledge") {
		t.Fatalf("ack page: %d", resp.StatusCode)
	}
	if got, _ := st.Incident(context.Background(), inc.ID); got.AckedAt != nil {
		t.Fatal("GET acknowledged the incident")
	}
	if resp, _ := get(t, srv, "/ack/"+token+"x", ""); resp.StatusCode != 404 {
		t.Fatalf("tampered token: %d", resp.StatusCode)
	}

	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ = c.PostForm(srv.URL+"/ack/"+token, url.Values{"name": {"Harsh"}})
	if resp.StatusCode != 303 {
		t.Fatalf("ack post: %d", resp.StatusCode)
	}
	got, _ := st.Incident(context.Background(), inc.ID)
	if got.AckedAt == nil || got.AckedBy != "Harsh (alert link)" {
		t.Fatalf("not acknowledged: %+v", got)
	}
}

func TestActorRecordedAndAckAPI(t *testing.T) {
	srv, st := setup(t, nil)
	code, body := post(t, srv, "/api/v1/incidents", "admin-token-123", `{"title":"Slow","components":["website"],"impact":"degraded","message":"looking"}`)
	if code != 201 {
		t.Fatal(code, body)
	}
	incs, _ := st.RecentIncidents(context.Background(), 1)
	full, _ := st.Incident(context.Background(), incs[0].ID)
	if full.Updates[0].Actor != "admin-token-1" {
		t.Fatalf("actor = %q", full.Updates[0].Actor)
	}
	auto, _ := st.ActiveIncident(context.Background(), "api")
	if code, _ := post(t, srv, "/api/v1/incidents/"+strconv.FormatInt(auto.ID, 10)+"/ack", "read-token-123", ""); code != 403 {
		t.Fatalf("read key acked: %d", code)
	}
	if code, b := post(t, srv, "/api/v1/incidents/"+strconv.FormatInt(auto.ID, 10)+"/ack", "admin-token-123", ""); code != 200 || !strings.Contains(b, `"acked_by":"admin-token-1"`) {
		t.Fatalf("ack: %d %s", code, b)
	}
	if code, _ := post(t, srv, "/api/v1/incidents/999/ack", "admin-token-123", ""); code != 404 {
		t.Fatalf("missing incident: %d", code)
	}
}

// Through a real ReverseProxy: the leader trusts NO proxy IP, so the only way
// in is the standby's signature — which must survive path + query rewriting.
func TestSSOThroughRealStandbyProxy(t *testing.T) {
	leaderCfg := authCfg(t)
	leaderCfg.Server.Auth.TrustedProxies = []string{"192.0.2.255/32"} // nothing local
	la := NewAuthn(leaderCfg)
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := la.Principal(r)
		if p == nil {
			http.Error(w, "anonymous", 401)
			return
		}
		io.WriteString(w, p.Name+" "+r.URL.RequestURI())
	}))
	defer leader.Close()

	sb := NewSwitch("standby", "t", func(context.Context) (string, error) { return leader.URL, nil }, NewAuthn(authCfg(t)))
	standby := httptest.NewServer(sb)
	defer standby.Close()

	r, _ := http.NewRequest("GET", standby.URL+"/admin/incidents?filter=open&x=1", nil)
	r.Header.Set("X-Forwarded-Email", "harsh@acme.io") // from the SSO proxy (127.0.0.1 is trusted by the standby)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "harsh@acme.io /admin/incidents?filter=open&x=1" {
		t.Fatalf("forwarded identity lost: %d %q", resp.StatusCode, b)
	}
	// directly at the leader the same header is worthless
	r2, _ := http.NewRequest("GET", leader.URL+"/admin", nil)
	r2.Header.Set("X-Forwarded-Email", "harsh@acme.io")
	if resp, _ := http.DefaultClient.Do(r2); resp.StatusCode != 401 {
		t.Fatalf("leader trusted a raw header from an untrusted IP: %d", resp.StatusCode)
	}
}
