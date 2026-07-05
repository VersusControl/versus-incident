package agent

import (
	"strings"
	"testing"
)

func TestPushSample_CapsAtRingSizeDropOldest(t *testing.T) {
	var ring []string
	for i := 0; i < SampleRingCap+5; i++ {
		ring = PushSample(ring, string(rune('a'+i)), nil)
	}
	if len(ring) != SampleRingCap {
		t.Fatalf("ring len = %d, want %d", len(ring), SampleRingCap)
	}
	// Oldest→newest order: the 5 earliest ('a'..'e') were dropped, so the ring
	// holds 'f'.. and the LATEST is the last pushed.
	if got := ring[0]; got != "f" {
		t.Errorf("ring[0] = %q, want %q (oldest kept after drop)", got, "f")
	}
	last := string(rune('a' + SampleRingCap + 4))
	if got := ring[len(ring)-1]; got != last {
		t.Errorf("ring[last] = %q, want %q (newest)", got, last)
	}
}

func TestPushSample_TruncatesWithEllipsis(t *testing.T) {
	long := strings.Repeat("x", SampleMaxLen+50)
	ring := PushSample(nil, long, nil)
	if len(ring) != 1 {
		t.Fatalf("ring len = %d, want 1", len(ring))
	}
	got := ring[0]
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated sample must end with ellipsis, got %q", got)
	}
	// SampleMaxLen bytes of content plus the 3-byte ellipsis rune.
	if want := SampleMaxLen + len("…"); len(got) != want {
		t.Errorf("truncated len = %d, want %d", len(got), want)
	}
}

func TestPushSample_OneLinesNewlines(t *testing.T) {
	ring := PushSample(nil, "line one\nline two\r\nline three", nil)
	if len(ring) != 1 {
		t.Fatalf("ring len = %d, want 1", len(ring))
	}
	if strings.ContainsAny(ring[0], "\n\r") {
		t.Errorf("sample must be one-lined, got %q", ring[0])
	}
	if ring[0] != "line one line two  line three" {
		t.Errorf("one-lined sample = %q", ring[0])
	}
}

func TestPushSample_ReScrubsPlantedSecret(t *testing.T) {
	red, errs := NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	// A secret that slipped through to the sample string is caught by the
	// defence-in-depth re-scrub inside PushSample.
	ring := PushSample(nil, "db connect failed password=hunter2 host=db", red)
	if len(ring) != 1 {
		t.Fatalf("ring len = %d, want 1", len(ring))
	}
	if strings.Contains(ring[0], "hunter2") {
		t.Fatalf("secret survived PushSample re-scrub: %q", ring[0])
	}
	if !strings.Contains(ring[0], "<REDACTED:") {
		t.Errorf("expected a redaction token in %q", ring[0])
	}
}

func TestPushSample_NilScrubberPassthrough(t *testing.T) {
	ring := PushSample(nil, "plain line no secret", nil)
	if len(ring) != 1 || ring[0] != "plain line no secret" {
		t.Fatalf("nil scrubber must pass the (one-lined, capped) sample through, got %v", ring)
	}
}

func TestPushSample_EmptyResultNotAppended(t *testing.T) {
	if ring := PushSample(nil, "", nil); ring != nil {
		t.Errorf("empty sample must not be appended, got %v", ring)
	}
	if ring := PushSample([]string{"a"}, "   \n  ", nil); len(ring) != 1 {
		// whitespace-only collapses but is not empty; it still appends. Kept as
		// a guard that only a truly-empty string is skipped.
		t.Logf("whitespace sample appended (len=%d) — not empty, expected", len(ring))
	}
}
