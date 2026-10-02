package engine_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/scheduler"
	"github.com/mali-harsh/vigil/internal/store"
)

func TestDerive(t *testing.T) {
	U, G, D, X := monitor.Up, monitor.Degraded, monitor.Down, monitor.Unknown
	cases := []struct {
		in     []monitor.State
		quorum int
		want   monitor.State
	}{
		{[]monitor.State{D}, 1, D},
		{[]monitor.State{U, U, D}, 2, U}, // one region failing: noise
		{[]monitor.State{U, D, D}, 2, D},
		{[]monitor.State{D, D, D}, 3, D},
		{[]monitor.State{U, D, D}, 3, U},
		{[]monitor.State{G, D, U}, 2, G}, // two unhealthy, one actually down: degraded
		{[]monitor.State{X, X, D}, 2, U}, // a single failing opinion is not quorum
		{[]monitor.State{X, X}, 1, X},
	}
	for _, c := range cases {
		if got := engine.Derive(c.in, c.quorum); got != c.want {
			t.Errorf("derive(%v, q=%d) = %s, want %s", c.in, c.quorum, got, c.want)
		}
	}
}

// manual harness: results are injected directly, as the agent API would.
type manual struct {
	eng *engine.Engine
	st  *store.Store
	rec *recorder
	in  chan check.Result
}

func startManual(t *testing.T, cfg *config.Config, agentTimeout time.Duration) *manual {
	t.Helper()
	cfg.Server.Retention = config.Duration(time.Hour)
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	in := make(chan check.Result, 64)
	eng, err := engine.New(ctx, cfg, st, rec, scheduler.New(in), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	eng.SetTimings(agentTimeout, 20*time.Millisecond)
	done := make(chan struct{})
	go func() { eng.Run(ctx, in); close(done) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	return &manual{eng: eng, st: st, rec: rec, in: in}
}

func (h *manual) feed(loc string, st check.Status, n int) {
	for range n {
		h.in <- check.Result{MonitorID: "api", Location: loc, At: time.Now(), Status: st, Message: string(st) + " from " + loc}
	}
}

func (h *manual) view() engine.View {
	for _, v := range h.eng.Views() {
		if v.ID == "api" {
			return v
		}
	}
	return engine.View{}
}

func multiCfg() *config.Config {
	return &config.Config{
		Agents: []config.Agent{{Name: "mumbai", Token: "t1"}, {Name: "singapore", Token: "t2"}},
		Monitors: []config.Monitor{{
			ID: "api", Name: "API", Type: "http", URL: "http://192.0.2.1", Method: "GET",
			Interval: config.Duration(time.Hour), Timeout: config.Duration(time.Second),
			FailThreshold: 2, RecoverThreshold: 1,
			Locations: []string{"mumbai", "singapore", "local"}, Quorum: 2,
		}},
	}
}

func TestQuorumAcrossLocations(t *testing.T) {
	h := startManual(t, multiCfg(), time.Hour)
	for _, l := range []string{"mumbai", "singapore", "local"} {
		h.feed(l, check.Up, 1)
	}
	waitFor(t, "up", func() bool { return h.view().State == monitor.Up })

	h.feed("singapore", check.Down, 3) // one region confirms down
	time.Sleep(50 * time.Millisecond)
	if v := h.view(); v.State != monitor.Up || len(h.rec.kinds()) != 0 {
		t.Fatalf("single-region failure must not alert: state=%s alerts=%v", v.State, h.rec.kinds())
	}

	h.feed("mumbai", check.Down, 2) // second region: quorum reached
	waitFor(t, "down", func() bool { return h.view().State == monitor.Down })
	inc, _ := h.st.ActiveIncident(context.Background(), "api")
	if inc == nil || inc.Cause != "[mumbai] down from mumbai" {
		t.Fatalf("incident should name the confirming location: %+v", inc)
	}

	h.feed("mumbai", check.Up, 1) // back below quorum
	waitFor(t, "recovered", func() bool { return h.view().State == monitor.Up })
	if k := h.rec.kinds(); len(k) != 2 || k[0] != notify.KindDown || k[1] != notify.KindRecovered {
		t.Fatalf("want [down recovered], got %v", k)
	}
}

func TestAgentOfflineIsNotAnOutage(t *testing.T) {
	cfg := multiCfg()
	cfg.Monitors[0].Locations, cfg.Monitors[0].Quorum = []string{"mumbai"}, 1
	h := startManual(t, cfg, 150*time.Millisecond)
	waitFor(t, "first contact", func() bool { return h.eng.AgentSeen("mumbai") == nil })
	h.feed("mumbai", check.Up, 1)
	waitFor(t, "up", func() bool { return h.view().State == monitor.Up })

	// agent goes silent (no AgentSeen); singapore never connects at all
	waitFor(t, "offline alerts", func() bool { return len(h.rec.kinds()) == 2 })
	for _, k := range h.rec.kinds() {
		if k != notify.KindAgentOffline {
			t.Fatalf("only agent-offline alerts expected, got %v", h.rec.kinds())
		}
	}
	v := h.view()
	if v.State != monitor.Up || !v.Stale {
		t.Fatalf("monitor must stay frozen at last state and be stale: %+v", v)
	}
	if open, _ := h.st.OpenIncidents(context.Background()); len(open) != 0 {
		t.Fatal("agent loss must not open an incident")
	}

	h.eng.AgentSeen("mumbai")
	waitFor(t, "online alert", func() bool { return len(h.rec.kinds()) == 3 })
	if k := h.rec.kinds()[2]; k != notify.KindAgentOnline {
		t.Fatalf("got %v", k)
	}
	if h.view().Stale {
		t.Fatal("still stale after reconnect")
	}
}

func TestUnassignedLocationIgnored(t *testing.T) {
	cfg := multiCfg()
	cfg.Monitors[0].Locations, cfg.Monitors[0].Quorum = []string{"mumbai"}, 1
	h := startManual(t, cfg, time.Hour)
	h.feed("singapore", check.Down, 5) // not assigned there
	time.Sleep(50 * time.Millisecond)
	if v := h.view(); v.State != monitor.Unknown {
		t.Fatalf("result from unassigned location changed state: %s", v.State)
	}
}

func TestHeartbeatDeadlineSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "v.db")
	m := config.Monitor{
		ID: "cron", Name: "Cron", Type: "push", Token: "0123456789abcdef",
		Interval: config.Duration(400 * time.Millisecond), Grace: config.Duration(100 * time.Millisecond),
		FailThreshold: 1, RecoverThreshold: 1,
	}
	h := start(t, db, m)
	h.sched.Beat(m.Token, time.Now())
	waitFor(t, "up", func() bool { return h.state("cron") == monitor.Up })
	h.shutdown()

	time.Sleep(600 * time.Millisecond) // deadline (500ms) passes while vigil is down
	h2 := start(t, db, m)
	t0 := time.Now()
	waitFor(t, "down", func() bool { return h2.state("cron") == monitor.Down })
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Fatalf("missed deadline took %s to detect after restart; should be immediate", d)
	}
}

