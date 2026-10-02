// Package api serves the public status page, the read/write API, the heartbeat
// push endpoint and the admin UI.
//
// Access levels:
//   - public:  status page, /api/status, badges, /push/{token}, /healthz
//   - read:    /api/v1 GETs — server.api_tokens (open if none configured)
//   - admin:   /admin UI and /api/v1 writes — server.admin_tokens (disabled if none)
package api

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/metrics"
	"github.com/mali-harsh/vigil/internal/statuspage"
	"github.com/mali-harsh/vigil/internal/store"
	"github.com/mali-harsh/vigil/internal/subscribers"
)

type Beater interface {
	Beat(token string, at time.Time) bool
}

type Server struct {
	Cfg       *config.Config
	Engine    *engine.Engine
	Store     *store.Store
	Beater    Beater
	Results   chan<- check.Result    // agent results enter the engine here
	Notifiers Notifiers              // test notifications (optional)
	Publisher *subscribers.Publisher // status-page subscribers (optional)
	Metrics   *metrics.Registry
	Version   string
	Node      string // this server's ID (HA)
	Log       *slog.Logger

	page *statuspage.Builder
	auth *Authn

	subPerIP, subGlobal *limiter           // sign-up abuse limits
	tmpl                *template.Template // admin templates bound to this deployment's timezone
}

func (s *Server) Handler() http.Handler {
	s.page = &statuspage.Builder{Cfg: s.Cfg, Engine: s.Engine, Store: s.Store}
	s.auth = NewAuthn(s.Cfg)
	loc := s.loc() // admin shows times in the deployment's timezone, like the status page
	s.tmpl = template.Must(adminTmpl.Clone()).Funcs(template.FuncMap{
		"ts": func(t time.Time) string { return t.In(loc).Format("Jan 2, 15:04:05") },
	})
	s.subPerIP = newLimiter(5, time.Hour)
	s.subGlobal = newLimiter(300, time.Hour) // caps confirmation mail even from many IPs
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.statusPage)
	mux.HandleFunc("GET /api/status", s.statusJSON)
	mux.HandleFunc("GET /badge/{id}", s.badge)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.healthz) // the active node is ready when healthy
	mux.HandleFunc("/push/{token}", s.push)  // GET/POST/HEAD: whatever the cron job can do

	mux.Handle("GET /metrics", s.need("read", s.metricsHandler))
	mux.Handle("GET /api/v1/agents", s.need("read", s.agents))
	mux.Handle("GET /api/v1/monitors", s.need("read", s.monitors))
	mux.Handle("GET /api/v1/monitors/{id}/results", s.need("read", s.results))
	mux.Handle("GET /api/v1/incidents", s.need("read", s.incidents))
	mux.Handle("GET /api/v1/incidents/{id}", s.need("read", s.incident))
	mux.Handle("POST /api/v1/incidents", s.need("incidents:write", s.createIncident))
	mux.Handle("POST /api/v1/incidents/{id}/updates", s.need("incidents:write", s.addUpdate))
	mux.Handle("POST /api/v1/incidents/{id}/ack", s.need("incidents:write", s.ackAPI))
	mux.Handle("POST /api/v1/notifiers/{name}/test", s.need("admin", s.testNotifier))
	mux.HandleFunc("GET /ack/{token}", s.ackPage)
	mux.HandleFunc("POST /subscribe", s.subscribe)
	mux.HandleFunc("GET /subscribe/confirm/{token}", s.confirmPage)
	mux.HandleFunc("POST /subscribe/confirm/{token}", s.confirm)
	mux.HandleFunc("GET /unsubscribe/{token}", s.unsubscribePage)
	mux.HandleFunc("POST /unsubscribe/{token}", s.unsubscribe)
	mux.Handle("GET /api/v1/subscribers", s.need("subscribers:write", s.listSubscribers))
	mux.Handle("POST /api/v1/subscribers", s.need("subscribers:write", s.addSubscriber))
	mux.Handle("DELETE /api/v1/subscribers/{id}", s.need("subscribers:write", s.deleteSubscriber))
	mux.HandleFunc("POST /ack/{token}", s.ackLink)

	s.agentRoutes(mux)
	s.adminRoutes(mux)
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

