package controllers

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestSnapshotFromIncident_PullsSeverityFromContent(t *testing.T) {
	created := time.Now().UTC()
	ack := created.Add(time.Minute)
	rec := &storage.IncidentRecord{
		ID:        "inc-1",
		Title:     "boom",
		Service:   "billing",
		Source:    "loki",
		Resolved:  false,
		CreatedAt: created,
		AckedAt:   &ack,
		Content:   map[string]any{"severity": "high", "extra": 7},
	}
	snap := snapshotFromIncident(rec, "alice")
	if snap.IncidentID != "inc-1" || snap.Service != "billing" || snap.Source != "loki" {
		t.Fatalf("snapshot identity wrong: %+v", snap)
	}
	if snap.Severity != "high" {
		t.Fatalf("severity = %q, want 'high'", snap.Severity)
	}
	if snap.RequestedBy != "alice" {
		t.Fatalf("requestedBy = %q", snap.RequestedBy)
	}
	if snap.AckedAt == nil || !snap.AckedAt.Equal(ack) {
		t.Fatalf("AckedAt not propagated: %+v", snap.AckedAt)
	}
}

func TestSnapshotFromIncident_NilContent(t *testing.T) {
	snap := snapshotFromIncident(&storage.IncidentRecord{ID: "x"}, "")
	if snap.Severity != "" {
		t.Fatalf("expected empty severity for nil content")
	}
}

func TestToolCallsFromCore_Empty(t *testing.T) {
	if got := toolCallsFromCore(nil); got != nil {
		t.Fatalf("expected nil for empty traces, got %+v", got)
	}
}

func TestToolCallsFromCore_RoundTrip(t *testing.T) {
	traces := []core.ToolCallTrace{
		{Name: "t1", Args: `{"q":"a"}`, Output: `"ok"`, DurationMs: 12, Error: ""},
		{Name: "t2", Args: `{}`, Output: ``, DurationMs: 0, Error: "boom"},
	}
	out := toolCallsFromCore(traces)
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].Name != "t1" || string(out[0].Args) != `{"q":"a"}` || string(out[0].Output) != `"ok"` || out[0].DurationMs != 12 {
		t.Fatalf("trace[0] wrong: %+v", out[0])
	}
	if out[1].Error != "boom" {
		t.Fatalf("trace[1] error not propagated: %+v", out[1])
	}
}
