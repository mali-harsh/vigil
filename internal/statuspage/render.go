package statuspage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed page.html
var pageHTML string

var (
	bannerText = map[string]string{Operational: "All systems operational", Degraded: "Degraded performance", Outage: "Service disruption", Maint: "Maintenance in progress", NoData: "Status unknown"}
	labelText  = map[string]string{Operational: "Operational", Degraded: "Degraded", Outage: "Outage", Maint: "Maintenance", NoData: "No data"}
)

var statusColor = map[string]string{Operational: "#1f9d55", Degraded: "#e8a317", Outage: "#dc3b3b", Maint: "#3b74e8", NoData: "#8a94a6"}

// icons for the hero (white on the status colour)
var statusIcon = map[string]string{
	Operational: `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>`,
	Degraded:    `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M12 7v6M12 17h.01"/></svg>`,
	Outage:      `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M7 7l10 10M17 7L7 17"/></svg>`,
	Maint:       `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L4 17l3 3 5.3-5.3a4 4 0 0 0 5.4-5.4l-2.6 2.6-2.4-.6-.6-2.4z"/></svg>`,
	NoData:      `<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M9.5 9a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .9-1 1.6V14M12 17.5h.01"/></svg>`,
}

var baseFuncs = template.FuncMap{
	"pct":    pct,
	"join":   strings.Join,
	"banner": func(s string) string { return bannerText[s] },
	"label":  func(s string) string { return labelText[s] },
	"icon":   func(s string) template.HTML { return template.HTML(statusIcon[s]) },
	// favicon: a dot in the overall status colour — the tab itself tells you
	"favicon": func(s string) template.URL {
		svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><circle cx="16" cy="16" r="13" fill="` + statusColor[s] + `"/></svg>`
		return template.URL("data:image/svg+xml," + url.PathEscape(svg))
	},
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"iso":     func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"joinsep": func(s []string) string { return strings.Join(s, "\x1f") },
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"impactText": func(s string) string {
		return map[string]string{"down": "Outage", "degraded": "Degraded", "none": "Informational"}[s]
	},
	"mins": func(m int) string {
		if m < 60 {
			return strconv.Itoa(m) + " min"
		}
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	},
	"dur": func(a time.Time, b *time.Time) string {
		if b == nil {
			return ""
		}
		d := b.Sub(a).Round(time.Minute)
		switch {
		case d < time.Minute:
			return "under a minute"
		case d < time.Hour:
			return fmt.Sprintf("%d min", int(d.Minutes()))
		case d < 48*time.Hour && int(d.Minutes())%60 == 0:
			return fmt.Sprintf("%dh", int(d.Hours()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
		}
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"dayname": func(day string) string {
		t, err := time.Parse(time.DateOnly, day)
		if err != nil {
			return day
		}
		return t.Format("Mon, Jan 2")
	},
	// per-render (timezone-dependent) — placeholders replaced in Render
	"tz":    func(t time.Time) string { return t.Format("Jan 2, 15:04 MST") },
	"month": func(t time.Time) string { return t.Format("Jan") },
	"dayn":  func(t time.Time) string { return t.Format("2") },
}

var pageTmpl = template.Must(template.New("page").Funcs(baseFuncs).Parse(pageHTML))

func pct(p *float64) string {
	if p == nil {
		return "—"
	}
	// floor, never round up: 99.996% must not display as 100.00%
	v := float64(int64(*p*100)) / 100
	return strconv.FormatFloat(v, 'f', 2, 64) + "%"
}

// Render writes the HTML page. live adds auto-refresh (the static export
// relies on its host's caching instead).
// RenderOptions controls optional page features.
type RenderOptions struct {
	Subscribe       bool
	SubscribeAction string // absolute URL for the static export (served elsewhere)
}

func Render(p *Page, live bool, opts ...RenderOptions) ([]byte, error) {
	var o RenderOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.SubscribeAction == "" {
		o.SubscribeAction = "/subscribe"
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t, err := pageTmpl.Clone()
	if err != nil {
		return nil, err
	}
	t.Funcs(template.FuncMap{
		"tz":    func(t time.Time) string { return t.In(loc).Format("Jan 2, 15:04 MST") },
		"month": func(t time.Time) string { return t.In(loc).Format("Jan") },
		"dayn":  func(t time.Time) string { return t.In(loc).Format("2") },
	})
	accent := p.Accent
	if accent == "" {
		accent = "#4f46e5"
	}
	zone, _ := time.Now().In(loc).Zone()
	if zone != loc.String() {
		zone += " (" + loc.String() + ")"
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]any{"P": p, "Live": live, "Subscribe": o.Subscribe, "SubscribeAction": o.SubscribeAction,
		"Accent": template.CSS(accent), "Zone": zone}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Badge renders a shields-style SVG for a component (or the whole page).
func Badge(label, status string) []byte {
	color := map[string]string{Operational: "#16a34a", Degraded: "#f59e0b", Outage: "#dc2626", Maint: "#3b82f6"}[status]
	if color == "" {
		color = "#64748b"
	}
	text := map[string]string{Operational: "operational", Degraded: "degraded", Outage: "outage", Maint: "maintenance"}[status]
	if text == "" {
		text = "unknown"
	}
	label = template.HTMLEscapeString(label)
	lw, rw := 7*len(label)+12, 7*len(text)+12
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`+
		`<rect width="%d" height="20" rx="3" fill="#555"/><rect x="%d" width="%d" height="20" rx="3" fill="%s"/><rect x="%d" width="4" height="20" fill="%s"/>`+
		`<g fill="#fff" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11"><text x="6" y="14">%s</text><text x="%d" y="14">%s</text></g></svg>`,
		lw+rw, label, text, lw+rw, lw, rw, color, lw, color, label, lw+6, text)
}

// Find returns the row with id (top level or child).
func (p *Page) Find(id string) *Row {
	for i := range p.Components {
		if p.Components[i].ID == id {
			return &p.Components[i]
		}
		for j := range p.Components[i].Children {
			if p.Components[i].Children[j].ID == id {
				return &p.Components[i].Children[j]
			}
		}
	}
	return nil
}

// Export writes index.html + status.json into dir every interval, atomically
// (write temp, rename), so a static host never serves a half-written file.
// Sync dir to S3 / Cloudflare Pages / any CDN and the page stays up even when
// vigil is down — its staleness banner tells visitors the data is old.
func Export(ctx context.Context, b *Builder, dir string, every time.Duration, log *slog.Logger) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Error("export dir", "err", err)
		return
	}
	write := func() {
		p, err := b.Build(ctx, time.Now())
		if err != nil {
			log.Error("export build", "err", err)
			return
		}
		html, err := Render(p, false, RenderOptions{
			Subscribe:       b.Cfg.StatusPage.Subscribe,
			SubscribeAction: strings.TrimRight(b.Cfg.Server.PublicURL, "/") + "/subscribe", // the export is hosted elsewhere
		})
		if err != nil {
			log.Error("export render", "err", err)
			return
		}
		js, _ := json.MarshalIndent(p, "", "  ")
		for name, data := range map[string][]byte{"index.html": html, "status.json": js} {
			if err := atomicWrite(filepath.Join(dir, name), data); err != nil {
				log.Error("export write", "file", name, "err", err)
			}
		}
	}
	write()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vigil-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
