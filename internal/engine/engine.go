// Package engine is the single writer that turns results into state,
// incidents and notifications. Results from every source (local scheduler,
// remote agents) flow through one goroutine, so there are no races between
// "is it down?" and "did we already alert?".
//
// Each monitor runs in one or more locations ("local" = this server, or an
// agent). Every location has its own state machine; the monitor's state is
// derived from them by quorum. A location whose agent goes offline is frozen
// (stale), never counted as down — losing an agent must not page anyone about
// the service behind it.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/acklink"
	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/maintenance"
	"github.com/mali-harsh/vigil/internal/metrics"
	"github.com/mali-harsh/vigil/internal/monitor"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/store"
)

type Notifier interface {
	Notify(e notify.Event, to []string)
}

// Runner schedules local probes (the scheduler). seed is the last known
// heartbeat for push monitors (zero if none).
type Runner interface {
	Start(ctx context.Context, m config.Monitor, seed time.Time) error
	Stop(id string)
}

// AgentTimeout: an agent silent this long is offline.
const AgentTimeout = 90 * time.Second

type View struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	Target      string         `json:"target"`
	State       monitor.State  `json:"state"`
	Since       time.Time      `json:"since"`
	Pending     int            `json:"pending"` // differing results awaiting confirmation (max over locations)
	Maintenance bool           `json:"maintenance"`
	Stale       bool           `json:"stale"` // every location is offline: state is last known, not current
	Quorum      int            `json:"quorum"`
	Locations   []LocationView `json:"locations"`
	Last        *LastResult    `json:"last,omitempty"`
}

type LocationView struct {
	Name  string        `json:"name"`
	State monitor.State `json:"state"`
	Stale bool          `json:"stale"`
	Last  *LastResult   `json:"last,omitempty"`
}

type LastResult struct {
	At        time.Time    `json:"at"`
	Status    check.Status `json:"status"`
	LatencyMS int64        `json:"latency_ms"`
	Message   string       `json:"message"`
	Location  string       `json:"location,omitempty"`
}

type AgentView struct {
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
	Monitors int       `json:"monitors"`
}

type location struct {
	m    *monitor.Machine
	last *check.Result
}

type entry struct {
	cfg        config.Monitor
	locs       map[string]*location
	state      monitor.State // derived
	since      time.Time
	last       *check.Result // most recent from any location
	inMaint    bool
	alerted    bool // a degraded alert went out; only then is "recovered" news
	components []config.Component
}

type agentState struct {
	cfg      config.Agent
	lastSeen time.Time
	online   bool
}

type Engine struct {
	store     *store.Store
	notifier  Notifier
	runner    Runner
	maint     *maintenance.Schedule
	metrics   *metrics.Registry
	log       *slog.Logger
	retention time.Duration
	leaves    map[string][]config.Component
	policies  map[string]config.Escalation
	publicURL string
	ackSecret string
	publisher interface{ Publish(incidentID int64) } // status-page subscribers (optional)
	now       func() time.Time

	agentTimeout time.Duration // AgentTimeout; shortened in tests
	tickEvery    time.Duration // housekeeping cadence (agent liveness, pulse)
	remindEvery  time.Duration // escalation / reminder cadence

	mu      sync.RWMutex
	order   []string
	entries map[string]*entry
	agents  map[string]*agentState
	pulse   time.Time // last loop iteration, for liveness
}

