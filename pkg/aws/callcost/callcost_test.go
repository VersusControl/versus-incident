package callcost

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	quietAccount = "123400000001"
	busyAccount  = "123400000002"
)

var costNow = time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)

type costFixture struct {
	ledger    storage.AWSCallCountStore
	snapshots *snapshot.Store
}

func newCostFixture(t *testing.T) costFixture {
	t.Helper()
	provider := storage.NewMemory()
	ledger, ok := storage.AWSCallCountsOf(provider)
	if !ok {
		t.Fatal("memory provider has no call ledger")
	}
	return costFixture{ledger: ledger, snapshots: snapshot.New(provider, tenancy.DefaultOrgScope())}
}

func (f costFixture) add(t *testing.T, account, region, source string, hoursAgo int, calls, metered, runs int64) {
	t.Helper()
	if err := f.ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{{
		AccountID: account, Region: region, Source: source, Hour: costNow.Add(-time.Duration(hoursAgo) * time.Hour), Calls: calls, Metered: metered, Runs: runs,
		Peak: (calls + runs - 1) / max(runs, 1),
	}}); err != nil {
		t.Fatal(err)
	}
}

func (f costFixture) trails(t *testing.T, account, region string, readEvents ...string) {
	t.Helper()
	var resources []model.Resource
	for index, answer := range readEvents {
		arn := model.ARN("aws", "cloudtrail", region, account, "trail/t"+string(rune('a'+index)))
		resources = append(resources, model.Resource{ARN: arn, Service: collect.TrailsService, Type: collect.TrailType, Account: account, Region: region,
			Name: "t" + string(rune('a'+index)), Attributes: map[string]string{model.AttrReadEvents: answer}})
	}
	if _, err := f.snapshots.Commit(model.Unit{Account: account, Region: region, Service: collect.TrailsService}, snapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
}

func (f costFixture) read(t *testing.T, accounts []string, guardDuty GuardDutyStatus) View {
	t.Helper()
	view, err := Read(context.Background(), Inputs{Ledger: f.ledger, Scope: tenancy.DefaultOrgScope(), Snapshots: f.snapshots, Accounts: accounts,
		Budgets: collect.DefaultCallBudgets(), CallsPerDay: 20000, GuardDuty: guardDuty, Now: costNow})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func near(got, want float64) bool { return math.Abs(got-want) < 0.005 }

// The month's estimate: metered requests at the S3 and KMS prices, extra
// trail copies at $2 per 100,000 events for each copy after the first,
// and the event volume a CloudTrail Lake store or a log pipeline takes in.
// GuardDuty counts only accounts whose GuardDuty is known.
func TestEstimateFromTheLedgerAndTrails(t *testing.T) {
	f := newCostFixture(t)
	f.add(t, quietAccount, "eu-south-2", "elbv2", 1, 1000, 0, 50)
	f.add(t, quietAccount, "eu-south-2", "kms", 2, 10000, 10000, 4)
	f.add(t, quietAccount, model.GlobalRegion, "s3-policy", 3, 100000, 100000, 4)
	// Older than the last 24 hours: left out.
	f.add(t, quietAccount, "eu-south-2", "lambda", 30, 7777, 0, 1)
	f.trails(t, quietAccount, "eu-south-2", collect.ReadEventsYes, collect.ReadEventsYes, collect.ReadEventsNo)
	f.trails(t, quietAccount, "us-east-1", collect.ReadEventsYes)

	view := f.read(t, []string{quietAccount}, nil)
	if view.Calls24h != 111000 || view.Events24h != 111000 || view.Metered24h != 110000 {
		t.Fatalf("24 h = %d calls, %d events, %d metered", view.Calls24h, view.Events24h, view.Metered24h)
	}
	estimate := view.EstimateMonth
	// S3: 100,000 × $0.0004/1,000 × 30 = $1.20; KMS: 10,000 × $0.03/10,000 × 30 = $0.90.
	if !near(estimate.DirectUSD, 2.10) {
		t.Errorf("direct = %v, want 2.10", estimate.DirectUSD)
	}
	// eu-south-2 keeps one extra copy: 11,000 × 30 × $2/100,000 = $6.60;
	// global calls land in us-east-1, where one trail is the free copy.
	if estimate.ExtraTrailCopiesUSD == nil || !near(*estimate.ExtraTrailCopiesUSD, 6.60) || *estimate.ExtraTrailCopies != 1 {
		t.Errorf("extra trail copies = %v / %v, want 6.60 / 1", estimate.ExtraTrailCopiesUSD, estimate.ExtraTrailCopies)
	}
	// 3,330,000 events a month × 1.5 KB.
	if !near(estimate.LakeGB, 5.0) || !near(estimate.SIEMGB, 5.0) {
		t.Errorf("volume = %v / %v GB, want 5", estimate.LakeGB, estimate.SIEMGB)
	}
	// No GuardDuty status is read: the amount is what it costs if it is
	// on, marked not known.
	if estimate.GuardDutyUSD == nil || !near(*estimate.GuardDutyUSD, 13.32) || estimate.GuardDutyKnown {
		t.Errorf("GuardDuty with no status read = %v (known %t), want 13.32 if on, not known", estimate.GuardDutyUSD, estimate.GuardDutyKnown)
	}
	on := f.read(t, []string{quietAccount}, func(string) (bool, bool) { return true, true })
	// 3,330,000 events × $4 per million.
	if on.EstimateMonth.GuardDutyUSD == nil || !near(*on.EstimateMonth.GuardDutyUSD, 13.32) || !on.EstimateMonth.GuardDutyKnown {
		t.Errorf("GuardDuty on = %v, want 13.32", on.EstimateMonth.GuardDutyUSD)
	}
	off := f.read(t, []string{quietAccount}, func(string) (bool, bool) { return false, true })
	if off.EstimateMonth.GuardDutyUSD == nil || *off.EstimateMonth.GuardDutyUSD != 0 {
		t.Errorf("GuardDuty off = %v, want 0", off.EstimateMonth.GuardDutyUSD)
	}
	// Keys and bucket policies are both policy reads.
	if len(view.ByAccountSource) != 2 || view.ByAccountSource[0].Source != collect.SourceHealth || view.ByAccountSource[1].Source != collect.SourcePolicies ||
		view.ByAccountSource[1].Calls != 110000 || view.ByAccountSource[1].Metered != 110000 {
		t.Errorf("by account and source = %+v", view.ByAccountSource)
	}
	budgets := map[string]Budget{}
	for _, budget := range view.Budgets {
		budgets[budget.Source] = budget
	}
	if budgets["calls_per_day"].Limit != 20000 || budgets["calls_per_day"].Used != 111000 ||
		budgets[collect.SourceHealth].Limit != 200 || budgets[collect.SourceHealth].Used != 20 ||
		budgets[collect.SourcePolicies].Used != 25000 || budgets[collect.SourceFindings].Unit != "pages" || len(view.Budgets) != 8 {
		t.Errorf("budgets = %+v", view.Budgets)
	}
}

// A trail whose selectors could not be read leaves the extra copies of its
// account unknown when it might be a copy beyond the free one: the line is
// the known lower bound with the account named. An unreadable trail that
// can only be the free copy (an organization trail alone) is a known zero.
// A GuardDuty status one account lacks counts that account as if GuardDuty
// were on, marked not known. With no trail read anywhere the line is null,
// never zero.
func TestUnknownMultipliersAreBoundsOrNull(t *testing.T) {
	f := newCostFixture(t)
	f.add(t, quietAccount, "eu-south-2", "elbv2", 1, 1000, 0, 50)
	f.add(t, busyAccount, "eu-west-1", "ec2", 1, 500, 0, 25)
	f.trails(t, quietAccount, "eu-south-2", collect.ReadEventsUnknown)
	f.trails(t, busyAccount, "eu-west-1", collect.ReadEventsUnknown, collect.ReadEventsYes, collect.ReadEventsYes)
	view := f.read(t, []string{quietAccount, busyAccount}, func(account string) (bool, bool) { return true, account == quietAccount })
	estimate := view.EstimateMonth
	// The busy account keeps at least one extra copy: 500 × 30 × $2/100,000.
	if estimate.ExtraTrailCopiesUSD == nil || !near(*estimate.ExtraTrailCopiesUSD, 0.30) || *estimate.ExtraTrailCopies != 1 ||
		len(estimate.UnknownAccounts) != 1 || estimate.UnknownAccounts[0] != busyAccount {
		t.Fatalf("lower bound = %v / %v, unknown %v", estimate.ExtraTrailCopiesUSD, estimate.ExtraTrailCopies, estimate.UnknownAccounts)
	}
	// 1,500 events a day × 30 × $4 per million, as if on everywhere.
	if estimate.GuardDutyUSD == nil || !near(*estimate.GuardDutyUSD, 0.18) || estimate.GuardDutyKnown {
		t.Fatalf("GuardDuty = %v (known %t)", estimate.GuardDutyUSD, estimate.GuardDutyKnown)
	}

	// An organization trail a member cannot read, and nothing else: the
	// free copy, so a known zero.
	org := newCostFixture(t)
	org.add(t, quietAccount, "eu-south-2", "elbv2", 1, 1000, 0, 50)
	org.add(t, quietAccount, model.GlobalRegion, "iam", 1, 24, 0, 24)
	org.trails(t, quietAccount, "eu-south-2", collect.ReadEventsUnknown)
	only := org.read(t, []string{quietAccount}, nil).EstimateMonth
	if only.ExtraTrailCopiesUSD == nil || *only.ExtraTrailCopiesUSD != 0 || len(only.UnknownAccounts) != 0 {
		t.Fatalf("organization trail only = %v, unknown %v; want a known $0", only.ExtraTrailCopiesUSD, only.UnknownAccounts)
	}

	// No trail read anywhere: null.
	g := newCostFixture(t)
	g.add(t, quietAccount, "eu-south-2", "elbv2", 1, 1000, 0, 50)
	none := g.read(t, []string{quietAccount}, nil).EstimateMonth
	encoded, err := json.Marshal(none)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if value, present := fields["extra_trail_copies_usd"]; !present || value != nil || len(none.UnknownAccounts) != 1 {
		t.Fatalf("no trail read = %s", encoded)
	}
}

// The last 24 hours are a true 24 hours: the hour the window starts in
// counts only for its part inside the window. The stretch is the
// scheduler's, and a failed ledger write marks the answer incomplete.
func TestWindowStretchAndIncompleteLedger(t *testing.T) {
	f := newCostFixture(t)
	// costNow is 12:30, so the window starts at 12:30 yesterday: the 12:00
	// hour of yesterday counts for half.
	if err := f.ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{
		{AccountID: quietAccount, Region: "eu-south-2", Source: "elbv2", Hour: costNow.Add(-24 * time.Hour).Truncate(time.Hour), Calls: 100, Runs: 1, Peak: 100},
		{AccountID: quietAccount, Region: "eu-south-2", Source: "elbv2", Hour: costNow.Add(-25 * time.Hour), Calls: 1000, Runs: 1, Peak: 1000},
		{AccountID: quietAccount, Region: "eu-south-2", Source: "elbv2", Hour: costNow, Calls: 10, Runs: 1, Peak: 10},
	}); err != nil {
		t.Fatal(err)
	}
	view, err := Read(context.Background(), Inputs{Ledger: f.ledger, Scope: tenancy.DefaultOrgScope(), Snapshots: f.snapshots, Accounts: []string{quietAccount},
		Budgets: collect.DefaultCallBudgets(), CallsPerDay: 20000, Now: costNow,
		Stretched:         func() map[string]int { return map[string]int{quietAccount: 2} },
		LedgerFailedSince: func(since time.Time) bool { return since.Equal(costNow.Add(-24 * time.Hour)) }})
	if err != nil {
		t.Fatal(err)
	}
	if view.Calls24h != 60 {
		t.Fatalf("calls in the last 24 hours = %d, want 60 (half of 100, plus 10)", view.Calls24h)
	}
	// Under the target, but the scheduler still holds the stretch until
	// the account falls below 80% of it: the card says what runs.
	if len(view.Stretched) != 2 || view.Stretched[0].Factor != 2 {
		t.Fatalf("stretched = %+v", view.Stretched)
	}
	if !view.LedgerIncomplete {
		t.Fatal("a failed ledger write in the window did not mark the answer incomplete")
	}
	totals, err := Totals(context.Background(), f.ledger, tenancy.DefaultOrgScope(), costNow)
	if err != nil || totals[quietAccount] != 60 {
		t.Fatalf("totals = %v, %v", totals, err)
	}
}

// Over the daily target the account's inventory and policy reads are
// reported as stretched; a capped read and an optional feature that is
// off show from the stored reads.
func TestStretchedCappedAndOptionalOff(t *testing.T) {
	f := newCostFixture(t)
	f.add(t, quietAccount, "eu-south-2", "elbv2", 1, 1000, 0, 50)
	f.add(t, busyAccount, "eu-south-2", "lambda", 1, 25000, 0, 100)
	if _, err := f.snapshots.Commit(model.Unit{Account: busyAccount, Region: "eu-south-2", Service: "lambda"}, snapshot.Candidate{
		DetailGaps: []model.Gap{{Reason: model.GapCapped, Operation: "ListFunctions", Seen: 400}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.snapshots.RecordFailure(model.Unit{Account: quietAccount, Region: model.GlobalRegion, Service: "aws-health"}, "not_on_plan"); err != nil {
		t.Fatal(err)
	}
	view := f.read(t, []string{quietAccount, busyAccount}, nil)
	if len(view.Stretched) != 2 || view.Stretched[0].Account != busyAccount || view.Stretched[0].Source != collect.SourceInventory ||
		view.Stretched[1].Source != collect.SourcePolicies || view.Stretched[0].Factor != 2 {
		t.Fatalf("stretched = %+v", view.Stretched)
	}
	if view.SourcesCapped != 1 {
		t.Fatalf("sources capped = %d", view.SourcesCapped)
	}
	found := false
	for _, optional := range view.OptionalOff {
		if optional.ID == "aws-health" {
			found = optional.Known && len(optional.AccountsOff) == 1 && optional.AccountsOff[0] == quietAccount
		}
		if optional.Price == "" || optional.Name == "" {
			t.Errorf("optional without AWS's price words: %+v", optional)
		}
	}
	if !found || len(view.OptionalOff) < 5 {
		t.Fatalf("optional off = %+v", view.OptionalOff)
	}
	// The stretched accounts are counted once each, whatever the sources
	// stretched; the inventory budget that capped the lambda read is
	// reached and names its coverage source, so its coverage row matches.
	if view.StretchedAccounts != 1 {
		t.Fatalf("stretched accounts = %d; want 1", view.StretchedAccounts)
	}
	for _, budget := range view.Budgets {
		switch budget.Source {
		case collect.SourceInventory:
			if !budget.Reached || len(budget.CoverageSources) != 1 || budget.CoverageSources[0] != "lambda" {
				t.Fatalf("inventory budget = %+v; want reached, naming lambda", budget)
			}
		case collect.SourcePolicies:
			if budget.CoverageSources == nil || len(budget.CoverageSources) != 0 {
				t.Fatalf("policies budget = %+v; want no coverage source, as an empty list", budget)
			}
		}
	}
	// An account no longer connected is left out.
	if only := f.read(t, []string{quietAccount}, nil); only.Calls24h != 1000 || len(only.Stretched) != 0 || only.SourcesCapped != 0 ||
		only.StretchedAccounts != 0 {
		t.Fatalf("one account = %+v", only)
	}
}

// A budget's use is the largest single run, not the hour's average: ten
// runs of 100 calls where one made 40 use 40 of the budget.
func TestBudgetUseIsTheLargestRun(t *testing.T) {
	f := newCostFixture(t)
	if err := f.ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{
		{AccountID: quietAccount, Region: "eu-south-2", Source: "elbv2", Hour: costNow, Calls: 100, Runs: 10, Peak: 40},
		{AccountID: quietAccount, Region: "eu-west-1", Source: "rds", Hour: costNow, Calls: 30, Runs: 10, Peak: 3},
	}); err != nil {
		t.Fatal(err)
	}
	for _, budget := range f.read(t, []string{quietAccount}, nil).Budgets {
		if budget.Source == collect.SourceHealth && budget.Used != 40 {
			t.Fatalf("health budget used %d, want 40", budget.Used)
		}
	}
}
