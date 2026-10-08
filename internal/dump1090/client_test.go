package dump1090

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFixtureServer serves the three decoder documents from an in-memory
// map keyed by path, plus any extra routes the test registers.
func newFixtureServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
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

func TestFetchDecodesAllThreeDocuments(t *testing.T) {
	srv := newFixtureServer(t, map[string]string{
		"/data/receiver.json": `{"version":"readsb 3.14.1","refresh":1,"lat":48.2,"lon":11.5}`,
		"/data/stats.json":    `{"total":{"local":{"blocks_dropped":3},"cpu":{"demod":10},"tracks":{"all":5}}}`,
		"/data/aircraft.json": `{"now":1759470000,"messages":42000,"aircraft":[{"hex":"abc123"}]}`,
	})

	c := NewClient(srv.URL+"/", 2*time.Second)
	doc, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.Receiver.Version != "readsb 3.14.1" {
		t.Errorf("Receiver.Version = %q", doc.Receiver.Version)
	}
	if doc.Stats.Total == nil || doc.Stats.Total.Local == nil ||
		doc.Stats.Total.Local.BlocksDropped == nil || *doc.Stats.Total.Local.BlocksDropped != 3 {
		t.Error("stats not decoded")
	}
	if doc.Aircraft.Messages != 42000 || len(doc.Aircraft.Aircraft) != 1 {
		t.Errorf("aircraft doc = %+v", doc.Aircraft)
	}
	// Trailing slash in the flag value must not produce double slashes.
	if c.BaseURL() != strings.TrimRight(srv.URL, "/") {
		t.Errorf("BaseURL() = %q, want %q", c.BaseURL(), srv.URL)
	}
}

func TestFetchAbortsOnFirstFailure(t *testing.T) {
	// stats.json is broken; the scrape must fail rather than return a
	// Document with receiver data but no stats.
	srv := newFixtureServer(t, map[string]string{
		"/data/receiver.json": `{"version":"readsb"}`,
		"/data/stats.json":    `{not json`,
		"/data/aircraft.json": `{"now":1,"messages":1,"aircraft":[]}`,
	})

	c := NewClient(srv.URL, 2*time.Second)
	doc, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch succeeded, want error")
	}
	if !strings.Contains(err.Error(), "stats.json") {
		t.Errorf("error %q should name stats.json", err)
	}
	if doc.Receiver.Version != "" || doc.Aircraft.Messages != 0 {
		t.Error("partial Document must be zero on failure")
	}
}

func TestFetchRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "go away", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, 2*time.Second)
	_, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want mention of 503", err)
	}
}

func TestFetchSurfacesConnectionErrors(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", 250*time.Millisecond)
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch against closed port succeeded, want error")
	}
	if !strings.Contains(err.Error(), "receiver.json") {
		t.Errorf("error %q should name the failing document", err)
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-time.After(5 * time.Second)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := NewClient(srv.URL, 0) // no client timeout; context alone must stop it
	if _, err := c.Fetch(ctx); err == nil {
		t.Fatal("Fetch succeeded after context deadline, want error")
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	huge := `{"now":1,"messages":1,"aircraft":[` + strings.Repeat(`{"hex":"aaaaaa"},`, 1<<21) + `{}]}`
	srv := newFixtureServer(t, map[string]string{
		"/data/receiver.json": `{}`,
		"/data/stats.json":    `{}`,
		"/data/aircraft.json": huge,
	})
	c := NewClient(srv.URL, 10*time.Second)
	_, err := c.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want size cap message", err)
	}
}

func TestNewClientNormalisesURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "trailing slash", in: "http://localhost:8080/", want: "http://localhost:8080"},
		{name: "many trailing slashes", in: "http://decoder///", want: "http://decoder"},
		{name: "surrounding space", in: "  http://decoder  ", want: "http://decoder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewClient(tt.in, time.Second).BaseURL(); got != tt.want {
				t.Fatalf("BaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