func New(ctx context.Context, cfg *config.Config, st *store.Store, n Notifier, r Runner, reg *metrics.Registry, log *slog.Logger) (*Engine, error) {
	if reg == nil {
		reg = metrics.New()
	}
	e := &Engine{
		store: st, notifier: n, runner: r, maint: maintenance.New(cfg), metrics: reg, log: log,
		retention: cfg.Server.Retention.D(), now: time.Now, agentTimeout: AgentTimeout, tickEvery: 5 * time.Second, remindEvery: 5 * time.Second, // escalation steps fire within 5s of due
		entries: map[string]*entry{}, agents: map[string]*agentState{}, leaves: map[string][]config.Component{},
		policies: map[string]config.Escalation{}, publicURL: cfg.Server.PublicURL, ackSecret: cfg.Server.AckSecret,
	}
	for _, p := range cfg.Escalations {
		e.policies[p.Name] = p
	}
	for _, c := range cfg.StatusPage.Leaves() {
		for _, id := range c.Monitors {
			e.leaves[id] = append(e.leaves[id], c)
		}
	}
	now := e.now().UTC()
	e.pulse = now
	for _, a := range cfg.Agents {
		// grace period: agents get one timeout to connect after startup
		e.agents[a.Name] = &agentState{cfg: a, lastSeen: now, online: true}
	}
	states, err := st.LoadStates(ctx)
	if err != nil {
		return nil, err
	}
	for _, mc := range cfg.Monitors {
		if err := e.add(ctx, mc, states[mc.ID]); err != nil {
			return nil, err
		}
	}
	// Monitors removed from config (or paused) must not keep incidents open
	// forever. Manually declared incidents (no monitor) are left alone.
	// Discovered monitors may legitimately reappear shortly; they are only
	// closed when discovery removes them.
	open, err := st.OpenIncidents(ctx)
	if err != nil {
		return nil, err
	}
	for _, inc := range open {
		if inc.MonitorID == "" {
			continue
		}
		if en, ok := e.entries[inc.MonitorID]; !ok || en.cfg.Paused {
			if strings.Contains(inc.MonitorID, ":") { // discovered (docker:..., k8s:...)
				continue
			}
			if _, err := st.ResolveIncident(ctx, inc.MonitorID, now, "Monitoring for this service was stopped."); err != nil {
				return nil, err
			}
			log.Info("closed incident of removed/paused monitor", "monitor", inc.MonitorID, "incident", inc.ID)
		}
	}
	return e, nil
}

// add registers a monitor and starts its local probe. Callers hold e.mu
// (or run before the engine is shared).
func (e *Engine) add(ctx context.Context, mc config.Monitor, prev store.StateRow) error {
	mc.ApplyLocationDefaults()
	now := e.now().UTC()
	since := prev.Since
	if since.IsZero() {
		since = now
	}
	state := prev.State
	if state == "" || state == monitor.Paused {
		state = monitor.Unknown
	}
	en := &entry{cfg: mc, locs: map[string]*location{}, state: state, since: since, components: e.leaves[mc.ID], inMaint: e.maint.InMaintenance(mc.ID, now)}
	for _, l := range mc.Locations {
		// every location resumes from the persisted monitor state
		en.locs[l] = &location{m: monitor.NewMachine(state, since, mc.FailThreshold, mc.RecoverThreshold)}
	}
	if mc.Paused {
		en.state, en.since = monitor.Paused, now
		if err := e.store.SaveState(ctx, mc.ID, monitor.Paused, now); err != nil {
			return err
		}
	}
	e.entries[mc.ID] = en
	e.order = append(e.order, mc.ID)
	if mc.Paused || !slices.Contains(mc.Locations, config.LocalLocation) || e.runner == nil {
		return nil
	}
	var seed time.Time
	if mc.Type == "push" {
		var err error
		if seed, err = e.store.LastUp(ctx, mc.ID); err != nil {
			return err
		}
	}
	return e.runner.Start(ctx, mc, seed)
}

func (e *Engine) remove(ctx context.Context, id string) {
	if e.runner != nil {
		e.runner.Stop(id)
	}
	delete(e.entries, id)
	e.order = slices.DeleteFunc(e.order, func(x string) bool { return x == id })
	if _, err := e.store.ResolveIncident(ctx, id, e.now().UTC(), "Monitoring for this service was stopped."); err != nil {
		e.log.Error("resolve incident of removed monitor", "monitor", id, "err", err)
	}
}

// Sync makes the set of monitors from source equal to ms: new ones start,
// missing ones stop (their incidents close), changed ones restart keeping
// their state. Used by discovery providers.
func (e *Engine) Sync(ctx context.Context, source string, ms []config.Monitor) error {
	states, err := e.store.LoadStates(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	want := map[string]config.Monitor{}
	for _, m := range ms {
		m.Source = source
		m.ApplyLocationDefaults()
		want[m.ID] = m
	}
	ids := slices.Clone(e.order) // snapshot: the loop adds/removes entries
	for _, id := range ids {
		en := e.entries[id]
		if en == nil || en.cfg.Source != source {
			continue
		}
		m, keep := want[id]
		switch {
		case !keep:
			e.log.Info("monitor removed", "monitor", id, "source", source)
			e.remove(ctx, id)
		case !reflect.DeepEqual(m, en.cfg):
			e.log.Info("monitor changed", "monitor", id, "source", source)
			e.remove(ctx, id)
			if err := e.add(ctx, m, store.StateRow{State: en.state, Since: en.since}); err != nil {
				return err
			}
		}
		delete(want, id)
	}
	for id, m := range want {
		if old, clash := e.entries[id]; clash {
			e.log.Warn("discovered monitor id clashes with an existing one, ignoring", "monitor", id, "source", source, "existing_source", old.cfg.Source)
			continue
		}
		e.log.Info("monitor added", "monitor", id, "source", source)
		if err := e.add(ctx, m, states[id]); err != nil {
			return err
		}
	}
	return nil
}

// SetPublisher tells subscribers about automatic incidents. Call before Run.
func (e *Engine) SetPublisher(p interface{ Publish(int64) }) { e.publisher = p }

func (e *Engine) publish(id int64) {
	if e.publisher != nil && id > 0 {
		e.publisher.Publish(id)
	}
}

// Maintenance exposes the schedule for the status page.
func (e *Engine) Maintenance() *maintenance.Schedule { return e.maint }

// Run consumes results until ctx is done.
func (e *Engine) Run(ctx context.Context, results <-chan check.Result) {
	tick := time.NewTicker(e.tickEvery)
	reminders := time.NewTicker(e.remindEvery)
	prune := time.NewTicker(time.Hour)
	defer tick.Stop()
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
		case <-tick.C:
			e.checkAgents()
		case <-reminders.C:
			e.remind(ctx)
		case <-prune.C:
			if n, err := e.store.Prune(ctx, e.now().Add(-e.retention)); err != nil {
				e.log.Error("prune", "err", err)
			} else if n > 0 {
				e.log.Info("pruned results", "rows", n)
			}
		}
		e.mu.Lock()
		e.pulse = e.now()
		e.mu.Unlock()
	}
}

