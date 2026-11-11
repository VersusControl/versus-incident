package cost

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Resource-level data is optional. When a service has an open increase the
// refresh spends one request on GetCostAndUsageWithResources, through the
// meter, and the source returns the resources under their usage type with
// the cost AWS reported, to the micro-dollar.
func TestResourceDetailRefinesUsageTypesWhenOn(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.ResourceServices = func() []string { return []string{"EC2 - Other"} }
	})
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 1 {
		t.Fatalf("committed %v", res.Committed)
	}
	if h.fake.count(callResources) != 1 {
		t.Fatalf("resource requests %d, want 1", h.fake.count(callResources))
	}
	if used := h.adapter.Meter().Used(ctx); used != 7 {
		t.Fatalf("meter used %d, want 7 (six calls and resource detail)", used)
	}
	src := h.adapter.Source()
	st, err := src.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ResourceData {
		t.Fatal("resource data is on after a successful read")
	}

	today := finops.MustDate("2026-11-09")
	window := finops.DateRange{Start: today.AddDays(-resourceDays), End: today}
	got, err := src.ResourceLines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no resource lines")
	}
	want := map[[2]string]int64{}
	for _, r := range sc.rows() {
		if r.resource != "" && r.service == "EC2 - Other" && window.Contains(r.day) {
			want[[2]string{r.usageType, r.resource}] += r.cost
		}
	}
	have := map[[2]string]int64{}
	for _, l := range got {
		if l.ResourceID == "" || l.Account != "111111111111" || l.RecordType != finops.RecordUsage {
			t.Fatalf("resource line %+v", l)
		}
		if !window.Contains(l.Day) {
			t.Fatalf("resource line outside the %d-day window: %s", resourceDays, l.Day)
		}
		if l.UsageType == "USE1-NatGateway-Bytes" && (l.Service != "network.nat" || l.Region != "us-east-1") {
			t.Fatalf("resource line mapped to %s in %s", l.Service, l.Region)
		}
		have[[2]string{l.UsageType, l.ResourceID}] += l.NetAmortizedMicros
	}
	if len(have) != len(want) {
		t.Fatalf("resources %v, want %v", have, want)
	}
	for k, w := range want {
		if have[k] != w {
			t.Fatalf("%v cost %d, want %d", k, have[k], w)
		}
	}
	// Only the services asked for are read: the compute instance has a
	// resource ID too but no open increase.
	for _, l := range got {
		if l.ResourceID == "i-0abc" {
			t.Fatal("a service without an open increase was read at resource level")
		}
	}
	// Resource lines refine; they never change the totals.
	lines, _ := src.Lines(ctx, sc.Range)
	natTotal := sumLines(lines, func(l finops.CostLine) bool {
		return l.UsageType == "USE1-NatGateway-Bytes" && window.Contains(l.Day)
	})
	if natTotal != want[[2]string{"USE1-NatGateway-Bytes", "nat-1a"}] {
		t.Fatalf("usage total %d, resource total %d", natTotal, want[[2]string{"USE1-NatGateway-Bytes", "nat-1a"}])
	}
}

// A refused resource read is optional: the day still commits and the
// source reports resource data as off instead of showing nothing.
func TestResourceDetailDeniedKeepsTheDayAndTurnsOff(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.ResourceServices = func() []string { return []string{"EC2 - Other"} }
	})
	h.fake.failures[callResources] = fakeFailure{Type: "DataUnavailableException", Message: "resource-level data is not enabled"}
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 1 {
		t.Fatalf("an optional call must not hold the day back: %v", res.Committed)
	}
	src := h.adapter.Source()
	st, _ := src.Status(ctx)
	if st.ResourceData || st.Stale {
		t.Fatalf("resource data off, data not stale: %+v", st)
	}
	rl, err := src.ResourceLines(ctx, sc.Range)
	if err != nil || len(rl) != 0 {
		t.Fatalf("no resource lines when off: %d %v", len(rl), err)
	}
	if used := h.adapter.Meter().Used(ctx); used != 7 {
		t.Fatalf("the refused request was still paid and counted: %d", used)
	}
}

