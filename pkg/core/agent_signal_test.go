package core

import "testing"

func TestAgentResult_BaselineDelta(t *testing.T) {
	cases := []struct {
		name string
		freq int
		base float64
		want string
	}{
		{"big spike rounds to integer", 240, 6.1, "240 this tick vs baseline 6.1/tick (39× normal)"},
		{"small multiple keeps one decimal", 9, 6.0, "9 this tick vs baseline 6.0/tick (1.5× normal)"},
		{"at threshold uses integer", 100, 10.0, "100 this tick vs baseline 10.0/tick (10× normal)"},
		{"no baseline yet", 240, 0, "240 this tick (no baseline yet)"},
		{"negative baseline treated as none", 5, -1, "5 this tick (no baseline yet)"},
		{"zero frequency yields empty", 0, 6.1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := AgentResult{Frequency: c.freq, Baseline: c.base}
			if got := r.BaselineDelta(); got != c.want {
				t.Errorf("BaselineDelta() = %q, want %q", got, c.want)
			}
		})
	}
}