// Healthy reports whether the engine loop is alive (it ticks every 5s).
func (e *Engine) Healthy() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if age := e.now().Sub(e.pulse); age > 30*time.Second {
		return fmt.Errorf("engine loop stalled for %s", age.Round(time.Second))
	}
	return nil
}

// derive combines location states by quorum. Stale (offline-agent)
// locations keep their frozen state.
func derive(states []monitor.State, quorum int) monitor.State {
	var down, bad, up int
	for _, s := range states {
		switch s {
		case monitor.Down:
			down++
			bad++
		case monitor.Degraded:
			bad++
		case monitor.Up:
			up++
		}
	}
	switch {
	case down >= quorum:
		return monitor.Down
	case bad >= quorum:
		return monitor.Degraded
	case up > 0 || bad > 0:
		return monitor.Up // failures below quorum: regional noise, not an outage
	}
	return monitor.Unknown
}

// derived computes the monitor state from its locations. Offline (stale)
// locations are left out and the quorum shrinks to what is still online, so a
// dead agent can't blind detection (2-of-2 with one agent gone → 1-of-1).
// The one exception: if leaving stale locations out would IMPROVE the state,
// it is only accepted when the full picture (stale included) agrees — losing
// the agents that saw an outage is not a recovery.
func (e *Engine) derived(en *entry) monitor.State {
	var online, all []monitor.State
	for _, l := range en.cfg.Locations {
		st := en.locs[l].m.State
		all = append(all, st)
		if !e.locationStale(l) {
			online = append(online, st)
		}
	}
	if len(online) == 0 {
		return en.state // completely blind: hold
	}
	d := derive(online, min(en.cfg.Quorum, len(online)))
	if rank(d) < rank(en.state) && rank(derive(all, en.cfg.Quorum)) >= rank(en.state) {
		return en.state
	}
	return d
}

func rank(s monitor.State) int {
	switch s {
	case monitor.Down:
		return 3
	case monitor.Degraded:
		return 2
	case monitor.Up:
		return 1
	}
	return 0
}

// effective is the raw (unconfirmed) quorum status at this instant — what a
// single sample counts as for uptime.
func (e *Engine) effective(en *entry) check.Status {
	var ss []monitor.State
	for _, l := range en.cfg.Locations {
		if last := en.locs[l].last; last != nil && !e.locationStale(l) {
			ss = append(ss, monitor.State(last.Status))
		}
	}
	switch derive(ss, max(min(en.cfg.Quorum, len(ss)), 1)) {
	case monitor.Down:
		return check.Down
	case monitor.Degraded:
		return check.Degraded
	}
	return check.Up
}

