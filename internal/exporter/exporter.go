// Package exporter turns dump1090/readsb JSON documents into Prometheus
// metrics.
//
// The design is deliberately small: one Exporter owns a private registry
// full of real prometheus metric objects, and each successful scrape feeds
// them through Update while each failed scrape flips dump1090_up to 0 via
// MarkUnreachable. Label cardinality is bounded by construction — aircraft
// are counted, never labelled, so a fleet of 10 000 aircraft still produces
// a fixed handful of series.
package exporter

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/technothatch/dump1090-exporter/internal/dump1090"
)

// Metric and label names, exported so dashboards and tests can reference
// them without repeating string literals.
const (
	// NameUp is 1 after a successful scrape, 0 after a failed one.
	NameUp = "dump1090_up"
	// NameMessagesTotal counts messages the decoder has seen, cumulative
	// across decoder restarts.
	NameMessagesTotal = "dump1090_messages_total"
	// NameAircraft is the number of aircraft currently tracked.
	NameAircraft = "dump1090_aircraft"
	// NameAircraftWithPosition is the subset carrying lat/lon.
	NameAircraftWithPosition = "dump1090_aircraft_with_position"
	// NameMessagesPerSecond is the observed message rate between scrapes.
	NameMessagesPerSecond = "dump1090_messages_per_second"
	// NameSignalDBFS is receive signal strength quantile (label quantile).
	NameSignalDBFS = "dump1090_signal_dbfs"
	// NameBlocksDroppedTotal counts demodulator overload drops.
	NameBlocksDroppedTotal = "dump1090_blocks_dropped_total"
	// NameCPUMilliseconds counts CPU time per pipeline stage (label stage).
	NameCPUMilliseconds = "dump1090_cpu_milliseconds"
	// NameScrapeDurationSeconds is how long the last scrape took.
	NameScrapeDurationSeconds = "dump1090_scrape_duration_seconds"
	// NameAircraftAltitude is a histogram of barometric altitudes in feet.
	// Its buckets appear as dump1090_aircraft_altitude_bucket.
	NameAircraftAltitude = "dump1090_aircraft_altitude"
	// NameAircraftSquawk is the aircraft count per squawk (label squawk).
	NameAircraftSquawk = "dump1090_aircraft_squawk"
	// NameReceiverInfo carries the decoder version (label version).
	NameReceiverInfo = "dump1090_receiver_info"
	// NameReceiverRefreshSeconds is the decoder's JSON refresh interval.
	NameReceiverRefreshSeconds = "dump1090_receiver_refresh_seconds"
)

// Label names used by the metrics above.
const (
	// LabelQuantile is the quantile label of NameSignalDBFS.
	LabelQuantile = "quantile"
	// LabelStage is the pipeline stage label of NameCPUMilliseconds.
	LabelStage = "stage"
	// LabelSquawk is the Mode A code label of NameAircraftSquawk.
	LabelSquawk = "squawk"
	// LabelVersion is the decoder version label of NameReceiverInfo.
	LabelVersion = "version"
)

// DefaultAltitudeBuckets are the histogram buckets for NameAircraftAltitude,
// in feet: general-aviation floors through flight levels, plus everything
// at or below sea level in the first bucket.
var DefaultAltitudeBuckets = []float64{0, 1000, 2500, 5000, 10000, 15000, 20000, 25000, 30000, 35000, 40000, 50000}

// DefaultSignalQuantiles are the RSSI quantiles exported on
// NameSignalDBFS, computed over the aircraft seen in each scrape.
var DefaultSignalQuantiles = []float64{0.5, 0.9, 0.99}

// squawkRe bounds NameAircraftSquawk cardinality: only well-formed 4-digit
// octal codes are exported, so at most 4096 series can ever exist — and a
// misbehaving upstream feeding garbage labels cannot explode the index.
var squawkRe = regexp.MustCompile(`^[0-7]{4}$`)

// Options configures an Exporter. The zero value uses sensible defaults.
type Options struct {
	// AltitudeBuckets overrides DefaultAltitudeBuckets when non-empty.
	AltitudeBuckets []float64
	// Quantiles overrides DefaultSignalQuantiles when non-empty.
	Quantiles []float64
}

