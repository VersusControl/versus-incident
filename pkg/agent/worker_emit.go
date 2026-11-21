package agent

import (
	"context"
	"log"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// emitDetect handles one Unknown / Spike pattern in detect mode. Returns
// a short outcome label ("emitted" | "cached" | "emitted_basic" |
// "emitted_basic_quota" | "emitted_basic_error" | "send_error") used as a
// stat suffix in the tick log.
//
// A brain-detected anomaly is NEVER silently dropped: detection is AI-free, so
// whenever AI is unavailable the worker builds a deterministic (templated)
// finding and sends it through the exact same emitter path the AI finding uses.
// AI is an enrichment layer on top of that finding, not the switch that decides
// whether anyone is paged.
//
//   - w.ai.Detect == nil / AI disabled → deterministic finding → emit (emitted_basic).
//   - w.ai.Cache hit                   → reuse the prior AI finding (cached).
//   - w.ai.Rate.Allow == false         → deterministic finding → emit (emitted_basic_quota).
//   - w.ai.Detect.Run error            → deterministic finding → emit (emitted_basic_error).
//   - w.emitter == nil                 → still no incident is sent; send() logs the
//     would-be call and returns the ok label (shadow-of-detect).
func (w *Worker) emitDetect(
	ctx context.Context,
	source, signalKind, patternID, template, service string,
	frequency int,
	signals []core.Signal,
	verdict core.AgentVerdict,
	prevBaseline float64,
	score, baselineStd float64,
	explanation string,
	fullObservationSeverity ...string,
) string {
	ruleSeverity := strongestSeverity(signals)
	if len(fullObservationSeverity) > 0 && utils.SeverityRank(fullObservationSeverity[0]) > utils.SeverityRank(ruleSeverity) {
		ruleSeverity = utils.NormalizeSeverity(fullObservationSeverity[0])
	}
	result := core.AgentResult{
		Verdict:           verdict,
		PatternID:         patternID,
		Template:          template,
		SampleSignals:     signals,
		Frequency:         frequency,
		Baseline:          prevBaseline,
		AgentKind:         "detect",
		SignalKind:        signalKind,
		DetectionEmission: &core.DetectionEmissionResult{},
		RuleSeverity:      ruleSeverity,
	}

	// Build a partial DetectEvent up front so every outcome path can
	// stamp itself into the audit log.
	evt := &DetectEvent{
		Source:       source,
		PatternID:    patternID,
		Template:     template,
		Service:      service,
		Verdict:      verdict.String(),
		Frequency:    frequency,
		Baseline:     prevBaseline,
		BaselineStd:  baselineStd,
		Score:        score,
		Explanation:  explanation,
		Samples:      sampleMessages(signals, 3),
		RuleSeverity: result.RuleSeverity,
	}

	// 1. AI unavailable — analyzer not configured, or a runtime resolver
	// disabled AI for this tick. Detection already happened AI-free, so build
	// a deterministic (templated) finding and emit it: a real anomaly must
	// never silently drop just because AI is off. The resolver is read live so
	// an enterprise off→on toggle takes effect on the next call without a
	// restart; OSS registers no resolver so this collapses to the original
	// `w.ai.Detect == nil` check.
	if w.ai.Detect == nil || !w.effectiveAIEnabled(ctx) {
		log.Printf("%sagent[detect:basic]: templated alert pattern=%q service=%q verdict=%s freq=%d %s",
			colorGreen, patternID, service, verdict, len(signals), colorReset)
		finding := deterministicFinding(result, verdict, service, score, baselineStd, explanation)
		evt.Finding = finding
		evt.Model = "heuristic"
		outcome := w.send(finding, result, source, service, "emitted_basic")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error"
		}
		w.recordDetectEvent(evt, result.DetectionEmission)
		return outcome
	}

	// 2. Cache hit — reuse the prior finding.
	if cached, ok := w.ai.Cache.Get(patternID); ok {
		log.Printf("%sagent[detect]: cache hit pattern=%q verdict=%s freq=%d%s",
			colorGreen, patternID, verdict, len(signals), colorReset)
		evt.Finding = cached
		outcome := w.send(cached, result, source, service, "cached")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error (see logs)"
		}
		w.recordDetectEvent(evt, result.DetectionEmission)
		return outcome
	}

	// 3. Rate limit guard. AI is over its hourly cap — fall back to the
	// deterministic finding so a transient budget exhaustion never drops a
	// page.
	if !w.ai.Rate.Allow() {
		log.Printf("agent[detect]: AI quota exceeded; templated alert pattern=%q freq=%d", patternID, len(signals))
		finding := deterministicFinding(result, verdict, service, score, baselineStd, explanation)
		evt.Finding = finding
		evt.Model = "heuristic"
		outcome := w.send(finding, result, source, service, "emitted_basic_quota")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error"
		}
		w.recordDetectEvent(evt, result.DetectionEmission)
		return outcome
	}

	// 4. Real AI call. On error, fall back to the deterministic finding so a
	// transient AI outage never drops a page.
	call, err := w.ai.Detect.Run(ctx, core.DetectTask{Result: result})
	if err != nil {
		log.Printf("agent[detect]: AI failed pattern=%q: %v; sending templated alert", patternID, err)
		finding := deterministicFinding(result, verdict, service, score, baselineStd, explanation)
		evt.Finding = finding
		evt.Model = "heuristic"
		evt.Error = err.Error()
		outcome := w.send(finding, result, source, service, "emitted_basic_error")
		evt.Outcome = outcome
		w.recordDetectEvent(evt, result.DetectionEmission)
		return outcome
	}
	finding := call.Finding
	w.ai.Cache.Put(patternID, finding)

	evt.Finding = finding
	evt.Model = call.Model
	evt.UserPrompt = call.UserPrompt
	evt.RawResponse = call.RawResponse
	evt.DurationMs = call.DurationMs

	outcome := w.send(finding, result, source, service, "emitted")
	evt.Outcome = outcome
	if outcome == "send_error" {
		evt.Error = "emitter returned error (see logs)"
	}
	w.recordDetectEvent(evt, result.DetectionEmission)
	return outcome
}

