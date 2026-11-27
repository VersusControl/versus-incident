package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/agent/ai"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// freshFindingModel returns a new finding on every call, as a real model
// does. Its own evidence slice has spare capacity, so an append that is not
// capped writes into a backing array the cache and earlier findings share.
type freshFindingModel struct{ calls atomic.Int32 }

func (*freshFindingModel) Name() string          { return "fresh" }
func (*freshFindingModel) Kind() core.AITaskKind { return core.AITaskDetect }

func (m *freshFindingModel) Run(_ context.Context, _ core.AITask) (*core.AICallResult, error) {
	m.calls.Add(1)
	evidence := make([]core.EvidenceItem, 1, 8)
	evidence[0] = core.EvidenceItem{Source: "model", Summary: "model's own evidence"}
	return &core.AICallResult{
		Finding: &core.AIFinding{Title: "AI title", Summary: "AI summary", Severity: "medium", Confidence: 0.7, Evidence: evidence},
		Model:   "fresh-model",
	}, nil
}

// lockedAudit is a DetectAudit safe for concurrent use.
type lockedAudit struct {
	mu     sync.Mutex
	events []*DetectEvent
}

func (a *lockedAudit) Record(e *DetectEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func requestFor(key, owner string) DetectRequest {
	r := emitRequest()
	r.ConditionKey = key
	r.Subject = core.FindingSubject{Kind: core.FindingSubjectPod, Name: owner}
	r.Evidence = []core.EvidenceItem{{Source: "metrics", Summary: "owner " + owner}}
	return r
}

// Many goroutines share one emitter, cache and audit sink. Each finding
// carries only its own request's subject and evidence, and the cache never
// holds request evidence.
func TestDetectEmitterConcurrentEmitKeepsEachRequestsEvidence(t *testing.T) {
	model := &freshFindingModel{}
	cache := ai.NewResultCache(time.Hour, nil)
	audit := &lockedAudit{}
	var mu sync.Mutex
	sent := map[*core.AIFinding]string{}
	e := &DetectEmitter{
		AI:    AIBundle{Detect: model, Cache: cache},
		Audit: audit,
		Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			sent[f] = f.Subject.Name
			return nil
		},
		logf: func(string, ...any) {},
	}
	const goroutines, perGoroutine, keys = 32, 20, 4
	var wg sync.WaitGroup
	outcomes := make([][]string, goroutines)
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				owner := fmt.Sprintf("pod-%d-%d", g, i)
				outcomes[g] = append(outcomes[g], e.Emit(context.Background(), requestFor(fmt.Sprintf("cond-%d", (g+i)%keys), owner)))
			}
		})
	}
	wg.Wait()

	if len(sent) != goroutines*perGoroutine || len(audit.events) != goroutines*perGoroutine {
		t.Fatalf("sent %d findings, %d audit records, want %d", len(sent), len(audit.events), goroutines*perGoroutine)
	}
	for f, owner := range sent {
		var own []core.EvidenceItem
		for _, item := range f.Evidence {
			if item.Source == "metrics" {
				own = append(own, item)
			}
		}
		if len(own) != 1 || own[0].Summary != "owner "+owner {
			t.Fatalf("finding for %s carries evidence %+v", owner, f.Evidence)
		}
		if len(f.Evidence) != 2 || f.Evidence[0].Source != "model" {
			t.Fatalf("finding for %s lost the model's evidence: %+v", owner, f.Evidence)
		}
	}
	for key := range keys {
		cached, ok := cache.Get(fmt.Sprintf("cond-%d", key))
		if !ok {
			t.Fatalf("cond-%d not cached", key)
		}
		if !cached.Subject.IsZero() || len(cached.Evidence) != 1 || cached.Evidence[0].Source != "model" {
			t.Fatalf("cache holds request data for cond-%d: %+v", key, cached)
		}
	}
	counts := map[string]int{}
	for _, list := range outcomes {
		for _, outcome := range list {
			counts[outcome]++
		}
	}
	if counts["emitted"]+counts["cached"] != goroutines*perGoroutine || counts["emitted"] < keys {
		t.Fatalf("outcomes = %v", counts)
	}
	if int(model.calls.Load()) != counts["emitted"] {
		t.Fatalf("model calls %d != emitted %d", model.calls.Load(), counts["emitted"])
	}
}

