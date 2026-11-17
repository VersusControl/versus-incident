package cost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// limitRecorder passes cost-line reads on and remembers the Limit each was
// given, so a test can prove a bounded read asked for the budget and no more.
type limitRecorder struct {
	storage.Provider
	storage.FinOpsStore
	mu     sync.Mutex
	limits []int
}

func (r *limitRecorder) FinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, query storage.FinOpsCostQuery) ([]storage.FinOpsCostLine, error) {
	r.mu.Lock()
	r.limits = append(r.limits, query.Limit)
	r.mu.Unlock()
	return r.FinOpsStore.FinOpsCostLines(ctx, scope, query)
}

func (r *limitRecorder) readLimits() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.limits...)
}

func mustSource(t *testing.T, provider storage.Provider, scope tenancy.OrgScope, opts Options) *Source {
	t.Helper()
	a, err := New(provider, scope, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a.Source()
}

// A period holding more stored lines than the budget is refused, not
// summarised from a page: the read asks for exactly the budget and a read
// that comes back full refuses. The same period under a fitting budget is
// unchanged, and resource-level lines cannot take the unbounded path.
func TestLinesRefusePeriodOverBudget(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	rec := &limitRecorder{Provider: h.provider, FinOpsStore: tables}
	scope := h.adapter.cache.scope

	opts := h.opts
	opts.LineBudget = 7
	src := mustSource(t, rec, scope, opts)
	if _, err := src.Lines(ctx, sc.Range); !errors.Is(err, ErrCostLinesBudgetExceeded) {
		t.Fatalf("over-budget period: %v", err)
	}
	if limits := rec.readLimits(); len(limits) != 1 || limits[0] != 7 {
		t.Fatalf("the read asked for %v rows, want exactly the 7-row budget", limits)
	}
	if _, err := src.ResourceLines(ctx, sc.Range); !errors.Is(err, ErrCostLinesBudgetExceeded) {
		t.Fatalf("resource lines bypassed the budget: %v", err)
	}

	// Totals answer the same over-budget period: they return groups, not
	// lines, so the line budget does not apply to them.
	totals, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{})
	if err != nil {
		t.Fatalf("totals over a period over the line budget: %v", err)
	}
	if len(totals) != 1 || totals[0].NetAmortizedMicros != sumRows(sc.rows(), nil) {
		t.Fatalf("totals %+v, the bill is %d", totals, sumRows(sc.rows(), nil))
	}

	opts.LineBudget = 1_000_000
	src = mustSource(t, rec, scope, opts)
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sumLines(lines, nil), sumRows(sc.rows(), nil); got != want {
		t.Fatalf("under a fitting budget %d, want %d", got, want)
	}
	if limits := rec.readLimits(); limits[len(limits)-1] != 1_000_000 {
		t.Fatalf("the fitting read asked for %d rows", limits[len(limits)-1])
	}
}

