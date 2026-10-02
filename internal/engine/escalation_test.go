package engine_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/monitor"
)

func escCfg() *config.Config {
	return &config.Config{
		Server:    config.Server{PublicURL: "https://status.example.com", AckSecret: "0123456789abcdef0123456789abcdef"},
		Notifiers: []config.Notifier{{Name: "slack"}, {Name: "pd"}, {Name: "cto"}},
		Escalations: []config.Escalation{{Name: "prod", Steps: []config.EscalationStep{
			{Notify: []string{"slack"}},
			{After: config.Duration(300 * time.Millisecond), Notify: []string{"pd"}},
			{After: config.Duration(900 * time.Millisecond), Notify: []string{"cto"}},
		}}},
		Monitors: []config.Monitor{{
			ID: "api", Name: "API", Type: "http", URL: "http://192.0.2.1", Method: "GET",
			Interval: config.Duration(time.Hour), Timeout: config.Duration(time.Second),
			FailThreshold: 1, RecoverThreshold: 1, Escalation: "prod", Notify: []string{"slack"},
		}},
	}
}

func TestEscalationStopsOnAck(t *testing.T) {
	h := startManual(t, escCfg(), time.Hour)
	h.in <- check.Result{MonitorID: "api", At: time.Now(), Status: check.Down, Message: "503"}
	waitFor(t, "down", func() bool { return h.view().State == monitor.Down })
	waitFor(t, "step 2 (pd)", func() bool { return slices.Contains(h.rec.sent(), "down→pd") })

	inc, _ := h.st.ActiveIncident(context.Background(), "api")
	if err := h.eng.Ack(context.Background(), inc.ID, "harsh"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // past step 3's deadline
	if slices.Contains(h.rec.sent(), "down→cto") {
		t.Fatalf("escalated after ack: %v", h.rec.sent())
	}
	h.in <- check.Result{MonitorID: "api", At: time.Now(), Status: check.Up, Message: "200"}
	waitFor(t, "up", func() bool { return h.view().State == monitor.Up })
	want := []string{"down→slack", "down→pd", "acknowledged→slack,pd", "recovered→slack,pd"}
	if got := h.rec.sent(); !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if !h.rec.events[0].AckURLSet() {
		t.Fatal("first alert carries no ack link")
	}
}

func TestUnacknowledgedEscalatesToTheEnd(t *testing.T) {
	h := startManual(t, escCfg(), time.Hour)
	h.in <- check.Result{MonitorID: "api", At: time.Now(), Status: check.Down, Message: "503"}
	waitFor(t, "step 3 (cto)", func() bool { return slices.Contains(h.rec.sent(), "down→cto") })
	h.in <- check.Result{MonitorID: "api", At: time.Now(), Status: check.Up, Message: "200"}
	waitFor(t, "recovered", func() bool { return slices.Contains(h.rec.sent(), "recovered→slack,pd,cto") })
	if n := len(h.rec.sent()); n != 4 {
		t.Fatalf("each step pages once: %v", h.rec.sent())
	}
}
