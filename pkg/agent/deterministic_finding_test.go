package agent

import (
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestDeterministicFinding_FieldPopulation asserts the builder fills every
// detect-mode field with deterministic, honest content: an "Anomaly:" title
// carrying the service, the "anomaly" category, the heuristic marker in the
// Summary, the pattern id for traceability, and at least one suggestion.
func TestDeterministicFinding_FieldPopulation(t *testing.T) {
	result := core.AgentResult{
		Verdict:   core.VerdictSpike,
		PatternID: "pid-1",
		Template:  "connection refused to <*>",
		Frequency: 12,
		Baseline:  38.4,
		SampleSignals: []core.Signal{
			{Message: "connection refused to db-01", Source: "loki:prod"},
		},
	}

	f := deterministicFinding(result, core.VerdictSpike, "checkout", 12.4, 3.1, "")
	if f == nil {
		t.Fatal("builder returned nil")
	}
	if !strings.HasPrefix(f.Title, "Anomaly:") {
		t.Fatalf("title should start with Anomaly:, got %q", f.Title)
	}
	if !strings.Contains(f.Title, "checkout") {
		t.Fatalf("title should carry the service, got %q", f.Title)
	}
	if f.Category != "anomaly" {
		t.Fatalf("category = %q, want anomaly", f.Category)
	}
	if !strings.Contains(f.Summary, heuristicMarker) {
		t.Fatalf("summary missing heuristic marker: %q", f.Summary)
	}
	if len(f.SampleIDs) != 1 || f.SampleIDs[0] != "pid-1" {
		t.Fatalf("sample ids = %v, want [pid-1]", f.SampleIDs)
	}
	if len(f.Suggestions) == 0 {
		t.Fatal("expected at least one deterministic suggestion")
	}
}

// TestDeterministicFinding_SeverityFromSigma covers the sigma→severity mapping,
// including the baselineStd<=0 unknown-dispersion guard collapsing to medium.
// The sigma is score / baselineStd per the detect design.
func TestDeterministicFinding_SeverityFromSigma(t *testing.T) {
	cases := []struct {
		name        string
		score       float64
		baselineStd float64
		want        string
	}{
		{"critical at 5 sigma", 5, 1, "critical"},
		{"critical above 5 sigma", 12, 2, "critical"}, // 6σ
		{"high at 3 sigma", 3, 1, "high"},
		{"high between 3 and 5", 4.5, 1, "high"},
		{"medium below 3 sigma", 2, 1, "medium"},
		{"unknown dispersion is medium", 100, 0, "medium"},
		{"negative dispersion is medium", 100, -1, "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := deterministicFinding(core.AgentResult{}, core.VerdictSpike, "svc", tc.score, tc.baselineStd, "")
			if f.Severity != tc.want {
				t.Fatalf("severity = %q, want %q (score=%v std=%v)", f.Severity, tc.want, tc.score, tc.baselineStd)
			}
		})
	}
}

// TestDeterministicFinding_RuleSeverityFloorWins proves an operator-declared
// RuleSeverity is honoured as a floor even when the sigma-derived severity is
// lower. A weak deviation (medium) with a critical rule floor emits critical.
func TestDeterministicFinding_RuleSeverityFloorWins(t *testing.T) {
	result := core.AgentResult{RuleSeverity: "critical"}
	// score/std = 1σ → sigma severity would be medium; the floor must win.
	f := deterministicFinding(result, core.VerdictSpike, "svc", 1, 1, "")
	if f.Severity != "critical" {
		t.Fatalf("severity = %q, want critical (rule floor wins)", f.Severity)
	}
}

