package cost

import (
	"slices"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

func orgHarness(t *testing.T) (*harness, *scenario) {
	sc := loadScenario(t, "organization")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{{AccountID: "999999999999", Organization: true, Credentials: testCreds}}
		o.Connected = []string{"111111111111", "222222222222"}
		o.MaxDrillDowns = 3
		o.MaxPaidRequests = 40
	})
	return h, sc
}

// backfillOrganization runs the next day's refresh, which backfills the
// previous month that an Organization's first setup leaves out.
func backfillOrganization(t *testing.T, h *harness) {
	t.Helper()
	ps := mustStatus(t, h).Payers["999999999999"]
	if ps.HistoryStart != finops.MustDate("2026-11-01") {
		t.Fatalf("first setup reads the current month: history from %s", ps.HistoryStart)
	}
	h.clock.set(at("2026-11-10", 10))
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Committed, "999999999999|backfill") {
		t.Fatalf("backfill not committed: %v", res.Committed)
	}
	if got := mustStatus(t, h).Payers["999999999999"].HistoryStart; got != finops.MustDate("2026-10-01") {
		t.Fatalf("history after backfill from %s", got)
	}
}

func TestOrganizationTwoStageDrillDown(t *testing.T) {
	h, sc := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	// Four tables, three mover drill-downs, forecast and coverage.
	if used := h.adapter.Meter().Used(ctx); used != 9 || h.fake.count(callDrill) != 3 {
		t.Fatalf("used %d drills %d", used, h.fake.count(callDrill))
	}
	backfillOrganization(t, h)
	src := h.adapter.Source()
	rows := sc.rows()
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sumLines(lines, nil), sumRows(rows, nil); got != want {
		t.Fatalf("whole bill %d, want %d", got, want)
	}
	// Account × service lines are exact; usage types are left to the
	// drill-down so line keys stay stable from day to day.
	for _, l := range lines {
		if l.RecordType == finops.RecordUsage && l.UsageType != "" {
			t.Fatalf("organization usage line carries a usage type: %+v", l)
		}
	}
	prod := sumLines(lines, func(l finops.CostLine) bool {
		return l.Account == "111111111111" && l.ServiceName == "Amazon Elastic Compute Cloud - Compute"
	})
	if want := sumRows(rows, func(r row) bool {
		return r.account == "111111111111" && r.service == "Amazon Elastic Compute Cloud - Compute"
	}); prod != want {
		t.Fatalf("production compute %d, want %d", prod, want)
	}
	// Regions split by the service's mix and keep the line exact.
	regions := map[string]bool{}
	for _, l := range lines {
		if l.Account == "111111111111" && l.ServiceName == "Amazon Elastic Compute Cloud - Compute" {
			regions[l.Region] = true
		}
	}
	if !regions["us-east-1"] || !regions["eu-west-1"] {
		t.Fatalf("regions %v", regions)
	}
	// Payer rows are drivers of their own on the payer account.
	unused := sumLines(lines, func(l finops.CostLine) bool { return l.Service == "commitment.unused" && l.Account == "999999999999" })
	fee := sumLines(lines, func(l finops.CostLine) bool { return l.Service == "billing.fee" && l.ServiceName == "Fee" })
	if unused == 0 || fee != sumRows(rows, func(r row) bool { return r.recordType == "Fee" }) {
		t.Fatalf("payer rows unused %d fee %d", unused, fee)
	}

	st, _ := src.Status(ctx)
	kinds := map[string]string{}
	names := map[string]string{}
	for _, a := range st.Accounts {
		kinds[a.ID], names[a.ID] = a.Kind, a.Name
	}
	if kinds["111111111111"] != finops.AccountConnected || kinds["333333333333"] != finops.AccountCostOnly || kinds["999999999999"] != finops.AccountCostOnly {
		t.Fatalf("kinds %v", kinds)
	}
	if names["333333333333"] != "Sandbox" || names["999999999999"] != "Management" {
		t.Fatalf("names from Cost Explorer %v", names)
	}
	if fc := st.Accounts; len(fc) != 4 {
		t.Fatalf("accounts %+v", fc)
	}

	// Drilled detail is exact per usage type for its account and service.
	var drilled []string
	ps := mustStatus(t, h).Payers["999999999999"]
	for k := range ps.Drilled {
		drilled = append(drilled, k)
	}
	if len(drilled) != 3 {
		t.Fatalf("drilled %v", drilled)
	}
	account, service := splitPair(drilled[0])
	detail, ok, err := src.UsageDetail(ctx, account, service, sc.Range)
	if err != nil || !ok {
		t.Fatalf("detail %v %v", ok, err)
	}
	if got, want := sumLines(detail, nil), sumRows(rows, func(r row) bool {
		return r.account == account && r.service == service && isUsageRecord(r.recordType)
	}); got != want || got == 0 {
		t.Fatalf("drilled %s: %d, want %d", drilled[0], got, want)
	}

	// A click on an undrilled pair spends one request; a second click the
	// same day spends nothing.
	var free string
	for _, p := range []string{pairKey("333333333333", "Amazon Simple Storage Service"), pairKey("222222222222", "Amazon Relational Database Service"), pairKey("111111111111", "EC2 - Other"), pairKey("222222222222", "Amazon Elastic Compute Cloud - Compute")} {
		if _, done := ps.Drilled[p]; !done {
			free = p
			break
		}
	}
	a, s := splitPair(free)
	if _, ok, _ := src.UsageDetail(ctx, a, s, sc.Range); ok {
		t.Fatal("no detail before the click")
	}
	used := h.adapter.Meter().Used(ctx)
	if err := h.adapter.DrillDown(ctx, a, s); err != nil {
		t.Fatal(err)
	}
	if err := h.adapter.DrillDown(ctx, a, s); err != nil {
		t.Fatal(err)
	}
	if h.adapter.Meter().Used(ctx) != used+1 {
		t.Fatalf("click drill spent %d", h.adapter.Meter().Used(ctx)-used)
	}
	if _, ok, _ := src.UsageDetail(ctx, a, s, sc.Range); !ok {
		t.Fatal("detail cached after the click")
	}
	if err := h.adapter.DrillDown(ctx, "444444444444", s); err == nil {
		t.Fatal("an account no payer covers is refused")
	}
}

