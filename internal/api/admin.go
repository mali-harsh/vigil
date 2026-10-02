package api

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/store"
)

const sessionCookie = "vigil_session"

//go:embed admin.html
var adminFS embed.FS

var adminTmpl = template.Must(template.New("admin.html").Funcs(template.FuncMap{
	"pct": func(p *float64) string {
		if p == nil {
			return "—"
		}
		return strconv.FormatFloat(float64(int64(*p*100))/100, 'f', 2, 64) + "%"
	},
	"ago": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return fmt.Sprintf("%ds ago", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		}
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	},
	"ts":   func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") },
	"join": strings.Join,
}).ParseFS(adminFS, "admin.html"))

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", s.loginPage)
	mux.HandleFunc("POST /admin/login", s.login)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.Handle("GET /admin", s.ui("read", s.dashboard))
	mux.Handle("GET /admin/monitors/{id}", s.ui("read", s.monitorPage))
	mux.Handle("GET /admin/incidents", s.ui("read", s.incidentsPage))
	mux.Handle("POST /admin/incidents", s.ui("incidents:write", s.incidentCreateForm))
	mux.Handle("POST /admin/incidents/{id}", s.ui("incidents:write", s.incidentUpdateForm))
	mux.Handle("POST /admin/incidents/{id}/ack", s.ui("incidents:write", s.incidentAckForm))
	mux.Handle("POST /admin/notifiers/{name}/test", s.ui("admin", s.notifierTestForm))
}

// ui guards admin pages: no credentials → login, wrong role → 403.
func (s *Server) ui(scope string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() {
			http.Error(w, "admin UI disabled: configure server.api_keys, admin_tokens or server.auth", http.StatusNotFound)
			return
		}
		p := s.auth.Principal(r)
		if p == nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if !p.Can(scope) {
			http.Error(w, p.Name+" lacks "+scope+" (ask an admin for a higher role)", http.StatusForbidden)
			return
		}
		// Cookie / SSO auth + form posts: reject cross-site submissions (CSRF).
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	data["Title"] = s.Cfg.StatusPage.Title
	data["Me"] = principalFrom(r.Context())
	var buf bytes.Buffer
	if err := adminTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// ---- login ----

// failed logins are slowed globally: cheap brute-force protection for a
// single-tenant admin.
var loginMu sync.Mutex

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() {
		http.Error(w, "admin UI disabled: configure server.api_keys, admin_tokens or server.auth", http.StatusNotFound)
		return
	}
	if s.auth.Principal(r) != nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", map[string]any{"Error": r.URL.Query().Get("e") != "", "SSO": s.auth.SSO()})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok := r.PostFormValue("token")
	if s.auth.keyFor(tok, nil) == nil {
		loginMu.Lock()
		time.Sleep(time.Second)
		loginMu.Unlock()
		s.Log.Warn("admin login failed", "remote", r.RemoteAddr)
		http.Redirect(w, r, "/admin/login?e=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sessionValue(tok), Path: "/", MaxAge: 7 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ---- pages ----

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ms, err := s.withUptime(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	// problems first, then by name
	order := map[string]int{"down": 0, "degraded": 1, "unknown": 2, "up": 3, "paused": 4}
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := order[string(ms[i].State)], order[string(ms[j].State)]
		if a != b {
			return a < b
		}
		return ms[i].Name < ms[j].Name
	})
	counts := map[string]int{}
	for _, m := range ms {
		counts[string(m.State)]++
	}
	open, err := s.Store.OpenIncidents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var notifiers []string
	if s.Notifiers != nil {
		notifiers = s.Notifiers.Names()
	}
	s.render(w, r, "dashboard", map[string]any{"Monitors": ms, "Counts": counts, "Open": len(open), "Agents": s.Engine.Agents(),
		"Notifiers": notifiers, "Flash": r.URL.Query().Get("flash"), "Nav": "monitors"})
}

func (s *Server) monitorPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var mon *monitorOut
	ms, err := s.withUptime(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for i := range ms {
		if ms[i].ID == id {
			mon = &ms[i]
		}
	}
	if mon == nil {
		http.NotFound(w, r)
		return
	}
	res, err := s.Store.Results(r.Context(), id, time.Now().Add(-24*time.Hour), 5000)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.Store.RecentIncidents(r.Context(), 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	var incs []store.Incident
	for _, inc := range all {
		if inc.MonitorID == id {
			incs = append(incs, inc)
		}
	}
	recent := res
	if len(recent) > 50 {
		recent = recent[:50]
	}
	s.render(w, r, "monitor", map[string]any{"M": mon, "Chart": latencyChart(res), "Recent": recent, "Incidents": incs, "Nav": "monitors"})
}

type chart struct {
	Line  string
	Fails []float64
	MaxMS int64
	Empty bool
}

// latencyChart builds an SVG polyline (viewBox 0..1000 x 0..200) spanning
// the results given (at most the last 24h).
func latencyChart(res []check.Result) chart {
	if len(res) == 0 {
		return chart{Empty: true}
	}
	end := time.Now()
	start := res[len(res)-1].At // oldest; results are newest-first
	if end.Sub(start) < time.Minute {
		start = end.Add(-time.Minute)
	}
	var maxMS int64 = 1
	for _, r := range res {
		maxMS = max(maxMS, r.Latency.Milliseconds())
	}
	var pts []string
	var fails []float64
	for i := len(res) - 1; i >= 0; i-- { // results are newest-first
		r := res[i]
		x := float64(r.At.Sub(start)) / float64(end.Sub(start)) * 1000
		if r.Status == check.Down {
			fails = append(fails, x)
			continue
		}
		y := 195 - float64(r.Latency.Milliseconds())/float64(maxMS)*185
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return chart{Line: strings.Join(pts, " "), Fails: fails, MaxMS: maxMS}
}

func (s *Server) incidentsPage(w http.ResponseWriter, r *http.Request) {
	incs, err := s.Store.IncidentsSince(r.Context(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	names := s.componentNames()
	type comp struct{ ID, Name string }
	var comps []comp
	for id, n := range names {
		comps = append(comps, comp{id, n})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	for i := range incs {
		for j, c := range incs[i].Components {
			if n, ok := names[c]; ok {
				incs[i].Components[j] = n
			}
		}
	}
	s.render(w, r, "incidents", map[string]any{
		"Incidents": incs, "Components": comps, "Nav": "incidents",
		"Error": r.URL.Query().Get("error"), "Statuses": []string{store.Investigating, store.Identified, store.Monitoring, store.Resolved},
	})
}

func (s *Server) incidentCreateForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	_, err := s.doCreate(r.Context(), createReq{
		Title: r.PostFormValue("title"), Components: r.PostForm["components"],
		Impact: r.PostFormValue("impact"), Message: r.PostFormValue("message"),
	}, actor(r))
	redirectResult(w, r, err)
}

func (s *Server) incidentUpdateForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.doUpdate(r.Context(), id, updateReq{Status: r.PostFormValue("status"), Message: r.PostFormValue("message"), Title: r.PostFormValue("title")}, actor(r))
	redirectResult(w, r, err)
}

func redirectResult(w http.ResponseWriter, r *http.Request, err error) {
	target := "/admin/incidents"
	if err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrNotFound) {
			msg = "incident not found"
		}
		target += "?error=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
