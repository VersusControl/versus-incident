package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// DetectAudit receives one audit record per emission attempt. *DetectLog
// satisfies it. A process other than the agent worker passes its own sink so
// it never rewrites the agent's detect log.
type DetectAudit interface {
	Record(*DetectEvent)
}

// DetectEmitter turns one decided finding into an incident: optional AI
// enrichment (with cache, budget and fallback to a templated finding), then
// the emitter. It is the one emission path for every agent.
//
// A detection is never dropped for lack of AI: whenever the model is not
// configured, disabled, over budget or failing, a deterministic templated
// finding goes through the same emitter the AI finding would.
//
//   - AI.Detect == nil / AIEnabled false → templated finding (emitted_basic).
//   - AI.Cache hit on ConditionKey       → reuse the prior AI finding (cached).
//   - AI.Rate.Allow false                → templated finding (emitted_basic_quota).
//   - AI.Detect.Run error                → templated finding (emitted_basic_error).
//   - AI finding                         → emitted.
//   - Emitter error on any path          → send_error.
//
// Every field is optional; the zero value emits templated findings, logs the
// would-be incident and writes no audit record.
type DetectEmitter struct {
	// AI supplies the detect analyzer, the result cache and the hourly rate
	// limiter. Only Detect, Cache and Rate are read. A caller that must hold
	// its own budget passes its own Rate.
	AI AIBundle
	// AIEnabled is read on every call so a runtime toggle applies to the next
	// emission without a restart. nil means enabled (AI.Detect decides).
	AIEnabled func(ctx context.Context) bool
	// Emitter delivers the finding. nil logs the would-be call and reports
	// the success label.
	Emitter Emitter
	// Audit records one DetectEvent per call. nil records nothing.
	Audit DetectAudit
	// Scrub redacts request evidence text before it reaches the finding or the
	// audit record. nil uses the built-in redaction rules.
	Scrub func(string) string

	// logf replaces log.Printf in tests that compare the emitted lines.
	logf func(format string, args ...any)
}

// DetectRequest is one decided finding handed to DetectEmitter.Emit.
type DetectRequest struct {
	// Source is the data-source name, SignalKind its kind (logs, metrics, ...).
	Source, SignalKind string
	// ConditionKey identifies the condition: it is the finding's pattern ID
	// and the AI cache key, so two findings share cached AI text only when
	// they share a ConditionKey. Include the subject in it when one template
	// can fire for many subjects.
	ConditionKey string
	// Title is the pattern template or signal name the finding is about.
	Title   string
	Service string

	Frequency int
	// Samples are already-redacted representative signals.
	Samples     []core.Signal
	Verdict     core.AgentVerdict
	Baseline    float64
	Score       float64
	BaselineStd float64
	Explanation string
	// Severity is the strongest severity seen across the whole observation,
	// before sampling. It raises the floor taken from Samples; empty adds
	// nothing.
	Severity string

	// Subject and Evidence are optional. When set they are copied into the
	// finding, and into the templated finding's summary; when empty the
	// output is the same as without them. A subject whose kind is unknown
	// or whose name fails core.FindingSubject.Valid is dropped; a valid
	// name is scrubbed like evidence.
	Subject  core.FindingSubject
	Evidence []core.EvidenceItem
}

var builtinEvidenceRedactor = sync.OnceValue(func() *Redactor {
	r, _ := NewRedactor(false, nil)
	return r
})

