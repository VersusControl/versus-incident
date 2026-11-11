package finops

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Summary answers "how much, and is that OK": totals, change, forecast,
// budget, savings total, unallocated share, freshness and the meter.
func (s *Service) Summary(ctx context.Context, q Query) (SummaryView, error) {
	sc, err := s.loadSummary(ctx, q)
	if err != nil || !sc.header.Enabled {
		v := SummaryView{Header: headerOf(sc)}
		if sc != nil && sc.meter != nil {
			// The meter still shows used/limit when no view can be built
			// (a first refresh stopped at the cap).
			v.Meter = *sc.meter
		}
		return v, err
	}
	body, err := s.summaryBody(ctx, sc)
	return SummaryView{Header: sc.header, SummaryBody: body}, err
}

func headerOf(sc *scope) Header {
	if sc == nil {
		return Header{}
	}
	return sc.header
}

func (s *Service) summaryBody(ctx context.Context, sc *scope) (SummaryBody, error) {
	b := sc.getBridge()
	body := SummaryBody{Total: b.Total, Meter: MeterView{Used: sc.st.MeterUsed, Cap: sc.st.MeterCap}}
	if body.Meter.Cap == 0 {
		body.Meter.Cap = s.cfg.MaxPaidRequests
	}
	if sc.comp.Available {
		cv := changeView(b.Base, b.Change)
		body.Change = &cv
	}
	daily := sc.dailyTotals(sc.period.Calendar, nil)
	sc.period.Calendar.Each(func(d Date) {
		amt, ok := daily[d]
		switch {
		case !d.After(sc.period.Range.End):
			body.Sparkline = append(body.Sparkline, DayAmount{Day: d, Amount: amt})
		case ok:
			body.Sparkline = append(body.Sparkline, DayAmount{Day: d, Amount: amt, Partial: true})
		}
	})
	body.Forecast = s.forecast(sc, nil)
	if !sc.filtered {
		budgets, err := s.allBudgets(ctx, sc)
		if err != nil {
			return body, err
		}
		for _, bg := range budgets {
			if bg.Scope == ScopeDeployment {
				bl := s.budgetLine(sc, bg)
				body.Budget = &bl
				break
			}
		}
	}
	sav, err := s.getSavings(ctx, sc)
	if err != nil {
		return body, err
	}
	body.Savings = savingsTotal(sav)
	un := sc.periodTotal(func(l CostLine) bool { return sc.ownerOf(l).Rule == "unallocated" })
	body.Unallocated = UnallocatedShare{Amount: un, ShareBP: basisPoints(un, body.Total)}

	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return body, err
	}
	body.Answer = narrative.AnswerLine(s.answer(sc, b, body, incs), sc.depth)
	body.ScopeOptions = sc.scopeOptions()
	return body, nil
}

// scopeOptions lists the accounts, regions and owners of the period before
// the query filters, so a filter never hides the other choices.
func (sc *scope) scopeOptions() ScopeOptions {
	out := ScopeOptions{Accounts: []ScopeAccountOption{}, Regions: []string{}, Teams: []string{}}
	for _, id := range sortedKeys(sc.accounts) {
		a := sc.accounts[id]
		out.Accounts = append(out.Accounts, ScopeAccountOption{ID: a.ID, Name: a.DisplayName(), Kind: a.Kind})
	}
	regions, teams := map[string]bool{}, map[string]bool{}
	for _, l := range sc.scopeLines {
		if !l.Day.After(sc.st.LastCompleteDay) {
			r, _ := sc.groupKey(l, GroupRegion)
			regions[r] = true
			teams[sc.ownerOf(l).Name] = true
		}
	}
	out.Regions, out.Teams = sortedKeys(regions), sortedKeys(teams)
	return out
}

// payerServices are bill-level rows: they belong to the bill, not to an
// account's resources.
func payerService(key string) bool {
	return strings.HasPrefix(key, "billing.") || strings.HasPrefix(key, "commitment.")
}

