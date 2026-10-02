package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

// Docker discovers containers labelled vigil.enable=true via the Docker
// Engine API on a unix socket (mount /var/run/docker.sock read-only).
//
//	docker run -l vigil.enable=true -l vigil.url=http://api:8080/health ...
type Docker struct {
	client *http.Client
	Log    *slog.Logger
}

func NewDocker(socket string, log *slog.Logger) *Docker {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Docker{client: &http.Client{Transport: tr, Timeout: 10 * time.Second}, Log: log}
}

func (d *Docker) Name() string { return "docker" }

type dockerContainer struct {
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

func (d *Docker) List(ctx context.Context) ([]config.Monitor, error) {
	// all=true: a stopped container must stay monitored (and alert), not vanish
	q := url.Values{"all": {"true"}, "filters": {`{"label":["vigil.enable"]}`}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("docker API %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var cs []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
		return nil, err
	}
	return dockerMonitors(cs, d.Log), nil
}

func dockerMonitors(cs []dockerContainer, log *slog.Logger) []config.Monitor {
	var out []config.Monitor
	for _, c := range cs {
		name := "container"
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		kv := keysWithPrefix(c.Labels, "vigil.")
		// ID from the display name, not the container ID: `compose up`
		// recreates containers, and history must survive that.
		label := kv["name"]
		if label == "" {
			label = name
		}
		m, ok, err := fromKeys("docker:"+config.Slug(label), name, kv)
		if !ok {
			continue
		}
		if err != nil {
			log.Warn("bad vigil labels", "container", name, "err", err)
			continue
		}
		out = append(out, m)
	}
	return out
}
