package chat

import (
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/analyze"
)

func TestSystemPromptAndOrder(t *testing.T) {
	prompt := SystemPrompt()
	last := -1
	for _, marker := range []string{"# Identity", "# Inputs", "# Output", "# Rules"} {
		index := strings.Index(prompt, marker)
		if index <= last {
			t.Fatalf("marker %q missing or out of order", marker)
		}
		last = index
	}
	order := PromptOrder()
	order[0] = "changed"
	if PromptOrder()[0] == "changed" {
		t.Fatal("PromptOrder returned mutable shared state")
	}
}

func TestSystemPromptIsIndependentFromAnalyze(t *testing.T) {
	if SystemPrompt() == analyze.SystemPrompt() {
		t.Fatal("chat and analyze prompts unexpectedly match")
	}
	if strings.Contains(SystemPrompt(), "single incident") {
		t.Fatal("chat prompt inherited incident-only identity")
	}
	if !strings.Contains(analyze.SystemPrompt(), "structured JSON") {
		t.Fatal("analyze output contract changed")
	}
}

func TestSystemPromptRequiresKnowledgeToolsAndHonestUnknowns(t *testing.T) {
	prompt := SystemPrompt()
	for _, required := range []string{"get_system_overview", "list_capabilities", "get_incident", "get_service", "get_pattern", "get_alert_decision", "get_detection_health", "Never infer a resolver"} {
		if !strings.Contains(prompt, required) {
			t.Errorf("system prompt missing %q", required)
		}
	}
	for _, forbidden := range []string{"versus_overview", "describe_incident", "describe_service", "pattern_history", "explain_known_status", "explain_spam", "service_objectives", "capability_status"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("system prompt retains old tool name %q", forbidden)
		}
	}
}