func changeView(base, change Micros) ChangeView {
	bp := basisPoints(change, base)
	dir := narrative.DirectionOf(bp)
	if base == 0 && change != 0 {
		dir = narrative.Up
		if change < 0 {
			dir = narrative.Down
		}
	}
	return ChangeView{Base: base, Change: change, ChangeBP: bp, Direction: dir, Word: narrative.ChangeWord(dir)}
}

// savingsTotal adds the open items once each. Alternatives are never items,
// and a commitment (computed before the other savings) stays out of the
// total; it is counted on its own so a zero total can say what it leaves out.
func savingsTotal(items []SavingView) SavingsTotal {
	var t SavingsTotal
	for _, it := range items {
		switch {
		case it.Status != "open":
		case it.BeforeOtherSavings:
			t.CommitmentMonthly += it.Monthly
			t.CommitmentCount++
		default:
			t.Monthly += it.Monthly
			t.Count++
		}
	}
	return t
}

// baseLabel names the base inside the headline ("from October"). A month
// base cut to the open period's complete days names those days ("from Oct
// 1–9"), never the whole month; a base in another year than the period
// carries its year ("from Nov 1–9, 2025", "from December 2025").
func baseLabel(p Period, c Comparison) string {
	if !p.month {
		return c.Short
	}
	r := c.Range
	label := c.Short
	if r.Start.Day != 1 || r.End != r.Start.MonthEnd() {
		label = r.Start.Time().Format("Jan 2")
		if r.End != r.Start {
			label += "–" + strconv.Itoa(r.End.Day)
		}
		if r.Start.Year != p.Calendar.Start.Year {
			label += ", " + strconv.Itoa(r.Start.Year)
		}
		return label
	}
	if r.Start.Year != p.Calendar.Start.Year {
		label += " " + strconv.Itoa(r.Start.Year)
	}
	return label
}

func (s *Service) answer(sc *scope, b Bridge, body SummaryBody, incs []IncreaseRecord) narrative.Answer {
	a := narrative.Answer{
		Currency:         sc.st.Currency,
		PeriodLabel:      sc.period.Short,
		Open:             sc.period.Open,
		Total:            body.Total,
		AccountsReadable: sc.header.Sync.AccountsReadable,
		AccountsTotal:    sc.header.Sync.AccountsTotal,
	}
	if sc.period.Key == PeriodLast30Days {
		a.PeriodLabel = "The last 30 days"
	}
	if body.Change != nil {
		a.HasBase, a.BaseLabel = true, baseLabel(sc.period, sc.comp)
		a.Change, a.ChangeBP = body.Change.Change, body.Change.ChangeBP
		if body.Change.Direction != narrative.DirectionOf(a.ChangeBP) {
			// A zero base: the sentence says up/down without a percent band.
			a.ChangeBP = 10_000
			if a.Change < 0 {
				a.ChangeBP = -10_000
			}
		}
		if top, ok := topDriver(b, narrative.DirectionOf(a.ChangeBP)); ok {
			p := s.phrase(sc, top, incs)
			a.Top = &p
			if b.Change != 0 {
				a.TopShareBP = basisPoints(top.Amount, b.Change)
				if a.TopShareBP < 0 {
					a.TopShareBP = -a.TopShareBP
				}
			}
		}
	}
	if body.Budget != nil && sc.period.month {
		a.HasBudget, a.Budget = true, body.Budget.Monthly
		if sc.period.Open && body.Forecast.Source != ForecastNone &&
			body.Forecast.Month == sc.period.Calendar.Start.MonthKey() {
			a.HasForecast, a.Forecast = true, body.Forecast.Amount
			a.ForecastIsEst = body.Forecast.Source == ForecastEstimate
		}
	}
	return a
}

func topDriver(b Bridge, dir string) (Driver, bool) {
	var best Driver
	found := false
	for _, d := range b.Drivers {
		switch dir {
		case narrative.Up:
			if d.Amount > 0 && (!found || d.Amount > best.Amount) {
				best, found = d, true
			}
		case narrative.Down:
			if d.Amount < 0 && (!found || d.Amount < best.Amount) {
				best, found = d, true
			}
		default:
			if !found || absMicros(d.Amount) > absMicros(best.Amount) {
				best, found = d, true
			}
		}
	}
	return best, found
}

