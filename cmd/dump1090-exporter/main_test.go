package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/technothatch/dump1090-exporter/internal/dump1090"
	"github.com/technothatch/dump1090-exporter/internal/exporter"
)

// newDecoderFixture stands in for a dump1090/readsb HTTP server by serving
// the three JSON documents the client fetches, under /data/.
func newDecoderFixture(t *testing.T) *httptest.Server {
	t.Helper()
	routes := map[string]string{
		"/data/receiver.json": `{"version": "readsb 4.0.0", "refresh": 1, "lat": 48.208, "lon": 11.512}`,
		"/data/stats.json":    `{"total": {"local": {"blocks_dropped": 7}, "cpu": {"demod": 10.5}, "tracks": {"all": 12, "single_message": 2}}}`,
		"/data/aircraft.json": `{"now": 1759470000.5, "messages": 123456, "aircraft": [
			{"hex": "3c673a", "flight": "BAW117  ", "alt_baro": 34000, "squawk": "7700", "lat": 51.47, "lon": -0.45, "rssi": -3.2},
			{"hex": "4ca1b2", "alt_baro": "ground", "squawk": "1200", "rssi": -21.5},
			{"hex": "abc123", "alt_baro": 1200, "rssi": -12.4}
		]}`,
	}
	mux := http.NewServeMux()
	for path, body := range routes {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestApp wires an app to the fixture decoder with a silent logger.
func newTestApp(t *testing.T, decoderURL string) *app {
	t.Helper()
	e, err := exporter.New(exporter.Options{})
	if err != nil {
		t.Fatalf("exporter.New: %v", err)
	}
	return &app{
		client:   dump1090.NewClient(decoderURL, 2*time.Second),
		exporter: e,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval: time.Minute,
	}
}

func TestResolveBaseURL(t *testing.T) {
	tests := []struct {
		name string
		flag string
		env  string
		want string
	}{
		{name: "explicit flag wins over environment", flag: "http://flag:8080", env: "http://env:9090", want: "http://flag:8080"},
		{name: "flag with trailing slashes is trimmed", flag: "http://decoder:8080//", want: "http://decoder:8080"},
		{name: "environment is used when the flag is blank", flag: "", env: "http://env:9090/", want: "http://env:9090"},
		{name: "whitespace-only flag falls through to environment", flag: "   ", env: "http://env:9090", want: "http://env:9090"},
		{name: "localhost default", flag: "", env: "", want: defaultURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envURL, tt.env)
			if got := resolveBaseURL(tt.flag); got != tt.want {
				t.Fatalf("resolveBaseURL(%q) = %q, want %q", tt.flag, got, tt.want)
			}
		})
	}
}

func TestRunRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "zero scrape interval", args: []string{"-scrape-interval=0"}},
		{name: "negative scrape interval", args: []string{"-scrape-interval=-1s"}},
		{name: "telemetry path without leading slash", args: []string{"-web.telemetry-path=metrics"}},
		{name: "unknown flag", args: []string{"-no-such-flag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(context.Background(), tt.args); got != 2 {
				t.Fatalf("run(%v) = %d, want exit code 2", tt.args, got)
			}
		})
	}
}

// TestHandlerServesMetricsOnDemand exercises the full HTTP surface: the first
// /metrics fetch must scrape the decoder on demand (no background loop is
// running here) and answer with live values.
func TestHandlerServesMetricsOnDemand(t *testing.T) {
	decoder := newDecoderFixture(t)
	a := newTestApp(t, decoder.URL)
	h := newHandler(a, "/metrics")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	health, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	healthBody, _ := io.ReadAll(health.Body)
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK || string(healthBody) != "ok\n" {
		t.Errorf("/healthz = %d %q, want 200 %q", health.StatusCode, healthBody, "ok\n")
	}

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	body := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		"dump1090_up 1",
		"dump1090_aircraft 3",
		"dump1090_aircraft_with_position 1",
		"dump1090_messages_total 123456",
		"dump1090_blocks_dropped_total 7",
		`dump1090_cpu_milliseconds{stage="demod"} 10.5`,
		`dump1090_aircraft_squawk{squawk="7700"} 1`,
		`dump1090_receiver_info{version="readsb 4.0.0"} 1`,
		"dump1090_tracks_all 12",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q", want)
		}
	}

	// A second fetch within the scrape interval must still answer 200.
	again, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("second GET /metrics: %v", err)
	}
	_, _ = io.ReadAll(again.Body)
	_ = again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Errorf("second /metrics status = %d, want 200", again.StatusCode)
	}
}

// TestHandlerReportsUnreachableDecoder checks that a dead decoder still yields
// a valid exposition with dump1090_up 0 rather than an error page.
func TestHandlerReportsUnreachableDecoder(t *testing.T) {
	decoder := newDecoderFixture(t)
	a := newTestApp(t, decoder.URL)
	h := newHandler(a, "/metrics")

	decoder.Close()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "dump1090_up 0") {
		t.Errorf("body missing \"dump1090_up 0\":\n%s", body)
	}
}

// freePort asks the kernel for a currently unused TCP port so run() can bind
// without colliding with anything else on the machine.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// TestRunServesUntilContextCancelled drives the real entry point end to end:
// flags are parsed, the decoder is scraped, /metrics answers with live data,
// and cancelling the context — the same path SIGINT takes in main — makes run
// shut the server down and return exit code 0.
func TestRunServesUntilContextCancelled(t *testing.T) {
	decoder := newDecoderFixture(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- run(ctx, []string{
			"-web.listen-address", addr,
			"-web.telemetry-path", "/metrics",
			"-url", decoder.URL,
			"-scrape-interval", "100ms",
		})
	}()

	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			// Read the body before closing: an unread body makes the
			// transport drop the connection mid-handshake, which can
			// leave a dangling connection that graceful shutdown
			// waits on.
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("exporter did not start serving on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "dump1090_up 1") {
		t.Errorf("/metrics body missing \"dump1090_up 1\":\n%s", body)
	}

	cancel()
	select {
	case got := <-codeCh:
		if got != 0 {
			t.Errorf("run exit code = %d, want 0 after graceful shutdown", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down after the context was cancelled")
	}
}
