package storage

import (
	"sort"
	"time"
)

// The daily cost rollup holds one row per payer, day, account, region,
// service, service name, usage group, record type and team: the cost lines
// without a resource ID, summed over their usage types. A page total groups
// by those columns or fewer, so reading the rollup answers it from one row
// per group instead of one per usage type. It is kept in the same
// transaction as the cost lines it sums, so a reader never sees the two
// disagree.
//
// A rollup row whose sum did not fit a signed 64-bit amount is stored with
// overflowed set and zero sums. A total that touches one is answered from the
// cost lines instead, which report the closed overflow error or the exact
// sum, so the rollup never changes an answer.

// finOpsDailyDimensions are the dimensions the rollup can group by: its key,
// and the month derived from the day.
var finOpsDailyDimensions = map[FinOpsDimension]bool{
	FinOpsByDay: true, FinOpsByMonth: true, FinOpsByPayer: true, FinOpsByAccount: true, FinOpsByRegion: true,
	FinOpsByService: true, FinOpsByServiceName: true, FinOpsByUsageGroup: true, FinOpsByRecordType: true, FinOpsByTeam: true,
}

// finOpsDailyKey is the rollup key in the order a page groups by it; with
// every one of them requested a rollup row is already a group.
var finOpsDailyKey = []FinOpsDimension{
	FinOpsByDay, FinOpsByAccount, FinOpsByRegion, FinOpsByService, FinOpsByServiceName,
	FinOpsByUsageGroup, FinOpsByRecordType, FinOpsByTeam, FinOpsByPayer,
}

var finOpsDailyColumns = []string{"org_id", "payer", "day", "account", "region", "service", "service_name", "usage_group", "record_type", "team",
	"net_amortized_micros", "quantity_micros", "lines", "estimated_team", "estimated_region", "overflowed"}

// finOpsDailyRow is one rollup row. Its line carries the key and the sums;
// the usage type, resource ID and per-line attributes are empty.
type finOpsDailyRow struct {
	line       FinOpsCostLine
	lines      int64
	overflowed bool
}

type finOpsDailyRowKey struct {
	payer, day, account, region, service, serviceName, usageGroup, recordType, team string
}

func finOpsDailyKeyOf(line FinOpsCostLine) finOpsDailyRowKey {
	return finOpsDailyRowKey{line.Payer, line.Day, line.Account, line.Region, line.Service, line.ServiceName, line.UsageGroup, line.RecordType, line.Team}
}

// rollUpFinOpsLines sums the lines without a resource ID into rollup rows,
// ordered by key. A group whose running sum leaves the signed 64-bit range is
// marked overflowed with zero sums: the cost lines answer for it.
func rollUpFinOpsLines(lines []FinOpsCostLine) []finOpsDailyRow {
	groups := map[finOpsDailyRowKey]*finOpsDailyRow{}
	for _, line := range lines {
		if line.ResourceID != "" {
			continue
		}
		key := finOpsDailyKeyOf(line)
		row := groups[key]
		if row == nil {
			row = &finOpsDailyRow{line: FinOpsCostLine{Payer: line.Payer, Day: line.Day, Account: line.Account, Region: line.Region,
				Service: line.Service, ServiceName: line.ServiceName, UsageGroup: line.UsageGroup, RecordType: line.RecordType, Team: line.Team}}
			groups[key] = row
		}
		row.lines++
		row.line.EstimatedTeam = row.line.EstimatedTeam || line.EstimatedTeam
		row.line.EstimatedRegion = row.line.EstimatedRegion || line.EstimatedRegion
		if row.overflowed {
			continue
		}
		net, netErr := addAmount(row.line.NetAmortizedMicros, line.NetAmortizedMicros)
		quantity, quantityErr := addAmount(row.line.QuantityMicros, line.QuantityMicros)
		if netErr != nil || quantityErr != nil {
			row.overflowed = true
			row.line.NetAmortizedMicros, row.line.QuantityMicros = 0, 0
			continue
		}
		row.line.NetAmortizedMicros, row.line.QuantityMicros = net, quantity
	}
	out := make([]finOpsDailyRow, 0, len(groups))
	for _, row := range groups {
		out = append(out, *row)
	}
	sort.Slice(out, func(left, right int) bool {
		return lessFinOpsLine(out[left].line, out[right].line) || lessFinOpsUsageGroup(out[left].line, out[right].line)
	})
	return out
}

// lessFinOpsUsageGroup orders two rollup rows equal on every line key column
// by their usage group, the one rollup key column a line key does not hold.
func lessFinOpsUsageGroup(left, right FinOpsCostLine) bool {
	return left.key() == right.key() && left.UsageGroup < right.UsageGroup
}

// finOpsDailyAnswers reports whether the rollup answers a totals query: it
// sums only lines without a resource ID, over no usage type, grouped by
// rollup columns. Everything else reads the cost lines.
func finOpsDailyAnswers(query FinOpsCostQuery, groupBy []FinOpsDimension) bool {
	if query.ResourceID == nil || *query.ResourceID != "" || query.UsageType != "" {
		return false
	}
	for _, dimension := range groupBy {
		if !finOpsDailyDimensions[dimension] {
			return false
		}
	}
	return true
}

