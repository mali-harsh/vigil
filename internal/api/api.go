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
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/statuspage"
	"github.com/mali-harsh/vigil/internal/store"
)

type Beater interface {
	Beat(token string, at time.Time) bool
}

type Server struct {
	Cfg    *config.Config
	Engine *engine.Engine
	Store  *store.Store
	Beater Beater
	Log    *slog.Logger

	page *statuspage.Builder
}

func (s *Server) Handler() http.Handler {
	s.page = &statuspage.Builder{Cfg: s.Cfg, Engine: s.Engine, Store: s.Store}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.statusPage)
	mux.HandleFunc("GET /api/status", s.statusJSON)
	mux.HandleFunc("GET /badge/{id}", s.badge)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("/push/{token}", s.push) // GET/POST/HEAD: whatever the cron job can do

	mux.Handle("GET /api/v1/monitors", s.read(s.monitors))
	mux.Handle("GET /api/v1/monitors/{id}/results", s.read(s.results))
	mux.Handle("GET /api/v1/incidents", s.read(s.incidents))
	mux.Handle("GET /api/v1/incidents/{id}", s.read(s.incident))
	mux.Handle("POST /api/v1/incidents", s.admin(s.createIncident))
	mux.Handle("POST /api/v1/incidents/{id}/updates", s.admin(s.addUpdate))

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

// ---- auth ----

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return t
	}
	return ""
}

func anyMatch(got string, tokens []string) bool {
	ok := false
	for _, t := range tokens {
		// evaluate every token: timing doesn't reveal which (or whether one) matched
		ok = subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 || ok
	}
	return ok && got != ""
}

// sessionValue derives the admin cookie from a token, so the cookie never
// contains the token itself and rotating the token logs everyone out.
func sessionValue(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("vigil-admin-session-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) isAdmin(r *http.Request) bool {
	tokens := s.Cfg.Server.AdminTokens
	if len(tokens) == 0 {
		return false
	}
	if anyMatch(bearer(r), tokens) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	sessions := make([]string, len(tokens))
	for i, t := range tokens {
		sessions[i] = sessionValue(t)
	}
	return anyMatch(c.Value, sessions)
}

func (s *Server) read(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.Cfg.Server.APITokens) > 0 && !anyMatch(bearer(r), s.Cfg.Server.APITokens) && !s.isAdmin(r) {
			writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
			return
		}
		h(w, r)
	})
}

func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			writeJSON(w, http.StatusUnauthorized, errBody("admin token required"))
			return
		}
		h(w, r)
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
	html, err := statuspage.Render(p, true)
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

func (s *Server) doCreate(ctx context.Context, req createReq) (int64, error) {
	if req.Impact == "" {
		req.Impact = "down"
	}
	if err := s.validateCreate(req); err != nil {
		return 0, err
	}
	id, err := s.Store.OpenIncident(ctx, store.NewIncident{
		Title: strings.TrimSpace(req.Title), Impact: req.Impact, Components: req.Components,
		At: time.Now().UTC(), Message: strings.TrimSpace(req.Message),
	})
	if err == nil {
		s.Log.Info("incident declared", "incident", id, "title", req.Title)
	}
	return id, err
}

func (s *Server) doUpdate(ctx context.Context, id int64, req updateReq) error {
	if !store.ValidStatus(req.Status) {
		return errors.New("status must be investigating, identified, monitoring or resolved")
	}
	if strings.TrimSpace(req.Message) == "" || len(req.Message) > 5000 {
		return errors.New("message required (max 5000 chars)")
	}
	if len(req.Title) > 200 {
		return errors.New("title too long")
	}
	err := s.Store.AddUpdate(ctx, id, time.Now().UTC(), req.Status, strings.TrimSpace(req.Message), strings.TrimSpace(req.Title))
	if err == nil {
		s.Log.Info("incident updated", "incident", id, "status", req.Status)
	}
	return err
}

func (s *Server) createIncident(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !decode(w, r, &req) {
		return
	}
	id, err := s.doCreate(r.Context(), req)
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
	switch err := s.doUpdate(r.Context(), id, req); {
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
