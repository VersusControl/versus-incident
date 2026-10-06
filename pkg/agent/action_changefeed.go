package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/core"
)

type actionApprovalSource interface {
	Approvals(context.Context, bool) ([]act.Approval, error)
}

type actionChangeFeed struct {
	approvals actionApprovalSource
}

func newActionChangeFeed(approvals actionApprovalSource) commontools.ChangeFeed {
	if approvals == nil {
		return nil
	}
	return actionChangeFeed{approvals: approvals}
}

func (feed actionChangeFeed) Changes(ctx context.Context, since time.Time) ([]commontools.ChangeRecord, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return []commontools.ChangeRecord{}, nil
	}
	approvals, err := feed.approvals.Approvals(ctx, false)
	if err != nil {
		return nil, err
	}
	changes := make([]commontools.ChangeRecord, 0, len(approvals))
	for _, approval := range approvals {
		if approval.ExecutedAt.IsZero() || !approval.ExecutedAt.After(since) {
			continue
		}
		summary := fmt.Sprintf("%s: %s", approval.Proposal.Type, approval.Proposal.DryRun)
		if approval.State == "verification_failed" {
			summary += " (execution recorded; verification failed)"
		}
		changes = append(changes, commontools.ChangeRecord{
			Timestamp: approval.ExecutedAt, Service: approval.Proposal.Target.Name,
			Kind: "agent_action", Summary: summary, Ref: approval.Proposal.ID,
		})
	}
	return changes, nil
}