// ---- public ----

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
		return
	}
	if err := s.Engine.Healthy(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	if !s.Beater.Beat(r.PathValue("token"), time.Now()) {
		http.Error(w, "unknown token", http.StatusNotFound)
		return
	}
	w.Write([]byte("ok\n"))
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.page.Build(r.Context(), time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	html, err := statuspage.Render(p, true, statuspage.RenderOptions{Subscribe: s.subscribeEnabled()})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.Write(html)
}

func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	p, err := s.page.Build(r.Context(), time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*") // public data, embeddable
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".svg")
	p, err := s.page.Build(r.Context(), time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	label, status := "status", p.Status
	if id != "overall" {
		row := p.Find(id)
		if row == nil {
			http.NotFound(w, r)
			return
		}
		label, status = row.Name, row.Status
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(statuspage.Badge(label, status))
}

// ---- read API ----

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, orEmpty(s.Engine.Agents()))
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	var g []metrics.Sample
	add := func(name, help string, l metrics.L, v float64) {
		g = append(g, metrics.Sample{Name: name, Help: help, Labels: l, Value: v})
	}
	add("vigil_build_info", "vigil build.", metrics.L{"version": s.Version}, 1)
	add("vigil_leader", "1 on the active (leader) node.", metrics.L{"node": s.Node}, 1)
	b := func(c bool) float64 {
		if c {
			return 1
		}
		return 0
	}
	for _, v := range s.Engine.Views() {
		l := metrics.L{"monitor": v.ID, "type": v.Type, "source": v.Source}
		add("vigil_monitor_up", "1 if the monitor is up or degraded (service answering), else 0.", l, b(v.State == "up" || v.State == "degraded"))
		for _, st := range []string{"up", "degraded", "down", "unknown", "paused"} {
			add("vigil_monitor_state", "Current state, one series per state (1 = current).", metrics.L{"monitor": v.ID, "state": st}, b(string(v.State) == st))
		}
		add("vigil_monitor_maintenance", "1 while in a maintenance window.", metrics.L{"monitor": v.ID}, b(v.Maintenance))
		add("vigil_monitor_state_since_seconds", "Unix time of the last state change.", metrics.L{"monitor": v.ID}, float64(v.Since.Unix()))
		for _, loc := range v.Locations {
			if loc.Last == nil {
				continue
			}
			ll := metrics.L{"monitor": v.ID, "location": loc.Name}
			add("vigil_check_latency_seconds", "Latency of the last probe.", ll, float64(loc.Last.LatencyMS)/1000)
			add("vigil_check_last_timestamp_seconds", "Unix time of the last probe.", ll, float64(loc.Last.At.Unix()))
		}
	}
	for _, a := range s.Engine.Agents() {
		add("vigil_agent_up", "1 if the agent has reported recently.", metrics.L{"agent": a.Name}, b(a.Online))
		add("vigil_agent_last_seen_seconds", "Unix time of the agent's last contact.", metrics.L{"agent": a.Name}, float64(a.LastSeen.Unix()))
	}
	open, err := s.Store.OpenIncidents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	add("vigil_incidents_open", "Open incidents.", nil, float64(len(open)))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.Metrics.Write(w, g)
}

type monitorOut struct {
	engine.View
	Uptime24h *float64 `json:"uptime_24h"`
	Uptime7d  *float64 `json:"uptime_7d"`
}

func (s *Server) withUptime(ctx context.Context) ([]monitorOut, error) {
	now := time.Now()
	views := s.Engine.Views()
	out := make([]monitorOut, len(views))
	for i, v := range views {
		out[i].View = v
		for _, w := range []struct {
			dst **float64
			d   time.Duration
		}{{&out[i].Uptime24h, 24 * time.Hour}, {&out[i].Uptime7d, 7 * 24 * time.Hour}} {
			p, ok, err := s.Store.Uptime(ctx, v.ID, now.Add(-w.d))
			if err != nil {
				return nil, err
			}
			if ok {
				*w.dst = &p
			}
		}
	}
	return out, nil
}

