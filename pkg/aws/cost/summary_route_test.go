package cost

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// readCounter counts the store reads a FinOps read makes.
type readCounter struct {
	storage.Provider
	storage.FinOpsStore
	mu         sync.Mutex
	totals     int
	lineRanges []storage.FinOpsCostQuery
}

func (r *readCounter) FinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, q storage.FinOpsCostQuery) ([]storage.FinOpsCostLine, error) {
	r.mu.Lock()
	r.lineRanges = append(r.lineRanges, q)
	r.mu.Unlock()
	return r.FinOpsStore.FinOpsCostLines(ctx, scope, q)
}

func (r *readCounter) FinOpsCostTotals(ctx context.Context, scope tenancy.OrgScope, q storage.FinOpsCostQuery, groupBy ...storage.FinOpsDimension) ([]storage.FinOpsCostTotal, error) {
	r.mu.Lock()
	r.totals++
	r.mu.Unlock()
	return r.FinOpsStore.FinOpsCostTotals(ctx, scope, q, groupBy...)
}

func (r *readCounter) counts() (int, []storage.FinOpsCostQuery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totals, append([]storage.FinOpsCostQuery(nil), r.lineRanges...)
}

func TestSummaryReadsTotalsNotLines(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	rec := &readCounter{Provider: h.provider, FinOpsStore: tables}
	src := mustSource(t, rec, h.adapter.cache.scope, h.opts)
	svc := finops.NewService(src, nil, finops.Config{Now: h.clock.Now})
	if _, err := svc.Summary(ctx, finops.Query{Period: "2026-10"}); err != nil {
		t.Fatal(err)
	}
	totals, ranges := rec.counts()
	if totals == 0 {
		t.Fatal("the summary did not sum in the store")
	}
	// The saved-idle and NAT scan still reads the last 30 days of lines; the
	// summary period itself must never be read as lines.
	for _, q := range ranges {
		from, ferr := finops.ParseDate(q.From)
		through, terr := finops.ParseDate(q.Through)
		if ferr != nil || terr != nil || (finops.DateRange{Start: from, End: through}).Days() != 30 {
			t.Fatalf("the summary read lines for %s..%s; only the 30-day savings window may", q.From, q.Through)
		}
	}
}

// TestScaleSummaryProfile seeds one payer-day of the measured worst-case
// shape and runs the summary, for the profile command; it is skipped unless
// asked for.
func TestScaleSummaryProfile(t *testing.T) {
	if os.Getenv("QA_SCALE_PROFILE") != "1" {
		t.Skip("set QA_SCALE_PROFILE=1 to run the 200k-line summary profile")
	}
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	rows := make([]storage.FinOpsCostLine, 0, 200_000)
	for i := 0; i < 200_000; i++ {
		rows = append(rows, storage.FinOpsCostLine{
			Day: "2026-10-15", Account: "111111111111", Region: "us-east-1", Service: "compute.ec2",
			ServiceName: "Amazon Elastic Compute Cloud - Compute", UsageType: fmt.Sprintf("USE1-BoxUsage:type-%06d", i),
			RecordType: finops.RecordUsage, Team: "checkout", NetAmortizedMicros: 1_000, QuantityMicros: 1, Currency: "USD",
		})
	}
	if err := tables.ReplaceFinOpsCostDays(ctx, h.adapter.cache.scope, "111111111111", []string{"2026-10-15"}, rows); err != nil {
		t.Fatal(err)
	}
	src := h.adapter.Source()
	svc := finops.NewService(src, nil, finops.Config{Now: h.clock.Now})
	start := time.Now()
	v, err := svc.Summary(ctx, finops.Query{Period: "2026-10"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary total=%d in %s", v.Total, time.Since(start))
	// The store's cost is the group count: the page dims alone collapse the
	// 200k usage types into the handful of services the summary renders.
	start = time.Now()
	groups, err := src.TotalsGroups(ctx, finops.DateRange{Start: finops.MustDate("2026-10-01"), End: finops.MustDate("2026-10-31")},
		TotalsQuery{}, ByDay, ByAccount, ByRegion, ByService, ByTeam)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("page-dimension totals: %d groups in %s", len(groups), time.Since(start))
}
