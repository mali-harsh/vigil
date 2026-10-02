package statuspage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
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

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"pct":    pct,
	"join":   strings.Join,
	"banner": func(s string) string { return bannerText[s] },
	"label":  func(s string) string { return labelText[s] },
	"tz":     func(t time.Time) string { return t.Format("Jan 2, 15:04 MST") }, // overridden per render
}).Parse(pageHTML))

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
	t.Funcs(template.FuncMap{"tz": func(t time.Time) string { return t.In(loc).Format("Jan 2, 15:04 MST") }})
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]any{"P": p, "Live": live, "Subscribe": o.Subscribe, "SubscribeAction": o.SubscribeAction}); err != nil {
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