// phrase turns a driver into narrative input.
func (s *Service) phrase(sc *scope, d Driver, incs []IncreaseRecord) narrative.DriverPhrase {
	p := narrative.DriverPhrase{
		Cause: d.Cause, RecordType: d.Key.RecordType, Amount: d.Amount,
		ServiceKey: d.Key.Service, ServiceName: d.ServiceName, Resource: d.Key.ResourceID,
		AccountName: sc.accountName(d.Key.Account), Since: dateTime(d.Since), OffsetsCount: d.Lines,
	}
	if sc.depth == narrative.Details {
		p.UsageType = d.Key.UsageType
	}
	if o := sc.owners.ResolveShare(d.Key.Account, d.Team, "", d.TeamEstimated, sc.depth); o.Rule == "tag" {
		p.Owner, p.OwnerEstimated = o.Name, o.Estimated
	}
	if inc := matchIncrease(d, incs); inc != nil && p.Since.IsZero() {
		p.Since = inc.StartedOn.Time()
	}
	return p
}

// Drivers answers "what moved the cost": the bridge from the base total to
// the period total, one row per driver.
func (s *Service) Drivers(ctx context.Context, q Query) (BridgeView, error) {
	// A bridge row carries line identity (its id, usage type and resource),
	// so this view reads usage types.
	sc, err := s.loadFor(ctx, q, detailDims)
	if err != nil || !sc.header.Enabled {
		return BridgeView{Header: headerOf(sc)}, err
	}
	body, err := s.bridgeBody(ctx, sc)
	return BridgeView{Header: sc.header, BridgeBody: body}, err
}

func (s *Service) bridgeBody(ctx context.Context, sc *scope) (BridgeBody, error) {
	b := sc.getBridge()
	body := BridgeBody{Available: sc.comp.Available, Total: b.Total, MinDriver: s.cfg.MinDriver, Rows: []BridgeRow{}}
	if !sc.comp.Available {
		if d, err := ParseDate(sc.comp.FirstFull + "-01"); err == nil {
			body.Note = narrative.NoBaseNote(d.Month)
		}
		return body, nil
	}
	cv := changeView(b.Base, b.Change)
	body.Base, body.Change, body.ChangeBP, body.Direction = b.Base, b.Change, cv.ChangeBP, cv.Direction
	body.Up, body.Down, body.Steady = b.Up, b.Down, b.Steady
	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return body, err
	}
	topRes, cut, err := s.topResources(ctx, sc, b.Drivers)
	if err != nil {
		return body, err
	}
	if len(cut) > 0 {
		body.ResourceNote = narrative.ResourceDetailTooLargeNote()
	}
	tagOwners := map[string]bool{}
	anyTag := false
	accounts := map[string]bool{}
	for _, d := range b.Drivers {
		if d.Cause != CauseNewAccount {
			d.Key.ResourceID = topRes[usageKey(d.Key)]
		}
		row := s.bridgeRow(sc, d, incs)
		if d.Cause != CauseNewAccount && cut[resourcePair{d.Key.Account, d.Key.Service}] {
			row.Resources = ResourcesTooLarge
		}
		if row.Owner != nil && row.Owner.Rule == "tag" {
			tagOwners[row.Owner.Name] = true
			anyTag = true
		}
		if row.Account != "" {
			accounts[row.Account] = true
		}
		body.Rows = append(body.Rows, row)
	}
	if b.Other.Lines > 0 || b.Other.Amount != 0 {
		o := BridgeRow{ID: b.Other.ID, Cause: CauseOther, Amount: b.Other.Amount, Lines: b.Other.Lines,
			Label: narrative.DriverLine(narrative.DriverPhrase{Cause: CauseOther, Amount: b.Other.Amount, OffsetsCount: b.Other.Lines}, sc.depth)}
		body.Other = &o
	}
	if b.Steady {
		body.SteadyNote = narrative.SteadyNote(s.cfg.MinDriver, sc.st.Currency)
	} else if cv.Direction == narrative.Same && b.Up != 0 && b.Down != 0 {
		body.OffsetNote = narrative.OffsetNote(b.Up, b.Down, sc.st.Currency)
	}
	tag := sc.st.TeamTag
	switch {
	case tag.Active && tag.Allowlisted && len(tagOwners) >= 2:
		body.OwnerColumn = ColumnRule{Show: true}
	case len(tagOwners) == 1 && anyTag:
		for name := range tagOwners {
			body.OwnerColumn = ColumnRule{Note: "All cost has " + tag.Key + "=" + name}
		}
	default:
		body.OwnerColumn = ColumnRule{Note: "Owner not shown: no team tag on this cost"}
	}
	body.AccountColumn = ColumnRule{Show: len(accounts) >= 2}
	return body, nil
}

