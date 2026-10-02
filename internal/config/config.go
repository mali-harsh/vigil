// Package config loads and validates vigil's YAML configuration.
//
// Config-as-code is the source of truth for monitors and notifiers: on every
// start the loaded set is synced into the store, keyed by monitor ID.
package config

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
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
	Agents      []Agent       `yaml:"agents"`
	Escalations []Escalation  `yaml:"escalations"`
	Discovery   Discovery     `yaml:"discovery"`
	Maintenance []Maintenance `yaml:"maintenance"`
}

type Server struct {
	Listen      string    `yaml:"listen"`
	DataDir     string    `yaml:"data_dir"`
	PublicURL   string    `yaml:"public_url"`
	AckSecret   string    `yaml:"ack_secret"`   // signs one-click acknowledge links in alerts
	APITokens   []string  `yaml:"api_tokens"`   // read-only bearer tokens; empty = read API open
	AdminTokens []string  `yaml:"admin_tokens"` // admin UI + write API; empty = admin disabled
	SMTP        SMTP      `yaml:"smtp"`
	APIKeys     []APIKey  `yaml:"api_keys"`
	Auth        Auth      `yaml:"auth"`
	Retention   Duration  `yaml:"retention"` // raw check results
	Heartbeat   Heartbeat `yaml:"heartbeat"` // dead-man's switch: vigil pings this while healthy
	Database    Database  `yaml:"database"`
	HA          HA        `yaml:"ha"`
	TLS         TLS       `yaml:"tls"`
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
	Type string `yaml:"type"` // slack | webhook | discord | teams | email | pagerduty | opsgenie | telegram
	URL  string `yaml:"url"`  // slack, webhook, discord, teams

	To         []string `yaml:"to"`          // email recipients (needs server.smtp)
	RoutingKey string   `yaml:"routing_key"` // pagerduty Events API v2 integration key
	APIKey     string   `yaml:"api_key"`     // opsgenie
	Region     string   `yaml:"region"`      // opsgenie: us (default) | eu
	BotToken   string   `yaml:"bot_token"`   // telegram
	ChatID     string   `yaml:"chat_id"`     // telegram
}

// APIKey is a named bearer token limited to scopes:
//
//	read             monitors, results, incidents, agents, metrics
//	incidents:write  declare / update / acknowledge incidents
//	subscribers:write manage status-page subscribers
//	admin            everything (incl. admin UI forms, notifier tests)
type APIKey struct {
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Scopes []string `yaml:"scopes"`
}

var Scopes = []string{"read", "incidents:write", "subscribers:write", "admin"}

// Roles map people to scopes.
var Roles = map[string][]string{
	"viewer":    {"read"},
	"responder": {"read", "incidents:write"},
	"admin":     {"admin"},
}

// Auth enables SSO through an authenticating proxy (oauth2-proxy, Cloudflare
// Access, Google IAP, ...). The proxy sets an identity header; vigil trusts it
// only from trusted_proxies and maps the email to a role.
type Auth struct {
	Header         string     `yaml:"header"`          // e.g. X-Forwarded-Email, Cf-Access-Authenticated-User-Email
	TrustedProxies []string   `yaml:"trusted_proxies"` // CIDRs the header is accepted from
	Users          []AuthUser `yaml:"users"`
}

type AuthUser struct {
	Email string `yaml:"email"` // exact, or "*@example.com" for a whole domain
	Role  string `yaml:"role"`  // viewer | responder | admin
}

// Keys returns all API keys, including legacy api_tokens (read) and
// admin_tokens (admin).
func (s Server) Keys() []APIKey {
	keys := append([]APIKey(nil), s.APIKeys...)
	for i, t := range s.APITokens {
		keys = append(keys, APIKey{Name: fmt.Sprintf("api-token-%d", i+1), Token: t, Scopes: []string{"read"}})
	}
	for i, t := range s.AdminTokens {
		keys = append(keys, APIKey{Name: fmt.Sprintf("admin-token-%d", i+1), Token: t, Scopes: []string{"admin"}})
	}
	return keys
}

