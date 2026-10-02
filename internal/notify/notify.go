// Package notify delivers alert events. Delivery is asynchronous and retried so
// a slow or failing channel never stalls monitoring.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/metrics"
)

type Kind string

const (
	KindDown      Kind = "down"
	KindDegraded  Kind = "degraded"
	KindRecovered Kind = "recovered"
	KindReminder  Kind = "reminder"
	// agent connectivity — about the monitoring itself, not a service
	KindAgentOffline Kind = "agent_offline"
	KindAgentOnline  Kind = "agent_online"
	KindTest         Kind = "test"
)

type Event struct {
	Kind       Kind      `json:"kind"`
	MonitorID  string    `json:"monitor_id"`
	Monitor    string    `json:"monitor"`
	Target     string    `json:"target"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	At         time.Time `json:"at"`
	Reason     string    `json:"reason"`
	IncidentID int64     `json:"incident_id,omitempty"`
	Downtime   string    `json:"downtime,omitempty"` // set on recovered / reminder
}

func (e Event) Text() string {
	switch e.Kind {
	case KindDown:
		return fmt.Sprintf("🔴 *%s* is DOWN — %s\n%s", e.Monitor, e.Reason, e.Target)
	case KindDegraded:
		return fmt.Sprintf("🟡 *%s* is DEGRADED — %s\n%s", e.Monitor, e.Reason, e.Target)
	case KindRecovered:
		return fmt.Sprintf("🟢 *%s* RECOVERED (%s → %s) after %s\n%s", e.Monitor, e.From, e.To, e.Downtime, e.Target)
	case KindAgentOffline:
		return fmt.Sprintf("⚠️ vigil *%s* is OFFLINE — %s", e.Monitor, e.Reason)
	case KindAgentOnline:
		return fmt.Sprintf("✅ vigil *%s* is back online", e.Monitor)
	case KindTest:
		return "🔔 vigil test notification — this channel works."
	case KindReminder:
		return fmt.Sprintf("🔴 *%s* still %s for %s — %s\n%s", e.Monitor, e.To, e.Downtime, e.Reason, e.Target)
	}
	return fmt.Sprintf("%s: %s", e.Monitor, e.Kind)
}

type Sender interface {
	Send(ctx context.Context, e Event) error
}

func NewSender(n config.Notifier, c *http.Client) Sender {
	switch n.Type {
	case "slack":
		return &postJSON{url: n.URL, client: c, body: func(e Event) any { return map[string]string{"text": e.Text()} }}
	default:
		return &postJSON{url: n.URL, client: c, body: func(e Event) any { return e }}
	}
}

// postJSON covers Slack incoming webhooks, Slack Workflow webhooks (variable
// named "text") and generic webhooks.
type postJSON struct {
	url    string
	client *http.Client
	body   func(Event) any
}

func (p *postJSON) Send(ctx context.Context, e Event) error {
	b, _ := json.Marshal(p.body(e))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

type job struct {
	name   string
	sender Sender
	event  Event
}

// Dispatcher fans events out to named senders with retries.
type Dispatcher struct {
	senders map[string]Sender
	queue   chan job
	backoff []time.Duration
	log     *slog.Logger
	Metrics *metrics.Registry
	wg      sync.WaitGroup
}

func NewDispatcher(senders map[string]Sender, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		senders: senders,
		queue:   make(chan job, 4096),
		backoff: []time.Duration{0, 2 * time.Second, 10 * time.Second, 30 * time.Second},
		log:     log,
	}
}

// Run starts workers and returns once Close has been called and the queue is
// drained, or ctx is cancelled (hard stop).
func (d *Dispatcher) Run(ctx context.Context, workers int) {
	for range workers {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for j := range d.queue {
				d.deliver(ctx, j)
			}
		}()
	}
	d.wg.Wait()
}

// Close stops accepting events; queued ones are still delivered.
func (d *Dispatcher) Close() { close(d.queue) }

func (d *Dispatcher) Notify(e Event, to []string) {
	for _, name := range to {
		s, ok := d.senders[name]
		if !ok {
			continue
		}
		select {
		case d.queue <- job{name, s, e}:
		default:
			d.log.Error("notification queue full, dropping", "notifier", name, "monitor", e.MonitorID, "kind", e.Kind)
			d.Metrics.Inc("vigil_notifications_total", metrics.L{"notifier": name, "result": "dropped"})
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, j job) {
	var err error
	for i, wait := range d.backoff {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = j.sender.Send(sctx, j.event)
		cancel()
		if err == nil {
			d.log.Info("notified", "notifier", j.name, "monitor", j.event.MonitorID, "kind", j.event.Kind, "attempt", i+1)
			d.Metrics.Inc("vigil_notifications_total", metrics.L{"notifier": j.name, "result": "sent"})
			return
		}
		d.log.Warn("notify failed", "notifier", j.name, "monitor", j.event.MonitorID, "attempt", i+1, "err", err)
	}
	d.log.Error("notification gave up", "notifier", j.name, "monitor", j.event.MonitorID, "kind", j.event.Kind, "err", err)
	d.Metrics.Inc("vigil_notifications_total", metrics.L{"notifier": j.name, "result": "failed"})
}
