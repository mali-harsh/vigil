package discovery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/config"
)

const (
	saDir      = "/var/run/secrets/kubernetes.io/serviceaccount"
	annoPrefix = "vigil.dev/"
)

// Kubernetes discovers Services annotated vigil.dev/enable: "true" using the
// in-cluster service account (RBAC: get/list services). If no url/host
// annotation is given, the target defaults to the Service's cluster DNS name
// and first port: tcp <svc>.<ns>.svc:<port>, or with vigil.dev/path an http
// check on http://<svc>.<ns>.svc:<port><path>.
type Kubernetes struct {
	Namespaces []string // empty = all
	Log        *slog.Logger

	api    string
	token  func() (string, error)
	client *http.Client
}

func NewKubernetes(namespaces []string, log *slog.Logger) (*Kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Kubernetes{
		Namespaces: namespaces, Log: log,
		api: "https://" + host + ":" + port,
		// re-read every call: projected tokens rotate
		token: func() (string, error) {
			b, err := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b)), err
		},
		client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

func (k *Kubernetes) Name() string { return "kubernetes" }

type kubeServiceList struct {
	Items []kubeService `json:"items"`
}

type kubeService struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Ports []struct {
			Port int `json:"port"`
		} `json:"ports"`
	} `json:"spec"`
}

func (k *Kubernetes) List(ctx context.Context) ([]config.Monitor, error) {
	paths := []string{"/api/v1/services"}
	if len(k.Namespaces) > 0 {
		paths = nil
		for _, ns := range k.Namespaces {
			paths = append(paths, "/api/v1/namespaces/"+ns+"/services")
		}
	}
	var all []kubeService
	for _, p := range paths {
		var l kubeServiceList
		if err := k.get(ctx, p, &l); err != nil {
			return nil, err
		}
		all = append(all, l.Items...)
	}
	return kubeMonitors(all, k.Log), nil
}

func (k *Kubernetes) get(ctx context.Context, path string, v any) error {
	tok, err := k.token()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}

func kubeMonitors(svcs []kubeService, log *slog.Logger) []config.Monitor {
	var out []config.Monitor
	for _, s := range svcs {
		md := s.Metadata
		kv := keysWithPrefix(md.Annotations, annoPrefix)
		if !isTrue(kv["enable"]) {
			continue
		}
		dns := md.Name + "." + md.Namespace + ".svc"
		if kv["url"] == "" && kv["host"] == "" && len(s.Spec.Ports) > 0 {
			port := fmt.Sprint(s.Spec.Ports[0].Port)
			if path := kv["path"]; path != "" {
				kv["url"] = "http://" + dns + ":" + port + path
			} else if kv["type"] == "" || kv["type"] == "tcp" {
				kv["host"] = dns + ":" + port
			}
		}
		m, _, err := fromKeys("k8s:"+md.Namespace+"."+md.Name, // DNS labels never contain dots, so "." is unambiguous (and URL-safe)
			md.Namespace+"/"+md.Name, kv)
		if err != nil {
			log.Warn("bad vigil annotations", "service", md.Namespace+"/"+md.Name, "err", err)
			continue
		}
		out = append(out, m)
	}
	return out
}
