package exporter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/technothatch/dump1090-exporter/internal/dump1090"
)

// The fixtures below are trimmed but realistic renderings of the three
// documents dump1090-fa and readsb publish under /data/. They deliberately
// contain keys the exporter never reads (modeac, remote, accepted, signal,
// future windows) — those must be tolerated, not rejected.
const (
	receiverFixture = `{
		"version": "readsb 4.0.0",
		"refresh": 1,
		"lat": 48.208,
		"lon": 11.512,
		"altitude": 563,
		"messages": 1234567,
		"accepted": [1, 2, 3]
	}`
	statsFixture = `{
		"latest": {"start": 1759469995, "end": 1759470000, "local": {"blocks_dropped": 1}, "messages": 12},
		"last1min": {"messages": 700},
		"last5min": {"messages": 3500},
		"last15min": {"messages": 10500},
		"total": {
			"start": 1759460000,
			"end": 1759470000,
			"local": {
				"blocks_processed": 12345678,
				"blocks_dropped": 42,
				"modeac": {"messages": 100},
				"modes": {"messages": 900},
				"bad": 10,
				"unknown_icao": 5,
				"accepted": [2, 3, 4],
				"signal": {"strong": 5, "total": 100},
				"peak_signal": -3.1,
				"strong_signals": 5
			},
			"remote": {"messages": 5000},
			"cpu": {"demod": 12345.6, "reader": 2345.6, "background": 345.6},
			"cpr": {"global_ok": 1000, "global_bad": 3, "global_skipped": 7, "local_ok": 900, "local_bad": 2, "local_skipped": 11},
			"tracks": {"all": 42, "single_message": 3},
			"messages": 1000000
		}
	}`
	aircraftFixture = `{
		"now": 1759470000.5,
		"messages": 1000000,
		"aircraft": [
			{"hex": "3c673a", "type": "A321", "flight": "BAW117  ", "alt_baro": 34000, "alt_geom": 34125, "gs": 452.1, "ias": 448, "tas": 461, "mach": 0.84, "track": 243.5, "squawk": "7700", "emergency": "none", "category": "A3", "lat": 51.47, "lon": -0.45, "nic": 8, "rc": 186, "seen_pos": 0.3, "messages": 1420, "seen": 0.4, "rssi": -3.2, "mlat": [], "tisb": []},
			{"hex": "4ca1b2", "type": "B738", "flight": "RYR834K ", "alt_baro": "ground", "gs": 0, "track": 84.1, "squawk": "1200", "lat": 48.208, "lon": 11.512, "seen_pos": 0.9, "rssi": -21.5},
			{"hex": "abc123", "alt_baro": 1200, "baro_rate": 512, "squawk": "0000", "seen": 5, "rssi": -12.4},
			{"hex": "def456", "flight": "DLH441  ", "alt_baro": 11000, "squawk": "4545", "messages": 90, "seen": 1.2},
			{"hex": "780abc", "seen": 2}
		]
	}`
)

// fixtureDoc decodes the three fixtures into a dump1090.Document exactly the
// way the HTTP client does, so every test below exercises the real JSON path.
func fixtureDoc(t *testing.T) dump1090.Document {
	t.Helper()
	var doc dump1090.Document
	parts := []struct {
		name string
		raw  string
		dst  any
	}{
		{"receiver.json", receiverFixture, &doc.Receiver},
		{"stats.json", statsFixture, &doc.Stats},
		{"aircraft.json", aircraftFixture, &doc.Aircraft},
	}
	for _, p := range parts {
		if err := json.Unmarshal([]byte(p.raw), p.dst); err != nil {
			t.Fatalf("decode %s fixture: %v", p.name, err)
		}
	}
	return doc
}

// newTestExporter builds an Exporter with default options, failing the test
// on the (impossible in practice) registration error.
func newTestExporter(t *testing.T) *Exporter {
	t.Helper()
	e, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// exposesMetric reports whether a text exposition contains a sample line for
// name, ignoring HELP/TYPE comment lines and longer names that share the
// prefix (dump1090_aircraft vs. dump1090_aircraft_with_position).
func exposesMetric(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			return true
		}
	}
	return false
}

