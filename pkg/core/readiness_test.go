package core

import (
	"encoding/json"
	"testing"
)

// TestReadiness_JSONTags pins the wire shape the UI (C4) and the enterprise
// metric/trace reader (E1) both consume. The JSON tags are the contract; a
// rename here would silently break every consumer, so assert them explicitly.
func TestReadiness_JSONTags(t *testing.T) {
	r := Readiness{Ready: true, Seen: 7, Needed: 20, RatePerMin: 1.5}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	want := `{"ready":true,"seen":7,"needed":20,"rate_per_min":1.5}`
	if got != want {
		t.Fatalf("Readiness JSON = %s, want %s", got, want)
	}

	// Round-trip must preserve every field.
	var back Readiness
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != r {
		t.Fatalf("round-trip = %+v, want %+v", back, r)
	}
}

// TestReadiness_ZeroValueSentinels documents the two sentinels a consumer must
// treat specially: Needed==0 (indeterminate — no count gate applies) and
// RatePerMin==0 (unknown/stalled — no honest ETA). The zero value is a valid,
// meaningful Readiness (Learning, indeterminate, no ETA), not "missing".
func TestReadiness_ZeroValueSentinels(t *testing.T) {
	var r Readiness // zero value

	if r.Ready {
		t.Errorf("zero Readiness.Ready = true, want false (Learning)")
	}
	if r.Needed != 0 {
		t.Errorf("zero Readiness.Needed = %d, want 0 (indeterminate sentinel)", r.Needed)
	}
	if r.RatePerMin != 0 {
		t.Errorf("zero Readiness.RatePerMin = %v, want 0 (unknown/stalled sentinel)", r.RatePerMin)
	}

	// A JSON zero value still carries every field (no omitempty) so the UI can
	// distinguish "indeterminate" from "field absent".
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"ready":false,"seen":0,"needed":0,"rate_per_min":0}`
	if string(b) != want {
		t.Fatalf("zero Readiness JSON = %s, want %s (all fields present, no omitempty)", b, want)
	}
}
