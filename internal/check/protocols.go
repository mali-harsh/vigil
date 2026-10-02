package check

// Protocol-level checks: each speaks just enough of the protocol to prove the
// service is actually answering, not merely accepting TCP connections.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mali-harsh/vigil/internal/config"
	"golang.org/x/net/http2"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// dial connects with ctx's deadline applied to the whole conversation.
func dial(ctx context.Context, m config.Monitor) (net.Conn, error) {
	var d net.Dialer
	var conn net.Conn
	var err error
	if m.TLS {
		host, _, _ := net.SplitHostPort(m.Host)
		td := tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: host, InsecureSkipVerify: m.Expect.SkipTLSVerify}}
		conn, err = td.DialContext(ctx, "tcp", m.Host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", m.Host)
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	return conn, nil
}

// ---- postgres ----

type postgresCheck struct{ m config.Monitor }

func (c *postgresCheck) Check(ctx context.Context) Result {
	start := time.Now()
	conn, err := pgx.Connect(ctx, c.m.URL)
	if err != nil {
		return down(c.m, time.Since(start), "%s", redactErr(err, c.m.URL))
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return down(c.m, time.Since(start), "query: %v", err)
	}
	return grade(c.m, time.Since(start), "SELECT 1 ok")
}

// redactErr strips a password that a driver might echo back in an error.
func redactErr(err error, rawURL string) string {
	msg := err.Error()
	if i := strings.Index(rawURL, "://"); i >= 0 {
		if at := strings.LastIndex(rawURL, "@"); at > i {
			if colon := strings.Index(rawURL[i+3:at], ":"); colon >= 0 {
				if pw := rawURL[i+3+colon+1 : at]; pw != "" {
					msg = strings.ReplaceAll(msg, pw, "xxxxx")
				}
			}
		}
	}
	return msg
}

// ---- redis (RESP: optional AUTH, then PING → +PONG) ----

type redisCheck struct{ m config.Monitor }

func respCmd(args ...string) []byte {
	b := fmt.Appendf(nil, "*%d\r\n", len(args))
	for _, a := range args {
		b = fmt.Appendf(b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b
}

func (c *redisCheck) Check(ctx context.Context) Result {
	start := time.Now()
	conn, err := dial(ctx, c.m)
	if err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	if c.m.Password != "" {
		args := []string{"AUTH", c.m.Password}
		if c.m.Username != "" {
			args = []string{"AUTH", c.m.Username, c.m.Password}
		}
		conn.Write(respCmd(args...))
		line, err := r.ReadString('\n')
		if err != nil {
			return down(c.m, time.Since(start), "auth: %v", err)
		}
		if !strings.HasPrefix(line, "+OK") {
			return down(c.m, time.Since(start), "auth rejected: %s", strings.TrimSpace(line))
		}
	}
	if _, err := conn.Write(respCmd("PING")); err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return down(c.m, time.Since(start), "ping: %v", err)
	}
	if line = strings.TrimSpace(line); line != "+PONG" {
		return down(c.m, time.Since(start), "unexpected reply %q", line)
	}
	return grade(c.m, time.Since(start), "PONG")
}

// ---- kafka (ApiVersions v0: the broker must answer in Kafka protocol) ----

type kafkaCheck struct{ m config.Monitor }

func (c *kafkaCheck) Check(ctx context.Context) Result {
	start := time.Now()
	conn, err := dial(ctx, c.m)
	if err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	defer conn.Close()
	corr := rand.Int32()
	client := "vigil"
	body := binary.BigEndian.AppendUint16(nil, 18) // api_key ApiVersions
	body = binary.BigEndian.AppendUint16(body, 0)  // api_version
	body = binary.BigEndian.AppendUint32(body, uint32(corr))
	body = binary.BigEndian.AppendUint16(body, uint16(len(client)))
	body = append(body, client...)
	req := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if _, err := conn.Write(append(req, body...)); err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	head := make([]byte, 10) // size, correlation_id, error_code
	if _, err := io.ReadFull(conn, head); err != nil {
		return down(c.m, time.Since(start), "no kafka response: %v", err)
	}
	if got := int32(binary.BigEndian.Uint32(head[4:8])); got != corr {
		return down(c.m, time.Since(start), "not a kafka broker (bad correlation id)")
	}
	if code := int16(binary.BigEndian.Uint16(head[8:10])); code != 0 {
		return down(c.m, time.Since(start), "ApiVersions error code %d", code)
	}
	return grade(c.m, time.Since(start), "ApiVersions ok")
}

// ---- smtp (banner 220 + EHLO) ----

type smtpCheck struct{ m config.Monitor }

func (c *smtpCheck) Check(ctx context.Context) Result {
	start := time.Now()
	conn, err := dial(ctx, c.m)
	if err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	host, _, _ := net.SplitHostPort(c.m.Host)
	cl, err := smtp.NewClient(conn, host) // reads and checks the 220 banner
	if err != nil {
		conn.Close()
		return down(c.m, time.Since(start), "banner: %v", err)
	}
	defer cl.Close()
	if err := cl.Hello("vigil.local"); err != nil {
		return down(c.m, time.Since(start), "EHLO: %v", err)
	}
	cl.Quit()
	return grade(c.m, time.Since(start), "220 + EHLO ok")
}

// ---- grpc (grpc.health.v1.Health/Check over HTTP/2, no grpc dependency) ----

type grpcCheck struct {
	m      config.Monitor
	client *http.Client
}

func newGRPC(m config.Monitor) *grpcCheck {
	tr := &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: m.Expect.SkipTLSVerify}}
	if !m.TLS { // h2c: HTTP/2 without TLS, as most in-cluster gRPC servers speak
		tr.AllowHTTP = true
		tr.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	return &grpcCheck{m: m, client: &http.Client{Transport: tr}}
}

