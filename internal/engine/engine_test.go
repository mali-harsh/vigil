package engine_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/scheduler"
	"github.com/mali-harsh/vigil/internal/store"
	"github.com/mali-harsh/vigil/internal/store/storetest"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Notify(e notify.Event, _ []string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) kinds() []notify.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	var k []notify.Kind
	for _, e := range r.events {
		k = append(k, e.Kind)
	}
	return k
}

// target is an HTTP server whose health we flip at will.
type target struct {
	*httptest.Server
	code atomic.Int32
	hits atomic.Int32
}

func newTarget(t *testing.T) *target {
	tg := &target{}
	tg.code.Store(200)
	tg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tg.hits.Add(1)
		w.WriteHeader(int(tg.code.Load()))
	}))
	t.Cleanup(tg.Close)
	return tg
}

type harness struct {
	eng   *engine.Engine
	st    *store.Store
	rec   *recorder
	sched *scheduler.Scheduler
	stop  func()
}

func start(t *testing.T, dbPath string, ms ...config.Monitor) *harness {
	t.Helper()
	return startCfg(t, dbPath, &config.Config{Monitors: ms})
}

func startCfg(t *testing.T, dbPath string, cfg *config.Config) *harness {
	t.Helper()
	cfg.Server.Retention = config.Duration(time.Hour)
	st := storetest.OpenAt(t, dbPath)
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &recorder{}
	results := make(chan check.Result, 64)
	sched := scheduler.New(results)
	eng, err := engine.New(ctx, cfg, st, rec, sched, nil, log) // engine starts the monitors
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { eng.Run(ctx, results); close(done) }()
	h := &harness{eng: eng, st: st, rec: rec, sched: sched}
	h.stop = func() { cancel(); sched.Wait(); <-done; st.Close() }
	t.Cleanup(func() {
		if h.stop != nil {
			h.stop()
		}
	})
	return h
}

func (h *harness) shutdown() { h.stop(); h.stop = nil }

