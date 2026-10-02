package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/metrics"
	"github.com/mali-harsh/vigil/internal/store/storetest"
	"github.com/mali-harsh/vigil/internal/subscribers"
)

// mailbox is a fake SMTP server that keeps every message.
type mailbox struct {
	mu   sync.Mutex
	msgs []string
	port int
}

func newMailbox(t *testing.T) *mailbox {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	mb := &mailbox{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go mb.session(c)
		}
	}()
	return mb
}

func (mb *mailbox) session(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(s string) { io.WriteString(c, s+"\r\n") }
	w("220 fake")
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(l)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			w("250 ok")
		case cmd == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				dl, _ := r.ReadString('\n')
				if dl == ".\r\n" {
					break
				}
				b.WriteString(dl)
			}
			mb.mu.Lock()
			mb.msgs = append(mb.msgs, b.String())
			mb.mu.Unlock()
			w("250 queued")
		case cmd == "QUIT":
			w("221 bye")
			return
		}
	}
}

func (mb *mailbox) all() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return append([]string(nil), mb.msgs...)
}

func (mb *mailbox) waitN(t *testing.T, n int) []string {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if m := mb.all(); len(m) >= n {
			return m
		}
	}
	t.Fatalf("want %d mails, got %d", n, len(mb.all()))
	return nil
}

func itoa[T ~int | ~int64](n T) string { return strconv.FormatInt(int64(n), 10) }

func TestSubscribersEndToEnd(t *testing.T) {
	mb := newMailbox(t)
	var hookMu sync.Mutex
	var hooks []*http.Request
	var hookBodies [][]byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hookMu.Lock()
		hooks, hookBodies = append(hooks, r), append(hookBodies, b)
		hookMu.Unlock()
	}))
	defer hook.Close()

	cfg, err := config.Parse([]byte(`
server:
  public_url: http://status.test
  admin_tokens: ["admin-token-123"]
  smtp: {host: 127.0.0.1, port: ` + itoa(mb.port) + `, from: "Acme Status <status@acme.test>", security: none}
status_page: {title: Acme Status, subscribe: true, components: [{name: Website, monitors: [web]}]}
monitors:
  - {name: web, type: http, url: "https://example.com"}
  - {name: private, type: tcp, host: "10.0.0.1:1"}
`))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := storetest.OpenAt(t, filepath.Join(t.TempDir(), "v.db"))
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng, _ := engine.New(ctx, cfg, st, nopNotifier{}, nil, nil, log)
	pub := subscribers.New(cfg, st, log)
	go pub.Run(ctx)
	srv := httptest.NewServer((&Server{Cfg: cfg, Engine: eng, Store: st, Beater: nopBeater{}, Results: make(chan check.Result, 1),
		Publisher: pub, Metrics: metrics.New(), Log: log}).Handler())
	defer srv.Close()
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// form is on the page
	if _, page := get(t, srv, "/", ""); !strings.Contains(page, `action="/subscribe"`) {
		t.Fatal("subscribe form missing")
	}
	// honeypot: silently ignored
	c.PostForm(srv.URL+"/subscribe", url.Values{"email": {"bot@spam.test"}, "website": {"http://spam"}})
	// real sign-up → confirmation mail
	if resp, _ := c.PostForm(srv.URL+"/subscribe", url.Values{"email": {"Ops@Acme.test"}}); resp.StatusCode != 303 {
		t.Fatalf("subscribe: %d", resp.StatusCode)
	}
	msgs := mb.waitN(t, 1)
	time.Sleep(100 * time.Millisecond)
	if len(mb.all()) != 1 || strings.Contains(msgs[0], "spam") {
		t.Fatalf("honeypot sent mail: %d", len(mb.all()))
	}
	link := regexp.MustCompile(`http://status\.test(/subscribe/confirm/[A-Za-z0-9_-]+)`).FindStringSubmatch(msgs[0])
	if link == nil {
		t.Fatalf("no confirm link in:\n%s", msgs[0])
	}
	// GET (mail scanner) does not confirm
	get(t, srv, link[1], "")
	if subs, _ := st.Subscribers(ctx, true); len(subs) != 0 {
		t.Fatal("GET confirmed the subscription")
	}
	c.PostForm(srv.URL+link[1], nil)
	if subs, _ := st.Subscribers(ctx, true); len(subs) != 1 {
		t.Fatal("POST did not confirm")
	}

	// webhook subscriber via API (needs subscribers:write)
	code, body := post(t, srv, "/api/v1/subscribers", "admin-token-123", `{"kind":"webhook","address":"`+hook.URL+`"}`)
	if code != 201 {
		t.Fatalf("add webhook: %d %s", code, body)
	}
	var created struct{ Secret string }
	json.Unmarshal([]byte(body), &created)

	// a public incident reaches both
	if code, b := post(t, srv, "/api/v1/incidents", "admin-token-123", `{"title":"Checkout errors","components":["website"],"impact":"down","message":"Investigating failed payments."}`); code != 201 {
		t.Fatal(code, b)
	}
	msgs = mb.waitN(t, 2)
	m := msgs[1]
	if !strings.Contains(m, "Subject: [Acme Status] Checkout errors =?") && !strings.Contains(m, "Checkout errors") {
		t.Fatalf("incident mail:\n%s", m)
	}
	if !strings.Contains(m, "List-Unsubscribe: <http://status.test/unsubscribe/") || !strings.Contains(m, "List-Unsubscribe-Post: List-Unsubscribe=One-Click") {
		t.Fatalf("missing one-click unsubscribe headers:\n%s", m)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		hookMu.Lock()
		n := len(hooks)
		hookMu.Unlock()
		if n > 0 {
			break
		}
	}
	hookMu.Lock()
	if len(hooks) != 1 || hooks[0].Header.Get("X-Vigil-Signature") != subscribers.Signature(created.Secret, hookBodies[0]) {
		t.Fatalf("webhook missing or badly signed: %d", len(hooks))
	}
	if !strings.Contains(string(hookBodies[0]), `"title":"Checkout errors"`) || !strings.Contains(string(hookBodies[0]), `"components":["Website"]`) {
		t.Fatalf("payload: %s", hookBodies[0])
	}
	hookMu.Unlock()

	// one-click unsubscribe from the mail client (cross-origin POST)
	unsub := regexp.MustCompile(`/unsubscribe/[A-Za-z0-9_-]+`).FindString(m)
	req, _ := http.NewRequest("POST", srv.URL+unsub, strings.NewReader("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://mail.google.com")
	if resp, _ := c.Do(req); resp.StatusCode != 200 {
		t.Fatalf("one-click unsubscribe: %d", resp.StatusCode)
	}
	if subs, _ := st.Subscribers(ctx, true); len(subs) != 1 || subs[0].Kind != "webhook" {
		t.Fatalf("email still subscribed: %+v", subs)
	}

	// per-IP limit is 5/hour; one real sign-up was used above (the honeypot doesn't count)
	for i := range 5 {
		resp, _ := c.PostForm(srv.URL+"/subscribe", url.Values{"email": {"x" + itoa(i) + "@acme.test"}})
		want := http.StatusSeeOther
		if i == 4 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("sign-up %d: got %d want %d", i+2, resp.StatusCode, want)
		}
	}
}