// finOpsDailyWholeKey reports whether groupBy is exactly the rollup key, in
// any order: then each rollup row is one group and nothing is summed.
func finOpsDailyWholeKey(groupBy []FinOpsDimension) bool {
	if len(groupBy) != len(finOpsDailyKey) {
		return false
	}
	for _, dimension := range groupBy {
		if dimension == FinOpsByMonth || !finOpsDailyDimensions[dimension] {
			return false
		}
	}
	return true
}

// finOpsDailyQuery is the query the rollup is filtered by: the resource ID is
// implied, since the rollup only holds lines without one.
func finOpsDailyQuery(query FinOpsCostQuery) FinOpsCostQuery {
	query.ResourceID = nil
	return query
}

var finOpsMonthlyColumns = []string{"org_id", "payer", "month", "account", "region", "service", "service_name", "usage_group", "record_type", "team", "min_day",
	"net_amortized_micros", "quantity_micros", "lines", "estimated_team", "estimated_region", "overflowed"}

// finOpsMonthOf is the first day of a day's month.
func finOpsMonthOf(day string) string { return day[:7] + "-01" }

// finOpsMonthEnd is the last day of the month a first day starts.
func finOpsMonthEnd(month string) string {
	start, err := time.Parse(time.DateOnly, month)
	if err != nil {
		return month
	}
	return start.AddDate(0, 1, -1).Format(time.DateOnly)
}

// rollUpFinOpsMonths sums daily rollup rows per payer, month and rollup key,
// in the order the groups first appear. A group summing an overflowed row,
// or leaving the signed 64-bit range, is marked overflowed with zero sums.
func rollUpFinOpsMonths(daily []finOpsDailyRow) []finOpsMonthlyRow {
	groups := map[finOpsDailyRowKey]*finOpsMonthlyRow{}
	order := make([]finOpsDailyRowKey, 0)
	for _, row := range daily {
		l := row.line
		key := finOpsDailyKeyOf(l)
		key.day = finOpsMonthOf(l.Day)
		month := groups[key]
		if month == nil {
			month = &finOpsMonthlyRow{finOpsDailyRow: finOpsDailyRow{line: FinOpsCostLine{Payer: l.Payer, Day: key.day, Account: l.Account, Region: l.Region,
				Service: l.Service, ServiceName: l.ServiceName, UsageGroup: l.UsageGroup, RecordType: l.RecordType, Team: l.Team}}, minDay: l.Day}
			groups[key] = month
			order = append(order, key)
		}
		if l.Day < month.minDay {
			month.minDay = l.Day
		}
		month.lines += row.lines
		month.line.EstimatedTeam = month.line.EstimatedTeam || l.EstimatedTeam
		month.line.EstimatedRegion = month.line.EstimatedRegion || l.EstimatedRegion
		if month.overflowed {
			continue
		}
		net, netErr := addAmount(month.line.NetAmortizedMicros, l.NetAmortizedMicros)
		quantity, quantityErr := addAmount(month.line.QuantityMicros, l.QuantityMicros)
		if row.overflowed || netErr != nil || quantityErr != nil {
			month.overflowed = true
			month.line.NetAmortizedMicros, month.line.QuantityMicros = 0, 0
			continue
		}
		month.line.NetAmortizedMicros, month.line.QuantityMicros = net, quantity
	}
	out := make([]finOpsMonthlyRow, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out
}

// finOpsMonthlyRow is one monthly rollup row: the month's first day is the
// line's Day, and minDay is the group's first billing day in the month.
type finOpsMonthlyRow struct {
	finOpsDailyRow
	minDay string
}

// finOpsWholeMonths splits an inclusive day range into the whole calendar
// months it holds, as first days, and the days before and after them. Days
// are "YYYY-MM-DD"; a range without a bound holds no whole month.
func finOpsWholeMonths(from, through string) (months []string, head, tail [2]string) {
	if from == "" || through == "" || from > through {
		return nil, [2]string{}, [2]string{}
	}
	first := finOpsMonthOf(from)
	if first != from {
		first = finOpsNextMonth(first)
	}
	month := first
	for finOpsMonthEnd(month) <= through {
		months = append(months, month)
		month = finOpsNextMonth(month)
	}
	if len(months) == 0 {
		return nil, [2]string{}, [2]string{}
	}
	if from < first {
		head = [2]string{from, finOpsDayBefore(first)}
	}
	if end := finOpsMonthEnd(months[len(months)-1]); end < through {
		tail = [2]string{finOpsDayAfter(end), through}
	}
	return months, head, tail
}

func finOpsNextMonth(month string) string {
	start, err := time.Parse(time.DateOnly, month)
	if err != nil {
		return month
	}
	return start.AddDate(0, 1, 0).Format(time.DateOnly)
}

func finOpsDayBefore(day string) string {
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return at.AddDate(0, 0, -1).Format(time.DateOnly)
}

func finOpsDayAfter(day string) string {
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return at.AddDate(0, 0, 1).Format(time.DateOnly)
}
