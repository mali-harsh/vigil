// Package statuspage builds the public status model and renders it as HTML,
// JSON and badges. It only ever exposes component names and public incident
// text — never monitor targets or raw probe errors.
package statuspage

import (
	"context"
	"slices"
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
	Accent      string                   `json:"-"`
	PastByDay   []DayIncidents           `json:"-"`
}

type DayIncidents struct {
	Day       string // "Oct 1"
	Incidents []Incident
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
	Day       string   `json:"day"`
	Uptime    *float64 `json:"uptime"`
	Level     string   `json:"level"`               // none | up | blip | minor | major | maint
	DownMin   int      `json:"down_minutes"`        // estimated from the share of failed checks
	Incidents []string `json:"incidents,omitempty"` // public incident titles touching this day
}

// Bar levels: 100% up; ≥99.9% a blip (shown subtly — a failed check or two is
// not an outage); ≥99% minor; below that major.
func level(pct float64) string {
	switch {
	case pct >= 100:
		return "up"
	case pct >= 99.9:
		return "blip"
	case pct >= 99:
		return "minor"
	}
	return "major"
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

	// 90 days of incidents feed the bar tooltips; the list shows the last 14.
	incs, err := b.Store.IncidentsSince(ctx, now.AddDate(0, 0, -Days))
	if err != nil {
		return nil, err
	}
	dayTitles := map[string]map[string][]string{} // component → day → incident titles
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
		end := now
		if inc.ResolvedAt != nil {
			end = *inc.ResolvedAt
		}
		// every local day the incident touched, start day through end day
		for d, last := midnight(inc.StartedAt, loc), midnight(end, loc); !d.After(last); d = d.AddDate(0, 0, 1) {
			key := d.Format(time.DateOnly)
			for _, c := range inc.Components {
				if dayTitles[c] == nil {
					dayTitles[c] = map[string][]string{}
				}
				dayTitles[c][key] = append(dayTitles[c][key], inc.Title)
			}
		}
		switch {
		case inc.ResolvedAt == nil:
			p.Active = append(p.Active, pub)
		case now.Sub(*inc.ResolvedAt) <= historyWindow:
			p.Past = append(p.Past, pub)
		}
	}
	p.PastByDay = byDay(p.Past, loc)
	p.Accent = b.Cfg.StatusPage.Accent

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
		for i := range r.Bars { // incidents on this component (or, for a group, its children)
			b := &r.Bars[i]
			b.Incidents = uniq(titlesFor(c, b.Day, dayTitles))
			// a day with a public incident is never shown as clean, even when the
			// checks were fine (manually declared incidents have no check data)
			if len(b.Incidents) > 0 && (b.Level == "up" || b.Level == "blip" || b.Level == "none") {
				b.Level = "minor"
			}
		}
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

func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

func titlesFor(c config.Component, day string, m map[string]map[string][]string) []string {
	out := append([]string(nil), m[c.ID][day]...)
	for _, ch := range c.Components {
		out = append(out, titlesFor(ch, day, m)...)
	}
	return out
}

func uniq(s []string) []string {
	var out []string
	for _, v := range s {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// byDay groups resolved incidents by the local day they started, newest first.
func byDay(incs []Incident, loc *time.Location) []DayIncidents {
	var out []DayIncidents
	for _, inc := range incs { // already newest first
		d := inc.StartedAt.In(loc).Format("Jan 2, 2006")
		if len(out) == 0 || out[len(out)-1].Day != d {
			out = append(out, DayIncidents{Day: d})
		}
		out[len(out)-1].Incidents = append(out[len(out)-1].Incidents, inc)
	}
	return out
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
		default:
			b.Level = level(*b.Uptime)
			b.DownMin = int((100 - *b.Uptime) / 100 * 1440)
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
