package suspect

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

const (
	issueAccount = "111122223333"
	issueRegion  = "eu-west-1"
	clusterARN   = "arn:aws:eks:eu-west-1:111122223333:cluster/ci-eks"
	nodeRoleARN  = "arn:aws:iam::111122223333:role/ci-node"
	runnerARN    = "arn:aws:ecs:eu-west-1:111122223333:service/ci/runner"
	strangerARN  = "arn:aws:ecs:eu-west-1:111122223333:service/shop/web"
)

// issueInventory: the cluster uses its node role (one hop out), the runner
// service runs on the cluster (one hop in), the stranger is unrelated.
func issueInventory() []model.Resource {
	return []model.Resource{
		{ARN: clusterARN, Service: "eks", Type: "cluster", Account: issueAccount, Region: issueRegion, Name: "ci-eks",
			Relations: []model.Relation{{Type: model.RelationAssociated, Target: nodeRoleARN}}},
		{ARN: nodeRoleARN, Service: "iam", Type: "role", Account: issueAccount, Region: model.GlobalRegion, Name: "ci-node"},
		{ARN: runnerARN, Service: "ecs", Type: "service", Account: issueAccount, Region: issueRegion, Name: "runner",
			Relations: []model.Relation{{Type: model.RelationMemberOf, Target: clusterARN}}},
		{ARN: strangerARN, Service: "ecs", Type: "service", Account: issueAccount, Region: issueRegion, Name: "web"},
	}
}

// fakeChanges serves changes and says which windows are settled.
type fakeChanges struct {
	changes []model.Change
	settled bool
}

func (source fakeChanges) Changes(_ context.Context, account, region string, from, to time.Time) ([]model.Change, error) {
	var out []model.Change
	for _, change := range source.changes {
		if change.Account == account && change.Region == region && !change.Time.Before(from) && change.Time.Before(to) {
			out = append(out, change)
		}
	}
	return out, nil
}

func (source fakeChanges) Settled(string, string, time.Time, time.Time) bool { return source.settled }

func issueChange(id, arn, region string, at time.Time) model.Change {
	return model.Change{ID: id, Time: at, Account: issueAccount, Region: region, EventName: "UpdateService",
		Summary: "Runner image updated", Resources: []string{arn},
		Actor: model.Actor{Class: model.ActorPipeline, PrincipalARN: "arn:aws:iam::111122223333:role/ci-deployer"}}
}

// AnchorIssue gives each status in the design's words: a change on the
// owner or a one-hop neighbour is found; a fully read window with none is
// "no change found before it"; an issue with no start "existed when first
// read"; a window not fully read "older than our change history".
func TestAnchorIssueStatuses(t *testing.T) {
	start := time.Date(2026, 11, 25, 13, 2, 0, 0, time.UTC)
	issue := IssueAnchor{ID: "issue-1", Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start, History: 14 * 24 * time.Hour}
	cases := []struct {
		name   string
		issue  IssueAnchor
		source fakeChanges
		status Status
		text   string
	}{
		{"owner changed", issue, fakeChanges{changes: []model.Change{issueChange("c1", clusterARN, issueRegion, start.Add(-2*time.Hour))}, settled: true},
			StatusFound, "Runner image updated by ci-deployer, 2 h before"},
		{"neighbour in changed", issue, fakeChanges{changes: []model.Change{issueChange("c2", runnerARN, issueRegion, start.Add(-5*time.Minute))}, settled: true},
			StatusFound, "Runner image updated by ci-deployer, 5 min before"},
		{"neighbour out changed in the global stream", issue, fakeChanges{changes: []model.Change{issueChange("c3", nodeRoleARN, model.GlobalRegion, start.Add(-3*time.Hour))}, settled: true},
			StatusFound, "Runner image updated by ci-deployer, 3 h before"},
		{"only an unrelated change", issue, fakeChanges{changes: []model.Change{issueChange("c4", strangerARN, issueRegion, start.Add(-time.Hour))}, settled: true},
			StatusNone, IssueCauseNone},
		{"a change after the start", issue, fakeChanges{changes: []model.Change{issueChange("c5", clusterARN, issueRegion, start.Add(time.Minute))}, settled: true},
			StatusNone, IssueCauseNone},
		{"existed when first read", IssueAnchor{ID: "issue-1", Account: issueAccount, Region: issueRegion, Owner: clusterARN},
			fakeChanges{settled: true}, StatusStartUnknown, IssueCauseStartUnknown},
		{"window not fully read", issue, fakeChanges{settled: false}, StatusIncomplete, IssueCauseIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anchor := AnchorIssue(tc.issue, issueInventory())
			result, err := Find(context.Background(), anchor, tc.source, tc.source)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.status || IssueCauseText(result, tc.issue.Start) != tc.text {
				t.Fatalf("status %s text %q; want %s %q", result.Status, IssueCauseText(result, tc.issue.Start), tc.status, tc.text)
			}
		})
	}
}

