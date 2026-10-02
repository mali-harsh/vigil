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
	"slices"
	"strings"
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
	KindAcknowledged Kind = "acknowledged"
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
	AckURL     string    `json:"ack_url,omitempty"`  // signed one-click acknowledge link
	AckedBy    string    `json:"acked_by,omitempty"`
	Step       int       `json:"escalation_step,omitempty"` // >0 when sent by an escalation step
}

// AckURLSet reports whether the event carries an acknowledge link.
func (e Event) AckURLSet() bool { return e.AckURL != "" }

// DedupKey identifies the alert in incident tools (PagerDuty/Opsgenie), so
// trigger → acknowledge → resolve all land on the same alert.
func (e Event) DedupKey() string {
	switch {
	case e.Kind == KindAgentOffline || e.Kind == KindAgentOnline:
		return "vigil:" + strings.ReplaceAll(e.Monitor, " ", ":")
	case e.IncidentID > 0:
		return fmt.Sprintf("vigil:%s:incident:%d", e.MonitorID, e.IncidentID)
	}
	return "vigil:" + e.MonitorID + ":degraded"
}

func (e Event) Text() string {
	t := e.text()
	if e.Step > 0 {
		t = fmt.Sprintf("[escalation step %d] %s", e.Step+1, t)
	}
	if e.AckURL != "" && (e.Kind == KindDown || e.Kind == KindReminder || e.Kind == KindDegraded) {
		t += "\nAcknowledge: " + e.AckURL
	}
	return t
}

func (e Event) text() string {
	switch e.Kind {
	case KindAcknowledged:
		return fmt.Sprintf("👀 *%s* incident acknowledged by %s", e.Monitor, e.AckedBy)
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

func NewSender(n config.Notifier, smtpCfg config.SMTP, c *http.Client) Sender {
	switch n.Type {
	case "slack":
		return &postJSON{url: n.URL, client: c, body: func(e Event) any { return map[string]string{"text": e.Text()} }}
	case "discord":
		return &postJSON{url: n.URL, client: c, body: func(e Event) any {
			return map[string]string{"content": strings.ReplaceAll(e.Text(), "*", "**")}
		}}
	case "teams": // Teams "Workflows" webhook (the old Office 365 connectors are retired)
		return &postJSON{url: n.URL, client: c, body: teamsCard}
	case "telegram":
		return &postJSON{url: "https://api.telegram.org/bot" + n.BotToken + "/sendMessage", client: c, body: func(e Event) any {
			return map[string]string{"chat_id": n.ChatID, "text": strings.ReplaceAll(e.Text(), "*", "")}
		}}
	case "pagerduty":
		return &pagerDuty{key: n.RoutingKey, client: c, url: "https://events.pagerduty.com/v2/enqueue"}
	case "opsgenie":
		base := "https://api.opsgenie.com"
		if n.Region == "eu" {
			base = "https://api.eu.opsgenie.com"
		}
		return &opsgenie{key: n.APIKey, base: base, client: c}
	case "email":
		return &email{to: n.To, m: NewMailer(smtpCfg)}
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

// Names lists configured notifiers.
func (d *Dispatcher) Names() []string {
	var out []string
	for n := range d.senders {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Test sends a test message synchronously and reports the channel's answer
// (so "does my webhook work?" gets a real yes/no).
func (d *Dispatcher) Test(ctx context.Context, name string) error {
	s, ok := d.senders[name]
	if !ok {
		return fmt.Errorf("unknown notifier %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.Send(ctx, Event{Kind: KindTest, Monitor: "vigil test", MonitorID: "test", At: time.Now().UTC(), Reason: "test notification"})
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
