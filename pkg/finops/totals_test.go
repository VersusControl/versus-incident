package finops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// totalsFakeSource is the reference story's cache with the store-side
// summing half: Totals folds the same lines a line read would return, so a
// difference between the two services is a difference in the views, never in
// the data. It counts both reads.
type totalsFakeSource struct {
	*fakeSource
	lineReads     int
	resourceReads int
	totalReads    int
	// savingsLineScans counts the savings path's line-shaped read; a source
	// with totals must answer from them instead.
	savingsLineScans int
	// savingsReads counts the savings seam; savingsDayReads counts only the
	// payer × day aggregation a source runs when the caller could not supply
	// the billed-day count.
	savingsReads    int
	savingsDayReads int
	savingsDays     int
	savingsDaysOK   bool
}

func (f *totalsFakeSource) Lines(ctx context.Context, r DateRange) ([]CostLine, error) {
	f.lineReads++
	return f.fakeSource.Lines(ctx, r)
}

func (f *totalsFakeSource) ResourceLines(ctx context.Context, r DateRange) ([]CostLine, error) {
	f.resourceReads++
	return f.fakeSource.ResourceLines(ctx, r)
}

func (f *totalsFakeSource) SavingsInputs(ctx context.Context) (SavingsInputs, error) {
	f.savingsLineScans++
	return f.fakeSource.SavingsInputs(ctx)
}

func (f *totalsFakeSource) SavingsInputsFromFacts(ctx context.Context, q SavingsFactsQuery) (SavingsInputs, error) {
	f.savingsReads++
	if !q.DaysKnown {
		f.savingsDayReads++
	}
	f.savingsDays, f.savingsDaysOK = q.BilledDays, q.DaysKnown
	return f.fakeSource.SavingsInputs(ctx)
}

func (f *totalsFakeSource) Totals(_ context.Context, r DateRange, q TotalsQuery, groupBy ...Dimension) ([]Total, error) {
	f.totalReads++
	groups := map[string]*Total{}
	for _, l := range f.lines {
		if !r.Contains(l.Day) || !totalsMatch(l, q) {
			continue
		}
		key := make([]string, len(groupBy))
		for i, d := range groupBy {
			key[i] = totalDimValue(l, d)
		}
		k := strings.Join(key, "\x00")
		t := groups[k]
		if t == nil {
			t = &Total{MinDay: l.Day}
			groups[k] = t
		}
		t.NetAmortizedMicros += int64(l.NetAmortizedMicros)
		t.QuantityMicros += l.QuantityMicros
		t.Lines++
		t.EstimatedTeam = t.EstimatedTeam || l.EstimatedTeam
		t.EstimatedRegion = t.EstimatedRegion || l.EstimatedRegion
		if l.Day.Before(t.MinDay) {
			t.MinDay = l.Day
		}
		setTotalDims(t, l, groupBy)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Total, 0, len(keys))
	for _, k := range keys {
		out = append(out, *groups[k])
	}
	return out, nil
}

