package agent

import (
	"context"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// emitDetect handles one Unknown / Spike pattern in detect mode through the
// worker's DetectEmitter and returns its outcome label ("emitted" | "cached" |
// "emitted_basic" | "emitted_basic_quota" | "emitted_basic_error" |
// "send_error"), used as a stat suffix in the tick log.
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
	request := DetectRequest{
		Source:       source,
		SignalKind:   signalKind,
		ConditionKey: patternID,
		Title:        template,
		Service:      service,
		Frequency:    frequency,
		Samples:      signals,
		Verdict:      verdict,
		Baseline:     prevBaseline,
		Score:        score,
		BaselineStd:  baselineStd,
		Explanation:  explanation,
	}
	if len(fullObservationSeverity) > 0 {
		request.Severity = fullObservationSeverity[0]
	}
	emitter := w.detectEmitter
	if emitter == nil {
		emitter = w.newDetectEmitter()
	}
	return emitter.Emit(ctx, request)
}

// newDetectEmitter wires the worker's AI bundle, live AI toggle, emitter and
// detect log into the shared emission path. The toggle and the detect log are
// read through the worker on every call, so a runtime change applies to the
// next emission. Evidence is scrubbed with the worker's redactor, the same
// rules its samples went through; without one the built-in rules apply.
func (w *Worker) newDetectEmitter() *DetectEmitter {
	e := &DetectEmitter{
		AI:        w.ai,
		AIEnabled: w.effectiveAIEnabled,
		Emitter:   w.emitter,
		Audit:     workerDetectAudit{w: w},
	}
	if w.redactor != nil {
		e.Scrub = w.redactor.Scrub
	}
	return e
}

// workerDetectAudit records into whichever detect log the worker holds at the
// time of the call (nil records nothing).
type workerDetectAudit struct{ w *Worker }

func (a workerDetectAudit) Record(event *DetectEvent) { a.w.detect.Record(event) }

func (w *Worker) recordDetectEvent(event *DetectEvent, emission *core.DetectionEmissionResult) {
	stampDetectEmission(event, emission)
	w.detect.Record(event)
}

// stampDetectEmission copies the episode and notification result onto the
// audit record.
func stampDetectEmission(event *DetectEvent, emission *core.DetectionEmissionResult) {
	if emission != nil {
		event.EpisodeID = emission.EpisodeID
		event.IncidentID = emission.IncidentID
		event.OccurrenceDelta = emission.OccurrenceDelta
		event.OccurrenceCount = emission.OccurrenceCount
		event.EpisodeAction = emission.EpisodeAction
		event.NotificationOutcome = emission.NotificationOutcome
		event.EpisodeError = emission.EpisodeError
	}
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
