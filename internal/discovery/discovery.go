// Package discovery turns Docker labels and Kubernetes annotations into
// monitors, so workloads opt in to monitoring where they are defined.
//
// Both providers use the same keys (Docker label "vigil.<key>", Kubernetes
// annotation "vigil.dev/<key>"):
//
//	enable         "true" to opt in (required)
//	name           display name (default: container / service name)
//	type           http | tcp | tls | dns (default: http if url, else tcp)
//	url            http target            e.g. http://api:8080/health
//	host           tcp/tls target         e.g. db:5432
//	interval       e.g. 30s
//	timeout        e.g. 5s
//	expect-status  e.g. 200,204
//	body-contains  substring that must be in the body
//	max-latency    over this = degraded, e.g. 2s
//	notify         comma-separated notifier names
//	locations      comma-separated (local and/or agent names)
//
// Discovered monitors appear in the admin UI, API, metrics and alerts. They
// are not placed on the public status page (that layout stays in config).
// A discovered monitor is removed only when its container/service is
// deleted; a stopped container stays monitored, so it alerts as DOWN.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

// Syncer receives the full desired set for a source (engine.Sync).
type Syncer interface {
	Sync(ctx context.Context, source string, ms []config.Monitor) error
}

// Provider lists the monitors currently declared in its system.
type Provider interface {
	Name() string // source: "docker", "kubernetes"
	List(ctx context.Context) ([]config.Monitor, error)
}

// Run polls p every interval and syncs. A failed list keeps the previous set
// (an unreachable Docker/API server must not delete every monitor).
func Run(ctx context.Context, p Provider, every time.Duration, cfg *config.Config, sink Syncer, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ms, err := p.List(ctx)
		if err != nil {
			log.Warn("discovery failed; keeping previous monitors", "provider", p.Name(), "err", err)
		} else {
			var ok []config.Monitor
			for _, m := range ms {
				cfg.DefaultMonitor(&m)
				if err := cfg.ValidateMonitor(m); err != nil {
					log.Warn("ignoring invalid discovered monitor", "provider", p.Name(), "monitor", m.ID, "err", err)
					continue
				}
				ok = append(ok, m)
			}
			if err := sink.Sync(ctx, p.Name(), ok); err != nil {
				log.Error("discovery sync", "provider", p.Name(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fromKeys builds a monitor from label/annotation values (keys without
// prefix). id must be stable across restarts/recreations.
func fromKeys(id, defaultName string, kv map[string]string) (config.Monitor, bool, error) {
	if !isTrue(kv["enable"]) {
		return config.Monitor{}, false, nil
	}
	m := config.Monitor{ID: id, Name: kv["name"], Type: kv["type"], URL: kv["url"], Host: kv["host"]}
	if m.Name == "" {
		m.Name = defaultName
	}
	if m.Type == "" {
		m.Type = "tcp"
		if m.URL != "" {
			m.Type = "http"
		}
	}
	var err error
	dur := func(key string, dst *config.Duration) {
		if v := kv[key]; v != "" && err == nil {
			d, e := time.ParseDuration(v)
			if e != nil {
				err = fmt.Errorf("%s: %v", key, e)
			}
			*dst = config.Duration(d)
		}
	}
	dur("interval", &m.Interval)
	dur("timeout", &m.Timeout)
	dur("max-latency", &m.Expect.MaxLatency)
	if v := kv["expect-status"]; v != "" {
		for _, p := range list(v) {
			n, e := strconv.Atoi(p)
			if e != nil {
				return m, true, fmt.Errorf("expect-status: %q is not a number", p)
			}
			m.Expect.Status = append(m.Expect.Status, n)
		}
	}
	m.Expect.BodyContains = kv["body-contains"]
	if v := kv["notify"]; v != "" {
		m.Notify = list(v)
	}
	if v := kv["locations"]; v != "" {
		m.Locations = list(v)
	}
	return m, true, err
}

func isTrue(s string) bool { b, _ := strconv.ParseBool(strings.TrimSpace(s)); return b }

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func keysWithPrefix(m map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			out[rest] = v
		}
	}
	return out
}
