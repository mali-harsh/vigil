// Command vigil is a self-hosted uptime monitor and status page.
//
//	vigil -config vigil.yaml           run the server
//	vigil -config vigil.yaml -check    validate config and exit
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

	"github.com/mali-harsh/vigil/internal/api"
	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
	"github.com/mali-harsh/vigil/internal/engine"
	"github.com/mali-harsh/vigil/internal/notify"
	"github.com/mali-harsh/vigil/internal/scheduler"
	"github.com/mali-harsh/vigil/internal/statuspage"
	"github.com/mali-harsh/vigil/internal/store"
	"golang.org/x/crypto/acme/autocert"
)

var version = "dev"

func main() {
	path := flag.String("config", envOr("VIGIL_CONFIG", "vigil.yaml"), "config file")
	onlyCheck := flag.Bool("check", false, "validate config and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error:\n%v\n", err)
		os.Exit(2)
	}
	if *onlyCheck {
		fmt.Printf("ok: %d monitors, %d notifiers\n", len(cfg.Monitors), len(cfg.Notifiers))
		return
	}
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
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

	hc := &http.Client{Timeout: 15 * time.Second}
	senders := map[string]notify.Sender{}
	for _, n := range cfg.Notifiers {
		senders[n.Name] = notify.NewSender(n, hc)
	}
	disp := notify.NewDispatcher(senders, log)
	dispCtx, stopDisp := context.WithCancel(context.Background())
	defer stopDisp()
	dispDone := make(chan struct{})
	go func() { disp.Run(dispCtx, 4); close(dispDone) }()

	eng, err := engine.New(ctx, cfg, st, disp, log)
	if err != nil {
		return err
	}
	results := make(chan check.Result, 1024)
	sched := scheduler.New(results)
	for _, m := range cfg.Monitors {
		if err := sched.Start(ctx, m); err != nil {
			return fmt.Errorf("monitor %s: %w", m.ID, err)
		}
	}
	engDone := make(chan struct{})
	go func() { eng.Run(ctx, results); close(engDone) }()

	handler := (&api.Server{Cfg: cfg, Engine: eng, Store: st, Beater: sched, Log: log}).Handler()
	if dir := cfg.StatusPage.ExportDir; dir != "" {
		go statuspage.Export(ctx, &statuspage.Builder{Cfg: cfg, Engine: eng, Store: st}, dir, time.Minute, log)
	}

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
	log.Info("vigil started", "version", version, "listen", cfg.Server.Listen, "monitors", len(cfg.Monitors))

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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
