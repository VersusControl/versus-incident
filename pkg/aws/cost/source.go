package cost

import (
	"context"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Source implements finops.CostSource over the cache. No method calls AWS
// or takes from the meter, so the FinOps service and every agent tool built
// on it spend no paid request.
type Source struct {
	adapter *Adapter
	cache   cache
	meter   *Meter
}

func (s *Source) options() Options { return s.adapter.options() }

var _ finops.CostSource = (*Source)(nil)

// unreadable codes mean the payer's data cannot be read at all; throttling
// and timeouts only make the data stale.
func unreadable(code string) bool {
	return code == CodeDenied || code == CodeUnavailable || code == CodeDataNotReady
}

// payers are the payers whose stored cost is read: the reachable ones and
// those that cannot be read now (their stored cost stays visible, stale).
// Reads never use their credentials. A payer removed at a revision the
// options are not newer than is left out at once, whichever replica
// removed it, so its cost is not shown while the purge is pending.
func (s *Source) payers() []Payer {
	st, err := s.cache.status()
	if err != nil {
		st = nil
	}
	removed := s.adapter.removedPayers(st)
	ps := make([]Payer, 0, len(s.options().Payers)+len(s.options().Unreachable))
	for _, p := range s.options().Payers {
		if !removed[p.AccountID] {
			ps = append(ps, p)
		}
	}
	for _, u := range s.options().Unreachable {
		if !removed[u.AccountID] {
			ps = append(ps, Payer{AccountID: u.AccountID, Organization: u.Organization})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].AccountID < ps[j].AccountID })
	return ps
}

// Status describes the cache: freshness per payer, accounts and gaps.
func (s *Source) Status(ctx context.Context) (finops.SourceStatus, error) {
	out := finops.SourceStatus{Enabled: s.options().enabled()}
	disabledReason := ReasonCostDataOff
	if r := s.options().DisabledReason; r != "" {
		disabledReason = r
	}
	// Cost data on but no payer reachable: the stored cost of the payers
	// that lost access is still served, marked stale. Only when none of
	// them ever stored cost does the page say why it is empty.
	readStored := !out.Enabled && disabledReason != ReasonCostDataOff && len(s.options().Unreachable) > 0
	if !out.Enabled && !readStored {
		out.DisabledReason = disabledReason
		return out, nil
	}
	st, err := s.cache.status()
	if err != nil {
		return out, err
	}
	if readStored {
		stored := false
		removed := s.adapter.removedPayers(st)
		for _, u := range s.options().Unreachable {
			if ps := st.Payers[u.AccountID]; ps != nil && ps.hasData() && !removed[u.AccountID] {
				stored = true
			}
		}
		if !stored {
			out.DisabledReason = disabledReason
			return out, nil
		}
		out.Enabled = true
	}
	out.UpdatedAt = st.UpdatedAt
	out.SkippedCalls = append([]string(nil), st.SkippedCalls...)
	out.MeterUsed, out.MeterCap = s.meter.Used(ctx), s.meter.Cap()
	out.TeamTag = finops.TagStatus{Key: s.options().TeamTagKey, Allowlisted: s.options().TagAllowlisted}
	out.NameTagAllowlisted = s.options().NameAllowlisted
	// Team tables are per account (Organizations) or per service of one
	// account, so team totals per account are exact in both modes.
	out.TeamExactPerAccount = true

	tagKnown, tagActive := false, true
	var forecasts []*finops.ProviderForecast
	var currencies []string
	withData, failedCodes := 0, []string{}
	seen := map[string]bool{}
	for _, p := range s.payers() {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			ps = &payerState{}
		}
		readable := ps.hasData() && !unreadable(ps.ErrorCode)
		if !ps.hasData() && ps.ErrorCode != "" {
			failedCodes = append(failedCodes, ps.ErrorCode)
		}
		if readable {
			withData++
			if out.LastCompleteDay.IsZero() || (!ps.LastCompleteDay.IsZero() && ps.LastCompleteDay.Before(out.LastCompleteDay)) {
				out.LastCompleteDay = ps.LastCompleteDay
			}
			if out.HistoryStart.IsZero() || ps.HistoryStart.Before(out.HistoryStart) {
				out.HistoryStart = ps.HistoryStart
			}
			forecasts = append(forecasts, ps.Forecast)
			currencies = append(currencies, ps.Currency)
			out.ResourceData = out.ResourceData || ps.ResourceData
		}
		if ps.Stale && !out.Stale {
			out.Stale, out.StaleReason = true, ps.ErrorCode
		}
		if ps.TagActive != nil {
			tagKnown = true
			tagActive = tagActive && *ps.TagActive
		}
		out.Budgets = append(out.Budgets, ps.Budgets...)
		out.SkippedBudgets = append(out.SkippedBudgets, ps.SkippedBudgets...)
		ids := []string{p.AccountID}
		for id := range ps.Accounts {
			if id != p.AccountID {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids[1:])
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			a := finops.Account{ID: id, Kind: finops.AccountCostOnly, Readable: readable, Owner: s.options().AccountOwners[id]}
			if s.options().connected(id) || (id == p.AccountID && !p.Organization) {
				a.Kind = finops.AccountConnected
			}
			if as := ps.Accounts[id]; as != nil {
				a.Name, a.FirstCost = as.Name, as.FirstCost
			}
			if n := s.options().AccountNames[id]; n != "" {
				a.Name = n
			}
			if !readable {
				a.ErrorCode = ps.ErrorCode
			}
			out.Accounts = append(out.Accounts, a)
		}
	}
	out.TeamTag.Active = tagKnown && tagActive
	s.markUnreachable(st, &out)
	s.describeSource(st, &out)
	if len(currencies) > 0 {
		out.Currency = currencies[0]
	}
	if withData == 0 && len(failedCodes) > 0 && len(failedCodes) == len(s.options().Payers) {
		// Nothing was ever readable: the page shows one explanation.
		out.Enabled, out.DisabledReason = false, failedCodes[0]
		return out, nil
	}
	out.Forecast = sumForecasts(forecasts, currencies)
	return out, nil
}

