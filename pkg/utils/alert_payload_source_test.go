package utils

import (
	"testing"
)

// TestFoldedPayload_SourceAndPatternPriority pins the lookup contract of the
// two fingerprint components that used to be read by an ad-hoc map copy:
// priority order across the key set, case-insensitive matching, trimming, and
// fail-soft on an unusable value.
func TestFoldedPayload_SourceAndPatternPriority(t *testing.T) {
	tests := []struct {
		name        string
		content     map[string]interface{}
		wantSource  string
		wantPattern string
	}{
		{
			name:        "exact lowercase",
			content:     map[string]interface{}{"source": "sns", "patternid": "P1"},
			wantSource:  "sns",
			wantPattern: "P1",
		},
		{
			name:        "case-insensitive",
			content:     map[string]interface{}{"Source": "sns", "PatternID": "P1"},
			wantSource:  "sns",
			wantPattern: "P1",
		},
		{
			name:        "pattern priority: PatternID beats pattern_id beats key",
			content:     map[string]interface{}{"key": "K", "pattern_id": "P2", "PatternID": "P1"},
			wantPattern: "P1",
		},
		{
			name:        "pattern priority: pattern_id beats key",
			content:     map[string]interface{}{"key": "K", "pattern_id": "P2"},
			wantPattern: "P2",
		},
		{
			name:        "generic key is the last resort",
			content:     map[string]interface{}{"key": "K"},
			wantPattern: "K",
		},
		{
			name:        "trimmed",
			content:     map[string]interface{}{"source": "  sns  ", "PatternID": "  P1  "},
			wantSource:  "sns",
			wantPattern: "P1",
		},
		{
			name:    "whitespace only is absent",
			content: map[string]interface{}{"source": "   ", "PatternID": "\t"},
		},
		{
			name:    "non-string is absent",
			content: map[string]interface{}{"source": 42, "PatternID": nil},
		},
		{
			name:    "nothing to read",
			content: map[string]interface{}{"unrelated": "x"},
		},
		{
			name:        "an unusable duplicate does not shadow the usable spelling",
			content:     map[string]interface{}{"SOURCE": "", "Source": "sns", "PATTERNID": nil, "PatternID": "P1"},
			wantSource:  "sns",
			wantPattern: "P1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewFoldedPayload(tc.content)
			if got := p.Source(); got != tc.wantSource {
				t.Errorf("Source() = %q, want %q", got, tc.wantSource)
			}
			if got := p.PatternID(); got != tc.wantPattern {
				t.Errorf("PatternID() = %q, want %q", got, tc.wantPattern)
			}
			if got := ExtractSource(tc.content); got != tc.wantSource {
				t.Errorf("ExtractSource = %q, want %q", got, tc.wantSource)
			}
			if got := ExtractPatternID(tc.content); got != tc.wantPattern {
				t.Errorf("ExtractPatternID = %q, want %q", got, tc.wantPattern)
			}
		})
	}
}

// TestFoldedPayload_SourceAndPatternDeterminism is the guarantee the dedup
// fingerprint rests on: a payload carrying two spellings of one key resolves to
// the same value on every read. Go randomises map iteration, so a lookup that
// ranges the payload would answer differently between reads of the SAME map.
func TestFoldedPayload_SourceAndPatternDeterminism(t *testing.T) {
	contents := []map[string]interface{}{
		{"Source": "a", "source": "b", "PatternID": "p", "patternid": "q"},
		{"SOURCE": "a", "Source": "b", "PATTERNID": "p", "PatternId": "q"},
		{"source": "a", "SoUrCe": "b", "key": "k", "KEY": "K"},
	}
	for i, content := range contents {
		wantSource := ExtractSource(content)
		wantPattern := ExtractPatternID(content)
		if wantSource == "" || wantPattern == "" {
			t.Fatalf("content %d: fixture resolved empty (%q/%q)", i, wantSource, wantPattern)
		}
		for n := 0; n < 2000; n++ {
			if got := ExtractSource(content); got != wantSource {
				t.Fatalf("content %d read %d: ExtractSource = %q, want %q", i, n, got, wantSource)
			}
			if got := ExtractPatternID(content); got != wantPattern {
				t.Fatalf("content %d read %d: ExtractPatternID = %q, want %q", i, n, got, wantPattern)
			}
		}
	}
}

