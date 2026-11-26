package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// captureAudit is a DetectAudit that keeps every record in memory.
type captureAudit struct{ events []*DetectEvent }

func (c *captureAudit) Record(e *DetectEvent) { c.events = append(c.events, e) }

// captureLines collects formatted log lines in place of log.Printf.
type captureLines struct{ lines []string }

func (c *captureLines) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// emitCase is one outcome of the emission path with fresh fixtures, so the
// same case can run through the worker and through DetectEmitter directly.
type emitCase struct {
	name string
	// build returns a fresh AI bundle, emitter and AI toggle, plus counters.
	build func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters)

	wantOutcome    string
	wantModel      string
	wantError      string
	wantModelCalls int32
	wantEmits      int32
	wantTemplated  bool
	wantFindTitle  string
	wantLogs       func(r DetectRequest) []string
}

type emitCounters struct {
	model *fakeAgent
	emits atomic.Int32
}

func emitRequest() DetectRequest {
	return DetectRequest{
		Source: "loki-prod", SignalKind: "logs", ConditionKey: "cond-1", Title: "db timeout <*>",
		Service: "checkout", Frequency: 7,
		Samples:  []core.Signal{{Message: "db timeout 30s", Source: "checkout", Severity: "low"}, {Message: "db timeout 31s"}},
		Verdict:  core.VerdictSpike,
		Baseline: 2, Score: 4.5, BaselineStd: 1, Explanation: "7/s = 4.5σ above 2/s ± 1",
		Severity: "high",
	}
}

func aiFinding() *core.AIFinding {
	return &core.AIFinding{Title: "Checkout DB timeouts", Summary: "pool exhausted", Severity: "medium", Confidence: 0.8}
}

func countingEmitter(c *emitCounters, err error) Emitter {
	return func(*core.AIFinding, core.AgentResult, string, string) error {
		c.emits.Add(1)
		return err
	}
}

// templatedEmittedLine is the emitter's success line for the templated
// finding the request produces.
func templatedEmittedLine(r DetectRequest) string {
	result := core.AgentResult{PatternID: r.ConditionKey, Template: r.Title, SampleSignals: r.Samples,
		Frequency: r.Frequency, Baseline: r.Baseline, SignalKind: r.SignalKind, RuleSeverity: "high"}
	return emittedLine(r, deterministicFinding(result, r.Verdict, r.Service, r.Score, r.BaselineStd, r.Explanation))
}

func templatedLogs(r DetectRequest) []string {
	return []string{
		fmt.Sprintf("%sagent[detect:basic]: templated alert pattern=%q service=%q verdict=%s freq=%d %s",
			colorGreen, r.ConditionKey, r.Service, r.Verdict, len(r.Samples), colorReset),
		templatedEmittedLine(r),
	}
}

func emittedLine(r DetectRequest, f *core.AIFinding) string {
	return fmt.Sprintf("%sagent[detect]: emitted pattern=%q service=%q severity=%q confidence=%.2f title=%q%s",
		colorGreen, r.ConditionKey, r.Service, f.Severity, f.Confidence, truncateString(f.Title, 80), colorReset)
}