// sumForecasts adds the payers' forecasts when every readable payer has one
// for the same month in the same currency; otherwise the FinOps core falls
// back to its labelled estimate.
func sumForecasts(fs []*finops.ProviderForecast, currencies []string) *finops.ProviderForecast {
	if len(fs) == 0 {
		return nil
	}
	var out finops.ProviderForecast
	for i, f := range fs {
		if f == nil || (i > 0 && (f.Month != fs[0].Month || currencies[i] != currencies[0])) {
			return nil
		}
		out.Month = f.Month
		out.Mean += f.Mean
		out.Low += f.Low
		out.High += f.High
	}
	return &out
}

// payerLines reads every configured payer's stored rows for the range and
// hands each payer's rows, in stored order, to fn.
func (s *Source) payerLines(ctx context.Context, r finops.DateRange, query storage.FinOpsCostQuery, fn func(p Payer, organization bool, rows []storage.FinOpsCostLine)) error {
	if r.Days() == 0 {
		return nil
	}
	store, err := s.cache.tables()
	if err != nil {
		return err
	}
	st, err := s.cache.status()
	if err != nil {
		return err
	}
	query.From, query.Through = r.Start.String(), r.End.String()
	rows, err := store.FinOpsCostLines(ctx, s.cache.scope, query)
	if err != nil {
		return err
	}
	byPayer := map[string][]storage.FinOpsCostLine{}
	for _, row := range rows {
		byPayer[row.Payer] = append(byPayer[row.Payer], row)
	}
	for _, p := range s.payers() {
		if len(byPayer[p.AccountID]) > 0 {
			fn(p, s.organization(st, p), byPayer[p.AccountID])
		}
	}
	return nil
}

