package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

type capture struct {
	mu   sync.Mutex
	reqs []struct {
		Path string
		Auth string
		Body map[string]any
	}
}

func (c *capture) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		c.mu.Lock()
		c.reqs = append(c.reqs, struct {
			Path string
			Auth string
			Body map[string]any
		}{r.URL.RequestURI(), r.Header.Get("Authorization"), b})
		c.mu.Unlock()
		w.WriteHeader(202)
	}))
	t.Cleanup(s.Close)
	return s
}

var down = Event{Kind: KindDown, MonitorID: "api", Monitor: "API", IncidentID: 7, Reason: "503", Target: "https://api", At: time.Unix(0, 0)}

func TestPagerDutyLifecycle(t *testing.T) {
	c := &capture{}
	pd := &pagerDuty{key: "k", url: c.server(t).URL, client: http.DefaultClient}
	ctx := context.Background()
	for _, k := range []Kind{KindDown, KindReminder, KindAcknowledged, KindRecovered} {
		e := down
		e.Kind = k
		if err := pd.Send(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.reqs) != 3 { // reminder is not forwarded
		t.Fatalf("want trigger, acknowledge, resolve; got %d requests", len(c.reqs))
	}
	for i, want := range []string{"trigger", "acknowledge", "resolve"} {
		b := c.reqs[i].Body
		if b["event_action"] != want || b["dedup_key"] != "vigil:api:incident:7" {
			t.Errorf("req %d: %v", i, b)
		}
	}
	if p, _ := c.reqs[0].Body["payload"].(map[string]any); p["severity"] != "critical" || p["source"] != "vigil" {
		t.Errorf("payload: %v", p)
	}
}

func TestOpsgenieLifecycle(t *testing.T) {
	c := &capture{}
	og := &opsgenie{key: "gk", base: c.server(t).URL, client: http.DefaultClient}
	ctx := context.Background()
	for _, k := range []Kind{KindDown, KindAcknowledged, KindRecovered} {
		e := down
		e.Kind = k
		og.Send(ctx, e)
	}
	paths := []string{"/v2/alerts", "/v2/alerts/vigil:api:incident:7/acknowledge?identifierType=alias", "/v2/alerts/vigil:api:incident:7/close?identifierType=alias"}
	for i, p := range paths {
		if c.reqs[i].Path != p || c.reqs[i].Auth != "GenieKey gk" {
			t.Errorf("req %d: %s %s", i, c.reqs[i].Path, c.reqs[i].Auth)
		}
	}
	if c.reqs[0].Body["priority"] != "P1" || c.reqs[0].Body["alias"] != "vigil:api:incident:7" {
		t.Errorf("create: %v", c.reqs[0].Body)
	}
}

func TestChatPayloads(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	for _, typ := range []string{"discord", "teams", "slack"} {
		NewSender(config.Notifier{Type: typ, URL: srv.URL}, config.SMTP{}, http.DefaultClient).Send(context.Background(), down)
	}
	if !strings.Contains(c.reqs[0].Body["content"].(string), "**API**") {
		t.Errorf("discord bold: %v", c.reqs[0].Body)
	}
	if c.reqs[1].Body["type"] != "message" {
		t.Errorf("teams card: %v", c.reqs[1].Body)
	}
	if !strings.Contains(c.reqs[2].Body["text"].(string), "*API* is DOWN") {
		t.Errorf("slack: %v", c.reqs[2].Body)
	}
}

func TestAckLinkAndEscalationText(t *testing.T) {
	e := down
	e.AckURL, e.Step = "https://status/ack/abc", 1
	txt := e.Text()
	if !strings.HasPrefix(txt, "[escalation step 2]") || !strings.Contains(txt, "Acknowledge: https://status/ack/abc") {
		t.Fatal(txt)
	}
	e.Kind = KindRecovered
	if strings.Contains(e.Text(), "Acknowledge") {
		t.Fatal("no ack link on recovery")
	}
}

// fakeSMTP records one message.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := func(s string) { io.WriteString(c, s+"\r\n") }
		w("220 fake ESMTP")
		var data strings.Builder
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(l)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				w("250 ok")
			case cmd == "DATA":
				w("354 go")
				for {
					dl, _ := r.ReadString('\n')
					if dl == ".\r\n" {
						break
					}
					data.WriteString(dl)
				}
				w("250 queued")
				got <- data.String()
			case cmd == "QUIT":
				w("221 bye")
				return
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestEmail(t *testing.T) {
	port, got := fakeSMTP(t)
	m := NewMailer(config.SMTP{Host: "127.0.0.1", Port: port, From: "vigil <status@example.com>", Security: "none"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.Send(ctx, []string{"ops@example.com"}, "🔴 API is DOWN\r\nBcc: victim@evil.com", "line1\nline2", map[string]string{"List-Unsubscribe": "<https://x/u>\r\nBcc: evil@x"}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	if strings.Contains(msg, "\r\nBcc:") || !strings.Contains(msg, "List-Unsubscribe: <https://x/u>") {
		t.Fatal("header injection through subject")
	}
	if !strings.Contains(msg, "Subject: =?utf-8?B?") || !strings.Contains(msg, "line1\r\nline2") || !strings.Contains(msg, "To: ops@example.com") {
		t.Fatalf("message:\n%s", msg)
	}
	if err := m.Send(ctx, []string{"a@b.c\r\nRCPT TO:<x@y>"}, "s", "b", nil); err == nil {
		t.Fatal("CRLF in recipient accepted")
	}
	_ = strconv.Itoa
}