func TestDiscoverySync(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	h := start(t, filepath.Join(t.TempDir(), "v.db"))
	ctx := context.Background()
	m := httpMonitor(tg.URL)
	m.ID, m.Name = "docker:web", "web"
	if err := h.eng.Sync(ctx, "docker", []config.Monitor{m}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "discovered monitor down", func() bool { return h.state("docker:web") == monitor.Down })

	changed := m
	changed.Expect.Status = []int{503} // now 503 is "healthy"
	h.eng.Sync(ctx, "docker", []config.Monitor{changed})
	waitFor(t, "changed config applied", func() bool { return h.state("docker:web") == monitor.Up })

	tg.code.Store(500)
	waitFor(t, "down again", func() bool { return h.state("docker:web") == monitor.Down })
	h.eng.Sync(ctx, "docker", nil) // container deleted
	if h.eng.Has("docker:web") {
		t.Fatal("monitor not removed")
	}
	if open, _ := h.st.OpenIncidents(ctx); len(open) != 0 {
		t.Fatalf("removed monitor left incident open: %+v", open)
	}
	// a static monitor is never touched by a discovery source
	h.eng.Sync(ctx, "kubernetes", nil)
}

func TestDeadAgentDoesNotBlindDetection(t *testing.T) {
	cfg := multiCfg()
	cfg.Monitors[0].Locations, cfg.Monitors[0].Quorum = []string{"local", "mumbai"}, 2
	cfg.Agents = cfg.Agents[:1]
	h := startManual(t, cfg, 150*time.Millisecond)
	h.feed("local", check.Up, 1)
	h.feed("mumbai", check.Up, 1)
	waitFor(t, "up", func() bool { return h.view().State == monitor.Up })
	waitFor(t, "mumbai offline", func() bool { return len(h.rec.kinds()) == 1 }) // agent-offline alert

	h.feed("local", check.Down, 2) // real outage, only local can see it now
	waitFor(t, "down detected with the agent gone", func() bool { return h.view().State == monitor.Down })
}

func TestLosingTheAgentsThatSawAnOutageIsNotARecovery(t *testing.T) {
	cfg := multiCfg() // locations mumbai, singapore, local; quorum 2
	h := startManual(t, cfg, 150*time.Millisecond)
	keepAlive := func() { h.eng.AgentSeen("mumbai"); h.eng.AgentSeen("singapore") }
	keepAlive()
	h.feed("local", check.Up, 1)
	h.feed("mumbai", check.Down, 2)
	h.feed("singapore", check.Down, 2)
	waitFor(t, "down", func() bool { keepAlive(); return h.view().State == monitor.Down })

	// both agents die; local keeps saying up
	time.Sleep(300 * time.Millisecond)
	h.feed("local", check.Up, 3)
	time.Sleep(50 * time.Millisecond)
	if s := h.view().State; s != monitor.Down {
		t.Fatalf("losing the vantage points that saw the outage must not recover it, got %s", s)
	}
	for _, k := range h.rec.kinds() {
		if k == notify.KindRecovered {
			t.Fatal("false recovery alert")
		}
	}
}