func (s *Source) organization(st *statusRecord, p Payer) bool {
	if ps := st.Payers[p.AccountID]; ps != nil && ps.Organization {
		return true
	}
	return p.Organization
}

// viewDays returns every configured payer's lines of the range as views
// read them: one store read per call, bounded by Options.LineBudget. A
// period holding the whole budget of rows is refused rather than answered
// from a page, so a truncated sum can never pass for the whole.
func (s *Source) viewDays(ctx context.Context, r finops.DateRange) ([]payerDay, error) {
	if r.Days() == 0 {
		return nil, nil
	}
	st, err := s.cache.status()
	if err != nil {
		return nil, err
	}
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	allowlisted := s.options().TagAllowlisted
	organizations := map[string]bool{}
	configured := map[string]bool{}
	for _, p := range s.payers() {
		configured[p.AccountID] = true
		if s.organization(st, p) {
			organizations[p.AccountID] = true
		}
	}
	budget := s.options().LineBudget
	rows, err := store.FinOpsCostLines(ctx, s.cache.scope, storage.FinOpsCostQuery{
		From: r.Start.String(), Through: r.End.String(), Limit: budget,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) >= budget {
		return nil, ErrCostLinesBudgetExceeded
	}
	byDay := map[finops.Date][]payerDay{}
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].Day == rows[start].Day && rows[end].Payer == rows[start].Payer {
			end++
		}
		if configured[rows[start].Payer] {
			day, err := finops.ParseDate(rows[start].Day)
			if err != nil {
				return nil, err
			}
			byDay[day] = append(byDay[day], viewLines(rows[start].Payer, rows[start:end], organizations[rows[start].Payer], allowlisted))
		}
		start = end
	}
	var out []payerDay
	for d := r.Start; !d.After(r.End); d = d.AddDays(1) {
		out = append(out, byDay[d]...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].payer < out[j].payer })
	return out, nil
}

// viewLines turns one payer's stored rows of a day into the lines views
// read: an Organization's drilled pairs as one line per region and team,
// team values hidden unless the tag is allowlisted, and resource-level
// refinements apart (single accounts only).
func viewLines(payer string, all []storage.FinOpsCostLine, organization, allowlisted bool) payerDay {
	pd := payerDay{payer: payer}
	rows := make([]storage.FinOpsCostLine, 0, len(all))
	for _, row := range all {
		switch {
		case row.ResourceID == "":
			rows = append(rows, row)
		case !organization:
			pd.resources = append(pd.resources, lineOf(row))
		}
	}
	if organization {
		pd.lines = foldDrills(rows)
	} else {
		pd.lines = make([]finops.CostLine, 0, len(rows))
		for _, row := range rows {
			pd.lines = append(pd.lines, lineOf(row))
		}
	}
	if !allowlisted {
		pd.lines = withoutTeams(pd.lines)
	}
	return pd
}

// Lines returns the stored daily lines, partial days included. A period
// holding more stored lines than Options.LineBudget is refused with
// ErrCostLinesBudgetExceeded rather than answered from a partial set.
func (s *Source) Lines(ctx context.Context, r finops.DateRange) ([]finops.CostLine, error) {
	days, err := s.viewDays(ctx, r)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, pd := range days {
		n += len(pd.lines)
	}
	out := make([]finops.CostLine, 0, n)
	for _, pd := range days {
		out = append(out, pd.lines...)
	}
	return out, nil
}

