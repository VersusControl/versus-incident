package finops

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Period keys.
const (
	PeriodMonthToDate = "mtd"
	PeriodLast30Days  = "last_30d"
)

// Compare keys.
const (
	ComparePrevPeriod = "prev_period"
	CompareLastYear   = "same_period_last_year"
)

// Group-by keys.
const (
	GroupService      = "service"
	GroupAccount      = "account"
	GroupAccountGroup = "account_group"
	GroupRegion       = "region"
	GroupTeam         = "team"
	GroupApplication  = "application"
	GroupResource     = "resource"
	GroupCause        = "cause"
)

var groupBys = []string{GroupService, GroupAccount, GroupAccountGroup, GroupRegion, GroupTeam, GroupApplication, GroupResource, GroupCause}

// ErrBadQuery is returned for a query value outside the closed vocabulary.
// The wrapped message names the parameter, never echoes free text.
var ErrBadQuery = errors.New("finops: bad query")

// ErrNotFound is returned when an increase or savings item does not exist.
var ErrNotFound = errors.New("finops: not found")

// Query is the shared scope of every read. Zero values mean the defaults.
type Query struct {
	Period   string   `json:"period,omitempty"`  // "2026-10" | mtd | last_30d
	Compare  string   `json:"compare,omitempty"` // prev_period | same_period_last_year
	Accounts []string `json:"accounts,omitempty"`
	Regions  []string `json:"regions,omitempty"`
	Teams    []string `json:"teams,omitempty"`
	GroupBy  string   `json:"group_by,omitempty"`
	Depth    string   `json:"depth,omitempty"`  // summary | details
	Status   string   `json:"status,omitempty"` // increases filter: open | expected | resolved | all
}

var monthKey = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// Normalize fills defaults and validates the closed vocabularies.
func (q Query) Normalize() (Query, error) {
	if q.Period == "" {
		q.Period = PeriodMonthToDate
	}
	if q.Period != PeriodMonthToDate && q.Period != PeriodLast30Days && !monthKey.MatchString(q.Period) {
		return q, fmt.Errorf("%w: period", ErrBadQuery)
	}
	if q.Compare == "" {
		q.Compare = ComparePrevPeriod
	}
	if q.Compare != ComparePrevPeriod && q.Compare != CompareLastYear {
		return q, fmt.Errorf("%w: compare", ErrBadQuery)
	}
	if q.GroupBy != "" && !slices.Contains(groupBys, q.GroupBy) {
		return q, fmt.Errorf("%w: group_by", ErrBadQuery)
	}
	switch q.Depth {
	case "":
		q.Depth = string(narrative.Summary)
	case string(narrative.Summary), string(narrative.Details):
	default:
		return q, fmt.Errorf("%w: depth", ErrBadQuery)
	}
	switch q.Status {
	case "", "open", "expected", "resolved", "all":
	default:
		return q, fmt.Errorf("%w: status", ErrBadQuery)
	}
	q.Accounts = cleanList(q.Accounts)
	q.Regions = cleanList(q.Regions)
	q.Teams = cleanList(q.Teams)
	return q, nil
}

func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (q Query) depth() narrative.Depth { return narrative.ParseDepth(q.Depth) }

// Period is a resolved reporting period over complete days only.
type Period struct {
	Key   string `json:"key"`
	Label string `json:"label"` // "October 2026", "Last 30 days"
	Short string `json:"short"` // "October", used inside sentences
	// Range holds the complete days used for every number.
	Range DateRange `json:"range"`
	// Calendar is the whole span (the full month for a month period).
	Calendar DateRange `json:"calendar"`
	// Open reports that the calendar span has days still to come.
	Open         bool `json:"open"`
	CompleteDays int  `json:"complete_days"`
	month        bool
}

// Comparison is the base the period is compared with.
type Comparison struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Short string    `json:"short"`
	Range DateRange `json:"range"`
	// Available is false when the history does not cover the base range;
	// Reason then says why and FirstFull names the first comparable month.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	FirstFull string `json:"first_full,omitempty"` // "2026-11"
}

