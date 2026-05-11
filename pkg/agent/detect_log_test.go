package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestDetectLog_RecordAndPersistRoundTrip(t *testing.T) {
	store := storage.NewMemory()
	d, err := LoadDetectLog(store, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	d.Record(&DetectEvent{
		Source:      "es-prod",
		PatternID:   "pat-1",
		Template:    "DB connection refused",
		Service:     "checkout",
		Verdict:     "unknown",
		Frequency:   3,
		Baseline:    0.5,
		Samples:     []string{"db conn refused 1", "db conn refused 2"},
		Model:       "gpt-test",
		UserPrompt:  "what is happening?",
		RawResponse: `{"title":"DB outage"}`,
		DurationMs:  42,
		Finding:     &core.AIFinding{Title: "DB outage", Severity: "high", Confidence: 0.9},
		Outcome:     "emitted",
	})
	if d.Len() != 1 {
		t.Fatalf("len=%d, want 1", d.Len())
	}
	if !d.Dirty() {
		t.Fatal("expected dirty after Record")
	}
	if err := d.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if d.Dirty() {
		t.Fatal("dirty should reset after Persist")
	}

	d2, err := LoadDetectLog(store, 0)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if d2.Len() != 1 {
		t.Fatalf("reloaded len=%d, want 1", d2.Len())
	}
	got := d2.All()[0]
	if got.PatternID != "pat-1" || got.UserPrompt != "what is happening?" {
		t.Fatalf("bad reload: %+v", got)
	}
	if got.Finding == nil || got.Finding.Title != "DB outage" {
		t.Fatalf("finding lost on reload: %+v", got.Finding)
	}
	if got.ID == "" {
		t.Fatal("ID should be assigned on Record")
	}
}

func TestDetectLog_CapEvictsOldest(t *testing.T) {
	d, _ := LoadDetectLog(nil, 3)
	for i := 0; i < 5; i++ {
		d.Record(&DetectEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			PatternID: "p",
			Outcome:   "emitted",
		})
	}
	if d.Len() != 3 {
		t.Fatalf("len=%d, want 3 (capped)", d.Len())
	}
}

func TestDetectLog_GetByID(t *testing.T) {
	d, _ := LoadDetectLog(nil, 0)
	e := &DetectEvent{ID: "abc123", PatternID: "p", Outcome: "emitted"}
	d.Record(e)
	got := d.Get("abc123")
	if got == nil || got.ID != "abc123" {
		t.Fatalf("Get returned %+v", got)
	}
	if d.Get("missing") != nil {
		t.Fatal("Get should return nil for unknown id")
	}
}

func TestDetectLog_Stats(t *testing.T) {
	d, _ := LoadDetectLog(nil, 0)
	d.Record(&DetectEvent{Outcome: "emitted", Verdict: "unknown",
		Finding: &core.AIFinding{Severity: "high"}})
	d.Record(&DetectEvent{Outcome: "cached", Verdict: "unknown"})
	d.Record(&DetectEvent{Outcome: "ai_error", Verdict: "spike"})

	s := d.Stats()
	if s["events"] != 3 {
		t.Fatalf("events=%d", s["events"])
	}
	if s["outcome_emitted"] != 1 || s["outcome_cached"] != 1 || s["outcome_ai_error"] != 1 {
		t.Fatalf("outcome stats wrong: %+v", s)
	}
	if s["verdict_unknown"] != 2 || s["verdict_spike"] != 1 {
		t.Fatalf("verdict stats wrong: %+v", s)
	}
	if s["severity_high"] != 1 {
		t.Fatalf("severity stats wrong: %+v", s)
	}
}

func TestDetectLog_TruncatesLargeFields(t *testing.T) {
	d, _ := LoadDetectLog(nil, 0)
	big := strings.Repeat("x", detectResponseMaxBytes+500)
	d.Record(&DetectEvent{
		PatternID:   "p",
		Outcome:     "emitted",
		UserPrompt:  big,
		RawResponse: big,
	})
	got := d.All()[0]
	if len(got.UserPrompt) > detectPromptMaxBytes+10 {
		t.Fatalf("prompt not truncated: %d", len(got.UserPrompt))
	}
	if len(got.RawResponse) > detectResponseMaxBytes+10 {
		t.Fatalf("response not truncated: %d", len(got.RawResponse))
	}
}

func TestDetectLog_ClearAndPersistEmpty(t *testing.T) {
	store := storage.NewMemory()
	d, _ := LoadDetectLog(store, 0)
	d.Record(&DetectEvent{PatternID: "p", Outcome: "emitted"})
	_ = d.Persist()

	if n := d.Clear(); n != 1 {
		t.Fatalf("Clear returned %d, want 1", n)
	}
	if d.Len() != 0 {
		t.Fatal("expected empty after Clear")
	}
	if err := d.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	d2, _ := LoadDetectLog(store, 0)
	if d2.Len() != 0 {
		t.Fatalf("reload after clear: len=%d", d2.Len())
	}
}