func usageKey(k LineKey) LineKey {
	return LineKey{Account: k.Account, Service: k.Service, UsageType: k.UsageType, RecordType: k.RecordType}
}

// ResourcesTooLarge marks a bridge row that names no resource because its
// account and service had more resource lines than one read holds.
const ResourcesTooLarge = "too_large"

// resourcePair is an account and service whose resource lines are read
// together.
type resourcePair struct{ account, service string }

// topResources maps a usage line to its costliest resource in the period,
// when resource-level data exists. It reads the resource lines of each
// driver's account and service only; cut holds the pairs whose lines were
// more than the source reads at once, left without a resource name.
func (s *Service) topResources(ctx context.Context, sc *scope, drivers []Driver) (map[LineKey]string, map[resourcePair]bool, error) {
	out := map[LineKey]string{}
	cut := map[resourcePair]bool{}
	if !sc.st.ResourceData {
		return out, cut, nil
	}
	read := map[resourcePair]bool{}
	cost := map[LineKey]map[string]Micros{}
	for _, d := range drivers {
		p := resourcePair{d.Key.Account, d.Key.Service}
		if d.Cause == CauseNewAccount || read[p] {
			continue
		}
		read[p] = true
		rl, over, err := s.resourceLinesOf(ctx, sc.period.Range, p.account, p.service)
		if err != nil {
			return nil, nil, err
		}
		if over {
			cut[p] = true
		}
		for _, l := range rl {
			if !sc.match(l) || l.Day.After(sc.st.LastCompleteDay) {
				continue
			}
			k := usageKey(l.Key())
			if cost[k] == nil {
				cost[k] = map[string]Micros{}
			}
			cost[k][l.ResourceID] += l.NetAmortizedMicros
		}
	}
	for k, m := range cost {
		best, bestAmt := "", Micros(0)
		for _, id := range sortedKeys(m) {
			if best == "" || m[id] > bestAmt {
				best, bestAmt = id, m[id]
			}
		}
		out[k] = best
	}
	return out, cut, nil
}

func (s *Service) bridgeRow(sc *scope, d Driver, incs []IncreaseRecord) BridgeRow {
	row := BridgeRow{
		ID: d.ID, Cause: d.Cause, Amount: d.Amount, Account: d.Key.Account,
		AccountName: sc.accountName(d.Key.Account), Service: d.Key.Service,
		RecordType: d.Key.RecordType, ResourceID: d.Key.ResourceID, Since: d.Since, Lines: d.Lines,
		Label: narrative.DriverLine(s.phrase(sc, d, incs), sc.depth),
	}
	if d.Key.Service != "" {
		row.ServiceLabel = narrative.ServiceLabel(d.Key.Service, d.ServiceName, sc.depth)
	}
	if sc.depth == narrative.Details {
		row.UsageType = d.Key.UsageType
	}
	if d.Cause != CauseNewAccount {
		o := sc.owners.ResolveShare(d.Key.Account, d.Team, d.Key.ResourceID, d.TeamEstimated, sc.depth)
		row.Owner = &o
	}
	inc := matchIncrease(d, incs)
	if inc != nil {
		row.IncreaseID, row.Status, row.Expected = inc.ID, inc.Status, inc.Expected
	}
	if payerService(d.Key.Service) {
		row.AccountKind = "payer"
	} else if a, ok := sc.accounts[d.Key.Account]; ok {
		row.AccountKind = a.Kind
	}
	if a, ok := sc.accounts[d.Key.Account]; ok && a.Kind == AccountCostOnly {
		row.Why = &WhyView{State: "account_not_connected", Text: "Why: account not connected"}
	} else if inc != nil {
		sv := s.suspectOf(sc, *inc)
		row.Why = &WhyView{State: sv.State, Text: whyText(sv)}
	}
	return row
}

