package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// LeaderResolver returns the current leader's internal URL ("" if none).
type LeaderResolver func(ctx context.Context) (string, error)

// proxiedHeader marks a request a standby already forwarded: if a stale
// lease points at another standby, it answers 503 instead of looping.
const proxiedHeader = "X-Vigil-Proxied"

// Switch serves the full app while this node leads. As a standby it stays
// healthy (/healthz 200, so orchestrators keep it and rollouts proceed) and
// transparently proxies every other request to the leader — any load
// balancer can sit in front of all nodes. /readyz is 200 only on the leader,
// for balancers that prefer to route to it directly.
type Switch struct {
	active  atomic.Pointer[http.Handler]
	node    string
	version string
	leader  LeaderResolver

	mu       sync.Mutex
	target   string
	proxy    *httputil.ReverseProxy
	resolved time.Time
}

func NewSwitch(node, version string, leader LeaderResolver) *Switch {
	return &Switch{node: node, version: version, leader: leader}
}

// Set installs the active handler (nil = standby).
func (s *Switch) Set(h http.Handler) {
	if h == nil {
		s.active.Store(nil)
		return
	}
	s.active.Store(&h)
}

func (s *Switch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.active.Load(); h != nil {
		if r.URL.Path == "/readyz" || r.URL.Path == "/healthz" {
			w.Header().Set("X-Vigil-Node", s.node)
		}
		(*h).ServeHTTP(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "standby", "node": s.node})
		return
	case "/readyz":
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "standby", "node": s.node})
		return
	case "/metrics": // this node's own view; scrape every node
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP vigil_leader 1 on the active (leader) node.\n# TYPE vigil_leader gauge\nvigil_leader{node=%q} 0\n", s.node)
		fmt.Fprintf(w, "# HELP vigil_build_info vigil build.\n# TYPE vigil_build_info gauge\nvigil_build_info{version=%q} 1\n", s.version)
		return
	}
	unavailable := func(msg string) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg, "node": s.node})
	}
	if r.Header.Get(proxiedHeader) != "" {
		unavailable("standby node: no leader reachable (failover in progress?)")
		return
	}
	p := s.proxyToLeader(r.Context())
	if p == nil {
		unavailable("standby node: no leader elected right now (failover in progress?)")
		return
	}
	r.Header.Set(proxiedHeader, s.node)
	p.ServeHTTP(w, r)
}

// proxyToLeader returns a proxy for the current leader, re-resolving at most
// once a second.
func (s *Switch) proxyToLeader(ctx context.Context) *httputil.ReverseProxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.resolved) < time.Second {
		return s.proxy
	}
	s.resolved = time.Now()
	if s.leader == nil {
		s.proxy = nil
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addr, err := s.leader(rctx)
	if err != nil || addr == "" {
		s.proxy, s.target = nil, ""
		return nil
	}
	if addr == s.target && s.proxy != nil {
		return s.proxy
	}
	u, err := url.Parse(addr)
	if err != nil {
		s.proxy = nil
		return nil
	}
	p := httputil.NewSingleHostReverseProxy(u)
	orig := p.Director
	p.Director = func(r *http.Request) {
		host := r.Host
		orig(r)
		r.Host = host // keep the public Host (status page links, cookies)
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "leader unreachable: " + err.Error(), "node": s.node})
	}
	s.proxy, s.target = p, addr
	return p
}
