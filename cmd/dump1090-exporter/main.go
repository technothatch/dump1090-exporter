// Command dump1090-exporter serves Prometheus metrics for a dump1090-fa or
// readsb ADS-B decoder.
//
// It polls receiver.json, stats.json and aircraft.json in the background at
// a fixed interval and exposes them at --web.telemetry-path. When a scrape
// request arrives after a missed background poll (exporter restart, decoder
// outage), the handler performs an on-demand scrape first so the very first
// /metrics fetch already carries live data.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/technothatch/dump1090-exporter/internal/dump1090"
	"github.com/technothatch/dump1090-exporter/internal/exporter"
)

const (
	// defaultURL is the dump1090/readsb default HTTP server.
	defaultURL = "http://localhost:8080"
	// envURL overrides the decoder URL when --url is not given.
	envURL = "DUMP1090_URL"
)

// resolveBaseURL implements the URL precedence: explicit flag, then the
// DUMP1090_URL environment variable, then the localhost default. Trailing
// slashes are stripped so path joining is predictable.
func resolveBaseURL(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv(envURL)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultURL
}

// app owns the scrape loop and the HTTP handler wiring.
type app struct {
	client   *dump1090.Client
	exporter *exporter.Exporter
	logger   *slog.Logger
	interval time.Duration

	// mu guards lastAttempt so concurrent /metrics scrapes do not stampede
	// the decoder with parallel on-demand fetches.
	mu          sync.Mutex
	lastAttempt time.Time
}

// scrape performs one full scrape. Failures only log and flip dump1090_up
// to 0: the exporter must keep serving whatever it last knew.
func (a *app) scrape(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastAttempt = time.Now()

	start := time.Now()
	doc, err := a.client.Fetch(ctx)
	duration := time.Since(start)
	if err != nil {
		a.exporter.MarkUnreachable(duration)
		a.logger.Warn("scrape failed", "url", a.client.BaseURL(), "error", err)
		return
	}
	a.exporter.Update(doc, duration, time.Now())
}

// maybeScrape runs a scrape only if the background loop has not produced one
// within the configured interval — the on-demand fallback.
func (a *app) maybeScrape(ctx context.Context) {
	a.mu.Lock()
	stale := time.Since(a.lastAttempt) >= a.interval
	a.mu.Unlock()
	if stale {
		a.scrape(ctx)
	}
}

// newHandler builds the HTTP surface of the exporter: the telemetry path
// (with an on-demand scrape in front of the promhttp handler) plus a liveness
// probe at /healthz. Keeping it separate from run makes the wiring testable
// without binding a socket.
func newHandler(a *app, telemetryPath string) http.Handler {
	mux := http.NewServeMux()
	ph := promhttp.HandlerFor(a.exporter.Registry(), promhttp.HandlerOpts{})
	mux.Handle(telemetryPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.maybeScrape(r.Context())
		ph.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// run parses flags, starts the poll loop and serves /metrics until ctx is
// cancelled (SIGINT or SIGTERM, wired up in main), then shuts the HTTP
// server down gracefully. It returns the exit code for main.
func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dump1090-exporter", flag.ContinueOnError)
	listen := fs.String("web.listen-address", ":9702", "address to serve /metrics on")
	telemetryPath := fs.String("web.telemetry-path", "/metrics", "path under which to expose metrics")
	interval := fs.Duration("scrape-interval", 5*time.Second, "background scrape interval")
	timeout := fs.Duration("timeout", 5*time.Second, "HTTP timeout per decoder request")
	baseURL := fs.String("url", "", "decoder base URL (default $DUMP1090_URL or http://localhost:8080)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *interval <= 0 {
		slog.Error("scrape-interval must be positive", "value", *interval)
		return 2
	}
	if !strings.HasPrefix(*telemetryPath, "/") {
		slog.Error("web.telemetry-path must start with /", "value", *telemetryPath)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	exp, err := exporter.New(exporter.Options{})
	if err != nil {
		logger.Error("build exporter", "error", err)
		return 1
	}
	a := &app{
		client:   dump1090.NewClient(resolveBaseURL(*baseURL), *timeout),
		exporter: exp,
		logger:   logger,
		interval: *interval,
	}

	// Prime metrics before the first request so /metrics never answers with
	// an empty registry.
	a.scrape(ctx)

	srv := &http.Server{Addr: *listen, Handler: newHandler(a, *telemetryPath), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.scrape(ctx)
			}
		}
	}()

	logger.Info("serving",
		"address", *listen,
		"path", *telemetryPath,
		"decoder", a.client.BaseURL(),
		"interval", *interval,
	)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// A connection outlived the grace period. Drop it rather
			// than hang; the signal was handled, so this is not an
			// error the operator has to act on.
			logger.Warn("graceful shutdown timed out, forcing close", "error", err)
			if err := srv.Close(); err != nil {
				logger.Error("force close", "error", err)
				return 1
			}
		}
		return 0
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		logger.Error("serve", "address", *listen, "error", err)
		return 1
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
