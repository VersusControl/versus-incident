package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type GetAlertDecision struct {
	Provider AlertDecisionProvider
	Scope    tenancy.OrgScope
	Redactor LineRedactor
}

func (GetAlertDecision) Name() string        { return "get_alert_decision" }
func (GetAlertDecision) DisplayName() string { return "Inspecting alert decision" }
func (GetAlertDecision) Description() string {
	return "Inspect a provider-neutral decision for an alert, incident, or fingerprint, including action, outcome, reason availability, evidence, provenance, and timestamp."
}
func (GetAlertDecision) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"identifier"}, "properties": map[string]any{
		"identifier": map[string]any{"type": "string", "description": "Required. An exact alert id, incident id, or fingerprint understood by the configured provider."},
	}}
}
func (tool GetAlertDecision) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	var parsed struct {
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
	}
	parsed.Identifier = strings.TrimSpace(parsed.Identifier)
	if parsed.Identifier == "" {
		err := errors.New("identifier is required")
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	if tool.Provider == nil {
		result := core.UnavailableToolResult(tool.Name(), "alert decision provider is not configured")
		result.Data = map[string]any{
			"identifier": parsed.Identifier, "configured": false, "licensed": CapabilityStatusUnknown,
			"available": CapabilityStatusFalse, "reason_known": false,
			"reason": "alert decision records are unavailable", "setup_action": "Configure an alert decision provider for this deployment.",
		}
		return result, nil
	}
	snapshot, err := tool.Provider.AlertDecision(ctx, tool.Scope.Normalized(), parsed.Identifier)
	if err != nil {
		return nil, fmt.Errorf("get_alert_decision: provider: %w", err)
	}
	if snapshot.Identifier == "" {
		snapshot.Identifier = parsed.Identifier
	}
	if len(snapshot.Evidence) > alertDecisionEvidenceLimit {
		snapshot.Evidence = snapshot.Evidence[:alertDecisionEvidenceLimit]
	}
	snapshot.Identifier = scrubModelText(snapshot.Identifier, tool.Redactor)
	snapshot.DecisionType = scrubModelText(snapshot.DecisionType, tool.Redactor)
	snapshot.Status = scrubModelText(snapshot.Status, tool.Redactor)
	snapshot.Action = scrubModelText(snapshot.Action, tool.Redactor)
	snapshot.Outcome = scrubModelText(snapshot.Outcome, tool.Redactor)
	snapshot.Reason = scrubModelText(snapshot.Reason, tool.Redactor)
	snapshot.Provenance = scrubModelText(snapshot.Provenance, tool.Redactor)
	snapshot.Rule = scrubModelText(snapshot.Rule, tool.Redactor)
	snapshot.Fingerprint = scrubModelText(snapshot.Fingerprint, tool.Redactor)
	for index := range snapshot.Evidence {
		snapshot.Evidence[index] = scrubModelText(snapshot.Evidence[index], tool.Redactor)
	}
	return &core.ToolResult{Tool: tool.Name(), Found: snapshot.Found, Data: map[string]any{"decision": snapshot}}, nil
}