// SMTP is the outgoing mail server for email notifiers and subscribers.
type SMTP struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"` // default 587
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"`     // "vigil <status@example.com>"
	Security string `yaml:"security"` // starttls (default) | tls (implicit, :465) | none
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

	// protocol checks (postgres uses URL; redis/grpc/kafka/smtp use Host)
	Username string `yaml:"username"` // redis ACL user
	Password string `yaml:"password"` // redis AUTH
	Service  string `yaml:"service"`  // grpc health service name ("" = server overall)
	TLS      bool   `yaml:"tls"`      // redis/grpc/kafka/smtp over TLS (smtp: implicit TLS, e.g. :465)

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

	// Escalation names a policy; its first step replaces Notify.
	Escalation string `yaml:"escalation"`

	// Where the check runs: "local" (this server) and/or agent names.
	// With several locations the monitor is DOWN only when at least Quorum
	// of them confirm it (default: majority).
	Locations []string `yaml:"locations"`
	Quorum    int      `yaml:"quorum"`

	Source string `yaml:"-"` // "config", "docker", "kubernetes"
}

// Escalation pages more people the longer an outage stays unacknowledged.
//
//	escalations:
//	  - name: prod
//	    steps:
//	      - notify: [slack]              # at once
//	      - {after: 10m, notify: [pagerduty]}
//	      - {after: 30m, notify: [cto-email]}
//
// Acknowledging the incident (signed link in the alert, admin UI or API)
// stops further steps and reminders.
type Escalation struct {
	Name  string           `yaml:"name"`
	Steps []EscalationStep `yaml:"steps"`
}

type EscalationStep struct {
	After  Duration `yaml:"after"`
	Notify []string `yaml:"notify"`
}

// Policy returns the escalation named n.
func (c *Config) Policy(n string) (Escalation, bool) {
	for _, e := range c.Escalations {
		if e.Name == n {
			return e, true
		}
	}
	return Escalation{}, false
}

// LocalLocation is the vigil server itself.
const LocalLocation = "local"

type Agent struct {
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Notify []string `yaml:"notify"` // who hears "agent offline"; default: defaults.notify
}

// Database selects the store. sqlite (default) lives in data_dir; postgres
// lets several vigil servers share state and fail over (see HA).
type Database struct {
	Driver string `yaml:"driver"` // sqlite | postgres
	URL    string `yaml:"url"`    // postgres://user:pass@host/db?sslmode=require
}

// HA tunes leader election (postgres only). Failover after a crash takes
// about LeaseTTL + LeaseTTL/3; a graceful restart hands over immediately.
type HA struct {
	LeaseTTL Duration `yaml:"lease_ttl"` // default 15s
	// AdvertiseURL is how other nodes reach this one (standbys proxy to the
	// leader). Default: env VIGIL_ADVERTISE_URL, else http://<primary IP>:<port>.
	AdvertiseURL string `yaml:"advertise_url"`
}

type Heartbeat struct {
	URL      string   `yaml:"url"`
	Interval Duration `yaml:"interval"`
}

type Discovery struct {
	Docker     DockerDiscovery `yaml:"docker"`
	Kubernetes KubeDiscovery   `yaml:"kubernetes"`
}

type DockerDiscovery struct {
	Enabled  bool     `yaml:"enabled"`
	Socket   string   `yaml:"socket"` // default /var/run/docker.sock
	Interval Duration `yaml:"interval"`
}

type KubeDiscovery struct {
	Enabled    bool     `yaml:"enabled"`
	Namespaces []string `yaml:"namespaces"` // empty = all (needs cluster-wide RBAC)
	Interval   Duration `yaml:"interval"`
}

type Expect struct {
	Status        []int        `yaml:"status"`        // http; default 200-399
	BodyContains  string       `yaml:"body_contains"` // http
	MaxLatency    Duration     `yaml:"max_latency"`   // over this = DEGRADED
	CertMinDays   int          `yaml:"cert_min_days"` // tls/http(s): under this = DOWN
	ResolvesTo    []string     `yaml:"resolves_to"`   // dns
	JSON          []JSONExpect `yaml:"json"`          // http: assertions on a JSON body
	SkipTLSVerify bool         `yaml:"skip_tls_verify"`
}