// Exporter owns the metric state for one decoder. All methods are safe for
// concurrent use: scrapes run Update/MarkUnreachable while Prometheus
// collects through Registry.
type Exporter struct {
	mu sync.Mutex

	up                   prometheus.Gauge
	receiverRefresh      prometheus.Gauge
	receiverInfo         *prometheus.GaugeVec
	messagesTotal        prometheus.Counter
	messagesPerSecond    prometheus.Gauge
	aircraft             prometheus.Gauge
	aircraftWithPosition prometheus.Gauge
	signalDBFS           *prometheus.GaugeVec
	blocksDroppedTotal   prometheus.Counter
	cpuMilliseconds      *prometheus.CounterVec
	cprLocalOK           prometheus.Gauge
	cprLocalBad          prometheus.Gauge
	cprLocalSkipped      prometheus.Gauge
	cprGlobalOK          prometheus.Gauge
	cprGlobalBad         prometheus.Gauge
	cprGlobalSkipped     prometheus.Gauge
	tracksAll            prometheus.Gauge
	tracksSingleMessage  prometheus.Gauge
	scrapeDuration       prometheus.Gauge
	altitude             prometheus.Histogram
	squawk               *prometheus.GaugeVec

	registry *prometheus.Registry

	// The following fields are only touched under mu.
	messagesDelta deltaTracker
	blocksDelta   deltaTracker
	cpuDeltas     map[string]*deltaTracker
	lastRateTime  time.Time
	lastVersion   string
	seenSquawks   map[string]bool
	lastUpdate    time.Time
	quantiles     []float64
}

// New builds an Exporter and registers every metric on a private registry.
// Callers serve the registry with promhttp.HandlerFor.
func New(opts Options) (*Exporter, error) {
	buckets := opts.AltitudeBuckets
	if len(buckets) == 0 {
		buckets = DefaultAltitudeBuckets
	}
	quantiles := opts.Quantiles
	if len(quantiles) == 0 {
		quantiles = DefaultSignalQuantiles
	}

	e := &Exporter{
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameUp,
			Help: "1 if the last scrape of dump1090/readsb succeeded, 0 otherwise.",
		}),
		receiverRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameReceiverRefreshSeconds,
			Help: "Configured JSON refresh interval of the decoder, in seconds (receiver.json).",
		}),
		receiverInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameReceiverInfo,
			Help: "Decoder version reported by receiver.json; 1 for the current version, 0 for versions no longer seen.",
		}, []string{LabelVersion}),
		messagesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameMessagesTotal,
			Help: "Messages decoded since the decoder started, monotonically increased across decoder restarts.",
		}),
		messagesPerSecond: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameMessagesPerSecond,
			Help: "Message rate between the two most recent successful scrapes, in messages per second.",
		}),
		aircraft: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameAircraft,
			Help: "Aircraft currently tracked in aircraft.json.",
		}),
		aircraftWithPosition: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameAircraftWithPosition,
			Help: "Tracked aircraft that currently carry a lat/lon position.",
		}),
		signalDBFS: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameSignalDBFS,
			Help: "Quantile of rssi (dBFS) over the aircraft seen in the last scrape; only exported when at least one aircraft reports rssi.",
		}, []string{LabelQuantile}),
		blocksDroppedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameBlocksDroppedTotal,
			Help: "Sample blocks dropped by the demodulator, cumulative across decoder restarts.",
		}),
		cpuMilliseconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameCPUMilliseconds,
			Help: "CPU time consumed by the decoder per pipeline stage, in milliseconds, cumulative across decoder restarts.",
		}, []string{LabelStage}),
		cprLocalOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_local_ok",
			Help: "Successfully decoded local CPR position reports (stats.json total window).",
		}),
		cprLocalBad: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_local_bad",
			Help: "Failed local CPR position decodes (stats.json total window).",
		}),
		cprLocalSkipped: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_local_skipped",
			Help: "Local CPR frames skipped as redundant (stats.json total window).",
		}),
		cprGlobalOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_global_ok",
			Help: "Successfully decoded global CPR position reports (stats.json total window).",
		}),
		cprGlobalBad: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_global_bad",
			Help: "Failed global CPR position decodes (stats.json total window).",
		}),
		cprGlobalSkipped: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_cpr_global_skipped",
			Help: "Global CPR frames skipped as redundant (stats.json total window).",
		}),
		tracksAll: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_tracks_all",
			Help: "Aircraft tracks currently maintained (stats.json total window).",
		}),
		tracksSingleMessage: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dump1090_tracks_single_message",
			Help: "Tracks that only ever produced a single message (stats.json total window).",
		}),
		scrapeDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameScrapeDurationSeconds,
			Help: "Wall time the last scrape of the decoder took, in seconds.",
		}),
		altitude: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    NameAircraftAltitude,
			Help:    "Barometric altitude of tracked aircraft in feet, observed per aircraft per scrape; alt_baro \"ground\" counts as 0.",
			Buckets: buckets,
		}),
		squawk: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: NameAircraftSquawk,
			Help: "Aircraft currently broadcasting each Mode A squawk; squawks no longer present are reported as 0.",
		}, []string{LabelSquawk}),

		registry:  prometheus.NewRegistry(),
		cpuDeltas: make(map[string]*deltaTracker),
		// Two generations of squawk gauges are kept alive: the counts from
		// the last scrape, plus explicit zeros for squawks that vanished.
		seenSquawks: make(map[string]bool),
		quantiles:   quantiles,
	}
	e.cpuDeltas["demod"] = &deltaTracker{}
	e.cpuDeltas["reader"] = &deltaTracker{}
	e.cpuDeltas["background"] = &deltaTracker{}

	collectors := []prometheus.Collector{
		e.up, e.receiverRefresh, e.receiverInfo, e.messagesTotal,
		e.messagesPerSecond, e.aircraft, e.aircraftWithPosition, e.signalDBFS,
		e.blocksDroppedTotal, e.cpuMilliseconds, e.cprLocalOK, e.cprLocalBad,
		e.cprLocalSkipped, e.cprGlobalOK, e.cprGlobalBad, e.cprGlobalSkipped,
		e.tracksAll, e.tracksSingleMessage, e.scrapeDuration, e.altitude,
		e.squawk,
	}
	for _, c := range collectors {
		if err := e.registry.Register(c); err != nil {
			return nil, fmt.Errorf("exporter: register metric: %w", err)
		}
	}
	return e, nil
}

