// Package ai contains the AI SRE analyzer that turns Unknown / Spike
// AgentResults into structured AIFindings. The implementation is a
// plain net/http client against any OpenAI-compatible
// /chat/completions endpoint (vanilla OpenAI, Azure OpenAI, vLLM,
// LM Studio, Ollama, OpenRouter — anything that speaks the same
// JSON shape).
//
// The system prompt is split across the prompts/ subdirectory in the
// OpenClaw / Versus DevOps Guidelines multi-file style — one file per
// concern (SOUL, INPUTS, OUTPUT, RULES). Each file is embedded at
// build time via go:embed and concatenated in a fixed order to form
// the `system` message. Operators tune the prompt by editing the
// Markdown files and rebuilding — no Go changes required.
package ai

import (
	"embed"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// promptFS holds the prompt fragments. The `prompts/*.md` glob keeps
// the wiring declarative: drop a new file in and add it to
// promptOrder below.
//
//go:embed prompts/*.md
var promptFS embed.FS

// promptOrder is the canonical concatenation order of the prompt
// fragments. Order matters — identity first, then inputs, then the
// output contract, then the behavior rules.
var promptOrder = []string{
	"prompts/SOUL.md",
	"prompts/INPUTS.md",
	"prompts/OUTPUT.md",
	"prompts/RULES.md",
}

// systemPrompt is the assembled system message sent on every AI
// call. Built once at package init so the cost is paid up front and
// every Analyze call gets the same byte-identical prompt (good for
// model-side prompt caching).
var systemPrompt = mustLoadSystemPrompt()

func mustLoadSystemPrompt() string {
	var b strings.Builder
	for i, name := range promptOrder {
		data, err := promptFS.ReadFile(name)
		if err != nil {
			// go:embed guarantees the files exist at build time, so a
			// failure here means promptOrder drifted from the actual
			// files — a programming error worth panicking on.
			panic(fmt.Sprintf("ai: missing embedded prompt %q: %v", name, err))
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.Write(data)
	}
	return b.String()
}

// SystemPrompt returns the assembled system prompt sent on every AI
// call. Exposed so the admin API can render it for operators.
func SystemPrompt() string { return systemPrompt }

// BuildPrompt builds the (system, user) messages for a single AgentResult.
// Pure function: no I/O, no time. Easy to golden-test.
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
		return nil, fmt.Errorf("ai: no JSON object found in model reply")
	}
	var f core.AIFinding
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("ai: parse finding: %w", err)
	}
	if f.Title == "" || f.Summary == "" {
		return nil, fmt.Errorf("ai: finding missing required fields (title/summary)")
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
