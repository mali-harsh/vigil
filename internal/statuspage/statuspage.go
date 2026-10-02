// Package statuspage builds the public status model and renders it as HTML,
// JSON and badges. It only ever exposes component names and public incident
// text — never monitor targets or raw probe errors.
package statuspage

import (
	"context"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/maintenance"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/store"
)

const (
	Days            = 90
	historyWindow   = 14 * 24 * time.Hour
	upcomingHorizon = 7 * 24 * time.Hour
)

// Public component states.
const (
	Operational = "operational"
	Degraded    = "degraded"
	Outage      = "outage"
	Maint       = "maintenance"
	NoData      = "unknown"
)

func rank(s string) int {
	switch s {
	case Outage:
		return 4
	case Degraded:
		return 3
	case Maint:
		return 2
	case Operational:
		return 1
	}
	return 0
}

func worse(a, b string) string {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

type Page struct {
	Title       string                   `json:"title"`
	Description string                   `json:"description,omitempty"`
	LogoURL     string                   `json:"logo_url,omitempty"`
	Status      string                   `json:"status"`
	Components  []Row                    `json:"components"`
	Active      []Incident               `json:"active_incidents"`
	Past        []Incident               `json:"past_incidents"`
	Maintenance []maintenance.Occurrence `json:"active_maintenance"`
	Upcoming    []maintenance.Occurrence `json:"upcoming_maintenance"`
	GeneratedAt time.Time                `json:"generated_at"`
	Timezone    string                   `json:"timezone"`
}

type Row struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status"`
	Uptime      *float64 `json:"uptime_90d"`
	Bars        []Bar    `json:"days"`
	Children    []Row    `json:"components,omitempty"`
	Collapsed   bool     `json:"-"`
}

type Bar struct {
	Day    string   `json:"day"`
	Uptime *float64 `json:"uptime"`
	Level  string   `json:"level"` // none | up | minor | major | maint
}

type Incident struct {
	ID         int64          `json:"id"`
	Title      string         `json:"title"`
	Status     string         `json:"status"`
	Impact     string         `json:"impact"`
	Components []string       `json:"components"`
	StartedAt  time.Time      `json:"started_at"`
	ResolvedAt *time.Time     `json:"resolved_at,omitempty"`
	Updates    []store.Update `json:"updates"`
}

type Builder struct {
	Cfg    *config.Config
	Engine *engine.Engine
	Store  *store.Store
}

func (b *Builder) Build(ctx context.Context, now time.Time) (*Page, error) {
	sp := b.Cfg.StatusPage
	loc := sp.Location
	if loc == nil {
		loc = time.UTC
	}
	p := &Page{
		Title: sp.Title, Description: sp.Description, LogoURL: sp.LogoURL,
		GeneratedAt: now.UTC(), Timezone: loc.String(),
		Maintenance: orEmpty(b.Engine.Maintenance().Active(now)),
		Upcoming:    orEmpty(b.Engine.Maintenance().Upcoming(now, upcomingHorizon)),
		Active:      []Incident{}, Past: []Incident{},
	}

	comps := sp.Components
	fallback := len(comps) == 0
	if fallback { // no layout configured: one row per monitor
		for _, m := range b.Cfg.Monitors {
			comps = append(comps, config.Component{ID: m.ID, Name: m.Name, Monitors: []string{m.ID}})
		}
	}
	names := map[string]string{}
	var monitorIDs []string
	var collect func([]config.Component)
	collect = func(cs []config.Component) {
		for _, c := range cs {
			names[c.ID] = c.Name
			monitorIDs = append(monitorIDs, c.Monitors...)
			collect(c.Components)
		}
	}
	collect(comps)

	incs, err := b.Store.IncidentsSince(ctx, now.Add(-historyWindow))
	if err != nil {
		return nil, err
	}
	// manual open incidents raise the state of the components they name
	impact := map[string]string{}
	for _, inc := range incs {
		if fallback && inc.MonitorID != "" && len(inc.Components) == 0 {
			inc.Components = []string{inc.MonitorID}
		}
		if len(inc.Components) == 0 {
			continue // private: monitor not on the page
		}
		pub := Incident{ID: inc.ID, Title: inc.Title, Status: inc.Status, Impact: inc.Impact, StartedAt: inc.StartedAt, ResolvedAt: inc.ResolvedAt, Updates: inc.Updates}
		for _, c := range inc.Components {
			if n, ok := names[c]; ok {
				pub.Components = append(pub.Components, n)
			}
			if inc.ResolvedAt == nil && inc.MonitorID == "" {
				impact[c] = worse(impact[c], map[string]string{"down": Outage, "degraded": Degraded}[inc.Impact])
			}
		}
		if inc.ResolvedAt == nil {
			p.Active = append(p.Active, pub)
		} else {
			p.Past = append(p.Past, pub)
		}
	}

	state := map[string]string{}
	for _, v := range b.Engine.Views() {
		state[v.ID] = publicState(v)
	}
	days := make([]string, Days)
	for i := range Days {
		days[i] = now.In(loc).AddDate(0, 0, i-Days+1).Format(time.DateOnly)
	}
	daily, err := b.Store.Daily(ctx, monitorIDs, days[0])
	if err != nil {
		return nil, err
	}

	var row func(c config.Component) Row
	row = func(c config.Component) Row {
		r := Row{ID: c.ID, Name: c.Name, Description: c.Description, Collapsed: c.Collapsed, Status: NoData}
		ids := append([]string(nil), c.Monitors...) // never alias config slices
		for _, ch := range c.Components {
			cr := row(ch)
			r.Children = append(r.Children, cr)
			r.Status = worse(r.Status, cr.Status)
			ids = append(ids, leafMonitors(ch)...)
		}
		for _, id := range c.Monitors {
			r.Status = worse(r.Status, state[id])
		}
		r.Status = worse(r.Status, impact[c.ID])
		r.Bars, r.Uptime = bars(days, ids, daily)
		return r
	}
	p.Status = NoData
	for _, c := range comps {
		r := row(c)
		p.Status = worse(p.Status, r.Status)
		p.Components = append(p.Components, r)
	}
	if p.Components == nil {
		p.Components = []Row{}
	}
	for _, list := range [][]maintenance.Occurrence{p.Maintenance, p.Upcoming} {
		for i := range list {
			list[i].Components = componentNames(list[i], names, b.Cfg)
		}
	}
	return p, nil
}