// ResolvePeriod turns query keys into day ranges. lastComplete is the last
// complete billing day; historyStart is the first day the source has data
// for (zero when unknown).
func ResolvePeriod(period, compare string, lastComplete, historyStart Date) (Period, Comparison, error) {
	if lastComplete.IsZero() {
		return Period{}, Comparison{}, fmt.Errorf("%w: no complete day yet", ErrBadQuery)
	}
	var p Period
	switch period {
	case "", PeriodMonthToDate:
		p = monthPeriod(lastComplete.MonthStart(), lastComplete)
		p.Key = PeriodMonthToDate
	case PeriodLast30Days:
		p = Period{Key: PeriodLast30Days, Label: "Last 30 days", Short: "the last 30 days",
			Range: DateRange{lastComplete.AddDays(-29), lastComplete}}
		p.Calendar = p.Range
	default:
		start, err := ParseDate(period + "-01")
		if err != nil {
			return Period{}, Comparison{}, fmt.Errorf("%w: period", ErrBadQuery)
		}
		if start.After(lastComplete) {
			return Period{}, Comparison{}, fmt.Errorf("%w: period has no complete day", ErrBadQuery)
		}
		p = monthPeriod(start, lastComplete)
		p.Key = period
	}
	p.CompleteDays = p.Range.Days()
	c := comparisonFor(p, compare)
	c.Available = true
	if !historyStart.IsZero() && c.Range.Start.Before(historyStart) {
		c.Available = false
		c.Reason = "no_full_base_period"
		first := historyStart.MonthStart()
		if historyStart.Day != 1 {
			first = historyStart.MonthEnd().AddDays(1)
		}
		c.FirstFull = first.MonthKey()
	}
	return p, c, nil
}

func monthPeriod(start, lastComplete Date) Period {
	end := start.MonthEnd()
	p := Period{
		Label:    start.Month.String() + " " + fmt.Sprint(start.Year),
		Short:    start.Month.String(),
		Calendar: DateRange{start, end},
		Range:    DateRange{start, end},
		month:    true,
	}
	if lastComplete.Before(end) {
		p.Range.End = lastComplete
		p.Open = true
	}
	return p
}

func comparisonFor(p Period, compare string) Comparison {
	if compare == "" {
		compare = ComparePrevPeriod
	}
	c := Comparison{Key: compare}
	if !p.month {
		shift := -p.Range.Days()
		if compare == CompareLastYear {
			c.Range = DateRange{shiftYear(p.Range.Start, -1), shiftYear(p.Range.End, -1)}
			c.Label, c.Short = "Same 30 days last year", "the same 30 days last year"
		} else {
			c.Range = DateRange{p.Range.Start.AddDays(shift), p.Range.End.AddDays(shift)}
			c.Label, c.Short = "Previous 30 days", "the previous 30 days"
		}
		return c
	}
	var start Date
	if compare == CompareLastYear {
		start = Date{p.Calendar.Start.Year - 1, p.Calendar.Start.Month, 1}
	} else {
		start = p.Calendar.Start.AddDays(-1).MonthStart()
	}
	end := start.MonthEnd()
	if p.Open {
		// Same number of complete days, clamped to the base month.
		if n := p.Range.Days(); n < DaysIn(start.Year, start.Month) {
			end = start.AddDays(n - 1)
		}
	}
	c.Range = DateRange{start, end}
	c.Short = start.Month.String()
	c.Label = start.Month.String() + " " + fmt.Sprint(start.Year)
	if compare == ComparePrevPeriod && start.Year == p.Calendar.Start.Year {
		c.Label = start.Month.String()
	}
	return c
}

func shiftYear(d Date, years int) Date {
	t := time.Date(d.Year+years, d.Month, 1, 0, 0, 0, 0, time.UTC)
	day := min(d.Day, DaysIn(t.Year(), t.Month()))
	return Date{t.Year(), t.Month(), day}
}

// MonthClosed reports whether the provider has closed the bill of month m
// as of today: bills close on closeDay of the following month.
func MonthClosed(m Date, today Date, closeDay int) bool {
	closeOn := m.MonthEnd().AddDays(closeDay)
	return !today.Before(closeOn)
}
