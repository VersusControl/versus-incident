package utils

import (
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestIsAgentIncident(t *testing.T) {
	cases := []struct {
		name    string
		content map[string]interface{}
		want    bool
	}{
		{"nil map", nil, false},
		{"empty map", map[string]interface{}{}, false},
		{"non-agent content", map[string]interface{}{"AlertName": "X", "Source": "alertmanager"}, false},
		{"pattern id set", map[string]interface{}{"PatternID": "p_abc123"}, true},
		{"pattern id empty string", map[string]interface{}{"PatternID": ""}, false},
		{"pattern id wrong type", map[string]interface{}{"PatternID": 42}, false},
		{"source starts with agent:", map[string]interface{}{"Source": "agent:elasticsearch"}, true},
		{"source agent prefix-only", map[string]interface{}{"Source": "agent:"}, true},
		{"source too short", map[string]interface{}{"Source": "agent"}, false},
		{"source wrong type", map[string]interface{}{"Source": 7}, false},
		{"both pattern id and agent source", map[string]interface{}{"PatternID": "p_x", "Source": "agent:loki"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAgentIncident(c.content); got != c.want {
				t.Errorf("IsAgentIncident(%v) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

// The agent templates print the baseline next to the frequency only when the
// detection carries one. A finding with a frequency and no baseline shows the
// frequency alone, never a printf error, and a baseline of zero is a real
// value that is shown. Providers hand a template the content map or a pointer
// to it, so both forms are rendered.
func TestAgentTemplatesShowTheBaselineOnlyWhenSet(t *testing.T) {
	paths := []string{AgentSlackTemplatePath, AgentTelegramTemplatePath, AgentMSTeamsTemplatePath, AgentLarkTemplatePath, AgentViberTemplatePath, AgentEmailTemplatePath}
	cases := []struct {
		name     string
		baseline interface{}
		set      bool
		want     string
	}{
		{"no baseline", nil, false, ""},
		{"baseline zero", 0.0, true, "(baseline 0.00)"},
		{"baseline set", 2.5, true, "(baseline 2.50)"},
	}
	for _, path := range paths {
		tmpl, err := template.New(filepath.Base(path)).Funcs(GetTemplateFuncMaps()).ParseFiles(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, c := range cases {
			content := map[string]interface{}{"AlertName": "Error burst", "PatternID": "p_checkout", "Frequency": 7}
			if c.set {
				content["Baseline"] = c.baseline
			}
			for form, data := range map[string]interface{}{"map": content, "pointer": &content} {
				var out strings.Builder
				if err := tmpl.Execute(&out, data); err != nil {
					t.Fatalf("%s %s (%s): %v", path, c.name, form, err)
				}
				text := out.String()
				if strings.Contains(text, "%!") {
					t.Errorf("%s %s (%s): format error in output:\n%s", path, c.name, form, text)
				}
				if !strings.Contains(text, "7") {
					t.Errorf("%s %s (%s): frequency missing:\n%s", path, c.name, form, text)
				}
				if c.want == "" && strings.Contains(text, "baseline") {
					t.Errorf("%s %s (%s): baseline shown without one:\n%s", path, c.name, form, text)
				}
				if c.want != "" && !strings.Contains(text, c.want) {
					t.Errorf("%s %s (%s): want %q in:\n%s", path, c.name, form, c.want, text)
				}
			}
		}
	}
}