// The resource lines behind one bridge driver or one increase are read for
// that account and service only, so the line budget counts that pair's
// stored lines: a period whose lines are over the budget still names the
// pair's resources, and the pair's own read is refused exactly at the
// budget.
func TestResourceLinesOfCountsOnlyThePairAgainstTheBudget(t *testing.T) {
	h := newHarness(t, loadScenario(t, "single-account"), nil)
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	const payer, day = "111111111111", "2026-10-05"
	row := func(account, service, usage, resource string, amount int64) storage.FinOpsCostLine {
		return storage.FinOpsCostLine{Payer: payer, Day: day, Account: account, Region: "us-east-1", Service: service, ServiceName: "Amazon " + service,
			UsageType: usage, RecordType: finops.RecordUsage, ResourceID: resource, Currency: "USD", NetAmortizedMicros: amount}
	}
	rows := []storage.FinOpsCostLine{
		row("222222222222", "compute.vm", "BoxUsage", "", 20),
		row("222222222222", "compute.vm", "EBS:Volume", "", 4),
		row("222222222222", "compute.vm", "BoxUsage", "i-a", 5),
		row("222222222222", "compute.vm", "BoxUsage", "i-b", 9),
		row("222222222222", "compute.vm", "EBS:Volume", "vol-a", 4),
	}
	for i := 0; i < 10; i++ {
		rows = append(rows, row("333333333333", "storage.object", fmt.Sprintf("Requests-%d", i), "", 1))
	}
	if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, payer, []string{day}, rows); err != nil {
		t.Fatal(err)
	}
	r := finops.DateRange{Start: finops.MustDate(day), End: finops.MustDate(day)}

	opts := h.opts
	opts.LineBudget = 6
	src := mustSource(t, h.provider, h.adapter.cache.scope, opts)
	if _, err := src.ResourceLines(ctx, r); !errors.Is(err, ErrCostLinesBudgetExceeded) {
		t.Fatalf("the period's 15 lines under a 6-line budget: %v", err)
	}
	lines, err := src.ResourceLinesOf(ctx, r, "222222222222", "compute.vm")
	if err != nil {
		t.Fatalf("the pair's 5 lines under a 6-line budget: %v", err)
	}
	got := map[string]int64{}
	for _, l := range lines {
		got[l.ResourceID] += l.NetAmortizedMicros
	}
	if len(lines) != 3 || got["i-a"] != 5 || got["i-b"] != 9 || got["vol-a"] != 4 {
		t.Fatalf("resource lines of the pair: %+v", lines)
	}

	opts.LineBudget = 5
	src = mustSource(t, h.provider, h.adapter.cache.scope, opts)
	if _, err := src.ResourceLinesOf(ctx, r, "222222222222", "compute.vm"); !errors.Is(err, ErrCostLinesBudgetExceeded) {
		t.Fatalf("the pair's 5 lines at a 5-line budget: %v", err)
	}
}

// Totals must answer the same sum as the lines they replace, for every
// grouping the views read, so a view can switch over without changing a
// number. The Organization case carries folded drill detail; the hidden
// team case carries redacted rows.
func TestTotalsEqualTheLinesSum(t *testing.T) {
	combos := [][]Dimension{
		{ByDay},
		{ByAccount},
		{ByService},
		{ByTeam},
		{ByUsageGroup},
		{ByUsageGroup, ByUnit},
		{ByDay, ByAccount, ByService, ByTeam, ByUsageGroup},
	}
	t.Run("single account", func(t *testing.T) {
		sc := loadScenario(t, "single-account")
		h := newHarness(t, sc, nil)
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		checkTotals(t, h.adapter.Source(), sc.Range, combos)
	})
	t.Run("organization", func(t *testing.T) {
		h, sc := orgHarness(t)
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		backfillOrganization(t, h)
		checkTotals(t, h.adapter.Source(), sc.Range, combos)
	})
	t.Run("organization without the team allowlist", func(t *testing.T) {
		sc := loadScenario(t, "organization")
		h := newHarness(t, sc, func(o *Options) {
			o.Payers = []Payer{{AccountID: "999999999999", Organization: true, Credentials: testCreds}}
			o.Connected = []string{"111111111111", "222222222222"}
			o.MaxDrillDowns = 3
			o.MaxPaidRequests = 40
			o.TagAllowlisted = false
		})
		if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
			t.Fatal(err)
		}
		backfillOrganization(t, h)
		src := h.adapter.Source()
		checkTotals(t, src, sc.Range, combos)
		totals, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{}, ByTeam)
		if err != nil {
			t.Fatal(err)
		}
		if len(totals) != 1 || totals[0].Team != "" {
			t.Fatalf("a hidden tag leaked a team: %+v", totals)
		}
	})
}

