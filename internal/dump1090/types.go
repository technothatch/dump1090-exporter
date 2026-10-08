// Package dump1090 provides typed, tolerant access to the JSON documents
// published by dump1090-fa and readsb: receiver.json, stats.json and
// aircraft.json (see http://HOST/data/).
//
// Every field is optional. Decoders omit keys they do not know, older
// versions emit fewer keys than newer ones, and both are fine: decoding only
// fails when a document is not valid JSON at all. Unknown keys added by
// future decoder versions are ignored silently.
package dump1090

import (
	"bytes"
	"encoding/json"
)

// Receiver mirrors receiver.json: http://HOST/data/receiver.json
type Receiver struct {
	// Version is the decoder version string, e.g. "readsb 3.14.1".
	Version string `json:"version"`
	// Refresh is the configured JSON write interval in seconds. It is nil
	// when the decoder did not report one.
	Refresh *float64 `json:"refresh"`
	// Lat and Lon are the receiver location. They are nil when the decoder
	// has no position configured (common for mobile setups).
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

// Stats mirrors stats.json: http://HOST/data/stats.json
//
// The decoder keeps one window per interval; only Total is cumulative since
// the decoder started, the others are rolling windows.
type Stats struct {
	// Latest covers the most recent interval.
	Latest *StatsWindow `json:"latest"`
	// Last1Min covers the previous minute.
	Last1Min *StatsWindow `json:"last1min"`
	// Last5Min covers the previous five minutes.
	Last5Min *StatsWindow `json:"last5min"`
	// Last15Min covers the previous fifteen minutes.
	Last15Min *StatsWindow `json:"last15min"`
	// Total is cumulative since the decoder started.
	Total *StatsWindow `json:"total"`
}

// StatsWindow is one interval of decoder statistics. All sub-objects are
// optional and nil when the decoder did not report them.
type StatsWindow struct {
	// Local holds the RF/demodulator counters for this window.
	Local *LocalStats `json:"local"`
	// CPU holds CPU time consumed per pipeline stage, in milliseconds.
	CPU *CPUStats `json:"cpu"`
	// CPR holds compact-position-report decoding counters.
	CPR *CPRStats `json:"cpr"`
	// Tracks holds aircraft track counters.
	Tracks *TracksStats `json:"tracks"`
	// Messages is the number of messages attributed to this window.
	Messages *float64 `json:"messages"`
}

// LocalStats holds the local (demodulator) counters of one stats window.
type LocalStats struct {
	// BlocksProcessed is the number of sample blocks demodulated.
	BlocksProcessed *float64 `json:"blocks_processed"`
	// BlocksDropped is the number of sample blocks lost to overload.
	BlocksDropped *float64 `json:"blocks_dropped"`
}

// CPUStats holds CPU time per pipeline stage, in milliseconds. Stages the
// decoder does not report are nil.
type CPUStats struct {
	// Demod is time spent in the demodulator.
	Demod *float64 `json:"demod"`
	// Reader is time spent reading samples.
	Reader *float64 `json:"reader"`
	// Background is time spent on background work.
	Background *float64 `json:"background"`
}

// CPRStats holds compact-position-report (CPR) decoding counters. A "local"
// CPR is decoded from aircraft visible to this receiver only; a "global" CPR
// needs two aircraft or two messages. Skipped counts CPR frames ignored
// because no new information could be derived.
type CPRStats struct {
	// GlobalOK counts successfully decoded global CPRs.
	GlobalOK *float64 `json:"global_ok"`
	// GlobalBad counts failed global CPR decodes.
	GlobalBad *float64 `json:"global_bad"`
	// GlobalSkipped counts ignored global CPR frames.
	GlobalSkipped *float64 `json:"global_skipped"`
	// LocalOK counts successfully decoded local CPRs.
	LocalOK *float64 `json:"local_ok"`
	// LocalBad counts failed local CPR decodes.
	LocalBad *float64 `json:"local_bad"`
	// LocalSkipped counts ignored local CPR frames.
	LocalSkipped *float64 `json:"local_skipped"`
}

// TracksStats holds aircraft track counters for one window.
type TracksStats struct {
	// All is the number of aircraft tracks maintained.
	All *float64 `json:"all"`
	// SingleMessage counts tracks that only ever produced one message.
	SingleMessage *float64 `json:"single_message"`
}

// AircraftDoc mirrors aircraft.json: http://HOST/data/aircraft.json
type AircraftDoc struct {
	// Now is the decoder clock, in seconds since the Unix epoch.
	Now float64 `json:"now"`
	// Messages is the cumulative message count since the decoder started.
	Messages float64 `json:"messages"`
	// Aircraft holds every currently tracked aircraft.
	Aircraft []Aircraft `json:"aircraft"`
}

// Aircraft mirrors one element of aircraft.json. Every field except Hex is
// optional: a message that carries no position has no lat/lon, a purely
// Mode-S message may have no altitude, and so on.
type Aircraft struct {
	// Hex is the ICAO 24-bit address in lowercase hex, e.g. "3c673a".
	Hex string `json:"hex"`
	// Flight is the trimmed callsign, e.g. "BAW117".
	Flight string `json:"flight"`
	// AltBaro is the barometric altitude in feet or the literal string
	// "ground". It is raw JSON because of the two possible types; use
	// AltFeet to read it.
	AltBaro json.RawMessage `json:"alt_baro"`
	// AltGeom is the geometric (GNSS) altitude in feet.
	AltGeom *float64 `json:"alt_geom"`
	// Lat and Lon are the aircraft position in decimal degrees, nil when
	// unknown.
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
	// Squawk is the 4-digit Mode A code as a string, e.g. "7700".
	Squawk string `json:"squawk"`
	// RSSI is the receive signal strength in dBFS (negative).
	RSSI *float64 `json:"rssi"`
	// GS is ground speed in knots.
	GS *float64 `json:"gs"`
	// Track is the true track in degrees.
	Track *float64 `json:"track"`
	// Seen is the seconds since this aircraft last produced a message.
	Seen *float64 `json:"seen"`
}

// AltFeet decodes AltBaro. The second return value is false when the field
// is absent or not understood; a value of "ground" decodes to 0 feet with
// true, because the aircraft really is at field elevation.
func (a *Aircraft) AltFeet() (float64, bool) {
	raw := bytes.TrimSpace(a.AltBaro)
	// A JSON null unmarshals into a float without error but means "no
	// altitude was reported" — reject it explicitly.
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var feet float64
	if err := json.Unmarshal(raw, &feet); err == nil {
		return feet, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s == "ground" {
		return 0, true
	}
	return 0, false
}

// HasPosition reports whether the aircraft carries a usable lat/lon pair.
func (a *Aircraft) HasPosition() bool {
	return a.Lat != nil && a.Lon != nil
}
