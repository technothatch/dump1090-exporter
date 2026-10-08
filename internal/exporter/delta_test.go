package exporter

import (
	"math"
	"testing"
)

// almostEqual compares two floats within a tolerance so assertions on
// counter sums, rates and histogram sums survive floating-point rounding.
func almostEqual(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9
}

func TestDeltaTracker(t *testing.T) {
	tests := []struct {
		name    string
		samples []float64
		want    []float64
	}{
		{
			name:    "first sample seeds with the cumulative value",
			samples: []float64{42},
			want:    []float64{42},
		},
		{
			name:    "monotonic growth yields per-scrape deltas",
			samples: []float64{100, 150, 200},
			want:    []float64{100, 50, 50},
		},
		{
			name:    "stalled counter contributes zero",
			samples: []float64{10, 10, 10},
			want:    []float64{10, 0, 0},
		},
		{
			name:    "decoder restart counts the new value as progress",
			samples: []float64{1000, 4},
			want:    []float64{1000, 4},
		},
		{
			name:    "restart that lands on zero adds nothing",
			samples: []float64{500, 0},
			want:    []float64{500, 0},
		},
		{
			name:    "repeated restarts never go backwards",
			samples: []float64{10, 5000, 30, 40},
			want:    []float64{10, 4990, 30, 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d deltaTracker
			for i, sample := range tt.samples {
				got := d.Delta(sample)
				if !almostEqual(got, tt.want[i]) {
					t.Fatalf("Delta(%v) after %v = %v, want %v", sample, tt.samples[:i], got, tt.want[i])
				}
			}
		})
	}
}

func TestRate(t *testing.T) {
	tests := []struct {
		name    string
		delta   float64
		elapsed float64
		want    float64
	}{
		{name: "steady traffic", delta: 500, elapsed: 5, want: 100},
		{name: "fractional seconds", delta: 3, elapsed: 4, want: 0.75},
		{name: "no messages", delta: 0, elapsed: 5, want: 0},
		{name: "zero elapsed avoids division by zero", delta: 10, elapsed: 0, want: 0},
		{name: "negative elapsed (clock step) returns zero", delta: 10, elapsed: -1, want: 0},
		{name: "negative delta returns zero", delta: -5, elapsed: 1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Rate(tt.delta, tt.elapsed)
			if !almostEqual(got, tt.want) {
				t.Fatalf("Rate(%v, %v) = %v, want %v", tt.delta, tt.elapsed, got, tt.want)
			}
		})
	}
}