// emitCases lists the six outcome labels plus AI turned off at runtime. The
// labels, model names, error texts and log lines are the agent's existing
// contract: the detect log reader, the tick stats and dashboards read them.
func emitCases() []emitCase {
	return []emitCase{
		{
			name: "AI not configured",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{}
				return AIBundle{}, countingEmitter(c, nil), nil, c
			},
			wantOutcome: "emitted_basic", wantModel: "heuristic", wantEmits: 1, wantTemplated: true,
			wantLogs: templatedLogs,
		},
		{
			name: "AI turned off at runtime",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{finding: aiFinding()}}
				return AIBundle{Detect: c.model}, countingEmitter(c, nil), func(context.Context) bool { return false }, c
			},
			wantOutcome: "emitted_basic", wantModel: "heuristic", wantEmits: 1, wantTemplated: true,
			wantLogs: templatedLogs,
		},
		{
			name: "cache hit",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{finding: aiFinding()}}
				cache := ai.NewResultCache(time.Hour, nil)
				cache.Put("cond-1", &core.AIFinding{Title: "Cached title", Severity: "low", Confidence: 0.4})
				return AIBundle{Detect: c.model, Cache: cache}, countingEmitter(c, nil), nil, c
			},
			wantOutcome: "cached", wantEmits: 1, wantFindTitle: "Cached title",
			wantLogs: func(r DetectRequest) []string {
				return []string{
					fmt.Sprintf("%sagent[detect]: cache hit pattern=%q verdict=%s freq=%d%s", colorGreen, r.ConditionKey, r.Verdict, len(r.Samples), colorReset),
					emittedLine(r, &core.AIFinding{Title: "Cached title", Severity: "high", Confidence: 0.4}),
				}
			},
		},
		{
			name: "over the AI budget",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{finding: aiFinding()}}
				rate := ai.NewRateLimiter(1)
				rate.Allow()
				return AIBundle{Detect: c.model, Rate: rate}, countingEmitter(c, nil), nil, c
			},
			wantOutcome: "emitted_basic_quota", wantModel: "heuristic", wantEmits: 1, wantTemplated: true,
			wantLogs: func(r DetectRequest) []string {
				return []string{fmt.Sprintf("agent[detect]: AI quota exceeded; templated alert pattern=%q freq=%d", r.ConditionKey, len(r.Samples)),
					templatedEmittedLine(r)}
			},
		},
		{
			name: "AI error",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{err: errors.New("model unavailable")}}
				return AIBundle{Detect: c.model}, countingEmitter(c, nil), nil, c
			},
			wantOutcome: "emitted_basic_error", wantModel: "heuristic", wantError: "model unavailable",
			wantModelCalls: 1, wantEmits: 1, wantTemplated: true,
			wantLogs: func(r DetectRequest) []string {
				return []string{fmt.Sprintf("agent[detect]: AI failed pattern=%q: %v; sending templated alert", r.ConditionKey, "model unavailable"),
					templatedEmittedLine(r)}
			},
		},
		{
			name: "AI finding",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{finding: aiFinding()}}
				return AIBundle{Detect: c.model}, countingEmitter(c, nil), func(context.Context) bool { return true }, c
			},
			wantOutcome: "emitted", wantModel: "fake-model", wantModelCalls: 1, wantEmits: 1,
			wantFindTitle: "Checkout DB timeouts",
			wantLogs: func(r DetectRequest) []string {
				f := aiFinding()
				f.Severity = "high"
				return []string{emittedLine(r, f)}
			},
		},
		{
			name: "emitter error",
			build: func() (AIBundle, Emitter, func(context.Context) bool, *emitCounters) {
				c := &emitCounters{model: &fakeAgent{finding: aiFinding()}}
				return AIBundle{Detect: c.model}, countingEmitter(c, errors.New("channel down")), nil, c
			},
			wantOutcome: "send_error", wantModel: "fake-model", wantError: "emitter returned error (see logs)",
			wantModelCalls: 1, wantEmits: 1, wantFindTitle: "Checkout DB timeouts",
			wantLogs: func(r DetectRequest) []string {
				return []string{fmt.Sprintf("agent[detect]: emit failed pattern=%q: %v", r.ConditionKey, "channel down")}
			},
		},
	}
}

