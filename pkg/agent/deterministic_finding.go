package agent

import (
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// heuristicMarker is stamped into every deterministic Summary so an operator
// reading the incident is never misled into thinking an AI model analyzed it.
// It is the honesty contract of the AI-free detect path.
const heuristicMarker = "Heuristic detection — AI enrichment not configured or unavailable."

// deterministicFinding builds a core.AIFinding from a brain verdict WITHOUT
// calling any AI. It is the templated alert the worker sends whenever AI is
// off, quota-limited, or errors, so a brain-detected anomaly is never silently
// dropped. It fills the same detect-mode fields the AI path fills (Title,
// Summary, Severity, Category, Confidence, Suggestions, SampleIDs) using only
// data already on the AgentResult plus the worker's deviation score/std.
//
// It is pure: no I/O, no clock, no globals — every output is a function of its
// inputs, which makes it fully unit-testable and reproducible.
//
// Redaction guarantee: it only ever reads already-redacted sample Messages via
// sampleMessages; it never touches Signal.Raw.
func deterministicFinding(
	result core.AgentResult,
	verdict core.AgentVerdict,
	service string,
	score, baselineStd float64,
	explanation string,
) *core.AIFinding {
	svc := resolveSubjectService(result, service)
	sigma := deviationSigma(score, baselineStd)

	f := &core.AIFinding{
		Title:      deterministicTitle(result, svc, sigma, baselineStd),
		Summary:    deterministicSummary(result, verdict, svc, explanation),
		Severity:   severityFromSigma(baselineStd, sigma),
		Category:   "anomaly",
		Confidence: heuristicConfidence(baselineStd, sigma),
		Suggestions: []string{
			"Review the sampled signals above and correlate with recent deploys or config changes.",
			"Configure an AI key to enrich future detections with automated triage.",
		},
		SampleIDs: deterministicSampleIDs(result),
	}

	// Honour an operator-declared severity as a floor exactly as the AI path
	// does. This makes the builder self-consistent for unit tests; send() also
	// applies the same clamp, so the two never disagree.
	clampSeverityFloor(f, result.RuleSeverity)
	return f
}

// deviationSigma returns the deviation magnitude in standard deviations. Per
// the detect design the magnitude is score / baselineStd; a non-positive
// baselineStd means the dispersion is unknown (e.g. a log novelty verdict with
// no learned spread), signalled by a zero sigma so callers treat it as
// indeterminate.
func deviationSigma(score, baselineStd float64) float64 {
	if baselineStd <= 0 {
		return 0
	}
	s := score / baselineStd
	if s < 0 {
		s = -s
	}
	return s
}

// severityFromSigma maps deviation magnitude to a canonical severity. With an
// unknown dispersion (baselineStd<=0) there is no honest magnitude, so it
// defaults to medium. Otherwise: >=5σ critical, >=3σ high, else medium.
func severityFromSigma(baselineStd, sigma float64) string {
	if baselineStd <= 0 {
		return "medium"
	}
	switch {
	case sigma >= 5:
		return "critical"
	case sigma >= 3:
		return "high"
	default:
		return "medium"
	}
}

// heuristicConfidence is a deterministic, bounded function of the deviation
// magnitude — NOT a fabricated AI score. Unknown dispersion pins it to 0.5;
// otherwise it rises linearly with sigma and saturates so it never claims
// certainty. The result is always within [0, 1].
func heuristicConfidence(baselineStd, sigma float64) float64 {
	if baselineStd <= 0 {
		return 0.5
	}
	c := 0.5 + 0.09*sigma
	if c > 0.95 {
		c = 0.95
	}
	if c < 0.5 {
		c = 0.5
	}
	return c
}

// resolveSubjectService picks the incident's service label, staying robust when
// the observation carries no discovered service: it falls back to the first
// sample signal's Source, then the pattern id, then a generic placeholder.
func resolveSubjectService(result core.AgentResult, service string) string {
	if service != "" {
		return service
	}
	for _, s := range result.SampleSignals {
		if s.Source != "" {
			return s.Source
		}
	}
	if result.PatternID != "" {
		return result.PatternID
	}
	return "unknown-service"
}

// titleSubject picks the most descriptive label available for the anomaly: the
// logical signal name a metric/trace source stamps, then the pattern template,
// then the pattern id.
func titleSubject(result core.AgentResult) string {
	for _, s := range result.SampleSignals {
		if s.Fields != nil {
			if v, ok := s.Fields[core.FieldSignal].(string); ok && v != "" {
				return v
			}
		}
	}
	if result.Template != "" {
		return result.Template
	}
	if result.PatternID != "" {
		return result.PatternID
	}
	return "anomaly"
}

// deterministicTitle renders the incident title. With a learned dispersion it
// uses the metric/trace form carrying the deviation and baseline; otherwise it
// uses the log form carrying the template and frequency.
func deterministicTitle(result core.AgentResult, svc string, sigma, baselineStd float64) string {
	subject := truncateString(titleSubject(result), 120)
	if baselineStd > 0 {
		return fmt.Sprintf("Anomaly: %s — %s (%.1fσ above baseline %.2f±%.2f)",
			svc, subject, sigma, result.Baseline, baselineStd)
	}
	return fmt.Sprintf("Anomaly: %s — %s ×%d", svc, subject, result.Frequency)
}

// deterministicSummary renders a templated one-liner, the honesty marker, and
// up to three ALREADY-REDACTED sample messages. It never reads Signal.Raw.
func deterministicSummary(
	result core.AgentResult,
	verdict core.AgentVerdict,
	svc string,
	explanation string,
) string {
	var b strings.Builder
	line := explanation
	if line == "" {
		line = fmt.Sprintf("%s anomaly on %s (frequency %d).",
			capitalize(verdict.String()), svc, result.Frequency)
	}
	b.WriteString(line)
	b.WriteString("\n")
	b.WriteString(heuristicMarker)

	if samples := sampleMessages(result.SampleSignals, 3); len(samples) > 0 {
		b.WriteString("\nSample signals:")
		for _, s := range samples {
			b.WriteString("\n- ")
			b.WriteString(truncateString(s, 200))
		}
	}
	return b.String()
}

// deterministicSampleIDs returns traceability ids for the finding. The pattern
// id is the stable anchor; sample signals carry no stable id of their own, so
// the pattern id is the sole entry when present.
func deterministicSampleIDs(result core.AgentResult) []string {
	if result.PatternID == "" {
		return nil
	}
	return []string{result.PatternID}
}

// capitalize upper-cases the first rune of s, leaving the rest untouched. Used
// to render a verdict label ("spike") at the start of a summary sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