// A request with a subject and evidence, then plain requests for the same
// condition key: the plain findings carry neither, and a later request does
// not rewrite a finding already sent.
func TestDetectEmitterEvidenceDoesNotLeakThroughTheCache(t *testing.T) {
	model := &freshFindingModel{}
	var sent []*core.AIFinding
	e := &DetectEmitter{
		AI:      AIBundle{Detect: model, Cache: ai.NewResultCache(time.Hour, nil)},
		Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = append(sent, f); return nil },
		logf:    func(string, ...any) {},
	}
	withEvidence := requestFor("cond-x", "pod-a")
	plain := emitRequest()
	plain.ConditionKey = "cond-x"
	other := requestFor("cond-x", "pod-b")

	got := []string{
		e.Emit(context.Background(), withEvidence),
		e.Emit(context.Background(), plain),
		e.Emit(context.Background(), other),
		e.Emit(context.Background(), plain),
	}
	if want := "emitted cached cached cached"; strings.Join(got, " ") != want {
		t.Fatalf("outcomes = %v, want %s", got, want)
	}
	for _, index := range []int{1, 3} {
		if !sent[index].Subject.IsZero() || len(sent[index].Evidence) != 1 || sent[index].Evidence[0].Source != "model" {
			t.Fatalf("plain finding %d carries request data: subject %+v evidence %+v", index, sent[index].Subject, sent[index].Evidence)
		}
	}
	if len(sent[0].Evidence) != 2 || sent[0].Evidence[1].Summary != "owner pod-a" || sent[0].Subject.Name != "pod-a" {
		t.Fatalf("first finding was rewritten by a later request: %+v %+v", sent[0].Subject, sent[0].Evidence)
	}
	if len(sent[2].Evidence) != 2 || sent[2].Evidence[1].Summary != "owner pod-b" {
		t.Fatalf("third finding evidence = %+v", sent[2].Evidence)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("model calls = %d, want 1", model.calls.Load())
	}
}

// Evidence with 10,000 items and very long text: the templated summary stays
// bounded (ten lines of at most 200 bytes and a count of every item left
// out), and the finding keeps at most 32 items plus one "+N more" item, each
// text scrubbed and then cut to 4 KiB on a rune boundary.
func TestDetectEmitterLargeEvidenceKeepsTheSummaryBounded(t *testing.T) {
	const items = 10000
	// One ASCII byte, then two-byte runes: byte 4096 is the middle of a rune.
	long := "x" + strings.Repeat("é", 1<<19)
	r := emitRequest()
	r.Evidence = make([]core.EvidenceItem, items)
	for index := range r.Evidence {
		r.Evidence[index] = core.EvidenceItem{Source: "events", Summary: fmt.Sprintf("item %d %s", index, strings.Repeat("y", 64))}
	}
	r.Evidence[0].Detail = long
	r.Evidence[0].Source = long
	r.Evidence[1].Summary = long
	r.Evidence[5].Summary = "auth failed password=" + fuzzEvidenceCanary
	r.Evidence[6].Detail = long[:4001] + " token: " + fuzzEvidenceCanary
	r.Evidence[items-1].Summary = "password=" + fuzzEvidenceCanary
	plain := emitRequest()

	var sent []*core.AIFinding
	e := &DetectEmitter{
		Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = append(sent, f); return nil },
		logf:    func(string, ...any) {},
	}
	started := time.Now()
	if got := e.Emit(context.Background(), r); got != "emitted_basic" {
		t.Fatalf("outcome = %q", got)
	}
	elapsed := time.Since(started)
	e.Emit(context.Background(), plain)

	added := len(sent[0].Summary) - len(sent[1].Summary)
	// "\nEvidence:" + 10 lines of "\n- " + at most 200 bytes + "…" + the count line.
	limit := len("\nEvidence:") + maxEvidenceLines*(len("\n- ")+200+len("…")) + len(fmt.Sprintf("\n- … %d more", items))
	if added <= 0 || added > limit {
		t.Fatalf("summary grew by %d bytes, want at most %d", added, limit)
	}
	if !strings.HasSuffix(sent[0].Summary, fmt.Sprintf("\n- … %d more", items-maxEvidenceLines)) {
		t.Fatalf("summary does not end with the count of the rest: %q", sent[0].Summary[len(sent[0].Summary)-80:])
	}

	evidence := sent[0].Evidence
	if len(evidence) != 33 {
		t.Fatalf("evidence list = %d items, want 32 kept and one count item", len(evidence))
	}
	if last := evidence[32]; last != (core.EvidenceItem{Source: "detect", Summary: fmt.Sprintf("+%d more", items-32)}) {
		t.Fatalf("closing evidence item = %+v", last)
	}
	for index, item := range evidence[:32] {
		if item.Summary != r.Evidence[index].Summary && index != 1 && index != 5 {
			t.Fatalf("item %d is not request item %d: %q", index, index, item.Summary)
		}
		for field, text := range map[string]string{"source": item.Source, "summary": item.Summary, "detail": item.Detail} {
			if len(text) > 4<<10 || !utf8.ValidString(text) {
				t.Fatalf("item %d %s: %d bytes, valid UTF-8 %v", index, field, len(text), utf8.ValidString(text))
			}
		}
	}
	// The cut keeps as much as fits: 4 KiB less the half rune at the edge.
	for field, text := range map[string]string{"source": evidence[0].Source, "detail": evidence[0].Detail, "summary": evidence[1].Summary} {
		if len(text) != 4<<10-1 || !strings.HasPrefix(long, text) {
			t.Fatalf("long %s cut to %d bytes, want %d from the start of the text", field, len(text), 4<<10-1)
		}
	}
	blob, err := json.Marshal(sent[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), fuzzEvidenceCanary) {
		t.Fatal("canary reached the finding")
	}
	t.Logf("10,000 items, 1 MiB texts: summary +%d bytes, %d evidence items kept, emit took %s", added, len(evidence), elapsed)
}