func whyText(sv SuspectView) string {
	if sv.State == "found" && sv.Tied {
		return "Several changes fit; open the increase"
	}
	if sv.State == "found" {
		t := sv.Summary
		if sv.Actor != "" {
			t += " by " + sv.Actor
		}
		return t
	}
	return narrative.SuspectNote(sv.State)
}

// matchIncrease links a usage driver to the latest increase on its line. A
// driver read at page granularity carries no usage type and stands for the
// whole service; it links only when exactly one increase covers that service,
// because two would make the choice a guess.
func matchIncrease(d Driver, incs []IncreaseRecord) *IncreaseRecord {
	if d.Key.RecordType != RecordUsage || d.Cause == CauseNewAccount || d.Cause == CauseOther {
		return nil
	}
	var best *IncreaseRecord
	matches := 0
	for i := range incs {
		k := incs[i].Key
		if k.Account != d.Key.Account || k.Service != d.Key.Service {
			continue
		}
		if d.Key.UsageType != "" && k.UsageType != d.Key.UsageType {
			continue
		}
		matches++
		if best == nil || incs[i].StartedOn.After(best.StartedOn) {
			best = &incs[i]
		}
	}
	if d.Key.UsageType == "" && matches > 1 {
		return nil
	}
	return best
}

// Trend returns the daily series by group, change pins, increase bands and
// the budget pace line.
func (s *Service) Trend(ctx context.Context, q Query) (TrendView, error) {
	sc, err := s.loadFor(ctx, q, viewDims(q))
	if err != nil || !sc.header.Enabled {
		return TrendView{Header: headerOf(sc)}, err
	}
	groupBy := sc.q.GroupBy
	if groupBy == "" || groupBy == GroupCause || groupBy == GroupResource {
		groupBy = GroupService
	}
	v := TrendView{Header: sc.header, GroupBy: groupBy, Series: []TrendSeries{}, Pins: []TrendPin{}, Bands: []TrendBand{}}
	idx := map[Date]int{}
	sc.period.Calendar.Each(func(d Date) {
		if !d.After(sc.period.Range.End) || slices.Contains(sc.header.Sync.PartialDays, d) {
			idx[d] = len(v.Days)
			v.Days = append(v.Days, d)
			if d.After(sc.period.Range.End) {
				v.PartialDays = append(v.PartialDays, d)
			}
		}
	})
	type ser struct {
		label  string
		vals   []Micros
		period Micros
	}
	series := map[string]*ser{}
	v.Totals = make([]Micros, len(v.Days))
	for _, l := range sc.lines {
		i, ok := idx[l.Day]
		if !ok {
			continue
		}
		k, label := sc.groupKey(l, groupBy)
		sr := series[k]
		if sr == nil {
			sr = &ser{label: label, vals: make([]Micros, len(v.Days))}
			series[k] = sr
		}
		sr.vals[i] += l.NetAmortizedMicros
		if !l.Day.After(sc.period.Range.End) {
			sr.period += l.NetAmortizedMicros
		}
		v.Totals[i] += l.NetAmortizedMicros
	}
	keys := sortedKeys(series)
	sort.SliceStable(keys, func(i, j int) bool { return series[keys[i]].period > series[keys[j]].period })
	const maxSeries = 6
	var other *TrendSeries
	for i, k := range keys {
		sr := series[k]
		if i < maxSeries {
			v.Series = append(v.Series, TrendSeries{Key: k, Label: sr.label, Values: sr.vals})
			continue
		}
		if other == nil {
			other = &TrendSeries{Key: otherKey, Label: "Other", Values: make([]Micros, len(v.Days))}
		}
		for j, x := range sr.vals {
			other.Values[j] += x
		}
	}
	if other != nil {
		v.Series = append(v.Series, *other)
	}

	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return v, err
	}
	for _, inc := range incs {
		end := sc.st.LastCompleteDay
		if !inc.ResolvedOn.IsZero() {
			end = inc.ResolvedOn
		}
		if end.Before(sc.period.Range.Start) || inc.StartedOn.After(sc.period.Range.End) {
			continue
		}
		label := narrative.ServiceLabel(inc.Key.Service, inc.ServiceName, sc.depth) + " " +
			narrative.IncreaseTitle(inc.MonthlyImpact, sc.st.Currency, "a month")
		if sc.period.Range.Contains(inc.StartedOn) {
			v.Pins = append(v.Pins, TrendPin{Day: inc.StartedOn, Label: label, IncreaseID: inc.ID})
		}
		start := inc.StartedOn
		if start.Before(sc.period.Range.Start) {
			start = sc.period.Range.Start
		}
		if end.After(sc.period.Range.End) {
			end = sc.period.Range.End
		}
		v.Bands = append(v.Bands, TrendBand{IncreaseID: inc.ID, Start: start, End: end})
	}

	if sc.period.month && !sc.filtered {
		budgets, err := s.allBudgets(ctx, sc)
		if err != nil {
			return v, err
		}
		for _, bg := range budgets {
			if bg.Scope != ScopeDeployment {
				continue
			}
			n := int64(sc.period.Calendar.Days())
			for _, d := range v.Days {
				v.BudgetPace = append(v.BudgetPace, mulDiv(bg.Monthly, int64(d.Day), n))
			}
			break
		}
	}
	return v, nil
}

