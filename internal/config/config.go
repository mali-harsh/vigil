// Package config loads and validates vigil's YAML configuration.
//
// Config-as-code is the source of truth for monitors and notifiers: on every
// start the loaded set is synced into the store, keyed by monitor ID.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	Server      Server        `yaml:"server"`
	Defaults    Defaults      `yaml:"defaults"`
	Notifiers   []Notifier    `yaml:"notifiers"`
	Monitors    []Monitor     `yaml:"monitors"`
	StatusPage  StatusPage    `yaml:"status_page"`
	Maintenance []Maintenance `yaml:"maintenance"`
}

type Server struct {
	Listen      string   `yaml:"listen"`
	DataDir     string   `yaml:"data_dir"`
	PublicURL   string   `yaml:"public_url"`
	APITokens   []string `yaml:"api_tokens"`   // read-only bearer tokens; empty = read API open
	AdminTokens []string `yaml:"admin_tokens"` // admin UI + write API; empty = admin disabled
	Retention   Duration `yaml:"retention"`    // raw check results
	TLS         TLS      `yaml:"tls"`
}

// TLS enables automatic HTTPS via Let's Encrypt. vigil then serves :443 and
// redirects :80; Listen is ignored.
type TLS struct {
	Domains []string `yaml:"domains"`
	Email   string   `yaml:"email"`
}

type Defaults struct {
	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	FailThreshold    int      `yaml:"fail_threshold"`
	RecoverThreshold int      `yaml:"recover_threshold"`
	ReminderEvery    Duration `yaml:"reminder_every"`
	Notify           []string `yaml:"notify"`
}

type Notifier struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // slack | webhook
	URL  string `yaml:"url"`
}

type Monitor struct {
	ID     string `yaml:"id"` // stable key; derived from name if empty
	Name   string `yaml:"name"`
	Type   string `yaml:"type"` // http | tcp | tls | dns | push
	Paused bool   `yaml:"paused"`

	// target
	URL    string            `yaml:"url"`  // http
	Host   string            `yaml:"host"` // tcp/tls: host:port, dns: hostname
	Method string            `yaml:"method"`
	Header map[string]string `yaml:"headers"`
	Body   string            `yaml:"body"`

	Expect Expect `yaml:"expect"`

	// push (heartbeat)
	Token string   `yaml:"token"`
	Grace Duration `yaml:"grace"`

	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	FailThreshold    int      `yaml:"fail_threshold"`
	RecoverThreshold int      `yaml:"recover_threshold"`
	ReminderEvery    Duration `yaml:"reminder_every"`
	Notify           []string `yaml:"notify"`
}

