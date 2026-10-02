package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/agent"
	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/metrics"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/scheduler"
	"github.com/mali-harsh/vigil/internal/store/storetest"
)

const agentToken = "agent-token-0123456789abcdef"

func TestAgentEndToEnd(t *testing.T) {
	// a "private" target only the agent is meant to reach
	var code atomic.Int32
	code.Store(200)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) }))
	defer target.Close()

	cfg, err := config.Parse([]byte(`
agents: [{name: vpc, token: "` + agentToken + `"}]
monitors:
  - {name: private-api, type: http, url: "` + target.URL + `", interval: 1s, timeout: 500ms, fail_threshold: 2, recover_threshold: 1, locations: [vpc]}
  - {name: local-only, type: tcp, host: "127.0.0.1:1", interval: 1h, timeout: 1s}
`))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := storetest.OpenAt(t, filepath.Join(t.TempDir(), "v.db"))
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan check.Result, 256)
	sched := scheduler.New(results)
	reg := metrics.New()
	eng, err := engine.New(ctx, cfg, st, nopNotifier{}, sched, reg, log)
	if err != nil {
		t.Fatal(err)
	}
	go eng.Run(ctx, results)
	srv := httptest.NewServer((&Server{Cfg: cfg, Engine: eng, Store: st, Beater: sched, Results: results, Metrics: reg, Version: "test", Log: log}).Handler())
	defer srv.Close()

	// wrong token is refused at first contact
	bad := &agent.Agent{Server: srv.URL, Token: "nope-nope-nope-nope-nope", Log: log}
	if err := bad.Run(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}

	actx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan struct{})
	go func() { (&agent.Agent{Server: srv.URL, Token: agentToken, Log: log}).Run(actx); close(agentDone) }()

	state := func() (monitor.State, string) {
		for _, v := range eng.Views() {
			if v.ID == "private-api" {
				loc := ""
				if v.Last != nil {
					loc = v.Last.Location
				}
				return v.State, loc
			}
		}
		return "", ""
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out: %s", what)
	}
	wait("up via agent", func() bool { s, loc := state(); return s == monitor.Up && loc == "vpc" })
	code.Store(503)
	wait("down via agent", func() bool { s, _ := state(); return s == monitor.Down })
	code.Store(200)
	wait("recovered via agent", func() bool { s, _ := state(); return s == monitor.Up })

	// agent may only report monitors assigned to it
	body := `{"results":[{"monitor_id":"local-only","at":"2026-01-01T00:00:00Z","status":"down","message":"forged"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/agent/v1/results", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"rejected":1`) {
		t.Fatalf("forged result accepted: %s", b)
	}

	// metrics expose the agent and the monitor
	_, m := get(t, srv, "/metrics", "")
	for _, want := range []string{`vigil_agent_up{agent="vpc"} 1`, `vigil_monitor_up{monitor="private-api",source="config",type="http"} 1`, `vigil_check_results_total{location="vpc",monitor="private-api",status="down"}`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	stopAgent()
	<-agentDone
}

func TestAgentRetriesUntilServerIsUp(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "failing over", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"agent":"edge","poll_seconds":5,"monitors":[]}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&agent.Agent{Server: srv.URL, Token: agentToken, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Run(ctx)
	}()
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("agent gave up on a 503: %v", err)
	default:
	}
	up.Store(true)
	time.Sleep(2500 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("agent: %v", err)
	}
}