const otherKey = "_other"

// groupKey returns a line's key and label for a group-by.
func (sc *scope) groupKey(l CostLine, groupBy string) (string, string) {
	switch groupBy {
	case GroupAccount:
		return l.Account, sc.accountName(l.Account)
	case GroupAccountGroup:
		g := sc.accounts[l.Account].Group
		if g == "" {
			g = sc.cfg.AccountGroups[l.Account]
		}
		if g == "" {
			return "_ungrouped", "Ungrouped"
		}
		return g, g
	case GroupRegion:
		if l.Region == UnknownRegion {
			return UnknownRegion, "Region not known"
		}
		if l.Region == "" || l.Region == GlobalRegion {
			return GlobalRegion, "Global"
		}
		return l.Region, l.Region
	case GroupTeam:
		o := sc.ownerOf(l)
		return o.Name, o.Name
	case GroupApplication:
		if l.App == "" {
			return Unallocated, Unallocated
		}
		return l.App, l.App
	case GroupResource:
		if l.ResourceID == "" {
			return otherKey, "Other"
		}
		return l.ResourceID, l.ResourceID
	}
	return l.Service, narrative.ServiceLabel(l.Service, l.ServiceName, sc.depth)
}

// Breakdown returns table rows by group: cost, change, share, owner, plus
// which "spend by" options split the cost meaningfully.
func (s *Service) Breakdown(ctx context.Context, q Query) (BreakdownView, error) {
	sc, err := s.loadFor(ctx, q, viewDims(q))
	if err != nil || !sc.header.Enabled {
		return BreakdownView{Header: headerOf(sc)}, err
	}
	body, err := s.breakdownBody(ctx, sc, sc.q.GroupBy)
	return BreakdownView{Header: sc.header, BreakdownBody: body}, err
}

// breakdownRule: an option splits the cost when it has two or more values
// and the largest is under 95% of the period cost.
const breakdownDominantBP = 9_500

