package api

import (
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/store"
)

// limiter: at most n events per key per window (in memory; per node).
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.n {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	if len(l.hits) > 50000 { // bound memory under a flood
		clear(l.hits)
	}
	return true
}

// clientIP honours X-Forwarded-For only from trusted proxies.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.auth.trusted(r.RemoteAddr) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

func (s *Server) subscribeEnabled() bool {
	return s.Cfg.StatusPage.Subscribe && s.Publisher.MailConfigured()
}

// subscribe always answers the same way, so it can't be used to learn who
// is subscribed; it only mails when the address is new or still pending.
func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	if !s.subscribeEnabled() {
		http.NotFound(w, r)
		return
	}
	done := func() { http.Redirect(w, r, "/?subscribed=1#subscribe", http.StatusSeeOther) }
	if r.PostFormValue("website") != "" { // honeypot: humans never see this field
		done()
		return
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(r.PostFormValue("email")))
	if err != nil || len(addr.Address) > 254 || addr.Name != "" {
		http.Redirect(w, r, "/?subscribe_error=1#subscribe", http.StatusSeeOther)
		return
	}
	if !s.subPerIP.allow(s.clientIP(r)) || !s.subGlobal.allow("all") {
		http.Error(w, "too many sign-ups, try again later", http.StatusTooManyRequests)
		return
	}
	sub, confirmed, err := s.Store.AddSubscriber(r.Context(), "email", addr.Address, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !confirmed {
		if err := s.Publisher.SendConfirmation(r.Context(), sub); err != nil {
			s.Log.Warn("confirmation mail failed", "err", err)
		}
	}
	done()
}

func (s *Server) tokenPage(w http.ResponseWriter, r *http.Request, name string) {
	sub, err := s.Store.SubscriberByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, name, map[string]any{"Sub": sub, "Done": r.URL.Query().Get("done") != ""})
}

// GET pages only show a button: link scanners must not confirm or unsubscribe.
func (s *Server) confirmPage(w http.ResponseWriter, r *http.Request) {
	s.tokenPage(w, r, "confirm")
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.ConfirmSubscriber(r.Context(), r.PathValue("token")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "link expired or already used", http.StatusNotFound)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, r.URL.Path+"?done=1", http.StatusSeeOther)
}

func (s *Server) unsubscribePage(w http.ResponseWriter, r *http.Request) {
	s.tokenPage(w, r, "unsubscribe")
}

// unsubscribe handles the page's button and RFC 8058 one-click POSTs from
// mail clients (body "List-Unsubscribe=One-Click", cross-origin by design).
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Unsubscribe(r.Context(), r.PathValue("token")); err != nil {
		s.fail(w, err)
		return
	}
	if r.PostFormValue("List-Unsubscribe") == "One-Click" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, r.URL.Path+"?done=1", http.StatusSeeOther)
}

// ---- webhook subscribers (API) ----

func (s *Server) listSubscribers(w http.ResponseWriter, r *http.Request) {
	subs, err := s.Store.Subscribers(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(subs))
}

func (s *Server) addSubscriber(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Address string `json:"address"`
	}
	if !decode(w, r, &req) {
		return
	}
	switch req.Kind {
	case "webhook":
		if u, err := url.Parse(req.Address); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			writeJSON(w, http.StatusBadRequest, errBody("address must be an http(s) URL"))
			return
		}
	case "email":
		if _, err := mail.ParseAddress(req.Address); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody("invalid email"))
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, errBody("kind must be webhook or email"))
		return
	}
	// added by an operator: no double opt-in needed
	sub, _, err := s.Store.AddSubscriber(r.Context(), req.Kind, req.Address, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"subscriber": sub, "secret": sub.Token,
		"note": "secret signs webhook bodies (X-Vigil-Signature: sha256=HMAC) and is shown only once"})
}

func (s *Server) deleteSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	if ok, err := s.Store.DeleteSubscriber(r.Context(), id); err != nil {
		s.fail(w, err)
	} else if !ok {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}