func checkEmitCase(t *testing.T, tc emitCase, outcome string, c *emitCounters, events []*DetectEvent, lines []string, r DetectRequest) {
	t.Helper()
	if outcome != tc.wantOutcome {
		t.Fatalf("outcome = %q, want %q", outcome, tc.wantOutcome)
	}
	if c.model != nil && atomic.LoadInt32(&c.model.calls) != tc.wantModelCalls {
		t.Fatalf("model calls = %d, want %d", c.model.calls, tc.wantModelCalls)
	}
	if c.emits.Load() != tc.wantEmits {
		t.Fatalf("emitter calls = %d, want %d", c.emits.Load(), tc.wantEmits)
	}
	if len(events) != 1 {
		t.Fatalf("audit records = %d, want 1", len(events))
	}
	evt := events[0]
	if evt.Outcome != tc.wantOutcome || evt.Model != tc.wantModel || evt.Error != tc.wantError {
		t.Fatalf("audit outcome/model/error = %q/%q/%q, want %q/%q/%q",
			evt.Outcome, evt.Model, evt.Error, tc.wantOutcome, tc.wantModel, tc.wantError)
	}
	if evt.PatternID != r.ConditionKey || evt.Template != r.Title || evt.Source != r.Source || evt.Service != r.Service ||
		evt.Verdict != "spike" || evt.Frequency != r.Frequency || evt.Baseline != r.Baseline || evt.Score != r.Score ||
		evt.BaselineStd != r.BaselineStd || evt.Explanation != r.Explanation || evt.RuleSeverity != "high" ||
		!reflect.DeepEqual(evt.Samples, []string{"db timeout 30s", "db timeout 31s"}) {
		t.Fatalf("audit record context = %+v", evt)
	}
	f := evt.Finding
	if f == nil {
		t.Fatal("audit record has no finding")
	}
	// The severity floor holds on every path: the full observation said high.
	if f.Severity != "high" && f.Severity != "critical" {
		t.Fatalf("finding severity = %q, want at least high", f.Severity)
	}
	if tc.wantTemplated {
		if f.Category != "anomaly" || !strings.Contains(f.Summary, heuristicMarker) {
			t.Fatalf("templated finding = %+v", f)
		}
	} else if f.Title != tc.wantFindTitle {
		t.Fatalf("finding title = %q, want %q", f.Title, tc.wantFindTitle)
	}
	if want := tc.wantLogs(r); !reflect.DeepEqual(lines, want) {
		t.Fatalf("log lines:\n got %q\nwant %q", lines, want)
	}
}

func TestDetectEmitter_EveryOutcome(t *testing.T) {
	for _, tc := range emitCases() {
		t.Run(tc.name, func(t *testing.T) {
			bundle, emitter, enabled, counters := tc.build()
			audit := &captureAudit{}
			lines := &captureLines{}
			e := &DetectEmitter{AI: bundle, AIEnabled: enabled, Emitter: emitter, Audit: audit, logf: lines.logf}
			r := emitRequest()
			outcome := e.Emit(context.Background(), r)
			checkEmitCase(t, tc, outcome, counters, audit.events, lines.lines, r)
		})
	}
}

// normalizedEvents drops the fields the detect log assigns at record time.
func normalizedEvents(events []*DetectEvent) []DetectEvent {
	out := make([]DetectEvent, len(events))
	for i, e := range events {
		out[i] = *e
		out[i].ID = ""
		out[i].Timestamp = time.Time{}
	}
	return out
}

