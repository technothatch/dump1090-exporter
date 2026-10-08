package dump1090

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxJSONSize caps how much of a response body is read. aircraft.json on a
// busy receiver is a few hundred kilobytes; the cap protects against a
// broken or hostile server streaming forever.
const maxJSONSize = 32 << 20 // 32 MiB

// Client fetches the JSON documents of one decoder. It is safe for
// sequential use from a single goroutine; concurrent scrapes should be
// serialised by the caller.
type Client struct {
	// baseURL is the decoder origin, e.g. "http://localhost:8080".
	baseURL string
	// http does the actual transfers.
	http *http.Client
}

// NewClient builds a Client for the decoder at baseURL (scheme://host[:port],
// no path) with the given request timeout. A zero timeout means no timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// BaseURL returns the normalised origin the client talks to.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Document is the result of one scrape: all three JSON documents decoded
// together. A Document is only meaningful when the accompanying error is nil.
type Document struct {
	// Receiver is receiver.json.
	Receiver Receiver
	// Stats is stats.json.
	Stats Stats
	// Aircraft is aircraft.json.
	Aircraft AircraftDoc
}

// Fetch retrieves receiver.json, stats.json and aircraft.json in order. The
// first failure aborts the scrape: a partial Document is never returned, so
// callers cannot mistake stale data for a fresh view.
func (c *Client) Fetch(ctx context.Context) (Document, error) {
	var doc Document
	if err := c.getJSON(ctx, "/data/receiver.json", &doc.Receiver); err != nil {
		return Document{}, err
	}
	if err := c.getJSON(ctx, "/data/stats.json", &doc.Stats); err != nil {
		return Document{}, err
	}
	if err := c.getJSON(ctx, "/data/aircraft.json", &doc.Aircraft); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// getJSON GETs path relative to the decoder origin and decodes the response
// body into v. Non-2xx responses, oversized bodies and malformed JSON all
// surface as errors wrapped with the URL that caused them.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("dump1090: build request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dump1090-exporter/0.1")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dump1090: fetch %s: %w", url, err)
	}
	defer func() {
		// The body is fully consumed or abandoned below; closing is what
		// returns the connection to the pool.
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dump1090: fetch %s: unexpected status %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONSize+1))
	if err != nil {
		return fmt.Errorf("dump1090: read %s: %w", url, err)
	}
	if len(body) > maxJSONSize {
		return fmt.Errorf("dump1090: read %s: body larger than %d bytes", url, maxJSONSize)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("dump1090: decode %s: %w", url, err)
	}
	return nil
}
