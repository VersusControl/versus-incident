package finops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Grouped totals are the page path's alternative to reading cost lines: the
// source sums and groups in the store, so a view reads one row per group
// instead of one row per line. The types below are deliberately identical to
// the provider adapter's totals types, field for field, so adapting one to
// the other is a conversion and nothing more.

// Dimension names a column cost totals can be grouped by.
type Dimension string

// The grouping dimensions.
const (
	ByDay         Dimension = "day"
	ByMonth       Dimension = "month"
	ByAccount     Dimension = "account"
	ByRegion      Dimension = "region"
	ByService     Dimension = "service"
	ByServiceName Dimension = "service_name"
	ByUsageType   Dimension = "usage_type"
	ByRecordType  Dimension = "record_type"
	ByTeam        Dimension = "team"
	ByUsageGroup  Dimension = "usage_group"
	ByUnit        Dimension = "unit"
	ByResourceID  Dimension = "resource_id"
)

// TotalsQuery narrows a totals read the way a view narrows lines: any listed
// account, region, team, service or usage type matches, and an empty list
// matches everything. A Teams entry of "" selects the cost with no team. The
// service and usage-type lists let a drill-down ask for one line's detail
// instead of the payer's whole history.
type TotalsQuery struct {
	Accounts   []string
	Regions    []string
	Teams      []string
	Services   []string
	UsageTypes []string
}

// empty reports that the query narrows nothing.
func (q TotalsQuery) empty() bool {
	return len(q.Accounts) == 0 && len(q.Regions) == 0 && len(q.Teams) == 0 &&
		len(q.Services) == 0 && len(q.UsageTypes) == 0
}