func mustStatus(t *testing.T, h *harness) *statusRecord {
	t.Helper()
	st, err := h.adapter.cache.status()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMonthCloseOnTheFifthFreezesTheMonth(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	h.clock.set(at("2026-11-04", 10))
	sc.PartialFrom = finops.MustDate("2026-11-01")
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if mustStatus(t, h).Payers["111111111111"].ClosedThrough != "" {
		t.Fatal("October is open before the 5th")
	}
	h.clock.set(at("2026-11-05", 10))
	sc.PartialFrom = finops.MustDate("2026-11-01")
	est, _ := h.adapter.Estimate(ctx, KindScheduled)
	closeCalls := 0
	for _, c := range est.Run {
		if c.Group == "111111111111|close" {
			closeCalls++
		}
	}
	if closeCalls != 4 {
		t.Fatalf("month close calls %d, want 4", closeCalls)
	}
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil || !slices.Contains(res.Committed, "111111111111|close") {
		t.Fatalf("%+v %v", res.Committed, err)
	}
	if ps := mustStatus(t, h).Payers["111111111111"]; !ps.closed("2026-10") || ps.ClosedThrough != "2026-10" {
		t.Fatal("October closed")
	}
	// Closed months are not rewritten by later refreshes.
	before, _ := h.adapter.Source().Lines(ctx, finops.DateRange{Start: finops.MustDate("2026-10-31"), End: finops.MustDate("2026-10-31")})
	sc.Series[1].Steps = append(sc.Series[1].Steps, scenarioStep{From: finops.MustDate("2026-10-31"), Cost: "1", Quantity: "1"})
	h.clock.set(at("2026-11-02", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	after, _ := h.adapter.Source().Lines(ctx, finops.DateRange{Start: finops.MustDate("2026-10-31"), End: finops.MustDate("2026-10-31")})
	if sumLines(after, nil) != sumLines(before, nil) {
		t.Fatal("a closed month changed")
	}
	h.clock.set(at("2026-11-06", 10))
	est, _ = h.adapter.Estimate(ctx, KindScheduled)
	for _, c := range est.Run {
		if c.Group == "111111111111|close" {
			t.Fatal("the month closes once")
		}
	}
}

func TestPlannerDropsOptionalCallsInFixedOrder(t *testing.T) {
	today := finops.MustDate("2026-11-05")
	o := Options{MaxDrillDowns: 5, ResourceServices: func() []string { return []string{"EC2 - Other"} }}.withDefaults()
	st := &statusRecord{Payers: map[string]*payerState{}}
	for _, id := range []string{"111111111111", "222222222222", "333333333333"} {
		o.Payers = append(o.Payers, Payer{AccountID: id})
		st.Payers[id] = &payerState{HistoryStart: finops.MustDate("2026-09-01"), ClosedThrough: "2026-10"}
	}
	pl := planCalls(o, st, nil, KindScheduled, today, 100)
	full := finops.BuildPlan(pl.calls, 100)
	if full.Estimate != 3*7 {
		t.Fatalf("three payers want %d, plan %d", 3*7, full.Estimate)
	}
	p := finops.BuildPlan(pl.calls, 20)
	if p.Estimate > 20 {
		t.Fatalf("plan %d over the cap", p.Estimate)
	}
	var dropped []string
	for _, s := range p.Skipped {
		dropped = append(dropped, s.Call.Name)
		if s.Call.Required {
			t.Fatalf("a required call dropped while optional ones fit: %+v", s)
		}
	}
	if len(dropped) != 1 || dropped[0] != callResources {
		t.Fatalf("dropped %v, want resource detail first", dropped)
	}
	p = finops.BuildPlan(pl.calls, 15)
	order := []string{}
	for _, s := range p.Skipped {
		order = append(order, s.Call.Name)
	}
	want := []string{callResources, callResources, callResources, callCoverage, callCoverage, callCoverage}
	if !slices.Equal(order, want) {
		t.Fatalf("drop order %v, want %v", order, want)
	}
	p = finops.BuildPlan(pl.calls, 11)
	for _, r := range p.Run {
		if r.Group == "333333333333|daily" {
			t.Fatal("a payer whose four tables do not fit is dropped whole")
		}
	}
	// Organization paging: about one more page per 50 linked accounts.
	if pagesFor(48, false) != 1 || pagesFor(150, false) != 3 || pagesFor(1, true) != 2 {
		t.Fatal("page estimate")
	}
}

func teamTotalsOf(lines []finops.CostLine) map[[2]string]int64 {
	out := map[[2]string]int64{}
	for _, l := range lines {
		if l.RecordType == finops.RecordUsage {
			out[[2]string{l.Account, l.Team}] += l.NetAmortizedMicros
		}
	}
	return out
}

func TestOrganizationTeamSharesAreExactOrFlagged(t *testing.T) {
	sc := loadScenario(t, "organization")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{{AccountID: "999999999999", Organization: true, Credentials: testCreds}}
		o.Connected = []string{"111111111111", "222222222222"}
		o.MaxDrillDowns = 1
		o.MaxPaidRequests = 40
	})
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	// Keep the NAT pair undrilled through the backfill day.
	h.fake.failures[callDrill] = fakeFailure{Type: "LimitExceededException", Message: "Rate exceeded"}
	backfillOrganization(t, h)
	delete(h.fake.failures, callDrill)
	src := h.adapter.Source()
	rows := sc.rows()
	want := map[[2]string]int64{}
	for _, r := range rows {
		if isUsageRecord(r.recordType) {
			want[[2]string{r.account, r.team}] += r.cost
		}
	}
	check := func(stage string) []finops.CostLine {
		lines, err := src.Lines(ctx, sc.Range)
		if err != nil {
			t.Fatal(err)
		}
		got := teamTotalsOf(lines)
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: account × team %v = %d, want %d exactly", stage, k, got[k], v)
			}
		}
		return lines
	}
	lines := check("before the drill-down")
	natEstimated := false
	for _, l := range lines {
		if l.Account == "111111111111" && l.ServiceName == "EC2 - Other" {
			natEstimated = natEstimated || l.EstimatedTeam
		}
		if l.Account == "111111111111" && l.ServiceName == "Amazon Elastic Compute Cloud - Compute" && (l.EstimatedTeam || l.EstimatedRegion || l.Team != "checkout") {
			t.Fatalf("the drilled pair is exact: %+v", l)
		}
	}
	if !natEstimated {
		t.Fatal("an undrilled cross cell (two services, two teams) must be flagged estimated")
	}
	svc := finops.NewService(src, NewStore(h.provider, h.adapter.cache.scope), finops.Config{Now: h.clock.Now})
	natIncrease := func() finops.IncreaseView {
		if _, err := svc.SyncIncreases(ctx); err != nil {
			t.Fatal(err)
		}
		list, err := svc.Increases(ctx, finops.Query{Status: "all"})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range list.Items {
			if v.Account == "111111111111" && v.Service == "EC2 - Other" {
				return v
			}
		}
		t.Fatalf("NAT increase not detected: %+v", list.Items)
		return finops.IncreaseView{}
	}
	inc := natIncrease()
	if !inc.Owner.Estimated || !strings.Contains(inc.Sentence, "(estimated)") || !strings.Contains(inc.Sentence, "Likely") {
		t.Fatalf("an estimated owner is never stated as fact: %+v / %q", inc.Owner, inc.Sentence)
	}
	acc, err := svc.Accounts(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acc.Accounts {
		if a.TopTeamEstimated {
			t.Fatalf("top team per account is an exact sum: %+v", a)
		}
	}

	// Drilling the NAT pair (usage type × team) makes its owner exact.
	if err := h.adapter.DrillDown(ctx, "111111111111", "EC2 - Other"); err != nil {
		t.Fatal(err)
	}
	lines = check("after the drill-down")
	for _, l := range lines {
		if l.Account == "111111111111" && (l.ServiceName == "EC2 - Other" || l.ServiceName == "Amazon Simple Storage Service") && l.EstimatedTeam {
			t.Fatalf("drilled pair and the account's last service are exact: %+v", l)
		}
	}
	inc = natIncrease()
	if inc.Owner.Name != "checkout" || inc.Owner.Estimated || strings.Contains(inc.Sentence, "(estimated)") {
		t.Fatalf("drilled increase owner %+v %q", inc.Owner, inc.Sentence)
	}
}
