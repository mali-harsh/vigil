// Package metrics is a minimal Prometheus text-format registry (no client
// library dependency). Counters are accumulated here; gauges are produced at
// scrape time by a callback so they are always current.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type L map[string]string

type Registry struct {
	mu       sync.Mutex
	counters map[string]map[string]float64 // name → rendered labels → value
	help     map[string]string
}

func New() *Registry {
	return &Registry{counters: map[string]map[string]float64{}, help: map[string]string{
		"vigil_check_results_total":   "Probe results received, by monitor, location and status.",
		"vigil_notifications_total":   "Notification deliveries, by notifier and result (sent, failed, dropped).",
		"vigil_heartbeat_pings_total": "Outbound dead-man's-switch pings, by result.",
	}}
}

func (r *Registry) Inc(name string, l L) { r.Add(name, l, 1) }

func (r *Registry) Add(name string, l L, v float64) {
	if r == nil {
		return
	}
	k := labels(l)
	r.mu.Lock()
	if r.counters[name] == nil {
		r.counters[name] = map[string]float64{}
	}
	r.counters[name][k] += v
	r.mu.Unlock()
}

// Sample is one gauge value.
type Sample struct {
	Name, Help string
	Labels     L
	Value      float64
}

// Write renders counters plus gauges in Prometheus text format.
func (r *Registry) Write(w io.Writer, gauges []Sample) {
	byName := map[string][]Sample{}
	for _, g := range gauges {
		byName[g.Name] = append(byName[g.Name], g)
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		ss := byName[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, ss[0].Help, name)
		for _, s := range ss {
			fmt.Fprintf(w, "%s%s %s\n", name, labels(s.Labels), num(s.Value))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(r.counters)) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, r.help[name], name)
		vals := r.counters[name]
		for _, k := range slices.Sorted(maps.Keys(vals)) {
			fmt.Fprintf(w, "%s%s %s\n", name, k, num(vals[k]))
		}
	}
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labels(l L) string {
	if len(l) == 0 {
		return ""
	}
	parts := make([]string, 0, len(l))
	for _, k := range slices.Sorted(maps.Keys(l)) {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escaper.Replace(l[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
