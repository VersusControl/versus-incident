package cost

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Cost lines live in the FinOps cost tables (storage.FinOpsStore), one set
// per payer and day. A refresh replaces whole (payer, day) sets in one
// transaction, so a day is either the previous refresh's or this one's.
//
// What is stored is the line set built from the day's Cost Explorer tables
// with team values kept only while the tag is allowlisted (reads redact
// them again as a second line of defence), an Organization's drilled pairs
// at usage-type level, and resource-level refinements (rows with a resource
// ID, never part of a total).

// ErrNoFinOpsTables means the storage backend has no FinOps cost tables.
var ErrNoFinOpsTables = errors.New("cost: the storage backend has no FinOps tables")

func (c cache) tables() (storage.FinOpsStore, error) {
	store, ok := storage.FinOps(c.provider)
	if !ok {
		return nil, ErrNoFinOpsTables
	}
	return store, nil
}

// noResource selects lines without a resource ID.
var noResource = func() *string { s := ""; return &s }()

// storedRows builds the rows stored for one payer day. Team values are
// redacted before the rows leave the process when the tag is not
// allowlisted, so an unauthorised value never reaches storage.
func storedRows(day finops.Date, t *dayTables, o synthOptions, refreshed time.Time, allowTeams bool) []storage.FinOpsCostLine {
	o.keepTeams, o.detail = true, true
	lines := buildLines(day, t, o)
	if !o.organization {
		lines = append(lines, buildResourceLines(day, t, o)...)
	}
	if !allowTeams {
		lines = withoutTeams(lines)
	}
	return rowsOf(o.payer, lines, refreshed)
}

// rowsOf converts lines to rows. The stored key includes the AWS service
// name, so two AWS services under one neutral key stay apart; lines that
// still share a key are added together.
func rowsOf(payer string, lines []finops.CostLine, refreshed time.Time) []storage.FinOpsCostLine {
	type key struct{ day, account, region, service, serviceName, usageType, recordType, team, resource string }
	index := map[key]int{}
	var out []storage.FinOpsCostLine
	for _, l := range lines {
		record := l.RecordType
		if record == "" {
			record = finops.RecordUsage
		}
		k := key{l.Day.String(), l.Account, l.Region, l.Service, l.ServiceName, l.UsageType, record, l.Team, l.ResourceID}
		if i, ok := index[k]; ok {
			r := &out[i]
			r.NetAmortizedMicros += l.NetAmortizedMicros
			r.QuantityMicros += l.QuantityMicros
			r.EstimatedTeam = r.EstimatedTeam || l.EstimatedTeam
			r.EstimatedRegion = r.EstimatedRegion || l.EstimatedRegion
			if r.Unit != l.Unit {
				r.Unit = ""
			}
			continue
		}
		index[k] = len(out)
		out = append(out, storage.FinOpsCostLine{
			Payer: payer, Day: k.day, Account: l.Account, Region: l.Region, Service: l.Service, UsageType: l.UsageType,
			RecordType: record, Team: l.Team, ResourceID: l.ResourceID, ServiceName: l.ServiceName, UsageGroup: l.UsageGroup,
			App: l.App, Env: l.Env, Currency: l.Currency, NetAmortizedMicros: l.NetAmortizedMicros, QuantityMicros: l.QuantityMicros,
			Unit: l.Unit, EstimatedTeam: l.EstimatedTeam, EstimatedRegion: l.EstimatedRegion, RefreshedAt: refreshed,
		})
	}
	return out
}

// lineOf converts a stored row back to a cost line.
func lineOf(r storage.FinOpsCostLine) finops.CostLine {
	day, _ := finops.ParseDate(r.Day)
	return finops.CostLine{
		Day: day, Account: r.Account, Region: r.Region, Service: r.Service, ServiceName: r.ServiceName,
		UsageType: r.UsageType, UsageGroup: r.UsageGroup, RecordType: r.RecordType, ResourceID: r.ResourceID,
		Team: r.Team, App: r.App, Env: r.Env, Currency: r.Currency, NetAmortizedMicros: r.NetAmortizedMicros,
		QuantityMicros: r.QuantityMicros, Unit: r.Unit, EstimatedTeam: r.EstimatedTeam, EstimatedRegion: r.EstimatedRegion,
	}
}