// handle applies one result atomically: readers never observe a state whose
// incident/notification hasn't been recorded yet.
func (e *Engine) handle(ctx context.Context, r check.Result) {
	if r.Location == "" {
		r.Location = config.LocalLocation
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.entries[r.MonitorID]
	if !ok || en.state == monitor.Paused {
		return
	}
	loc, ok := en.locs[r.Location]
	if !ok {
		return // not assigned there (stale config on an agent)
	}
	e.metrics.Inc("vigil_check_results_total", metrics.L{"monitor": r.MonitorID, "location": r.Location, "status": string(r.Status)})

	maint := e.maint.InMaintenance(r.MonitorID, r.At)
	wasMaint := en.inMaint
	loc.last, en.last, en.inMaint = &r, &r, maint
	if err := e.store.InsertResult(ctx, r, e.effective(en), maint); err != nil {
		e.log.Error("store result", "monitor", r.MonitorID, "err", err)
	}
	loc.m.Observe(r)

	var tr *monitor.Transition
	if d := e.derived(en); d != en.state {
		reason := r.Message
		if len(en.cfg.Locations) > 1 {
			reason = fmt.Sprintf("[%s] %s", r.Location, r.Message)
		}
		tr = &monitor.Transition{From: en.state, To: d, At: r.At, Lasted: r.At.Sub(en.since), Reason: reason}
		en.state, en.since = d, r.At
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
			e.onTransition(ctx, en, monitor.Transition{From: monitor.Up, To: en.state, At: r.At, Reason: r.Message + " (still failing after maintenance)"})
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
		ev.Kind, ev.IncidentID, ev.AckURL = notify.KindDown, id, acklink.URL(e.publicURL, e.ackSecret, id)
		e.publish(id)

	case tr.From == monitor.Down:
		inc := e.resolve(ctx, en, tr)
		if inc == nil {
			return // the outage was never alerted (e.g. began in maintenance)
		}
		ev.Kind, ev.IncidentID, ev.Downtime = notify.KindRecovered, inc.ID, human(tr.At.Sub(inc.StartedAt))
		en.alerted = tr.To == monitor.Degraded
		// everyone the outage reached (incl. escalation steps) hears it's over
		e.notifier.Notify(ev, e.reached(en, inc.EscStep))
		return

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
	switch en.state {
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
	if inc != nil {
		e.publish(inc.ID)
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

// ---- agents ----

var ErrUnknownAgent = errors.New("unknown agent")

// AgentSeen records contact from an agent; it comes back online if it was off.
func (e *Engine) AgentSeen(name string) error {
	e.mu.Lock()
	a, ok := e.agents[name]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownAgent
	}
	a.lastSeen = e.now()
	back := !a.online
	a.online = true
	e.mu.Unlock()
	if back {
		e.log.Info("agent back online", "agent", name)
		e.notifier.Notify(notify.Event{Kind: notify.KindAgentOnline, Monitor: "agent " + name, At: e.now().UTC(), To: "online"}, a.cfg.Notify)
	}
	return nil
}

func (e *Engine) checkAgents() {
	now := e.now()
	var gone []*agentState
	e.mu.Lock()
	for _, a := range e.agents {
		if a.online && now.Sub(a.lastSeen) > e.agentTimeout {
			a.online = false
			gone = append(gone, a)
		}
	}
	e.mu.Unlock()
	for _, a := range gone {
		e.log.Warn("agent offline", "agent", a.cfg.Name, "last_seen", a.lastSeen)
		e.notifier.Notify(notify.Event{
			Kind: notify.KindAgentOffline, Monitor: "agent " + a.cfg.Name, At: now.UTC(), To: "offline",
			Reason: "no contact for " + now.Sub(a.lastSeen).Round(time.Second).String() + "; its monitors are frozen (not counted as down)",
		}, a.cfg.Notify)
	}
}

// Assignments returns the monitors an agent must run.
func (e *Engine) Assignments(agent string) []config.Monitor {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []config.Monitor
	for _, id := range e.order {
		en := e.entries[id]
		if !en.cfg.Paused && slices.Contains(en.cfg.Locations, agent) {
			out = append(out, en.cfg)
		}
	}
	return out
}

// AssignedTo reports whether monitor id runs on agent.
func (e *Engine) AssignedTo(id, agent string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	en, ok := e.entries[id]
	return ok && slices.Contains(en.cfg.Locations, agent)
}

func (e *Engine) Agents() []AgentView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []AgentView
	for _, a := range e.agents {
		v := AgentView{Name: a.cfg.Name, Online: a.online, LastSeen: a.lastSeen}
		for _, en := range e.entries {
			if slices.Contains(en.cfg.Locations, a.cfg.Name) {
				v.Monitors++
			}
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b AgentView) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (e *Engine) locationStale(l string) bool {
	if l == config.LocalLocation {
		return false
	}
	a, ok := e.agents[l]
	return !ok || !a.online
}

// ---- reminders & views ----

// reached is everyone notified up to escalation step: Notify for monitors
// without a policy, else the union of steps 0..step.
func (e *Engine) reached(en *entry, step int) []string {
	pol, ok := e.policies[en.cfg.Escalation]
	if !ok {
		return en.cfg.Notify
	}
	var out []string
	for i := 0; i <= step && i < len(pol.Steps); i++ {
		for _, n := range pol.Steps[i].Notify {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// Ack acknowledges an open incident: escalation and reminders stop, and
// everyone already paged is told who took it.
func (e *Engine) Ack(ctx context.Context, id int64, by string) error {
	inc, err := e.store.Incident(ctx, id)
	if err != nil {
		return err
	}
	acked, err := e.store.AckIncident(ctx, id, e.now().UTC(), by)
	if err != nil || !acked {
		return err
	}
	e.log.Info("incident acknowledged", "incident", id, "by", by)
	e.mu.RLock()
	en, ok := e.entries[inc.MonitorID]
	e.mu.RUnlock()
	if ok {
		e.notifier.Notify(notify.Event{
			Kind: notify.KindAcknowledged, MonitorID: inc.MonitorID, Monitor: en.cfg.Name, Target: Target(en.cfg),
			At: e.now().UTC(), IncidentID: id, AckedBy: by,
		}, e.reached(en, inc.EscStep))
	}
	return nil
}

func (e *Engine) remind(ctx context.Context) {
	open, err := e.store.OpenIncidents(ctx)
	if err != nil {
		e.log.Error("load incidents", "err", err)
		return
	}
	now := e.now().UTC()
	for _, inc := range open {
		if inc.AckedAt != nil {
			continue // someone owns it: no escalation, no reminders
		}
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
		if !ok || silenced {
			continue
		}
		ackURL := acklink.URL(e.publicURL, e.ackSecret, inc.ID)
		if pol, ok := e.policies[en.cfg.Escalation]; ok {
			for i := inc.EscStep + 1; i < len(pol.Steps) && now.Sub(inc.StartedAt) >= pol.Steps[i].After.D(); i++ {
				e.log.Warn("escalating", "incident", inc.ID, "monitor", inc.MonitorID, "step", i+1, "notify", pol.Steps[i].Notify)
				e.notifier.Notify(notify.Event{
					Kind: notify.KindDown, MonitorID: inc.MonitorID, Monitor: en.cfg.Name, Target: Target(en.cfg),
					From: string(monitor.Up), To: string(monitor.Down), At: now, Reason: reason + " — unacknowledged for " + human(now.Sub(inc.StartedAt)),
					IncidentID: inc.ID, AckURL: ackURL, Step: i,
				}, pol.Steps[i].Notify)
				if err := e.store.SetEscalationStep(ctx, inc.ID, i); err != nil {
					e.log.Error("save escalation step", "err", err)
				}
				inc.EscStep = i
			}
		}
		if en.cfg.ReminderEvery <= 0 || now.Sub(inc.LastNotifiedAt) < en.cfg.ReminderEvery.D() {
			continue
		}
		e.notifier.Notify(notify.Event{
			Kind: notify.KindReminder, MonitorID: inc.MonitorID, Monitor: en.cfg.Name, Target: Target(en.cfg),
			To: string(monitor.Down), At: now, Reason: reason, IncidentID: inc.ID, Downtime: human(now.Sub(inc.StartedAt)), AckURL: ackURL,
		}, e.reached(en, inc.EscStep))
		if err := e.store.TouchIncident(ctx, inc.ID, now); err != nil {
			e.log.Error("touch incident", "err", err)
		}
	}
}

func lastView(r *check.Result) *LastResult {
	if r == nil {
		return nil
	}
	return &LastResult{At: r.At, Status: r.Status, LatencyMS: r.Latency.Milliseconds(), Message: r.Message, Location: r.Location}
}

func (e *Engine) Views() []View {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]View, 0, len(e.order))
	now := e.now()
	for _, id := range e.order {
		en := e.entries[id]
		v := View{
			ID: id, Name: en.cfg.Name, Type: en.cfg.Type, Source: en.cfg.Source, Target: Target(en.cfg),
			State: en.state, Since: en.since, Maintenance: e.maint.InMaintenance(id, now),
			Quorum: en.cfg.Quorum, Last: lastView(en.last), Stale: true,
		}
		for _, l := range en.cfg.Locations {
			loc := en.locs[l]
			stale := e.locationStale(l)
			v.Stale = v.Stale && stale
			v.Pending = max(v.Pending, loc.m.Pending())
			v.Locations = append(v.Locations, LocationView{Name: l, State: loc.m.State, Stale: stale, Last: lastView(loc.last)})
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
	case "postgres":
		if u, err := url.Parse(m.URL); err == nil {
			return u.Redacted() // never show the password
		}
		return "postgres"
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
