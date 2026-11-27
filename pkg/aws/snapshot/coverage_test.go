package snapshot

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var coverageAccounts = []string{"123400000001", "123400000002", "123400000003", "123400000004"}

func coverageStore(t *testing.T) *Store {
	t.Helper()
	at := &clock{now: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	return New(storage.NewMemory(), tenancy.DefaultOrgScope(), WithClock(at.Now))
}

func mustCommit(t *testing.T, store *Store, unit model.Unit, gaps ...model.Gap) {
	t.Helper()
	if _, err := store.Commit(unit, Candidate{DetailGaps: gaps}); err != nil {
		t.Fatalf("commit %+v: %v", unit, err)
	}
}

func findRow(rows []CoverageRow, source, operation string) (CoverageRow, bool) {
	for _, row := range rows {
		if row.Source == source && row.Operation == operation {
			return row, true
		}
	}
	return CoverageRow{}, false
}

// The coverage read groups gaps per source and operation across accounts,
// sums the per-item counts, sorts each gap into its outcome and counts the
// answers that are not gaps.
func TestCoverageGroupsGapsAcrossAccounts(t *testing.T) {
	store := coverageStore(t)
	ctx := context.Background()
	perAccount := []model.Gap{{Seen: 7, Total: 9}, {Seen: 7, Total: 9}, {Seen: 7, Total: 9}, {Seen: 7, Total: 8}}
	for index, id := range coverageAccounts {
		gap := perAccount[index]
		gap.Reason, gap.Operation = model.GapDenied, "GetQueueAttributes"
		mustCommit(t, store, model.Unit{Account: id, Region: region, Service: "sqs"}, gap)
		if err := store.RecordFailure(model.Unit{Account: id, Region: model.GlobalRegion, Service: "iam"}, "denied"); err != nil {
			t.Fatal(err)
		}
	}
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "elbv2"},
		model.Gap{Reason: model.GapCapped, Operation: "DescribeTargetHealth", Seen: 100, Total: 140})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[1], Region: region, Service: "elbv2"},
		model.Gap{Reason: model.GapCapped, Operation: "DescribeTargetHealth", Seen: 100})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "vpc"},
		model.Gap{Reason: model.GapNotEnabled, Operation: "DescribeFlowLogs"})
	if err := store.RecordSuccess(model.Unit{Account: coverageAccounts[2], Region: region, Service: "appregistry"},
		[]model.Gap{{Reason: model.GapNotInRegion}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailure(model.Unit{Account: coverageAccounts[3], Region: "eu-south-2", Service: "ec2"}, "region_disabled"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPending([]model.Unit{{Account: coverageAccounts[0], Region: region, Service: "sqs"}}); err != nil {
		t.Fatal(err)
	}
	expected := []ScopeAccount{{Account: "123400000005", NotEnabled: []string{"ap-east-1"}}}

	view, err := store.Coverage(ctx, CoverageScope{Expected: expected})
	if err != nil {
		t.Fatal(err)
	}
	want := CoverageSummary{AccountsRead: 4, AccountsTotal: 5, NotRead: 2, Capped: 1, OffInAWS: 1}
	if view.Summary != want {
		t.Fatalf("summary = %+v, want %+v", view.Summary, want)
	}

	urls, ok := findRow(view.NotRead, "sqs", "GetQueueAttributes")
	if !ok || urls.Outcome != model.OutcomeNotRead || urls.Reason != model.GapDenied || !slices.Equal(urls.Accounts, coverageAccounts) ||
		urls.Seen != 28 || urls.Total != 35 || !slices.Equal(urls.Fix, []string{"sqs:GetQueueAttributes"}) {
		t.Fatalf("queue attributes row = %+v", urls)
	}
	iam, ok := findRow(view.NotRead, "iam", "")
	if !ok || len(iam.Accounts) != 4 || !iam.LastGoodAt.IsZero() || !slices.Equal(iam.Fix, catalog.FixActionsFor("iam")) || iam.Hides == "" {
		t.Fatalf("one denial in four accounts = %+v, want one row naming four", iam)
	}
	capped, ok := findRow(view.Capped, "elbv2", "DescribeTargetHealth")
	if !ok || capped.Outcome != model.OutcomeCapped || capped.Seen != 200 || capped.Total != 0 || len(capped.Fix) != 0 || capped.Hides == "" {
		t.Fatalf("capped row = %+v, want more than 200 with no IAM fix", capped)
	}
	off, ok := findRow(view.OffInAWS, "vpc", "DescribeFlowLogs")
	if !ok || off.Outcome != model.OutcomeOffInAWS || off.CantRun == "" || off.Hides != "" {
		t.Fatalf("off in AWS row = %+v", off)
	}
	if view.NotGaps != (CoverageNotGaps{ServicesNotOffered: 1, RegionsNotEnabled: 2}) {
		t.Fatalf("not gaps = %+v", view.NotGaps)
	}
	if _, listed := findRow(view.NotRead, "sqs", ""); listed {
		t.Fatal("a unit on its first read is listed as not read")
	}

	warning, err := store.Warning(ctx, CoverageScope{Expected: expected})
	if err != nil || warning == nil || warning.NotRead != 2 {
		t.Fatalf("warning = %+v, %v", warning, err)
	}

	// A Region filter keeps the global units, which serve every Region.
	scoped, err := store.Coverage(ctx, CoverageScope{Region: "eu-south-2"})
	if err != nil || scoped.Summary.NotRead != 1 || scoped.NotRead[0].Source != "iam" || scoped.NotGaps.RegionsNotEnabled != 1 {
		t.Fatalf("eu-south-2 coverage = %+v, %v", scoped, err)
	}
	single, err := store.Coverage(ctx, CoverageScope{Account: coverageAccounts[1]})
	if err != nil || single.Summary.AccountsTotal != 1 || len(single.Capped) != 1 || single.Capped[0].Seen != 100 {
		t.Fatalf("one account = %+v, %v", single, err)
	}
}

// Capped and Off in AWS rows do not raise the warning line; data that read
// in full raises nothing.
func TestCoverageWarningCountsNotReadOnly(t *testing.T) {
	store := coverageStore(t)
	ctx := context.Background()
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "elbv2"},
		model.Gap{Reason: model.GapCapped, Operation: "DescribeTargetHealth", Seen: 100, Total: 140})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "vpc"},
		model.Gap{Reason: model.GapNotEnabled, Operation: "DescribeFlowLogs"})
	warning, err := store.Warning(ctx, CoverageScope{})
	if err != nil || warning != nil {
		t.Fatalf("warning = %+v, %v, want none", warning, err)
	}
	view, err := store.Coverage(ctx, CoverageScope{})
	if err != nil || view.Summary != (CoverageSummary{AccountsRead: 1, AccountsTotal: 1, Capped: 1, OffInAWS: 1}) {
		t.Fatalf("coverage = %+v, %v", view, err)
	}
	if view.NotRead == nil || len(view.NotRead) != 0 {
		t.Fatalf("not read = %#v, want an empty list", view.NotRead)
	}
}

