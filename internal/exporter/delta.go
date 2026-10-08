package exporter

// deltaTracker converts a cumulative source counter into per-scrape deltas
// that survive decoder restarts.
//
// dump1090/readsb counters (messages, blocks dropped, CPU time) are
// cumulative since the decoder started. When the decoder restarts the source
// counter drops back to (or near) zero — a counter reset. Prometheus
// counters must never decrease, so each scrape contributes only the positive
// difference; on reset the new value itself is the delta.
type deltaTracker struct {
	// initialised is false until the first sample has been seen.
	initialised bool
	// prev is the source value at the previous scrape.
	prev float64
}

// Delta returns the amount the source counter grew since the previous call.
// The first call returns v itself, so exported counters start at the
// decoder's own cumulative value rather than at zero.
func (d *deltaTracker) Delta(v float64) float64 {
	if !d.initialised {
		d.initialised = true
		d.prev = v
		return v
	}
	delta := v - d.prev
	if delta < 0 {
		// Counter reset: the decoder restarted between scrapes. The whole
		// new value counts as progress since we last looked.
		delta = v
	}
	d.prev = v
	return delta
}

// Rate returns the per-second rate given the delta since the previous
// sample and the elapsed wall time. It returns 0 for a non-positive or
// zero elapsed time (first sample, frozen clock) instead of dividing by
// zero or emitting a negative rate.
func Rate(delta, elapsedSeconds float64) float64 {
	if elapsedSeconds <= 0 || delta < 0 {
		return 0
	}
	return delta / elapsedSeconds
}
