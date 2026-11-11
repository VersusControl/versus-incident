package cost

import (
	"context"
	"errors"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Grouped totals answer a view from the store instead of from the lines:
// the store sums and groups, so a period holding more lines than the line
// budget can still be summarised, and no line set is allocated.
//
// The types below are the neutral FinOps shapes a view needs. They mirror
// the storage grouping dimensions and are meant to move to pkg/finops once
// the views read totals; until then they live here, unopinionated.

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

// ErrInvalidDimension reports a grouping dimension this source cannot
// answer, or one named twice.
var ErrInvalidDimension = errors.New("cost: invalid totals dimension")

// storageDimensions maps the neutral dimension onto the store's column.
var storageDimensions = map[Dimension]storage.FinOpsDimension{
	ByDay:         storage.FinOpsByDay,
	ByMonth:       storage.FinOpsByMonth,
	ByAccount:     storage.FinOpsByAccount,
	ByRegion:      storage.FinOpsByRegion,
	ByService:     storage.FinOpsByService,
	ByServiceName: storage.FinOpsByServiceName,
	ByUsageType:   storage.FinOpsByUsageType,
	ByRecordType:  storage.FinOpsByRecordType,
	ByTeam:        storage.FinOpsByTeam,
	ByUsageGroup:  storage.FinOpsByUsageGroup,
	ByUnit:        storage.FinOpsByUnit,
	ByResourceID:  storage.FinOpsByResourceID,
}

// TotalsQuery narrows a totals read the way a view narrows Lines: any
// listed account, region or team matches, and an empty list matches
// everything. A Teams entry of "" selects the lines with no team.
//
// A Teams filter selects stored tag values, so it is meaningful only while
// the team tag is allowlisted; with the tag hidden, Lines carries no team
// value and the totals never report one either.
type TotalsQuery struct {
	Accounts []string
	Regions  []string
	Teams    []string
}

// Total is one group of a cost aggregation. Only the requested dimensions
// are set. MinDay is the earliest day in the group whether or not day is
// grouped. Lines counts the stored rows summed, which for an Organization
// is more than the lines it shows because drill detail is folded back into
// service lines.
type Total struct {
	Day                finops.Date `json:"day,omitzero"`
	Month              string      `json:"month,omitempty"`
	Account            string      `json:"account,omitempty"`
	Region             string      `json:"region,omitempty"`
	Service            string      `json:"service,omitempty"`
	ServiceName        string      `json:"service_name,omitempty"`
	UsageType          string      `json:"usage_type,omitempty"`
	RecordType         string      `json:"record_type,omitempty"`
	Team               string      `json:"team,omitempty"`
	ResourceID         string      `json:"resource_id,omitempty"`
	UsageGroup         string      `json:"usage_group,omitempty"`
	Unit               string      `json:"unit,omitempty"`
	MinDay             finops.Date `json:"min_day,omitzero"`
	NetAmortizedMicros int64       `json:"net_amortized_micros"`
	QuantityMicros     int64       `json:"quantity_micros"`
	Lines              int64       `json:"lines"`
	EstimatedTeam      bool        `json:"estimated_team,omitempty"`
	EstimatedRegion    bool        `json:"estimated_region,omitempty"`
}

// TotalsGroups returns the period's cost grouped by the dimensions, summed
// by the store over every configured payer and narrowed by q. It agrees with
// a sum of Lines for the day, account, service, team and usage-group
// groupings: account, region, service, team, usage group, day and month
// survive the Organization fold unchanged.
//
// An Organization's usage types, units and quantities are drill detail that
// a line read folds away, so its totals report them folded: usage rows
// carry no usage type or unit and no quantity, exactly like its lines. Its
// record rows keep the record name as their usage type.
//
// The line budget does not apply: the answer is one row per group, not one
// per line. Resource-level rows refine a usage type and are never summed, so
// ByResourceID is refused. A sum that does not fit a signed 64-bit total
// returns storage.ErrAmountOverflow unchanged.
func (s *Source) TotalsGroups(ctx context.Context, r finops.DateRange, q TotalsQuery, groupBy ...Dimension) ([]Total, error) {
	if r.Days() == 0 {
		return nil, nil
	}
	dims, err := totalsDimensions(groupBy, s.options().TagAllowlisted)
	if err != nil {
		return nil, err
	}
	st, err := s.cache.status()
	if err != nil {
		return nil, err
	}
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	configured := map[string]bool{}
	organizations := map[string]bool{}
	for _, p := range s.payers() {
		configured[p.AccountID] = true
		if s.organization(st, p) {
			organizations[p.AccountID] = true
		}
	}
	if len(configured) == 0 {
		return nil, nil
	}
	// One store query answers the whole deployment: the payer is grouped
	// only to drop rows of payers the deployment no longer reads and to
	// fold an Organization's drill detail. It is appended last, so the
	// requested dimensions stay the ordering prefix and a group's payers
	// arrive together.
	request := append(totalsRequest(dims, len(organizations) > 0), storage.FinOpsByPayer)
	// Resource-level rows refine a usage type; they are never part of a
	// total, exactly as a line read keeps them apart from its lines.
	rows, err := store.FinOpsCostTotals(ctx, s.cache.scope, storage.FinOpsCostQuery{
		From: r.Start.String(), Through: r.End.String(),
		ResourceID: noResource,
		Accounts:   q.Accounts, Regions: q.Regions, Teams: q.Teams,
	}, request...)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*Total, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if !configured[row.Payer] {
			continue
		}
		part, err := totalOf(row, dims, organizations[row.Payer])
		if err != nil {
			return nil, err
		}
		key := totalKey(dims, part)
		if cur := groups[key]; cur != nil {
			if err := mergeTotal(cur, part); err != nil {
				return nil, err
			}
			continue
		}
		groups[key] = &part
		order = append(order, key)
	}
	// The store orders by the requested dimensions, so first-seen order is
	// already the answer's order unless a fold moved a key; sort only then.
	if !sort.StringsAreSorted(order) {
		sort.Strings(order)
	}
	out := make([]Total, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out, nil
}

// totalsDimensions validates the requested grouping and hides the team
// dimension while the tag is not allowlisted. The result is ordered like
// the request, which is also the output order.
func totalsDimensions(groupBy []Dimension, allowlisted bool) ([]Dimension, error) {
	out := make([]Dimension, 0, len(groupBy))
	for _, d := range groupBy {
		if _, ok := storageDimensions[d]; !ok {
			return nil, ErrInvalidDimension
		}
		if d == ByTeam && !allowlisted {
			continue
		}
		if d == ByResourceID {
			// Resource rows are refinements kept out of every total.
			return nil, ErrInvalidDimension
		}
		if slices.Contains(out, d) {
			return nil, ErrInvalidDimension
		}
		out = append(out, d)
	}
	return out, nil
}

// totalsRequest maps the output dimensions onto the store grouping. When an
// Organization payer needs folding it adds the record type to a usage-type
// grouping, so drill rows (whose type is folded away) stay apart from record
// rows; the fold itself happens per row, in totalOf.
func totalsRequest(dims []Dimension, fold bool) []storage.FinOpsDimension {
	out := make([]storage.FinOpsDimension, 0, len(dims)+1)
	usageType := false
	for _, d := range dims {
		out = append(out, storageDimensions[d])
		if d == ByUsageType {
			usageType = true
		}
	}
	if fold && usageType && !slices.Contains(out, storage.FinOpsByRecordType) {
		out = append(out, storage.FinOpsByRecordType)
	}
	return out
}

// totalOf turns one store group into the neutral total, keeping only the
// requested dimensions.
func totalOf(row storage.FinOpsCostTotal, dims []Dimension, organization bool) (Total, error) {
	t := Total{
		NetAmortizedMicros: row.NetAmortizedMicros,
		QuantityMicros:     row.QuantityMicros,
		Lines:              row.Lines,
		EstimatedTeam:      row.EstimatedTeam,
		EstimatedRegion:    row.EstimatedRegion,
	}
	if row.MinDay != "" {
		day, err := finops.ParseDate(row.MinDay)
		if err != nil {
			return Total{}, err
		}
		t.MinDay = day
	}
	for _, d := range dims {
		switch d {
		case ByDay:
			day, err := finops.ParseDate(row.Day)
			if err != nil {
				return Total{}, err
			}
			t.Day = day
		case ByMonth:
			t.Month = row.Month
		case ByAccount:
			t.Account = row.Account
		case ByRegion:
			t.Region = row.Region
		case ByService:
			t.Service = row.Service
		case ByServiceName:
			t.ServiceName = row.ServiceName
		case ByUsageType:
			t.UsageType = row.UsageType
		case ByRecordType:
			t.RecordType = row.RecordType
		case ByTeam:
			t.Team = row.Team
		case ByUsageGroup:
			t.UsageGroup = row.UsageGroup
		case ByUnit:
			t.Unit = row.Unit
		case ByResourceID:
			t.ResourceID = row.ResourceID
		}
	}
	if organization {
		// A line read folds an Organization's drill rows back into service
		// lines, which drops the type, unit and quantity of the usage
		// detail. Totals drop them too, so both answer the same sum.
		if row.RecordType == finops.RecordUsage {
			t.UsageType = ""
		}
		t.Unit, t.QuantityMicros = "", 0
	}
	return t, nil
}

// mergeTotal adds one store group into the answer. A group can arrive from
// several payers, so the amounts are added with the store's overflow rule
// and the earliest day kept.
func mergeTotal(cur *Total, part Total) error {
	net, err := addTotalAmount(cur.NetAmortizedMicros, part.NetAmortizedMicros)
	if err != nil {
		return err
	}
	qty, err := addTotalAmount(cur.QuantityMicros, part.QuantityMicros)
	if err != nil {
		return err
	}
	cur.NetAmortizedMicros, cur.QuantityMicros = net, qty
	cur.Lines += part.Lines
	if !part.MinDay.IsZero() && (cur.MinDay.IsZero() || part.MinDay.Before(cur.MinDay)) {
		cur.MinDay = part.MinDay
	}
	cur.EstimatedTeam = cur.EstimatedTeam || part.EstimatedTeam
	cur.EstimatedRegion = cur.EstimatedRegion || part.EstimatedRegion
	return nil
}

// addTotalAmount adds two amounts, reporting the store's closed overflow
// error instead of a wrapped total.
func addTotalAmount(total, amount int64) (int64, error) {
	if amount > 0 && total > math.MaxInt64-amount || amount < 0 && total < math.MinInt64-amount {
		return 0, storage.ErrAmountOverflow
	}
	return total + amount, nil
}

// totalKey is a group's requested dimension values, joined so they sort in
// the requested order.
func totalKey(dims []Dimension, t Total) string {
	values := make([]string, len(dims))
	for i, d := range dims {
		values[i] = totalValue(t, d)
	}
	return strings.Join(values, "\x00")
}

func totalValue(t Total, d Dimension) string {
	switch d {
	case ByDay:
		return t.Day.String()
	case ByMonth:
		return t.Month
	case ByAccount:
		return t.Account
	case ByRegion:
		return t.Region
	case ByService:
		return t.Service
	case ByServiceName:
		return t.ServiceName
	case ByUsageType:
		return t.UsageType
	case ByRecordType:
		return t.RecordType
	case ByTeam:
		return t.Team
	case ByUsageGroup:
		return t.UsageGroup
	case ByUnit:
		return t.Unit
	case ByResourceID:
		return t.ResourceID
	}
	return ""
}
