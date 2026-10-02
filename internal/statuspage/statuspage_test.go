package statuspage

import (
	"strings"
	"testing"
	"time"
)

func TestLevel(t *testing.T) {
	for pct, want := range map[float64]string{100: "up", 99.99: "blip", 99.9: "blip", 99.5: "minor", 99: "minor", 98.9: "major", 0: "major"} {
		if got := level(pct); got != want {
			t.Errorf("level(%v) = %s, want %s", pct, got, want)
		}
	}
}

func TestByDayAndMidnight(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	incs := []Incident{
		{Title: "c", StartedAt: at("2026-10-02T20:00:00Z")}, // Oct 3 01:30 IST
		{Title: "b", StartedAt: at("2026-10-02T10:00:00Z")}, // Oct 2 IST
		{Title: "a", StartedAt: at("2026-10-02T09:00:00Z")}, // Oct 2 IST
	}
	g := byDay(incs, ist)
	if len(g) != 2 || g[0].Day != "Oct 3, 2026" || len(g[1].Incidents) != 2 {
		t.Fatalf("%+v", g)
	}
	if m := midnight(at("2026-10-02T20:00:00Z"), ist); m.Format(time.RFC3339) != "2026-10-03T00:00:00+05:30" {
		t.Fatal(m)
	}
}

func TestRenderIsClean(t *testing.T) {
	up := 99.95
	end := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	p := &Page{
		Title: "Acme <Corp>", Status: Degraded, Timezone: "Asia/Kolkata", Accent: "#123abc", GeneratedAt: end,
		Components: []Row{{ID: "web", Name: "Web", Status: Operational, Uptime: &up, Bars: []Bar{{Day: "2026-10-01", Uptime: &up, Level: "blip", DownMin: 1, Incidents: []string{`<script>x</script>`}}}}},
		Active:     []Incident{{ID: 1, Title: "Slow", Status: "investigating", Impact: "degraded", StartedAt: end}},
	}
	p.PastByDay = byDay([]Incident{{ID: 2, Title: "Old", Status: "resolved", Impact: "down", StartedAt: end.Add(-time.Hour), ResolvedAt: &end}}, time.UTC)
	for _, live := range []bool{true, false} {
		b, err := Render(p, live, RenderOptions{Subscribe: true})
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, bad := range []string{"ZgotmplZ", "<script>x</script>", "Acme <Corp>"} {
			if strings.Contains(s, bad) {
				t.Errorf("rendered page contains %q (unsafe value or sanitizer marker)", bad)
			}
		}
		for _, want := range []string{"--accent:#123abc", "data:image/svg&#43;xml,", "Degraded performance", "Lasted 1h", `data-x="≈ 1 min down"`} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %q", want)
			}
		}
	}
}
