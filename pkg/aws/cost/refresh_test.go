package cost

import (
	"context"
	"errors"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

var ctx = context.Background()

func sumLines(ls []finops.CostLine, keep func(finops.CostLine) bool) int64 {
	var s int64
	for _, l := range ls {
		if keep == nil || keep(l) {
			s += l.NetAmortizedMicros
		}
	}
	return s
}

func sumRows(rs []row, keep func(row) bool) int64 {
	var s int64
	for _, r := range rs {
		if keep == nil || keep(r) {
			s += r.cost
		}
	}
	return s
}

func TestSetupRefreshSingleAccountLinesAreExact(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 1 || res.Committed[0] != "111111111111|setup" {
		t.Fatalf("committed %v", res.Committed)
	}
	// Four tables, forecast and coverage: six paid requests, each counted.
	if used := h.adapter.Meter().Used(ctx); used != 6 {
		t.Fatalf("meter used %d, want 6", used)
	}
	paid := 0
	for _, c := range []string{callAccount, callUsage, callTeam, callRecord, callForecast, callCoverage} {
		paid += h.fake.count(c)
	}
	if paid != 6 {
		t.Fatalf("paid requests sent %d, want 6", paid)
	}
	for host := range h.fake.hosts {
		switch host {
		case costExplorerURL, budgetsHost, optimizationURL:
		default:
			t.Fatalf("request left the pinned hosts: %s", host)
		}
	}

	src := h.adapter.Source()
	rows := sc.rows()
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	// Every day's total equals the bill to the micro-dollar.
	sc.Range.Each(func(d finops.Date) {
		got := sumLines(lines, func(l finops.CostLine) bool { return l.Day == d })
		want := sumRows(rows, func(r row) bool { return r.day == d })
		if got != want {
			t.Fatalf("%s total %d, want %d", d, got, want)
		}
	})
	// Usage types are exact for a single account, with region from the
	// usage-type prefix.
	nat := sumLines(lines, func(l finops.CostLine) bool { return l.UsageType == "USE1-NatGateway-Bytes" })
	if want := sumRows(rows, func(r row) bool { return r.usageType == "USE1-NatGateway-Bytes" }); nat != want {
		t.Fatalf("NAT %d, want %d", nat, want)
	}
	regions := map[string]string{}
	for _, l := range lines {
		regions[l.UsageType] = l.Region
		if l.UsageType == "USE1-NatGateway-Bytes" && (l.Service != "network.nat" || l.Team != "checkout") {
			t.Fatalf("NAT line mapped to %s team %q", l.Service, l.Team)
		}
	}
	for ut, want := range map[string]string{
		"USE1-NatGateway-Bytes": "us-east-1", "EUW1-BoxUsage:t3.medium": "eu-west-1",
		"TimedStorage-ByteHrs": "us-east-1", "US-DataTransfer-Out-Bytes": finops.GlobalRegion,
	} {
		if regions[ut] != want {
			t.Fatalf("%s region %q, want %q", ut, regions[ut], want)
		}
	}
	// Credits, tax and the unused commitment are their own lines.
	for rt, want := range map[string]string{"Credit": "billing.credit", "Tax": "billing.tax", "SavingsPlanRecurringFee": "commitment.unused"} {
		got := sumLines(lines, func(l finops.CostLine) bool { return l.UsageType == rt && l.Service == want })
		if exp := sumRows(rows, func(r row) bool { return r.recordType == rt }); got != exp || got == 0 {
			t.Fatalf("%s line %d, want %d", rt, got, exp)
		}
	}
	// Team totals per service are exact; usage type × team cross cells are
	// flagged estimated.
	for _, team := range []string{"checkout", "platform", ""} {
		got := sumLines(lines, func(l finops.CostLine) bool {
			return l.ServiceName == "Amazon Elastic Compute Cloud - Compute" && l.Team == team && l.RecordType == finops.RecordUsage
		})
		want := sumRows(rows, func(r row) bool {
			return r.service == "Amazon Elastic Compute Cloud - Compute" && r.team == team && isUsageRecord(r.recordType)
		})
		if got != want {
			t.Fatalf("team %q compute %d, want %d", team, got, want)
		}
	}

	cross := false
	for _, l := range lines {
		if l.ServiceName == "Amazon Elastic Compute Cloud - Compute" && l.RecordType == finops.RecordUsage {
			cross = cross || l.EstimatedTeam
		}
		if l.ServiceName == "EC2 - Other" && l.EstimatedTeam {
			t.Fatalf("a one-team service is exact: %+v", l)
		}
	}
	if !cross {
		t.Fatal("compute usage types across two teams are estimated cross cells")
	}

	st, err := src.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.LastCompleteDay != finops.MustDate("2026-11-07") || st.HistoryStart != finops.MustDate("2026-10-01") {
		t.Fatalf("status %+v", st)
	}
	if st.Currency != "USD" || !st.TeamTag.Active || !st.TeamTag.Allowlisted || st.MeterUsed != 6 || st.MeterCap != 20 {
		t.Fatalf("status %+v", st)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Name != "Production" || st.Accounts[0].Kind != finops.AccountConnected || !st.Accounts[0].Readable {
		t.Fatalf("accounts %+v", st.Accounts)
	}
	// The forecast covers the whole month: days before today from the
	// cache plus the provider's forecast for the rest.
	mtd := sumRows(rows, func(r row) bool { return r.day.Month == 11 && r.day.Day < 9 })
	mean, _ := finops.ParseMicros(sc.Forecast["mean"])
	if st.Forecast == nil || st.Forecast.Month != "2026-11" || st.Forecast.Mean != mtd+mean {
		t.Fatalf("forecast %+v, want mean %d", st.Forecast, mtd+mean)
	}
	assertBudgets(t, sc, st)
	in, err := src.SavingsInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if in.Coverage == nil || in.Coverage.CoveredBP != 7_100 || len(in.Recommendations) != 1 ||
		in.Recommendations[0].Monthly != 61_320_000 || in.Recommendations[0].Team != "checkout" {
		t.Fatalf("savings inputs %+v", in)
	}
}

func TestDailyAndManualRefreshUseTheirBudget(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	est, err := h.adapter.Estimate(ctx, KindScheduled)
	if err != nil || est.Estimate != 6 {
		t.Fatalf("daily estimate %+v %v", est, err)
	}
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil || len(res.Committed) != 1 || res.Committed[0] != "111111111111|daily" {
		t.Fatalf("%+v %v", res, err)
	}
	if est, _ := h.adapter.Estimate(ctx, KindManual); est.Estimate != 2 {
		t.Fatalf("manual estimate %+v", est)
	}
	if _, err := h.adapter.Refresh(ctx, KindManual); err != nil {
		t.Fatal(err)
	}
	if used := h.adapter.Meter().Used(ctx); used != 14 {
		t.Fatalf("used %d, want 6+6+2", used)
	}
	if _, err := h.adapter.Refresh(ctx, KindManual); !errors.Is(err, ErrManualUsed) {
		t.Fatalf("second manual refresh: %v", err)
	}
	// Reading through the FinOps service spends nothing and calls nothing.
	before := h.fake.requests()
	svc := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now})
	sum, err := svc.Summary(ctx, finops.Query{})
	if err != nil || !sum.Enabled {
		t.Fatalf("summary %+v %v", sum.Header, err)
	}
	if _, err := svc.Drivers(ctx, finops.Query{}); err != nil {
		t.Fatal(err)
	}
	if h.fake.requests() != before || h.adapter.Meter().Used(ctx) != 14 {
		t.Fatal("reading the cache must not call AWS or spend a request")
	}
}