// TestUpdateFromFixtures checks that realistic receiver.json, stats.json and
// aircraft.json documents populate every metric family with the right values.
func TestUpdateFromFixtures(t *testing.T) {
	e := newTestExporter(t)
	doc := fixtureDoc(t)
	at := time.Unix(1759470000, 0)
	e.Update(doc, 150*time.Millisecond, at)

	tests := []struct {
		name  string
		value func() float64
		want  float64
	}{
		{name: "up", value: func() float64 { return testutil.ToFloat64(e.up) }, want: 1},
		{name: "receiver refresh", value: func() float64 { return testutil.ToFloat64(e.receiverRefresh) }, want: 1},
		{name: "receiver version", value: func() float64 { return testutil.ToFloat64(e.receiverInfo.WithLabelValues("readsb 4.0.0")) }, want: 1},
		{name: "messages total adopts decoder counter", value: func() float64 { return testutil.ToFloat64(e.messagesTotal) }, want: 1000000},
		{name: "messages per second on first scrape", value: func() float64 { return testutil.ToFloat64(e.messagesPerSecond) }, want: 0},
		{name: "aircraft", value: func() float64 { return testutil.ToFloat64(e.aircraft) }, want: 5},
		{name: "aircraft with position", value: func() float64 { return testutil.ToFloat64(e.aircraftWithPosition) }, want: 2},
		{name: "blocks dropped total", value: func() float64 { return testutil.ToFloat64(e.blocksDroppedTotal) }, want: 42},
		{name: "cpu demod", value: func() float64 { return testutil.ToFloat64(e.cpuMilliseconds.WithLabelValues("demod")) }, want: 12345.6},
		{name: "cpu reader", value: func() float64 { return testutil.ToFloat64(e.cpuMilliseconds.WithLabelValues("reader")) }, want: 2345.6},
		{name: "cpu background", value: func() float64 { return testutil.ToFloat64(e.cpuMilliseconds.WithLabelValues("background")) }, want: 345.6},
		{name: "cpr local ok", value: func() float64 { return testutil.ToFloat64(e.cprLocalOK) }, want: 900},
		{name: "cpr local bad", value: func() float64 { return testutil.ToFloat64(e.cprLocalBad) }, want: 2},
		{name: "cpr local skipped", value: func() float64 { return testutil.ToFloat64(e.cprLocalSkipped) }, want: 11},
		{name: "cpr global ok", value: func() float64 { return testutil.ToFloat64(e.cprGlobalOK) }, want: 1000},
		{name: "cpr global bad", value: func() float64 { return testutil.ToFloat64(e.cprGlobalBad) }, want: 3},
		{name: "cpr global skipped", value: func() float64 { return testutil.ToFloat64(e.cprGlobalSkipped) }, want: 7},
		{name: "tracks all", value: func() float64 { return testutil.ToFloat64(e.tracksAll) }, want: 42},
		{name: "tracks single message", value: func() float64 { return testutil.ToFloat64(e.tracksSingleMessage) }, want: 3},
		{name: "scrape duration", value: func() float64 { return testutil.ToFloat64(e.scrapeDuration) }, want: 0.15},
		{name: "squawk 7700", value: func() float64 { return testutil.ToFloat64(e.squawk.WithLabelValues("7700")) }, want: 1},
		{name: "squawk 1200", value: func() float64 { return testutil.ToFloat64(e.squawk.WithLabelValues("1200")) }, want: 1},
		{name: "squawk 0000", value: func() float64 { return testutil.ToFloat64(e.squawk.WithLabelValues("0000")) }, want: 1},
		{name: "squawk 4545", value: func() float64 { return testutil.ToFloat64(e.squawk.WithLabelValues("4545")) }, want: 1},
		{name: "signal quantile 0.5", value: func() float64 { return testutil.ToFloat64(e.signalDBFS.WithLabelValues("0.5")) }, want: -12.4},
		{name: "signal quantile 0.9", value: func() float64 { return testutil.ToFloat64(e.signalDBFS.WithLabelValues("0.9")) }, want: -3.2},
		{name: "signal quantile 0.99", value: func() float64 { return testutil.ToFloat64(e.signalDBFS.WithLabelValues("0.99")) }, want: -3.2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.value(); !almostEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	// Five aircraft carry altitudes (34000, "ground" -> 0, 1200, 11000);
	// the fifth reports none. The histogram is asserted sample by sample.
	wantHistogram := `
# HELP dump1090_aircraft_altitude Barometric altitude of tracked aircraft in feet, observed per aircraft per scrape; alt_baro \"ground\" counts as 0.
# TYPE dump1090_aircraft_altitude histogram
dump1090_aircraft_altitude_bucket{le="0"} 1
dump1090_aircraft_altitude_bucket{le="1000"} 1
dump1090_aircraft_altitude_bucket{le="2500"} 2
dump1090_aircraft_altitude_bucket{le="5000"} 2
dump1090_aircraft_altitude_bucket{le="10000"} 2
dump1090_aircraft_altitude_bucket{le="15000"} 3
dump1090_aircraft_altitude_bucket{le="20000"} 3
dump1090_aircraft_altitude_bucket{le="25000"} 3
dump1090_aircraft_altitude_bucket{le="30000"} 3
dump1090_aircraft_altitude_bucket{le="35000"} 4
dump1090_aircraft_altitude_bucket{le="40000"} 4
dump1090_aircraft_altitude_bucket{le="50000"} 4
dump1090_aircraft_altitude_bucket{le="+Inf"} 4
dump1090_aircraft_altitude_sum 46200
dump1090_aircraft_altitude_count 4
`
	if err := testutil.CollectAndCompare(e.altitude, strings.NewReader(wantHistogram), NameAircraftAltitude); err != nil {
		t.Errorf("altitude histogram mismatch: %v", err)
	}
	if got := e.LastUpdate(); !got.Equal(at) {
		t.Errorf("LastUpdate() = %v, want %v", got, at)
	}
}

// TestUpdateTracksMessageRate verifies the inter-scrape rate and that the
// exported total only ever grows.
func TestUpdateTracksMessageRate(t *testing.T) {
	e := newTestExporter(t)
	doc := fixtureDoc(t)
	at := time.Unix(1759470000, 0)

	steps := []struct {
		name       string
		messages   float64
		elapsed    time.Duration
		wantTotal  float64
		wantPerSec float64
	}{
		{name: "first scrape seeds the counter", messages: 1000000, wantTotal: 1000000, wantPerSec: 0},
		{name: "steady traffic", messages: 1001500, elapsed: 5 * time.Second, wantTotal: 1001500, wantPerSec: 300},
		{name: "quiet interval", messages: 1001500, elapsed: 5 * time.Second, wantTotal: 1001500, wantPerSec: 0},
		{name: "traffic resumes", messages: 1002000, elapsed: 10 * time.Second, wantTotal: 1002000, wantPerSec: 50},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			doc.Aircraft.Messages = tt.messages
			at = at.Add(tt.elapsed)
			e.Update(doc, time.Millisecond, at)
			if got := testutil.ToFloat64(e.messagesTotal); !almostEqual(got, tt.wantTotal) {
				t.Errorf("dump1090_messages_total = %v, want %v", got, tt.wantTotal)
			}
			if got := testutil.ToFloat64(e.messagesPerSecond); !almostEqual(got, tt.wantPerSec) {
				t.Errorf("dump1090_messages_per_second = %v, want %v", got, tt.wantPerSec)
			}
		})
	}
}