func (h *harness) state(id string) monitor.State {
	for _, v := range h.eng.Views() {
		if v.ID == id {
			return v.State
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func httpMonitor(url string) config.Monitor {
	return config.Monitor{
		ID: "web", Name: "Web", Type: "http", URL: url, Method: "GET",
		Interval: config.Duration(40 * time.Millisecond), Timeout: config.Duration(30 * time.Millisecond),
		FailThreshold: 3, RecoverThreshold: 2,
	}
}

func TestOutageLifecycle(t *testing.T) {
	tg := newTarget(t)
	h := start(t, filepath.Join(t.TempDir(), "v.db"), httpMonitor(tg.URL))

	waitFor(t, "up", func() bool { return h.state("web") == monitor.Up })
	if k := h.rec.kinds(); len(k) != 0 {
		t.Fatalf("first sighting must be silent, got %v", k)
	}

	tg.code.Store(503)
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	inc, _ := h.st.ActiveIncident(context.Background(), "web")
	if inc == nil {
		t.Fatal("expected open incident")
	}

	tg.code.Store(200)
	waitFor(t, "recovered", func() bool { return h.state("web") == monitor.Up })
	if inc, _ := h.st.ActiveIncident(context.Background(), "web"); inc != nil {
		t.Fatal("incident should be resolved")
	}
	k := h.rec.kinds()
	if len(k) != 2 || k[0] != notify.KindDown || k[1] != notify.KindRecovered {
		t.Fatalf("want exactly [down recovered], got %v", k)
	}
}

func TestSingleBlipDoesNotAlert(t *testing.T) {
	tg := newTarget(t)
	h := start(t, filepath.Join(t.TempDir(), "v.db"), httpMonitor(tg.URL))
	waitFor(t, "up", func() bool { return h.state("web") == monitor.Up })

	tg.code.Store(500)
	before := tg.hits.Load()
	waitFor(t, "one failed probe", func() bool { return tg.hits.Load() > before })
	tg.code.Store(200)
	time.Sleep(300 * time.Millisecond) // several more intervals

	if k := h.rec.kinds(); len(k) != 0 {
		t.Fatalf("blip must not alert, got %v", k)
	}
}

func TestHangingTargetTimesOutWithoutBlockingOthers(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	tg := newTarget(t)

	slow := httpMonitor(hang.URL)
	slow.ID, slow.Name = "hang", "Hang"
	h := start(t, filepath.Join(t.TempDir(), "v.db"), slow, httpMonitor(tg.URL))

	waitFor(t, "healthy monitor up", func() bool { return h.state("web") == monitor.Up })
	waitFor(t, "hanging monitor down", func() bool { return h.state("hang") == monitor.Down })
	if h.state("web") != monitor.Up {
		t.Fatal("healthy monitor affected by hanging one")
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	tg := newTarget(t)
	db := filepath.Join(t.TempDir(), "v.db")
	tg.code.Store(503)

	h := start(t, db, httpMonitor(tg.URL))
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	h.shutdown()

	// restart while target recovers: must close the SAME incident and alert recovery
	tg.code.Store(200)
	h2 := start(t, db, httpMonitor(tg.URL))
	if h2.state("web") != monitor.Down {
		t.Fatalf("state not restored: %s", h2.state("web"))
	}
	waitFor(t, "recovered", func() bool { return h2.state("web") == monitor.Up })
	k := h2.rec.kinds()
	if len(k) != 1 || k[0] != notify.KindRecovered {
		t.Fatalf("want [recovered] after restart, got %v", k)
	}
	incs, _ := h2.st.RecentIncidents(context.Background(), 10)
	if len(incs) != 1 || incs[0].ResolvedAt == nil {
		t.Fatalf("want 1 resolved incident, got %+v", incs)
	}
}

func TestRemovedMonitorIncidentIsClosed(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	db := filepath.Join(t.TempDir(), "v.db")
	h := start(t, db, httpMonitor(tg.URL))
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	h.shutdown()

	h2 := start(t, db) // monitor removed from config
	if open, _ := h2.st.OpenIncidents(context.Background()); len(open) != 0 {
		t.Fatalf("orphan incident left open: %+v", open)
	}
}

func TestHeartbeat(t *testing.T) {
	m := config.Monitor{
		ID: "cron", Name: "Cron", Type: "push", Token: "0123456789abcdef",
		Interval: config.Duration(100 * time.Millisecond), Grace: config.Duration(50 * time.Millisecond),
		FailThreshold: 1, RecoverThreshold: 1,
	}
	h := start(t, filepath.Join(t.TempDir(), "v.db"), m)

	if !h.sched.Beat(m.Token, time.Now()) {
		t.Fatal("beat rejected")
	}
	if h.sched.Beat("wrong-token-xxxxxxxx", time.Now()) {
		t.Fatal("unknown token accepted")
	}
	waitFor(t, "up", func() bool { return h.state("cron") == monitor.Up })

	// keep beating faster than the deadline: must stay up
	for range 5 {
		time.Sleep(60 * time.Millisecond)
		h.sched.Beat(m.Token, time.Now())
	}
	if h.state("cron") != monitor.Up {
		t.Fatalf("regular beats went %s", h.state("cron"))
	}

	// go silent past interval+grace
	waitFor(t, "down", func() bool { return h.state("cron") == monitor.Down })
	h.sched.Beat(m.Token, time.Now())
	waitFor(t, "back up", func() bool { return h.state("cron") == monitor.Up })

	k := h.rec.kinds()
	if len(k) != 2 || k[0] != notify.KindDown || k[1] != notify.KindRecovered {
		t.Fatalf("want [down recovered], got %v", k)
	}
}

func maintCfg(url string, start time.Time, d time.Duration) *config.Config {
	return &config.Config{
		Monitors:    []config.Monitor{httpMonitor(url)},
		Maintenance: []config.Maintenance{{Name: "deploy", Start: start, Duration: config.Duration(d), Monitors: []string{"web"}}},
	}
}

func TestMaintenanceSilencesOutage(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	// down for the whole window, then recovers inside it: nothing to say
	h := startCfg(t, filepath.Join(t.TempDir(), "v.db"), maintCfg(tg.URL, time.Now().Add(-time.Second), time.Hour))
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	tg.code.Store(200)
	waitFor(t, "up", func() bool { return h.state("web") == monitor.Up })
	if k := h.rec.kinds(); len(k) != 0 {
		t.Fatalf("maintenance must be silent, got %v", k)
	}
	if open, _ := h.st.OpenIncidents(context.Background()); len(open) != 0 {
		t.Fatalf("no incident during maintenance, got %+v", open)
	}
	if pct, ok, _ := h.st.Uptime(context.Background(), "web", time.Now().Add(-time.Hour)); ok {
		t.Fatalf("maintenance results must not count toward uptime, got %.1f%%", pct)
	}
}

func TestStillDownAfterMaintenanceAlerts(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	h := startCfg(t, filepath.Join(t.TempDir(), "v.db"), maintCfg(tg.URL, time.Now(), 400*time.Millisecond))
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	if k := h.rec.kinds(); len(k) != 0 {
		t.Fatalf("alert during maintenance: %v", k)
	}
	waitFor(t, "alert after window", func() bool { return len(h.rec.kinds()) == 1 })
	if k := h.rec.kinds(); k[0] != notify.KindDown {
		t.Fatalf("want down, got %v", k)
	}
	if inc, _ := h.st.ActiveIncident(context.Background(), "web"); inc == nil {
		t.Fatal("incident should open once the window ends")
	}
	tg.code.Store(200)
	waitFor(t, "recovered", func() bool { return len(h.rec.kinds()) == 2 })
	if k := h.rec.kinds(); k[1] != notify.KindRecovered {
		t.Fatalf("want recovered, got %v", k)
	}
}

func TestAutoIncidentIsPublicWithComponentName(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	cfg := &config.Config{
		Monitors:   []config.Monitor{httpMonitor(tg.URL)},
		StatusPage: config.StatusPage{Components: []config.Component{{ID: "website", Name: "Website", Monitors: []string{"web"}}}},
	}
	h := startCfg(t, filepath.Join(t.TempDir(), "v.db"), cfg)
	waitFor(t, "down", func() bool { return h.state("web") == monitor.Down })
	inc, _ := h.st.ActiveIncident(context.Background(), "web")
	if inc == nil || inc.Title != "Website outage" || len(inc.Components) != 1 || inc.Components[0] != "website" {
		t.Fatalf("got %+v", inc)
	}
	tg.code.Store(200)
	waitFor(t, "up", func() bool { return h.state("web") == monitor.Up })
	full, _ := h.st.Incident(context.Background(), inc.ID)
	if full.Status != "resolved" || len(full.Updates) != 2 {
		t.Fatalf("want resolved with 2 updates, got %+v", full)
	}
}

func TestOutageAlertedBeforeMaintenanceIsNotRepeated(t *testing.T) {
	tg := newTarget(t)
	tg.code.Store(503)
	h := startCfg(t, filepath.Join(t.TempDir(), "v.db"), maintCfg(tg.URL, time.Now().Add(250*time.Millisecond), 300*time.Millisecond))
	waitFor(t, "down alert", func() bool { return len(h.rec.kinds()) == 1 })
	time.Sleep(800 * time.Millisecond) // window passes, still down
	if k := h.rec.kinds(); len(k) != 1 {
		t.Fatalf("outage re-alerted after maintenance: %v", k)
	}
}

// Pings far more frequent than the interval must not inflate the result count:
// uptime % has to reflect time, not how chatty the job is.
func TestHeartbeatResultsAreTimeProportional(t *testing.T) {
	m := config.Monitor{
		ID: "cron", Name: "Cron", Type: "push", Token: "0123456789abcdef",
		Interval: config.Duration(100 * time.Millisecond), Grace: config.Duration(50 * time.Millisecond),
		FailThreshold: 1, RecoverThreshold: 1,
	}
	h := start(t, filepath.Join(t.TempDir(), "v.db"), m)
	for range 40 { // ~400ms healthy, pinging every 10ms
		h.sched.Beat(m.Token, time.Now())
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, "down", func() bool { return h.state("cron") == monitor.Down })
	time.Sleep(250 * time.Millisecond) // ~400ms down in total
	res, _ := h.st.Results(context.Background(), "cron", time.Now().Add(-time.Hour), 1000)
	var up, down int
	for _, r := range res {
		if r.Status == check.Up {
			up++
		} else {
			down++
		}
	}
	if up > 8 || down < 2 {
		t.Fatalf("results not time-proportional: %d up, %d down (40 pings sent)", up, down)
	}
}