// TestDetectEmitter_WorkerMatchesDirectEmitter runs every outcome through the
// worker's detect path and through a DetectEmitter built by hand from the
// same inputs. Both must give the same label, finding, audit record and log
// lines, and both must match the fixed expectations of the existing agent.
func TestDetectEmitter_WorkerMatchesDirectEmitter(t *testing.T) {
	for _, tc := range emitCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := emitRequest()

			bundle, emitter, enabled, workerCounters := tc.build()
			w := newWorkerForTest(t, bundle, emitter)
			if enabled != nil && !enabled(context.Background()) {
				// The worker reads its toggle from the agent config: an
				// enabled analyzer with no usable credential is off.
				w.cfg.AI = config.AgentAIConfig{Enable: true, Provider: "openai"}
			}
			workerLog, err := LoadDetectLog(storage.NewMemory(), 10)
			if err != nil {
				t.Fatalf("LoadDetectLog: %v", err)
			}
			w.detect = workerLog
			workerLines := &captureLines{}
			w.detectEmitter.logf = workerLines.logf
			workerOutcome := w.emitDetect(context.Background(), r.Source, r.SignalKind, r.ConditionKey, r.Title, r.Service,
				r.Frequency, r.Samples, r.Verdict, r.Baseline, r.Score, r.BaselineStd, r.Explanation, r.Severity)
			checkEmitCase(t, tc, workerOutcome, workerCounters, workerLog.All(), workerLines.lines, r)

			bundle, emitter, enabled, directCounters := tc.build()
			directLog, err := LoadDetectLog(storage.NewMemory(), 10)
			if err != nil {
				t.Fatalf("LoadDetectLog: %v", err)
			}
			directLines := &captureLines{}
			direct := &DetectEmitter{AI: bundle, AIEnabled: enabled, Emitter: emitter, Audit: directLog, logf: directLines.logf}
			directOutcome := direct.Emit(context.Background(), r)
			checkEmitCase(t, tc, directOutcome, directCounters, directLog.All(), directLines.lines, r)

			if workerOutcome != directOutcome {
				t.Fatalf("worker outcome %q != direct outcome %q", workerOutcome, directOutcome)
			}
			if got, want := normalizedEvents(workerLog.All()), normalizedEvents(directLog.All()); !reflect.DeepEqual(got, want) {
				t.Fatalf("audit records differ:\nworker %+v\ndirect %+v", got, want)
			}
			if !reflect.DeepEqual(workerLines.lines, directLines.lines) {
				t.Fatalf("log lines differ:\nworker %q\ndirect %q", workerLines.lines, directLines.lines)
			}
		})
	}
}

func evidenceRequest() DetectRequest {
	r := emitRequest()
	r.Subject = core.FindingSubject{Kind: core.FindingSubjectPod, Name: "checkout/checkout-7d9f"}
	r.Evidence = []core.EvidenceItem{
		{Source: "metrics", Summary: "restarts 0 -> 5 in 10m", Detail: "baseline 0.1/h"},
		{Source: "events", Summary: "BackOff restarting failed container"},
	}
	return r
}

func TestDetectEmitter_CarriesSubjectAndEvidence(t *testing.T) {
	cases := []struct {
		name      string
		bundle    func() AIBundle
		outcome   string
		templated bool
	}{
		{"templated", func() AIBundle { return AIBundle{} }, "emitted_basic", true},
		{"AI finding", func() AIBundle { return AIBundle{Detect: &fakeAgent{finding: aiFinding()}} }, "emitted", false},
		{"cached AI finding", func() AIBundle {
			cache := ai.NewResultCache(time.Hour, nil)
			cache.Put("cond-1", aiFinding())
			return AIBundle{Detect: &fakeAgent{finding: aiFinding()}, Cache: cache}
		}, "cached", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := evidenceRequest()
			callerEvidence := append([]core.EvidenceItem(nil), r.Evidence...)
			var sent *core.AIFinding
			audit := &captureAudit{}
			e := &DetectEmitter{AI: tc.bundle(), Audit: audit, logf: (&captureLines{}).logf,
				Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
			if got := e.Emit(context.Background(), r); got != tc.outcome {
				t.Fatalf("outcome = %q, want %q", got, tc.outcome)
			}
			if sent == nil || sent.Subject != r.Subject || !reflect.DeepEqual(sent.Evidence, r.Evidence) {
				t.Fatalf("finding subject/evidence = %+v / %+v", sent.Subject, sent.Evidence)
			}
			if audit.events[0].Finding != sent {
				t.Fatal("audit record does not carry the sent finding")
			}
			if !reflect.DeepEqual(r.Evidence, callerEvidence) {
				t.Fatal("caller's evidence slice was modified")
			}
			hasText := strings.Contains(sent.Summary, "\nSubject: pod checkout/checkout-7d9f") &&
				strings.Contains(sent.Summary, "\nEvidence:\n- metrics: restarts 0 -> 5 in 10m (baseline 0.1/h)\n- events: BackOff restarting failed container")
			if hasText != tc.templated {
				t.Fatalf("summary text with subject/evidence = %v, want %v: %q", hasText, tc.templated, sent.Summary)
			}
			if !tc.templated && sent.Summary != "pool exhausted" {
				t.Fatalf("AI summary changed: %q", sent.Summary)
			}
			if cached, ok := e.AI.Cache.Get("cond-1"); ok && (len(cached.Evidence) != 0 || !cached.Subject.IsZero()) {
				t.Fatalf("cache holds request evidence: %+v", cached)
			}
		})
	}
}