var grpcStatus = map[uint64]string{0: "UNKNOWN", 1: "SERVING", 2: "NOT_SERVING", 3: "SERVICE_UNKNOWN"}

func (c *grpcCheck) Check(ctx context.Context) Result {
	start := time.Now()
	// HealthCheckRequest{service = 1}
	var msg []byte
	if c.m.Service != "" {
		msg = append([]byte{0x0a}, binary.AppendUvarint(nil, uint64(len(c.m.Service)))...)
		msg = append(msg, c.m.Service...)
	}
	frame := append([]byte{0}, binary.BigEndian.AppendUint32(nil, uint32(len(msg)))...)
	frame = append(frame, msg...)
	scheme := "http"
	if c.m.TLS {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scheme+"://"+c.m.Host+"/grpc.health.v1.Health/Check", strings.NewReader(string(frame)))
	if err != nil {
		return down(c.m, 0, "%v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := c.client.Do(req)
	if err != nil {
		return down(c.m, time.Since(start), "%v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	lat := time.Since(start)
	if err != nil {
		return down(c.m, lat, "read: %v", err)
	}
	st := resp.Trailer.Get("Grpc-Status")
	if st == "" {
		st = resp.Header.Get("Grpc-Status") // trailers-only response
	}
	if st != "0" {
		return down(c.m, lat, "grpc-status %s %s", st, resp.Trailer.Get("Grpc-Message")+resp.Header.Get("Grpc-Message"))
	}
	// HealthCheckResponse{status = 1 (varint)}; an empty message means UNKNOWN
	status := uint64(0)
	if len(body) >= 7 && body[5] == 0x08 {
		status, _ = binary.Uvarint(body[6:])
	}
	if status != 1 {
		return down(c.m, lat, "health status %s", grpcStatus[status])
	}
	return grade(c.m, lat, "SERVING")
}

// ---- icmp ping ----

// Unprivileged ICMP ("udp4") works on macOS and on Linux when the gid is in
// net.ipv4.ping_group_range (many distros and Docker allow it); otherwise
// vigil falls back to raw sockets, which need CAP_NET_RAW.
type icmpCheck struct{ m config.Monitor }

func (c *icmpCheck) Check(ctx context.Context) Result {
	start := time.Now()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, c.m.Host)
	if err != nil || len(ips) == 0 {
		return down(c.m, time.Since(start), "resolve: %v", err)
	}
	ip := ips[0].IP
	v4 := ip.To4() != nil
	network, proto, typ, reply := "udp6", 58, icmp.Type(ipv6.ICMPTypeEchoRequest), icmp.Type(ipv6.ICMPTypeEchoReply)
	if v4 {
		network, proto, typ, reply = "udp4", 1, ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	}
	conn, err := icmp.ListenPacket(network, "")
	privileged := false
	if err != nil {
		raw := "ip6:ipv6-icmp"
		if v4 {
			raw = "ip4:icmp"
		}
		if conn, err = icmp.ListenPacket(raw, ""); err != nil {
			return down(c.m, time.Since(start), "icmp not permitted (allow ping_group_range or CAP_NET_RAW): %v", err)
		}
		privileged = true
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	seq := int(rand.Uint32() & 0xffff)
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: seq, Data: []byte("vigil")}}
	b, _ := msg.Marshal(nil)
	var dst net.Addr = &net.UDPAddr{IP: ip}
	if privileged {
		dst = &net.IPAddr{IP: ip}
	}
	sent := time.Now()
	if _, err := conn.WriteTo(b, dst); err != nil {
		return down(c.m, time.Since(start), "send: %v", err)
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return down(c.m, time.Since(start), "no echo reply: %v", timeoutMsg(err))
		}
		rm, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil || rm.Type != reply {
			continue
		}
		// unprivileged sockets rewrite the ID; the sequence number identifies our probe
		if e, ok := rm.Body.(*icmp.Echo); ok && e.Seq == seq {
			rtt := time.Since(sent)
			return grade(c.m, rtt, "reply from "+ip.String())
		}
	}
}

func timeoutMsg(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return err.Error()
}

// ---- JSON assertions for http ----

// jsonAssert checks dotted paths ("a.b.0.c") in a JSON document.
func jsonAssert(body []byte, exps []config.JSONExpect) error {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("body is not JSON: %v", err)
	}
	for _, e := range exps {
		v, ok := doc, true
		for _, part := range strings.Split(e.Path, ".") {
			switch node := v.(type) {
			case map[string]any:
				v, ok = node[part]
			case []any:
				i, err := strconv.Atoi(part)
				ok = err == nil && i >= 0 && i < len(node)
				if ok {
					v = node[i]
				}
			default:
				ok = false
			}
			if !ok {
				return fmt.Errorf("json %s: missing", e.Path)
			}
		}
		if got := render(v); got != e.Equals {
			return fmt.Errorf("json %s = %q, want %q", e.Path, got, e.Equals)
		}
	}
	return nil
}

func render(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "null"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