func totalsMatch(l CostLine, q TotalsQuery) bool {
	return anyOf(q.Accounts, l.Account) && anyOf(q.Regions, l.Region) && anyOf(q.Teams, l.Team) &&
		anyOf(q.Services, l.Service) && anyOf(q.UsageTypes, l.UsageType)
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func totalDimValue(l CostLine, d Dimension) string {
	switch d {
	case ByDay:
		return l.Day.String()
	case ByAccount:
		return l.Account
	case ByRegion:
		return l.Region
	case ByService:
		return l.Service
	case ByServiceName:
		return l.ServiceName
	case ByUsageType:
		return l.UsageType
	case ByRecordType:
		return l.recordType()
	case ByTeam:
		return l.Team
	case ByUsageGroup:
		return l.UsageGroup
	case ByUnit:
		return l.Unit
	case ByResourceID:
		return l.ResourceID
	}
	return ""
}

func setTotalDims(t *Total, l CostLine, groupBy []Dimension) {
	for _, d := range groupBy {
		switch d {
		case ByDay:
			t.Day = l.Day
		case ByAccount:
			t.Account = l.Account
		case ByRegion:
			t.Region = l.Region
		case ByService:
			t.Service = l.Service
		case ByServiceName:
			t.ServiceName = l.ServiceName
		case ByUsageType:
			t.UsageType = l.UsageType
		case ByRecordType:
			t.RecordType = l.recordType()
		case ByTeam:
			t.Team = l.Team
		case ByUsageGroup:
			t.UsageGroup = l.UsageGroup
		case ByUnit:
			t.Unit = l.Unit
		case ByResourceID:
			t.ResourceID = l.ResourceID
		}
	}
}

// storyViews renders every read-only view of the reference story.
func storyViews(t *testing.T, svc *Service) map[string]any {
	t.Helper()
	ctx := t.Context()
	must := func(v any, err error) any {
		if err != nil {
			t.Fatalf("view: %v", err)
		}
		return v
	}
	// Detection is a sync-time step; render what it stored.
	must(svc.SyncIncreases(ctx))
	inc := must(svc.Increases(ctx, october)).(IncreasesView)
	return map[string]any{
		"summary":         must(svc.Summary(ctx, october)),
		"bridge":          must(svc.Drivers(ctx, october)),
		"bridge-details":  must(svc.Drivers(ctx, Query{Period: "2026-10", Depth: "details"})),
		"trend":           must(svc.Trend(ctx, october)),
		"breakdown":       must(svc.Breakdown(ctx, october)),
		"breakdown-team":  must(svc.Breakdown(ctx, Query{Period: "2026-10", GroupBy: GroupTeam})),
		"increases":       inc,
		"increase-detail": must(svc.Increase(ctx, october, inc.Items[0].ID)),
		"savings":         must(svc.Savings(ctx, october)),
		"budgets":         must(svc.Budget(ctx, october)),
		"accounts":        must(svc.Accounts(ctx, october)),
		"report":          must(svc.Report(ctx, october)),
		"trend-region":    must(svc.Trend(ctx, Query{Period: "2026-10", GroupBy: GroupRegion})),
		"summary-team":    must(svc.Summary(ctx, Query{Period: "2026-10", Teams: []string{"checkout"}})),
		// The summary reads its comparison period per group and the rest of
		// its window per day: every period shape must still answer exactly
		// what the full day-by-day read answers.
		"summary-mtd":    must(svc.Summary(ctx, Query{})),
		"summary-last30": must(svc.Summary(ctx, Query{Period: PeriodLast30Days})),
		"summary-sep":    must(svc.Summary(ctx, Query{Period: "2026-09"})),
	}
}

// TestTotalsViewsMatchLineViews proves the store-side summing answers every
// view with the same numbers as the line read.
func TestTotalsViewsMatchLineViews(t *testing.T) {
	lineSrc, cfg := natStory()
	lineSvc := NewService(lineSrc, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	totalSrc := &totalsFakeSource{fakeSource: lineSrc}
	totalSvc := NewService(totalSrc, newMemStore(), cfg).WithSuspectFinder(storySuspects())

	lineViews := storyViews(t, lineSvc)
	totalViews := storyViews(t, totalSvc)

	for name, want := range lineViews {
		gotRaw, err := json.Marshal(totalViews[name])
		if err != nil {
			t.Fatal(err)
		}
		wantRaw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotRaw) != string(wantRaw) {
			t.Errorf("%s differs between totals and lines: %s", name, firstDiff(gotRaw, wantRaw))
		}
	}
	if totalSrc.totalReads == 0 {
		t.Fatal("the totals source was never read")
	}
}

// firstDiff names the first place two JSON answers differ, with a little
// context on each side.
func firstDiff(got, want []byte) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	lo := max(0, i-120)
	return fmt.Sprintf("at byte %d\n got: %q\nwant: %q", i, got[lo:min(len(got), i+160)], want[lo:min(len(want), i+160)])
}