// Without a subject or evidence the finding and its JSON are exactly what
// they were before either existed.
func TestDetectEmitter_EmptySubjectAndEvidenceChangeNothing(t *testing.T) {
	r := emitRequest()
	f := deterministicFinding(core.AgentResult{PatternID: r.ConditionKey, Template: r.Title, SampleSignals: r.Samples,
		Frequency: r.Frequency, Baseline: r.Baseline, RuleSeverity: "high"}, r.Verdict, r.Service, r.Score, r.BaselineStd, r.Explanation)
	before, _ := json.Marshal(f)
	addSubjectAndEvidence(f, core.FindingSubject{}, nil, 0, true)
	after, _ := json.Marshal(f)
	if string(before) != string(after) || strings.Contains(string(after), "subject") || strings.Contains(string(after), "evidence") {
		t.Fatalf("finding JSON changed:\n%s\n%s", before, after)
	}
}

func TestDetectEmitter_ScrubsEvidenceText(t *testing.T) {
	const canary = "hunter2canary"
	request := func() DetectRequest {
		r := emitRequest()
		r.Evidence = []core.EvidenceItem{{Source: "events", Summary: "auth failed password=" + canary, Detail: "token: " + canary}}
		return r
	}
	customRedactor, errs := NewRedactor(false, []string{`custom-[0-9]+`})
	if len(errs) > 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	cases := []struct {
		name    string
		emitter func(audit DetectAudit, emit Emitter) *DetectEmitter
		extra   string
	}{
		{"built-in rules when no scrubber is set", func(audit DetectAudit, emit Emitter) *DetectEmitter {
			return &DetectEmitter{Audit: audit, Emitter: emit}
		}, ""},
		{"caller scrubber", func(audit DetectAudit, emit Emitter) *DetectEmitter {
			return &DetectEmitter{Audit: audit, Emitter: emit, Scrub: func(s string) string { return strings.ReplaceAll(s, canary, "[gone]") }}
		}, ""},
		{"worker wires its redactor", func(audit DetectAudit, emit Emitter) *DetectEmitter {
			w := &Worker{redactor: customRedactor, emitter: emit}
			e := w.newDetectEmitter()
			e.Audit = audit
			return e
		}, "custom-12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			if tc.extra != "" {
				r.Evidence[0].Detail += " " + tc.extra
			}
			var sent *core.AIFinding
			audit := &captureAudit{}
			e := tc.emitter(audit, func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil })
			e.logf = (&captureLines{}).logf
			if got := e.Emit(context.Background(), r); got != "emitted_basic" {
				t.Fatalf("outcome = %q", got)
			}
			blob, err := json.Marshal(audit.events)
			if err != nil {
				t.Fatalf("marshal audit: %v", err)
			}
			for _, secret := range []string{canary, tc.extra} {
				if secret == "" {
					continue
				}
				if strings.Contains(sent.Summary, secret) || strings.Contains(fmt.Sprint(sent.Evidence), secret) || strings.Contains(string(blob), secret) {
					t.Fatalf("secret %q reached the finding or audit record: %+v", secret, sent)
				}
			}
			if !strings.Contains(r.Evidence[0].Summary, canary) {
				t.Fatal("caller's evidence was modified in place")
			}
		})
	}
}

