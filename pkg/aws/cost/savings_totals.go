package cost

import (
	"context"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The FinOps core hands the savings window to a source that sums in the
// store, so the last 30 days never need a line read. The source keeps the
// choice of granularity: the rules name two usage-type families, and AWS
// bills them under two services, so two filtered totals queries answer them
// where grouping every usage type in the window would cost the line scan
// again.

var (
	_ finops.SavingsFactsSource  = (*Source)(nil)
	_ finops.SavingsTotalsSource = (*Source)(nil)
)

// savingsServices are the AWS services the billed savings facts come from:
// NAT gateway processing bills under EC2 - Other and unused public IPv4
// addresses under Amazon Virtual Private Cloud.
const (
	savingsNATService  = "EC2 - Other"
	savingsIdleService = "Amazon Virtual Private Cloud"
)

// SavingsInputsFromFacts is the facts seam the FinOps core prefers: it hands
// the window and, when the page totals already counted it, the number of days
// that billed, so the source skips its own payer-by-day aggregation.
func (s *Source) SavingsInputsFromFacts(ctx context.Context, q finops.SavingsFactsQuery) (finops.SavingsInputs, error) {
	st, err := s.cache.status()
	if err != nil {
		return finops.SavingsInputs{}, err
	}
	in := s.savingsFacts(st)
	if st.UpdatedAt.IsZero() {
		return in, nil
	}
	billed, err := s.billedTotals(ctx, st, q.Range, q.BilledDays, q.DaysKnown)
	if err != nil {
		return in, err
	}
	return s.addBilledSavings(ctx, in, billed)
}

// SavingsInputsFromTotals is the older seam: the same facts, with the source
// counting the window's billed days from the store itself.
func (s *Source) SavingsInputsFromTotals(ctx context.Context, r finops.DateRange) (finops.SavingsInputs, error) {
	st, err := s.cache.status()
	if err != nil {
		return finops.SavingsInputs{}, err
	}
	in := s.savingsFacts(st)
	if st.UpdatedAt.IsZero() {
		return in, nil
	}
	billed, err := s.billedTotals(ctx, st, r, 0, false)
	if err != nil {
		return in, err
	}
	return s.addBilledSavings(ctx, in, billed)
}

// billedTotals sums the window's billed savings facts from store totals. A
// caller that already counted the billed days passes them, and the
// payer-by-day query is skipped; otherwise the store answers it.
func (s *Source) billedTotals(ctx context.Context, st *statusRecord, r finops.DateRange, days int, daysKnown bool) (billedUsage, error) {
	out := billedUsage{natCost: map[usageKey]int64{}, idleIP: map[usageKey]int64{}, idleHours: map[usageKey]int64{}}
	store, err := s.cache.tables()
	if err != nil {
		return out, err
	}
	configured := map[string]bool{}
	organizations := map[string]bool{}
	for _, p := range s.payers() {
		configured[p.AccountID] = true
		if s.organization(st, p) {
			organizations[p.AccountID] = true
		}
	}
	query := storage.FinOpsCostQuery{From: r.Start.String(), Through: r.End.String(), ResourceID: noResource}

	// The unused-address count divides by the days the window billed. A page
	// that already counted them from its day totals passes the number; only
	// a caller without it pays for the payer-by-day aggregation.
	if daysKnown {
		out.days = days
	} else {
		dayRows, err := store.FinOpsCostTotals(ctx, s.cache.scope, query, storage.FinOpsByPayer, storage.FinOpsByDay)
		if err != nil {
			return out, err
		}
		seen := map[string]bool{}
		for _, row := range dayRows {
			if configured[row.Payer] {
				seen[row.Day] = true
			}
		}
		out.days = len(seen)
	}

	if err := s.addBilledTotals(ctx, store, query, configured, organizations, savingsNATService, natProcessingUsage, false, &out); err != nil {
		return out, err
	}
	if err := s.addBilledTotals(ctx, store, query, configured, organizations, savingsIdleService, idleAddressUsage, true, &out); err != nil {
		return out, err
	}
	return out, nil
}

// addBilledTotals sums one service's usage-type groups whose type names the
// marker. An Organization's drill rows are skipped: its lines carry no usage
// type, so the line-based answer names a gap instead of finding these.
func (s *Source) addBilledTotals(ctx context.Context, store storage.FinOpsStore, query storage.FinOpsCostQuery, configured, organizations map[string]bool, service, marker string, idle bool, out *billedUsage) error {
	read := query
	read.ServiceName = service
	rows, err := store.FinOpsCostTotals(ctx, s.cache.scope, read,
		storage.FinOpsByPayer, storage.FinOpsByAccount, storage.FinOpsByRegion, storage.FinOpsByUsageType)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !configured[row.Payer] || organizations[row.Payer] || !strings.Contains(row.UsageType, marker) {
			continue
		}
		k := usageKey{row.Account, row.Region}
		if idle {
			if out.idleIP[k], err = addTotalAmount(out.idleIP[k], row.NetAmortizedMicros); err != nil {
				return err
			}
			if out.idleHours[k], err = addTotalAmount(out.idleHours[k], row.QuantityMicros); err != nil {
				return err
			}
			continue
		}
		if out.natCost[k], err = addTotalAmount(out.natCost[k], row.NetAmortizedMicros); err != nil {
			return err
		}
	}
	return nil
}