// anyOf reports whether v is one of xs; an empty xs matches everything.
func anyOf(xs []string, v string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// matchesLine reports whether a line is inside the query's narrowing.
func (q TotalsQuery) matchesLine(l CostLine) bool {
	return anyOf(q.Accounts, l.Account) && anyOf(q.Regions, l.Region) && anyOf(q.Teams, l.Team) &&
		anyOf(q.Services, l.Service) && anyOf(q.UsageTypes, l.UsageType)
}

// matchesTotal is matchesLine for a summed group. A narrow read groups by the
// fields it narrows on, so every returned group carries them and a source
// that could not apply the filter is still narrowed here.
func (q TotalsQuery) matchesTotal(t Total) bool {
	return anyOf(q.Accounts, t.Account) && anyOf(q.Regions, t.Region) && anyOf(q.Teams, t.Team) &&
		anyOf(q.Services, t.Service) && anyOf(q.UsageTypes, t.UsageType)
}

// Total is one group of a cost aggregation. Only the requested dimensions
// are set. MinDay is the earliest day in the group whether or not day is
// grouped, so a view can tell when a group first billed without reading its
// lines. Lines counts the stored rows summed, which for an Organization is
// more than the lines it shows because drill detail is folded back into
// service lines.
type Total struct {
	Day                Date   `json:"day,omitzero"`
	Month              string `json:"month,omitempty"`
	Account            string `json:"account,omitempty"`
	Region             string `json:"region,omitempty"`
	Service            string `json:"service,omitempty"`
	ServiceName        string `json:"service_name,omitempty"`
	UsageType          string `json:"usage_type,omitempty"`
	RecordType         string `json:"record_type,omitempty"`
	Team               string `json:"team,omitempty"`
	ResourceID         string `json:"resource_id,omitempty"`
	UsageGroup         string `json:"usage_group,omitempty"`
	Unit               string `json:"unit,omitempty"`
	MinDay             Date   `json:"min_day,omitzero"`
	NetAmortizedMicros int64  `json:"net_amortized_micros"`
	QuantityMicros     int64  `json:"quantity_micros"`
	Lines              int64  `json:"lines"`
	EstimatedTeam      bool   `json:"estimated_team,omitempty"`
	EstimatedRegion    bool   `json:"estimated_region,omitempty"`
}

// TotalsSource is the optional half of a cost source that sums in the store.
// A source that implements it answers the views without a line read; one
// that does not keeps working through CostSource.Lines.
type TotalsSource interface {
	// Totals returns the range's cost grouped by the dimensions, as typed
	// fields, with the amount and quantity summed in integer micro-units.
	Totals(ctx context.Context, r DateRange, q TotalsQuery, groupBy ...Dimension) ([]Total, error)
}

// SavingsTotalsSource is the optional savings half of a source that sums in
// the store. Instead of reading the last 30 days of cost lines to find NAT
// processing and unused addresses, the source is handed the window and asks
// the store for just the usage types its rules name. It keeps that choice:
// grouping the whole window by usage type would cost exactly what the line
// scan cost. A source that keeps the line scan implements only
// SavingsInputs, and the numbers must not change: both shapes are the same
// sum.
type SavingsTotalsSource interface {
	SavingsInputsFromTotals(ctx context.Context, r DateRange) (SavingsInputs, error)
}

// SavingsFactsQuery is the window the savings facts cover and, when the
// caller already summed it, how many distinct days billed inside it. The
// page totals group by day, so the caller can count that set without the
// source asking the store for payer × day a second time.
type SavingsFactsQuery struct {
	Range DateRange
	// BilledDays is the number of distinct days inside Range that had cost;
	// meaningful only when DaysKnown is true.
	BilledDays int
	// DaysKnown says the caller counted BilledDays from totals it holds. A
	// source that reads the window itself sets it false.
	DaysKnown bool
}

// SavingsFactsSource is the savings seam that accepts the billed-day count a
// page already has. It is the preferred shape; a source that still implements
// SavingsTotalsSource keeps working through it.
type SavingsFactsSource interface {
	SavingsInputsFromFacts(ctx context.Context, q SavingsFactsQuery) (SavingsInputs, error)
}

// ErrPeriodTooLarge reports a period holding more cost lines than the source
// can summarise at once. It is the closed class an over-budget line read
// takes, so the route and the agent tools answer the same: narrow the period.
var ErrPeriodTooLarge = errors.New("finops: the period holds more cost lines than this deployment can summarise at once; narrow the period")

// sourceError classes a source's own error into a core sentinel. A provider
// adapter cannot import this package's sentinel into its error value without
// an import cycle, so the over-budget class is recognised by its phrase, the
// same way the read-only agent tools already recognise a bad-argument error.
func sourceError(err error) error {
	if err == nil || errors.Is(err, ErrPeriodTooLarge) {
		return err
	}
	if strings.Contains(strings.ToLower(err.Error()), "narrow the period") {
		return fmt.Errorf("%w: %v", ErrPeriodTooLarge, err)
	}
	return err
}

// lineOfTotal is the field copy from a summed group to the line shape the
// views already aggregate. Day falls back to MinDay so a read that does not
// group by day still places the group on its first billing day.
func lineOfTotal(t Total) CostLine {
	day := t.Day
	if day.IsZero() {
		day = t.MinDay
	}
	return CostLine{
		Day: day, Account: t.Account, Region: t.Region, Service: t.Service, ServiceName: t.ServiceName,
		UsageType: t.UsageType, UsageGroup: t.UsageGroup, RecordType: t.RecordType, ResourceID: t.ResourceID,
		Team: t.Team, NetAmortizedMicros: Micros(t.NetAmortizedMicros), QuantityMicros: t.QuantityMicros,
		Unit: t.Unit, EstimatedTeam: t.EstimatedTeam, EstimatedRegion: t.EstimatedRegion,
	}
}

// pageDims is the granularity a page shows: day, account, region, service,
// record type, team and usage group. A usage group is the coarsest label a
// service row drills into; the individual usage type is detail, and reading
// every usage type of a large payer is what made a page ask the store for
// hundreds of thousands of groups. The resource ID is not here either: a
// drill-down reads its own resource rows.
var pageDims = []Dimension{
	ByDay, ByAccount, ByRegion, ByService, ByServiceName, ByUsageGroup, ByRecordType, ByTeam,
}

// detailDims adds the usage type an explicit details depth shows in a bridge
// row and a service row's drill. It is still a page read, but only the pages
// that name a usage type ask for it.
var detailDims = append(append([]Dimension{}, pageDims...), ByUsageType)

// increaseDims is the granularity an increase's own series and cause tree
// read. It adds the unit the increase labels its usage signal with; an
// increase view reads it through a query narrowed to the increase's account
// and service, never over the whole payer.
var increaseDims = append(append([]Dimension{}, detailDims...), ByUnit)

// viewDims is the granularity a grouping view reads from totals. Only the
// application split falls back to lines: it reads the per-line app tag
// totals do not carry.
func viewDims(q Query) []Dimension {
	if q.GroupBy == GroupApplication {
		return nil
	}
	if q.depth() == narrative.Details {
		return detailDims
	}
	return pageDims
}
