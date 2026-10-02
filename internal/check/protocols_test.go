package check

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func run(t *testing.T, m config.Monitor) Result {
	t.Helper()
	c, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.Check(ctx)
}

// serve accepts one connection at a time and hands it to h.
func serve(t *testing.T, h func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); h(c) }()
		}
	}()
	return ln.Addr().String()
}

func fakeRedis(t *testing.T, password, pong string) string {
	return serve(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		for {
			// read one RESP array command
			head, err := r.ReadString('\n')
			if err != nil {
				return
			}
			n := 0
			for _, ch := range strings.TrimSpace(head[1:]) {
				n = n*10 + int(ch-'0')
			}
			var args []string
			for range n {
				r.ReadString('\n')
				a, _ := r.ReadString('\n')
				args = append(args, strings.TrimSpace(a))
			}
			switch args[0] {
			case "AUTH":
				if args[len(args)-1] == password {
					io.WriteString(c, "+OK\r\n")
				} else {
					io.WriteString(c, "-WRONGPASS invalid password\r\n")
				}
			case "PING":
				io.WriteString(c, pong+"\r\n")
			}
		}
	})
}

func TestRedis(t *testing.T) {
	addr := fakeRedis(t, "s3cret", "+PONG")
	if r := run(t, config.Monitor{Type: "redis", Host: addr, Password: "s3cret"}); r.Status != Up {
		t.Errorf("good: %+v", r)
	}
	if r := run(t, config.Monitor{Type: "redis", Host: addr, Password: "wrong"}); r.Status != Down || !strings.Contains(r.Message, "auth rejected") {
		t.Errorf("bad password: %+v", r)
	}
	loading := fakeRedis(t, "", "-LOADING Redis is loading the dataset")
	if r := run(t, config.Monitor{Type: "redis", Host: loading}); r.Status != Down {
		t.Errorf("loading redis must be down: %+v", r)
	}
}

func TestKafka(t *testing.T) {
	broker := func(errCode uint16, wrongCorr bool) string {
		return serve(t, func(c net.Conn) {
			head := make([]byte, 4)
			io.ReadFull(c, head)
			body := make([]byte, binary.BigEndian.Uint32(head))
			io.ReadFull(c, body)
			if binary.BigEndian.Uint16(body[0:2]) != 18 {
				return
			}
			corr := binary.BigEndian.Uint32(body[4:8])
			if wrongCorr {
				corr++
			}
			resp := binary.BigEndian.AppendUint32(nil, corr)
			resp = binary.BigEndian.AppendUint16(resp, errCode)
			resp = binary.BigEndian.AppendUint32(resp, 0) // empty api array
			c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(resp))), resp...))
		})
	}
	if r := run(t, config.Monitor{Type: "kafka", Host: broker(0, false)}); r.Status != Up {
		t.Errorf("good: %+v", r)
	}
	if r := run(t, config.Monitor{Type: "kafka", Host: broker(35, false)}); r.Status != Down {
		t.Errorf("error code: %+v", r)
	}
	if r := run(t, config.Monitor{Type: "kafka", Host: broker(0, true)}); r.Status != Down || !strings.Contains(r.Message, "not a kafka") {
		t.Errorf("wrong correlation: %+v", r)
	}
	http := serve(t, func(c net.Conn) { io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n") })
	if r := run(t, config.Monitor{Type: "kafka", Host: http}); r.Status != Down {
		t.Errorf("an HTTP server is not a broker: %+v", r)
	}
}

func TestSMTP(t *testing.T) {
	good := serve(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		io.WriteString(c, "220 mail.example ESMTP\r\n")
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(l, "EHLO"):
				io.WriteString(c, "250-mail.example\r\n250 SIZE 1000\r\n")
			case strings.HasPrefix(l, "QUIT"):
				io.WriteString(c, "221 bye\r\n")
				return
			}
		}
	})
	if r := run(t, config.Monitor{Type: "smtp", Host: good}); r.Status != Up {
		t.Errorf("good: %+v", r)
	}
	busy := serve(t, func(c net.Conn) { io.WriteString(c, "554 no service\r\n") })
	if r := run(t, config.Monitor{Type: "smtp", Host: busy}); r.Status != Down {
		t.Errorf("554 banner: %+v", r)
	}
}