type Expect struct {
	Status        []int    `yaml:"status"`        // http; default 200-399
	BodyContains  string   `yaml:"body_contains"` // http
	MaxLatency    Duration `yaml:"max_latency"`   // over this = DEGRADED
	CertMinDays   int      `yaml:"cert_min_days"` // tls/http(s): under this = DOWN
	ResolvesTo    []string `yaml:"resolves_to"`   // dns
	SkipTLSVerify bool     `yaml:"skip_tls_verify"`
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads path, expands ${ENV} references (so secrets stay out of git),
// applies defaults and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// Expand ${ENV} inside scalar values only — after parsing, so comments are
	// ignored and a secret containing YAML syntax can't alter the structure.
	var missing []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			n.Value = envRe.ReplaceAllStringFunc(n.Value, func(m string) string {
				name := envRe.FindStringSubmatch(m)[1]
				v, ok := os.LookupEnv(name)
				if !ok {
					missing = append(missing, name)
				}
				return v
			})
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variables: %s", strings.Join(missing, ", "))
	}

	var c Config
	if len(root.Content) > 0 {
		// re-encode so the strict decoder (unknown keys are errors) can run
		b, err := yaml.Marshal(&root)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true) // typos in keys are errors, not silently ignored
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	c.applyDefaults()
	if err := errors.Join(c.validate(), c.validateStatusPage()); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Listen == "" {
		s.Listen = ":8080"
	}
	if s.DataDir == "" {
		s.DataDir = os.Getenv("VIGIL_DATA_DIR")
	}
	if s.DataDir == "" {
		s.DataDir = "./data"
	}
	if s.Retention == 0 {
		s.Retention = Duration(30 * 24 * time.Hour)
	}
	c.applyStatusPageDefaults()
	d := &c.Defaults
	if d.Interval == 0 {
		d.Interval = Duration(60 * time.Second)
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(10 * time.Second)
	}
	if d.FailThreshold == 0 {
		d.FailThreshold = 3
	}
	if d.RecoverThreshold == 0 {
		d.RecoverThreshold = 2
	}
	if d.ReminderEvery == 0 {
		d.ReminderEvery = Duration(30 * time.Minute)
	}
	for i := range c.Monitors {
		m := &c.Monitors[i]
		if m.ID == "" {
			m.ID = Slug(m.Name)
		}
		if m.Interval == 0 {
			m.Interval = d.Interval
		}
		if m.Timeout == 0 {
			m.Timeout = d.Timeout
		}
		if m.FailThreshold == 0 {
			m.FailThreshold = d.FailThreshold
			if m.Type == "push" {
				m.FailThreshold = 1 // grace already is the tolerance
			}
		}
		if m.RecoverThreshold == 0 {
			m.RecoverThreshold = d.RecoverThreshold
			if m.Type == "push" {
				m.RecoverThreshold = 1
			}
		}
		if m.ReminderEvery == 0 {
			m.ReminderEvery = d.ReminderEvery
		}
		if m.Notify == nil {
			m.Notify = d.Notify
		}
		if m.Type == "http" && m.Method == "" {
			m.Method = "GET"
		}
		if m.Type == "push" && m.Grace == 0 {
			m.Grace = Duration(m.Interval.D() / 2)
		}
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func Slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func (c *Config) validate() error {
	var errs []error
	notifiers := map[string]bool{}
	for _, n := range c.Notifiers {
		switch {
		case n.Name == "":
			errs = append(errs, errors.New("notifier: name required"))
		case notifiers[n.Name]:
			errs = append(errs, fmt.Errorf("notifier %q: duplicate name", n.Name))
		case n.Type != "slack" && n.Type != "webhook":
			errs = append(errs, fmt.Errorf("notifier %q: unknown type %q", n.Name, n.Type))
		case !isURL(n.URL):
			errs = append(errs, fmt.Errorf("notifier %q: invalid url", n.Name))
		}
		notifiers[n.Name] = true
	}

	ids, tokens := map[string]bool{}, map[string]bool{}
	for _, m := range c.Monitors {
		p := fmt.Sprintf("monitor %q", m.Name)
		if m.Name == "" || m.ID == "" {
			errs = append(errs, errors.New("monitor: name required"))
			continue
		}
		if ids[m.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", p, m.ID))
		}
		ids[m.ID] = true
		switch m.Type {
		case "http":
			if !isURL(m.URL) {
				errs = append(errs, fmt.Errorf("%s: valid url required", p))
			}
		case "tcp", "tls":
			if !strings.Contains(m.Host, ":") {
				errs = append(errs, fmt.Errorf("%s: host must be host:port", p))
			}
		case "dns":
			if m.Host == "" {
				errs = append(errs, fmt.Errorf("%s: host required", p))
			}
		case "push":
			if len(m.Token) < 16 {
				errs = append(errs, fmt.Errorf("%s: token of >=16 chars required (it is the secret in the ping URL)", p))
			}
			if tokens[m.Token] {
				errs = append(errs, fmt.Errorf("%s: duplicate token", p))
			}
			tokens[m.Token] = true
		default:
			errs = append(errs, fmt.Errorf("%s: unknown type %q", p, m.Type))
		}
		if m.Type != "push" && m.Timeout >= m.Interval {
			errs = append(errs, fmt.Errorf("%s: timeout must be shorter than interval", p))
		}
		if m.Interval.D() < time.Second {
			errs = append(errs, fmt.Errorf("%s: interval must be >= 1s", p))
		}
		for _, n := range m.Notify {
			if !notifiers[n] {
				errs = append(errs, fmt.Errorf("%s: unknown notifier %q", p, n))
			}
		}
	}
	return errors.Join(errs...)
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
