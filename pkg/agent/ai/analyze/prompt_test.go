package analyze

import (
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestSystemPrompt_AssemblesAllFragments(t *testing.T) {
	sp := SystemPrompt()
	for _, marker := range []string{"# Identity", "# Inputs", "# Output", "# Rules"} {
		if !strings.Contains(sp, marker) {
			t.Errorf("system prompt missing fragment marker %q", marker)
		}
	}
}

func TestPromptOrder_StableCopy(t *testing.T) {
	a := PromptOrder()
	b := PromptOrder()
	if &a[0] == &b[0] {
		t.Fatalf("PromptOrder returned shared slice header; must copy")
	}
	if len(a) != 4 {
		t.Fatalf("len(PromptOrder) = %d, want 4", len(a))
	}
}

func TestBuildUserPrompt_IncludesAllFields(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	snap := core.AnalyzeIncidentSnapshot{
		IncidentID:  "i-42",
		Title:       "disk full",
		Service:     "billing",
		Source:      "loki",
		Severity:    "high",
		CreatedAt:   created,
		Content:     map[string]any{"msg": "boom"},
		RequestedBy: "alice",
	}
	out := BuildUserPrompt(snap)
	for _, want := range []string{"i-42", "billing", "loki", "high", "2024-01-02T03:04:05Z", "alice", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("user prompt missing %q\n%s", want, out)
		}
	}
}

func TestParseFinding_HappyPath(t *testing.T) {
	raw := `Here is JSON:
` + "```json" + `
{
  "title": "X",
  "summary": "Y",
  "severity": "HIGH",
  "confidence": 1.5,
  "root_cause_hypotheses": [
    {"hypothesis": "h1", "confidence": 0.8, "rationale": "r1"}
  ],
  "next_steps": ["a","b","c","d","e","f","g"]
}
` + "```"
	f, err := ParseFinding(raw)
	if err != nil {
		t.Fatalf("ParseFinding: %v", err)
	}
	if f.Severity != "high" {
		t.Errorf("severity = %q, want lowercased", f.Severity)
	}
	if f.Confidence != 1 {
		t.Errorf("confidence = %v, want clamped to 1", f.Confidence)
	}
	if len(f.NextSteps) != 5 {
		t.Errorf("next_steps len = %d, want 5 (cap)", len(f.NextSteps))
	}
	if len(f.RootCauseHypotheses) != 1 {
		t.Errorf("hypotheses len = %d", len(f.RootCauseHypotheses))
	}
}

func TestParseFinding_RejectsEmpty(t *testing.T) {
	if _, err := ParseFinding(`{"title":"a","summary":"b"}`); err == nil {
		t.Fatalf("expected error for empty hypotheses+next_steps")
	}
}

func TestParseFinding_RejectsMissingTitle(t *testing.T) {
	if _, err := ParseFinding(`{"summary":"b","next_steps":["x"]}`); err == nil {
		t.Fatalf("expected error for missing title")
	}
}
