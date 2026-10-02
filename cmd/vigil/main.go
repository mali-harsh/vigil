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
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "vigil.db"), cfg.StatusPage.Location)
	if err != nil {
		return err
	}
	defer st.Close()

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

	results := make(chan check.Result, 1024)
	sched := scheduler.New(results)
	eng, err := engine.New(ctx, cfg, st, disp, sched, reg, log) // starts every local monitor
	if err != nil {
		return err
	}
	engDone := make(chan struct{})
	go func() { eng.Run(ctx, results); close(engDone) }()

	if d := cfg.Discovery.Docker; d.Enabled {
		go discovery.Run(ctx, discovery.NewDocker(d.Socket, log), d.Interval.D(), cfg, eng, log)
	}
	if k := cfg.Discovery.Kubernetes; k.Enabled {
		p, err := discovery.NewKubernetes(k.Namespaces, log)
		if err != nil {
			return fmt.Errorf("kubernetes discovery: %w", err)
		}
		go discovery.Run(ctx, p, k.Interval.D(), cfg, eng, log)
	}
	if hb := cfg.Server.Heartbeat; hb.URL != "" {
		go heartbeat(ctx, hb.URL, hb.Interval.D(), eng, reg, log)
	}
	if dir := cfg.StatusPage.ExportDir; dir != "" {
		go statuspage.Export(ctx, &statuspage.Builder{Cfg: cfg, Engine: eng, Store: st}, dir, time.Minute, log)
	}

	handler := (&api.Server{Cfg: cfg, Engine: eng, Store: st, Beater: sched, Results: results, Metrics: reg, Version: version, Log: log}).Handler()
	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	srvErr := make(chan error, 2)
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
	log.Info("vigil started", "version", version, "listen", srv.Addr, "tls_domains", cfg.Server.TLS.Domains, "monitors", len(cfg.Monitors), "agents", len(cfg.Agents))

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shCtx)
	sched.Wait()
	<-engDone
	// let queued alerts go out, but never hang shutdown on a dead channel
	disp.Close()
	select {
	case <-dispDone:
	case <-time.After(10 * time.Second):
		log.Warn("pending notifications abandoned at shutdown")
	}
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
