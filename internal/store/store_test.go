package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "v.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigratesPhase0Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil { // a phase 0 DB: tables, user_version 0
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO incidents(monitor_id, started_at, cause, last_notified_at) VALUES('web', 1, 'boom', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inc, err := s.ActiveIncident(context.Background(), "web")
	if err != nil || inc == nil || inc.Status != Investigating || inc.Impact != "down" {
		t.Fatalf("old incident not carried over: %+v %v", inc, err)
	}
	s.Close()
	if s2, err := Open(path, nil); err != nil { // reopening is a no-op
		t.Fatal(err)
	} else {
		s2.Close()
	}
}

func TestDailyRollup(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for i, st := range []check.Status{check.Up, check.Down, check.Degraded, check.Up} {
		if err := s.InsertResult(ctx, check.Result{MonitorID: "m", At: day.Add(time.Duration(i) * time.Minute), Status: st}, false); err != nil {
			t.Fatal(err)
		}
	}
	s.InsertResult(ctx, check.Result{MonitorID: "m", At: day, Status: check.Down}, true) // in maintenance
	got, err := s.Daily(ctx, []string{"m"}, "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	if d := got["m"]["2026-10-02"]; d != (DayStat{Total: 5, Down: 1, Degraded: 1, Maint: 1}) || d.Counted() != 4 {
		t.Fatalf("got %+v", d)
	}
}

func TestUpdatesAndResolution(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := s.OpenIncident(ctx, NewIncident{Title: "API errors", Components: []string{"api"}, Impact: "degraded", At: now, Message: "Looking"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(time.Minute), Identified, "Bad deploy", "API errors after deploy"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(2*time.Minute), Resolved, "Rolled back", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUpdate(ctx, id, now.Add(3*time.Minute), Monitoring, "late", ""); !errors.Is(err, ErrResolved) {
		t.Fatalf("update after resolve: %v", err)
	}
	if err := s.AddUpdate(ctx, 999, now, Monitoring, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing incident: %v", err)
	}
	inc, _ := s.Incident(ctx, id)
	if inc.Title != "API errors after deploy" || inc.Status != Resolved || inc.ResolvedAt == nil || len(inc.Updates) != 3 || inc.Updates[0].Message != "Rolled back" {
		t.Fatalf("got %+v", inc)
	}
}

func TestMonitorIncidentIsIdempotent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a, _ := s.OpenIncident(ctx, NewIncident{MonitorID: "web", At: time.Now()})
	b, _ := s.OpenIncident(ctx, NewIncident{MonitorID: "web", At: time.Now()})
	if a != b {
		t.Fatalf("two open incidents for one monitor: %d %d", a, b)
	}
}