// Registry returns the private registry holding every exporter metric. It is
// what promhttp.HandlerFor should be pointed at.
func (e *Exporter) Registry() *prometheus.Registry {
	return e.registry
}

// LastUpdate returns the time of the most recent successful scrape, or the
// zero time if none has succeeded yet — which callers read as "stale".
func (e *Exporter) LastUpdate() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastUpdate
}

// MarkUnreachable records a failed scrape: dump1090_up goes to 0 while all
// previously exported values stay in place, so charts keep their last known
// state instead of flapping to absent.
func (e *Exporter) MarkUnreachable(duration time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.up.Set(0)
	e.scrapeDuration.Set(duration.Seconds())
}

// Update records a successful scrape of all three decoder documents.
// at is the scrape timestamp used for rate maths; duration is how long the
// scrape itself took.
func (e *Exporter) Update(doc dump1090.Document, duration time.Duration, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.up.Set(1)
	e.scrapeDuration.Set(duration.Seconds())
	e.lastUpdate = at

	e.updateReceiver(doc.Receiver)
	delta := e.updateAircraft(doc.Aircraft)
	e.updateRates(delta, at)
	e.updateStats(doc.Stats)
}

// updateReceiver folds receiver.json into the gauges.
func (e *Exporter) updateReceiver(r dump1090.Receiver) {
	if r.Version != "" {
		if e.lastVersion != "" && e.lastVersion != r.Version {
			// A decoder upgrade replaced the version label; retire the old
			// series so dashboards do not keep a phantom decoder alive.
			e.receiverInfo.WithLabelValues(e.lastVersion).Set(0)
		}
		e.receiverInfo.WithLabelValues(r.Version).Set(1)
		e.lastVersion = r.Version
	}
	if r.Refresh != nil {
		e.receiverRefresh.Set(*r.Refresh)
	}
}

// updateAircraft folds aircraft.json into the gauges and returns the message
// delta since the previous scrape (0 on the very first scrape is impossible:
// the delta tracker seeds itself with the cumulative value).
func (e *Exporter) updateAircraft(doc dump1090.AircraftDoc) float64 {
	e.aircraft.Set(float64(len(doc.Aircraft)))

	aircraft := doc.Aircraft
	withPosition := 0
	counts := make(map[string]int, 8)
	rssi := make([]float64, 0, len(aircraft))
	for i := range aircraft {
		a := &aircraft[i]
		if a.HasPosition() {
			withPosition++
		}
		if squawkRe.MatchString(a.Squawk) {
			counts[a.Squawk]++
		}
		if a.RSSI != nil {
			rssi = append(rssi, *a.RSSI)
		}
		if alt, ok := a.AltFeet(); ok {
			e.altitude.Observe(alt)
		}
	}
	e.aircraftWithPosition.Set(float64(withPosition))
	e.updateSquawks(counts)
	e.updateSignal(rssi)
	return e.messagesDelta.Delta(doc.Messages)
}