func (e *DetectEmitter) printf(format string, args ...any) {
	if e.logf != nil {
		e.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (e *DetectEmitter) scrub(s string) string {
	if e.Scrub != nil {
		return e.Scrub(s)
	}
	return builtinEvidenceRedactor().Scrub(s)
}

// checkedSubject returns the subject with its name scrubbed, or the zero
// subject when the kind is unknown or the name is not safe to show. An
// invalid subject is dropped, never an error: the finding still emits.
func (e *DetectEmitter) checkedSubject(subject core.FindingSubject) core.FindingSubject {
	if subject.IsZero() || !subject.Valid() {
		return core.FindingSubject{}
	}
	subject.Name = e.scrub(subject.Name)
	return subject
}

// Evidence bounds: a finding keeps at most maxFindingEvidence items, then one
// item saying how many were left out, and each text at most
// maxEvidenceTextBytes.
const (
	maxFindingEvidence   = 32
	maxEvidenceTextBytes = 4 << 10
)

// scrubbedEvidence returns a redacted, bounded copy; the caller's slice is
// untouched. Text is truncated after scrubbing, so a cut can never split a
// secret out of a rule's reach.
func (e *DetectEmitter) scrubbedEvidence(items []core.EvidenceItem) []core.EvidenceItem {
	if len(items) == 0 {
		return nil
	}
	kept := min(len(items), maxFindingEvidence)
	out := make([]core.EvidenceItem, kept, kept+1)
	for i, item := range items[:kept] {
		out[i] = core.EvidenceItem{
			Source:  truncateUTF8(e.scrub(item.Source), maxEvidenceTextBytes),
			Summary: truncateUTF8(e.scrub(item.Summary), maxEvidenceTextBytes),
			Detail:  truncateUTF8(e.scrub(item.Detail), maxEvidenceTextBytes),
		}
	}
	if dropped := len(items) - kept; dropped > 0 {
		out = append(out, core.EvidenceItem{Source: "detect", Summary: fmt.Sprintf("+%d more", dropped)})
	}
	return out
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Emit runs one finding through the emission path and returns its outcome
// label: "emitted", "cached", "emitted_basic", "emitted_basic_quota",
// "emitted_basic_error" or "send_error".
func (e *DetectEmitter) Emit(ctx context.Context, r DetectRequest) string {
	if e == nil {
		e = &DetectEmitter{}
	}
	ruleSeverity := strongestSeverity(r.Samples)
	if utils.SeverityRank(r.Severity) > utils.SeverityRank(ruleSeverity) {
		ruleSeverity = utils.NormalizeSeverity(r.Severity)
	}
	patternID := r.ConditionKey
	service := r.Service
	source := r.Source
	verdict := r.Verdict
	signals := r.Samples
	evidence := e.scrubbedEvidence(r.Evidence)
	r.Subject = e.checkedSubject(r.Subject)

	result := core.AgentResult{
		Verdict:           verdict,
		PatternID:         patternID,
		Template:          r.Title,
		SampleSignals:     signals,
		Frequency:         r.Frequency,
		Baseline:          r.Baseline,
		AgentKind:         "detect",
		SignalKind:        r.SignalKind,
		DetectionEmission: &core.DetectionEmissionResult{},
		RuleSeverity:      ruleSeverity,
	}

	// Build a partial DetectEvent up front so every outcome path can
	// stamp itself into the audit log.
	evt := &DetectEvent{
		Source:       source,
		PatternID:    patternID,
		Template:     r.Title,
		Service:      service,
		Verdict:      verdict.String(),
		Frequency:    r.Frequency,
		Baseline:     r.Baseline,
		BaselineStd:  r.BaselineStd,
		Score:        r.Score,
		Explanation:  r.Explanation,
		Samples:      sampleMessages(signals, 3),
		RuleSeverity: result.RuleSeverity,
	}

	templated := func() *core.AIFinding {
		f := deterministicFinding(result, verdict, service, r.Score, r.BaselineStd, r.Explanation)
		addSubjectAndEvidence(f, r.Subject, evidence, len(r.Evidence), true)
		return f
	}

	// 1. AI unavailable — analyzer not configured, or a runtime resolver
	// disabled AI for this call. Detection already happened AI-free, so build
	// a deterministic (templated) finding and emit it: a real anomaly must
	// never silently drop just because AI is off.
	if e.AI.Detect == nil || (e.AIEnabled != nil && !e.AIEnabled(ctx)) {
		e.printf("%sagent[detect:basic]: templated alert pattern=%q service=%q verdict=%s freq=%d %s",
			colorGreen, patternID, service, verdict, len(signals), colorReset)
		finding := templated()
		evt.Finding = finding
		evt.Model = "heuristic"
		outcome := e.send(finding, result, source, service, "emitted_basic")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error"
		}
		e.record(evt, result.DetectionEmission)
		return outcome
	}

	// 2. Cache hit — reuse the prior finding.
	if cached, ok := e.AI.Cache.Get(patternID); ok {
		e.printf("%sagent[detect]: cache hit pattern=%q verdict=%s freq=%d%s",
			colorGreen, patternID, verdict, len(signals), colorReset)
		addSubjectAndEvidence(cached, r.Subject, evidence, len(r.Evidence), false)
		evt.Finding = cached
		outcome := e.send(cached, result, source, service, "cached")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error (see logs)"
		}
		e.record(evt, result.DetectionEmission)
		return outcome
	}

	// 3. Rate limit guard. AI is over its hourly cap — fall back to the
	// deterministic finding so a transient budget exhaustion never drops a
	// page.
	if !e.AI.Rate.Allow() {
		e.printf("agent[detect]: AI quota exceeded; templated alert pattern=%q freq=%d", patternID, len(signals))
		finding := templated()
		evt.Finding = finding
		evt.Model = "heuristic"
		outcome := e.send(finding, result, source, service, "emitted_basic_quota")
		evt.Outcome = outcome
		if outcome == "send_error" {
			evt.Error = "emitter returned error"
		}
		e.record(evt, result.DetectionEmission)
		return outcome
	}

	// 4. Real AI call. On error, fall back to the deterministic finding so a
	// transient AI outage never drops a page.
	call, err := e.AI.Detect.Run(ctx, core.DetectTask{Result: result})
	if err != nil {
		e.printf("agent[detect]: AI failed pattern=%q: %v; sending templated alert", patternID, err)
		finding := templated()
		evt.Finding = finding
		evt.Model = "heuristic"
		evt.Error = err.Error()
		outcome := e.send(finding, result, source, service, "emitted_basic_error")
		evt.Outcome = outcome
		e.record(evt, result.DetectionEmission)
		return outcome
	}
	finding := call.Finding
	// Cache the model's own text; subject and evidence belong to this
	// request and are attached after.
	e.AI.Cache.Put(patternID, finding)
	addSubjectAndEvidence(finding, r.Subject, evidence, len(r.Evidence), false)

	evt.Finding = finding
	evt.Model = call.Model
	evt.UserPrompt = call.UserPrompt
	evt.RawResponse = call.RawResponse
	evt.DurationMs = call.DurationMs

	outcome := e.send(finding, result, source, service, "emitted")
	evt.Outcome = outcome
	if outcome == "send_error" {
		evt.Error = "emitter returned error (see logs)"
	}
	e.record(evt, result.DetectionEmission)
	return outcome
}

func (e *DetectEmitter) record(event *DetectEvent, emission *core.DetectionEmissionResult) {
	stampDetectEmission(event, emission)
	if e.Audit != nil {
		e.Audit.Record(event)
	}
}

// send delegates to the configured emitter and translates errors into
// the outcome vocabulary.
func (e *DetectEmitter) send(finding *core.AIFinding, result core.AgentResult, source, service, okLabel string) string {
	// Honour an operator-declared severity as a floor: the AI may escalate
	// but must not silently demote below the rule-declared severity.
	clampSeverityFloor(finding, result.RuleSeverity)
	if e.Emitter == nil {
		e.printf("%sagent[detect]: pattern=%q service=%q severity=%q confidence=%.2f title=%q (no emitter wired)%s",
			colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
			truncateString(finding.Title, 80), colorReset)
		return okLabel
	}
	if err := e.Emitter(finding, result, source, service); err != nil {
		e.printf("agent[detect]: emit failed pattern=%q: %v", result.PatternID, err)
		return "send_error"
	}
	e.printf("%sagent[detect]: emitted pattern=%q service=%q severity=%q confidence=%.2f title=%q%s",
		colorGreen, result.PatternID, service, finding.Severity, finding.Confidence,
		truncateString(finding.Title, 80), colorReset)
	return okLabel
}
