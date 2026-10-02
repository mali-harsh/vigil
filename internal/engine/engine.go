// Package engine is the single writer that turns results into state,
// incidents and notifications. Everything flows through one goroutine, so
// there are no races between "is it down?" and "did we already alert?".
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/maintenance"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/store"
)

type Notifier interface {
	Notify(e notify.Event, to []string)
}

type View struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Target      string        `json:"target"`
	State       monitor.State `json:"state"`
	Since       time.Time     `json:"since"`
	Pending     int           `json:"pending"` // differing results awaiting confirmation
	Maintenance bool          `json:"maintenance"`
	Last        *LastResult   `json:"last,omitempty"`
}

type LastResult struct {
	At        time.Time    `json:"at"`
	Status    check.Status `json:"status"`
	LatencyMS int64        `json:"latency_ms"`
	Message   string       `json:"message"`
}

type entry struct {
	cfg        config.Monitor
	m          *monitor.Machine
	last       *check.Result
	inMaint    bool
	alerted    bool               // a degraded alert went out; only then is "recovered" news
	components []config.Component // leaf components showing this monitor
}

type Engine struct {
	store     *store.Store
	notifier  Notifier
	maint     *maintenance.Schedule
	log       *slog.Logger
	retention time.Duration
	now       func() time.Time

	mu      sync.RWMutex
	order   []string
	entries map[string]*entry
}

func New(ctx context.Context, cfg *config.Config, st *store.Store, n Notifier, log *slog.Logger) (*Engine, error) {
	e := &Engine{
		store: st, notifier: n, maint: maintenance.New(cfg), log: log,
		retention: cfg.Server.Retention.D(), now: time.Now, entries: map[string]*entry{},
	}
	states, err := st.LoadStates(ctx)
	if err != nil {
		return nil, err
	}
	leaves := map[string][]config.Component{}
	for _, c := range cfg.StatusPage.Leaves() {
		for _, id := range c.Monitors {
			leaves[id] = append(leaves[id], c)
		}
	}
	now := e.now().UTC()
	for _, mc := range cfg.Monitors {
		prev := states[mc.ID]
		since := prev.Since
		if since.IsZero() {
			since = now
		}
		m := monitor.NewMachine(prev.State, since, mc.FailThreshold, mc.RecoverThreshold)
		if mc.Paused {
			m.State, m.Since = monitor.Paused, now
			if err := st.SaveState(ctx, mc.ID, monitor.Paused, now); err != nil {
				return nil, err
			}
		}
		e.entries[mc.ID] = &entry{cfg: mc, m: m, components: leaves[mc.ID], inMaint: e.maint.InMaintenance(mc.ID, now)}
		e.order = append(e.order, mc.ID)
	}
	// Monitors removed from config (or paused) must not keep incidents open
	// forever. Manually declared incidents (no monitor) are left alone.
	open, err := st.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	for _, inc := range open {
		if inc.MonitorID == "" {
			continue
		}
		if en, ok := e.entries[inc.MonitorID]; !ok || en.cfg.Paused {
			if _, err := st.ResolveIncident(ctx, inc.MonitorID, now, "Monitoring for this service was stopped."); err != nil {
				return nil, err
			}
			log.Info("closed incident of removed/paused monitor", "monitor", inc.MonitorID, "incident", inc.ID)
		}
	}
	return e, nil
}

// Maintenance exposes the schedule for the status page.
func (e *Engine) Maintenance() *maintenance.Schedule { return e.maint }

// Run consumes results until ctx is done.
func (e *Engine) Run(ctx context.Context, results <-chan check.Result) {
	reminders := time.NewTicker(30 * time.Second)
	prune := time.NewTicker(time.Hour)
	defer reminders.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-results:
			// Persistence must complete even if shutdown starts mid-handle,
			// otherwise a confirmed DOWN could be lost across a restart.
			e.handle(context.WithoutCancel(ctx), r)
		case <-reminders.C:
			e.remind(ctx)
		case <-prune.C:
			if n, err := e.store.Prune(ctx, e.now().Add(-e.retention)); err != nil {
				e.log.Error("prune", "err", err)
			} else if n > 0 {
				e.log.Info("pruned results", "rows", n)
			}
		}
	}
}

// handle applies one result atomically: readers never observe a state whose
// incident/notification hasn't been recorded yet.
func (e *Engine) handle(ctx context.Context, r check.Result) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.entries[r.MonitorID]
	if !ok {
		return
	}
	maint := e.maint.InMaintenance(r.MonitorID, r.At)
	wasMaint := en.inMaint
	en.last, en.inMaint = &r, maint
	if err := e.store.InsertResult(ctx, r, maint); err != nil {
		e.log.Error("store result", "monitor", r.MonitorID, "err", err)
	}
	tr := en.m.Observe(r)
	if tr != nil {
		e.log.Info("state change", "monitor", r.MonitorID, "from", tr.From, "to", tr.To, "reason", tr.Reason, "maintenance", maint)
		if err := e.store.SaveState(ctx, r.MonitorID, tr.To, tr.At); err != nil {
			e.log.Error("store state", "monitor", r.MonitorID, "err", err)
		}
	}
	switch {
	case maint:
		// Silence: no alerts, no new incidents. A recovery still closes an
		// incident that was open before the window began.
		if tr != nil && tr.From == monitor.Down {
			e.resolve(ctx, en, *tr)
		}
	case wasMaint && tr == nil:
		// Window just ended: anything still broken that was never alerted
		// (it broke during the window) gets its alert now.
		if e.unalerted(ctx, en) {
			e.onTransition(ctx, en, monitor.Transition{From: monitor.Up, To: en.m.State, At: r.At, Reason: r.Message + " (still failing after maintenance)"})
		}
	case tr != nil:
		e.onTransition(ctx, en, *tr)
	}
}