func (s *Service) breakdownBody(ctx context.Context, sc *scope, groupBy string) (BreakdownBody, error) {
	if groupBy == "" {
		groupBy = GroupService
	}
	body := BreakdownBody{GroupBy: groupBy, Rows: []BreakdownRow{}}
	body.Options, body.DefaultGroupBy = s.breakdownOptions(sc)
	body.Total = sc.periodTotal(nil)

	if groupBy == GroupCause {
		b := sc.getBridge()
		agg := map[string]Micros{}
		for _, d := range b.Drivers {
			agg[d.Cause] += d.Amount
		}
		if b.Other.Amount != 0 {
			agg[CauseOther] += b.Other.Amount
		}
		for _, c := range sortedKeys(agg) {
			body.Rows = append(body.Rows, BreakdownRow{Key: c, Label: narrative.CauseLabel(c, sign(agg[c]), "", sc.depth),
				Amount: agg[c], ShareBP: basisPoints(agg[c], b.Change)})
		}
		sort.SliceStable(body.Rows, func(i, j int) bool { return absMicros(body.Rows[i].Amount) > absMicros(body.Rows[j].Amount) })
		return body, nil
	}

	lines := sc.complete
	if groupBy == GroupResource {
		if !sc.st.ResourceData {
			return body, nil
		}
		rl, err := s.src.ResourceLines(ctx, sc.period.Range)
		if err != nil {
			return body, err
		}
		lines = nil
		for _, l := range rl {
			if sc.match(l) && !l.Day.After(sc.st.LastCompleteDay) {
				lines = append(lines, l)
			}
		}
	}
	type agg struct {
		label     string
		amt, base Micros
		owner     *Owner
		inside    map[string]Micros
		estimated bool
	}
	rows := map[string]*agg{}
	for _, l := range lines {
		inP, inB := sc.period.Range.Contains(l.Day), sc.comp.Available && sc.comp.Range.Contains(l.Day)
		if !inP && !inB {
			continue
		}
		k, label := sc.groupKey(l, groupBy)
		a := rows[k]
		if a == nil {
			a = &agg{label: label, inside: map[string]Micros{}}
			rows[k] = a
		}
		if inP {
			a.amt += l.NetAmortizedMicros
			ug := l.UsageGroup
			if ug == "" || sc.depth == narrative.Details {
				ug = l.UsageType
			}
			a.inside[ug] += l.NetAmortizedMicros
		}
		if inB {
			a.base += l.NetAmortizedMicros
		}
		if groupBy == GroupTeam && a.owner == nil {
			o := sc.ownerOf(l)
			a.owner = &o
		}
		a.estimated = a.estimated || sc.rowEstimated(l, groupBy)
	}
	keys := sortedKeys(rows)
	sort.SliceStable(keys, func(i, j int) bool { return rows[keys[i]].amt > rows[keys[j]].amt })
	limit := 10
	switch groupBy {
	case GroupService:
		limit = 6
	case GroupAccount:
		limit = 8
	}
	if len(keys) == limit+1 {
		// One leftover value reads better as itself than as "1 other".
		limit++
	}
	var other *BreakdownRow
	for i, k := range keys {
		a := rows[k]
		if a.amt == 0 && a.base == 0 {
			continue
		}
		if i >= limit {
			if other == nil {
				other = &BreakdownRow{Key: otherKey, Label: "Other"}
				if sc.comp.Available {
					other.Base, other.Change = new(Micros), new(Micros)
				}
			}
			other.Amount += a.amt
			other.Count++
			if other.Base != nil {
				*other.Base += a.base
				*other.Change += a.amt - a.base
			}
			continue
		}
		if a.owner != nil {
			a.owner.Estimated = a.estimated
		}
		r := BreakdownRow{Key: k, Label: a.label, Amount: a.amt, ShareBP: basisPoints(a.amt, body.Total), Owner: a.owner, Estimated: a.estimated}
		if sc.comp.Available {
			base, ch := a.base, a.amt-a.base
			r.Base, r.Change = &base, &ch
		}
		if groupBy == GroupService && body.DefaultGroupBy == "" {
			r.Inside = insideRows(a.inside, a.amt)
		}
		body.Rows = append(body.Rows, r)
	}
	if other != nil {
		other.ShareBP = basisPoints(other.Amount, body.Total)
		body.Rows = append(body.Rows, *other)
	}
	return body, nil
}

