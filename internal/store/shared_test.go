package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/store"
	"github.com/mali-harsh/vigil/internal/store/storetest"
)

// These run on SQLite, and also on Postgres when VIGIL_TEST_POSTGRES is set.

func open(t *testing.T) *store.Store {
	t.Helper()
	s := storetest.OpenAt(t, filepath.Join(t.TempDir(), "v.db"))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDailyRollup(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for i, st := range []check.Status{check.Up, check.Down, check.Degraded, check.Up} {
		if err := s.InsertResult(ctx, check.Result{MonitorID: "m", At: day.Add(time.Duration(i) * time.Minute), Status: st}, "", false); err != nil {
			t.Fatal(err)
		}
	}
	s.InsertResult(ctx, check.Result{MonitorID: "m", At: day, Status: check.Down}, "", true) // in maintenance
	got, err := s.Daily(ctx, []string{"m"}, "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	if d := got["m"]["2026-10-02"]; d != (store.DayStat{Total: 5, Down: 1, Degraded: 1, Maint: 1}) || d.Counted() != 4 {
		t.Fatalf("got %+v", d)
	}
}

func TestUpdatesAndResolution(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := s.OpenIncident(ctx, store.NewIncident{Title: "API errors", Components: []string{"api"}, Impact: "degraded", At: now, Message: "Looking"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(time.Minute), store.Identified, "Bad deploy", "API errors after deploy", "harsh"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(2*time.Minute), store.Resolved, "Rolled back", "", "harsh"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(3*time.Minute), store.Monitoring, "late", "", "harsh"); !errors.Is(err, store.ErrResolved) {
		t.Fatalf("update after resolve: %v", err)
	}
	if err := s.AddUpdate(ctx, 999, now, store.Monitoring, "x", "", "harsh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing incident: %v", err)
	}
	inc, _ := s.Incident(ctx, id)
	if inc.Title != "API errors after deploy" || inc.Status != store.Resolved || inc.ResolvedAt == nil || len(inc.Updates) != 3 || inc.Updates[0].Message != "Rolled back" || inc.Updates[0].Actor != "harsh" {
		t.Fatalf("got %+v", inc)
	}
}

func TestMonitorIncidentIsIdempotent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, _ := s.OpenIncident(ctx, store.NewIncident{MonitorID: "web", At: time.Now()})
	b, _ := s.OpenIncident(ctx, store.NewIncident{MonitorID: "web", At: time.Now()})
	if a != b {
		t.Fatalf("two open incidents for one monitor: %d %d", a, b)
	}
}

func TestUptimeAndLastUp(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, st := range []check.Status{check.Up, check.Up, check.Down, check.Up} {
		s.InsertResult(ctx, check.Result{MonitorID: "m", At: now.Add(time.Duration(i) * time.Second), Status: st}, "", false)
	}
	pct, ok, err := s.Uptime(ctx, "m", now.Add(-time.Minute))
	if err != nil || !ok || pct != 75 {
		t.Fatalf("uptime %v %v %v", pct, ok, err)
	}
	if _, ok, _ := s.Uptime(ctx, "none", now.Add(-time.Minute)); ok {
		t.Fatal("no data must not report uptime")
	}
	last, _ := s.LastUp(ctx, "m")
	if !last.Equal(now.Add(3 * time.Second)) {
		t.Fatalf("LastUp %v", last)
	}
}

func requirePG(t *testing.T) *store.Store {
	t.Helper()
	if storetest.PostgresURL() == "" {
		t.Skip("set VIGIL_TEST_POSTGRES to run lease tests")
	}
	return open(t)
}

func TestLease(t *testing.T) {
	s := requirePG(t)
	ctx := context.Background()
	ttl := 300 * time.Millisecond

	ea, ok, err := s.AcquireLease(ctx, "leader", "a", "http://x", ttl)
	if err != nil || !ok || ea != 1 {
		t.Fatalf("a acquire: %d %v %v", ea, ok, err)
	}
	if _, ok, _ := s.AcquireLease(ctx, "leader", "b", "http://x", ttl); ok {
		t.Fatal("b took an unexpired lease")
	}
	if ok, err := s.RenewLease(ctx, "leader", "a", ea, ttl); !ok || err != nil {
		t.Fatalf("a renew: %v %v", ok, err)
	}
	if ok, _ := s.RenewLease(ctx, "leader", "b", ea, ttl); ok {
		t.Fatal("non-holder renewed")
	}

	time.Sleep(ttl + 100*time.Millisecond) // a stops renewing
	eb, ok, _ := s.AcquireLease(ctx, "leader", "b", "http://x", ttl)
	if !ok || eb != ea+1 {
		t.Fatalf("b should take the expired lease with a new epoch: %d %v", eb, ok)
	}
	if ok, _ := s.RenewLease(ctx, "leader", "a", ea, ttl); ok {
		t.Fatal("old holder renewed after takeover (fencing broken)")
	}

	if err := s.ReleaseLease(ctx, "leader", "b", eb); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.AcquireLease(ctx, "leader", "a", "http://x", ttl); !ok {
		t.Fatal("released lease not immediately available")
	}
}

func TestLeaseRaceHasOneWinner(t *testing.T) {
	s := requirePG(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := s.AcquireLease(ctx, "race", string(rune('a'+i)), "http://x", time.Minute); err == nil && ok {
				mu.Lock()
				winners++
				mu.Unlock()
			} else if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("acquire: %v", err)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d winners", winners)
	}
}

func TestSubscribers(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	sub, confirmed, err := s.AddSubscriber(ctx, "email", " Ops@Example.com ", false)
	if err != nil || confirmed || sub.Address != "ops@example.com" {
		t.Fatalf("add: %+v %v %v", sub, confirmed, err)
	}
	if subs, _ := s.Subscribers(ctx, true); len(subs) != 0 {
		t.Fatal("unconfirmed subscriber would receive mail")
	}
	again, _, _ := s.AddSubscriber(ctx, "email", "ops@example.com", false)
	if again.ID != sub.ID || again.Token == sub.Token {
		t.Fatal("re-subscribing a pending address must reuse the row with a fresh token")
	}
	if _, err := s.ConfirmSubscriber(ctx, sub.Token); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old token still confirms")
	}
	if _, err := s.ConfirmSubscriber(ctx, again.Token); err != nil {
		t.Fatal(err)
	}
	if subs, _ := s.Subscribers(ctx, true); len(subs) != 1 {
		t.Fatal("confirmed subscriber missing")
	}
	if _, confirmed, _ := s.AddSubscriber(ctx, "email", "ops@example.com", false); !confirmed {
		t.Fatal("confirmed address should report confirmed (no new mail)")
	}
	if ok, _ := s.Unsubscribe(ctx, again.Token); !ok {
		t.Fatal("unsubscribe")
	}
	if subs, _ := s.Subscribers(ctx, false); len(subs) != 0 {
		t.Fatal("still subscribed")
	}
}
