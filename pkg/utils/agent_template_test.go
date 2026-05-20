package utils

import "testing"

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