func insideRows(m map[string]Micros, total Micros) []BreakdownRow {
	keys := sortedKeys(m)
	sort.SliceStable(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	var out []BreakdownRow
	var rest Micros
	for i, k := range keys {
		if i < 3 {
			label := k
			if label == "" {
				label = "Other"
			}
			out = append(out, BreakdownRow{Key: k, Label: label, Amount: m[k], ShareBP: basisPoints(m[k], total)})
			continue
		}
		rest += m[k]
	}
	if rest != 0 {
		out = append(out, BreakdownRow{Key: otherKey, Label: "Other", Amount: rest, ShareBP: basisPoints(rest, total)})
	}
	return out
}

// breakdownOptions applies the one rule for every "spend by" option and
// picks the default.
func (s *Service) breakdownOptions(sc *scope) ([]BreakdownOption, string) {
	total := sc.periodTotal(nil)
	values := func(groupBy string) map[string]Micros {
		m := map[string]Micros{}
		for _, l := range sc.complete {
			if sc.period.Range.Contains(l.Day) {
				k, _ := sc.groupKey(l, groupBy)
				m[k] += l.NetAmortizedMicros
			}
		}
		return m
	}
	judge := func(groupBy string) BreakdownOption {
		o := BreakdownOption{GroupBy: groupBy}
		m := values(groupBy)
		if len(m) < 2 {
			o.Reason = "single_value"
			return o
		}
		var top Micros
		for _, v := range m {
			top = max(top, v)
		}
		if total > 0 && basisPoints(top, total) >= breakdownDominantBP {
			o.Reason = "dominant_value"
			return o
		}
		o.Available = true
		return o
	}
	acct := judge(GroupAccount)
	region := judge(GroupRegion)
	team := BreakdownOption{GroupBy: GroupTeam}
	tag := sc.st.TeamTag
	tagged := map[string]bool{}
	for _, l := range sc.complete {
		if l.Team != "" && sc.period.Range.Contains(l.Day) {
			tagged[l.Team] = true
		}
	}
	switch {
	case !tag.Active:
		team.Reason = "tag_not_active"
		team.Note = "Activate " + tag.Key + " as a cost allocation tag; spend shows as Unallocated until then"
	case !tag.Allowlisted || len(tagged) == 0:
		team.Reason = "no_team_tag"
		team.Note = "No " + tag.Key + " tag on this cost"
	case len(tagged) == 1:
		team.Reason = "same_tag"
		for v := range tagged {
			team.Note = "All cost has " + tag.Key + "=" + v
		}
	default:
		team = judge(GroupTeam)
	}
	opts := []BreakdownOption{acct, region, team}
	groupsSet := len(s.cfg.AccountGroups) > 0
	grp := BreakdownOption{GroupBy: GroupAccountGroup, Reason: "no_groups"}
	if groupsSet {
		grp = judge(GroupAccountGroup)
	}
	opts = append([]BreakdownOption{grp}, opts...)

	def := ""
	switch {
	case grp.Available && len(sc.accounts) > 8:
		def = GroupAccountGroup
	case acct.Available:
		def = GroupAccount
	case region.Available:
		def = GroupRegion
	case team.Available:
		def = GroupTeam
	}
	return opts, def
}

// rowEstimated reports whether a line makes its breakdown row an estimate:
// a region row or region filter over an estimated region share, or a team
// cut over an estimated team share that is not a whole-account sum.
func (sc *scope) rowEstimated(l CostLine, groupBy string) bool {
	if l.EstimatedRegion && (groupBy == GroupRegion || len(sc.q.Regions) > 0) {
		return true
	}
	if !l.EstimatedTeam || (groupBy != GroupTeam && len(sc.q.Teams) == 0) {
		return false
	}
	wholeAccounts := groupBy == GroupTeam || groupBy == GroupAccount || groupBy == GroupAccountGroup
	return !(sc.st.TeamExactPerAccount && wholeAccounts)
}

func sign(v Micros) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
