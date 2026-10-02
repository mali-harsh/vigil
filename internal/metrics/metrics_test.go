package metrics

import (
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	r := New()
	r.Inc("vigil_check_results_total", L{"monitor": "web", "status": "up"})
	r.Inc("vigil_check_results_total", L{"monitor": "web", "status": "up"})
	r.Inc("vigil_check_results_total", L{"monitor": `we"b`, "status": "down"})
	var b strings.Builder
	r.Write(&b, []Sample{{Name: "vigil_monitor_up", Help: "1 if up.", Labels: L{"monitor": "web"}, Value: 1}})
	out := b.String()
	for _, want := range []string{
		"# TYPE vigil_monitor_up gauge\nvigil_monitor_up{monitor=\"web\"} 1\n",
		"# TYPE vigil_check_results_total counter\n",
		`vigil_check_results_total{monitor="web",status="up"} 2`,
		`vigil_check_results_total{monitor="we\"b",status="down"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