// TestFoldedPayload_OneIndexMatchesOneShotCalls is the sharing guarantee the
// enterprise interceptor rides on: every field read off ONE FoldedPayload is
// byte-identical to the separate one-shot call for that field, so a caller that
// needs all five pays for one index instead of five.
func TestFoldedPayload_OneIndexMatchesOneShotCalls(t *testing.T) {
	corpus := payloadCorpus()
	corpus["fingerprint components"] = map[string]interface{}{
		"Source": "sns", "ServiceName": "checkout", "PatternID": "P1",
		"Severity": "critical", "AlarmName": "CPUHigh",
	}
	corpus["duplicate fingerprint spellings"] = map[string]interface{}{
		"Source": "sns", "source": "sns-eu", "PatternID": "P1", "patternid": "P2",
		"service": "checkout", "severity": "high", "title": "boom",
	}

	for name, content := range corpus {
		t.Run(name, func(t *testing.T) {
			p := NewFoldedPayload(content)
			// Read every field off the one index, repeatedly and in a
			// different order each pass, so a cached miss or a lazily built
			// sub-index can never change a later answer.
			for i := 0; i < 3; i++ {
				got := []string{p.Service(), p.Severity(), p.Title(), p.Source(), p.PatternID()}
				want := []string{
					ExtractService(content), ExtractSeverity(content), ExtractTitle(content),
					ExtractSource(content), ExtractPatternID(content),
				}
				for j := range got {
					if got[j] != want[j] {
						t.Fatalf("pass %d field %d: shared index = %q, one-shot = %q", i, j, got[j], want[j])
					}
				}
				if got := p.PatternID(); got != want[4] {
					t.Fatalf("pass %d: re-read PatternID = %q, want %q", i, got, want[4])
				}
				if got := p.Source(); got != want[3] {
					t.Fatalf("pass %d: re-read Source = %q, want %q", i, got, want[3])
				}
			}
		})
	}
}

// TestFoldedPayload_NilSafe keeps the fail-soft rule for the new readers: a nil
// payload or a nil content map yields "" rather than panicking.
func TestFoldedPayload_NilSafe(t *testing.T) {
	var nilPayload *FoldedPayload
	if got := nilPayload.Source(); got != "" {
		t.Errorf("nil payload Source() = %q, want empty", got)
	}
	if got := nilPayload.PatternID(); got != "" {
		t.Errorf("nil payload PatternID() = %q, want empty", got)
	}
	if got := ExtractSource(nil); got != "" {
		t.Errorf("ExtractSource(nil) = %q, want empty", got)
	}
	if got := ExtractPatternID(nil); got != "" {
		t.Errorf("ExtractPatternID(nil) = %q, want empty", got)
	}
}

// BenchmarkFoldedPayload_FiveFieldsOneIndex measures the whole per-alert read
// set off one index against the same five fields read via the one-shot
// wrappers, which is what a caller doing five separate calls pays.
func BenchmarkFoldedPayload_FiveFieldsOneIndex(b *testing.B) {
	content := widePayload(10000)
	b.ResetTimer()
	for b.Loop() {
		p := NewFoldedPayload(content)
		_, _, _, _, _ = p.Service(), p.Severity(), p.Title(), p.Source(), p.PatternID()
	}
}

func BenchmarkFoldedPayload_FiveFieldsSeparateCalls(b *testing.B) {
	content := widePayload(10000)
	b.ResetTimer()
	for b.Loop() {
		_ = ExtractService(content)
		_ = ExtractSeverity(content)
		_ = ExtractTitle(content)
		_ = ExtractSource(content)
		_ = ExtractPatternID(content)
	}
}
