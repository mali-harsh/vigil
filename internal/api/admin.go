package api

import (
	"bytes"
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
	mux.Handle("GET /admin", s.adminUI(s.dashboard))
	mux.Handle("GET /admin/monitors/{id}", s.adminUI(s.monitorPage))
	mux.Handle("GET /admin/incidents", s.adminUI(s.incidentsPage))
	mux.Handle("POST /admin/incidents", s.adminUI(s.incidentCreateForm))
	mux.Handle("POST /admin/incidents/{id}", s.adminUI(s.incidentUpdateForm))
}

func (s *Server) adminUI(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.Cfg.Server.AdminTokens) == 0 {
			http.Error(w, "admin UI disabled: set server.admin_tokens", http.StatusNotFound)
			return
		}
		if !s.isAdmin(r) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		// Cookie auth + form posts: reject cross-site submissions (CSRF).
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		h(w, r)
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

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	data["Title"] = s.Cfg.StatusPage.Title
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
	if len(s.Cfg.Server.AdminTokens) == 0 {
		http.Error(w, "admin UI disabled: set server.admin_tokens", http.StatusNotFound)
		return
	}
	s.render(w, "login", map[string]any{"Error": r.URL.Query().Get("e") != ""})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if len(s.Cfg.Server.AdminTokens) == 0 || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok := r.PostFormValue("token")
	if !anyMatch(tok, s.Cfg.Server.AdminTokens) {
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
	s.render(w, "dashboard", map[string]any{"Monitors": ms, "Counts": counts, "Open": len(open), "Agents": s.Engine.Agents(), "Nav": "monitors"})
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
	s.render(w, "monitor", map[string]any{"M": mon, "Chart": latencyChart(res), "Recent": recent, "Incidents": incs, "Nav": "monitors"})
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
	s.render(w, "incidents", map[string]any{
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
	})
	redirectResult(w, r, err)
}

func (s *Server) incidentUpdateForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	err = s.doUpdate(r.Context(), id, updateReq{Status: r.PostFormValue("status"), Message: r.PostFormValue("message"), Title: r.PostFormValue("title")})
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