// Without services to read, no resource request is planned or sent.
func TestResourceDetailNotPlannedWithoutOpenIncreases(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.ResourceServices = func() []string { return nil }
	})
	plan, err := h.adapter.Estimate(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.Run {
		if c.Name == callResources {
			t.Fatal("resource detail planned with no open increase")
		}
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if h.fake.count(callResources) != 0 {
		t.Fatal("resource detail sent with no open increase")
	}
}

func TestStoreReportsAndSavingStates(t *testing.T) {
	h := newHarness(t, loadScenario(t, "single-account"), nil)
	s := NewStore(h.provider, h.adapter.cache.scope)
	oct := finops.ReportView{Header: finops.Header{Enabled: true, Period: &finops.Period{
		Calendar: finops.DateRange{Start: finops.MustDate("2026-10-01"), End: finops.MustDate("2026-10-31")},
	}}, Frozen: &finops.Frozen{At: at("2026-11-06", 9), By: "admin"}}
	oct.Summary.Total = 43_520_000_000
	if err := s.PutReport(ctx, oct); err != nil {
		t.Fatal(err)
	}
	if err := s.PutReport(ctx, finops.ReportView{}); err == nil {
		t.Fatal("a report without a period has no key")
	}
	refs, err := s.ListReports(ctx)
	if err != nil || len(refs) != 1 || refs[0].Month != "2026-10" || refs[0].Total != 43_520_000_000 || refs[0].Frozen.By != "admin" {
		t.Fatalf("reports %+v %v", refs, err)
	}
	got, err := s.GetReport(ctx, "2026-10")
	if err != nil || got == nil || got.Summary.Total != oct.Summary.Total {
		t.Fatalf("report %+v %v", got, err)
	}
	if _, err := s.GetReport(ctx, "../2026-10"); err == nil {
		t.Fatal("unsafe month refused")
	}

	if err := s.PutSavingState(ctx, "sv-1", finops.SavingState{Dismissed: true, Reason: "kept for DR", By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSavingState(ctx, "a/b", finops.SavingState{}); err == nil {
		t.Fatal("unsafe saving id refused")
	}
	states, err := s.SavingStates(ctx)
	if err != nil || len(states) != 1 || !states["sv-1"].Dismissed || states["sv-1"].Reason != "kept for DR" {
		t.Fatalf("saving states %+v %v", states, err)
	}
}

// The bridge lands on the bill on every cost scenario: base plus every
// driver row plus everything else equals the total, and both ends equal
// what AWS reported for those days, to the micro-dollar. The Organization
// scenario goes through estimated team and region splits on the way.
func TestBridgeSumsToTheBillOnEveryScenario(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (*harness, *scenario)
	}{
		{"single-account", func(t *testing.T) (*harness, *scenario) {
			sc := loadScenario(t, "single-account")
			h := newHarness(t, sc, nil)
			if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
				t.Fatal(err)
			}
			return h, sc
		}},
		{"organization", func(t *testing.T) (*harness, *scenario) {
			h, sc := orgHarness(t)
			if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
				t.Fatal(err)
			}
			backfillOrganization(t, h)
			return h, sc
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, sc := c.setup(t)
			svc := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now})
			for _, depth := range []string{"summary", "details"} {
				v, err := svc.Drivers(ctx, finops.Query{Depth: depth})
				if err != nil {
					t.Fatal(err)
				}
				if !v.Available || v.Period == nil || v.Compare == nil {
					t.Fatalf("bridge not available: %+v", v.Header)
				}
				sum := v.Base
				for _, r := range v.Rows {
					sum += r.Amount
				}
				if v.Other != nil {
					sum += v.Other.Amount
				}
				if sum != v.Total || v.Base+v.Change != v.Total || v.Up+v.Down != v.Change {
					t.Fatalf("%s: base %d + rows = %d, total %d, change %d, up %d down %d", depth, v.Base, sum, v.Total, v.Change, v.Up, v.Down)
				}
				rows := sc.rows()
				if want := sumRows(rows, func(r row) bool { return v.Period.Range.Contains(r.day) }); v.Total != want {
					t.Fatalf("%s: total %d, bill %d for %v", depth, v.Total, want, v.Period.Range)
				}
				if want := sumRows(rows, func(r row) bool { return v.Compare.Range.Contains(r.day) }); v.Base != want {
					t.Fatalf("%s: base %d, bill %d for %v", depth, v.Base, want, v.Compare.Range)
				}
			}
		})
	}
}

