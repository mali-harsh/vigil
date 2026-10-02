package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mali-harsh/vigil/internal/agent"
	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
)

// maxClockSkew: agent timestamps further off than this are replaced with
// the server's receive time (a broken agent clock must not rewrite history).
const maxClockSkew = 5 * time.Minute

func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/v1/assignments", s.agentAuth(s.assignments))
	mux.HandleFunc("POST /api/agent/v1/results", s.agentAuth(s.agentResults))
}

func (s *Server) agentAuth(h func(http.ResponseWriter, *http.Request, config.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Cfg.AgentByToken(bearer(r))
		if !ok || bearer(r) == "" {
			writeJSON(w, http.StatusUnauthorized, errBody("invalid agent token"))
			return
		}
		if err := s.Engine.AgentSeen(a.Name); err != nil {
			writeJSON(w, http.StatusUnauthorized, errBody(err.Error()))
			return
		}
		h(w, r, a)
	}
}

func (s *Server) assignments(w http.ResponseWriter, r *http.Request, a config.Agent) {
	body, _ := json.Marshal(agent.Assignments{Agent: a.Name, PollSeconds: 20, Monitors: orEmpty(s.Engine.Assignments(a.Name))})
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) agentResults(w http.ResponseWriter, r *http.Request, a config.Agent) {
	var batch agent.ResultBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&batch); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}
	now := time.Now()
	var resp agent.BatchResponse
	for _, x := range batch.Results {
		valid := x.Status == check.Up || x.Status == check.Degraded || x.Status == check.Down
		if !valid || !s.Engine.AssignedTo(x.MonitorID, a.Name) {
			resp.Rejected++
			continue
		}
		at := x.At
		if d := now.Sub(at); d > maxClockSkew || d < -maxClockSkew {
			at = now
		}
		msg := x.Message
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		res := check.Result{MonitorID: x.MonitorID, Location: a.Name, At: at.UTC(), Status: x.Status, Latency: time.Duration(x.LatencyMS) * time.Millisecond, Message: msg}
		select {
		case s.Results <- res:
			resp.Accepted++
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