// The window is the stored change history: the whole retention, but never
// further back than where the history starts.
func TestAnchorIssueLooksBackOverTheStoredHistory(t *testing.T) {
	start := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	issue := IssueAnchor{Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start, History: 14 * 24 * time.Hour}
	if anchor := AnchorIssue(issue, issueInventory()); anchor.Lookback != 14*24*time.Hour || anchor.Kind != AnchorIssueKind {
		t.Fatalf("lookback = %s kind %s; want the retention", anchor.Lookback, anchor.Kind)
	}
	issue.HistoryFrom = start.Add(-3 * 24 * time.Hour)
	if anchor := AnchorIssue(issue, issueInventory()); anchor.Lookback != 3*24*time.Hour {
		t.Fatalf("lookback = %s; want back to where the history starts", anchor.Lookback)
	}
	anchor := AnchorIssue(issue, issueInventory())
	if anchor.Targets[strangerARN] != ScopeNone || anchor.Targets[runnerARN] != ScopeResource || anchor.Targets[nodeRoleARN] != ScopeResource {
		t.Fatalf("targets = %v; want the owner and its one-hop neighbours only", anchor.Targets)
	}
}

// An owner that names the account, not a resource, matches no change.
func TestAnchorIssueOnAnAccountOwner(t *testing.T) {
	start := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	anchor := AnchorIssue(IssueAnchor{Account: issueAccount, Owner: "root", Start: start, History: time.Hour}, issueInventory())
	source := fakeChanges{changes: []model.Change{issueChange("c1", clusterARN, issueRegion, start.Add(-time.Minute))}, settled: true}
	result, err := Find(context.Background(), anchor, source, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusFound || IssueCauseText(result, start) != IssueCauseNone {
		t.Fatalf("an account owner = %s %q", result.Status, IssueCauseText(result, start))
	}
}

// A fully read window that reaches back to the start of the stored change
// history says when that history began; a window inside the history says
// no change was found before the issue; an issue older than the history
// is "older than our change history".
func TestIssueCauseTextSaysWhereTheHistoryBegan(t *testing.T) {
	start := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	quiet := fakeChanges{settled: true}
	for _, tc := range []struct {
		name  string
		issue IssueAnchor
		read  fakeChanges
		want  string
	}{
		{"bounded by the history", IssueAnchor{Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start,
			History: 14 * 24 * time.Hour, HistoryFrom: start.Add(-3 * 24 * time.Hour)}, quiet, "no change found since our change history began on 2026-11-22"},
		{"inside the history", IssueAnchor{Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start,
			History: 24 * time.Hour, HistoryFrom: start.Add(-3 * 24 * time.Hour)}, quiet, IssueCauseNone},
		{"older than the history", IssueAnchor{Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start,
			History: 14 * 24 * time.Hour, HistoryFrom: start.Add(time.Hour)}, fakeChanges{settled: false}, IssueCauseIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Find(context.Background(), AnchorIssue(tc.issue, issueInventory()), tc.read, tc.read)
			if err != nil {
				t.Fatal(err)
			}
			if got := IssueCauseTextFor(result, tc.issue); got != tc.want {
				t.Fatalf("= %q; want %q", got, tc.want)
			}
		})
	}
}