// TestUpdateSurvivesCounterResets simulates decoder restarts: every source
// counter falls back toward zero, but the exported Prometheus counters must
// keep their last value and add only the new counter's progress.
func TestUpdateSurvivesCounterResets(t *testing.T) {
	e := newTestExporter(t)
	doc := fixtureDoc(t)
	at := time.Unix(1759470000, 0)

	steps := []struct {
		name         string
		mutate       func(doc *dump1090.Document)
		wantMessages float64
		wantBlocks   float64
		wantCPU      float64
	}{
		{
			name:         "initial values are adopted",
			mutate:       func(*dump1090.Document) {},
			wantMessages: 1000000,
			wantBlocks:   42,
			wantCPU:      12345.6,
		},
		{
			name: "decoder restart counts the new value",
			mutate: func(d *dump1090.Document) {
				d.Aircraft.Messages = 120
				*d.Stats.Total.Local.BlocksDropped = 5
				*d.Stats.Total.CPU.Demod = 50
			},
			wantMessages: 1000120,
			wantBlocks:   47,
			wantCPU:      12395.6,
		},
		{
			name: "restart that lands on zero adds nothing",
			mutate: func(d *dump1090.Document) {
				d.Aircraft.Messages = 0
				*d.Stats.Total.Local.BlocksDropped = 0
				*d.Stats.Total.CPU.Demod = 0
			},
			wantMessages: 1000120,
			wantBlocks:   47,
			wantCPU:      12395.6,
		},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(&doc)
			at = at.Add(5 * time.Second)
			e.Update(doc, time.Millisecond, at)

			if got := testutil.ToFloat64(e.messagesTotal); !almostEqual(got, tt.wantMessages) {
				t.Errorf("dump1090_messages_total = %v, want %v", got, tt.wantMessages)
			}
			if got := testutil.ToFloat64(e.blocksDroppedTotal); !almostEqual(got, tt.wantBlocks) {
				t.Errorf("dump1090_blocks_dropped_total = %v, want %v", got, tt.wantBlocks)
			}
			if got := testutil.ToFloat64(e.cpuMilliseconds.WithLabelValues("demod")); !almostEqual(got, tt.wantCPU) {
				t.Errorf("dump1090_cpu_milliseconds{stage=demod} = %v, want %v", got, tt.wantCPU)
			}
			if got := testutil.ToFloat64(e.up); got != 1 {
				t.Errorf("dump1090_up = %v, want 1", got)
			}
		})
	}
}

