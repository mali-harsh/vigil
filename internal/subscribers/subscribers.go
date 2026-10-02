// Package subscribers tells status-page subscribers about public incidents:
// email (double opt-in, one-click unsubscribe) and signed webhooks.
package subscribers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/store"
)

type Publisher struct {
	st        *store.Store
	mail      *notify.Mailer
	client    *http.Client
	publicURL string
	title     string
	names     map[string]string
	log       *slog.Logger

	q    chan int64
	mu   sync.Mutex
	sent map[int64]int64 // incident → last update ID delivered
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Publisher {
	names := map[string]string{}
	var walk func([]config.Component)
	walk = func(cs []config.Component) {
		for _, c := range cs {
			names[c.ID] = c.Name
			walk(c.Components)
		}
	}
	walk(cfg.StatusPage.Components)
	for _, m := range cfg.Monitors { // fallback layout: monitors are components
		if _, ok := names[m.ID]; !ok && len(cfg.StatusPage.Components) == 0 {
			names[m.ID] = m.Name
		}
	}
	return &Publisher{
		st: st, mail: notify.NewMailer(cfg.Server.SMTP), client: &http.Client{Timeout: 10 * time.Second},
		publicURL: strings.TrimRight(cfg.Server.PublicURL, "/"), title: cfg.StatusPage.Title, names: names, log: log,
		q: make(chan int64, 1024), sent: map[int64]int64{},
	}
}

// Publish queues "incident changed"; the newest public update is delivered.
func (p *Publisher) Publish(incidentID int64) {
	if p == nil {
		return
	}
	select {
	case p.q <- incidentID:
	default:
		p.log.Error("subscriber queue full, dropping", "incident", incidentID)
	}
}

func (p *Publisher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-p.q:
			p.deliver(ctx, id)
		}
	}
}

type webhookPayload struct {
	Type     string       `json:"type"` // incident.update
	Page     string       `json:"page"`
	Incident wireIncident `json:"incident"`
	Update   store.Update `json:"update"`
}

type wireIncident struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Impact     string     `json:"impact"`
	Components []string   `json:"components"`
	StartedAt  time.Time  `json:"started_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	URL        string     `json:"url"`
}

func (p *Publisher) deliver(ctx context.Context, id int64) {
	inc, err := p.st.Incident(ctx, id)
	if err != nil || len(inc.Components) == 0 || len(inc.Updates) == 0 {
		return // private (monitor not on the page) or gone
	}
	u := inc.Updates[0]
	p.mu.Lock()
	dup := p.sent[id] == u.ID
	p.sent[id] = u.ID
	p.mu.Unlock()
	if dup {
		return
	}
	subs, err := p.st.Subscribers(ctx, true)
	if err != nil {
		p.log.Error("load subscribers", "err", err)
		return
	}
	var comps []string
	for _, c := range inc.Components {
		if n, ok := p.names[c]; ok {
			comps = append(comps, n)
		}
	}
	wire := wireIncident{ID: inc.ID, Title: inc.Title, Status: inc.Status, Impact: inc.Impact, Components: comps, StartedAt: inc.StartedAt, ResolvedAt: inc.ResolvedAt, URL: p.publicURL + "/"}
	subject := fmt.Sprintf("[%s] %s — %s", p.title, inc.Title, strings.ToUpper(u.Status[:1])+u.Status[1:])

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		sem <- struct{}{}
		go func(s store.Subscriber) {
			defer wg.Done()
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			var err error
			switch s.Kind {
			case "email":
				unsub := p.publicURL + "/unsubscribe/" + s.Token
				body := fmt.Sprintf("%s\n\n%s: %s\n\nAffects: %s\n\nLive status: %s/\n\n—\nUnsubscribe: %s\n",
					inc.Title, strings.ToUpper(u.Status[:1])+u.Status[1:], u.Message, strings.Join(comps, ", "), p.publicURL, unsub)
				err = p.mail.Send(sctx, []string{s.Address}, subject, body, map[string]string{
					"List-Unsubscribe":      "<" + unsub + ">",
					"List-Unsubscribe-Post": "List-Unsubscribe=One-Click", // RFC 8058: one-click from the mail client
				})
			case "webhook":
				err = p.webhook(sctx, s, webhookPayload{Type: "incident.update", Page: p.publicURL, Incident: wire, Update: u})
			}
			if err != nil {
				p.log.Warn("subscriber delivery failed", "kind", s.Kind, "subscriber", s.ID, "incident", id, "err", err)
			}
		}(s)
	}
	wg.Wait()
	p.log.Info("subscribers notified", "incident", id, "update", u.ID, "subscribers", len(subs))
}

// Signature lets receivers verify a webhook: hex HMAC-SHA256 of the body,
// keyed with the subscriber's secret (returned once at creation).
func Signature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (p *Publisher) webhook(ctx context.Context, s store.Subscriber, payload webhookPayload) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Address, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vigil-status/1")
	req.Header.Set("X-Vigil-Signature", Signature(s.Token, b))
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

// SendConfirmation mails the double opt-in link.
func (p *Publisher) SendConfirmation(ctx context.Context, s *store.Subscriber) error {
	link := p.publicURL + "/subscribe/confirm/" + s.Token
	body := fmt.Sprintf("Someone (hopefully you) asked to receive status updates for %s.\n\nConfirm: %s\n\nIf this wasn't you, ignore this message — nothing will be sent.\n", p.title, link)
	return p.mail.Send(ctx, []string{s.Address}, "Confirm your subscription to "+p.title, body, nil)
}

func (p *Publisher) MailConfigured() bool { return p != nil && p.mail.Configured() }
