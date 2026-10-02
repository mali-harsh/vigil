package config

import (
	"errors"
	"fmt"
	"time"
)

// StatusPage controls the public page. Only monitors placed in a component are
// shown — monitor names, targets and errors never leak. With no components
// configured, every monitor is listed by name (handy for a first run).
type StatusPage struct {
	Title       string      `yaml:"title"`
	Description string      `yaml:"description"`
	LogoURL     string      `yaml:"logo_url"`
	Timezone    string      `yaml:"timezone"` // day boundaries for the 90-day bars; default UTC
	ExportDir   string      `yaml:"export_dir"`
	Subscribe   bool        `yaml:"subscribe"` // email sign-up form (needs server.smtp + server.public_url)
	Components  []Component `yaml:"components"`

	Location *time.Location `yaml:"-"`
}

// Component is either a leaf (Monitors set) or a group (Components set).
type Component struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Monitors    []string    `yaml:"monitors"`
	Components  []Component `yaml:"components"`
	Collapsed   bool        `yaml:"collapsed"`
}

func (c Component) IsGroup() bool { return len(c.Components) > 0 }

// Maintenance silences alerts and excludes results from uptime while active.
type Maintenance struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Start       time.Time `yaml:"start"` // RFC 3339 with offset, e.g. 2026-10-05T22:00:00+05:30
	Duration    Duration  `yaml:"duration"`
	Repeat      string    `yaml:"repeat"` // "", daily, weekly
	Monitors    []string  `yaml:"monitors"`
	Components  []string  `yaml:"components"` // expands to every monitor inside
}

// Leaves returns all leaf components, depth-first.
func (s StatusPage) Leaves() []Component {
	var out []Component
	var walk func([]Component)
	walk = func(cs []Component) {
		for _, c := range cs {
			if c.IsGroup() {
				walk(c.Components)
			} else {
				out = append(out, c)
			}
		}
	}
	walk(s.Components)
	return out
}

// ComponentMonitors maps every component ID (groups included) to the monitor
// IDs it covers.
func (s StatusPage) ComponentMonitors() map[string][]string {
	out := map[string][]string{}
	var walk func(Component) []string
	walk = func(c Component) []string {
		ids := append([]string(nil), c.Monitors...)
		for _, ch := range c.Components {
			ids = append(ids, walk(ch)...)
		}
		out[c.ID] = ids
		return ids
	}
	for _, c := range s.Components {
		walk(c)
	}
	return out
}

func (c *Config) applyStatusPageDefaults() {
	sp := &c.StatusPage
	if sp.Title == "" {
		sp.Title = "System Status"
	}
	if sp.Timezone == "" {
		sp.Timezone = "UTC"
	}
	var fill func([]Component)
	fill = func(cs []Component) {
		for i := range cs {
			if cs[i].ID == "" {
				cs[i].ID = Slug(cs[i].Name)
			}
			fill(cs[i].Components)
		}
	}
	fill(sp.Components)
}

func (c *Config) validateStatusPage() error {
	var errs []error
	sp := &c.StatusPage
	loc, err := time.LoadLocation(sp.Timezone)
	if err != nil {
		errs = append(errs, fmt.Errorf("status_page.timezone: %v", err))
	}
	sp.Location = loc

	monitors := map[string]bool{}
	for _, m := range c.Monitors {
		monitors[m.ID] = true
	}
	seen := map[string]bool{}
	var walk func(cs []Component, depth int)
	walk = func(cs []Component, depth int) {
		for _, comp := range cs {
			p := fmt.Sprintf("component %q", comp.Name)
			switch {
			case comp.Name == "" || comp.ID == "":
				errs = append(errs, errors.New("component: name required"))
				continue
			case seen[comp.ID]:
				errs = append(errs, fmt.Errorf("%s: duplicate id %q", p, comp.ID))
			case comp.IsGroup() && len(comp.Monitors) > 0:
				errs = append(errs, fmt.Errorf("%s: a group holds components, not monitors", p))
			case comp.IsGroup() && depth > 0:
				errs = append(errs, fmt.Errorf("%s: groups cannot be nested", p))
			case !comp.IsGroup() && len(comp.Monitors) == 0:
				errs = append(errs, fmt.Errorf("%s: needs monitors or components", p))
			}
			seen[comp.ID] = true
			for _, m := range comp.Monitors {
				if !monitors[m] {
					errs = append(errs, fmt.Errorf("%s: unknown monitor %q", p, m))
				}
			}
			walk(comp.Components, depth+1)
		}
	}
	walk(sp.Components, 0)

	for _, mw := range c.Maintenance {
		p := fmt.Sprintf("maintenance %q", mw.Name)
		if mw.Name == "" {
			errs = append(errs, errors.New("maintenance: name required"))
		}
		if mw.Start.IsZero() {
			errs = append(errs, fmt.Errorf("%s: start required (RFC 3339)", p))
		}
		if mw.Duration <= 0 {
			errs = append(errs, fmt.Errorf("%s: duration required", p))
		}
		period := map[string]time.Duration{"daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}[mw.Repeat]
		if mw.Repeat != "" && period == 0 {
			errs = append(errs, fmt.Errorf("%s: repeat must be daily or weekly", p))
		}
		if period > 0 && mw.Duration.D() >= period {
			errs = append(errs, fmt.Errorf("%s: duration must be shorter than the repeat period", p))
		}
		if len(mw.Monitors)+len(mw.Components) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs monitors or components", p))
		}
		for _, m := range mw.Monitors {
			if !monitors[m] {
				errs = append(errs, fmt.Errorf("%s: unknown monitor %q", p, m))
			}
		}
		for _, cid := range mw.Components {
			if !seen[cid] {
				errs = append(errs, fmt.Errorf("%s: unknown component %q", p, cid))
			}
		}
	}
	if sp.Subscribe && (c.Server.SMTP.Host == "" || c.Server.PublicURL == "") {
		errs = append(errs, errors.New("status_page.subscribe needs server.smtp and server.public_url (confirmation links point there)"))
	}
	if (len(c.Server.TLS.Domains) > 0) != (c.Server.TLS.Email != "") {
		errs = append(errs, errors.New("server.tls: domains and email go together"))
	}
	return errors.Join(errs...)
}