// updateSquawks writes the per-squawk counts and zeroes series whose squawk
// disappeared since the last scrape.
func (e *Exporter) updateSquawks(counts map[string]int) {
	for sq, n := range counts {
		e.squawk.WithLabelValues(sq).Set(float64(n))
	}
	for sq := range e.seenSquawks {
		if _, ok := counts[sq]; !ok {
			e.squawk.WithLabelValues(sq).Set(0)
		}
	}
	e.seenSquawks = make(map[string]bool, len(counts))
	for sq := range counts {
		e.seenSquawks[sq] = true
	}
}

// updateSignal recomputes the RSSI quantiles from the current fleet. With no
// samples the series are removed entirely: "no data" is not "0 dBFS", and a
// stale quantile would silently outlive the aircraft it described.
func (e *Exporter) updateSignal(samples []float64) {
	if len(samples) == 0 {
		e.signalDBFS.Reset()
		return
	}
	sort.Float64s(samples)
	for _, q := range e.quantiles {
		e.signalDBFS.WithLabelValues(formatQuantile(q)).Set(quantileOf(samples, q))
	}
}

// updateRates turns the cumulative message counter into an exported total
// and an inter-scrape rate.
func (e *Exporter) updateRates(messageDelta float64, at time.Time) {
	e.messagesTotal.Add(messageDelta)
	if e.lastRateTime.IsZero() {
		e.messagesPerSecond.Set(0)
	} else {
		e.messagesPerSecond.Set(Rate(messageDelta, at.Sub(e.lastRateTime).Seconds()))
	}
	e.lastRateTime = at
}

// updateStats folds the stats.json total window into counters and gauges.
// Everything is optional: a window, sub-object or key that is absent simply
// leaves the corresponding metric untouched.
func (e *Exporter) updateStats(stats dump1090.Stats) {
	w := stats.Total
	if w == nil {
		return
	}
	if w.Local != nil && w.Local.BlocksDropped != nil {
		e.blocksDroppedTotal.Add(e.blocksDelta.Delta(*w.Local.BlocksDropped))
	}
	if w.CPU != nil {
		for stage, v := range map[string]*float64{
			"demod":      w.CPU.Demod,
			"reader":     w.CPU.Reader,
			"background": w.CPU.Background,
		} {
			if v == nil {
				continue
			}
			e.cpuMilliseconds.WithLabelValues(stage).Add(e.cpuDeltas[stage].Delta(*v))
		}
	}
	if w.CPR != nil {
		setIfPresent(e.cprLocalOK, w.CPR.LocalOK)
		setIfPresent(e.cprLocalBad, w.CPR.LocalBad)
		setIfPresent(e.cprLocalSkipped, w.CPR.LocalSkipped)
		setIfPresent(e.cprGlobalOK, w.CPR.GlobalOK)
		setIfPresent(e.cprGlobalBad, w.CPR.GlobalBad)
		setIfPresent(e.cprGlobalSkipped, w.CPR.GlobalSkipped)
	}
	if w.Tracks != nil {
		setIfPresent(e.tracksAll, w.Tracks.All)
		setIfPresent(e.tracksSingleMessage, w.Tracks.SingleMessage)
	}
}

// setIfPresent sets g only when the source key was present; absent keys must
// not masquerade as zeros on the dashboard.
func setIfPresent(g prometheus.Gauge, v *float64) {
	if v != nil {
		g.Set(*v)
	}
}

// quantileOf returns the nearest-rank quantile of a non-empty slice sorted
// in ascending order. Nearest rank (rather than interpolation) is used so a
// quantile is always a value an aircraft actually reported.
func quantileOf(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// formatQuantile renders a quantile for the label value without trailing
// zeros: 0.5, 0.9, 0.99.
func formatQuantile(q float64) string {
	return strconv.FormatFloat(q, 'g', -1, 64)
}