func (s *Server) monitors(w http.ResponseWriter, r *http.Request) {
	out, err := s.withUptime(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.Engine.Has(id) {
		writeJSON(w, http.StatusNotFound, errBody("unknown monitor"))
		return
	}
	since := 24 * time.Hour
	if v := r.URL.Query().Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeJSON(w, http.StatusBadRequest, errBody("since must be a duration like 1h"))
			return
		}
		since = d
	}
	limit := clampInt(r.URL.Query().Get("limit"), 500, 1, 10000)
	res, err := s.Store.Results(r.Context(), id, time.Now().Add(-since), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	type row struct {
		At        time.Time `json:"at"`
		Status    string    `json:"status"`
		LatencyMS int64     `json:"latency_ms"`
		Message   string    `json:"message"`
	}
	out := make([]row, len(res))
	for i, x := range res {
		out[i] = row{x.At, string(x.Status), x.Latency.Milliseconds(), x.Message}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) incidents(w http.ResponseWriter, r *http.Request) {
	incs, err := s.Store.RecentIncidents(r.Context(), clampInt(r.URL.Query().Get("limit"), 50, 1, 1000))
	if err != nil {
		s.fail(w, err)
		return
	}
	if incs == nil {
		incs = []store.Incident{}
	}
	writeJSON(w, http.StatusOK, incs)
}

func (s *Server) incident(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	inc, err := s.Store.Incident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// ---- write API ----

type createReq struct {
	Title      string   `json:"title"`
	Components []string `json:"components"`
	Impact     string   `json:"impact"`
	Message    string   `json:"message"`
}

type updateReq struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Title   string `json:"title"`
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON: "+err.Error()))
		return false
	}
	return true
}

func (s *Server) validateCreate(req createReq) error {
	if strings.TrimSpace(req.Title) == "" || len(req.Title) > 200 {
		return errors.New("title required (max 200 chars)")
	}
	if !store.ValidImpact(req.Impact) {
		return errors.New("impact must be down, degraded or none")
	}
	if len(req.Components) == 0 {
		return errors.New("at least one component required")
	}
	known := s.componentNames()
	for _, c := range req.Components {
		if _, ok := known[c]; !ok {
			return errors.New("unknown component " + strconv.Quote(c))
		}
	}
	if len(req.Message) > 5000 {
		return errors.New("message too long")
	}
	return nil
}

func (s *Server) doCreate(ctx context.Context, req createReq, who string) (int64, error) {
	if req.Impact == "" {
		req.Impact = "down"
	}
	if err := s.validateCreate(req); err != nil {
		return 0, err
	}
	id, err := s.Store.OpenIncident(ctx, store.NewIncident{
		Title: strings.TrimSpace(req.Title), Impact: req.Impact, Components: req.Components,
		At: time.Now().UTC(), Message: strings.TrimSpace(req.Message), Actor: who,
	})
	if err == nil {
		s.Log.Info("incident declared", "incident", id, "title", req.Title, "by", who)
		s.Publisher.Publish(id)
	}
	return id, err
}

func (s *Server) doUpdate(ctx context.Context, id int64, req updateReq, who string) error {
	if !store.ValidStatus(req.Status) {
		return errors.New("status must be investigating, identified, monitoring or resolved")
	}
	if strings.TrimSpace(req.Message) == "" || len(req.Message) > 5000 {
		return errors.New("message required (max 5000 chars)")
	}
	if len(req.Title) > 200 {
		return errors.New("title too long")
	}
	err := s.Store.AddUpdate(ctx, id, time.Now().UTC(), req.Status, strings.TrimSpace(req.Message), strings.TrimSpace(req.Title), who)
	if err == nil {
		s.Log.Info("incident updated", "incident", id, "status", req.Status, "by", who)
		s.Publisher.Publish(id)
	}
	return err
}

func (s *Server) createIncident(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !decode(w, r, &req) {
		return
	}
	id, err := s.doCreate(r.Context(), req, actor(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	inc, err := s.Store.Incident(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inc)
}

func (s *Server) addUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	var req updateReq
	if !decode(w, r, &req) {
		return
	}
	switch err := s.doUpdate(r.Context(), id, req, actor(r)); {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody("not found"))
	case errors.Is(err, store.ErrResolved):
		writeJSON(w, http.StatusConflict, errBody(err.Error()))
	case err != nil:
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
	default:
		inc, err := s.Store.Incident(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, inc)
	}
}

// ---- helpers ----

// componentNames maps component ID → name; with no layout configured every
// monitor is its own component (mirrors the status page fallback).
func (s *Server) componentNames() map[string]string {
	out := map[string]string{}
	var walk func([]config.Component)
	walk = func(cs []config.Component) {
		for _, c := range cs {
			out[c.ID] = c.Name
			walk(c.Components)
		}
	}
	walk(s.Cfg.StatusPage.Components)
	if len(out) == 0 {
		for _, m := range s.Cfg.Monitors {
			out[m.ID] = m.Name
		}
	}
	return out
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("api", "err", err)
	writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func clampInt(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}