// ResourceLines returns resource-level refinements; empty when resource
// data is off. They come from the same bounded read as Lines, so a period
// over the line budget is refused here too.
func (s *Source) ResourceLines(ctx context.Context, r finops.DateRange) ([]finops.CostLine, error) {
	days, err := s.viewDays(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []finops.CostLine
	for _, pd := range days {
		out = append(out, pd.resources...)
	}
	return out, nil
}

var _ finops.ResourceLinesOfSource = (*Source)(nil)

// ResourceLinesOf returns the resource-level lines of one account and
// service, read narrowed in the store: the line budget then counts that
// pair's stored lines, not the period's. Like ResourceLines it answers for
// the configured payers that are not an Organization, whose resource
// detail is folded away, and refuses an over-budget read with
// ErrCostLinesBudgetExceeded.
func (s *Source) ResourceLinesOf(ctx context.Context, r finops.DateRange, account, service string) ([]finops.CostLine, error) {
	if r.Days() == 0 {
		return nil, nil
	}
	st, err := s.cache.status()
	if err != nil {
		return nil, err
	}
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	resourceLevel := map[string]bool{}
	for _, p := range s.payers() {
		resourceLevel[p.AccountID] = !s.organization(st, p)
	}
	budget := s.options().LineBudget
	rows, err := store.FinOpsCostLines(ctx, s.cache.scope, storage.FinOpsCostQuery{
		From: r.Start.String(), Through: r.End.String(), Account: account, Service: service, Limit: budget,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) >= budget {
		return nil, ErrCostLinesBudgetExceeded
	}
	var out []finops.CostLine
	for _, row := range rows {
		if row.ResourceID != "" && resourceLevel[row.Payer] {
			out = append(out, lineOf(row))
		}
	}
	return out, nil
}

// UsageDetail returns the stored usage-type detail of one account and AWS
// service in an Organization (from the daily movers or an on-click
// drill-down). ok is false when no detail is stored for the range, so the
// caller can offer the paid drill-down instead of showing nothing.
func (s *Source) UsageDetail(ctx context.Context, account, awsService string, r finops.DateRange) (lines []finops.CostLine, ok bool, err error) {
	keep := s.options().TagAllowlisted
	err = s.payerLines(ctx, r, storage.FinOpsCostQuery{Account: account, RecordType: finops.RecordUsage, ResourceID: noResource},
		func(_ Payer, organization bool, rows []storage.FinOpsCostLine) {
			if !organization {
				return
			}
			for _, row := range rows {
				if row.ServiceName != awsService || !isDrillRow(row) {
					continue
				}
				ok = true
				l := lineOf(row)
				l.Service, l.UsageGroup = ServiceKey(awsService, row.UsageType)
				if !keep {
					l.Team = ""
				}
				lines = append(lines, l)
			}
		})
	if err != nil {
		return nil, false, err
	}
	// Detail still waiting for its day's tables.
	st, err := s.cache.status()
	if err != nil {
		return nil, false, err
	}
	pair := pairKey(account, awsService)
	for _, p := range s.payers() {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			continue
		}
		for _, day := range sortedKeys(ps.PendingDrill) {
			d, perr := finops.ParseDate(day)
			cells, found := ps.PendingDrill[day][pair]
			if perr != nil || !found || !r.Contains(d) {
				continue
			}
			ok = true
			lines = append(lines, buildDrillLines(d, account, awsService, cells, synthOptions{payer: p.AccountID, currency: ps.Currency, keepTeams: keep})...)
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Day.Before(lines[j].Day) })
	return lines, ok, nil
}

// SavingsInputs returns the rightsizing recommendations and discount
// coverage the cache holds, plus the billed idle and network facts read from
// the last 30 days of lines. A source with store totals answers those facts
// from totals instead (SavingsInputsFromTotals); this line scan is the
// fallback and the shape every number must match.
func (s *Source) SavingsInputs(ctx context.Context) (finops.SavingsInputs, error) {
	st, err := s.cache.status()
	if err != nil {
		return finops.SavingsInputs{}, err
	}
	in := s.savingsFacts(st)
	if st.UpdatedAt.IsZero() {
		return in, nil
	}
	end := completeThrough(s.options().Now(), DaySettle)
	lines, err := s.Lines(ctx, finops.DateRange{Start: end.AddDays(-29), End: end})
	if err != nil {
		return in, err
	}
	return s.addBilledSavings(ctx, in, billedFromLines(lines))
}

// savingsFacts is the status-derived half of the savings inputs:
// recommendations, coverage, commitment and the gaps the refresh state alone
// explains.
func (s *Source) savingsFacts(st *statusRecord) finops.SavingsInputs {
	in := finops.SavingsInputs{IdleGuides: idleGuides}
	var cv coverage
	sources := 0
	for _, p := range s.payers() {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			continue
		}
		in.Recommendations = append(in.Recommendations, ps.Recommendations...)
		in.Idle = append(in.Idle, ps.IdleFound...)
		if ps.Coverage != nil {
			cv.Covered += ps.Coverage.Covered
			cv.Total += ps.Coverage.Total
		}
		if ps.CommitmentMonthly > 0 {
			if in.Coverage == nil {
				in.Coverage = &finops.Coverage{}
			}
			in.Coverage.EstimatedMonthly += ps.CommitmentMonthly
			in.Coverage.Suggestion = ps.CommitmentSuggestion
		}
		if ps.RecommendationSource != "" {
			sources++
		}
	}
	if cv.Total > 0 {
		if in.Coverage == nil {
			in.Coverage = &finops.Coverage{}
		}
		in.Coverage.CoveredBP = coveredBP(cv)
		in.Coverage.Guide = commitmentGuide()
	} else if in.Coverage != nil {
		in.Coverage = nil
	}
	if sources == 0 {
		in.Gaps = append(in.Gaps, finops.SavingsGap{Check: finops.SavingRightsize, Reason: finops.GapProviderOff},
			finops.SavingsGap{Check: finops.IdleUnattachedVolume, Reason: finops.GapProviderOff})
	}
	s.hideSavingsTagValues(&in)
	return in
}