// isDrillRow reports whether an Organization row is drill-down detail: a
// usage line at usage-type level (undrilled Organization lines have none).
func isDrillRow(r storage.FinOpsCostLine) bool {
	return r.ResourceID == "" && r.RecordType == finops.RecordUsage && r.UsageType != ""
}

// tagGroup is the Cost Explorer tag group key of a team value.
func tagGroup(team string) string { return "$" + team }

// tablesOf rebuilds one payer day's tables from its stored rows. Every
// total is exact: account and record tables are the rows' sums, team
// totals are what the lines carry, drill and resource detail are stored as
// it came. An Organization has no usage table; its region mix is read from
// its undrilled lines, which were split by it.
func tablesOf(rows []storage.FinOpsCostLine, organization bool) *dayTables {
	t := &dayTables{}
	if organization {
		t.TeamBy = teamByAccount
	}
	type sum struct {
		m, q int64
		u    string
	}
	add := func(m map[[2]string]*sum, k [2]string, r storage.FinOpsCostLine) {
		s := m[k]
		if s == nil {
			m[k] = &sum{m: r.NetAmortizedMicros, q: r.QuantityMicros, u: r.Unit}
			return
		}
		s.m += r.NetAmortizedMicros
		s.q += r.QuantityMicros
		if s.u != r.Unit {
			s.u = ""
		}
	}
	account, usage, team := map[[2]string]*sum{}, map[[2]string]*sum{}, map[[2]string]*sum{}
	drill := map[string]map[[2]string]*sum{}
	mixAll, mixRest := map[[2]string]*sum{}, map[[2]string]*sum{}
	for _, r := range rows {
		switch {
		case r.ResourceID != "":
			t.Resource = append(t.Resource, cell{A: r.UsageType, B: r.ResourceID, M: r.NetAmortizedMicros, Q: r.QuantityMicros, U: r.Unit})
		case r.RecordType != finops.RecordUsage:
			t.Record = append(t.Record, cell{A: r.UsageType, B: r.Account, M: r.NetAmortizedMicros})
		default:
			add(account, [2]string{r.Account, r.ServiceName}, r)
			by := r.ServiceName
			if organization {
				by = r.Account
			}
			add(team, [2]string{tagGroup(r.Team), by}, r)
			switch {
			case organization && r.UsageType != "":
				pair := pairKey(r.Account, r.ServiceName)
				if drill[pair] == nil {
					drill[pair] = map[[2]string]*sum{}
				}
				add(drill[pair], [2]string{r.UsageType, tagGroup(r.Team)}, r)
				add(mixAll, [2]string{r.ServiceName, r.Region}, r)
			case organization:
				add(mixRest, [2]string{r.ServiceName, r.Region}, r)
				add(mixAll, [2]string{r.ServiceName, r.Region}, r)
			case r.UsageType != "":
				add(usage, [2]string{r.ServiceName, r.UsageType}, r)
			}
		}
	}
	cells := func(m map[[2]string]*sum, quantity bool) []cell {
		keys := make([][2]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		out := make([]cell, 0, len(keys))
		for _, k := range keys {
			c := cell{A: k[0], B: k[1], M: m[k].m}
			if quantity {
				c.Q, c.U = m[k].q, m[k].u
			}
			out = append(out, c)
		}
		return out
	}
	t.Account = cells(account, false)
	t.Team = cells(team, false)
	if organization {
		t.Drill = map[string][]cell{}
		for pair, m := range drill {
			t.Drill[pair] = cells(m, true)
		}
		t.regions = map[string][]weight{}
		for _, mix := range []map[[2]string]*sum{mixAll, mixRest} {
			bySvc := map[string][]weight{}
			for _, c := range cells(mix, false) {
				bySvc[c.A] = append(bySvc[c.A], weight{c.B, c.M})
			}
			for svc, ws := range bySvc {
				t.regions[svc] = ws
			}
		}
	} else {
		t.Usage = cells(usage, true)
	}
	sort.Slice(t.Record, func(i, j int) bool {
		if t.Record[i].B != t.Record[j].B {
			return t.Record[i].B < t.Record[j].B
		}
		return t.Record[i].A < t.Record[j].A
	})
	return t
}

// payerRows reads one payer's stored rows for the range, by day.
func (c cache) payerRows(ctx context.Context, payer string, r finops.DateRange) (map[finops.Date][]storage.FinOpsCostLine, error) {
	out := map[finops.Date][]storage.FinOpsCostLine{}
	if r.Days() == 0 {
		return out, nil
	}
	store, err := c.tables()
	if err != nil {
		return nil, err
	}
	rows, err := store.FinOpsCostLines(ctx, c.scope, storage.FinOpsCostQuery{From: r.Start.String(), Through: r.End.String(), Payer: payer})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		day, err := finops.ParseDate(row.Day)
		if err != nil {
			return nil, err
		}
		out[day] = append(out[day], row)
	}
	return out, nil
}

