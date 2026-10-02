// Command vigil is a self-hosted uptime monitor and status page.
//
//	vigil -config vigil.yaml                     run the server
//	vigil -config vigil.yaml -check              validate config and exit
//	vigil agent -server URL -token TOKEN         run a remote agent
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // distroless images ship no zoneinfo

	"github.com/mali-harsh/vigil/internal/agent"
	"github.com/mali-harsh/vigil/internal/api"
	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/discovery"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/ha"
	"github.com/mali-harsh/vigil/internal/metrics"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/scheduler"
	"github.com/mali-harsh/vigil/internal/statuspage"
	"github.com/mali-harsh/vigil/internal/store"
	"golang.org/x/crypto/acme/autocert"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(runAgent(os.Args[2:], log))
	}

	path := flag.String("config", envOr("VIGIL_CONFIG", "vigil.yaml"), "config file")
	onlyCheck := flag.Bool("check", false, "validate config and exit")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error:\n%v\n", err)
		os.Exit(2)
	}
	if *onlyCheck {
		fmt.Printf("ok: %d monitors, %d notifiers, %d agents\n", len(cfg.Monitors), len(cfg.Notifiers), len(cfg.Agents))
		return
	}
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runAgent(args []string, log *slog.Logger) int {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	server := fs.String("server", os.Getenv("VIGIL_SERVER"), "vigil server base URL (env VIGIL_SERVER)")
	token := fs.String("token", "", "agent token (prefer env VIGIL_AGENT_TOKEN: flags show up in ps)")
	fs.Parse(args)
	if *token == "" {
		*token = os.Getenv("VIGIL_AGENT_TOKEN")
	}
	if *server == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "usage: vigil agent -server https://status.example.com   (token via VIGIL_AGENT_TOKEN)")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a := &agent.Agent{Server: *server, Token: *token, Log: log.With("component", "agent", "version", version)}
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		return 1
	}
	return 0
}

func run(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.Server.DataDir, 0o750); err != nil {
		return err
	}
	var st *store.Store
	var err error
	if cfg.Server.Database.Driver == "postgres" {
		st, err = store.OpenPostgres(ctx, cfg.Server.Database.URL, cfg.StatusPage.Location)
	} else {
		st, err = store.Open(filepath.Join(cfg.Server.DataDir, "vigil.db"), cfg.StatusPage.Location)
	}
	if err != nil {
		return err
	}
	defer st.Close()

	// Shared across leadership terms: metrics, notification delivery, HTTP.
	reg := metrics.New()
	hc := &http.Client{Timeout: 15 * time.Second}
	senders := map[string]notify.Sender{}
	for _, n := range cfg.Notifiers {
		senders[n.Name] = notify.NewSender(n, hc)
	}
	disp := notify.NewDispatcher(senders, log)
	disp.Metrics = reg
	dispCtx, stopDisp := context.WithCancel(context.Background())
	defer stopDisp()
	dispDone := make(chan struct{})
	go func() { disp.Run(dispCtx, 4); close(dispDone) }()

	node := ha.NodeID()
	advertise := advertiseURL(cfg)
	sw := api.NewSwitch(node, version, func(ctx context.Context) (string, error) {
		if !st.Postgres() {
			return "", nil
		}
		li, err := st.LeaseInfo(ctx, ha.LeaseName)
		if err != nil || li == nil || li.ExpiresIn <= 0 || li.Holder == node {
			return "", err
		}
		return li.Address, nil
	})
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           sw,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	srvErr := make(chan error, 3) // http, redirect, single-node term
	if tls := cfg.Server.TLS; len(tls.Domains) > 0 {
		// Automatic HTTPS: certificates from Let's Encrypt, cached in data_dir.
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(tls.Domains...),
			Cache:      autocert.DirCache(filepath.Join(cfg.Server.DataDir, "certs")),
			Email:      tls.Email,
		}
		srv.Addr, srv.TLSConfig = ":443", m.TLSConfig()
		redirect := &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 5 * time.Second}
		go func() { srvErr <- redirect.ListenAndServe() }()
		defer redirect.Close()
		go func() { srvErr <- srv.ListenAndServeTLS("", "") }()
	} else {
		go func() { srvErr <- srv.ListenAndServe() }()
	}
	log.Info("vigil started", "version", version, "node", node, "listen", srv.Addr, "tls_domains", cfg.Server.TLS.Domains,
		"database", cfg.Server.Database.Driver, "monitors", len(cfg.Monitors), "agents", len(cfg.Agents))

	var termErr error
	lead := func(lctx context.Context) {
		if termErr = term(lctx, cfg, st, disp, reg, sw, node, log); termErr != nil {
			log.Error("leader term failed", "err", termErr)
		}
	}
	electDone := make(chan struct{})
	go func() {
		defer close(electDone)
		if st.Postgres() {
			// several servers may share this database: only the elected one acts
			if len(cfg.Server.TLS.Domains) > 0 {
				log.Warn("server.tls with several nodes: ACME challenges may reach a node without the token — terminate TLS at your load balancer for HA")
			}
			log.Info("high availability: competing for leadership", "node", node, "advertise", advertise, "lease_ttl", cfg.Server.HA.LeaseTTL.D())
			(&ha.Elector{Store: st, ID: node, Address: advertise, TTL: cfg.Server.HA.LeaseTTL.D(), Log: log}).Run(ctx, lead)
		} else {
			lead(ctx)             // sqlite: single node, always the leader
			if ctx.Err() == nil { // nobody else will take over: exit so the supervisor restarts us
				srvErr <- fmt.Errorf("engine stopped: %v", termErr)
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-electDone
			return err
		}
	}
	log.Info("shutting down")
	<-electDone // term ends, lease released → a standby takes over at once
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shCtx)
	// let queued alerts go out, but never hang shutdown on a dead channel
	disp.Close()
	select {
	case <-dispDone:
	case <-time.After(10 * time.Second):
		log.Warn("pending notifications abandoned at shutdown")
	}
	return nil
}