func TestDetectEmitter_NilAuditAndEmitterDoNotPanic(t *testing.T) {
	r := evidenceRequest()
	lines := &captureLines{}
	if got := (&DetectEmitter{logf: lines.logf}).Emit(context.Background(), r); got != "emitted_basic" {
		t.Fatalf("zero emitter outcome = %q", got)
	}
	if len(lines.lines) != 2 || !strings.Contains(lines.lines[1], "(no emitter wired)") {
		t.Fatalf("would-be call not logged: %q", lines.lines)
	}
	if got := (&DetectEmitter{AI: AIBundle{Detect: &fakeAgent{finding: aiFinding()}}, logf: lines.logf}).Emit(context.Background(), r); got != "emitted" {
		t.Fatalf("AI emitter without sink outcome = %q", got)
	}
	var nilEmitter *DetectEmitter
	if got := nilEmitter.Emit(context.Background(), emitRequest()); got != "emitted_basic" {
		t.Fatalf("nil emitter outcome = %q", got)
	}
}

// Each emitter spends only the limiter it was given, so a second process can
// hold its own AI budget.
func TestDetectEmitter_HonoursCallerRateLimiter(t *testing.T) {
	newEmitter := func(rate *ai.RateLimiter) (*DetectEmitter, *fakeAgent) {
		model := &fakeAgent{finding: aiFinding()}
		return &DetectEmitter{AI: AIBundle{Detect: model, Rate: rate}, logf: (&captureLines{}).logf}, model
	}
	first, firstModel := newEmitter(ai.NewRateLimiter(1))
	second, secondModel := newEmitter(ai.NewRateLimiter(1))

	r := emitRequest()
	if got := first.Emit(context.Background(), r); got != "emitted" {
		t.Fatalf("first emitter, call 1 = %q", got)
	}
	r.ConditionKey = "cond-2"
	if got := first.Emit(context.Background(), r); got != "emitted_basic_quota" {
		t.Fatalf("first emitter, call 2 = %q, want emitted_basic_quota", got)
	}
	if got := second.Emit(context.Background(), r); got != "emitted" {
		t.Fatalf("second emitter = %q, want emitted (its own budget)", got)
	}
	if firstModel.calls != 1 || secondModel.calls != 1 {
		t.Fatalf("model calls = %d/%d, want 1/1", firstModel.calls, secondModel.calls)
	}
}

// The AI cache is keyed by the request's condition key: two subjects of one
// template get their own AI text, a repeat of one subject reuses its own.
func TestDetectEmitter_CacheKeyIsConditionKey(t *testing.T) {
	model := &fakeAgent{finding: aiFinding()}
	e := &DetectEmitter{AI: AIBundle{Detect: model, Cache: ai.NewResultCache(time.Hour, nil)}, logf: (&captureLines{}).logf}
	podA, podB := emitRequest(), emitRequest()
	podA.ConditionKey, podA.Subject = "restarts|pod=a", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "a"}
	podB.ConditionKey, podB.Subject = "restarts|pod=b", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "b"}

	outcomes := []string{
		e.Emit(context.Background(), podA),
		e.Emit(context.Background(), podB),
		e.Emit(context.Background(), podA),
	}
	if want := []string{"emitted", "emitted", "cached"}; !reflect.DeepEqual(outcomes, want) {
		t.Fatalf("outcomes = %v, want %v", outcomes, want)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want 2", model.calls)
	}
}