func (e *Engine) onTransition(ctx context.Context, en *entry, tr monitor.Transition) {
	mc := en.cfg
	ev := notify.Event{
		MonitorID: mc.ID, Monitor: mc.Name, Target: Target(mc),
		From: string(tr.From), To: string(tr.To), At: tr.At, Reason: tr.Reason,
	}
	switch {
	case tr.To == monitor.Down:
		id, err := e.store.OpenIncident(ctx, store.NewIncident{
			MonitorID: mc.ID, Title: en.publicName() + " outage", Impact: "down",
			Components: en.componentIDs(), At: tr.At, Cause: tr.Reason,
			Message: fmt.Sprintf("We are investigating an outage affecting %s.", en.publicName()),
		})
		if err != nil {
			e.log.Error("open incident", "monitor", mc.ID, "err", err)
		}
		ev.Kind, ev.IncidentID = notify.KindDown, id

	case tr.From == monitor.Down:
		inc := e.resolve(ctx, en, tr)
		if inc == nil {
			return // the outage was never alerted (e.g. began in maintenance)
		}
		ev.Kind, ev.IncidentID, ev.Downtime = notify.KindRecovered, inc.ID, human(tr.At.Sub(inc.StartedAt))
		en.alerted = tr.To == monitor.Degraded

	case tr.To == monitor.Degraded:
		ev.Kind, en.alerted = notify.KindDegraded, true

	case tr.From == monitor.Degraded && tr.To == monitor.Up:
		if !en.alerted {
			return
		}
		ev.Kind, ev.Downtime, en.alerted = notify.KindRecovered, human(tr.Lasted), false

	default: // unknown → up: first sighting, nothing to say
		return
	}
	e.notifier.Notify(ev, mc.Notify)
}

func (e *Engine) unalerted(ctx context.Context, en *entry) bool {
	switch en.m.State {
	case monitor.Down:
		inc, err := e.store.ActiveIncident(ctx, en.cfg.ID)
		if err != nil {
			e.log.Error("load incident", "monitor", en.cfg.ID, "err", err)
			return true // when unsure, alert
		}
		return inc == nil
	case monitor.Degraded:
		return !en.alerted
	}
	return false
}

func (e *Engine) resolve(ctx context.Context, en *entry, tr monitor.Transition) *store.Incident {
	inc, err := e.store.ActiveIncident(ctx, en.cfg.ID)
	if err != nil || inc == nil {
		if err != nil {
			e.log.Error("load incident", "monitor", en.cfg.ID, "err", err)
		}
		return nil
	}
	msg := fmt.Sprintf("%s recovered after %s. This incident is resolved.", en.publicName(), human(tr.At.Sub(inc.StartedAt)))
	inc, err = e.store.ResolveIncident(ctx, en.cfg.ID, tr.At, msg)
	if err != nil {
		e.log.Error("resolve incident", "monitor", en.cfg.ID, "err", err)
	}
	return inc
}

// publicName is what the status page calls this monitor: its component
// names, or the monitor name when it is not on the page.
func (en *entry) publicName() string {
	if len(en.components) == 0 {
		return en.cfg.Name
	}
	names := make([]string, len(en.components))
	for i, c := range en.components {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func (en *entry) componentIDs() []string {
	ids := make([]string, len(en.components))
	for i, c := range en.components {
		ids[i] = c.ID
	}
	return ids
}

func (e *Engine) remind(ctx context.Context) {
	open, err := e.store.OpenIncidents(ctx)
	if err != nil {
		e.log.Error("load incidents", "err", err)
		return
	}
	now := e.now().UTC()
	for _, inc := range open {
		e.mu.RLock()
		en, ok := e.entries[inc.MonitorID]
		var reason string
		var silenced bool
		if ok {
			silenced = en.inMaint
			if en.last != nil {
				reason = en.last.Message
			}
		}
		e.mu.RUnlock()
		if !ok || silenced || en.cfg.ReminderEvery <= 0 || now.Sub(inc.LastNotifiedAt) < en.cfg.ReminderEvery.D() {
			continue
		}
		e.notifier.Notify(notify.Event{
			Kind: notify.KindReminder, MonitorID: inc.MonitorID, Monitor: en.cfg.Name, Target: Target(en.cfg),
			To: string(monitor.Down), At: now, Reason: reason, IncidentID: inc.ID, Downtime: human(now.Sub(inc.StartedAt)),
		}, en.cfg.Notify)
		if err := e.store.TouchIncident(ctx, inc.ID, now); err != nil {
			e.log.Error("touch incident", "err", err)
		}
	}
}

func (e *Engine) Views() []View {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]View, 0, len(e.order))
	now := e.now()
	for _, id := range e.order {
		en := e.entries[id]
		v := View{
			ID: id, Name: en.cfg.Name, Type: en.cfg.Type, Target: Target(en.cfg), State: en.m.State,
			Since: en.m.Since, Pending: en.m.Pending(), Maintenance: e.maint.InMaintenance(id, now),
		}
		if l := en.last; l != nil {
			v.Last = &LastResult{At: l.At, Status: l.Status, LatencyMS: l.Latency.Milliseconds(), Message: l.Message}
		}
		out = append(out, v)
	}
	return out
}

func (e *Engine) Has(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.entries[id]
	return ok
}

// Target is the human-readable thing a monitor watches. Push tokens are
// secrets and never shown.
func Target(m config.Monitor) string {
	switch m.Type {
	case "http":
		return m.URL
	case "push":
		return "heartbeat every " + m.Interval.D().String()
	}
	return m.Host
}

func human(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}
