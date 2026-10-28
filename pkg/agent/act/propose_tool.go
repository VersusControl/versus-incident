package act

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type ProposalTool struct {
	Service *Service
}

func (ProposalTool) Name() string { return "propose_action" }

func (ProposalTool) Description() string {
	return "Propose a typed, non-destructive operation for human approval. This never executes an action. Unsupported or destructive requests return a manual guide."
}

func (ProposalTool) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type": map[string]any{"type": "string"},
			"target": map[string]any{"type": "object", "properties": map[string]any{
				"cluster": map[string]any{"type": "string"}, "namespace": map[string]any{"type": "string"},
				"kind": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"},
			}},
			"params": map[string]any{"type": "object"},
			"reason": map[string]any{"type": "string"},
		},
		"required": []string{"type", "target", "params", "reason"},
	}
}

func (tool ProposalTool) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Service == nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "action proposals are unavailable", ErrActionDenied)
	}
	var input ProposalInput
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid action proposal", err)
	}
	result, err := tool.Service.Propose(ctx, input)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "action proposal was denied", err)
	}
	if result.Approval != nil && result.Nonce != "" {
		approval := result.Approval
		core.EmitChatEvent(ctx, core.ChatEvent{
			Kind: core.ChatEventApproval,
			Approval: &core.ChatApproval{
				ID: approval.ID, ProposalID: approval.Proposal.ID, RunID: approval.Proposal.RunID,
				Type:   string(approval.Proposal.Type),
				Target: fmt.Sprintf("%s/%s/%s", approval.Proposal.Target.Namespace, approval.Proposal.Target.Kind, approval.Proposal.Target.Name),
				Effect: approval.Proposal.DryRun, Risk: string(approval.Proposal.Risk),
				State: approval.State, ExpiresAt: approval.ExpiresAt,
			},
			ApprovalNonce: result.Nonce,
		})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "action proposal is unavailable", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "action proposal is unavailable", err)
	}
	delete(data, "nonce")
	return &core.ToolResult{Tool: tool.Name(), Found: true, Data: data}, nil
}

var _ core.Tool = ProposalTool{}