// componentNames lists what a maintenance window affects in public terms:
// named components, plus components of directly listed monitors.
func componentNames(o maintenance.Occurrence, names map[string]string, cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if n, ok := names[id]; ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, id := range o.Components {
		add(id)
	}
	for _, c := range cfg.StatusPage.Leaves() {
		for _, m := range c.Monitors {
			for _, om := range o.Monitors {
				if m == om {
					add(c.ID)
				}
			}
		}
	}
	if len(cfg.StatusPage.Components) == 0 {
		for _, m := range o.Monitors {
			add(m)
		}
	}
	return orEmpty(out)
}

func leafMonitors(c config.Component) []string {
	ids := append([]string(nil), c.Monitors...)
	for _, ch := range c.Components {
		ids = append(ids, leafMonitors(ch)...)
	}
	return ids
}

func publicState(v engine.View) string {
	if v.Stale {
		return NoData // every location offline: don't claim "operational" while blind
	}
	if v.Maintenance {
		return Maint
	}
	switch v.State {
	case monitor.Up:
		return Operational
	case monitor.Degraded:
		return Degraded
	case monitor.Down:
		return Outage
	}
	return NoData
}

// bars computes one bar per day. A day's uptime is the WORST monitor's uptime
// that day (a component is only as available as its weakest part); the 90-day
// figure pools all counted results.
func bars(days, ids []string, daily map[string]map[string]store.DayStat) ([]Bar, *float64) {
	out := make([]Bar, len(days))
	var good, counted int64
	for i, day := range days {
		b := Bar{Day: day, Level: "none"}
		var maint bool
		for _, id := range ids {
			d := daily[id][day]
			maint = maint || d.Maint > 0
			if d.Counted() == 0 {
				continue
			}
			pct := float64(d.Counted()-d.Down) * 100 / float64(d.Counted())
			if b.Uptime == nil || pct < *b.Uptime {
				b.Uptime = &pct
			}
			good += d.Counted() - d.Down
			counted += d.Counted()
		}
		switch {
		case b.Uptime == nil && maint:
			b.Level = "maint"
		case b.Uptime == nil:
		case *b.Uptime >= 100:
			b.Level = "up"
		case *b.Uptime >= 99:
			b.Level = "minor"
		default:
			b.Level = "major"
		}
		out[i] = b
	}
	if counted == 0 {
		return out, nil
	}
	u := float64(good) * 100 / float64(counted)
	return out, &u
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