// replaceDays stores the payer's rows for the days in one transaction.
func (c cache) replaceDays(ctx context.Context, payer string, days []finops.Date, rows []storage.FinOpsCostLine) error {
	if len(days) == 0 {
		return nil
	}
	store, err := c.tables()
	if err != nil {
		return err
	}
	names := make([]string, len(days))
	for i, d := range days {
		names[i] = d.String()
	}
	return store.ReplaceFinOpsCostDays(ctx, c.scope, payer, names, rows)
}

// foldDrills turns an Organization day's drill rows back into the pair
// lines a view reads: one line per region and team under the pair's stable
// service key, so a drill-down never splits a series.
func foldDrills(rows []storage.FinOpsCostLine) []finops.CostLine {
	type key struct{ account, svc, region, team string }
	var order []key
	sums := map[key]*finops.CostLine{}
	var out []finops.CostLine
	for _, r := range rows {
		if !isDrillRow(r) {
			out = append(out, lineOf(r))
			continue
		}
		k := key{r.Account, r.ServiceName, r.Region, r.Team}
		if l := sums[k]; l != nil {
			l.NetAmortizedMicros += r.NetAmortizedMicros
			continue
		}
		l := lineOf(r)
		l.UsageType, l.UsageGroup, l.QuantityMicros, l.Unit = "", "", 0, ""
		l.EstimatedTeam, l.EstimatedRegion = false, false
		sums[k] = &l
		order = append(order, k)
	}
	for _, k := range order {
		if l := sums[k]; l.NetAmortizedMicros != 0 {
			out = append(out, *l)
		}
	}
	return out
}

// withoutTeams hides team values when the tag is not allowlisted: lines
// that differ only by team are added together.
func withoutTeams(lines []finops.CostLine) []finops.CostLine {
	type key struct {
		day                                                    finops.Date
		account, region, service, usage, record, resource, svc string
	}
	index := map[key]int{}
	var out []finops.CostLine
	for _, l := range lines {
		k := key{l.Day, l.Account, l.Region, l.Service, l.UsageType, l.RecordType, l.ResourceID, l.ServiceName}
		if i, ok := index[k]; ok {
			out[i].NetAmortizedMicros += l.NetAmortizedMicros
			out[i].QuantityMicros += l.QuantityMicros
			out[i].EstimatedRegion = out[i].EstimatedRegion || l.EstimatedRegion
			continue
		}
		l.Team, l.EstimatedTeam = "", false
		index[k] = len(out)
		out = append(out, l)
	}
	kept := out[:0]
	for _, l := range out {
		if l.NetAmortizedMicros != 0 || l.QuantityMicros != 0 {
			kept = append(kept, l)
		}
	}
	return kept
}

// payerDay is one payer's lines of one day as views read them.
type payerDay struct {
	payer     string
	lines     []finops.CostLine
	resources []finops.CostLine
}
