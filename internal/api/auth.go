package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

// Principal is whoever is making a request: an API key, a UI session opened
// with a key, or a person identified by the SSO proxy.
type Principal struct {
	Name   string   // key name or email — recorded as the actor in audits
	Scopes []string // see config.Scopes
	Via    string   // "key" | "session" | "sso"
}

// Can reports whether the principal holds scope (admin holds every scope).
func (p *Principal) Can(scope string) bool {
	return p != nil && (slices.Contains(p.Scopes, "admin") || slices.Contains(p.Scopes, scope))
}

type ctxKey struct{}

func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

func actor(r *http.Request) string {
	if p := principalFrom(r.Context()); p != nil {
		return p.Name
	}
	return ""
}

// Headers a standby uses to forward an SSO identity it verified to the leader.
const (
	fwdIdentity = "X-Vigil-Identity"
	fwdSig      = "X-Vigil-Identity-Sig"
)

// Authn resolves principals.
type Authn struct {
	keys     []config.APIKey
	header   string
	proxies  []*net.IPNet
	users    []config.AuthUser
	nodeKey  []byte // shared by all nodes of one deployment (derived from the database URL)
	openRead bool   // no read credentials configured at all: read API stays public (first-run friendliness)
}

func NewAuthn(cfg *config.Config) *Authn {
	a := &Authn{keys: cfg.Server.Keys(), header: cfg.Server.Auth.Header, users: cfg.Server.Auth.Users}
	for _, c := range cfg.Server.Auth.TrustedProxies {
		if _, n, err := net.ParseCIDR(c); err == nil {
			a.proxies = append(a.proxies, n)
		}
	}
	if u := cfg.Server.Database.URL; u != "" {
		m := hmac.New(sha256.New, []byte(u))
		m.Write([]byte("vigil-node-identity-v1"))
		a.nodeKey = m.Sum(nil)
	}
	a.openRead = len(cfg.Server.Auth.Users) == 0
	for _, k := range a.keys {
		if slices.Contains(k.Scopes, "read") {
			a.openRead = false
		}
	}
	return a
}

// Enabled reports whether any credential exists (else the admin UI is off).
func (a *Authn) Enabled() bool { return len(a.keys) > 0 || len(a.users) > 0 }

// SSO reports whether people sign in through the proxy.
func (a *Authn) SSO() bool { return a.header != "" }

func bearer(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return t
	}
	return ""
}

// keyFor finds the key a secret belongs to, comparing in constant time
// against every key.
func (a *Authn) keyFor(secret string, derive func(string) string) *config.APIKey {
	if secret == "" {
		return nil
	}
	var found *config.APIKey
	for i := range a.keys {
		v := a.keys[i].Token
		if derive != nil {
			v = derive(v)
		}
		if subtle.ConstantTimeCompare([]byte(secret), []byte(v)) == 1 && found == nil {
			found = &a.keys[i]
		}
	}
	return found
}

// sessionValue derives the UI cookie from a key, so the cookie never contains
// the key itself and rotating the key logs everyone out.
func sessionValue(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("vigil-admin-session-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *Authn) Principal(r *http.Request) *Principal {
	if k := a.keyFor(bearer(r), nil); k != nil {
		return &Principal{Name: k.Name, Scopes: k.Scopes, Via: "key"}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if k := a.keyFor(c.Value, sessionValue); k != nil {
			return &Principal{Name: k.Name, Scopes: k.Scopes, Via: "session"}
		}
	}
	if email, ok := a.identity(r); ok {
		if role := a.role(email); role != "" {
			return &Principal{Name: email, Scopes: config.Roles[role], Via: "sso"}
		}
	}
	return nil
}

// identity returns the SSO email: from a standby's signed forward, or from
// the proxy header when the request comes from a trusted proxy.
func (a *Authn) identity(r *http.Request) (string, bool) {
	if a.header == "" {
		return "", false
	}
	if email, ok := a.verifyForward(r); ok {
		return email, true
	}
	return a.proxyIdentity(r)
}

// proxyIdentity reads the SSO header, only from a trusted proxy.
func (a *Authn) proxyIdentity(r *http.Request) (string, bool) {
	if a.header == "" || !a.trusted(r.RemoteAddr) {
		return "", false
	}
	v := strings.TrimSpace(r.Header.Get(a.header))
	if i := strings.LastIndex(v, ":"); i >= 0 { // Google IAP: "accounts.google.com:user@x.com"
		v = v[i+1:]
	}
	return strings.ToLower(v), v != ""
}

func (a *Authn) trusted(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	for _, n := range a.proxies {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *Authn) role(email string) string {
	best := ""
	for _, u := range a.users {
		match := strings.EqualFold(u.Email, email) ||
			(strings.HasPrefix(u.Email, "*@") && strings.HasSuffix(email, strings.ToLower(u.Email[1:])))
		if match && rank(u.Role) > rank(best) {
			best = u.Role
		}
	}
	return best
}

func rank(role string) int { return map[string]int{"viewer": 1, "responder": 2, "admin": 3}[role] }

// sign binds the identity to this exact request (method + path) and a
// timestamp, so a captured header can't be replayed elsewhere or later.
func (a *Authn) sign(email, ts string, r *http.Request) string {
	m := hmac.New(sha256.New, a.nodeKey)
	m.Write([]byte(email + "|" + ts + "|" + r.Method + "|" + r.URL.RequestURI()))
	return hex.EncodeToString(m.Sum(nil))
}

// SignForward runs on a standby before proxying: it strips any identity
// headers the client sent and, if the standby itself verified an SSO
// identity, re-attaches it signed for the leader.
func (a *Authn) SignForward(r *http.Request) {
	// only what this node verified itself — never re-sign a client's forward headers
	email, ok := a.proxyIdentity(r)
	r.Header.Del(fwdIdentity)
	r.Header.Del(fwdSig)
	if ok && len(a.nodeKey) > 0 {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		r.Header.Set(fwdIdentity, email)
		r.Header.Set(fwdSig, ts+"."+a.sign(email, ts, r))
	}
}

func (a *Authn) verifyForward(r *http.Request) (string, bool) {
	email, sig := r.Header.Get(fwdIdentity), r.Header.Get(fwdSig)
	if email == "" || len(a.nodeKey) == 0 {
		return "", false
	}
	ts, mac, ok := strings.Cut(sig, ".")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || time.Since(time.Unix(sec, 0)).Abs() > time.Minute {
		return "", false
	}
	return email, hmac.Equal([]byte(mac), []byte(a.sign(email, ts, r)))
}

// ---- middleware ----

// need guards JSON API routes with a scope.
func (s *Server) need(scope string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.auth.Principal(r)
		switch {
		case scope == "read" && s.auth.openRead:
		case p == nil:
			writeJSON(w, http.StatusUnauthorized, errBody("authentication required"))
			return
		case !p.Can(scope):
			writeJSON(w, http.StatusForbidden, errBody("missing scope "+scope))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}