// assertBudgets checks provider budgets: one net amortized measure, one
// account or one team; everything else is skipped by name with a reason.
func assertBudgets(t *testing.T, sc *scenario, st finops.SourceStatus) {
	t.Helper()
	got := map[string]string{}
	for _, b := range st.Budgets {
		scope := b.Scope
		if b.Value != "" {
			scope += ":" + b.Value
		}
		if b.Source != finops.BudgetFromProvider {
			t.Fatalf("budget source %+v", b)
		}
		got[b.Name] = scope
	}
	if len(got) != len(sc.ExpectedBudgets) {
		t.Fatalf("budgets %v, want %v", got, sc.ExpectedBudgets)
	}
	for name, scope := range sc.ExpectedBudgets {
		if got[name] != scope {
			t.Fatalf("budget %q scope %q, want %q", name, got[name], scope)
		}
	}
	skipped := map[string]string{}
	for _, s := range st.SkippedBudgets {
		skipped[s.Name] = s.Reason
	}
	if len(skipped) != len(sc.ExpectedSkipped) {
		t.Fatalf("skipped %v, want %v", skipped, sc.ExpectedSkipped)
	}
	for name, reason := range sc.ExpectedSkipped {
		if skipped[name] != reason {
			t.Fatalf("skipped %q reason %q, want %q", name, skipped[name], reason)
		}
	}
}

func TestProviderBudgetsMapOnlyWhatTheyMean(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	st, err := h.adapter.Source().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgets(t, sc, st)
	v, err := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now}).Budget(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Budgets) != len(sc.ExpectedBudgets) || len(v.Skipped) != len(sc.ExpectedSkipped) {
		t.Fatalf("budget view %d budgets %d skipped", len(v.Budgets), len(v.Skipped))
	}
}
