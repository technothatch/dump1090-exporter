package dump1090

import (
	"encoding/json"
	"testing"
)

func TestAltFeet(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   float64
		wantOK bool
	}{
		{name: "numeric altitude", raw: `34000`, want: 34000, wantOK: true},
		{name: "negative altitude", raw: `-1200`, want: -1200, wantOK: true},
		{name: "zero altitude", raw: `0`, want: 0, wantOK: true},
		{name: "ground is field elevation", raw: `"ground"`, want: 0, wantOK: true},
		{name: "unexpected string", raw: `"fl240"`, want: 0, wantOK: false},
		{name: "null", raw: `null`, want: 0, wantOK: false},
		{name: "object", raw: `{"ft":100}`, want: 0, wantOK: false},
		{name: "empty", raw: ``, want: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Aircraft{Hex: "abc123"}
			if tt.raw != "" {
				a.AltBaro = json.RawMessage(tt.raw)
			}
			got, ok := a.AltFeet()
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("AltFeet(%s) = (%v, %v), want (%v, %v)", tt.raw, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestHasPosition(t *testing.T) {
	lat, lon := 51.5, -0.1
	tests := []struct {
		name string
		a    Aircraft
		want bool
	}{
		{name: "both present", a: Aircraft{Lat: &lat, Lon: &lon}, want: true},
		{name: "lat only", a: Aircraft{Lat: &lat}, want: false},
		{name: "lon only", a: Aircraft{Lon: &lon}, want: false},
		{name: "neither", a: Aircraft{Hex: "abc123"}, want: false},
		{name: "explicit zero island position", a: Aircraft{Lat: ptr(0), Lon: ptr(0)}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.HasPosition(); got != tt.want {
				t.Fatalf("HasPosition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAircraftDecodesRealisticObject(t *testing.T) {
	// A trimmed but realistic aircraft.json element: every field the
	// exporter reads must survive the round trip, unknown fields must be
	// ignored, and "ground" must not break decoding.
	const payload = `{
		"hex": "3c673a",
		"type": "A321",
		"flight": "BAW117  ",
		"alt_baro": "ground",
		"gs": 0,
		"track": 84.1,
		"squawk": "7700",
		"lat": 51.47,
		"lon": -0.45,
		"rssi": -3.2,
		"seen": 0.4,
		"some_future_field": {"nested": true}
	}`
	var a Aircraft
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Hex != "3c673a" {
		t.Errorf("Hex = %q, want 3c673a", a.Hex)
	}
	if a.Flight != "BAW117  " {
		t.Errorf("Flight = %q, want preserved verbatim", a.Flight)
	}
	if a.Squawk != "7700" {
		t.Errorf("Squawk = %q, want 7700", a.Squawk)
	}
	if a.RSSI == nil || *a.RSSI != -3.2 {
		t.Errorf("RSSI = %v, want -3.2", a.RSSI)
	}
	if alt, ok := a.AltFeet(); !ok || alt != 0 {
		t.Errorf("AltFeet() = (%v, %v), want (0, true)", alt, ok)
	}
	if !a.HasPosition() {
		t.Error("HasPosition() = false, want true")
	}
}

func TestStatsDecodesTotalWindowAndIgnoresUnknown(t *testing.T) {
	const payload = `{
		"latest": {"messages": 12},
		"total": {
			"local": {"blocks_processed": 100, "blocks_dropped": 7, "modeac": {"messages": 4}},
			"cpu": {"demod": 1.5, "reader": 0.25},
			"cpr": {"local_ok": 9, "local_bad": 1, "future_key": 5},
			"tracks": {"all": 42, "single_message": 3},
			"messages": 555,
			"some_unknown_object": {"x": [1,2,3]}
		},
		"future_window": {"whatever": true}
	}`
	var s Stats
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Total == nil || s.Total.Local == nil || s.Total.Local.BlocksDropped == nil {
		t.Fatal("total.local.blocks_dropped missing")
	}
	if got := *s.Total.Local.BlocksDropped; got != 7 {
		t.Errorf("blocks_dropped = %v, want 7", got)
	}
	if s.Total.CPU == nil || s.Total.CPU.Demod == nil || s.Total.CPU.Reader == nil {
		t.Fatal("cpu stages missing")
	}
	if s.Total.CPU.Background != nil {
		t.Error("cpu.background should stay nil when absent")
	}
	if s.Total.CPR == nil || s.Total.CPR.LocalOK == nil || *s.Total.CPR.LocalOK != 9 {
		t.Error("cpr.local_ok not decoded")
	}
	if s.Total.CPR.GlobalOK != nil {
		t.Error("cpr.global_ok should stay nil when absent")
	}
	if s.Total.Tracks == nil || s.Total.Tracks.All == nil || *s.Total.Tracks.All != 42 {
		t.Error("tracks.all not decoded")
	}
	if s.Latest == nil || s.Latest.Messages == nil || *s.Latest.Messages != 12 {
		t.Error("latest.messages not decoded")
	}
	if s.Last1Min != nil || s.Total.Messages == nil || *s.Total.Messages != 555 {
		t.Error("window bookkeeping wrong")
	}
}

func TestReceiverDecodesOptionalPosition(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantPos bool
	}{
		{name: "with position", payload: `{"version":"readsb","refresh":1,"lat":48.2,"lon":11.5}`, wantPos: true},
		{name: "without position", payload: `{"version":"dump1090-fa","refresh":1}`, wantPos: false},
		{name: "empty object", payload: `{}`, wantPos: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Receiver
			if err := json.Unmarshal([]byte(tt.payload), &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := r.Lat != nil && r.Lon != nil; got != tt.wantPos {
				t.Fatalf("position present = %v, want %v", got, tt.wantPos)
			}
		})
	}
}

func TestAircraftDocCountsMessages(t *testing.T) {
	const payload = `{"now": 1759470000.1, "messages": 123456, "aircraft": [{"hex":"aa1111"},{"hex":"bb2222","lat":1,"lon":2}]}`
	var d AircraftDoc
	if err := json.Unmarshal([]byte(payload), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Messages != 123456 {
		t.Errorf("Messages = %v, want 123456", d.Messages)
	}
	if len(d.Aircraft) != 2 {
		t.Errorf("len(Aircraft) = %d, want 2", len(d.Aircraft))
	}
	if d.Now != 1759470000.1 {
		t.Errorf("Now = %v, want 1759470000.1", d.Now)
	}
}

func ptr(v float64) *float64 { return &v }
