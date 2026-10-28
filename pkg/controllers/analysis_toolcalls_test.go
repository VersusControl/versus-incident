package controllers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// These cover the save-path bug where an analysis was run, cost real money and
// real time, and was then thrown away at persist:
//
//	save: storage: marshal analysis: json: error calling MarshalJSON for type
//	json.RawMessage: invalid character 't' after object key:value pair
//
// AnalysisToolCall.Args/Output are json.RawMessage, which validates on marshal.
// toolCallsFromCore used to cast the captured strings straight in, so any tool
// text that was not valid JSON failed the marshal of the WHOLE record — the
// analysis was lost and the follow-up GET returned "not found".

// TestRawJSONOrString_PassesValidJSONThrough proves the common case is
// untouched: a well-formed envelope is stored as JSON, not double-encoded into
// a string (which would show up as escaped garbage in the UI).
func TestRawJSONOrString_PassesValidJSONThrough(t *testing.T) {
	for _, in := range []string{
		`{"ok":true,"rows":[1,2,3]}`,
		`[]`,
		`"already a json string"`,
		`123`,
		`null`,
	} {
		got := rawJSONOrString(in)
		if string(got) != in {
			t.Errorf("rawJSONOrString(%s) = %s, want it passed through unchanged", in, got)
		}
	}
}

// TestRawJSONOrString_EmptyIsOmitted keeps an unset field nil so `omitempty`
// drops it. A zero-length json.RawMessage is itself a marshal error
// ("unexpected end of JSON input"), so this is not merely cosmetic.
func TestRawJSONOrString_EmptyIsOmitted(t *testing.T) {
	if got := rawJSONOrString(""); got != nil {
		t.Errorf("rawJSONOrString(\"\") = %q, want nil", got)
	}
}

// TestRawJSONOrString_WrapsInvalidJSON is the fix itself: unparseable text is
// preserved as a JSON string rather than discarded or left to break the record.
func TestRawJSONOrString_WrapsInvalidJSON(t *testing.T) {
	// Exactly the shape capOutput produces when it truncates an oversized
	// envelope: a JSON object cut mid-structure with a marker appended.
	truncated := `{"service":"checkout","rows":[{"msg":"conn refused"` + `..."truncated"`

	got := rawJSONOrString(truncated)
	if !json.Valid(got) {
		t.Fatalf("rawJSONOrString returned invalid JSON: %s", got)
	}
	var back string
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("wrapped value is not a JSON string: %v", err)
	}
	if back != truncated {
		t.Errorf("wrapped value lost content:\n got %q\nwant %q", back, truncated)
	}
}

// TestToolCallsFromCore_RecordMarshalsWithUnparseableToolText is the
// regression proper: build the record the way the analyze handler does and
// prove it survives json.Marshal. Before the fix this failed, and the run was
// lost at save.
func TestToolCallsFromCore_RecordMarshalsWithUnparseableToolText(t *testing.T) {
	traces := []core.ToolCallTrace{
		{
			Name:       "query_logs",
			Args:       `{"service":"checkout"}`,
			Output:     `{"rows":[{"msg":"conn refused"` + `..."truncated"`, // truncated envelope
			DurationMs: 1240,
		},
		{
			Name:   "find_runbook",
			Args:   `{"q":"checkout 5xx"}`,
			Output: "no runbook matched", // a tool that answers in plain text
		},
		{
			Name:  "query_metrics",
			Args:  "",
			Error: "upstream timeout",
		},
	}

	calls := toolCallsFromCore(traces)
	if len(calls) != 3 {
		t.Fatalf("toolCallsFromCore returned %d calls, want 3", len(calls))
	}

	rec := &storage.AnalysisRecord{
		ID:         "an-1",
		IncidentID: "inc-1",
		Status:     "ok",
		ToolCalls:  calls,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal analysis record: %v — this is the bug: a valid run is discarded at save", err)
	}

	// The valid envelope stays real JSON the UI can read as an object.
	if !strings.Contains(string(b), `{"service":"checkout"}`) {
		t.Errorf("valid args were not stored as JSON; got %s", b)
	}
	// The unparseable output is preserved, not dropped.
	if !strings.Contains(string(b), "conn refused") {
		t.Errorf("truncated tool output was lost; got %s", b)
	}
	if !strings.Contains(string(b), "no runbook matched") {
		t.Errorf("plain-text tool output was lost; got %s", b)
	}

	// And it round-trips back to a usable record.
	var back storage.AnalysisRecord
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal analysis record: %v", err)
	}
	if len(back.ToolCalls) != 3 {
		t.Fatalf("round-trip lost tool calls: got %d, want 3", len(back.ToolCalls))
	}
	if back.ToolCalls[2].Args != nil {
		t.Errorf("empty args should be omitted, got %s", back.ToolCalls[2].Args)
	}
	if back.ToolCalls[2].Error != "upstream timeout" {
		t.Errorf("tool error lost: %q", back.ToolCalls[2].Error)
	}
}

// TestToolCallsFromCore_NoTracesIsNil keeps a tool-free run (the detect agent
// path) from persisting an empty array where the field should be absent.
func TestToolCallsFromCore_NoTracesIsNil(t *testing.T) {
	if got := toolCallsFromCore(nil); got != nil {
		t.Errorf("toolCallsFromCore(nil) = %v, want nil", got)
	}
	if got := toolCallsFromCore([]core.ToolCallTrace{}); got != nil {
		t.Errorf("toolCallsFromCore(empty) = %v, want nil", got)
	}
}