// checkTotals compares every grouping against a sum of the lines.
func checkTotals(t *testing.T, src *Source, r finops.DateRange, combos [][]Dimension) {
	t.Helper()
	lines, err := src.Lines(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, dims := range combos {
		totals, err := src.TotalsGroups(ctx, r, TotalsQuery{}, dims...)
		if err != nil {
			t.Fatalf("%v: %v", dims, err)
		}
		want := map[string]groupSum{}
		for _, l := range lines {
			key := lineGroupKey(dims, l)
			s := want[key]
			s.net += l.NetAmortizedMicros
			s.qty += l.QuantityMicros
			if s.minDay.IsZero() || l.Day.Before(s.minDay) {
				s.minDay = l.Day
			}
			want[key] = s
		}
		if len(totals) != len(want) {
			t.Fatalf("%v: %d totals, the lines hold %d groups", dims, len(totals), len(want))
		}
		for _, total := range totals {
			key := totalGroupKey(dims, total)
			expected, ok := want[key]
			if !ok {
				t.Fatalf("%v: total group %q is not a group of the lines", dims, key)
			}
			if total.NetAmortizedMicros != expected.net {
				t.Fatalf("%v %q: net %d, lines sum %d", dims, key, total.NetAmortizedMicros, expected.net)
			}
			if total.QuantityMicros != expected.qty {
				t.Fatalf("%v %q: quantity %d, lines sum %d", dims, key, total.QuantityMicros, expected.qty)
			}
			if total.MinDay != expected.minDay {
				t.Fatalf("%v %q: first day %s, lines %s", dims, key, total.MinDay, expected.minDay)
			}
		}
	}
}

type groupSum struct {
	net, qty int64
	minDay   finops.Date
}

func lineGroupKey(dims []Dimension, l finops.CostLine) string {
	values := make([]string, len(dims))
	for i, d := range dims {
		switch d {
		case ByDay:
			values[i] = l.Day.String()
		case ByMonth:
			values[i] = l.Day.MonthKey()
		case ByAccount:
			values[i] = l.Account
		case ByRegion:
			values[i] = l.Region
		case ByService:
			values[i] = l.Service
		case ByServiceName:
			values[i] = l.ServiceName
		case ByUsageType:
			values[i] = l.UsageType
		case ByRecordType:
			values[i] = l.RecordType
		case ByTeam:
			values[i] = l.Team
		case ByUsageGroup:
			values[i] = l.UsageGroup
		case ByUnit:
			values[i] = l.Unit
		case ByResourceID:
			values[i] = l.ResourceID
		}
	}
	return joinKey(values)
}

func totalGroupKey(dims []Dimension, t Total) string {
	values := make([]string, len(dims))
	for i, d := range dims {
		switch d {
		case ByDay:
			values[i] = t.Day.String()
		case ByMonth:
			values[i] = t.Month
		case ByAccount:
			values[i] = t.Account
		case ByRegion:
			values[i] = t.Region
		case ByService:
			values[i] = t.Service
		case ByServiceName:
			values[i] = t.ServiceName
		case ByUsageType:
			values[i] = t.UsageType
		case ByRecordType:
			values[i] = t.RecordType
		case ByTeam:
			values[i] = t.Team
		case ByUsageGroup:
			values[i] = t.UsageGroup
		case ByUnit:
			values[i] = t.Unit
		case ByResourceID:
			values[i] = t.ResourceID
		}
	}
	return joinKey(values)
}

func joinKey(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += "\x00"
		}
		out += v
	}
	return out
}

// The account, region and team filters narrow a totals read exactly as they
// narrow the lines a view sums.
func TestTotalsHonourScopeFilters(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		q    TotalsQuery
		keep func(finops.CostLine) bool
	}{
		{"account", TotalsQuery{Accounts: []string{"111111111111"}}, func(l finops.CostLine) bool { return l.Account == "111111111111" }},
		{"region", TotalsQuery{Regions: []string{"us-east-1"}}, func(l finops.CostLine) bool { return l.Region == "us-east-1" }},
		{"team", TotalsQuery{Teams: []string{"checkout"}}, func(l finops.CostLine) bool { return l.Team == "checkout" }},
	}
	for _, c := range cases {
		totals, err := src.TotalsGroups(ctx, sc.Range, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := sumLines(lines, c.keep)
		if want == 0 {
			t.Fatalf("%s: the filter selects nothing, the case proves nothing", c.name)
		}
		if len(totals) != 1 || totals[0].NetAmortizedMicros != want {
			t.Fatalf("%s: %+v, the filtered lines sum to %d", c.name, totals, want)
		}
	}
}