// TestViewsReadNoLinesWithTotals proves the page path no longer loads lines
// when the source can sum: the summary, bridge, trend, breakdown and the
// account, budget and increase views all answer from totals. A page load
// (summary, then the increases and savings lists) makes no line read at all:
// detection is stored and the savings facts come from grouped totals.
func TestViewsReadNoLinesWithTotals(t *testing.T) {
	lineSrc, cfg := natStory()
	src := &totalsFakeSource{fakeSource: lineSrc}
	svc := NewService(src, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	storyViews(t, svc)
	if src.lineReads != 0 {
		t.Fatalf("views read %d line sets with a totals source, want 0", src.lineReads)
	}
	if src.totalReads == 0 {
		t.Fatal("views never read totals")
	}

	page := &totalsFakeSource{fakeSource: lineSrc}
	pageSvc := NewService(page, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	ctx := t.Context()
	if _, err := pageSvc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	// A summary reads each part of its window once, at the granularity that
	// part needs: October per day, September per group, the days after
	// October and the forecast's last September days per day and account.
	// A fifth read would mean the summary re-reads days it already summed.
	before := page.totalReads
	if _, err := pageSvc.Summary(ctx, october); err != nil {
		t.Fatal(err)
	}
	if got := page.totalReads - before; got != 4 {
		t.Fatalf("a summary render issued %d totals reads, want 4", got)
	}
	// The billed-day count comes from the page totals, so the savings facts
	// never re-read payer × day: the render is at most six store reads.
	if page.savingsDayReads != 0 {
		t.Fatalf("the savings facts ran %d payer × day reads, want 0", page.savingsDayReads)
	}
	if !page.savingsDaysOK || page.savingsDays != 30 {
		t.Fatalf("billed days = %d (known %v), want 30 counted from the page totals", page.savingsDays, page.savingsDaysOK)
	}
	if got := page.totalReads + page.savingsReads; got > 6 {
		t.Fatalf("a summary render issued %d store reads, want at most 6", got)
	}
	if _, err := pageSvc.Increases(ctx, october); err != nil {
		t.Fatal(err)
	}
	if _, err := pageSvc.Savings(ctx, october); err != nil {
		t.Fatal(err)
	}
	if page.lineReads != 0 || page.resourceReads != 0 {
		t.Fatalf("a summary + increases + savings render read %d line sets and %d resource sets, want 0",
			page.lineReads, page.resourceReads)
	}
	if page.savingsLineScans != 0 || page.savingsReads == 0 {
		t.Fatalf("savings scanned lines %d times and read totals %d times, want 0 and at least 1",
			page.savingsLineScans, page.savingsReads)
	}
}

// TestSourceWithoutTotalsKeepsLinePath proves the fallback: a source without
// the summing half is answered from its lines, unchanged.
func TestSourceWithoutTotalsKeepsLinePath(t *testing.T) {
	src, cfg := natStory()
	svc := NewService(src, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	if _, err := svc.Summary(t.Context(), october); err != nil {
		t.Fatal(err)
	}
	if src.calls == 0 {
		t.Fatal("a source without totals must still be read through Lines")
	}
}

// TestPeriodTooLargeIsClassed proves an over-budget source error becomes the
// core's closed class, so the route and the agent tools answer the same.
func TestPeriodTooLargeIsClassed(t *testing.T) {
	src, cfg := natStory()
	over := &overBudgetSource{fakeSource: src}
	svc := NewService(over, nil, cfg)
	_, err := svc.Summary(t.Context(), october)
	if !errors.Is(err, ErrPeriodTooLarge) {
		t.Fatalf("error = %v, want ErrPeriodTooLarge", err)
	}
}

// overBudgetSource reports the adapter's over-budget line error.
type overBudgetSource struct{ *fakeSource }

func (f *overBudgetSource) Lines(context.Context, DateRange) ([]CostLine, error) {
	return nil, errOverBudget
}

type overBudgetError struct{}

func (overBudgetError) Error() string {
	return "cost: the period holds more cost lines than this deployment can summarise at once; narrow the period"
}

var errOverBudget = overBudgetError{}

// narrowResourceSource answers resource lines per account and service and
// refuses one pair as over its line budget.
type narrowResourceSource struct {
	*fakeSource
	refused [2]string
}

func (n *narrowResourceSource) ResourceLinesOf(ctx context.Context, r DateRange, account, service string) ([]CostLine, error) {
	if account == n.refused[0] && service == n.refused[1] {
		return nil, fmt.Errorf("%w: narrow the period", ErrPeriodTooLarge)
	}
	all, err := n.fakeSource.ResourceLines(ctx, r)
	var out []CostLine
	for _, l := range all {
		if l.Account == account && l.Service == service {
			out = append(out, l)
		}
	}
	return out, err
}

// TestBridgeRowsSayWhichResourcesWereTooLarge: a bridge row whose account
// and service had too many resource lines carries resources "too_large", the
// response says why once, and a row of another pair still names its
// resource and carries no flag.
func TestBridgeRowsSayWhichResourcesWereTooLarge(t *testing.T) {
	src, cfg := natStory()
	src.status.ResourceData = true
	query := Query{Period: "2026-10", Depth: "details"}
	plain, err := NewService(src, newMemStore(), cfg).Drivers(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	var cutRow, namedRow *BridgeRow
	for i := range plain.Rows {
		row := &plain.Rows[i]
		if row.Account == "" || row.Service == "" || row.Cause == CauseNewAccount {
			continue
		}
		switch {
		case cutRow == nil:
			cutRow = row
		case namedRow == nil && (row.Account != cutRow.Account || row.Service != cutRow.Service):
			namedRow = row
		}
	}
	if cutRow == nil || namedRow == nil {
		t.Fatalf("the story needs two driver rows of different account and service: %+v", plain.Rows)
	}
	src.resources = []CostLine{{Day: MustDate("2026-10-12"), Account: namedRow.Account, Service: namedRow.Service, UsageType: namedRow.UsageType,
		RecordType: namedRow.RecordType, ResourceID: "i-named", NetAmortizedMicros: USD(3)}}
	narrow := &narrowResourceSource{fakeSource: src, refused: [2]string{cutRow.Account, cutRow.Service}}
	v, err := NewService(narrow, newMemStore(), cfg).Drivers(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if v.ResourceNote == "" {
		t.Fatal("the bridge cut resources but does not say why")
	}
	for _, row := range v.Rows {
		cut := row.Cause != CauseNewAccount && row.Account == cutRow.Account && row.Service == cutRow.Service
		switch {
		case cut && (row.Resources != ResourcesTooLarge || row.ResourceID != ""):
			t.Errorf("row %s of the refused pair: resources %q resource %q", row.ID, row.Resources, row.ResourceID)
		case !cut && row.Resources != "":
			t.Errorf("row %s of a read pair is marked %q", row.ID, row.Resources)
		case row.ID == namedRow.ID && row.ResourceID != "i-named":
			t.Errorf("row %s of a read pair names %q, want i-named", row.ID, row.ResourceID)
		}
	}
}

// The headline names the base it compares with: the same complete days of
// the base month while the period is open, with the year when the base is
// in another year, and the whole month only when the base is whole.
func TestHeadlineBaseLabelNamesTheComparedDays(t *testing.T) {
	cases := []struct {
		period, compare string
		lastComplete    Date
		want            string
	}{
		{PeriodMonthToDate, ComparePrevPeriod, MustDate("2026-11-09"), "Oct 1–9"},
		{PeriodMonthToDate, CompareLastYear, MustDate("2026-11-09"), "Nov 1–9, 2025"},
		{PeriodMonthToDate, ComparePrevPeriod, MustDate("2026-11-01"), "Oct 1"},
		{"2026-10", ComparePrevPeriod, MustDate("2026-11-09"), "September"},
		{"2026-10", CompareLastYear, MustDate("2026-11-09"), "October 2025"},
		{PeriodLast30Days, ComparePrevPeriod, MustDate("2026-11-09"), "the previous 30 days"},
	}
	for _, c := range cases {
		p, comp, err := ResolvePeriod(c.period, c.compare, c.lastComplete, Date{})
		if err != nil {
			t.Fatal(err)
		}
		if got := baseLabel(p, comp); got != c.want {
			t.Errorf("%s vs %s on %s: base label = %q, want %q", c.period, c.compare, c.lastComplete, got, c.want)
		}
	}
}

// TestDetailsSummaryAnswerNamesUsageTypeOnBothPaths proves the summary's
// answer sentence at the details depth names the top driver's usage type
// whether the source sums in the store or returns lines, and that the two
// paths answer the same sentence.
func TestDetailsSummaryAnswerNamesUsageTypeOnBothPaths(t *testing.T) {
	lineSrc, cfg := natStory()
	lineSvc := NewService(lineSrc, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	totalSrc := &totalsFakeSource{fakeSource: lineSrc}
	totalSvc := NewService(totalSrc, newMemStore(), cfg).WithSuspectFinder(storySuspects())
	ctx := t.Context()
	for _, q := range []Query{
		{Period: "2026-10", Depth: "details"},
		{Period: "2026-10", Depth: "details", Teams: []string{"checkout"}},
	} {
		line, err := lineSvc.Summary(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		before := totalSrc.totalReads
		total, err := totalSvc.Summary(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if totalSrc.totalReads == before || totalSrc.lineReads != 0 {
			t.Fatalf("%+v: the totals service must answer from totals (%d totals, %d line reads)", q, totalSrc.totalReads-before, totalSrc.lineReads)
		}
		if total.Answer != line.Answer {
			t.Fatalf("%+v: answers differ\n totals: %q\n  lines: %q", q, total.Answer, line.Answer)
		}
		var usage string
		for _, l := range lineSrc.lines {
			if l.Service == "network.nat" && l.UsageType != "" {
				usage = l.UsageType
				break
			}
		}
		if usage == "" || !strings.Contains(total.Answer, usage) {
			t.Fatalf("%+v: details answer %q must name the usage type %q", q, total.Answer, usage)
		}
	}
	raw, err := json.Marshal(mustSummary(t, totalSvc, Query{Period: "2026-10", Depth: "details"}))
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(mustSummary(t, lineSvc, Query{Period: "2026-10", Depth: "details"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Fatalf("details summary differs between totals and lines: %s", firstDiff(raw, want))
	}
}

func mustSummary(t *testing.T, svc *Service, q Query) SummaryView {
	t.Helper()
	v, err := svc.Summary(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