// squawkDoc builds a minimal Document whose aircraft carry the given squawks.
func squawkDoc(squawks ...string) dump1090.Document {
	aircraft := make([]dump1090.Aircraft, 0, len(squawks))
	for _, s := range squawks {
		aircraft = append(aircraft, dump1090.Aircraft{Hex: "aaaaaa", Squawk: s})
	}
	return dump1090.Document{Aircraft: dump1090.AircraftDoc{Aircraft: aircraft}}
}

// TestUpdateCountsSquawksWithBoundedCardinality checks that only well-formed
// 4-digit octal squawks become series, that vanished squawks are reported as
// zero, and that garbage labels from the decoder are dropped.
func TestUpdateCountsSquawksWithBoundedCardinality(t *testing.T) {
	e := newTestExporter(t)
	at := time.Unix(1759470000, 0)

	e.Update(squawkDoc("7700", "1200", "", "9", "12345", "abc1"), time.Millisecond, at)
	if got := testutil.CollectAndCount(e.squawk, NameAircraftSquawk); got != 2 {
		t.Fatalf("series count = %d, want 2 (7700 and 1200 only)", got)
	}
	if got := testutil.ToFloat64(e.squawk.WithLabelValues("7700")); got != 1 {
		t.Errorf("squawk 7700 = %v, want 1", got)
	}

	e.Update(squawkDoc("1200", "7500"), time.Millisecond, at.Add(5*time.Second))
	if got := testutil.CollectAndCount(e.squawk, NameAircraftSquawk); got != 3 {
		t.Errorf("series count = %d, want 3", got)
	}
	if got := testutil.ToFloat64(e.squawk.WithLabelValues("7700")); got != 0 {
		t.Errorf("squawk 7700 = %v, want 0 after it vanished", got)
	}
	if got := testutil.ToFloat64(e.squawk.WithLabelValues("7500")); got != 1 {
		t.Errorf("squawk 7500 = %v, want 1", got)
	}
}

// TestUpdateSignalQuantiles covers the RSSI quantile series: present and
// correct with samples, removed entirely once no aircraft reports rssi.
func TestUpdateSignalQuantiles(t *testing.T) {
	e := newTestExporter(t)
	at := time.Unix(1759470000, 0)

	e.Update(fixtureDoc(t), time.Millisecond, at)
	if got := testutil.CollectAndCount(e.signalDBFS, NameSignalDBFS); got != 3 {
		t.Fatalf("quantile series = %d, want 3", got)
	}
	if got := testutil.ToFloat64(e.signalDBFS.WithLabelValues("0.5")); !almostEqual(got, -12.4) {
		t.Errorf("quantile 0.5 = %v, want -12.4", got)
	}

	empty := dump1090.Document{Aircraft: dump1090.AircraftDoc{
		Aircraft: []dump1090.Aircraft{{Hex: "abc123"}},
	}}
	e.Update(empty, time.Millisecond, at.Add(5*time.Second))
	if got := testutil.CollectAndCount(e.signalDBFS, NameSignalDBFS); got != 0 {
		t.Errorf("quantile series = %d after samples vanished, want 0", got)
	}
}

