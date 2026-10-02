// Package maintenance answers "is this monitor in a maintenance window at t?".
// Pure and clock-free. Recurring windows keep their wall-clock time in the
// status page timezone, so a weekly 02:00 window stays at 02:00 across DST.
package maintenance

import (
	"slices"
	"sort"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

type Occurrence struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Monitors    []string  `json:"-"`
	Components  []string  `json:"components"`
}

type window struct {
	cfg      config.Maintenance
	start    time.Time
	days     int // 0 = one-off
	monitors map[string]bool
}

type Schedule struct{ windows []window }

func New(cfg *config.Config) *Schedule {
	loc := cfg.StatusPage.Location
	if loc == nil {
		loc = time.UTC
	}
	compMonitors := cfg.StatusPage.ComponentMonitors()
	s := &Schedule{}
	for _, m := range cfg.Maintenance {
		w := window{cfg: m, start: m.Start.In(loc), monitors: map[string]bool{}}
		w.days = map[string]int{"daily": 1, "weekly": 7}[m.Repeat]
		for _, id := range m.Monitors {
			w.monitors[id] = true
		}
		for _, c := range m.Components {
			for _, id := range compMonitors[c] {
				w.monitors[id] = true
			}
		}
		s.windows = append(s.windows, w)
	}
	return s
}

// latestStart returns the most recent occurrence start <= t, or ok=false if
// the window has not begun yet.
func (w window) latestStart(t time.Time) (time.Time, bool) {
	if t.Before(w.start) {
		return time.Time{}, false
	}
	if w.days == 0 {
		return w.start, true
	}
	k := int(t.Sub(w.start).Hours()/24) / w.days
	c := w.start.AddDate(0, 0, k*w.days)
	for c.After(t) {
		k--
		c = w.start.AddDate(0, 0, k*w.days)
	}
	for n := w.start.AddDate(0, 0, (k+1)*w.days); !n.After(t); n = w.start.AddDate(0, 0, (k+1)*w.days) {
		k++
		c = n
	}
	return c, true
}

func (w window) occurrence(start time.Time) Occurrence {
	o := Occurrence{Name: w.cfg.Name, Description: w.cfg.Description, Start: start, End: start.Add(w.cfg.Duration.D()), Components: w.cfg.Components}
	for id := range w.monitors {
		o.Monitors = append(o.Monitors, id)
	}
	slices.Sort(o.Monitors)
	return o
}

func (w window) activeAt(t time.Time) (Occurrence, bool) {
	st, ok := w.latestStart(t)
	if !ok || !t.Before(st.Add(w.cfg.Duration.D())) {
		return Occurrence{}, false
	}
	return w.occurrence(st), true
}

// InMaintenance reports whether monitor id is covered by any active window.
func (s *Schedule) InMaintenance(id string, t time.Time) bool {
	for _, w := range s.windows {
		if w.monitors[id] {
			if _, ok := w.activeAt(t); ok {
				return true
			}
		}
	}
	return false
}

// Active lists windows in progress at t.
func (s *Schedule) Active(t time.Time) []Occurrence {
	var out []Occurrence
	for _, w := range s.windows {
		if o, ok := w.activeAt(t); ok {
			out = append(out, o)
		}
	}
	return out
}

// Upcoming lists occurrences starting in (t, t+horizon], soonest first.
func (s *Schedule) Upcoming(t time.Time, horizon time.Duration) []Occurrence {
	var out []Occurrence
	end := t.Add(horizon)
	for _, w := range s.windows {
		var next time.Time
		if st, ok := w.latestStart(t); !ok {
			next = w.start
		} else if w.days > 0 {
			next = st.AddDate(0, 0, w.days)
			// AddDate on the latest start, not on t, keeps the wall-clock time
			k := 1
			for !next.After(t) {
				k++
				next = st.AddDate(0, 0, k*w.days)
			}
		} else {
			continue // one-off already started
		}
		if !next.After(end) {
			out = append(out, w.occurrence(next))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}
