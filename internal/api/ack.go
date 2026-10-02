package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mali-harsh/vigil/internal/acklink"
	"github.com/mali-harsh/vigil/internal/store"
)

// Notifiers is the dispatcher surface the admin side needs.
type Notifiers interface {
	Names() []string
	Test(ctx context.Context, name string) error
}

func incidentID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func ackStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrResolved):
		return http.StatusConflict
	case err != nil:
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

func (s *Server) ackAPI(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentID(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	if err := s.Engine.Ack(r.Context(), id, actor(r)); err != nil {
		writeJSON(w, ackStatus(err), errBody(err.Error()))
		return
	}
	inc, err := s.Store.Incident(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

func (s *Server) incidentAckForm(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	redirectResult(w, r, s.Engine.Ack(r.Context(), id, actor(r)))
}

// ackPage answers GET on a link from an alert with a confirmation form.
// GET must never acknowledge: Slack unfurlers and mail security scanners
// fetch every link in a message.
func (s *Server) ackPage(w http.ResponseWriter, r *http.Request) {
	id, ok := acklink.Verify(s.Cfg.Server.AckSecret, r.PathValue("token"))
	if !ok {
		http.Error(w, "invalid or expired acknowledge link", http.StatusNotFound)
		return
	}
	inc, err := s.Store.Incident(r.Context(), id)
	if err != nil {
		http.Error(w, "incident not found", ackStatus(err))
		return
	}
	who := ""
	if p := s.auth.Principal(r); p != nil {
		who = p.Name
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // the token is in the URL
	s.render(w, r, "ack", map[string]any{"Inc": inc, "Who": who, "Done": r.URL.Query().Get("done") != ""})
}

func (s *Server) ackLink(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	id, ok := acklink.Verify(s.Cfg.Server.AckSecret, token)
	if !ok || !sameOrigin(r) {
		http.Error(w, "invalid acknowledge link", http.StatusNotFound)
		return
	}
	who := strings.TrimSpace(r.PostFormValue("name"))
	if p := s.auth.Principal(r); p != nil {
		who = p.Name // a signed-in identity beats a typed name
	}
	if who == "" {
		who = "someone"
	}
	if len(who) > 80 {
		who = who[:80]
	}
	if err := s.Engine.Ack(r.Context(), id, who+" (alert link)"); err != nil && ackStatus(err) != http.StatusOK {
		http.Error(w, err.Error(), ackStatus(err))
		return
	}
	http.Redirect(w, r, "/ack/"+token+"?done=1", http.StatusSeeOther)
}

func (s *Server) testNotifier(w http.ResponseWriter, r *http.Request) {
	if s.Notifiers == nil {
		writeJSON(w, http.StatusNotFound, errBody("no notifiers"))
		return
	}
	if err := s.Notifiers.Test(r.Context(), r.PathValue("name")); err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) notifierTestForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	msg := "Test sent to " + name + " — check the channel."
	if s.Notifiers == nil {
		msg = "no notifiers configured"
	} else if err := s.Notifiers.Test(r.Context(), name); err != nil {
		msg = name + " failed: " + err.Error()
	}
	http.Redirect(w, r, "/admin/system?flash="+urlQueryEscape(msg), http.StatusSeeOther)
}