// With the configured scope known, an account removed from settings and a
// Region the account no longer selects drop out; global units stay, and
// the account total is the configured one.
func TestCoverageFollowsTheConfiguredScope(t *testing.T) {
	store := coverageStore(t)
	ctx := context.Background()
	kept, removed := coverageAccounts[0], coverageAccounts[1]
	mustCommit(t, store, model.Unit{Account: kept, Region: region, Service: "sqs"},
		model.Gap{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 1, Total: 2})
	mustCommit(t, store, model.Unit{Account: kept, Region: "eu-west-1", Service: "ecs"},
		model.Gap{Reason: model.GapDenied, Operation: "DescribeServices"})
	if err := store.RecordFailure(model.Unit{Account: kept, Region: model.GlobalRegion, Service: "iam"}, "denied"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFailure(model.Unit{Account: removed, Region: region, Service: "lambda"}, "denied"); err != nil {
		t.Fatal(err)
	}
	scope := CoverageScope{ExpectedKnown: true, Expected: []ScopeAccount{{Account: kept, Regions: []string{region}}}}
	view, err := store.Coverage(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if view.Summary.AccountsTotal != 1 || view.Summary.AccountsRead != 1 {
		t.Fatalf("summary = %+v, want the one configured account", view.Summary)
	}
	if _, listed := findRow(view.NotRead, "lambda", ""); listed {
		t.Fatal("a removed account's old denial is still listed")
	}
	if _, listed := findRow(view.NotRead, "ecs", "DescribeServices"); listed {
		t.Fatal("a Region the account no longer selects is still listed")
	}
	if _, listed := findRow(view.NotRead, "iam", ""); !listed {
		t.Fatal("a global unit dropped out of the configured scope")
	}
	if row, _ := findRow(view.NotRead, "sqs", "GetQueueAttributes"); !slices.Equal(row.Accounts, []string{kept}) {
		t.Fatalf("configured account row = %+v", row)
	}
}

// A configured account that has nothing read, or is failing as a whole, is
// Not read: the warning line is never empty because an account is missing.
func TestCoverageCountsMissingAndFailingAccountsAsNotRead(t *testing.T) {
	store := coverageStore(t)
	ctx := context.Background()
	read, missing, pending, down := coverageAccounts[0], coverageAccounts[1], coverageAccounts[2], coverageAccounts[3]
	mustCommit(t, store, model.Unit{Account: read, Region: region, Service: "sqs"})
	mustCommit(t, store, model.Unit{Account: down, Region: region, Service: "sqs"})
	if err := store.RecordPending([]model.Unit{{Account: pending, Region: region, Service: "sqs"}}); err != nil {
		t.Fatal(err)
	}
	scope := CoverageScope{ExpectedKnown: true, Expected: []ScopeAccount{
		{Account: read}, {Account: missing}, {Account: pending},
		{Account: down, Disconnected: true, Outage: GapCredentialsInvalid},
	}}
	warning, err := store.Warning(ctx, scope)
	if err != nil || warning == nil || warning.NotRead != 2 {
		t.Fatalf("warning = %+v, %v, want two account rows", warning, err)
	}
	view, err := store.Coverage(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if view.Summary.AccountsRead != 1 || view.Summary.AccountsTotal != 4 {
		t.Fatalf("summary = %+v, want 1 of 4 read", view.Summary)
	}
	var notYet, failing CoverageRow
	for _, row := range view.NotRead {
		switch row.Detail {
		case AccountNotReadYet:
			notYet = row
		case string(GapCredentialsInvalid):
			failing = row
		}
	}
	if notYet.Source != AccountSource || !slices.Equal(notYet.Accounts, []string{missing, pending}) || !notYet.NeverRead || notYet.Outcome != model.OutcomeNotRead {
		t.Fatalf("not read yet row = %+v", notYet)
	}
	if !slices.Equal(failing.Accounts, []string{down}) || failing.NeverRead || failing.Hides == "" {
		t.Fatalf("failing account row = %+v", failing)
	}

	// Only the missing account configured: still a warning, never nil.
	alone, err := store.Warning(ctx, CoverageScope{ExpectedKnown: true, Expected: []ScopeAccount{{Account: missing}}})
	if err != nil || alone == nil || alone.NotRead != 1 {
		t.Fatalf("missing account warning = %+v, %v", alone, err)
	}
}

// A detail gap on a unit that committed is not "never read": its last good
// detail read is unknown. A unit that never read says so, and a failed unit
// with earlier data carries that data's time.
func TestCoverageDatesDetailGapsTruthfully(t *testing.T) {
	at := &clock{now: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store := New(storage.NewMemory(), tenancy.DefaultOrgScope(), WithClock(at.Now))
	ctx := context.Background()
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "sqs"},
		model.Gap{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 1, Total: 2},
		model.Gap{Reason: model.GapCapped, Operation: "GetQueueAttributes", Seen: 200, Total: 300})
	if err := store.RecordFailure(model.Unit{Account: coverageAccounts[0], Region: model.GlobalRegion, Service: "iam"}, "denied"); err != nil {
		t.Fatal(err)
	}
	committed := at.Now()
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "ecs"})
	at.Advance(time.Hour)
	if err := store.RecordFailure(model.Unit{Account: coverageAccounts[0], Region: region, Service: "ecs"}, "denied"); err != nil {
		t.Fatal(err)
	}
	view, err := store.Coverage(ctx, CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	detail, _ := findRow(view.NotRead, "sqs", "GetQueueAttributes")
	capped, _ := findRow(view.Capped, "sqs", "GetQueueAttributes")
	for name, row := range map[string]CoverageRow{"detail": detail, "capped": capped} {
		if row.Source == "" || row.NeverRead || !row.LastGoodAt.IsZero() {
			t.Fatalf("%s row = %+v, want an unknown last good read, not never read", name, row)
		}
	}
	if iam, _ := findRow(view.NotRead, "iam", ""); !iam.NeverRead {
		t.Fatalf("never read unit = %+v", iam)
	}
	if ecs, _ := findRow(view.NotRead, "ecs", ""); ecs.NeverRead || !ecs.LastGoodAt.Equal(committed) {
		t.Fatalf("failed unit with earlier data = %+v, want last good at %s", ecs, committed)
	}
}

// A row that mixes counted gaps with an uncounted one drops its counts, so
// it never reads "21 of 27" for four accounts when one is in neither number.
func TestCoverageMixedCountsDropTheCount(t *testing.T) {
	store := coverageStore(t)
	for index, id := range coverageAccounts {
		gap := model.Gap{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 7, Total: 9}
		if index == len(coverageAccounts)-1 {
			gap.Seen, gap.Total = 0, 0
		}
		mustCommit(t, store, model.Unit{Account: id, Region: region, Service: "sqs"}, gap)
	}
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "elbv2"},
		model.Gap{Reason: model.GapCapped, Operation: "DescribeTargetHealth", Seen: 100, Total: 140})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[1], Region: region, Service: "elbv2"},
		model.Gap{Reason: model.GapCapped, Operation: "DescribeTargetHealth"})
	view, err := store.Coverage(context.Background(), CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := findRow(view.NotRead, "sqs", "GetQueueAttributes"); len(row.Accounts) != 4 || row.Seen != 0 || row.Total != 0 {
		t.Fatalf("mixed denial row = %+v, want no count", row)
	}
	if row, _ := findRow(view.Capped, "elbv2", "DescribeTargetHealth"); len(row.Accounts) != 2 || row.Seen != 0 || row.Total != 0 {
		t.Fatalf("mixed capped row = %+v, want no count", row)
	}
}