// hideSavingsTagValues blanks every tag value a savings answer carries when
// its tag is not allowlisted: team labels for the team tag, resource names
// for the Name tag. The cached facts were captured while a tag may have been
// allowlisted, so narrowing it must stop the stored value from being served
// without waiting for the next refresh; one helper covers every tag-bearing
// field so a new one cannot leak. Resource IDs cached as ARNs are cut to the
// resource part so no account ID reaches a title.
func (s *Source) hideSavingsTagValues(in *finops.SavingsInputs) {
	for i := range in.Recommendations {
		in.Recommendations[i].ResourceID = resourcePart(in.Recommendations[i].ResourceID)
		if !s.options().NameAllowlisted {
			in.Recommendations[i].Name = ""
		}
	}
	for i := range in.Idle {
		in.Idle[i].ID = resourcePart(in.Idle[i].ID)
	}
	if s.options().TagAllowlisted {
		return
	}
	for i := range in.Recommendations {
		in.Recommendations[i].Team = ""
	}
	for i := range in.Idle {
		in.Idle[i].Team = ""
	}
	for i := range in.NetworkPaths {
		in.NetworkPaths[i].Team = ""
	}
	for i := range in.Buckets {
		in.Buckets[i].Team = ""
	}
}

// addBilledSavings adds the idle and network facts the billed usage yields
// and names each check once, the first and most specific reason winning.
func (s *Source) addBilledSavings(ctx context.Context, in finops.SavingsInputs, billed billedUsage) (finops.SavingsInputs, error) {
	idle, paths, gaps, err := s.inventorySavings(ctx, billed)
	if err != nil {
		return in, err
	}
	in.Idle = append(in.Idle, idle...)
	in.NetworkPaths = append(in.NetworkPaths, paths...)
	named := map[string]bool{}
	for _, g := range in.Gaps {
		named[g.Check] = true
	}
	for _, g := range gaps {
		if !named[g.Check] {
			named[g.Check] = true
			in.Gaps = append(in.Gaps, g)
		}
	}
	s.hideSavingsTagValues(&in)
	return in, nil
}

func coveredBP(c coverage) int64 {
	if c.Total <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(c.Covered), big.NewInt(10_000))
	n.Add(n, big.NewInt(c.Total/2))
	return n.Quo(n, big.NewInt(c.Total)).Int64()
}

