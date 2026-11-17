package finops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "write each contract view and its field table to testdata/finops/contract for local review")

// contractSnapshotDir receives the -update output. It is the gitignored local
// test-data folder: no test reads it.
var contractSnapshotDir = filepath.Join("..", "..", "testdata", "finops", "contract")

func newStory(t *testing.T) (*Service, *fakeSource, *memStore) {
	t.Helper()
	src, cfg := natStory()
	st := newMemStore()
	return NewService(src, st, cfg).WithSuspectFinder(storySuspects()), src, st
}

// syncedStory is the reference story after one sync run: detection has been
// persisted the way the deployment's refresh cadence does it, so the views
// read stored increases instead of detecting them.
func syncedStory(t *testing.T) (*Service, *fakeSource, *memStore) {
	t.Helper()
	svc, src, st := newStory(t)
	if _, err := svc.SyncIncreases(t.Context()); err != nil {
		t.Fatalf("sync increases: %v", err)
	}
	return svc, src, st
}

var october = Query{Period: "2026-10"}

func TestStorySummaryAndAnswer(t *testing.T) {
	svc, _, _ := syncedStory(t)
	v, err := svc.Summary(t.Context(), october)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || v.Measure != MeasureNetAmortized || v.Links.UI != "/aws/finops?period=2026-10" {
		t.Fatalf("header %+v", v.Header)
	}
	// Oct: NAT 8×20 + 23×85, VM 31×300, storage 31×55, logs 31, data 31×10 +
	// replica 12×40, fee 1,200, credit −500.
	want := usd(160+1955) + usd(9300) + usd(1705) + usd(31) + usd(310+480) + usd(1200) - usd(500)
	if v.Total != want {
		t.Fatalf("total %d want %d", v.Total, want)
	}
	if v.Change == nil || v.Change.Direction != "up" || v.Change.Base != usd(600+9000+1500+30+300) {
		t.Fatalf("change %+v", v.Change)
	}
	if len(v.Sparkline) != 31 {
		t.Fatalf("sparkline days %d", len(v.Sparkline))
	}
	if v.Sync.AccountsReadable != 2 || v.Sync.AccountsTotal != 3 || len(v.Sync.MissingAccounts) != 1 ||
		v.Sync.MissingAccounts[0].Reason != "denied" || !v.Sync.Partial {
		t.Fatalf("the unreadable account must be named: %+v", v.Sync)
	}
	if !strings.Contains(v.Answer, "(2 of 3 accounts)") || !strings.Contains(v.Answer, "over the $14,000 budget") ||
		!strings.Contains(v.Answer, "network traffic (NAT) in checkout since Oct 9") {
		t.Fatalf("answer %q", v.Answer)
	}
	// The $610 commitment is computed before other savings: shown, not summed.
	if v.Savings.Monthly != usd(1520+212+300) || v.Savings.Count != 3 {
		t.Fatalf("savings %+v", v.Savings)
	}
	if v.Unallocated.Amount != usd(1200-500) {
		t.Fatalf("unallocated: the account-owned data lines are not unallocated, fees and credits are: %+v", v.Unallocated)
	}
	if v.Forecast.Source != ForecastProvider || v.Forecast.Month != "2026-11" {
		t.Fatalf("forecast %+v", v.Forecast)
	}
	if v.Budget == nil || v.Budget.Source != BudgetFromConfig || !v.Budget.ReadOnly || v.Budget.Status != BudgetAtRisk {
		t.Fatalf("budget %+v", v.Budget)
	}
}

