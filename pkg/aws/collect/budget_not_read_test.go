package collect

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// eksUnitRun runs only the EKS unit of the production account under a
// per-run inventory budget.
func eksUnitRun(t *testing.T, budget int) (harness, func() UnitReport) {
	t.Helper()
	h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: budget}}
	}))
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "eks"}.Key(): true}
	return h, func() UnitReport {
		t.Helper()
		report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only)
		if err != nil || len(report.Units) != 1 {
			t.Fatalf("eks run = %+v, %v", report, err)
		}
		return report.Units[0]
	}
}

func eksResources(t *testing.T, h harness) (clusters, nodegroups []model.Resource) {
	t.Helper()
	for _, resource := range resourcesByARN(t, h, productionAccount) {
		if resource.Service != "eks" {
			continue
		}
		switch resource.Type {
		case "cluster":
			clusters = append(clusters, resource)
		case "node-group":
			nodegroups = append(nodegroups, resource)
		}
	}
	return clusters, nodegroups
}

// A node group the cluster's node group list names stays in the run's
// resource set when the budget skips its detail: it is there, its detail
// reads "not read". It never disappears as if it were deleted.
func TestSkippedNodegroupDetailKeepsTheListedNodegroup(t *testing.T) {
	h, run := eksUnitRun(t, 0)
	if unit := run(); unit.Outcome != OutcomeCommitted || unit.ErrorCode != "" {
		t.Fatalf("unbudgeted run = %+v", unit)
	}
	_, nodegroups := eksResources(t, h)
	if len(nodegroups) != 1 {
		t.Fatalf("the fixture has %d node groups, want 1", len(nodegroups))
	}

	// DescribeCluster fits; DescribeNodegroup does not. ListNodegroups is
	// a list and always runs to the end.
	h, run = eksUnitRun(t, 1)
	if unit := run(); unit.ErrorCode != CodeCapped {
		t.Fatalf("budget of 1 = %+v, want capped", unit)
	}
	clusters, nodegroups := eksResources(t, h)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %+v", clusters)
	}
	if len(nodegroups) != 1 {
		t.Fatalf("the listed node group is missing from a capped run (cluster not_read %q): it reads as deleted, not as not read",
			clusters[0].Attributes[model.AttrNotRead])
	}
	if len(model.NotReadOperations(nodegroups[0])) == 0 {
		t.Fatalf("the skipped node group does not say its detail was not read: %+v", nodegroups[0])
	}
}

// Rotation reaches every detail read even when the budget is smaller than
// the number of detail operations a unit makes per item: after the first
// run, each detail operation is still sent within a few runs. A window
// that rounds to zero would never read them again. The EKS unit makes two
// detail reads (DescribeCluster, DescribeNodegroup) and a budget of 1.
func TestRotationReadsEveryDetailWhenTheBudgetIsBelowTheOperations(t *testing.T) {
	h, run := eksUnitRun(t, 1)
	run()
	operations := []string{"/clusters/versus-eks", "/clusters/versus-eks/node-groups", "/clusters/versus-eks/node-groups/default"}
	before := map[string]int64{}
	for _, operation := range operations {
		before[operation] = h.fake.PathCount(operation)
	}
	if before[operations[0]] != 1 || before[operations[1]] != 1 {
		t.Fatalf("the first run sent %v; the fixture does not serve these paths", before)
	}
	for index := 0; index < 6; index++ {
		run()
	}
	for _, operation := range operations {
		if sent := h.fake.PathCount(operation) - before[operation]; sent == 0 {
			t.Errorf("%s (an EKS read) was never sent in 6 runs after the first with a budget of 1 for 2 detail reads", operation)
		}
	}
}

// A list that fails for a reason other than the budget keeps the whole
// last good read on a budgeted unit: the stored target health is the
// health the last run read, never replaced by "not read", and the unit's
// last good time does not move.
func TestThrottledListOnABudgetedUnitKeepsTheLastGoodRead(t *testing.T) {
	h, client, run := sizingELB(t)
	if unit := run(); unit.Outcome != OutcomeCommitted || unit.ErrorCode != CodeCapped {
		t.Fatalf("first run = %+v", unit)
	}
	good := syncState(t, h, productionAccount, "elbv2").LastGoodAt
	before, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "elbv2"})
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.throttle = func(form string) bool { return strings.Contains(form, "Action=DescribeTargetGroups") }
	client.mu.Unlock()
	unit := run()
	if unit.Outcome != OutcomeKept || unit.ErrorCode == CodeCapped || unit.ErrorCode == "" {
		t.Fatalf("throttled list = %+v, want kept with the throttle named", unit)
	}
	if state := syncState(t, h, productionAccount, "elbv2"); !state.LastGoodAt.Equal(good) {
		t.Fatalf("the last good read moved: %v -> %v", good, state.LastGoodAt)
	}
	after, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount, Service: "elbv2"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Health, before.Health) {
		t.Fatalf("the stored health changed on a failed run:\nbefore %+v\nafter  %+v", before.Health, after.Health)
	}
}

// A node group read in full and then skipped by the budget keeps the ARN
// it was stored under, so it is the same node group, now not read, and
// never one deleted and another created.
func TestSkippedNodegroupKeepsItsStoredARN(t *testing.T) {
	budget := 0
	h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options {
		return Options{CallBudgets: map[string]int{SourceInventory: budget}}
	}))
	only := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "eks"}.Key(): true}
	run := func() UnitReport {
		t.Helper()
		report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only)
		if err != nil || len(report.Units) != 1 {
			t.Fatalf("eks run = %+v, %v", report, err)
		}
		return report.Units[0]
	}
	run()
	_, before := eksResources(t, h)
	if len(before) != 1 || len(model.NotReadOperations(before[0])) != 0 {
		t.Fatalf("full read node groups = %+v", before)
	}
	budget = 1
	if unit := run(); unit.ErrorCode != CodeCapped {
		t.Fatalf("budget of 1 = %+v, want capped", unit)
	}
	_, after := eksResources(t, h)
	if len(after) != 1 || after[0].ARN != before[0].ARN {
		t.Fatalf("node group ARN %q became %+v on a capped run", before[0].ARN, after)
	}
	if len(model.NotReadOperations(after[0])) == 0 {
		t.Fatalf("the skipped node group is not marked not read: %+v", after[0])
	}
}
