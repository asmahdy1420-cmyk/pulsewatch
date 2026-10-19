// Command pulsewatch is a self-hosted uptime monitor with a status page.
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
	"syscall"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/config"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/monitor"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/notify"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/scheduler"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/server"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pulsewatch:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "pulsewatch.yaml", "path to the YAML config file")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("pulsewatch", version)
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid -log-level: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	clk := clock.Real{}

	var st store.Store
	if cfg.Database == ":memory:" {
		st = store.NewMemory()
	} else {
		sq, err := store.OpenSQLite(ctx, cfg.Database)
		if err != nil {
			return err
		}
		st = sq
	}
	defer st.Close()

	var notifiers notify.Multi
	for _, n := range cfg.Notifiers {
		switch n.Type {
		case "webhook":
			notifiers = append(notifiers, notify.NewWebhook(n.URL, n.Secret, clk))
		case "slack":
			notifiers = append(notifiers, notify.NewSlack(n.URL))
		}
	}

	specs := cfg.Specs()
	mon := monitor.New(specs, st, notifiers, clk, log)
	if err := mon.Restore(ctx); err != nil {
		return err
	}

	jobs := make([]scheduler.Job, 0, len(specs))
	for _, spec := range specs {
		c, err := check.New(spec, clk)
		if err != nil {
			return err
		}
		jobs = append(jobs, scheduler.Job{
			ID:       spec.ID,
			Interval: spec.Interval,
			Jitter:   spec.Jitter,
			Timeout:  spec.Timeout,
			Run:      func(ctx context.Context) { mon.Run(ctx, spec.ID, c) },
		})
	}

	srv, err := server.New(mon, st, clk, cfg.Page.Title, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		scheduler.New(cfg.Workers, log).Run(ctx, jobs)
	}()
	go prune(ctx, st, time.Duration(cfg.Retention), clk, log)

	srvErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "checks", len(specs), "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-srvErr:
		stop()
		<-schedDone
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	select {
	case <-schedDone:
	case <-shutdownCtx.Done():
		log.Warn("checks still running at shutdown deadline")
	}
	log.Info("stopped")
	return nil
}

// prune deletes results older than the retention window once an hour.
func prune(ctx context.Context, st store.Store, retention time.Duration, clk clock.Clock, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := st.Prune(ctx, clk.Now().Add(-retention))
		if err != nil && ctx.Err() == nil {
			log.Warn("prune results", "err", err)
		} else if n > 0 {
			log.Info("pruned results", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