const fuzzEvidenceCanary = "Zq7canaryW3x"

// FuzzDetectEmitterRequestText feeds arbitrary text into every text field of
// a request. Emit never panics, and a secret written as password=<value> in
// evidence never reaches the finding or the audit record.
func FuzzDetectEmitterRequestText(f *testing.F) {
	f.Add("db timeout <*>", "checkout", "loki", "events", "BackOff", "restart loop", "pod", "checkout/api-1", "7/s above 2/s", "high", 3)
	f.Add("", "", "", "", "", "", "", "", "", "", 0)
	f.Add("\x00\xff", "日本語", "a: b", "(", ")", "\n- ", "workload", strings.Repeat("é", 300), "%s%d", "critical", 12)
	f.Fuzz(func(t *testing.T, title, service, source, evSource, evSummary, evDetail, subjectKind, subjectName, explanation, severity string, count int) {
		for _, input := range []string{title, service, source, evSource, evSummary, evDetail, subjectKind, subjectName, explanation, severity} {
			if strings.Contains(strings.ToLower(input), strings.ToLower(fuzzEvidenceCanary)) {
				t.Skip("input already holds the canary")
			}
		}
		items := 1 + (count%31+31)%31
		r := DetectRequest{
			Source: source, SignalKind: "logs", ConditionKey: title + "|" + subjectName, Title: title, Service: service,
			Frequency: count, Verdict: core.VerdictSpike, Explanation: explanation, Severity: severity,
			Samples: []core.Signal{{Message: title, Severity: severity}},
			Subject: core.FindingSubject{Kind: subjectKind, Name: subjectName},
		}
		for index := range items {
			r.Evidence = append(r.Evidence, core.EvidenceItem{
				Source:  evSource,
				Summary: evSummary + " password=" + fuzzEvidenceCanary + " " + evSummary,
				Detail:  fmt.Sprintf("%s token: %s%d %s", evDetail, fuzzEvidenceCanary, index, evDetail),
			})
		}
		for _, bundle := range []AIBundle{{}, {Detect: &freshFindingModel{}, Cache: ai.NewResultCache(time.Hour, nil)}} {
			var sent *core.AIFinding
			audit := &lockedAudit{}
			e := &DetectEmitter{AI: bundle, Audit: audit, logf: func(string, ...any) {},
				Emitter: func(f *core.AIFinding, _ core.AgentResult, _, _ string) error { sent = f; return nil }}
			for range 2 { // the second call takes the cache path when AI is on
				outcome := e.Emit(context.Background(), r)
				if sent == nil || (outcome != "emitted_basic" && outcome != "emitted" && outcome != "cached") {
					t.Fatalf("outcome = %q, finding %v", outcome, sent)
				}
				blob, err := json.Marshal(struct {
					Finding *core.AIFinding
					Audit   []*DetectEvent
				}{sent, audit.events})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if strings.Contains(strings.ToLower(string(blob)), strings.ToLower(fuzzEvidenceCanary)) {
					t.Fatalf("canary reached the finding or audit record: %s", blob)
				}
			}
		}
	})
}