func fakeGRPC(t *testing.T, status byte, grpcStatus string) string {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grpc.health.v1.Health/Check" || r.Header.Get("Content-Type") != "application/grpc" {
			http.Error(w, "bad", http.StatusNotFound)
			return
		}
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		msg := []byte{0x08, status}
		w.Write(append([]byte{0, 0, 0, 0, byte(len(msg))}, msg...))
		w.Header().Set("Grpc-Status", grpcStatus)
	})
	srv := httptest.NewServer(h2c.NewHandler(h, &http2.Server{}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestGRPCHealth(t *testing.T) {
	if r := run(t, config.Monitor{Type: "grpc", Host: fakeGRPC(t, 1, "0")}); r.Status != Up {
		t.Errorf("serving: %+v", r)
	}
	if r := run(t, config.Monitor{Type: "grpc", Host: fakeGRPC(t, 2, "0")}); r.Status != Down || !strings.Contains(r.Message, "NOT_SERVING") {
		t.Errorf("not serving: %+v", r)
	}
	if r := run(t, config.Monitor{Type: "grpc", Host: fakeGRPC(t, 1, "12")}); r.Status != Down {
		t.Errorf("unimplemented: %+v", r)
	}
}

func TestJSONAssertions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"ok","db":{"connected":true,"lag_ms":12},"checks":[{"name":"redis","ok":false}]}`)
	}))
	defer srv.Close()
	m := config.Monitor{Type: "http", URL: srv.URL, Method: "GET"}
	m.Expect.JSON = []config.JSONExpect{{Path: "status", Equals: "ok"}, {Path: "db.connected", Equals: "true"}, {Path: "db.lag_ms", Equals: "12"}}
	if r := run(t, m); r.Status != Up {
		t.Errorf("all match: %+v", r)
	}
	m.Expect.JSON = []config.JSONExpect{{Path: "checks.0.ok", Equals: "true"}}
	if r := run(t, m); r.Status != Down || !strings.Contains(r.Message, `checks.0.ok = "false"`) {
		t.Errorf("mismatch: %+v", r)
	}
	m.Expect.JSON = []config.JSONExpect{{Path: "nope.x", Equals: "1"}}
	if r := run(t, m); r.Status != Down || !strings.Contains(r.Message, "missing") {
		t.Errorf("missing: %+v", r)
	}
}

func TestICMPLoopback(t *testing.T) {
	r := run(t, config.Monitor{Type: "icmp", Host: "127.0.0.1"})
	if strings.Contains(r.Message, "not permitted") {
		t.Skip("ICMP not permitted here: " + r.Message)
	}
	if r.Status != Up {
		t.Fatalf("ping 127.0.0.1: %+v", r)
	}
}

func TestPostgres(t *testing.T) {
	url := os.Getenv("VIGIL_TEST_POSTGRES")
	if url == "" {
		t.Skip("set VIGIL_TEST_POSTGRES")
	}
	if r := run(t, config.Monitor{Type: "postgres", URL: url}); r.Status != Up {
		t.Fatalf("good: %+v", r)
	}
	bad := strings.Replace(url, "vigiltest", "wrongpassword", 1)
	r := run(t, config.Monitor{Type: "postgres", URL: bad})
	if r.Status != Down || strings.Contains(r.Message, "wrongpassword") {
		t.Fatalf("bad password must be down without leaking it: %+v", r)
	}
}

func TestRedactErr(t *testing.T) {
	got := redactErr(io.EOF, "postgres://u:hunter2@h/db")
	if strings.Contains(got, "hunter2") {
		t.Fatal(got)
	}
	if msg := redactErr(errString("auth failed for hunter2"), "postgres://u:hunter2@h/db"); strings.Contains(msg, "hunter2") {
		t.Fatal(msg)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Opt-in checks against real servers:
//
//	VIGIL_TEST_REDIS=host:port[,password]   VIGIL_TEST_KAFKA=host:port
func TestRealRedisAndKafka(t *testing.T) {
	if v := os.Getenv("VIGIL_TEST_REDIS"); v != "" {
		host, pw, _ := strings.Cut(v, ",")
		if r := run(t, config.Monitor{Type: "redis", Host: host, Password: pw}); r.Status != Up {
			t.Errorf("real redis: %+v", r)
		}
		if r := run(t, config.Monitor{Type: "redis", Host: host, Password: "definitely-wrong"}); r.Status != Down {
			t.Errorf("real redis wrong password: %+v", r)
		}
	}
	if v := os.Getenv("VIGIL_TEST_KAFKA"); v != "" {
		if r := run(t, config.Monitor{Type: "kafka", Host: v}); r.Status != Up {
			t.Errorf("real kafka: %+v", r)
		}
	}
}