// markUnreachable marks the stored cost of a payer that cannot be read now
// as stale, with its cause and since when: the cause outranks a refresh
// error, since no refresh can run for that payer until access is back.
func (s *Source) markUnreachable(st *statusRecord, out *finops.SourceStatus) {
	removed := s.adapter.removedPayers(st)
	for _, u := range s.options().Unreachable {
		ps := st.Payers[u.AccountID]
		if ps == nil || !ps.hasData() || removed[u.AccountID] {
			continue
		}
		// Stale since the last good refresh; the loss of access when none
		// is recorded.
		since := ps.LastSuccess
		if since.IsZero() {
			since = u.Since
		}
		if !out.Stale || out.StaleReason == "" || isRefreshCode(out.StaleReason) {
			out.Stale, out.StaleReason = true, u.Cause
		}
		if !since.IsZero() && (out.StaleSince.IsZero() || since.Before(out.StaleSince)) {
			out.StaleSince = since
		}
	}
}

// isRefreshCode reports a stale reason that only describes a refresh error.
func isRefreshCode(code string) bool { return slices.Contains(Codes, code) }

// maxStaleDays bounds the list of settled days not loaded yet.
const maxStaleDays = 31

// describeSource names the cost source, the next scheduled refresh and,
// for a stale source, since when and which settled days are missing.
func (s *Source) describeSource(st *statusRecord, out *finops.SourceStatus) {
	o := s.options()
	payers := s.payers()
	switch {
	case len(payers) == 1 && payers[0].Organization:
		out.CostSource = &finops.CostSourceView{AccountID: payers[0].AccountID, Kind: "management", AccountName: s.accountName(st, payers[0].AccountID)}
	case len(payers) == 1:
		out.CostSource = &finops.CostSourceView{AccountID: payers[0].AccountID, Kind: "account", AccountName: s.accountName(st, payers[0].AccountID)}
	case len(payers) > 1:
		out.CostSource = &finops.CostSourceView{Kind: "account", Count: len(payers)}
	}
	now := o.Now()
	if o.RefreshSet {
		local := now.In(o.Location)
		next := time.Date(local.Year(), local.Month(), local.Day(), o.RefreshHour, o.RefreshMinute, 0, 0, o.Location)
		if st.ScheduledOn == finops.DateOf(local) {
			next = next.AddDate(0, 0, 1)
		}
		out.NextRefreshAt = next
	}
	if !out.Stale {
		return
	}
	complete := completeThrough(now, DaySettle)
	unreachable := map[string]bool{}
	for _, u := range o.Unreachable {
		unreachable[u.AccountID] = true
	}
	for _, p := range payers {
		ps := st.Payers[p.AccountID]
		if ps == nil || (!ps.Stale && !unreachable[p.AccountID]) {
			continue
		}
		since := ps.LastSuccess
		if since.IsZero() {
			since = ps.LastAttempt
		}
		if out.StaleSince.IsZero() || (!since.IsZero() && since.Before(out.StaleSince)) {
			out.StaleSince = since
		}
		from := ps.LastCompleteDay.AddDays(1)
		if ps.LastCompleteDay.IsZero() {
			from = complete.AddDays(-dailyLookbackDays)
		}
		for d := from; !d.After(complete) && len(out.StaleDays) < maxStaleDays; d = d.AddDays(1) {
			if !slices.Contains(out.StaleDays, d) {
				out.StaleDays = append(out.StaleDays, d)
			}
		}
	}
	slices.SortFunc(out.StaleDays, func(a, b finops.Date) int { return a.Compare(b) })
}

// accountName prefers the configured name, then Cost Explorer's.
func (s *Source) accountName(st *statusRecord, id string) string {
	if n := s.options().AccountNames[id]; n != "" {
		return n
	}
	for _, ps := range st.Payers {
		if a := ps.Accounts[id]; a != nil && a.Name != "" {
			return a.Name
		}
	}
	return ""
}