func TestStoryBridge(t *testing.T) {
	svc, _, _ := syncedStory(t)
	v, err := svc.Drivers(t.Context(), october)
	if err != nil {
		t.Fatal(err)
	}
	sum := v.Base
	for _, r := range v.Rows {
		sum += r.Amount
	}
	if v.Other != nil {
		sum += v.Other.Amount
	}
	if sum != v.Total {
		t.Fatalf("bridge rows must land on the total: %d vs %d", sum, v.Total)
	}
	byCause := map[string]BridgeRow{}
	for _, r := range v.Rows {
		byCause[r.Cause] = r
	}
	nat := v.Rows[0]
	if nat.Cause != CauseUsageUp || nat.Service != "network.nat" || nat.ResourceID != "nat-1a" ||
		nat.Label != "Used more: network traffic (NAT) through nat-1a" || nat.Owner.Name != "checkout" ||
		nat.IncreaseID == "" || nat.Status != StatusOpen || nat.Why == nil || nat.Why.State != "found" ||
		nat.Why.Text != "the checkout-api release by github-actions-prod" {
		t.Fatalf("NAT row %+v why %+v", nat, nat.Why)
	}
	if r := byCause[CauseNewResource]; r.Owner.Name != "data platform" || r.Owner.Rule != "account" || r.Since != MustDate("2026-10-20") {
		t.Fatalf("replica row %+v", r)
	}
	if r := byCause[CauseOneOff]; r.Label != "One-time charge: support plan" || r.Owner.Rule != "unallocated" {
		t.Fatalf("fee row %+v", r)
	}
	if r := byCause[CauseDiscountChange]; r.Label != "Credits applied" || r.Amount != usd(-500) {
		t.Fatalf("credit row %+v", r)
	}
	if r := byCause[CausePriceChange]; r.Amount != usd(155) {
		t.Fatalf("price row %+v", r)
	}
	if v.Other == nil || v.Other.Label != "Everything else" {
		t.Fatalf("logs roll into other: %+v", v.Other)
	}
	if !v.OwnerColumn.Show || !v.AccountColumn.Show {
		t.Fatalf("columns %+v %+v", v.OwnerColumn, v.AccountColumn)
	}
	if v.Rows[len(v.Rows)-1].Amount >= 0 {
		t.Fatal("decreases come after increases when the total rose")
	}

	d, err := svc.Drivers(t.Context(), Query{Period: "2026-10", Depth: "details"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Rows[0].Label != "usage_up: EC2 - Other / USE1-NatGateway-Bytes / nat-1a" || d.Rows[0].UsageType == "" {
		t.Fatalf("details label %q", d.Rows[0].Label)
	}
	for i := range d.Rows {
		if d.Rows[i].Amount != v.Rows[i].Amount {
			t.Fatal("depth changes labels, never numbers")
		}
	}
}

func TestOwnerColumnRules(t *testing.T) {
	src, cfg := natStory()
	for i := range src.lines {
		if src.lines[i].Team != "" {
			src.lines[i].Team = "platform"
		}
	}
	v, _ := NewService(src, nil, cfg).Drivers(t.Context(), october)
	if v.OwnerColumn.Show || v.OwnerColumn.Note != "All cost has team=platform" {
		t.Fatalf("same tag: %+v", v.OwnerColumn)
	}
	for i := range src.lines {
		src.lines[i].Team = ""
	}
	cfg.AccountOwners = nil
	v, _ = NewService(src, nil, cfg).Drivers(t.Context(), october)
	if v.OwnerColumn.Show || !strings.HasPrefix(v.OwnerColumn.Note, "Owner not shown") {
		t.Fatalf("no tag: %+v", v.OwnerColumn)
	}
}

func TestStoryIncreasesAndStatus(t *testing.T) {
	svc, _, st := syncedStory(t)
	ctx := t.Context()
	v, err := svc.Increases(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Items) != 2 {
		t.Fatalf("NAT and replica increases expected, got %+v", v.Items)
	}
	nat := v.Items[0]
	if nat.Service != "network.nat" || nat.StartedOn != MustDate("2026-10-09") || nat.Cause != CauseUsageUp ||
		nat.PeriodChange != usd(65*23) || nat.Title != "went up $1,495 in October" {
		t.Fatalf("NAT %+v", nat)
	}
	wantSentence := "Network traffic (NAT) in Production went up $1,495 in October. It started on Oct 9, after the checkout-api release by github-actions-prod. Checkout team owns it. Adding private endpoints for ECR and S3 in vpc-prod would save about $1,520 a month."
	if nat.Sentence != wantSentence {
		t.Fatalf("sentence\n got %q\nwant %q", nat.Sentence, wantSentence)
	}
	rep := v.Items[1]
	if rep.Cause != CauseNewResource || rep.Suspect.State != "none_found" || rep.Owner.Rule != "account" {
		t.Fatalf("replica %+v", rep)
	}

	// Mark expected, then the end date passes while it is still there.
	if _, err := svc.SetIncreaseStatus(ctx, nat.ID, StatusChange{Status: StatusExpected, Reason: "launch traffic"}, "admin"); !errors.Is(err, ErrUntilInvalid) {
		t.Fatalf("end date required: %v", err)
	}
	exp, err := svc.SetIncreaseStatus(ctx, nat.ID, StatusChange{Status: StatusExpected, Reason: "launch traffic", Until: MustDate("2026-11-20")}, "admin")
	if err != nil || exp.Status != StatusExpected {
		t.Fatalf("%+v %v", exp, err)
	}
	open, _ := svc.Increases(ctx, october)
	if len(open.Items) != 1 {
		t.Fatal("an expected increase leaves the open list")
	}
	all, _ := svc.Increases(ctx, Query{Period: "2026-10", Status: "expected"})
	if len(all.Items) != 1 || all.Items[0].Expected.Reason != "launch traffic" {
		t.Fatal("expected filter")
	}
	br, _ := svc.Drivers(ctx, october)
	if br.Rows[0].Status != StatusExpected || br.Rows[0].Expected == nil {
		t.Fatal("an expected increase stays in the bridge with its reason")
	}

	later := NewService(svc.src, st, Config{
		Budgets: svc.cfg.Budgets, ProviderName: "AWS",
		Now: func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) },
	})
	reopened, _ := later.Increases(ctx, october)
	if len(reopened.Items) != 2 {
		t.Fatal("expected increases reopen on their end date")
	}

	if _, err := svc.Increase(ctx, october, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown id")
	}
	det, err := svc.Increase(ctx, october, nat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if det.ResourceData != "available" || len(det.CauseTree) != 1 || det.CauseTree[0].Label != "NAT data processing" ||
		det.CauseTree[0].Resources[0].ID != "nat-1a" || det.UsageSignal == nil || len(det.RelatedSavings) != 1 ||
		len(det.History) != 1 || det.Series[0].Day != MustDate("2026-09-25") {
		t.Fatalf("detail %+v", det)
	}
}

func TestSyncAlertsOnce(t *testing.T) {
	svc, _, _ := newStory(t)
	ctx := t.Context()
	alerts, err := svc.SyncIncreases(ctx)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("%d %v", len(alerts), err)
	}
	for _, a := range alerts {
		if err := svc.MarkIncreaseAlerted(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	again, _ := svc.SyncIncreases(ctx)
	if len(again) != 0 {
		t.Fatalf("increases alert once: %+v", again)
	}
}

func TestStoryPartialDayAndTrend(t *testing.T) {
	svc, _, _ := syncedStory(t)
	v, err := svc.Trend(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	// mtd on Nov 6 with Oct 31 complete is October; Nov 1 is partial and
	// outside the period.
	if v.Period.Key != PeriodMonthToDate || v.Period.Label != "October 2026" || len(v.Days) != 31 {
		t.Fatalf("days %d period %+v", len(v.Days), v.Period)
	}
	if len(v.Sync.PartialDays) != 1 || v.Sync.PartialDays[0] != MustDate("2026-11-01") {
		t.Fatalf("partial days are named: %+v", v.Sync.PartialDays)
	}
	if len(v.Pins) != 2 || len(v.Bands) != 2 || len(v.BudgetPace) != 31 || v.BudgetPace[30] != usd(14_000) {
		t.Fatalf("pins %+v bands %+v pace %d", v.Pins, v.Bands, len(v.BudgetPace))
	}
	var sum Micros
	for _, s := range v.Series {
		for _, x := range s.Values {
			sum += x
		}
	}
	var tot Micros
	for _, x := range v.Totals {
		tot += x
	}
	if sum != tot {
		t.Fatal("series stack to the totals")
	}
}

func TestStoryBreakdownOptions(t *testing.T) {
	svc, _, _ := newStory(t)
	v, err := svc.Breakdown(t.Context(), Query{Period: "2026-10", GroupBy: GroupTeam})
	if err != nil {
		t.Fatal(err)
	}
	opts := map[string]BreakdownOption{}
	for _, o := range v.Options {
		opts[o.GroupBy] = o
	}
	if opts[GroupAccountGroup].Reason != "no_groups" || !opts[GroupTeam].Available {
		t.Fatalf("options %+v", v.Options)
	}
	if !opts[GroupAccount].Available || v.DefaultGroupBy != GroupAccount {
		t.Fatalf("two accounts: account is the default: %+v / %s", v.Options, v.DefaultGroupBy)
	}
	var share int64
	var total Micros
	for _, r := range v.Rows {
		share += r.ShareBP
		total += r.Amount
	}
	if total != v.Total {
		t.Fatal("rows sum to the total")
	}
	found := false
	for _, r := range v.Rows {
		if r.Key == Unallocated {
			found = true
		}
	}
	if !found {
		t.Fatal("Unallocated is its own row, never hidden")
	}

	// Smallest setup: one account, one region, no tags → nothing qualifies
	// and service rows show what is inside them.
	src, cfg := natStory()
	var lines []CostLine
	for _, l := range src.lines {
		if l.Account == acctProd {
			l.Team, l.Region = "", "us-east-1"
			lines = append(lines, l)
		}
	}
	src.lines = lines
	src.status.Accounts = src.status.Accounts[:1]
	src.status.TeamTag.Active = false
	b, _ := NewService(src, nil, cfg).Breakdown(t.Context(), october)
	if b.DefaultGroupBy != "" {
		t.Fatalf("nothing qualifies: %+v", b.Options)
	}
	if len(b.Rows) == 0 || len(b.Rows[0].Inside) == 0 || b.Rows[0].Inside[0].Label != "instances" {
		t.Fatalf("inside rows %+v", b.Rows)
	}
}

func TestStoryAccountsBudgetsSavings(t *testing.T) {
	svc, _, _ := newStory(t)
	ctx := t.Context()
	a, err := svc.Accounts(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Accounts) != 3 {
		t.Fatalf("accounts %+v", a.Accounts)
	}
	for _, r := range a.Accounts {
		if r.ID == acctSandbox && (r.Status != AccountNotReadable || r.Change != nil || r.Kind != AccountCostOnly) {
			t.Fatalf("an unreadable account has no change, not zero: %+v", r)
		}
		if r.ID == acctProd && (r.TopTeam != "checkout" || r.TopDriver == nil || len(r.Sparkline) != 30) {
			t.Fatalf("prod %+v", r)
		}
	}

	if _, err := svc.PutBudgets(ctx, []Budget{{Name: "dup", Scope: ScopeDeployment, Monthly: usd(1)}}); !errors.Is(err, ErrBadBudget) {
		t.Fatal("a UI budget cannot repeat a config scope")
	}
	if _, err := svc.PutBudgets(ctx, []Budget{{Name: "Acme org", Scope: "org", Value: "acme", Monthly: usd(1)}}); !errors.Is(err, ErrBadBudget) {
		t.Fatal("no per-org budgets")
	}
	if _, err := svc.PutBudgets(ctx, []Budget{{Name: "Checkout", Scope: ScopeTeam, Value: "checkout", Monthly: usd(100)}}); err != nil {
		t.Fatal(err)
	}
	bv, _ := svc.Budget(ctx, october)
	if len(bv.Budgets) != 2 || bv.Budgets[1].Source != BudgetFromUI || bv.Budgets[1].ReadOnly || bv.Budgets[1].ForecastSource != ForecastEstimate {
		t.Fatalf("budgets %+v", bv.Budgets)
	}

	if err := svc.DismissSaving(ctx, "x", "", "admin"); !errors.Is(err, ErrReasonRequired) {
		t.Fatal("reason required")
	}
	if err := svc.DismissSaving(ctx, "x", "no", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown saving")
	}
	sv, _ := svc.Savings(ctx, october)
	if err := svc.DismissSaving(ctx, sv.Items[0].ID, "already planned", "admin"); err != nil {
		t.Fatal(err)
	}
	after, _ := svc.Savings(ctx, october)
	if after.Total.Count != sv.Total.Count-1 || after.Items[0].Status != "dismissed" || after.Items[0].Dismissed.Reason != "already planned" {
		t.Fatalf("dismissed %+v", after.Total)
	}
}

func TestReportFreezeAfterBillCloses(t *testing.T) {
	src, cfg := natStory()
	st := newMemStore()
	cfg.Now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }
	early := NewService(src, st, cfg)
	r, err := early.Report(t.Context(), october)
	if err != nil {
		t.Fatal(err)
	}
	if r.CanFreeze || r.FreezeNote != "October may still change until AWS closes the bill on the 5th" {
		t.Fatalf("before the 5th: %+v", r.FreezeNote)
	}
	if _, err := early.FreezeReport(t.Context(), "2026-10", "admin"); !errors.Is(err, ErrBillNotClosed) {
		t.Fatalf("freeze before close: %v", err)
	}
	svc, _, _ := newStory(t)
	svc.store = st
	fr, err := svc.FreezeReport(t.Context(), "2026-10", "admin")
	if err != nil || fr.Frozen == nil || fr.Frozen.By != "admin" {
		t.Fatalf("%+v %v", fr.Frozen, err)
	}
	if _, err := svc.FreezeReport(t.Context(), "2026-10", "admin"); !errors.Is(err, ErrAlreadyFrozen) {
		t.Fatal("frozen once")
	}
	// Restated data must not change the frozen numbers.
	src.lines = append(src.lines, line(MustDate("2026-10-10"), acctProd, "compute.vm", "late", RecordUsage, "", usd(999), 0))
	again, _ := svc.Report(t.Context(), october)
	if again.Summary.Total != fr.Summary.Total || again.Frozen == nil {
		t.Fatal("a frozen report never shifts")
	}
	refs, _ := svc.Reports(t.Context())
	if len(refs) != 1 || refs[0].Month != "2026-10" {
		t.Fatalf("refs %+v", refs)
	}
	if _, err := svc.Report(t.Context(), Query{Period: PeriodLast30Days}); !errors.Is(err, ErrReportNotMonth) {
		t.Fatal("report is per month")
	}
}

func TestDisabledAndWorstCases(t *testing.T) {
	src, cfg := natStory()
	src.status = SourceStatus{Enabled: false, DisabledReason: "cost_data_off"}
	svc := NewService(src, nil, cfg)
	v, err := svc.Summary(t.Context(), Query{})
	if err != nil || v.Enabled || v.Reason != "cost_data_off" || v.Sync != nil {
		t.Fatalf("disabled: %+v %v", v.Header, err)
	}
	b, _ := json.Marshal(v)
	if !bytes.Contains(b, []byte(`"enabled":false,"reason":"cost_data_off"`)) {
		t.Fatalf("disabled JSON %s", b)
	}

	// Connected mid-September: no bridge, a named reason, totals still there.
	src, cfg = natStory()
	src.status.HistoryStart = MustDate("2026-09-12")
	var lines []CostLine
	for _, l := range src.lines {
		if !l.Day.Before(src.status.HistoryStart) {
			lines = append(lines, l)
		}
	}
	src.lines = lines
	svc = NewService(src, nil, cfg)
	br, _ := svc.Drivers(t.Context(), october)
	if br.Available || br.Note != "Compare from Oct (first full month)" || br.Total == 0 {
		t.Fatalf("no base: %+v", br.BridgeBody)
	}
	sum, _ := svc.Summary(t.Context(), october)
	if sum.Change != nil || strings.Contains(sum.Answer, "from September") {
		t.Fatalf("no change without a base: %+v %q", sum.Change, sum.Answer)
	}

	// Brand-new history: learning, no increases, savings still shown.
	src.status.HistoryStart = MustDate("2026-10-20")
	svc = NewService(src, nil, cfg)
	inc, _ := svc.Increases(t.Context(), october)
	if !inc.Learning || len(inc.Items) != 0 || inc.LearningNote != "Learning: increases start after 14 days" {
		t.Fatalf("learning %+v", inc)
	}

	// Skipped calls and other currencies are named, never zero.
	src, cfg = natStory()
	src.status.SkippedCalls = []string{"discount_coverage", "forecast"}
	src.status.Forecast = nil
	src.lines = append(src.lines, CostLine{Day: MustDate("2026-10-05"), Account: acctProd, Service: "compute.vm", RecordType: RecordUsage, Currency: "EUR", NetAmortizedMicros: usd(70)})
	svc = NewService(src, nil, cfg)
	sv, _ := svc.Summary(t.Context(), october)
	if len(sv.Sync.OtherCurrencies) != 1 || sv.Sync.OtherCurrencies[0].Amount != usd(70) {
		t.Fatalf("other currencies %+v", sv.Sync.OtherCurrencies)
	}
	if sv.Forecast.Source != ForecastEstimate || sv.Forecast.Reason != "forecast_skipped" {
		t.Fatalf("forecast %+v", sv.Forecast)
	}
	if !strings.Contains(strings.Join(sv.Sync.Notes, "|"), "Skipped today to stay under 20 requests: discount coverage, forecast") {
		t.Fatalf("notes %v", sv.Sync.Notes)
	}

	// Resource-level data off: the cause tree stops at usage type and says
	// how to turn it on.
	src, cfg = natStory()
	src.status.ResourceData = false
	svc = NewService(src, newMemStore(), cfg)
	if _, err := svc.SyncIncreases(t.Context()); err != nil {
		t.Fatal(err)
	}
	list, _ := svc.Increases(t.Context(), october)
	det, _ := svc.Increase(t.Context(), october, list.Items[0].ID)
	if det.ResourceData != "off" || det.ResourceNote == "" || len(det.CauseTree[0].Resources) != 0 {
		t.Fatalf("resource off %+v", det)
	}
	if det.Increase.Suspect.State != "collectors_pending" {
		t.Fatal("no finder: change history arrives with collectors")
	}

	if _, err := svc.Summary(t.Context(), Query{GroupBy: "org"}); !errors.Is(err, ErrBadQuery) {
		t.Fatal("bad query")
	}
}

// The reads never write and never touch anything but the cache.
func TestReadsDoNotWrite(t *testing.T) {
	svc, _, st := newStory(t)
	ctx := t.Context()
	_, _ = svc.Summary(ctx, october)
	_, _ = svc.Drivers(ctx, october)
	_, _ = svc.Trend(ctx, october)
	_, _ = svc.Breakdown(ctx, october)
	_, _ = svc.Increases(ctx, october)
	_, _ = svc.Savings(ctx, october)
	_, _ = svc.Budget(ctx, october)
	_, _ = svc.Accounts(ctx, october)
	_, _ = svc.Report(ctx, october)
	if len(st.increases) != 0 || len(st.savings) != 0 || len(st.reports) != 0 || len(st.budgets) != 0 {
		t.Fatal("read methods must not write")
	}
}

// contractViews renders every view of the reference story. The golden JSON
// files freeze the API contract and serve as front-end fixtures.
func contractViews(t *testing.T) map[string]any {
	svc, _, _ := syncedStory(t)
	ctx := context.Background()
	must := func(v any, err error) any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	inc := must(svc.Increases(ctx, october)).(IncreasesView)
	src, cfg := natStory()
	src.status = SourceStatus{Enabled: false, DisabledReason: "cost_data_off"}
	return map[string]any{
		"summary":         must(svc.Summary(ctx, october)),
		"bridge":          must(svc.Drivers(ctx, october)),
		"bridge-details":  must(svc.Drivers(ctx, Query{Period: "2026-10", Depth: "details"})),
		"trend":           must(svc.Trend(ctx, october)),
		"breakdown":       must(svc.Breakdown(ctx, october)),
		"increases":       inc,
		"increase-detail": must(svc.Increase(ctx, october, inc.Items[0].ID)),
		"savings":         must(svc.Savings(ctx, october)),
		"budgets":         must(svc.Budget(ctx, october)),
		"accounts":        must(svc.Accounts(ctx, october)),
		"report":          must(svc.Report(ctx, october)),
		"disabled":        must(NewService(src, nil, cfg).Summary(ctx, Query{})),
	}
}

// TestContractFields compares every view of the reference story field by
// field with contractFields, so a contract change is a reviewed edit of that
// table. With -update the rendered views and a regenerated table are written
// to testdata/finops/contract for review; the test still compares.
func TestContractFields(t *testing.T) {
	views := contractViews(t)
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	slices.Sort(names)
	if *update {
		writeContractSnapshot(t, views, names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(views[name])
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for _, field := range flattenJSON(t, raw) {
				got[field.path] = field.value
			}
			want, ok := contractFields[name]
			if !ok {
				t.Fatalf("view %s has no expected fields", name)
			}
			seen := map[string]bool{}
			for _, field := range want {
				seen[field.path] = true
				value, present := got[field.path]
				switch {
				case !present:
					t.Errorf("%s: field missing, want %#v", field.path, field.value)
				case !sameJSONValue(value, field.value):
					t.Errorf("%s = %#v, want %#v", field.path, value, field.value)
				}
			}
			for _, path := range sortedKeys(got) {
				if !seen[path] {
					t.Errorf("%s: unexpected field %#v", path, got[path])
				}
			}
		})
	}
}

// contractField is one leaf of a view's JSON: its path ("sync.notes[0]") and
// value (a string, an int, a bool, nil, emptyList or emptyObject).
type contractField struct {
	path  string
	value any
}

type jsonEmpty string

const (
	emptyList   jsonEmpty = "[]"
	emptyObject jsonEmpty = "{}"
)

// flattenJSON lists every leaf of a JSON document with its path, in document
// order. Numbers stay json.Number.
func flattenJSON(t *testing.T, raw []byte) []contractField {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out []contractField
	var value func(path string) error
	value = func(path string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			empty := true
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				empty = false
				child := key.(string)
				if path != "" {
					child = path + "." + child
				}
				if err := value(child); err != nil {
					return err
				}
			}
			if empty {
				out = append(out, contractField{path, emptyObject})
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			index := 0
			for ; decoder.More(); index++ {
				if err := value(path + "[" + strconv.Itoa(index) + "]"); err != nil {
					return err
				}
			}
			if index == 0 {
				out = append(out, contractField{path, emptyList})
			}
			_, err = decoder.Token()
			return err
		}
		out = append(out, contractField{path, token})
		return nil
	}
	if err := value(""); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameJSONValue compares a decoded leaf with an expected table value.
func sameJSONValue(got, want any) bool {
	switch want := want.(type) {
	case int:
		n, ok := got.(json.Number)
		return ok && n.String() == strconv.Itoa(want)
	default:
		return got == want
	}
}

// writeContractSnapshot writes each rendered view and a regenerated field
// table under the gitignored testdata folder.
func writeContractSnapshot(t *testing.T, views map[string]any, names []string) {
	t.Helper()
	if err := os.MkdirAll(contractSnapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var table strings.Builder
	table.WriteString("var contractFields = map[string][]contractField{\n")
	for _, name := range names {
		indented, err := json.MarshalIndent(views[name], "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(contractSnapshotDir, name+".json"), append(indented, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		table.WriteString("\t" + strconv.Quote(name) + ": {\n")
		for _, field := range flattenJSON(t, indented) {
			table.WriteString("\t\t{" + strconv.Quote(field.path) + ", " + goValue(field.value) + "},\n")
		}
		table.WriteString("\t},\n")
	}
	table.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(contractSnapshotDir, "contract_fields.go.txt"), []byte(table.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "nil"
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return v.String()
	case jsonEmpty:
		if v == emptyList {
			return "emptyList"
		}
		return "emptyObject"
	}
	return fmt.Sprintf("%#v", v)
}

// The model view an agent tool returns must stay small; the summary and
// bridge of the reference story are well under 6 KiB once indentation is
// dropped.
func TestCompactViewsStaySmall(t *testing.T) {
	views := contractViews(t)
	for _, name := range []string{"summary", "increases"} {
		b, _ := json.Marshal(views[name])
		if len(b) > 6*1024 {
			t.Errorf("%s is %d bytes", name, len(b))
		}
	}
}

// The core is cloud-neutral and dependency-free: production files import
// the standard library and the narrative package only.
func TestImportsStayNeutral(t *testing.T) {
	for _, dir := range []string{".", "narrative"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			pf, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range pf.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if p == "github.com/VersusControl/versus-incident/pkg/finops/narrative" && dir == "." {
					continue
				}
				if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
	}
}

// TestEstimatedSharesAreNamedInEveryView: figures built from estimated team
// or region shares say so; sums over whole accounts stay facts when the
// source reports team totals exact per account.
func TestEstimatedSharesAreNamedInEveryView(t *testing.T) {
	for _, exact := range []bool{false, true} {
		src, cfg := natStory()
		for i := range src.lines {
			if src.lines[i].Team == "checkout" {
				src.lines[i].EstimatedTeam = true
				src.lines[i].EstimatedRegion = true
			}
		}
		src.status.TeamExactPerAccount = exact
		cfg.Budgets = append(cfg.Budgets, Budget{Name: "Checkout", Scope: ScopeTeam, Value: "checkout", Monthly: usd(9_000)})
		svc := NewService(src, newMemStore(), cfg)

		bv, err := svc.Budget(context.Background(), Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bv.Budgets {
			if b.Scope == ScopeTeam && b.Estimated == exact {
				t.Fatalf("exact=%v: team budget estimated=%v", exact, b.Estimated)
			}
		}
		av, err := svc.Accounts(context.Background(), october)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range av.Accounts {
			if a.TopTeam == "checkout" && a.TopTeamEstimated == exact {
				t.Fatalf("exact=%v: top team estimated=%v", exact, a.TopTeamEstimated)
			}
		}
		rv, err := svc.Breakdown(context.Background(), Query{Period: "2026-10", GroupBy: GroupRegion})
		if err != nil {
			t.Fatal(err)
		}
		flagged := false
		for _, r := range rv.Rows {
			flagged = flagged || (r.Key == "us-east-1" && r.Estimated)
		}
		if !flagged {
			t.Fatal("a region row over estimated region shares is flagged")
		}
		fv, err := svc.Breakdown(context.Background(), Query{Period: "2026-10", GroupBy: GroupService, Regions: []string{"us-east-1"}, Teams: []string{"checkout"}})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fv.Header.Sync.Estimated, GroupRegion) || slices.Contains(fv.Header.Sync.Estimated, GroupTeam) == exact {
			t.Fatalf("exact=%v: filter notes %v", exact, fv.Header.Sync.Estimated)
		}
		for _, r := range fv.Rows {
			if r.Key == "network.nat" && !r.Estimated {
				t.Fatal("a service row under estimated filters is flagged")
			}
		}
	}
}

// A dismissal follows the resources it was made on: an idle load balancer
// dismissed as "kept for DR" stays dismissed when it comes back, but a
// different idle load balancer of the same account, idle after the first is
// gone, shows as new and the first one's decision stays orphaned.
func TestReturningSavingInheritsDismissalOnlyForTheSameSubject(t *testing.T) {
	svc, src, st := newStory(t)
	ctx := t.Context()
	idleLB := func(id string) IdleResource {
		return IdleResource{Kind: IdleEmptyLB, ID: id, Account: acctProd, Region: "us-east-1", Monthly: usd(40), Basis: BasisListPrice}
	}
	baseIdle := slices.Clone(src.savings.Idle)
	setIdle := func(ids ...string) {
		t.Helper()
		src.savings.Idle = slices.Clone(baseIdle)
		for _, id := range ids {
			src.savings.Idle = append(src.savings.Idle, idleLB(id))
		}
		if err := svc.SyncSavingDecisions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lbItem := func() (SavingView, bool) {
		t.Helper()
		sv, err := svc.Savings(ctx, october)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range sv.Items {
			if it.Kind == SavingIdle && it.Account == acctProd && slices.ContainsFunc(it.ResourceIDs, func(id string) bool { return strings.HasPrefix(id, "lb-") }) {
				return it, true
			}
		}
		return SavingView{}, false
	}

	setIdle("lb-A")
	a, ok := lbItem()
	if !ok || a.Status != "open" {
		t.Fatalf("lb-A must be listed open: %+v %v", a, ok)
	}
	if err := svc.DismissSaving(ctx, a.ID, "kept for DR", "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := lbItem(); got.Status != "dismissed" || got.Dismissed == nil || got.Dismissed.Reason != "kept for DR" {
		t.Fatalf("lb-A after dismissal: %+v", got)
	}

	// lb-A goes away, then comes back: the same resource keeps its dismissal.
	setIdle()
	if _, ok := lbItem(); ok {
		t.Fatal("no idle load balancer is listed while none is idle")
	}
	if _, orphaned := st.orphaned[a.ID]; !orphaned {
		t.Fatal("lb-A's decision must be orphaned while lb-A is not idle")
	}
	setIdle("lb-A")
	if got, _ := lbItem(); got.ID != a.ID || got.Status != "dismissed" {
		t.Fatalf("returning lb-A must keep its dismissal: %+v", got)
	}

	// lb-A is deleted and lb-B is idle in the same account: a new saving.
	setIdle()
	setIdle("lb-B")
	b, ok := lbItem()
	if !ok || b.Title != a.Title {
		t.Fatalf("lb-B must be listed with lb-A's title: %+v %v", b, ok)
	}
	if b.ID == a.ID || b.Status != "open" || b.Dismissed != nil {
		t.Fatalf("lb-B must show as new, not inherit lb-A's dismissal: %+v", b)
	}
	sv, err := svc.Savings(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(sv.Items, func(it SavingView) bool { return it.ID == b.ID && it.Status == "open" }) {
		t.Fatal("lb-B counts as an open saving")
	}
	if dec, kept := st.savings[a.ID]; !kept || dec.Reason != "kept for DR" {
		t.Fatalf("lb-A's decision is kept for audit: %+v", dec)
	}
	if _, orphaned := st.orphaned[a.ID]; !orphaned {
		t.Fatal("lb-A's decision stays orphaned while lb-B is listed")
	}
}
