package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

func teamsCard(e Event) any {
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard", "version": "1.4",
				"body": []any{map[string]any{"type": "TextBlock", "text": e.Text(), "wrap": true}},
			},
		}},
	}
}

func doJSON(ctx context.Context, c *http.Client, method, url string, hdr map[string]string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ---- PagerDuty Events API v2 ----

type pagerDuty struct {
	key, url string
	client   *http.Client
}

func (p *pagerDuty) Send(ctx context.Context, e Event) error {
	action, severity := "", "critical"
	switch e.Kind {
	case KindDown, KindTest:
		action = "trigger"
	case KindDegraded, KindAgentOffline:
		action, severity = "trigger", "warning"
	case KindRecovered, KindAgentOnline:
		action = "resolve"
	case KindAcknowledged:
		action = "acknowledge"
	default:
		return nil // reminders: PagerDuty does its own re-notification
	}
	body := map[string]any{"routing_key": p.key, "event_action": action, "dedup_key": e.DedupKey()}
	if e.Kind == KindTest {
		body["dedup_key"] = "vigil:test"
	}
	if action == "trigger" {
		body["payload"] = map[string]any{
			"summary": firstLine(e.Text()), "source": "vigil", "severity": severity,
			"timestamp": e.At.UTC().Format(time.RFC3339), "component": e.Monitor,
			"custom_details": map[string]any{"target": e.Target, "reason": e.Reason, "incident_id": e.IncidentID},
		}
	}
	return doJSON(ctx, p.client, http.MethodPost, p.url, nil, body)
}

// ---- Opsgenie ----

type opsgenie struct {
	key, base string
	client    *http.Client
}

func (o *opsgenie) Send(ctx context.Context, e Event) error {
	hdr := map[string]string{"Authorization": "GenieKey " + o.key}
	alias := e.DedupKey()
	switch e.Kind {
	case KindDown, KindDegraded, KindAgentOffline, KindTest:
		prio := "P1"
		if e.Kind != KindDown {
			prio = "P3"
		}
		return doJSON(ctx, o.client, http.MethodPost, o.base+"/v2/alerts", hdr, map[string]any{
			"message": truncate(firstLine(e.Text()), 130), "alias": alias, "description": e.Text(),
			"priority": prio, "source": "vigil", "entity": e.Monitor,
		})
	case KindRecovered, KindAgentOnline:
		return doJSON(ctx, o.client, http.MethodPost, o.base+"/v2/alerts/"+alias+"/close?identifierType=alias", hdr, map[string]any{"source": "vigil"})
	case KindAcknowledged:
		return doJSON(ctx, o.client, http.MethodPost, o.base+"/v2/alerts/"+alias+"/acknowledge?identifierType=alias", hdr, map[string]any{"source": "vigil", "user": e.AckedBy})
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(s, "*", "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---- email ----

type email struct {
	to []string
	m  *Mailer
}

func (e *email) Send(ctx context.Context, ev Event) error {
	return e.m.Send(ctx, e.to, firstLine(ev.Text()), strings.ReplaceAll(ev.Text(), "*", ""), nil)
}

// Mailer sends plain-text mail through server.smtp (used by email
// notifiers and status-page subscribers).
type Mailer struct{ cfg config.SMTP }

func NewMailer(cfg config.SMTP) *Mailer { return &Mailer{cfg: cfg} }

func (m *Mailer) Configured() bool { return m != nil && m.cfg.Host != "" }

// Send mails a plain-text message. extra adds headers (e.g. List-Unsubscribe);
// values must not contain CR/LF.
func (m *Mailer) Send(ctx context.Context, to []string, subject, body string, extra map[string]string) error {
	if !m.Configured() {
		return fmt.Errorf("server.smtp not configured")
	}
	from, err := mail.ParseAddress(m.cfg.From)
	if err != nil {
		return fmt.Errorf("server.smtp.from: %v", err)
	}
	for _, t := range to {
		if strings.ContainsAny(t, "\r\n") {
			return fmt.Errorf("invalid recipient")
		}
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	var d net.Dialer
	var conn net.Conn
	if m.cfg.Security == "tls" {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: m.cfg.Host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if m.cfg.Security == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if m.cfg.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted connection
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	clean := strings.NewReplacer("\r", " ", "\n", " ") // no header injection
	var hdr strings.Builder
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		fmt.Fprintf(&hdr, "%s: %s\r\n", clean.Replace(k), clean.Replace(extra[k]))
	}
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nAuto-Submitted: auto-generated\r\n%s\r\n%s\r\n",
		from.String(), strings.Join(to, ", "), mimeHeader(clean.Replace(subject)), time.Now().Format(time.RFC1123Z), hdr.String(), strings.ReplaceAll(body, "\n", "\r\n"))
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// mimeHeader encodes non-ASCII subjects (emoji) per RFC 2047.
func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?utf-8?B?" + b64(s) + "?="
		}
	}
	return s
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
