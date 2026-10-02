package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/statuspage"
	"github.com/mali-harsh/vigil/internal/store"
)

const secretTarget = "https://internal-secret.example.com/health"

type nopNotifier struct{}

func (nopNotifier) Notify(notify.Event, []string) {}

type nopBeater struct{}

func (nopBeater) Beat(string, time.Time) bool { return false }

func setup(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server:
  admin_tokens: ["admin-token-123"]
  api_tokens: ["read-token-123"]
status_page:
  title: Acme Status
  components:
    - name: Website
      monitors: [web]
    - name: Platform
      components:
        - {name: API, monitors: [api]}
        - {name: Workers, monitors: [worker]}
monitors:
  - {name: web, type: http, url: "https://example.com"}
  - {name: api, type: http, url: "` + secretTarget + `"}
  - {name: worker, type: tcp, host: "10.0.0.5:9000"}
  - {name: private-db, type: tcp, host: "10.0.0.9:5432"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"), cfg.StatusPage.Location)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	// api is down with an auto incident; private-db is down (not on the page)
	for id, s := range map[string]monitor.State{"web": monitor.Up, "api": monitor.Down, "worker": monitor.Up, "private-db": monitor.Down} {
		st.SaveState(ctx, id, s, now)
		status := check.Up
		if s == monitor.Down {
			status = check.Down
		}
		st.InsertResult(ctx, check.Result{MonitorID: id, At: now, Status: status, Message: "dial tcp 10.0.0.9: connection refused"}, false)
	}
	st.OpenIncident(ctx, store.NewIncident{MonitorID: "api", Title: "API outage", Components: []string{"api"}, At: now, Cause: "secret probe error", Message: "Investigating"})
	st.OpenIncident(ctx, store.NewIncident{MonitorID: "private-db", Title: "private-db outage", At: now, Cause: "secret db error", Message: "x"})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(ctx, cfg, st, nopNotifier{}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Cfg: cfg, Engine: eng, Store: st, Beater: nopBeater{}, Log: log}).Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
}

