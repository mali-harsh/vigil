package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndEnv(t *testing.T) {
	t.Setenv("HOOK", "https://hooks.slack.com/triggers/x")
	c, err := Parse([]byte(`
notifiers:
  - {name: slack, type: slack, url: "${HOOK}"}
defaults:
  notify: [slack]
monitors:
  - {name: "Geo API", type: http, url: "https://geo.example.com/health"}
  - {name: Backup, type: push, token: "0123456789abcdef", interval: 1h}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Notifiers[0].URL != "https://hooks.slack.com/triggers/x" {
		t.Errorf("env not expanded: %q", c.Notifiers[0].URL)
	}
	geo, backup := c.Monitors[0], c.Monitors[1]
	if geo.ID != "geo-api" || geo.Method != "GET" || geo.Interval.D() != time.Minute || geo.FailThreshold != 3 {
		t.Errorf("http defaults wrong: %+v", geo)
	}
	if len(geo.Notify) != 1 || geo.Notify[0] != "slack" {
		t.Errorf("default notify not applied: %v", geo.Notify)
	}
	if backup.FailThreshold != 1 || backup.Grace.D() != 30*time.Minute {
		t.Errorf("push defaults wrong: %+v", backup)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unset env":        `notifiers: [{name: a, type: slack, url: "${NOPE_NOT_SET}"}]`,
		"unknown key":      `monitors: [{name: a, type: http, url: "https://x", urll: "y"}]`,
		"bad type":         `monitors: [{name: a, type: ftp}]`,
		"bad url":          `monitors: [{name: a, type: http, url: "x"}]`,
		"tcp no port":      `monitors: [{name: a, type: tcp, host: "x"}]`,
		"short token":      `monitors: [{name: a, type: push, token: "abc"}]`,
		"timeout>interval": `monitors: [{name: a, type: tcp, host: "x:1", interval: 5s, timeout: 10s}]`,
		"unknown notifier": `monitors: [{name: a, type: tcp, host: "x:1", notify: [ghost]}]`,
		"duplicate id":     "monitors: [{name: a, type: tcp, host: \"x:1\"}, {name: A, type: tcp, host: \"y:1\"}]",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAllErrorsReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte(`monitors: [{name: a, type: http, url: x}, {name: b, type: tcp, host: y}]`))
	if err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestEnvIgnoredInCommentsAndCannotInjectYAML(t *testing.T) {
	t.Setenv("EVIL", "x\"\nmonitors: [{name: injected, type: tcp, host: \"a:1\"}]")
	c, err := Parse([]byte(`
# api_tokens: ["${NOT_SET_ANYWHERE}"]   <- comment: must be ignored
server:
  api_tokens: ["${EVIL}"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Monitors) != 0 || !strings.Contains(c.Server.APITokens[0], "injected") {
		t.Fatalf("env value altered structure: %+v", c.Monitors)
	}
}