func TestDetectEmitter_ReadsAIToggleOnEveryCall(t *testing.T) {
	var enabled atomic.Bool
	model := &fakeAgent{finding: aiFinding()}
	e := &DetectEmitter{AI: AIBundle{Detect: model}, AIEnabled: func(context.Context) bool { return enabled.Load() },
		logf: (&captureLines{}).logf}
	if got := e.Emit(context.Background(), emitRequest()); got != "emitted_basic" {
		t.Fatalf("off = %q", got)
	}
	enabled.Store(true)
	if got := e.Emit(context.Background(), emitRequest()); got != "emitted" {
		t.Fatalf("on = %q", got)
	}
}

// The finding's subject is always the request's: a subject in the model's
// reply (or in a cached reply) never reaches the emitter, audit or cache.
func TestDetectEmitter_SubjectComesOnlyFromTheRequest(t *testing.T) {
	attacker := core.FindingSubject{Kind: core.FindingSubjectPod, Name: "kube-system/attacker"}
	modelReply := func() *core.AIFinding {
		f := aiFinding()
		f.Subject = attacker
		return f
	}
	own := core.FindingSubject{Kind: core.FindingSubjectPod, Name: "checkout/checkout-7d9f"}
	for _, path := range []string{"AI finding", "cached AI finding"} {
		for _, want := range []core.FindingSubject{{}, own} {
			t.Run(fmt.Sprintf("%s, request subject %q", path, want.Name), func(t *testing.T) {
				cache := ai.NewResultCache(time.Hour, nil)
				if path == "cached AI finding" {
					cache.Put("cond-1", modelReply())
				}
				var sent *core.AIFinding
				audit := &captureAudit{}
				e := &DetectEmitter{AI: AIBundle{Detect: &fakeAgent{finding: modelReply()}, Cache: cache}, Audit: audit,
					logf:    (&captureLines{}).logf,
					Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
				r := emitRequest()
				r.Subject = want
				e.Emit(context.Background(), r)
				if sent == nil || sent.Subject != want || audit.events[0].Finding.Subject != want {
					t.Fatalf("finding subject = %+v, want %+v", sent.Subject, want)
				}
			})
		}
	}
}

// A finding keeps at most 32 evidence items plus one saying how many were
// left out, and each text is cut to 4 KiB on a rune boundary after scrubbing.
func TestDetectEmitter_BoundsEvidence(t *testing.T) {
	const items = 10000
	r := emitRequest()
	r.Evidence = make([]core.EvidenceItem, items)
	for index := range r.Evidence {
		r.Evidence[index] = core.EvidenceItem{Source: "events", Summary: fmt.Sprintf("item %d", index)}
	}
	// "é" is two bytes; one leading byte makes the 4 KiB cut fall mid-rune.
	r.Evidence[0].Detail = "a" + strings.Repeat("é", 4<<10)
	// A secret just past the cut is scrubbed before the text is cut.
	r.Evidence[1].Summary = strings.Repeat("b", (4<<10)-10) + " password=hunter2canary"

	var sent *core.AIFinding
	e := &DetectEmitter{logf: (&captureLines{}).logf,
		Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
	if got := e.Emit(context.Background(), r); got != "emitted_basic" {
		t.Fatalf("outcome = %q", got)
	}
	if len(sent.Evidence) != maxFindingEvidence+1 {
		t.Fatalf("evidence items = %d, want %d", len(sent.Evidence), maxFindingEvidence+1)
	}
	if last := sent.Evidence[maxFindingEvidence]; last.Summary != fmt.Sprintf("+%d more", items-maxFindingEvidence) {
		t.Fatalf("overflow item = %+v", last)
	}
	detail := sent.Evidence[0].Detail
	if len(detail) > 4<<10 || len(detail) < (4<<10)-1 || !utf8.ValidString(detail) {
		t.Fatalf("detail = %d bytes, valid UTF-8 %v", len(detail), utf8.ValidString(detail))
	}
	if s := sent.Evidence[1].Summary; len(s) > 4<<10 || strings.Contains(s, "hunter2") {
		t.Fatalf("summary = %d bytes, secret present %v", len(s), strings.Contains(s, "hunter2"))
	}
	if !strings.HasSuffix(sent.Summary, fmt.Sprintf("\n- … %d more", items-maxEvidenceLines)) {
		t.Fatalf("templated summary count: %q", sent.Summary[len(sent.Summary)-40:])
	}
	if len(r.Evidence) != items || len(r.Evidence[0].Detail) != 1+2*(4<<10) {
		t.Fatal("caller's evidence was modified")
	}
}

// Only a known kind with a safe name reaches the finding; anything else is
// dropped without failing the emission, and never reaches the summary.
func TestDetectEmitter_DropsInvalidSubject(t *testing.T) {
	long := strings.Repeat("a", 254)
	for _, test := range []struct {
		name    string
		subject core.FindingSubject
		want    core.FindingSubject
	}{
		{"valid pod namespace/name", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "checkout/checkout-7d9f"}, core.FindingSubject{Kind: core.FindingSubjectPod, Name: "checkout/checkout-7d9f"}},
		{"valid node", core.FindingSubject{Kind: core.FindingSubjectNode, Name: "ip-10-0-1-2.ec2.internal"}, core.FindingSubject{Kind: core.FindingSubjectNode, Name: "ip-10-0-1-2.ec2.internal"}},
		{"unknown kind", core.FindingSubject{Kind: "secret", Name: "default/token"}, core.FindingSubject{}},
		{"newline in name", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "ns/pod\nSeverity: none"}, core.FindingSubject{}},
		{"control character", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "ns/pod\x1b[31m"}, core.FindingSubject{}},
		{"space in name", core.FindingSubject{Kind: core.FindingSubjectZone, Name: "us east"}, core.FindingSubject{}},
		{"over-long part", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "ns/" + long}, core.FindingSubject{}},
		{"namespace on a node", core.FindingSubject{Kind: core.FindingSubjectNode, Name: "ns/node-1"}, core.FindingSubject{}},
		{"three parts", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "a/b/c"}, core.FindingSubject{}},
		{"empty name", core.FindingSubject{Kind: core.FindingSubjectPod}, core.FindingSubject{}},
		{"invalid UTF-8", core.FindingSubject{Kind: core.FindingSubjectPod, Name: "ns/\xff"}, core.FindingSubject{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sent *core.AIFinding
			e := &DetectEmitter{logf: (&captureLines{}).logf,
				Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
			r := emitRequest()
			r.Subject = test.subject
			if got := e.Emit(context.Background(), r); got != "emitted_basic" {
				t.Fatalf("outcome = %q", got)
			}
			if sent.Subject != test.want {
				t.Fatalf("subject = %+v, want %+v", sent.Subject, test.want)
			}
			if hasLine := strings.Contains(sent.Summary, "\nSubject:"); hasLine != !test.want.IsZero() {
				t.Fatalf("summary subject line = %v: %q", hasLine, sent.Summary)
			}
			if test.want.IsZero() && test.subject.Name != "" && strings.Contains(sent.Summary, test.subject.Name) {
				t.Fatalf("dropped subject reached the summary: %q", sent.Summary)
			}
		})
	}
}

func TestDetectEmitter_ScrubsSubjectName(t *testing.T) {
	var sent *core.AIFinding
	e := &DetectEmitter{logf: (&captureLines{}).logf,
		Scrub:   func(s string) string { return strings.ReplaceAll(s, "canary7q", "[gone]") },
		Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
	r := emitRequest()
	r.Subject = core.FindingSubject{Kind: core.FindingSubjectPod, Name: "ns/canary7q-pod"}
	e.Emit(context.Background(), r)
	if sent.Subject.Name != "ns/[gone]-pod" || strings.Contains(sent.Summary, "canary7q") {
		t.Fatalf("subject = %+v, summary %q", sent.Subject, sent.Summary)
	}
}