// An account whose selected Regions are all off is never read, and Regions
// that are off are not gaps: it raises no account row and no warning. A
// unit stored in a Region now off stays in the configured scope.
func TestCoverageAccountWithEveryRegionOffIsNotAGap(t *testing.T) {
	store := coverageStore(t)
	ctx := context.Background()
	off, mixed := coverageAccounts[0], coverageAccounts[1]
	mustCommit(t, store, model.Unit{Account: mixed, Region: region, Service: "sqs"})
	mustCommit(t, store, model.Unit{Account: mixed, Region: "eu-south-2", Service: "ecs"},
		model.Gap{Reason: model.GapDenied, Operation: "DescribeServices"})
	scope := CoverageScope{ExpectedKnown: true, Expected: []ScopeAccount{
		{Account: off, NotEnabled: []string{"ap-east-1"}},
		{Account: mixed, Regions: []string{region}, NotEnabled: []string{"eu-south-2"}},
	}}
	view, err := store.Coverage(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range view.NotRead {
		if row.Source == AccountSource {
			t.Fatalf("an account with every Region off is a gap: %+v", row)
		}
	}
	if row, listed := findRow(view.NotRead, "ecs", "DescribeServices"); !listed || !slices.Equal(row.Accounts, []string{mixed}) {
		t.Fatalf("a unit in a Region that is off dropped out: %+v", view.NotRead)
	}
	if view.NotGaps.RegionsNotEnabled != 2 || view.Summary.AccountsTotal != 2 {
		t.Fatalf("coverage = %+v", view)
	}

	alone := CoverageScope{ExpectedKnown: true, Expected: []ScopeAccount{{Account: off, NotEnabled: []string{"ap-east-1"}}}}
	if warning, err := store.Warning(ctx, alone); err != nil || warning != nil {
		t.Fatalf("warning = %+v, %v, want none", warning, err)
	}
}

// The Regions a scope reads are its accounts' selected Regions, those AWS
// reports off included, each once, within the Region filter; the global
// Region is not one.
func TestScopeRegionsCountsEachRegionOnce(t *testing.T) {
	scope := CoverageScope{Expected: []ScopeAccount{
		{Account: "123400000001", Regions: []string{"eu-west-1", model.GlobalRegion}, NotEnabled: []string{"ap-east-1"}},
		{Account: "123400000002", Regions: []string{"eu-west-1", "us-east-1"}},
	}}
	if got := scopeRegions(scope); strings.Join(got, ",") != "ap-east-1,eu-west-1,us-east-1" {
		t.Fatalf("regions = %v", got)
	}
	scope.Account, scope.Region = "123400000002", "us-east-1"
	if got := scopeRegions(scope); strings.Join(got, ",") != "us-east-1" {
		t.Fatalf("filtered regions = %v", got)
	}
}
