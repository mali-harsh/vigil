// Package check runs single probes. A probe never decides alerting — it only
// reports what it saw; the monitor state machine decides what that means.
package check

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

type Status string

const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
)

type Result struct {
	MonitorID string
	Location  string // "local" or agent name; set by whoever ran the probe
	At        time.Time
	Status    Status
	Latency   time.Duration
	Message   string
}

// Checker probes a target once. Implementations must honour ctx's deadline.
type Checker interface {
	Check(ctx context.Context) Result
}

func New(m config.Monitor) (Checker, error) {
	switch m.Type {
	case "http":
		return newHTTP(m), nil
	case "tcp":
		return &tcpCheck{m: m}, nil
	case "tls":
		return &tlsCheck{m: m}, nil
	case "dns":
		return &dnsCheck{m: m}, nil
	case "postgres":
		return &postgresCheck{m: m}, nil
	case "redis":
		return &redisCheck{m: m}, nil
	case "kafka":
		return &kafkaCheck{m: m}, nil
	case "smtp":
		return &smtpCheck{m: m}, nil
	case "grpc":
		return newGRPC(m), nil
	case "icmp":
		return &icmpCheck{m: m}, nil
	}
	return nil, fmt.Errorf("no active checker for type %q", m.Type)
}

// grade turns a successful probe into Up or Degraded based on latency.
func grade(m config.Monitor, lat time.Duration, msg string) Result {
	r := Result{MonitorID: m.ID, Status: Up, Latency: lat, Message: msg}
	if m.Expect.MaxLatency > 0 && lat > m.Expect.MaxLatency.D() {
		r.Status = Degraded
		r.Message = fmt.Sprintf("slow: %s > %s", lat.Round(time.Millisecond), m.Expect.MaxLatency.D())
	}
	return r
}

func down(m config.Monitor, lat time.Duration, format string, a ...any) Result {
	return Result{MonitorID: m.ID, Status: Down, Latency: lat, Message: fmt.Sprintf(format, a...)}
}

func certCheck(m config.Monitor, cs *tls.ConnectionState) (string, bool) {
	if cs == nil || len(cs.PeerCertificates) == 0 || m.Expect.CertMinDays == 0 {
		return "", true
	}
	left := time.Until(cs.PeerCertificates[0].NotAfter)
	days := int(left.Hours() / 24)
	if days < m.Expect.CertMinDays {
		return fmt.Sprintf("certificate expires in %d days (min %d)", days, m.Expect.CertMinDays), false
	}
	return "", true
}

// ---- http ----

type httpCheck struct {
	m      config.Monitor
	client *http.Client
}

func newHTTP(m config.Monitor) *httpCheck {
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: m.Expect.SkipTLSVerify},
		DisableKeepAlives: true, // every probe measures a fresh connection, like a real new user
	}
	return &httpCheck{m: m, client: &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}}
}

func (c *httpCheck) Check(ctx context.Context) Result {
	m := c.m
	var body io.Reader
	if m.Body != "" {
		body = strings.NewReader(m.Body)
	}
	req, err := http.NewRequestWithContext(ctx, m.Method, m.URL, body)
	if err != nil {
		return down(m, 0, "build request: %v", err)
	}
	req.Header.Set("User-Agent", "vigil-monitor/1")
	for k, v := range m.Header {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return down(m, time.Since(start), "%v", err)
	}
	defer resp.Body.Close()
	// cap the read: a monitor must never OOM on a huge response
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	lat := time.Since(start)
	if err != nil {
		return down(m, lat, "read body: %v", err)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if len(m.Expect.Status) > 0 {
		ok = slices.Contains(m.Expect.Status, resp.StatusCode)
	}
	if !ok {
		return down(m, lat, "unexpected status %d", resp.StatusCode)
	}
	if m.Expect.BodyContains != "" && !strings.Contains(string(b), m.Expect.BodyContains) {
		return down(m, lat, "body does not contain %q", m.Expect.BodyContains)
	}
	if len(m.Expect.JSON) > 0 {
		if err := jsonAssert(b, m.Expect.JSON); err != nil {
			return down(m, lat, "%v", err)
		}
	}
	if msg, ok := certCheck(m, resp.TLS); !ok {
		return down(m, lat, "%s", msg)
	}
	return grade(m, lat, fmt.Sprintf("%d", resp.StatusCode))
}

// ---- tcp ----

type tcpCheck struct{ m config.Monitor }

func (c *tcpCheck) Check(ctx context.Context) Result {
	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.m.Host)
	lat := time.Since(start)
	if err != nil {
		return down(c.m, lat, "%v", err)
	}
	conn.Close()
	return grade(c.m, lat, "connected")
}

// ---- tls ----

type tlsCheck struct{ m config.Monitor }

func (c *tlsCheck) Check(ctx context.Context) Result {
	host, _, _ := net.SplitHostPort(c.m.Host)
	start := time.Now()
	d := tls.Dialer{Config: &tls.Config{ServerName: host, InsecureSkipVerify: c.m.Expect.SkipTLSVerify}}
	conn, err := d.DialContext(ctx, "tcp", c.m.Host)
	lat := time.Since(start)
	if err != nil {
		return down(c.m, lat, "%v", err)
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
	if msg, ok := certCheck(c.m, &cs); !ok {
		return down(c.m, lat, "%s", msg)
	}
	days := int(time.Until(cs.PeerCertificates[0].NotAfter).Hours() / 24)
	return grade(c.m, lat, fmt.Sprintf("handshake ok, cert valid %dd", days))
}

// ---- dns ----

type dnsCheck struct{ m config.Monitor }

func (c *dnsCheck) Check(ctx context.Context) Result {
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, c.m.Host)
	lat := time.Since(start)
	if err != nil {
		return down(c.m, lat, "%v", err)
	}
	for _, want := range c.m.Expect.ResolvesTo {
		if !slices.Contains(addrs, want) {
			return down(c.m, lat, "resolved %v, missing %s", addrs, want)
		}
	}
	return grade(c.m, lat, strings.Join(addrs, ","))
}
