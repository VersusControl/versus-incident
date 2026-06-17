package detect

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/prompt"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// promptFS holds the detect-only prompt fragments. Drop a new file in
// and add it to promptOrder below.
//
//go:embed prompts/*.md
var promptFS embed.FS

// promptOrder is the canonical concatenation order. Identity first,
// then inputs, then the output contract, then the behavior rules.
var promptOrder = []string{
	"prompts/SOUL.md",
	"prompts/INPUTS.md",
	"prompts/OUTPUT.md",
	"prompts/RULES.md",
}

// systemPrompt is assembled once at package init so every Run call
// gets the same byte-identical prompt (good for model-side caching).
var systemPrompt = prompt.MustAssemble(promptFS, promptOrder)

// SystemPrompt returns the assembled system prompt sent on every
// detect call. Exposed so the admin API can render it for operators.
func SystemPrompt() string { return systemPrompt }

// PromptOrder returns the canonical fragment order so the admin
// endpoint can echo the list back to the UI alongside the assembled
// prompt.
func PromptOrder() []string {
	out := make([]string, len(promptOrder))
	copy(out, promptOrder)
	return out
}

// BuildPrompt builds the (system, user) messages for a single
// AgentResult. Pure function: no I/O, no time. Easy to golden-test.
func BuildPrompt(r core.AgentResult, source, service string, samples []string) (system, user string) {
	if service == "" {
		service = "_unknown"
	}
	// Cap samples defensively — the worker already passes ≤3 but the
	// prompt builder is reusable.
	if len(samples) > 3 {
		samples = samples[:3]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", source)
	fmt.Fprintf(&b, "service: %s\n", service)
	fmt.Fprintf(&b, "verdict: %s\n", r.Verdict.String())
	fmt.Fprintf(&b, "pattern_id: %s\n", r.PatternID)
	fmt.Fprintf(&b, "pattern_template: %s\n", r.Template)
	fmt.Fprintf(&b, "tick_frequency: %d\n", r.Frequency)
	fmt.Fprintf(&b, "ewma_baseline: %.3f\n", r.Baseline)
	// Pre-computed magnitude so the model cites it verbatim instead of
	// dividing frequency by baseline itself.
	if d := r.BaselineDelta(); d != "" {
		fmt.Fprintf(&b, "baseline_delta: %s\n", d)
	}
	if len(samples) > 0 {
		b.WriteString("samples:\n")
		for _, s := range samples {
			fmt.Fprintf(&b, "  - %s\n", utils.OneLine(s, 500))
		}
	}
	return systemPrompt, b.String()
}

// ParseFinding accepts the model's reply and returns an AIFinding.
// Tolerates the most common deviations: leading/trailing prose, ```json
// fences, prepended "Here is the JSON:" preamble.
func ParseFinding(raw string) (*core.AIFinding, error) {
	s := utils.ExtractJSONObject(raw)
	if s == "" {
		return nil, fmt.Errorf("detect: no JSON object found in model reply")
	}
	var f core.AIFinding
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("detect: parse finding: %w", err)
	}
	if f.Title == "" || f.Summary == "" {
		return nil, fmt.Errorf("detect: finding missing required fields (title/summary)")
	}
	f.Severity = utils.NormalizeSeverity(f.Severity)
	if f.Confidence < 0 {
		f.Confidence = 0
	}
	if f.Confidence > 1 {
		f.Confidence = 1
	}
	if len(f.Suggestions) > 5 {
		f.Suggestions = f.Suggestions[:5]
	}
	return &f, nil
}
