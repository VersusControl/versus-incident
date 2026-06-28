package eino

import "testing"

// TestIsFixedSamplingModel is the truth table for the OpenAI beta-limited /
// reasoning family detector. It pins the matched families (o1/o3/o4 + gpt-5),
// the case/space tolerance, the separator boundary (so suffixed variants match
// but unrelated lookalikes do not), and that ordinary chat models and
// non-OpenAI ids are NOT treated as fixed-sampling.
func TestIsFixedSamplingModel(t *testing.T) {
	fixed := []string{
		"o1",
		"o1-mini",
		"o1-preview",
		"o1-2024-12-17",
		"o3",
		"o3-mini",
		"o4-mini",
		"o4-mini-2025-04-16",
		"gpt-5",
		"gpt-5-mini",
		"gpt-5.4-mini", // the default config model
		"gpt-5.4",
		// case / whitespace tolerance
		"O1-MINI",
		"  gpt-5.4-mini  ",
	}
	for _, m := range fixed {
		if !isFixedSamplingModel(m) {
			t.Errorf("isFixedSamplingModel(%q) = false, want true", m)
		}
	}

	notFixed := []string{
		"",
		"gpt-4o",
		"gpt-4o-mini",
		"gpt-4-turbo",
		"gpt-50",  // shares "gpt-5" leading chars but no separator boundary
		"o10",     // shares "o1" leading chars but no separator boundary
		"o5-mini", // not (yet) a recognised family
		"deepseek-chat",
		"qwen-plus",
		"claude-3-5-sonnet-20241022",
	}
	for _, m := range notFixed {
		if isFixedSamplingModel(m) {
			t.Errorf("isFixedSamplingModel(%q) = true, want false", m)
		}
	}
}