// JSONExpect asserts that the value at a dotted path ("data.items.0.status")
// renders equal to Equals ("ok", "200", "true").
type JSONExpect struct {
	Path   string `yaml:"path"`
	Equals string `yaml:"equals"`
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
	if c.Server.SMTP.Port == 0 {
		c.Server.SMTP.Port = 587
	}
	if c.Server.SMTP.Security == "" {
		c.Server.SMTP.Security = "starttls"
	}
	if c.Server.Database.Driver == "" {
		c.Server.Database.Driver = "sqlite"
	}
	if c.Server.HA.LeaseTTL == 0 {
		c.Server.HA.LeaseTTL = Duration(15 * time.Second)
	}
	if c.Server.Heartbeat.URL != "" && c.Server.Heartbeat.Interval == 0 {
		c.Server.Heartbeat.Interval = Duration(time.Minute)
	}
	dd := &c.Discovery
	if dd.Docker.Socket == "" {
		dd.Docker.Socket = "/var/run/docker.sock"
	}
	if dd.Docker.Interval == 0 {
		dd.Docker.Interval = Duration(30 * time.Second)
	}
	if dd.Kubernetes.Interval == 0 {
		dd.Kubernetes.Interval = Duration(30 * time.Second)
	}
	for i := range c.Agents {
		if c.Agents[i].Notify == nil {
			c.Agents[i].Notify = c.Defaults.Notify
		}
	}
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
		c.DefaultMonitor(&c.Monitors[i])
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
		default:
			if err := c.checkNotifier(n); err != nil {
				errs = append(errs, fmt.Errorf("notifier %q: %w", n.Name, err))
			}
		}
		notifiers[n.Name] = true
	}

	agents := map[string]bool{}
	for _, a := range c.Agents {
		p := fmt.Sprintf("agent %q", a.Name)
		switch {
		case a.Name == "" || Slug(a.Name) != a.Name:
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits and dashes", p))
		case a.Name == LocalLocation:
			errs = append(errs, fmt.Errorf("%s: %q is reserved for the server itself", p, LocalLocation))
		case agents[a.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name", p))
		case len(a.Token) < 24:
			errs = append(errs, fmt.Errorf("%s: token of >=24 chars required", p))
		}
		agents[a.Name] = true
		for _, n := range a.Notify {
			if !notifiers[n] {
				errs = append(errs, fmt.Errorf("%s: unknown notifier %q", p, n))
			}
		}
	}
	for i, a := range c.Agents {
		for _, b := range c.Agents[i+1:] {
			if a.Token == b.Token {
				errs = append(errs, fmt.Errorf("agents %q and %q share a token", a.Name, b.Name))
			}
		}
	}
	switch db := c.Server.Database; db.Driver {
	case "sqlite":
		if db.URL != "" {
			errs = append(errs, errors.New("server.database.url: only for postgres (sqlite lives in data_dir)"))
		}
	case "postgres":
		if !strings.HasPrefix(db.URL, "postgres://") && !strings.HasPrefix(db.URL, "postgresql://") {
			errs = append(errs, errors.New("server.database.url: postgres://... URL required"))
		}
	default:
		errs = append(errs, fmt.Errorf("server.database.driver: %q (sqlite or postgres)", db.Driver))
	}
	if ttl := c.Server.HA.LeaseTTL.D(); ttl < 3*time.Second {
		errs = append(errs, errors.New("server.ha.lease_ttl: must be >= 3s"))
	}
	// New api_keys must be strong; legacy api_tokens/admin_tokens keep the
	// rules they shipped with, so upgrading never breaks a working config.
	for _, k := range c.Server.APIKeys {
		p := fmt.Sprintf("api key %q", k.Name)
		switch {
		case k.Name == "":
			errs = append(errs, errors.New("api key: name required"))
		case len(k.Token) < 16:
			errs = append(errs, fmt.Errorf("%s: token of >=16 chars required", p))
		case len(k.Scopes) == 0:
			errs = append(errs, fmt.Errorf("%s: scopes required", p))
		}
	}
	seenKeys := map[string]bool{}
	for _, k := range c.Server.Keys() {
		p := fmt.Sprintf("api key %q", k.Name)
		if k.Token == "" {
			errs = append(errs, fmt.Errorf("%s: empty token", p))
		} else if seenKeys[k.Token] {
			errs = append(errs, fmt.Errorf("%s: token reused by another key", p))
		}
		seenKeys[k.Token] = true
		for _, sc := range k.Scopes {
			if !slices.Contains(Scopes, sc) {
				errs = append(errs, fmt.Errorf("%s: unknown scope %q (have %v)", p, sc, Scopes))
			}
		}
	}
	if a := c.Server.Auth; a.Header != "" || len(a.Users) > 0 {
		if a.Header == "" || len(a.TrustedProxies) == 0 {
			errs = append(errs, errors.New("server.auth: header and trusted_proxies are both required (an untrusted identity header is a login bypass)"))
		}
		for _, cidr := range a.TrustedProxies {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				errs = append(errs, fmt.Errorf("server.auth.trusted_proxies: %q is not a CIDR", cidr))
			}
		}
		for _, u := range a.Users {
			if _, ok := Roles[u.Role]; !ok || u.Email == "" {
				errs = append(errs, fmt.Errorf("server.auth.users: %q needs email and role viewer|responder|admin", u.Email))
			}
		}
	}
	if hb := c.Server.Heartbeat; hb.URL != "" && !isURL(hb.URL) {
		errs = append(errs, errors.New("server.heartbeat.url: invalid url"))
	}

	for i, e := range c.Escalations {
		p := fmt.Sprintf("escalation %q", e.Name)
		if e.Name == "" {
			errs = append(errs, errors.New("escalation: name required"))
		}
		for _, o := range c.Escalations[:i] {
			if o.Name == e.Name {
				errs = append(errs, fmt.Errorf("%s: duplicate name", p))
			}
		}
		if len(e.Steps) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs steps", p))
		}
		for j, st := range e.Steps {
			if j == 0 && st.After != 0 {
				errs = append(errs, fmt.Errorf("%s: the first step fires immediately (no after)", p))
			}
			if j > 0 && st.After <= e.Steps[j-1].After {
				errs = append(errs, fmt.Errorf("%s: step %d: after must increase", p, j+1))
			}
			if len(st.Notify) == 0 {
				errs = append(errs, fmt.Errorf("%s: step %d: notify required", p, j+1))
			}
			for _, n := range st.Notify {
				if !notifiers[n] {
					errs = append(errs, fmt.Errorf("%s: unknown notifier %q", p, n))
				}
			}
		}
	}
	if c.Server.AckSecret != "" && c.Server.PublicURL == "" {
		errs = append(errs, errors.New("server.ack_secret needs server.public_url (the links point there)"))
	}
	if s := c.Server.AckSecret; s != "" && len(s) < 32 {
		errs = append(errs, errors.New("server.ack_secret: use >= 32 random characters"))
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
		errs = append(errs, c.checkMonitor(m, notifiers, agents)...)
		if m.Type == "push" {
			if tokens[m.Token] {
				errs = append(errs, fmt.Errorf("%s: duplicate token", p))
			}
			tokens[m.Token] = true
		}
	}
	return errors.Join(errs...)
}