// TestDeterministicFinding_RedactedSamplesOnly proves the builder never leaks
// unredacted content: it reads Signal.Message (already redacted upstream) and
// never Signal.Raw. A signal whose Raw differs from Message must surface only
// the Message text in the Summary.
func TestDeterministicFinding_RedactedSamplesOnly(t *testing.T) {
	result := core.AgentResult{
		PatternID: "pid-x",
		SampleSignals: []core.Signal{
			{
				Message: "login failed for user <redacted>",
				Raw:     map[string]interface{}{"msg": "login failed for user alice@example.com"},
			},
		},
	}
	f := deterministicFinding(result, core.VerdictUnknown, "auth", 0, 0, "")

	if !strings.Contains(f.Summary, "user <redacted>") {
		t.Fatalf("summary should carry the redacted message, got %q", f.Summary)
	}
	if strings.Contains(f.Summary, "alice@example.com") {
		t.Fatalf("summary leaked raw (unredacted) content: %q", f.Summary)
	}
}

// TestDeterministicFinding_ConfidenceBounded proves Confidence is deterministic
// and always within [0, 1]: unknown dispersion pins it to 0.5, and a large
// deviation saturates below 1 rather than exceeding it. Two identical inputs
// yield the identical value.
func TestDeterministicFinding_ConfidenceBounded(t *testing.T) {
	cases := []struct {
		name        string
		score       float64
		baselineStd float64
	}{
		{"unknown dispersion", 0, 0},
		{"small deviation", 2, 1},
		{"huge deviation saturates", 1000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := deterministicFinding(core.AgentResult{}, core.VerdictSpike, "svc", tc.score, tc.baselineStd, "")
			b := deterministicFinding(core.AgentResult{}, core.VerdictSpike, "svc", tc.score, tc.baselineStd, "")
			if a.Confidence != b.Confidence {
				t.Fatalf("confidence not deterministic: %v vs %v", a.Confidence, b.Confidence)
			}
			if a.Confidence < 0 || a.Confidence > 1 {
				t.Fatalf("confidence %v out of [0,1]", a.Confidence)
			}
		})
	}
	// Unknown dispersion is exactly 0.5.
	if f := deterministicFinding(core.AgentResult{}, core.VerdictSpike, "svc", 5, 0, ""); f.Confidence != 0.5 {
		t.Fatalf("unknown-dispersion confidence = %v, want 0.5", f.Confidence)
	}
}

// TestDeterministicFinding_TitleForms proves the title uses the metric/trace
// form (deviation + baseline) when a dispersion is learned, and the log form
// (template ×frequency) when it is not.
func TestDeterministicFinding_TitleForms(t *testing.T) {
	metric := deterministicFinding(
		core.AgentResult{Template: "latency", Baseline: 100, Frequency: 3},
		core.VerdictSpike, "api", 12, 3, "",
	)
	if !strings.Contains(metric.Title, "σ") || !strings.Contains(metric.Title, "baseline") {
		t.Fatalf("metric title should carry deviation+baseline, got %q", metric.Title)
	}

	logs := deterministicFinding(
		core.AgentResult{Template: "connection refused to <*>", Frequency: 7},
		core.VerdictUnknown, "db", 0, 0, "",
	)
	if !strings.Contains(logs.Title, "×7") {
		t.Fatalf("log title should carry ×frequency, got %q", logs.Title)
	}
}

// TestDeterministicFinding_ServiceFallback proves an empty service falls back to
// the sample signal's Source, then the pattern id.
func TestDeterministicFinding_ServiceFallback(t *testing.T) {
	fromSource := deterministicFinding(
		core.AgentResult{SampleSignals: []core.Signal{{Source: "loki:prod", Message: "x"}}},
		core.VerdictUnknown, "", 0, 0, "",
	)
	if !strings.Contains(fromSource.Title, "loki:prod") {
		t.Fatalf("title should fall back to source, got %q", fromSource.Title)
	}

	fromPattern := deterministicFinding(
		core.AgentResult{PatternID: "pid-only"},
		core.VerdictUnknown, "", 0, 0, "",
	)
	if !strings.Contains(fromPattern.Title, "pid-only") {
		t.Fatalf("title should fall back to pattern id, got %q", fromPattern.Title)
	}
}