// Rows of a payer the deployment does not read are not part of a total,
// exactly as they are not part of the lines.
func TestTotalsIgnoreRowsOfUnconfiguredPayers(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	stray := []storage.FinOpsCostLine{{
		Day: "2026-10-01", Account: "888888888888", Service: "compute",
		RecordType: finops.RecordUsage, NetAmortizedMicros: 7_000_000, Currency: "USD",
	}}
	if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, "888888888888", []string{"2026-10-01"}, stray); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sumLines(lines, nil), sumRows(sc.rows(), nil); got != want {
		t.Fatalf("lines %d, want %d", got, want)
	}
	if len(totals) != 1 || totals[0].NetAmortizedMicros != sumLines(lines, nil) {
		t.Fatalf("totals %+v, lines %d", totals, sumLines(lines, nil))
	}
}

// An amount that does not fit a signed total is reported, never wrapped:
// the store's own overflow and the merge across payers both surface
// storage.ErrAmountOverflow unchanged.
func TestTotalsPropagateAmountOverflow(t *testing.T) {
	line := func(payer, account, name string, amount int64) storage.FinOpsCostLine {
		return storage.FinOpsCostLine{
			Payer: payer, Day: "2026-10-01", Account: account, Service: "compute", ServiceName: name,
			RecordType: finops.RecordUsage, NetAmortizedMicros: amount, Currency: "USD",
		}
	}
	t.Run("store sum", func(t *testing.T) {
		h := newHarness(t, loadScenario(t, "single-account"), nil)
		tables, ok := storage.FinOps(h.provider)
		if !ok {
			t.Fatal("no FinOps tables")
		}
		rows := []storage.FinOpsCostLine{
			line("111111111111", "222222222222", "a", math.MaxInt64),
			line("111111111111", "222222222222", "b", 1),
		}
		if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, "111111111111", []string{"2026-10-01"}, rows); err != nil {
			t.Fatal(err)
		}
		src := mustSource(t, h.provider, h.adapter.cache.scope, h.opts)
		r := finops.DateRange{Start: finops.MustDate("2026-10-01"), End: finops.MustDate("2026-10-01")}
		if _, err := src.TotalsGroups(ctx, r, TotalsQuery{}, ByService); !errors.Is(err, storage.ErrAmountOverflow) {
			t.Fatalf("store overflow: %v", err)
		}
	})
	t.Run("merge across payers", func(t *testing.T) {
		h := newHarness(t, loadScenario(t, "single-account"), func(o *Options) {
			o.Payers = []Payer{
				{AccountID: "111111111111", Credentials: credsFor("AKIDONE")},
				{AccountID: "222222222222", Credentials: credsFor("AKIDTWO")},
			}
		})
		tables, ok := storage.FinOps(h.provider)
		if !ok {
			t.Fatal("no FinOps tables")
		}
		half := int64(math.MaxInt64/2 + 1)
		for _, payer := range []string{"111111111111", "222222222222"} {
			rows := []storage.FinOpsCostLine{line(payer, payer, "a", half)}
			if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, payer, []string{"2026-10-01"}, rows); err != nil {
				t.Fatal(err)
			}
		}
		src := mustSource(t, h.provider, h.adapter.cache.scope, h.opts)
		r := finops.DateRange{Start: finops.MustDate("2026-10-01"), End: finops.MustDate("2026-10-01")}
		if _, err := src.TotalsGroups(ctx, r, TotalsQuery{}); !errors.Is(err, storage.ErrAmountOverflow) {
			t.Fatalf("merge overflow: %v", err)
		}
	})
}

