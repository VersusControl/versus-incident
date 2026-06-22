package config

import "testing"

// TestProviderBaseURL asserts the provider→endpoint mapping: openai (and
// empty/unknown) use the OpenAI default (""), gemini maps to its
// OpenAI-compatible endpoint, and matching is case/space-insensitive.
func TestProviderBaseURL(t *testing.T) {
	const gemini = "https://generativelanguage.googleapis.com/v1beta/openai"
	cases := map[string]string{
		"":         "",
		"openai":   "",
		"OpenAI":   "",
		"unknown":  "",
		"gemini":   gemini,
		"Gemini":   gemini,
		" gemini ": gemini,
	}
	for in, want := range cases {
		if got := ProviderBaseURL(in); got != want {
			t.Errorf("ProviderBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveProviderOverride asserts a per-task provider overrides the
// shared default, and an empty task provider inherits it.
func TestResolveProviderOverride(t *testing.T) {
	base := AgentAIConfig{Provider: "openai", Model: "m"}

	if got := base.Resolve(AgentAITaskConfig{Provider: "gemini"}).Provider; got != "gemini" {
		t.Fatalf("task override: got %q want gemini", got)
	}
	if got := base.Resolve(AgentAITaskConfig{}).Provider; got != "openai" {
		t.Fatalf("inherit: got %q want openai", got)
	}
}
