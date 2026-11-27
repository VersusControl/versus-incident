package suspect

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// The issue's targets stop one hop from the owner: a resource two hops
// away, in either direction, is not a target, and a change on it is not a
// likely cause.
func TestAnchorIssueStopsAtOneHop(t *testing.T) {
	const (
		policyARN = "arn:aws:iam::111122223333:policy/ci-node-extra"
		albARN    = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:loadbalancer/app/ci/1"
	)
	inventory := issueInventory()
	// Two hops out: the node role uses a policy. Two hops in: a load
	// balancer fronts the runner service.
	inventory[1].Relations = []model.Relation{{Type: model.RelationAssociated, Target: policyARN}}
	inventory = append(inventory,
		model.Resource{ARN: policyARN, Service: "iam", Type: "policy", Account: issueAccount, Region: model.GlobalRegion, Name: "ci-node-extra"},
		model.Resource{ARN: albARN, Service: "elasticloadbalancing", Type: "loadbalancer", Account: issueAccount, Region: issueRegion, Name: "ci",
			Relations: []model.Relation{{Type: model.RelationAssociated, Target: runnerARN}}})
	start := time.Date(2026, 11, 25, 13, 2, 0, 0, time.UTC)
	issue := IssueAnchor{ID: "issue-1", Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: start, History: 14 * 24 * time.Hour}
	anchor := AnchorIssue(issue, inventory)
	for arn, want := range map[string]Scope{clusterARN: ScopeResource, nodeRoleARN: ScopeResource, runnerARN: ScopeResource,
		policyARN: ScopeNone, albARN: ScopeNone, strangerARN: ScopeNone} {
		if got := anchor.Targets[arn]; got != want {
			t.Errorf("target %s = %v; want %v", arn, got, want)
		}
	}
	for name, change := range map[string]model.Change{
		"two hops out": issueChange("p1", policyARN, model.GlobalRegion, start.Add(-10*time.Minute)),
		"two hops in":  issueChange("a1", albARN, issueRegion, start.Add(-10*time.Minute)),
	} {
		source := fakeChanges{changes: []model.Change{change}, settled: true}
		result, err := Find(context.Background(), anchor, source, source)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != StatusNone || IssueCauseText(result, start) != IssueCauseNone {
			t.Errorf("%s: a change = %s %q; want %q", name, result.Status, IssueCauseText(result, start), IssueCauseNone)
		}
	}
}

// A regress starts the problem again: anchored on the new start, the
// change just before the regress is the likely cause, and a change before
// the first opening is not chosen over it.
func TestAnchorIssueOnARegressStart(t *testing.T) {
	opened := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	regressed := opened.Add(6 * time.Hour)
	source := fakeChanges{settled: true, changes: []model.Change{
		issueChange("before-open", clusterARN, issueRegion, opened.Add(-5*time.Minute)),
		issueChange("before-regress", runnerARN, issueRegion, regressed.Add(-10*time.Minute)),
		issueChange("after-regress", clusterARN, issueRegion, regressed.Add(time.Minute)),
	}}
	issue := IssueAnchor{ID: "issue-1", Account: issueAccount, Region: issueRegion, Owner: clusterARN, Start: regressed, History: 14 * 24 * time.Hour}
	result, err := Find(context.Background(), AnchorIssue(issue, issueInventory()), source, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusFound || len(result.Candidates) == 0 || result.Candidates[0].Change.ID != "before-regress" ||
		IssueCauseText(result, regressed) != "Runner image updated by ci-deployer, 10 min before" {
		t.Fatalf("regress cause = %s %+v %q", result.Status, result.Candidates, IssueCauseText(result, regressed))
	}
}
