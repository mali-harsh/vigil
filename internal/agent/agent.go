// Package agent runs checks inside a private network (or another region) and
// reports to a vigil server. It only makes outbound HTTPS requests — no
// inbound ports, no VPN — so it fits anywhere a container can run: a VPC,
// Cloud Run, ECS, a Raspberry Pi in the office.
//
// Protocol (all requests carry "Authorization: Bearer <agent token>"):
//
//	GET  /api/agent/v1/assignments   → {agent, poll_seconds, monitors}; ETag / If-None-Match
//	POST /api/agent/v1/results       ← {results: [...]}
//
// Results are buffered in memory and retried while the server is unreachable;
// when the buffer is full the oldest are dropped (the newest matter most).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/scheduler"
)

// Wire types, shared with the server.

type Assignments struct {
	Agent       string           `json:"agent"`
	PollSeconds int              `json:"poll_seconds"`
	Monitors    []config.Monitor `json:"monitors"`
}

type WireResult struct {
	MonitorID string       `json:"monitor_id"`
	At        time.Time    `json:"at"`
	Status    check.Status `json:"status"`
	LatencyMS int64        `json:"latency_ms"`
	Message   string       `json:"message"`
}

type ResultBatch struct {
	Results []WireResult `json:"results"`
}

type BatchResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

const (
	maxBuffer  = 10000
	flushEvery = 2 * time.Second
	maxBatch   = 500
)

type Agent struct {
	Server string // base URL, e.g. https://status.example.com
	Token  string
	HTTP   *http.Client
	Log    *slog.Logger

	name    string
	etag    string
	running map[string]config.Monitor
}

func (a *Agent) Run(ctx context.Context) error {
	a.Server = strings.TrimRight(a.Server, "/")
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	if !strings.HasPrefix(a.Server, "https://") && !isLocal(a.Server) {
		a.Log.Warn("server URL is not https — the agent token and check configs travel in clear text", "server", a.Server)
	}
	a.running = map[string]config.Monitor{}

	// First contact must succeed: a wrong token or URL should fail loudly at
	// startup rather than retry silently forever.
	as, err := a.fetch(ctx)
	if err != nil {
		return fmt.Errorf("first contact with %s: %w", a.Server, err)
	}
	a.name = as.Agent
	a.Log.Info("agent connected", "agent", a.name, "server", a.Server, "monitors", len(as.Monitors))

	results := make(chan check.Result, 1024)
	sched := scheduler.NewAt(results, a.name)
	a.apply(ctx, sched, as)

	sendDone := make(chan struct{})
	go func() { a.sendLoop(ctx, results); close(sendDone) }()

	poll := time.Duration(max(as.PollSeconds, 5)) * time.Second
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			sched.Wait()
			<-sendDone
			return nil
		case <-t.C:
			as, err := a.fetch(ctx)
			switch {
			case errors.Is(err, errNotModified):
			case err != nil:
				a.Log.Warn("poll assignments failed; keeping current set", "err", err)
			default:
				a.apply(ctx, sched, as)
			}
		}
	}
}

var errNotModified = errors.New("not modified")

func (a *Agent) fetch(ctx context.Context) (*Assignments, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Server+"/api/agent/v1/assignments", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if a.etag != "" {
		req.Header.Set("If-None-Match", a.etag)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, errNotModified
	case http.StatusOK:
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var as Assignments
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&as); err != nil {
		return nil, fmt.Errorf("decode assignments: %w", err)
	}
	a.etag = resp.Header.Get("ETag")
	return &as, nil
}

// apply reconciles running probes with the assignment list.
func (a *Agent) apply(ctx context.Context, sched *scheduler.Scheduler, as *Assignments) {
	want := map[string]config.Monitor{}
	for _, m := range as.Monitors {
		want[m.ID] = m
	}
	for id := range a.running {
		if _, ok := want[id]; !ok {
			sched.Stop(id)
			delete(a.running, id)
			a.Log.Info("monitor unassigned", "monitor", id)
		}
	}
	for id, m := range want {
		if old, ok := a.running[id]; ok && reflect.DeepEqual(old, m) {
			continue
		}
		if err := sched.Start(ctx, m, time.Time{}); err != nil {
			a.Log.Error("cannot run monitor", "monitor", id, "err", err)
			continue
		}
		a.running[id] = m
		a.Log.Info("monitor assigned", "monitor", id, "type", m.Type)
	}
}

func (a *Agent) sendLoop(ctx context.Context, in <-chan check.Result) {
	var buf []WireResult
	dropped := 0
	backoff := time.Duration(0)
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	flush := func(ctx context.Context) {
		for len(buf) > 0 {
			n := min(len(buf), maxBatch)
			if err := a.post(ctx, buf[:n]); err != nil {
				backoff = min(max(2*backoff, flushEvery), time.Minute)
				a.Log.Warn("send results failed; buffering", "buffered", len(buf), "retry_in", backoff, "err", err)
				return
			}
			buf, backoff = buf[n:], 0
		}
	}
	var nextTry time.Time
	for {
		select {
		case <-ctx.Done():
			// best effort: one last flush with a short deadline
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			flush(fctx)
			cancel()
			return
		case r := <-in:
			buf = append(buf, WireResult{MonitorID: r.MonitorID, At: r.At, Status: r.Status, LatencyMS: r.Latency.Milliseconds(), Message: r.Message})
			if len(buf) > maxBuffer {
				dropped += len(buf) - maxBuffer
				buf = buf[len(buf)-maxBuffer:]
				a.Log.Warn("result buffer full, dropped oldest", "dropped_total", dropped)
			}
		case now := <-t.C:
			if now.After(nextTry) {
				flush(ctx)
				nextTry = now.Add(backoff)
			}
		}
	}
}

func (a *Agent) post(ctx context.Context, rs []WireResult) error {
	b, _ := json.Marshal(ResultBatch{Results: rs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Server+"/api/agent/v1/results", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var br BatchResponse
	if json.NewDecoder(resp.Body).Decode(&br) == nil && br.Rejected > 0 {
		a.Log.Warn("server rejected results (monitor unassigned or invalid)", "rejected", br.Rejected)
	}
	return nil
}

func isLocal(u string) bool {
	return strings.HasPrefix(u, "http://localhost") || strings.HasPrefix(u, "http://127.0.0.1") || strings.HasPrefix(u, "http://[::1]")
}