func get(t *testing.T, srv *httptest.Server, path, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestStatusPageLeaksNothingPrivate(t *testing.T) {
	srv, _ := setup(t, nil)
	for _, path := range []string{"/", "/api/status"} {
		resp, body := get(t, srv, path, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		for _, secret := range []string{secretTarget, "10.0.0", "secret probe error", "private-db", "connection refused"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q", path, secret)
			}
		}
	}
}

func TestStatusModel(t *testing.T) {
	srv, _ := setup(t, nil)
	_, body := get(t, srv, "/api/status", "")
	var p statuspage.Page
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != statuspage.Outage || len(p.Components) != 2 {
		t.Fatalf("status=%s comps=%d", p.Status, len(p.Components))
	}
	plat := p.Components[1]
	if plat.Status != statuspage.Outage || len(plat.Children) != 2 || plat.Children[0].Status != statuspage.Outage || plat.Children[1].Status != statuspage.Operational {
		t.Fatalf("group rollup wrong: %+v", plat)
	}
	if len(p.Active) != 1 || p.Active[0].Title != "API outage" || p.Active[0].Components[0] != "API" {
		t.Fatalf("active incidents: %+v", p.Active)
	}
	if len(plat.Bars) != statuspage.Days || plat.Bars[len(plat.Bars)-1].Level != "major" {
		t.Fatalf("today's bar should be major: %+v", plat.Bars[len(plat.Bars)-1])
	}
}

func TestBadge(t *testing.T) {
	srv, _ := setup(t, nil)
	resp, body := get(t, srv, "/badge/website.svg", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "operational") || resp.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp, _ := get(t, srv, "/badge/nope.svg", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown badge: %d", resp.StatusCode)
	}
}

func TestReadAPIAuth(t *testing.T) {
	srv, _ := setup(t, nil)
	for token, want := range map[string]int{"": 401, "wrong": 401, "read-token-123": 200, "admin-token-123": 200} {
		if resp, _ := get(t, srv, "/api/v1/monitors", token); resp.StatusCode != want {
			t.Errorf("token %q: got %d want %d", token, resp.StatusCode, want)
		}
	}
}

func post(t *testing.T, srv *httptest.Server, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestManualIncidentLifecycle(t *testing.T) {
	srv, _ := setup(t, nil)
	body := `{"title":"Slow uploads","components":["website"],"impact":"degraded","message":"Looking into it"}`
	if code, _ := post(t, srv, "/api/v1/incidents", "read-token-123", body); code != 401 {
		t.Fatalf("read token must not write: %d", code)
	}
	code, resp := post(t, srv, "/api/v1/incidents", "admin-token-123", body)
	if code != 201 {
		t.Fatalf("create: %d %s", code, resp)
	}
	var inc store.Incident
	json.Unmarshal([]byte(resp), &inc)

	// manual impact raises the component's public state
	_, page := get(t, srv, "/api/status", "")
	var p statuspage.Page
	json.Unmarshal([]byte(page), &p)
	if p.Components[0].Status != statuspage.Degraded {
		t.Fatalf("website should be degraded by manual incident, got %s", p.Components[0].Status)
	}

	path := "/api/v1/incidents/" + strconv.FormatInt(inc.ID, 10) + "/updates"
	if code, _ := post(t, srv, path, "admin-token-123", `{"status":"fixed","message":"x"}`); code != 400 {
		t.Fatalf("invalid status accepted: %d", code)
	}
	if code, r := post(t, srv, path, "admin-token-123", `{"status":"resolved","message":"All good"}`); code != 200 {
		t.Fatalf("resolve: %d %s", code, r)
	}
	if code, _ := post(t, srv, path, "admin-token-123", `{"status":"monitoring","message":"again"}`); code != 409 {
		t.Fatalf("update after resolve: %d", code)
	}
	for _, bad := range []string{
		`{"title":"","components":["website"]}`,
		`{"title":"x","components":["ghost"]}`,
		`{"title":"x","components":[]}`,
		`{"title":"x","components":["website"],"impact":"apocalyptic"}`,
		`{"title":"x","components":["website"],"extra":1}`,
	} {
		if code, _ := post(t, srv, "/api/v1/incidents", "admin-token-123", bad); code != 400 {
			t.Errorf("accepted %s: %d", bad, code)
		}
	}
}

func TestAdminUI(t *testing.T) {
	srv, _ := setup(t, nil)
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, _ := c.Get(srv.URL + "/admin")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/login" {
		t.Fatalf("unauthenticated admin: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(srv.URL+"/admin/login", url.Values{"token": {"wrong"}})
	if len(resp.Cookies()) != 0 {
		t.Fatal("cookie issued for wrong token")
	}
	resp, _ = c.PostForm(srv.URL+"/admin/login", url.Values{"token": {"admin-token-123"}})
	var session *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			session = ck
		}
	}
	if session == nil || strings.Contains(session.Value, "admin-token-123") || !session.HttpOnly {
		t.Fatalf("bad session cookie: %+v", session)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/admin", nil)
	req.AddCookie(session)
	resp, _ = c.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "private-db") {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}

	// cross-site form post is rejected even with a valid cookie
	req, _ = http.NewRequest("POST", srv.URL+"/admin/incidents", strings.NewReader("title=x&components=website"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(session)
	if resp, _ = c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("CSRF not blocked: %d", resp.StatusCode)
	}

	for _, p := range []string{"/admin/monitors/api", "/admin/incidents"} {
		req, _ = http.NewRequest("GET", srv.URL+p, nil)
		req.AddCookie(session)
		if resp, _ = c.Do(req); resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestAdminDisabledWithoutTokens(t *testing.T) {
	srv, _ := setup(t, func(c *config.Config) { c.Server.AdminTokens = nil })
	if resp, _ := get(t, srv, "/admin", ""); resp.StatusCode != 404 {
		t.Fatalf("admin should be disabled: %d", resp.StatusCode)
	}
	if code, _ := post(t, srv, "/api/v1/incidents", "", `{}`); code != 401 {
		t.Fatalf("writes should be refused: %d", code)
	}
}
