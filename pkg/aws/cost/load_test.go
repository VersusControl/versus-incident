package cost

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"
)

// TestSummaryLoadBudget loads 13 months of cost lines for an Organization
// of 150 accounts (about 1,800 lines a day) and holds the FinOps summary,
// read through the cost tables, under 300 ms. It takes minutes, so it runs
// only with TEST_FINOPS_LOAD=1 (on the backend storagefix selects).
func TestSummaryLoadBudget(t *testing.T) {
	if os.Getenv("TEST_FINOPS_LOAD") != "1" {
		t.Skip("TEST_FINOPS_LOAD=1 not set")
	}
	const (
		payer    = "999999999999"
		accounts = 150
	)
	provider := storagefix.Provider(t)
	scope := tenancy.DefaultOrgScope()
	store, ok := storage.FinOps(provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	services := []struct{ name, key string }{
		{"Amazon Elastic Compute Cloud - Compute", "compute.vm"},
		{"Amazon Relational Database Service", "database.relational"},
		{"Amazon Simple Storage Service", "storage.object"},
		{"EC2 - Other", "EC2 - Other"},
	}
	regions := []string{"us-east-1", "eu-west-1", "us-west-2"}
	teams := []string{"checkout", "platform", "data", ""}
	first, last := finops.MustDate("2025-11-01"), finops.MustDate("2026-11-08")
	started := time.Now()
	total := 0
	for month := first; !month.After(last); month = month.MonthEnd().AddDays(1) {
		var days []string
		var rows []storage.FinOpsCostLine
		for d := month; !d.After(month.MonthEnd()) && !d.After(last); d = d.AddDays(1) {
			days = append(days, d.String())
			for a := 0; a < accounts; a++ {
				account := strconv.FormatInt(100000000001+int64(a), 10)
				for s, svc := range services {
					for r := 0; r < 3; r++ {
						cost := int64(1_000_000 + a*1000 + s*100 + r)
						if svc.key == "EC2 - Other" && a%10 == 0 && !d.Before(finops.MustDate("2026-10-09")) {
							cost *= 4
						}
						rows = append(rows, storage.FinOpsCostLine{Payer: payer, Day: d.String(), Account: account, Region: regions[r],
							Service: svc.key, ServiceName: svc.name, RecordType: finops.RecordUsage, Team: teams[(a+s+r)%4],
							Currency: "USD", NetAmortizedMicros: cost, EstimatedRegion: true})
					}
				}
			}
		}
		if err := store.ReplaceFinOpsCostDays(ctx, scope, payer, days, rows); err != nil {
			t.Fatal(err)
		}
		total += len(rows)
	}
	t.Logf("loaded %d cost lines (%d accounts, %s..%s) in %s", total, accounts, first, last, time.Since(started).Round(time.Second))

	opts := Options{
		Payers:         []Payer{{AccountID: payer, Organization: true, Credentials: testCreds}},
		TagAllowlisted: true,
		Now:            func() time.Time { return at("2026-11-09", 10) },
	}
	a, err := New(provider, scope, opts)
	if err != nil {
		t.Fatal(err)
	}
	st := &statusRecord{UpdatedAt: at("2026-11-09", 9), Payers: map[string]*payerState{payer: {
		Organization: true, Currency: "USD", HistoryStart: first, LastCompleteDay: last, Accounts: map[string]*accountState{},
	}}}
	for i := 0; i < accounts; i++ {
		st.Payers[payer].Accounts[strconv.FormatInt(100000000001+int64(i), 10)] = &accountState{Name: fmt.Sprintf("account-%d", i), FirstCost: first}
	}
	if err := a.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	// Totals read the cost lines until the process has reconciled the
	// rollups; the scheduler does that in the background after startup.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	reconciled, err := a.ReconcileRollups(ctx)
	if err != nil || !reconciled.Done {
		t.Fatalf("rollup reconcile: %+v %v", reconciled, err)
	}
	runtime.ReadMemStats(&after)
	t.Logf("rollup reconcile on %s: %s, %d days differed, %d days and %d months rebuilt, heap allocated %d MiB, process memory %d -> %d MiB",
		storagefix.Backend(), reconciled.Took.Round(time.Millisecond), reconciled.Repair.Days, reconciled.Repair.RebuiltDays, reconciled.Repair.RebuiltMonths,
		(after.TotalAlloc-before.TotalAlloc)>>20, before.Sys>>20, after.Sys>>20)
	// On Postgres, also time the reconcile that has every day to rebuild:
	// the rollups emptied as if an older version had written every line.
	if raw, ok := provider.(storage.SQLAccessor); ok {
		if _, err := raw.DB().Exec(`DELETE FROM finops_cost_daily; DELETE FROM finops_cost_monthly`); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&before)
		rebuilt, err := a.reconcileAfterRefresh(ctx)
		runtime.ReadMemStats(&after)
		if err != nil || !rebuilt.Compared || rebuilt.Repair.RebuiltDays == 0 {
			t.Fatalf("rebuilding reconcile: %+v %v", rebuilt, err)
		}
		t.Logf("rollup reconcile rebuilding everything: %s, %d days and %d months rebuilt, heap allocated %d MiB, process memory %d -> %d MiB",
			rebuilt.Took.Round(time.Millisecond), rebuilt.Repair.RebuiltDays, rebuilt.Repair.RebuiltMonths,
			(after.TotalAlloc-before.TotalAlloc)>>20, before.Sys>>20, after.Sys>>20)
		again, err := a.reconcileAfterRefresh(ctx)
		if err != nil || again.Repair.RebuiltDays != 0 {
			t.Fatalf("compare after the rebuild: %+v %v", again, err)
		}
		t.Logf("rollup reconcile compare only, after the rebuild: %s", again.Took.Round(time.Millisecond))
	}
	svc := finops.NewService(a.Source(), NewStore(provider, scope), finops.Config{Now: opts.Now, ProviderName: "AWS"})
	var timings []time.Duration
	for i := 0; i < 3; i++ {
		begin := time.Now()
		v, err := svc.Summary(ctx, finops.Query{Period: "2026-10"})
		if err != nil {
			t.Fatal(err)
		}
		timings = append(timings, time.Since(begin))
		if !v.Enabled || v.Total == 0 {
			t.Fatalf("summary %+v", v.Header)
		}
	}
	t.Logf("summary on %s: %v", storagefix.Backend(), timings)
	best := timings[0]
	for _, d := range timings[1:] {
		best = min(best, d)
	}
	if best > 300*time.Millisecond {
		t.Fatalf("FinOps summary %s, over the 300 ms budget", best)
	}
}