// TestTagAllowlistAppliesOnReadWithoutARefetch: the cost tables keep every
// team value; turning the tag allowlist off hides them on the next read
// with the same totals, and turning it back on shows them again, with no
// paid request.
func TestTagAllowlistAppliesOnReadWithoutARefetch(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	used := h.adapter.Meter().Used(ctx)
	src := h.adapter.Source()
	tagged, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	teams := 0
	for _, l := range tagged {
		if l.Team != "" {
			teams++
		}
	}
	if teams == 0 {
		t.Fatal("allowlisted team values are read")
	}
	off := h.opts
	off.TagAllowlisted = false
	if err := h.adapter.Update(off); err != nil {
		t.Fatal(err)
	}
	hidden, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range hidden {
		if l.Team != "" || l.EstimatedTeam {
			t.Fatalf("a team value outside the allowlist reached a line: %+v", l)
		}
	}
	if sumLines(hidden, nil) != sumLines(tagged, nil) || len(hidden) >= len(tagged) {
		t.Fatalf("hidden teams change totals: %d lines %d, want %d", len(hidden), sumLines(hidden, nil), sumLines(tagged, nil))
	}
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	again, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(tagged) || h.adapter.Meter().Used(ctx) != used {
		t.Fatalf("allowlist back on: %d lines, want %d; meter %d, want %d", len(again), len(tagged), h.adapter.Meter().Used(ctx), used)
	}
}

// TestRemovedCostSourceLeavesNoLines: a payer that is no longer a cost
// source has its cost lines and refresh state deleted at the next refresh,
// and removing the payer deletes them at once.
func TestRemovedCostSourceLeavesNoLines(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = append(o.Payers, Payer{AccountID: "222222222222", Credentials: testCreds})
	})
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, _ := storage.FinOps(h.provider)
	count := func(payer string) int {
		rows, err := tables.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{Payer: payer})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if count("111111111111") == 0 || count("222222222222") == 0 {
		t.Fatal("both payers have lines")
	}
	one := h.opts
	one.Payers = one.Payers[:1]
	if err := h.adapter.Update(one); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if n := count("222222222222"); n != 0 {
		t.Fatalf("a removed cost source keeps %d lines", n)
	}
	if _, ok := mustStatus(t, h).Payers["222222222222"]; ok {
		t.Fatal("a removed cost source keeps its refresh state")
	}
	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatal(err)
	}
	if n := count("111111111111"); n != 0 {
		t.Fatalf("the payer removal leaves %d lines", n)
	}
	if lines, err := h.adapter.Source().Lines(ctx, sc.Range); err != nil || len(lines) != 0 {
		t.Fatalf("reads after the delete: %d lines, %v", len(lines), err)
	}
}

// TestRefreshKeepsThirteenMonthsOfLines: closing a month deletes cost lines
// older than the retention.
func TestRefreshKeepsThirteenMonthsOfLines(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	tables, _ := storage.FinOps(h.provider)
	old := storage.FinOpsCostLine{Day: "2025-09-30", Account: "111111111111", Region: "us-east-1", Service: "compute.vm",
		ServiceName: "Amazon Elastic Compute Cloud - Compute", RecordType: finops.RecordUsage, NetAmortizedMicros: 1}
	kept := old
	kept.Day = "2025-10-01"
	if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, "111111111111", []string{old.Day, kept.Day}, []storage.FinOpsCostLine{old, kept}); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-05", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	rows, err := tables.FinOpsCostLines(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{Through: "2025-10-31"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Day != "2025-10-01" {
		t.Fatalf("after the month close: %+v, want only 2025-10-01", rows)
	}
}

// TestTwoAWSServicesUnderOneKeyStayApart: RDS and Aurora both map to
// database.relational; their lines are stored as two rows, each with its
// AWS service name.
func TestTwoAWSServicesUnderOneKeyStayApart(t *testing.T) {
	day := finops.MustDate("2026-10-01")
	line := func(svc string, cost finops.Micros) finops.CostLine {
		return finops.CostLine{Day: day, Account: "111111111111", Region: "us-east-1", Service: "database.relational",
			ServiceName: svc, RecordType: finops.RecordUsage, NetAmortizedMicros: cost}
	}
	rows := rowsOf("111111111111", []finops.CostLine{line("Amazon Relational Database Service", 5), line("Amazon Aurora", 7)}, at("2026-10-02", 1))
	if len(rows) != 2 || rows[0].ServiceName == rows[1].ServiceName || rows[0].NetAmortizedMicros+rows[1].NetAmortizedMicros != 12 {
		t.Fatalf("rows %+v", rows)
	}
}