// DefaultMonitor fills unset fields from defaults (also used for discovered
// monitors, so they behave exactly like configured ones).
func (c *Config) DefaultMonitor(m *Monitor) {
	d := c.Defaults
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
	if pol, ok := c.Policy(m.Escalation); ok && len(pol.Steps) > 0 {
		m.Notify = pol.Steps[0].Notify // the policy decides who hears first
	}
	if m.Type == "http" && m.Method == "" {
		m.Method = "GET"
	}
	if m.Type == "push" && m.Grace == 0 {
		m.Grace = Duration(m.Interval.D() / 2)
	}
	m.ApplyLocationDefaults()
	if m.Source == "" {
		m.Source = "config"
	}
}

func (c *Config) checkMonitor(m Monitor, notifiers, agents map[string]bool) []error {
	p := fmt.Sprintf("monitor %q", m.Name)
	var errs []error
	switch m.Type {
	case "http":
		if !isURL(m.URL) {
			errs = append(errs, fmt.Errorf("%s: valid url required", p))
		}
	case "tcp", "tls":
		if !strings.Contains(m.Host, ":") {
			errs = append(errs, fmt.Errorf("%s: host must be host:port", p))
		}
	case "dns", "icmp":
		if _, _, err := net.SplitHostPort(m.Host); m.Host == "" || err == nil {
			errs = append(errs, fmt.Errorf("%s: host required, without a port", p))
		}
	case "redis", "grpc", "kafka", "smtp":
		if !strings.Contains(m.Host, ":") {
			errs = append(errs, fmt.Errorf("%s: host must be host:port", p))
		}
	case "postgres":
		if !strings.HasPrefix(m.URL, "postgres://") && !strings.HasPrefix(m.URL, "postgresql://") {
			errs = append(errs, fmt.Errorf("%s: url must be postgres://user:pass@host:5432/db", p))
		}
	case "push":
		if len(m.Token) < 16 {
			errs = append(errs, fmt.Errorf("%s: token of >=16 chars required (it is the secret in the ping URL)", p))
		}
	default:
		errs = append(errs, fmt.Errorf("%s: unknown type %q", p, m.Type))
	}
	errs = append(errs, m.validateLocations(agents)...)
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
	if m.Escalation != "" {
		if _, ok := c.Policy(m.Escalation); !ok {
			errs = append(errs, fmt.Errorf("%s: unknown escalation %q", p, m.Escalation))
		}
	}
	return errs
}

