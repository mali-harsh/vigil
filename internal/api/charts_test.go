package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/store"
)

func TestNiceCeil(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, 3: 5, 12: 20, 99: 100, 101: 200, 4999: 5000} {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%v)=%v want %v", in, got, want)
		}
	}
}

func TestCharts(t *testing.T) {
	to := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	from := to.Add(-time.Hour)
	var pts []store.Point
	for i := range 60 {
		pts = append(pts, store.Point{At: from.Add(time.Duration(i) * time.Minute), LatencyMS: int64(10 + i), Down: i == 30})
	}
	sp := sparkline(pts, from, to, 120, 28, 30)
	if sp.Empty || !strings.HasPrefix(sp.Path, "M") || len(sp.Downs) != 1 || !strings.HasSuffix(sp.Area, "Z") {
		t.Fatalf("spark: %+v", sp)
	}
	if e := sparkline(nil, from, to, 120, 28, 30); !e.Empty {
		t.Fatal("empty spark")
	}
	c, st := latencyDetail(pts, to.Add(-24*time.Hour), to, time.UTC)
	if c.Empty || st.Checks != 60 || st.Fails != 1 || st.P50 < 10 || st.P95 < st.P50 || len(c.Fails) != 1 || len(c.YTicks) != 3 || len(c.XTicks) == 0 {
		t.Fatalf("detail: %+v %+v", st, c.YTicks)
	}
	var bs []bucket
	if err := json.Unmarshal([]byte(c.Data), &bs); err != nil || len(bs) < 12 {
		t.Fatalf("data: %v %d", err, len(bs))
	}
}