// TestUpdateMigratesReceiverVersionLabel verifies that upgrading the decoder
// retires the old version series and that a receiver.json without refresh
// leaves the last known interval in place.
func TestUpdateMigratesReceiverVersionLabel(t *testing.T) {
	e := newTestExporter(t)
	doc := fixtureDoc(t)
	at := time.Unix(1759470000, 0)

	doc.Receiver.Version = "readsb 3.14.1"
	e.Update(doc, time.Millisecond, at)
	if got := testutil.ToFloat64(e.receiverInfo.WithLabelValues("readsb 3.14.1")); got != 1 {
		t.Fatalf("old version = %v, want 1", got)
	}

	doc.Receiver.Version = "readsb 4.0.0"
	doc.Receiver.Refresh = nil
	e.Update(doc, time.Millisecond, at.Add(5*time.Second))
	if got := testutil.ToFloat64(e.receiverInfo.WithLabelValues("readsb 3.14.1")); got != 0 {
		t.Errorf("retired version = %v, want 0", got)
	}
	if got := testutil.ToFloat64(e.receiverInfo.WithLabelValues("readsb 4.0.0")); got != 1 {
		t.Errorf("current version = %v, want 1", got)
	}
	if got := testutil.ToFloat64(e.receiverRefresh); !almostEqual(got, 1) {
		t.Errorf("refresh = %v after key disappeared, want 1 (last known)", got)
	}
}

// TestUpdateToleratesMissingAndUnknownFields feeds degenerate documents and
// asserts the exporter keeps serving sane values instead of panicking.
func TestUpdateToleratesMissingAndUnknownFields(t *testing.T) {
	tests := []struct {
		name         string
		receiver     string
		stats        string
		aircraft     string
		wantAircraft float64
		wantSignal   int
	}{
		{
			name:         "empty documents",
			receiver:     `{}`,
			stats:        `{}`,
			aircraft:     `{"now": 1759470000, "messages": 0, "aircraft": []}`,
			wantAircraft: 0,
			wantSignal:   0,
		},
		{
			name:         "unknown keys everywhere",
			receiver:     `{"future_field": {"nested": [1, 2, 3]}, "version": "dump1090-fa 3.1.0"}`,
			stats:        `{"future_window": {"whatever": true}, "total": {"future_subobject": 1}}`,
			aircraft:     `{"future": 5, "aircraft": [{"hex": "abc123", "future_aircraft_field": "x"}]}`,
			wantAircraft: 1,
			wantSignal:   0,
		},
		{
			name:         "stats without total window",
			receiver:     `{"version": "readsb"}`,
			stats:        `{"latest": {"messages": 3}, "last1min": {"messages": 70}}`,
			aircraft:     `{"aircraft": []}`,
			wantAircraft: 0,
			wantSignal:   0,
		},
		{
			name:         "stats total without sub-objects",
			receiver:     `{"version": "readsb"}`,
			stats:        `{"total": {}}`,
			aircraft:     `{"aircraft": [{"hex": "a1b2c3", "rssi": -18.5}]}`,
			wantAircraft: 1,
			wantSignal:   3,
		},
		{
			name:         "aircraft without altitude squawk or position",
			receiver:     `{"version": "readsb"}`,
			stats:        `{"total": {"local": {}, "cpu": {}, "cpr": {}, "tracks": {}}}`,
			aircraft:     `{"aircraft": [{"hex": "d4e5f6", "alt_baro": "ground"}, {"hex": "78a9b0"}]}`,
			wantAircraft: 2,
			wantSignal:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestExporter(t)
			var doc dump1090.Document
			for _, p := range []struct {
				name string
				raw  string
				dst  any
			}{
				{"receiver.json", tt.receiver, &doc.Receiver},
				{"stats.json", tt.stats, &doc.Stats},
				{"aircraft.json", tt.aircraft, &doc.Aircraft},
			} {
				if err := json.Unmarshal([]byte(p.raw), p.dst); err != nil {
					t.Fatalf("decode %s: %v", p.name, err)
				}
			}

			e.Update(doc, 25*time.Millisecond, time.Unix(1759470000, 0))

			if got := testutil.ToFloat64(e.up); got != 1 {
				t.Errorf("dump1090_up = %v, want 1", got)
			}
			if got := testutil.ToFloat64(e.aircraft); !almostEqual(got, tt.wantAircraft) {
				t.Errorf("dump1090_aircraft = %v, want %v", got, tt.wantAircraft)
			}
			if got := testutil.CollectAndCount(e.signalDBFS, NameSignalDBFS); got != tt.wantSignal {
				t.Errorf("signal series = %d, want %d", got, tt.wantSignal)
			}
			if got := testutil.CollectAndCount(e.squawk, NameAircraftSquawk); got != 0 {
				t.Errorf("unexpected squawk series: %d", got)
			}
		})
	}
}