// The core reads totals through its own vocabulary: the shim must answer the
// same groups as the adapter's own method, from the same store sums.
func TestTotalsAdaptsTheSourceVocabulary(t *testing.T) {
	combos := [][]Dimension{
		{ByDay},
		{ByAccount},
		{ByService},
		{ByTeam},
		{ByUsageGroup},
		{ByDay, ByAccount, ByService, ByTeam, ByUsageGroup},
	}
	sources := []struct {
		name string
		load func(t *testing.T) (*Source, finops.DateRange)
	}{
		{"single account", func(t *testing.T) (*Source, finops.DateRange) {
			sc := loadScenario(t, "single-account")
			h := newHarness(t, sc, nil)
			if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
				t.Fatal(err)
			}
			return h.adapter.Source(), sc.Range
		}},
		{"organization", func(t *testing.T) (*Source, finops.DateRange) {
			h, sc := orgHarness(t)
			if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
				t.Fatal(err)
			}
			backfillOrganization(t, h)
			return h.adapter.Source(), sc.Range
		}},
	}
	for _, s := range sources {
		t.Run(s.name, func(t *testing.T) {
			src, r := s.load(t)
			for _, dims := range combos {
				want, err := src.TotalsGroups(ctx, r, TotalsQuery{}, dims...)
				if err != nil {
					t.Fatalf("%v: %v", dims, err)
				}
				coreDims := make([]finops.Dimension, len(dims))
				for i, d := range dims {
					coreDims[i] = finops.Dimension(d)
				}
				got, err := src.Totals(ctx, r, finops.TotalsQuery{}, coreDims...)
				if err != nil {
					t.Fatalf("%v: %v", dims, err)
				}
				if len(got) != len(want) {
					t.Fatalf("%v: %d core groups, %d adapter groups", dims, len(got), len(want))
				}
				for i := range want {
					if got[i] != finopsTotal(want[i]) {
						t.Fatalf("%v group %d: %+v, want %+v", dims, i, got[i], finopsTotal(want[i]))
					}
				}
			}
			// The scope filters cross the boundary unchanged.
			filtered, err := src.Totals(ctx, r, finops.TotalsQuery{Teams: []string{"checkout"}})
			if err != nil {
				t.Fatal(err)
			}
			want, err := src.TotalsGroups(ctx, r, TotalsQuery{Teams: []string{"checkout"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(filtered) != len(want) || len(filtered) == 0 {
				t.Fatalf("filtered groups %d, want %d", len(filtered), len(want))
			}
			for i := range want {
				if filtered[i] != finopsTotal(want[i]) {
					t.Fatalf("filtered group %d: %+v, want %+v", i, filtered[i], finopsTotal(want[i]))
				}
			}
			if _, err := src.Totals(ctx, r, finops.TotalsQuery{}, finops.Dimension("owner")); !errors.Is(err, ErrInvalidDimension) {
				t.Fatalf("unknown dimension: %v", err)
			}
		})
	}
}

// An over-budget read keeps the adapter's sentinel and gains the core's
// class, so a caller matches either one and the message still says what to
// do.
func TestTotalsErrorClassesTheBudgetRefusal(t *testing.T) {
	err := totalsError(ErrCostLinesBudgetExceeded)
	if !errors.Is(err, finops.ErrPeriodTooLarge) {
		t.Fatalf("%v does not satisfy the core's class", err)
	}
	if !errors.Is(err, ErrCostLinesBudgetExceeded) {
		t.Fatalf("%v lost the adapter's sentinel", err)
	}
	if !strings.Contains(err.Error(), "narrow the period") {
		t.Fatalf("message %q does not say what to do", err)
	}
	if other := totalsError(storage.ErrAmountOverflow); other != storage.ErrAmountOverflow {
		t.Fatalf("a non-budget error was rewritten: %v", other)
	}
}

// Resource-level rows refine a usage type; they are never added to a total.
func TestTotalsLeaveResourceRowsOut(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.ResourceServices = func() []string { return []string{"EC2 - Other"} }
	})
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	resource, err := src.ResourceLines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	if len(resource) == 0 {
		t.Fatal("the refresh stored no resource rows, the case proves nothing")
	}
	lines, err := src.Lines(ctx, sc.Range)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].NetAmortizedMicros != sumLines(lines, nil) {
		t.Fatalf("totals %+v, lines %d", totals, sumLines(lines, nil))
	}
}
