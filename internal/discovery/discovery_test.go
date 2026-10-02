package discovery

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestDockerLabels(t *testing.T) {
	ms := dockerMonitors([]dockerContainer{
		{Names: []string{"/api-1"}, Labels: map[string]string{"vigil.enable": "true", "vigil.name": "API", "vigil.url": "http://api:8080/health", "vigil.interval": "30s", "vigil.expect-status": "200, 204", "vigil.notify": "slack"}},
		{Names: []string{"/db"}, Labels: map[string]string{"vigil.enable": "true", "vigil.host": "db:5432"}},
		{Names: []string{"/off"}, Labels: map[string]string{"vigil.enable": "false", "vigil.url": "http://x"}},
		{Names: []string{"/bad"}, Labels: map[string]string{"vigil.enable": "true", "vigil.interval": "soon"}},
	}, quiet)
	if len(ms) != 2 {
		t.Fatalf("want 2 monitors, got %+v", ms)
	}
	api, db := ms[0], ms[1]
	if api.ID != "docker:api" || api.Type != "http" || api.Interval.D() != 30*time.Second || len(api.Expect.Status) != 2 || api.Notify[0] != "slack" {
		t.Errorf("api: %+v", api)
	}
	if db.ID != "docker:db" || db.Type != "tcp" || db.Host != "db:5432" {
		t.Errorf("db: %+v", db)
	}
}

func TestKubeAnnotations(t *testing.T) {
	svc := func(ns, name string, port int, anno map[string]string) kubeService {
		var s kubeService
		s.Metadata.Name, s.Metadata.Namespace, s.Metadata.Annotations = name, ns, anno
		s.Spec.Ports = append(s.Spec.Ports, struct {
			Port int `json:"port"`
		}{port})
		return s
	}
	ms := kubeMonitors([]kubeService{
		svc("prod", "geo", 8282, map[string]string{"vigil.dev/enable": "true", "vigil.dev/path": "/health"}),
		svc("prod", "redis", 6379, map[string]string{"vigil.dev/enable": "true"}),
		svc("prod", "ignored", 80, nil),
	}, quiet)
	if len(ms) != 2 {
		t.Fatalf("got %+v", ms)
	}
	if ms[0].ID != "k8s:prod.geo" || ms[0].URL != "http://geo.prod.svc:8282/health" || ms[0].Type != "http" {
		t.Errorf("geo: %+v", ms[0])
	}
	if ms[1].Host != "redis.prod.svc:6379" || ms[1].Type != "tcp" {
		t.Errorf("redis: %+v", ms[1])
	}
	// defaults + validation make them indistinguishable from configured monitors
	cfg, _ := config.Parse([]byte(`defaults: {interval: 45s}`))
	m := ms[0]
	cfg.DefaultMonitor(&m)
	if err := cfg.ValidateMonitor(m); err != nil || m.Interval.D() != 45*time.Second || m.Locations[0] != "local" {
		t.Fatalf("defaulted: %+v err=%v", m, err)
	}
}
