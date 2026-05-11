package ai

import (
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestParseFinding_PlainJSON(t *testing.T) {
	raw := `{"title":"DB pool exhausted","summary":"connection pool saturated","severity":"high","category":"infra","confidence":0.85,"suggestions":["scale db","raise pool"]}`
	got, err := ParseFinding(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Title != "DB pool exhausted" || got.Severity != "high" || got.Confidence != 0.85 {
		t.Fatalf("unexpected finding: %+v", got)
	}
	if len(got.Suggestions) != 2 {
		t.Fatalf("want 2 suggestions, got %d", len(got.Suggestions))
	}
}

func TestParseFinding_FencedWithPreamble(t *testing.T) {
	raw := "Sure, here is the analysis:\n```json\n{\"title\":\"x\",\"summary\":\"y\",\"severity\":\"CRITICAL\",\"category\":\"\",\"confidence\":2.5,\"suggestions\":[]}\n```"
	got, err := ParseFinding(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Severity != "critical" {
		t.Fatalf("severity not normalized: %q", got.Severity)
	}
	if got.Confidence != 1.0 {
		t.Fatalf("confidence not clamped: %v", got.Confidence)
	}
}

func TestParseFinding_MissingTitle(t *testing.T) {
	if _, err := ParseFinding(`{"summary":"y"}`); err == nil {
		t.Fatal("expected error for missing title")
	}
}

func TestBuildPrompt_TruncatesSamples(t *testing.T) {
	r := core.AgentResult{
		Verdict:   core.VerdictUnknown,
		PatternID: "p1",
		Template:  "tpl",
		Frequency: 5,
	}
	_, user := BuildPrompt(r, "src", "svc", []string{"a", "b", "c", "d"})
	// only 3 samples should appear
	if strings.Count(user, "- ") < 3 {
		t.Fatalf("want at least 3 sample lines, got: %s", user)
	}
	if strings.Contains(user, "\"d\"") {
		t.Fatal("4th sample should not appear")
	}
}

func TestSystemPrompt_AssemblesAllFragments(t *testing.T) {
	// Each fragment must contribute its own marker — guards against
	// promptOrder drifting away from the actual prompts/*.md files.
	wantMarkers := []string{
		"# SOUL.md",
		"# INPUTS.md",
		"# OUTPUT.md",
		"# RULES.md",
	}
	for _, m := range wantMarkers {
		if !strings.Contains(systemPrompt, m) {
			t.Fatalf("system prompt missing marker %q", m)
		}
	}
	// Order check: SOUL precedes INPUTS precedes OUTPUT precedes RULES.
	prev := -1
	for _, m := range wantMarkers {
		idx := strings.Index(systemPrompt, m)
		if idx <= prev {
			t.Fatalf("fragments out of order at %q (idx=%d, prev=%d)", m, idx, prev)
		}
		prev = idx
	}
}

func TestSystemPrompt_KeepsCriticalRules(t *testing.T) {
	// Hard rules the rest of the code base depends on.
	mustContain := []string{
		"<REDACTED:",                     // redaction handling
		"critical | high | medium | low", // severity vocabulary
		"exactly one JSON object",        // single-object output contract
	}
	for _, s := range mustContain {
		if !strings.Contains(systemPrompt, s) {
			t.Fatalf("system prompt missing required rule %q", s)
		}
	}
}