// TestMarkUnreachableKeepsLastKnownState checks that a failed scrape flips
// dump1090_up to 0 while every previously exported value survives.
func TestMarkUnreachableKeepsLastKnownState(t *testing.T) {
	e := newTestExporter(t)
	at := time.Unix(1759470000, 0)
	e.Update(fixtureDoc(t), 100*time.Millisecond, at)

	e.MarkUnreachable(2 * time.Second)

	if got := testutil.ToFloat64(e.up); got != 0 {
		t.Errorf("dump1090_up = %v, want 0", got)
	}
	if got := testutil.ToFloat64(e.scrapeDuration); !almostEqual(got, 2) {
		t.Errorf("dump1090_scrape_duration_seconds = %v, want 2", got)
	}
	if got := testutil.ToFloat64(e.aircraft); !almostEqual(got, 5) {
		t.Errorf("dump1090_aircraft = %v, want 5 (last known)", got)
	}
	if got := testutil.ToFloat64(e.messagesTotal); !almostEqual(got, 1000000) {
		t.Errorf("dump1090_messages_total = %v, want 1000000 (last known)", got)
	}
	if got := e.LastUpdate(); !got.Equal(at) {
		t.Errorf("LastUpdate() = %v, want %v (unchanged by failure)", got, at)
	}
}

// TestHandlerRespondsWithExpectedMetrics serves the registry through promhttp
// and asserts a 200 response advertising every documented metric family.
func TestHandlerRespondsWithExpectedMetrics(t *testing.T) {
	e := newTestExporter(t)
	e.Update(fixtureDoc(t), 150*time.Millisecond, time.Unix(1759470000, 0))

	h := promhttp.HandlerFor(e.Registry(), promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	body := rec.Body.String()
	for _, name := range []string{
		NameUp,
		NameMessagesTotal,
		NameAircraft,
		NameAircraftWithPosition,
		NameMessagesPerSecond,
		NameSignalDBFS,
		NameBlocksDroppedTotal,
		NameCPUMilliseconds,
		"dump1090_cpr_local_ok",
		"dump1090_cpr_global_bad",
		"dump1090_tracks_all",
		"dump1090_tracks_single_message",
		NameScrapeDurationSeconds,
		NameAircraftAltitude + "_bucket",
		NameAircraftSquawk,
		NameReceiverInfo,
		NameReceiverRefreshSeconds,
	} {
		t.Run(name, func(t *testing.T) {
			if !exposesMetric(body, name) {
				t.Errorf("metric %s missing from exposition:\n%s", name, body)
			}
		})
	}
}

func TestQuantileOf(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		q      float64
		want   float64
	}{
		{name: "single sample", sorted: []float64{-5}, q: 0.5, want: -5},
		{name: "nearest rank lower half", sorted: []float64{-30, -20, -10, -5}, q: 0.5, want: -20},
		{name: "upper quantile picks a reported value", sorted: []float64{-30, -20, -10, -5}, q: 0.9, want: -5},
		{name: "maximum", sorted: []float64{-30, -20, -10, -5}, q: 1, want: -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quantileOf(tt.sorted, tt.q)
			if !almostEqual(got, tt.want) {
				t.Fatalf("quantileOf(%v, %v) = %v, want %v", tt.sorted, tt.q, got, tt.want)
			}
		})
	}
}

func TestFormatQuantile(t *testing.T) {
	tests := []struct {
		q    float64
		want string
	}{
		{q: 0.5, want: "0.5"},
		{q: 0.9, want: "0.9"},
		{q: 0.99, want: "0.99"},
		{q: 1, want: "1"},
	}
	for _, tt := range tests {
		if got := formatQuantile(tt.q); got != tt.want {
			t.Errorf("formatQuantile(%v) = %q, want %q", tt.q, got, tt.want)
		}
	}
}
