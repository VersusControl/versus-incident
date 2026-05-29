// Package analyze contains the analyze-kind AI agent: operator-
// triggered, tool-using, single-incident investigation. The agent runs
// only via the admin endpoint `POST /api/admin/incidents/:id/analyze`
// and writes its output to the analyses storage blob — it never fans
// out to notification channels (that contract is enforced by the
// import-graph guard test).
package analyze

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/prompt"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

//go:embed prompts/*.md
var promptFS embed.FS

var promptOrder = []string{
	"prompts/SOUL.md",
	"prompts/INPUTS.md",
	"prompts/OUTPUT.md",
	"prompts/RULES.md",
}

var systemPrompt = prompt.MustAssemble(promptFS, promptOrder)

// SystemPrompt returns the assembled system prompt sent on every
// analyze call. Exposed so the admin API can render it for operators.
func SystemPrompt() string { return systemPrompt }

// PromptOrder returns the canonical fragment order so the admin
// endpoint can echo the list back to the UI alongside the assembled
// prompt.
func PromptOrder() []string {
	out := make([]string, len(promptOrder))
	copy(out, promptOrder)
	return out
}

// BuildUserPrompt renders the snapshot as the first user message sent
// to the model. The tool catalog is attached via Eino's ToolInfo
// schema, not in the user message.
func BuildUserPrompt(s core.AnalyzeIncidentSnapshot) string {
	payload := map[string]any{
		"incident_id":  s.IncidentID,
		"title":        s.Title,
		"service":      s.Service,
		"source":       s.Source,
		"severity":     s.Severity,
		"resolved":     s.Resolved,
		"requested_by": s.RequestedBy,
	}
	if !s.CreatedAt.IsZero() {
		payload["created_at"] = s.CreatedAt.UTC().Format(time.RFC3339)
	}
	if s.AckedAt != nil {
		payload["acked_at"] = s.AckedAt.UTC().Format(time.RFC3339)
	}
	if s.ResolvedAt != nil {
		payload["resolved_at"] = s.ResolvedAt.UTC().Format(time.RFC3339)
	}
	if len(s.Content) > 0 {
		payload["content"] = s.Content
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		// Fall back to a degraded but still usable text form so a
		// pathological content map cannot block the whole run.
		return fmt.Sprintf("incident_id: %s\ntitle: %s\nservice: %s\n",
			s.IncidentID, s.Title, s.Service)
	}
	return string(b)
}

// ParseFinding decodes the model's final assistant message into an
// AIFinding. Like the detect parser it tolerates leading prose and
// code fences. Analyze-specific fields (root_cause_hypotheses,
// evidence, related_pattern_ids, next_steps) are passed through
// because AIFinding already declares them.
func ParseFinding(raw string) (*core.AIFinding, error) {
	s := utils.ExtractJSONObject(raw)
	if s == "" {
		return nil, fmt.Errorf("analyze: no JSON object found in model reply")
	}
	var f core.AIFinding
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return nil, fmt.Errorf("analyze: parse finding: %w", err)
	}
	f.Title = strings.TrimSpace(f.Title)
	f.Summary = strings.TrimSpace(f.Summary)
	if f.Title == "" || f.Summary == "" {
		return nil, fmt.Errorf("analyze: finding missing required fields (title/summary)")
	}
	f.Severity = utils.NormalizeSeverity(f.Severity)
	if f.Confidence < 0 {
		f.Confidence = 0
	}
	if f.Confidence > 1 {
		f.Confidence = 1
	}
	if len(f.RootCauseHypotheses) > 5 {
		f.RootCauseHypotheses = f.RootCauseHypotheses[:5]
	}
	if len(f.Evidence) > 8 {
		f.Evidence = f.Evidence[:8]
	}
	if len(f.NextSteps) > 5 {
		f.NextSteps = f.NextSteps[:5]
	}
	if len(f.Suggestions) > 5 {
		f.Suggestions = f.Suggestions[:5]
	}
	if len(f.RootCauseHypotheses) == 0 && len(f.NextSteps) == 0 && len(f.Suggestions) == 0 {
		return nil, fmt.Errorf("analyze: empty finding (no hypotheses or next_steps)")
	}
	return &f, nil
}