func (w *Worker) recordDetectEvent(event *DetectEvent, emission *core.DetectionEmissionResult) {
	if emission != nil {
		event.EpisodeID = emission.EpisodeID
		event.IncidentID = emission.IncidentID
		event.OccurrenceDelta = emission.OccurrenceDelta
		event.OccurrenceCount = emission.OccurrenceCount
		event.EpisodeAction = emission.EpisodeAction
		event.NotificationOutcome = emission.NotificationOutcome
		event.EpisodeError = emission.EpisodeError
	}
	w.detect.Record(event)
}

// send delegates to the configured emitter and translates errors into
// the worker's outcome vocabulary.
func (w *Worker) send(finding *core.AIFinding, result core.AgentResult, source, service, okLabel string) string {
	// Honour an operator-declared severity as a floor: the AI may escalate
	// but must not silently demote below the rule-declared severity.
	clampSeverityFloor(finding, result.RuleSeverity)
	if w.emitter == nil {
		log.Printf("%sagent[detect]: pattern=%q service=%q severity=%q confidence=%.2f title=%q (no emitter wired)%s",
			colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
			truncateString(finding.Title, 80), colorReset)
		return okLabel
	}
	if err := w.emitter(finding, result, source, service); err != nil {
		log.Printf("agent[detect]: emit failed pattern=%q: %v", result.PatternID, err)
		return "send_error"
	}
	log.Printf("%sagent[detect]: emitted pattern=%q service=%q severity=%q confidence=%.2f title=%q%s",
		colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
		truncateString(finding.Title, 80), colorReset)
	return okLabel
}

// sampleMessages returns up to `max` non-empty Message fields from
// signals — used to attach a few representative log lines to the
// detect audit log.
func sampleMessages(signals []core.Signal, max int) []string {
	if max <= 0 {
		return nil
	}
	out := make([]string, 0, max)
	for _, s := range signals {
		if s.Message == "" {
			continue
		}
		out = append(out, s.Message)
		if len(out) == max {
			break
		}
	}
	return out
}

// strongestSeverity returns the highest-ranked operator-declared severity
// carried by the grouped signals (the rule's `severity`), normalized to a
// canonical value. Returns "" when no signal carries a recognised declared
// severity (e.g. auto-discovered metric signals or log signals), so the
// downstream floor is simply absent and the AI decides.
func strongestSeverity(signals []core.Signal) string {
	best := ""
	bestRank := 0
	for _, s := range signals {
		if r := utils.SeverityRank(s.Severity); r > bestRank {
			bestRank = r
			best = utils.NormalizeSeverity(s.Severity)
		}
	}
	return best
}

func observationSeverity(observation core.Observation) string {
	if utils.SeverityRank(observation.StrongestSeverity) > 0 {
		return utils.NormalizeSeverity(observation.StrongestSeverity)
	}
	return strongestSeverity(observation.Samples)
}

// clampSeverityFloor raises finding.Severity up to floor when floor is a
// recognised severity stronger than the AI's verdict. It never demotes — an
// AI escalation above the floor is preserved. A no-op when floor is empty.
func clampSeverityFloor(f *core.AIFinding, floor string) {
	if f == nil || floor == "" {
		return
	}
	if utils.SeverityRank(floor) > utils.SeverityRank(f.Severity) {
		f.Severity = utils.NormalizeSeverity(floor)
	}
}
