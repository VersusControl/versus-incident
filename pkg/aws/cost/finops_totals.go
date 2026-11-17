package cost

import (
	"context"
	"errors"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// The FinOps core reads grouped totals through its own optional interface,
// so a source that sums in the store answers a view without a line read. The
// adapter's totals types carry the same fields as the core's, one for one;
// the method below converts them and nothing else, so the two vocabularies
// cannot drift without the build failing. Go matches an interface by method
// name, so the core-facing method is Totals; the adapter's own vocabulary
// lives in TotalsGroups.

var _ finops.TotalsSource = (*Source)(nil)

// Totals is TotalsGroups in the core's vocabulary. An unknown dimension is
// refused, and an over-budget read keeps its own sentinel while also
// satisfying the core's closed class, so the page and the agent tools report
// one reason without matching on the message.
func (s *Source) Totals(ctx context.Context, r finops.DateRange, q finops.TotalsQuery, groupBy ...finops.Dimension) ([]finops.Total, error) {
	dims := make([]Dimension, len(groupBy))
	for i, d := range groupBy {
		mapped, ok := totalsDimension(d)
		if !ok {
			return nil, ErrInvalidDimension
		}
		dims[i] = mapped
	}
	totals, err := s.TotalsGroups(ctx, r, TotalsQuery{Accounts: q.Accounts, Regions: q.Regions, Teams: q.Teams}, dims...)
	if err != nil {
		return nil, totalsError(err)
	}
	out := make([]finops.Total, len(totals))
	for i, t := range totals {
		out[i] = finopsTotal(t)
	}
	return out, nil
}

// totalsDimension maps a core dimension onto the adapter's, accepting only a
// dimension this source can answer.
func totalsDimension(d finops.Dimension) (Dimension, bool) {
	mapped := Dimension(d)
	if _, ok := storageDimensions[mapped]; !ok {
		return "", false
	}
	return mapped, true
}

// totalsError classes a refused read. The adapter's sentinel stays in the
// chain, so errors.Is finds it, and the core's class is added on top: the
// original message is part of both.
func totalsError(err error) error {
	if errors.Is(err, ErrCostLinesBudgetExceeded) {
		return fmt.Errorf("%w: %w", finops.ErrPeriodTooLarge, err)
	}
	return err
}

// finopsTotal copies one group field for field.
func finopsTotal(t Total) finops.Total {
	return finops.Total{
		Day: t.Day, Month: t.Month, Account: t.Account, Region: t.Region, Service: t.Service,
		ServiceName: t.ServiceName, UsageType: t.UsageType, RecordType: t.RecordType, Team: t.Team,
		ResourceID: t.ResourceID, UsageGroup: t.UsageGroup, Unit: t.Unit, MinDay: t.MinDay,
		NetAmortizedMicros: t.NetAmortizedMicros, QuantityMicros: t.QuantityMicros, Lines: t.Lines,
		EstimatedTeam: t.EstimatedTeam, EstimatedRegion: t.EstimatedRegion,
	}
}
