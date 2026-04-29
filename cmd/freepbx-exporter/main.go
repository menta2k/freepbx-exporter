// Command freepbx-exporter exposes Asterisk/FreePBX metrics over HTTP for
// Prometheus to scrape. It reads from the AMI (Asterisk Manager Interface).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/menta2k/freepbx-exporter/internal/ami"
	"github.com/menta2k/freepbx-exporter/internal/collector"
	"github.com/menta2k/freepbx-exporter/internal/config"
	"github.com/menta2k/freepbx-exporter/internal/rtcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	logger.Info("starting freepbx-exporter",
		"version", buildVersion(),
		"config", fmt.Sprintf("%+v", cfg.Redact()),
	)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	c := collector.New(
		ami.Config{
			Address:  cfg.AMIAddress,
			Username: cfg.AMIUsername,
			Secret:   cfg.AMISecret,
			Timeout:  cfg.AMITimeout,
		},
		collector.ScrapeOptions{
			DisableSIP:    cfg.DisableSIP,
			DisablePJSIP:  cfg.DisablePJSIP,
			DisableQueues: cfg.DisableQueues,
		},
		logger,
		ami.DefaultDialer{},
	)
	reg.MustRegister(c)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.EnableEvents {
		agg := rtcp.New(rtcp.Options{PerChannel: cfg.RTCPPerChannel})
		reg.MustRegister(agg)

		stream := ami.NewEventStream(
			ami.Config{
				Address:      cfg.AMIAddress,
				Username:     cfg.AMIUsername,
				Secret:       cfg.AMISecret,
				Timeout:      cfg.AMITimeout,
				EventClasses: "call,reporting",
			},
			agg,
			ami.EventStreamHooks{
				OnUp:        agg.SetStreamUp,
				OnReconnect: agg.IncReconnects,
			},
			logger,
			ami.DefaultDialer{},
		)
		go func() {
			if err := stream.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("event stream stopped", "err", err)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry:          reg,
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, indexHTML, cfg.MetricsPath)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddress, "metrics", cfg.MetricsPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

const indexHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>FreePBX Exporter</title></head>
<body><h1>FreePBX Exporter</h1>
<p><a href="%s">Metrics</a> | <a href="/healthz">Health</a></p>
</body></html>
`

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(cfg.LogFormat) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// buildVersion returns the module version baked in by `go build` or "dev"
// when running from source.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