// ValidateMonitor checks a single (discovered) monitor against this config.
func (c *Config) ValidateMonitor(m Monitor) error {
	notifiers, agents := map[string]bool{}, map[string]bool{}
	for _, n := range c.Notifiers {
		notifiers[n.Name] = true
	}
	for _, a := range c.Agents {
		agents[a.Name] = true
	}
	if m.Name == "" || m.ID == "" {
		return errors.New("monitor: name required")
	}
	return errors.Join(c.checkMonitor(m, notifiers, agents)...)
}

func (c *Config) checkNotifier(n Notifier) error {
	switch n.Type {
	case "slack", "webhook", "discord", "teams":
		if !isURL(n.URL) {
			return errors.New("valid url required")
		}
	case "email":
		if len(n.To) == 0 {
			return errors.New("to: at least one recipient")
		}
		if c.Server.SMTP.Host == "" {
			return errors.New("email needs server.smtp")
		}
	case "pagerduty":
		if len(n.RoutingKey) < 20 {
			return errors.New("routing_key (Events API v2 integration key) required")
		}
	case "opsgenie":
		if n.APIKey == "" {
			return errors.New("api_key required")
		}
		if n.Region != "" && n.Region != "us" && n.Region != "eu" {
			return errors.New("region must be us or eu")
		}
	case "telegram":
		if n.BotToken == "" || n.ChatID == "" {
			return errors.New("bot_token and chat_id required")
		}
	default:
		return fmt.Errorf("unknown type %q", n.Type)
	}
	return nil
}

// ApplyLocationDefaults fills Locations ([local]) and Quorum (majority).
func (m *Monitor) ApplyLocationDefaults() {
	if len(m.Locations) == 0 {
		m.Locations = []string{LocalLocation}
	}
	if m.Quorum == 0 {
		m.Quorum = len(m.Locations)/2 + 1
	}
}

func (m Monitor) validateLocations(agents map[string]bool) []error {
	p := fmt.Sprintf("monitor %q", m.Name)
	var errs []error
	seen := map[string]bool{}
	for _, l := range m.Locations {
		if l != LocalLocation && !agents[l] {
			errs = append(errs, fmt.Errorf("%s: unknown location %q (use %q or an agent name)", p, l, LocalLocation))
		}
		if seen[l] {
			errs = append(errs, fmt.Errorf("%s: duplicate location %q", p, l))
		}
		seen[l] = true
	}
	if m.Type == "push" && (len(m.Locations) != 1 || m.Locations[0] != LocalLocation) {
		errs = append(errs, fmt.Errorf("%s: push monitors are received by the server; locations must be [local]", p))
	}
	if m.Quorum < 1 || m.Quorum > len(m.Locations) {
		errs = append(errs, fmt.Errorf("%s: quorum must be between 1 and %d", p, len(m.Locations)))
	}
	return errs
}

// AgentByToken returns the agent owning token, if any.
func (c *Config) AgentByToken(token string) (Agent, bool) {
	for _, a := range c.Agents {
		if subtleEq(a.Token, token) {
			return a, true
		}
	}
	return Agent{}, false
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func subtleEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