// term runs everything only the leader does — probes, engine, discovery,
// dead-man heartbeat, static export — and serves the full app until lctx ends.
// A new term rebuilds state from the database, exactly like a restart.
func term(lctx context.Context, cfg *config.Config, st *store.Store, disp *notify.Dispatcher, reg *metrics.Registry, sw *api.Switch, node string, log *slog.Logger) error {
	results := make(chan check.Result, 1024)
	sched := scheduler.New(results)
	defer sched.Wait()
	eng, err := engine.New(lctx, cfg, st, disp, sched, reg, log) // starts every local monitor
	if err != nil {
		return err
	}
	engDone := make(chan struct{})
	go func() { eng.Run(lctx, results); close(engDone) }()
	defer func() { <-engDone }()

	if d := cfg.Discovery.Docker; d.Enabled {
		go discovery.Run(lctx, discovery.NewDocker(d.Socket, log), d.Interval.D(), cfg, eng, log)
	}
	if k := cfg.Discovery.Kubernetes; k.Enabled {
		p, err := discovery.NewKubernetes(k.Namespaces, log)
		if err != nil {
			return fmt.Errorf("kubernetes discovery: %w", err)
		}
		go discovery.Run(lctx, p, k.Interval.D(), cfg, eng, log)
	}
	if hb := cfg.Server.Heartbeat; hb.URL != "" {
		go heartbeat(lctx, hb.URL, hb.Interval.D(), eng, reg, log)
	}
	if dir := cfg.StatusPage.ExportDir; dir != "" {
		go statuspage.Export(lctx, &statuspage.Builder{Cfg: cfg, Engine: eng, Store: st}, dir, time.Minute, log)
	}

	sw.Set((&api.Server{Cfg: cfg, Engine: eng, Store: st, Beater: sched, Results: results, Metrics: reg, Version: version, Node: node, Log: log}).Handler())
	log.Info("serving as active node", "node", node)
	<-lctx.Done()
	sw.Set(nil) // back to standby before anything else stops
	return nil
}

// heartbeat is the dead-man's switch: while the engine is healthy, ping an
// external URL (healthchecks.io, another vigil's /push/<token>, ...). If vigil
// dies, hangs or loses its database, the pings stop and the OTHER side alerts.
func heartbeat(ctx context.Context, url string, every time.Duration, eng *engine.Engine, reg *metrics.Registry, log *slog.Logger) {
	c := &http.Client{Timeout: 10 * time.Second}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		result := "skipped_unhealthy"
		if err := eng.Healthy(); err != nil {
			log.Error("not sending heartbeat: unhealthy", "err", err)
		} else if req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil); err == nil {
			resp, err := c.Do(req)
			switch {
			case err != nil:
				result = "failed"
				log.Warn("heartbeat ping failed", "err", err)
			case resp.StatusCode >= 300:
				resp.Body.Close()
				result = "failed"
				log.Warn("heartbeat ping rejected", "status", resp.StatusCode)
			default:
				resp.Body.Close()
				result = "sent"
			}
		}
		reg.Inc("vigil_heartbeat_pings_total", metrics.L{"result": result})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// advertiseURL is how other nodes reach this one.
func advertiseURL(cfg *config.Config) string {
	if u := cfg.Server.HA.AdvertiseURL; u != "" {
		return u
	}
	if u := os.Getenv("VIGIL_ADVERTISE_URL"); u != "" {
		return u
	}
	host, port, _ := net.SplitHostPort(cfg.Server.Listen)
	if len(cfg.Server.TLS.Domains) > 0 {
		port = "443"
	}
	ip := host // bound to a specific address: that's the only one that works
	if ip == "" || ip == "0.0.0.0" || ip == "::" {
		ip = "127.0.0.1"
		// UDP "dial" sends nothing; it just picks the interface the OS would route on.
		if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
			ip = c.LocalAddr().(*net.UDPAddr).IP.String()
			c.Close()
		}
	}
	scheme := "http"
	if port == "443" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(ip, port)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
