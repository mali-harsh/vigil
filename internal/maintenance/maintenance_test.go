package maintenance

import (
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

func sched(t *testing.T, tz string, ms ...config.Maintenance) *Schedule {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	return New(&config.Config{
		StatusPage: config.StatusPage{Location: loc, Components: []config.Component{
			{ID: "api", Name: "API", Monitors: []string{"api-1", "api-2"}},
		}},
		Maintenance: ms,
	})
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestOneOff(t *testing.T) {
	s := sched(t, "UTC", config.Maintenance{Name: "db", Start: at("2026-10-05T22:00:00Z"), Duration: config.Duration(time.Hour), Monitors: []string{"db"}})
	cases := map[string]bool{
		"2026-10-05T21:59:59Z": false,
		"2026-10-05T22:00:00Z": true, // start inclusive
		"2026-10-05T22:59:59Z": true,
		"2026-10-05T23:00:00Z": false, // end exclusive
		"2026-10-06T22:30:00Z": false, // does not repeat
	}
	for ts, want := range cases {
		if got := s.InMaintenance("db", at(ts)); got != want {
			t.Errorf("%s: got %v want %v", ts, got, want)
		}
	}
	if s.InMaintenance("other", at("2026-10-05T22:30:00Z")) {
		t.Error("unrelated monitor in maintenance")
	}
}

func TestComponentsExpandToMonitors(t *testing.T) {
	s := sched(t, "UTC", config.Maintenance{Name: "deploy", Start: at("2026-10-05T22:00:00Z"), Duration: config.Duration(time.Hour), Components: []string{"api"}})
	if !s.InMaintenance("api-1", at("2026-10-05T22:10:00Z")) || !s.InMaintenance("api-2", at("2026-10-05T22:10:00Z")) {
		t.Fatal("component monitors not covered")
	}
}

func TestWeeklyKeepsWallClockAcrossDST(t *testing.T) {
	// Sunday 02:30 London. DST ends 2026-10-25: offset +01:00 → +00:00.
	s := sched(t, "Europe/London", config.Maintenance{Name: "weekly", Start: at("2026-10-18T02:30:00+01:00"), Duration: config.Duration(30 * time.Minute), Repeat: "weekly", Monitors: []string{"x"}})
	if !s.InMaintenance("x", at("2026-10-18T02:40:00+01:00")) {
		t.Error("first occurrence")
	}
	if !s.InMaintenance("x", at("2026-10-25T02:40:00Z")) { // 02:40 GMT, after DST end
		t.Error("post-DST occurrence should still be at 02:30 local")
	}
	if s.InMaintenance("x", at("2026-10-25T01:40:00Z")) {
		t.Error("an hour early: naive 168h stepping bug")
	}
	if s.InMaintenance("x", at("2026-10-21T02:40:00Z")) {
		t.Error("midweek should be outside")
	}
}

func TestDaily(t *testing.T) {
	s := sched(t, "Asia/Kolkata", config.Maintenance{Name: "nightly", Start: at("2026-10-01T03:00:00+05:30"), Duration: config.Duration(15 * time.Minute), Repeat: "daily", Monitors: []string{"x"}})
	if !s.InMaintenance("x", at("2026-12-31T03:05:00+05:30")) {
		t.Error("90 days later still active at 03:05")
	}
	if s.InMaintenance("x", at("2026-12-31T03:15:00+05:30")) {
		t.Error("end exclusive")
	}
}

func TestUpcoming(t *testing.T) {
	s := sched(t, "UTC",
		config.Maintenance{Name: "later", Start: at("2026-10-10T00:00:00Z"), Duration: config.Duration(time.Hour), Monitors: []string{"x"}},
		config.Maintenance{Name: "daily", Start: at("2026-10-01T05:00:00Z"), Duration: config.Duration(time.Hour), Repeat: "daily", Monitors: []string{"x"}},
		config.Maintenance{Name: "past", Start: at("2026-09-01T00:00:00Z"), Duration: config.Duration(time.Hour), Monitors: []string{"x"}},
	)
	up := s.Upcoming(at("2026-10-05T06:00:00Z"), 7*24*time.Hour)
	if len(up) != 2 || up[0].Name != "daily" || !up[0].Start.Equal(at("2026-10-06T05:00:00Z")) || up[1].Name != "later" {
		t.Fatalf("got %+v", up)
	}
}
