package finops

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// CostSource is the provider adapter's cache. Every method reads cached
// data only; none of them may call a paid billing API, so the Service is
// safe to expose as a read-only agent tool.
type CostSource interface {
	// Status describes the cache: enablement, freshness, accounts and gaps.
	Status(ctx context.Context) (SourceStatus, error)
	// Lines returns the cached daily lines at usage-type level (no
	// resource IDs) for the inclusive range. It may include partial days
	// after the last complete day.
	Lines(ctx context.Context, r DateRange) ([]CostLine, error)
	// ResourceLines returns optional resource-level lines for the range;
	// empty when resource-level data is off. They refine a usage type and
	// are never added to totals.
	ResourceLines(ctx context.Context, r DateRange) ([]CostLine, error)
	// SavingsInputs returns the neutral facts the savings rules read. A
	// source that sums in the store implements SavingsFactsSource (or the
	// older SavingsTotalsSource) instead, so the facts cost no line read.
	SavingsInputs(ctx context.Context) (SavingsInputs, error)
}

// ResourceLinesOfSource is an optional CostSource half that reads the
// resource-level lines of one account and service, so a view naming the
// resource behind one driver reads that pair's lines, not the period's.
type ResourceLinesOfSource interface {
	ResourceLinesOf(ctx context.Context, r DateRange, account, service string) ([]CostLine, error)
}

// SourceStatus is the cache's state.
type SourceStatus struct {
	Enabled bool
	// DisabledReason is a closed code ("cost_data_off", "unavailable").
	DisabledReason  string
	Currency        string
	LastCompleteDay Date
	// HistoryStart is the first day with data; zero when none.
	HistoryStart Date
	UpdatedAt    time.Time
	Stale        bool
	StaleReason  string
	Accounts     []Account
	SkippedCalls []string
	MeterUsed    int
	MeterCap     int
	Forecast     *ProviderForecast
	TeamTag      TagStatus
	// NameTagAllowlisted reports that the Name tag's values may be shown;
	// when false a frozen report names a rightsized resource by its ID.
	NameTagAllowlisted bool
	// TeamExactPerAccount reports that team totals per account are exact
	// even where a line's team share is estimated: sums over whole
	// accounts (top team, team budgets, spend by team) are then facts.
	TeamExactPerAccount bool
	ResourceData        bool
	// CostSource is the account cost is read through.
	CostSource *CostSourceView
	// NextRefreshAt is the next scheduled refresh; zero when none.
	NextRefreshAt time.Time
	// StaleSince and StaleDays detail a stale source.
	StaleSince time.Time
	StaleDays  []Date
	// Budgets read from the provider (read-only).
	Budgets []Budget
	// SkippedBudgets are provider budgets the page cannot show as-is.
	SkippedBudgets []SkippedBudget
}

// Store keeps the decisions users make: increase status, dismissed
// savings, UI budgets and frozen reports. Implementations persist through
// the storage provider.
type Store interface {
	ListIncreases(ctx context.Context) ([]IncreaseRecord, error)
	PutIncrease(ctx context.Context, rec IncreaseRecord) error
	SavingStates(ctx context.Context) (map[string]SavingState, error)
	PutSavingState(ctx context.Context, id string, st SavingState) error
	// SyncSavingStates marks the decisions whose item is not in present as
	// orphaned at the time, keeping an earlier mark, and clears the mark of
	// those whose item is present again. An orphaned decision stays stored
	// for audit until retention removes it. A decision stored without what
	// it refers to gets it from its present item.
	SyncSavingStates(ctx context.Context, present []Saving, at time.Time) error
	Budgets(ctx context.Context) ([]Budget, error)
	PutBudgets(ctx context.Context, bs []Budget) error
	BudgetAlertStates(ctx context.Context) (map[string]BudgetAlertState, error)
	PutBudgetAlertState(ctx context.Context, id string, st BudgetAlertState) error
	GetReport(ctx context.Context, month string) (*ReportView, error)
	PutReport(ctx context.Context, r ReportView) error
	ListReports(ctx context.Context) ([]ReportRef, error)
}

// SuspectFinder finds the change that likely started an increase. It is
// optional; without it increases say change history is not collected yet.
// It returns state "none_found" only when the change history covers the
// whole window; a window with a gap is "history_incomplete", never a guess.
type SuspectFinder interface {
	Suspect(ctx context.Context, d Detected) (SuspectView, *UsageSignal, error)
}

// SuspectRanker is an optional SuspectFinder that ranks up to three
// candidate changes, best first.
type SuspectRanker interface {
	Suspects(ctx context.Context, d Detected) ([]SuspectView, *UsageSignal, error)
}

// MaxSuspects bounds the ranked candidates an increase shows.
const MaxSuspects = 3

// Config is the per-deployment FinOps configuration. Callers build it from
// the cloned request config; the Service never mutates it.
type Config struct {
	MinIncrease      Micros
	MinDriver        Micros
	TargetCoverageBP int64
	MaxPaidRequests  int
	// Budgets from config (read-only in the UI).
	Budgets       []Budget
	AccountOwners map[string]string
	AccountGroups map[string]string
	// ProviderName is used in notes ("AWS closes the bill on the 5th").
	ProviderName string
	// UIPath is the page path for links ("/aws/finops").
	UIPath string
	// BillCloseDay is the day of the next month the bill closes (5).
	BillCloseDay int
	// Location is the deployment time zone for "today".
	Location *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// detectionLookbackDays bounds the history SyncIncreases walks for
// increases. Older increases that never resolved stay in the store.
const detectionLookbackDays = 180

// Service is the one core method set behind the FinOps routes and tools.
type Service struct {
	src CostSource
	// totals is the source's store-side summing half when it has one; nil
	// keeps every view on the line read.
	totals  TotalsSource
	store   Store
	cfg     Config
	suspect SuspectFinder
}

// NewService builds a Service. store may be nil for read-only use (no
// decisions are then overlaid and writes fail).
func NewService(src CostSource, store Store, cfg Config) *Service {
	if cfg.MinDriver <= 0 {
		cfg.MinDriver = DefaultMinDriver
	}
	if cfg.MinIncrease <= 0 {
		cfg.MinIncrease = DefaultMinIncrease
	}
	if cfg.TargetCoverageBP <= 0 {
		cfg.TargetCoverageBP = 8_000
	}
	if cfg.MaxPaidRequests <= 0 {
		cfg.MaxPaidRequests = DefaultMaxPaidRequests
	}
	if cfg.BillCloseDay <= 0 {
		cfg.BillCloseDay = 5
	}
	if cfg.UIPath == "" {
		cfg.UIPath = "/finops"
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	s := &Service{src: src, store: store, cfg: cfg}
	if ts, ok := src.(TotalsSource); ok {
		s.totals = ts
	}
	return s
}

// WithSuspectFinder sets the optional suspect-change finder.
func (s *Service) WithSuspectFinder(f SuspectFinder) *Service {
	s.suspect = f
	return s
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

func (s *Service) today() Date { return DateOf(s.now().In(s.cfg.Location)) }

// scope is one request's loaded, filtered data.
type scope struct {
	// ctx is the request context, kept for optional collaborators (the
	// suspect finder) reached from view builders.
	ctx    context.Context
	q      Query
	cfg    Config
	today  Date
	now    time.Time
	depth  narrative.Depth
	st     SourceStatus
	period Period
	comp   Comparison
	header Header
	lines  []CostLine // primary currency, filtered, complete and partial
	// meter is today's paid-request use.
	meter *MeterView
	// scopeLines are the period's complete lines before the query filters,
	// for the scope bar's choices.
	scopeLines []CostLine
	complete   []CostLine // day ≤ last complete day
	accounts   map[string]Account
	owners     Ownership
	filtered   bool

	bridge     *Bridge
	increases  []IncreaseRecord
	incDone    bool
	savings    []SavingView
	savDone    bool
	savingGaps []SavingsGap
	// savingsDays is the distinct days that billed in the savings window,
	// counted from the totals this load already holds.
	savingsDays int
	// savingsDaysKnown is true when savingsDays was counted from totals; a
	// line read or a narrowed drill load leaves it false.
	savingsDaysKnown bool
	// billed are the accounts with cost in the window when the comparison
	// period was read per group (loadSummary); nil means sc.lines holds them.
	billed map[string]bool
}

// meterOf is today's paid-request use; the cap falls back to the config.
func (s *Service) meterOf(st SourceStatus) *MeterView {
	m := &MeterView{Used: st.MeterUsed, Cap: st.MeterCap}
	if m.Cap == 0 {
		m.Cap = s.cfg.MaxPaidRequests
	}
	return m
}

func (s *Service) disabled(reason string) Header {
	return Header{Enabled: false, Reason: reason, Links: Links{UI: s.cfg.UIPath}}
}

// load reads the cache for a query. A disabled source returns a scope with
// Header.Enabled false and no error.
func (s *Service) load(ctx context.Context, q Query) (*scope, error) {
	return s.loadFor(ctx, q, pageDims)
}

// loadFor reads the cache at the granularity a view needs. dims nil reads
// the individual lines (a feature that needs a field totals does not carry);
// otherwise a source with totals is summed in the store, so a view reads one
// row per group instead of one per line. A source without totals always
// reads lines.
func (s *Service) loadFor(ctx context.Context, q Query, dims []Dimension) (*scope, error) {
	return s.loadWindow(ctx, q, dims, 0, TotalsQuery{}, false)
}

// loadSummary is load for the summary, which reads the comparison period as
// per-group sums: see summaryLines. At the details depth it reads the usage
// type as well, so the answer sentence names it as the line read does.
func (s *Service) loadSummary(ctx context.Context, q Query) (*scope, error) {
	return s.loadWindow(ctx, q, summaryPageDims(q.depth()), 0, TotalsQuery{}, true)
}

// summaryPageDims is the granularity the summary reads its period at: the
// page granularity, plus the usage type at the details depth.
func summaryPageDims(d narrative.Depth) []Dimension {
	if d == narrative.Details {
		return detailDims
	}
	return pageDims
}

// loadDetection reads the history increase detection walks: the view window
// plus the lookback the detector needs. Only the sync path uses it, so a
// page render never pays for history no view shows.
func (s *Service) loadDetection(ctx context.Context) (*scope, error) {
	return s.loadWindow(ctx, Query{}, detailDims, detectionLookbackDays, TotalsQuery{}, false)
}

// loadWindow reads the cache for a query. dims nil reads the individual
// lines; narrow is a source-side narrowing a drill-down uses to read one
// account's service instead of the payer's whole history. The result is
// narrowed in Go as well, so a source that cannot apply the filter still
// answers the same numbers. summary lets an unfiltered page read its
// comparison period as per-group sums (summaryLines).
func (s *Service) loadWindow(ctx context.Context, q Query, dims []Dimension, lookback int, narrow TotalsQuery, summary bool) (*scope, error) {
	q, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	st, err := s.src.Status(ctx)
	if err != nil {
		return nil, err
	}
	sc := &scope{ctx: ctx, q: q, depth: q.depth(), st: st, cfg: s.cfg, today: s.today(), now: s.now(), meter: s.meterOf(st)}
	if !st.Enabled {
		reason := st.DisabledReason
		if reason == "" {
			reason = "cost_data_off"
		}
		sc.header = s.disabled(reason)
		sc.header.CostSource = st.CostSource
		return sc, nil
	}
	if st.LastCompleteDay.IsZero() {
		sc.header = s.disabled("no_complete_day")
		sc.header.CostSource = st.CostSource
		return sc, nil
	}
	sc.period, sc.comp, err = ResolvePeriod(q.Period, q.Compare, st.LastCompleteDay, st.HistoryStart)
	if err != nil {
		return nil, err
	}
	sc.accounts = map[string]Account{}
	owners := map[string]string{}
	for k, v := range s.cfg.AccountOwners {
		owners[k] = v
	}
	for _, a := range st.Accounts {
		if a.Group == "" {
			a.Group = s.cfg.AccountGroups[a.ID]
		}
		if a.Owner != "" && owners[a.ID] == "" {
			owners[a.ID] = a.Owner
		}
		sc.accounts[a.ID] = a
	}
	sc.owners = Ownership{Tag: st.TeamTag, AccountOwners: owners}

	L := st.LastCompleteDay
	from := sc.period.Range.Start
	if sc.comp.Available && sc.comp.Range.Start.Before(from) {
		from = sc.comp.Range.Start
	}
	// The accounts sparkline reads 30 days and the run-rate estimate the last
	// week; a view that only shows the period pays for nothing older.
	for _, d := range []Date{L.AddDays(-29), ForecastMonth(L).AddDays(-RunRateDays)} {
		if d.Before(from) {
			from = d
		}
	}
	if lookback > 0 {
		if d := L.AddDays(-lookback); d.Before(from) {
			from = d
		}
	}
	if !st.HistoryStart.IsZero() && from.Before(st.HistoryStart) {
		from = st.HistoryStart
	}
	to := sc.period.Calendar.End
	if L.AddDays(1).MonthEnd().After(to) {
		to = L.AddDays(1).MonthEnd()
	}
	var raw, base []CostLine
	disjoint := sc.comp.Range.End.Before(sc.period.Range.Start) || sc.period.Range.End.Before(sc.comp.Range.Start)
	collapse := summary && s.totals != nil && narrow.empty() && sc.comp.Available && disjoint &&
		len(q.Accounts) == 0 && len(q.Regions) == 0 && len(q.Teams) == 0
	if collapse {
		raw, base, err = s.summaryLines(ctx, sc, DateRange{from, to})
	} else {
		raw, err = s.sourceLines(ctx, DateRange{from, to}, dims, narrow)
	}
	if err != nil {
		return nil, err
	}
	// Every view sums these lines by day, account, service, team and owner.
	// When both the positive and the negative magnitudes fit, no subset sum
	// can wrap; when one does not, the view reports the overflow instead of
	// a wrong total. A collapsed comparison period is summed by the bridge
	// with the lines outside it.
	if !magnitudesFit(raw) {
		return nil, ErrAmountOverflow
	}
	if collapse {
		bridged := slices.Clone(base)
		for _, l := range raw {
			if !sc.comp.Range.Contains(l.Day) {
				bridged = append(bridged, l)
			}
		}
		if !magnitudesFit(bridged) {
			return nil, ErrAmountOverflow
		}
	}

	// The savings facts divide by the days the window billed. The page totals
	// already carry one row per day, so count the set here: the source would
	// otherwise scan the same rows again for payer × day.
	if s.totals != nil && narrow.empty() {
		window := savingsWindow(sc)
		days := map[Date]bool{}
		for _, l := range raw {
			if window.Contains(l.Day) {
				days[l.Day] = true
			}
		}
		sc.savingsDays, sc.savingsDaysKnown = len(days), true
	}

	other := map[string]Micros{}
	sc.filtered = len(q.Accounts) > 0 || len(q.Regions) > 0 || len(q.Teams) > 0
	partial := map[Date]bool{}
	// Months of lines: size the slices once instead of growing them.
	sc.lines = make([]CostLine, 0, len(raw))
	sc.complete = make([]CostLine, 0, len(raw))
	for _, l := range raw {
		sameCurrency := l.Currency == "" || st.Currency == "" || strings.EqualFold(l.Currency, st.Currency)
		if sameCurrency && sc.period.Range.Contains(l.Day) {
			sc.scopeLines = append(sc.scopeLines, l)
		}
		if !sc.match(l) {
			continue
		}
		if l.Currency != "" && st.Currency != "" && !strings.EqualFold(l.Currency, st.Currency) {
			if sc.period.Range.Contains(l.Day) {
				other[strings.ToUpper(l.Currency)] += l.NetAmortizedMicros
			}
			continue
		}
		sc.lines = append(sc.lines, l)
		if l.Day.After(L) {
			partial[l.Day] = true
			continue
		}
		sc.complete = append(sc.complete, l)
	}
	if collapse {
		// The bridge reads the comparison period from its per-group sums;
		// the day rows read for it serve the daily views only.
		kept := sc.complete[:0]
		for _, l := range sc.complete {
			if !sc.comp.Range.Contains(l.Day) {
				kept = append(kept, l)
			}
		}
		sc.complete = append(kept, base...)
		sc.billed = make(map[string]bool, len(sc.accounts))
		for _, l := range sc.lines {
			sc.billed[l.Account] = true
		}
		for _, l := range base {
			sc.billed[l.Account] = true
		}
	}

	sync := &Sync{
		LastCompleteDay:   L,
		Stale:             st.Stale,
		StaleReason:       st.StaleReason,
		SkippedCalls:      slices.Clone(st.SkippedCalls),
		ResourceData:      st.ResourceData,
		TeamTag:           st.TeamTag,
		PreviousMonthOpen: !MonthClosed(s.today().MonthStart().AddDays(-1), s.today(), s.cfg.BillCloseDay),
	}
	if !st.UpdatedAt.IsZero() {
		t := st.UpdatedAt
		sync.UpdatedAt = &t
	}
	for d := range partial {
		sync.PartialDays = append(sync.PartialDays, d)
	}
	slices.SortFunc(sync.PartialDays, func(a, b Date) int { return a.Compare(b) })
	for _, a := range st.Accounts {
		if len(q.Accounts) > 0 && !slices.Contains(q.Accounts, a.ID) {
			continue
		}
		sync.AccountsTotal++
		if a.Readable {
			sync.AccountsReadable++
			continue
		}
		sync.MissingAccounts = append(sync.MissingAccounts, MissingAccount{ID: a.ID, Name: a.DisplayName(), Reason: a.ErrorCode})
	}
	for _, c := range sortedKeys(other) {
		sync.OtherCurrencies = append(sync.OtherCurrencies, CurrencyTotal{Currency: c, Amount: other[c]})
	}
	need := DefaultBaselineDays + DefaultTriggerDays
	sync.Learning = st.HistoryStart.IsZero() || st.HistoryStart.DaysUntil(L)+1 < need
	if sync.Learning && !st.HistoryStart.IsZero() {
		sync.LearningUntil = st.HistoryStart.AddDays(need - 1)
	}
	if sync.PreviousMonthOpen {
		sync.BillClosesOn = s.today().MonthStart().AddDays(-1).MonthEnd().AddDays(s.cfg.BillCloseDay)
	} else {
		sync.BillClosesOn = s.today().MonthEnd().AddDays(s.cfg.BillCloseDay)
	}
	if !st.NextRefreshAt.IsZero() {
		t := st.NextRefreshAt
		sync.NextRefreshAt = &t
	}
	if st.Stale {
		sync.StaleDetail = &StaleDetail{Days: append([]Date{}, st.StaleDays...)}
		if !st.StaleSince.IsZero() {
			t := st.StaleSince
			sync.StaleDetail.Since = &t
		}
	}
	estTeam, estRegion := false, false
	for _, l := range sc.lines {
		estTeam = estTeam || l.EstimatedTeam
		estRegion = estRegion || l.EstimatedRegion
	}
	if len(q.Teams) > 0 && sc.teamSumEstimated(estTeam) {
		sync.Estimated = append(sync.Estimated, GroupTeam)
	}
	if len(q.Regions) > 0 && estRegion {
		sync.Estimated = append(sync.Estimated, GroupRegion)
	}
	sync.Partial = len(sync.MissingAccounts) > 0 || len(sync.SkippedCalls) > 0 || len(sync.OtherCurrencies) > 0 || st.Stale
	if len(sync.MissingAccounts) > 0 {
		sync.Notes = append(sync.Notes, narrative.AccountsNote(sync.AccountsReadable, sync.AccountsTotal))
	}
	if n := narrative.SkippedNote(s.cfg.MaxPaidRequests, sync.SkippedCalls); n != "" {
		sync.Notes = append(sync.Notes, n)
	}
	if sync.Learning {
		sync.Notes = append(sync.Notes, narrative.LearningNote(DefaultBaselineDays))
	}
	prev := s.today().MonthStart().AddDays(-1)
	if sync.PreviousMonthOpen && sc.period.Range.Contains(prev) {
		sync.Notes = append(sync.Notes, narrative.BillOpenNote(prev.Month, s.cfg.ProviderName, s.cfg.BillCloseDay))
	}
	if !sc.comp.Available && sc.comp.FirstFull != "" {
		if d, err := ParseDate(sc.comp.FirstFull + "-01"); err == nil {
			sync.Notes = append(sync.Notes, narrative.NoBaseNote(d.Month))
		}
	}

	period, comp := sc.period, sc.comp
	sc.header = Header{
		Enabled:    true,
		Period:     &period,
		Compare:    &comp,
		Depth:      string(sc.depth),
		Currency:   st.Currency,
		Measure:    MeasureNetAmortized,
		Sync:       sync,
		Links:      Links{UI: s.link(q, "")},
		CostSource: st.CostSource,
	}
	return sc, nil
}

// magnitudesFit reports whether the positive and the negative amounts of
// the lines each fit, so no subset sum of them can wrap.
func magnitudesFit(lines []CostLine) bool {
	var positive, negative uint64
	for _, l := range lines {
		var fits bool
		if positive, negative, fits = addMagnitudes(positive, negative, l.NetAmortizedMicros); !fits {
			return false
		}
	}
	return true
}

// dayAccountDims is what a daily series and the billed-account check need.
var dayAccountDims = []Dimension{ByDay, ByAccount}

// summaryLines reads what the unfiltered summary uses, at the granularity
// each part needs, instead of every day of the window at page granularity:
//
//   - the period at page granularity per day: the bridge looks for a
//     one-day spike per line, and the scope choices, owners and the
//     sparkline read it;
//   - the comparison period per group without the day (base): the bridge
//     only sums it per line;
//   - every other day of the window per day and account, plus the
//     comparison days a daily view reads (the forecast's recent days), for
//     the daily series, partial days and the billed accounts.
//
// The day rows inside the comparison period are returned with the others;
// the caller keeps them out of the bridge, which reads base instead.
func (s *Service) summaryLines(ctx context.Context, sc *scope, window DateRange) (days, base []CostLine, err error) {
	read := func(r DateRange, dims []Dimension) ([]CostLine, error) {
		if r.Start.After(r.End) {
			return nil, nil
		}
		return s.sourceLines(ctx, r, dims, TotalsQuery{})
	}
	dims := summaryPageDims(sc.depth)
	if days, err = read(sc.period.Range, dims); err != nil {
		return nil, nil, err
	}
	// The comparison period of a bridge is summed per group, never per day.
	baseDims := slices.DeleteFunc(slices.Clone(dims), func(d Dimension) bool { return d == ByDay })
	if base, err = read(sc.comp.Range, baseDims); err != nil {
		return nil, nil, err
	}
	// The forecast reads the 40 days up to the last complete day; the budget
	// line and the savings window read fewer. Those of them inside the
	// comparison period are read per day as well.
	recent := DateRange{Start: sc.st.LastCompleteDay.AddDays(-40), End: sc.st.LastCompleteDay}
	ranges := outsideRanges(window, sc.period.Range, sc.comp.Range)
	ranges = append(ranges, overlap(sc.comp.Range, recent))
	for _, r := range ranges {
		lines, err := read(r, dayAccountDims)
		if err != nil {
			return nil, nil, err
		}
		days = append(days, lines...)
	}
	return days, base, nil
}

// outsideRanges is window without the two ranges, which do not overlap.
func outsideRanges(window, a, b DateRange) []DateRange {
	if b.Start.Before(a.Start) {
		a, b = b, a
	}
	out := []DateRange{}
	cursor := window.Start
	for _, cut := range []DateRange{a, b} {
		if cut.Start.After(cursor) {
			out = append(out, DateRange{Start: cursor, End: minDate(cut.Start.AddDays(-1), window.End)})
		}
		if next := cut.End.AddDays(1); next.After(cursor) {
			cursor = next
		}
	}
	if !cursor.After(window.End) {
		out = append(out, DateRange{Start: cursor, End: window.End})
	}
	return out
}

// overlap is the days two ranges share; an empty overlap starts after it
// ends.
func overlap(a, b DateRange) DateRange {
	start, end := a.Start, a.End
	if b.Start.After(start) {
		start = b.Start
	}
	if b.End.Before(end) {
		end = b.End
	}
	return DateRange{Start: start, End: end}
}

func minDate(a, b Date) Date {
	if b.Before(a) {
		return b
	}
	return a
}

// resourceLinesOf reads the resource-level lines of one account and service
// in the range. tooLarge reports a read the source refused as over its line
// budget: resource names are enrichment, so the caller answers without them
// and says why instead of failing the view.
func (s *Service) resourceLinesOf(ctx context.Context, r DateRange, account, service string) (lines []CostLine, tooLarge bool, err error) {
	var rl []CostLine
	if narrow, ok := s.src.(ResourceLinesOfSource); ok {
		rl, err = narrow.ResourceLinesOf(ctx, r, account, service)
	} else {
		rl, err = s.src.ResourceLines(ctx, r)
	}
	if err != nil {
		if errors.Is(sourceError(err), ErrPeriodTooLarge) {
			return nil, true, nil
		}
		return nil, false, err
	}
	for _, l := range rl {
		if l.Account == account && l.Service == service && l.ResourceID != "" {
			lines = append(lines, l)
		}
	}
	return lines, false, nil
}

// billedAccounts are the accounts with cost lines in the loaded window.
func (sc *scope) billedAccounts() map[string]bool {
	if sc.billed != nil {
		return sc.billed
	}
	billed := make(map[string]bool, len(sc.lines))
	for _, l := range sc.lines {
		billed[l.Account] = true
	}
	return billed
}

// sourceLines reads the range at the requested granularity. A source with
// store-side totals answers one row per group, so a view reads one row per
// group instead of one per line; a source without totals, or a view that
// needs a field totals does not carry, reads the lines.
//
// A summed answer is already in the source's billing currency, so the
// per-line other-currency guard has nothing to add here.
func (s *Service) sourceLines(ctx context.Context, r DateRange, dims []Dimension, narrow TotalsQuery) ([]CostLine, error) {
	if s.totals == nil || dims == nil {
		lines, err := s.src.Lines(ctx, r)
		if err != nil {
			return nil, sourceError(err)
		}
		if narrow.empty() {
			return lines, nil
		}
		out := make([]CostLine, 0, len(lines))
		for _, l := range lines {
			if narrow.matchesLine(l) {
				out = append(out, l)
			}
		}
		return out, nil
	}
	totals, err := s.totals.Totals(ctx, r, narrow, dims...)
	if err != nil {
		return nil, sourceError(err)
	}
	out := make([]CostLine, 0, len(totals))
	for _, t := range totals {
		if !narrow.matchesTotal(t) {
			continue
		}
		out = append(out, lineOfTotal(t))
	}
	return out, nil
}

// link builds the page link for a query and optional sub-path.
func (s *Service) link(q Query, sub string) string {
	v := url.Values{}
	if q.Period != "" && q.Period != PeriodMonthToDate {
		v.Set("period", q.Period)
	}
	if q.Compare != "" && q.Compare != ComparePrevPeriod {
		v.Set("compare", q.Compare)
	}
	if len(q.Accounts) > 0 {
		v.Set("accounts", strings.Join(q.Accounts, ","))
	}
	if len(q.Regions) > 0 {
		v.Set("regions", strings.Join(q.Regions, ","))
	}
	if len(q.Teams) > 0 {
		v.Set("teams", strings.Join(q.Teams, ","))
	}
	if q.Depth == string(narrative.Details) {
		v.Set("depth", q.Depth)
	}
	p := strings.TrimRight(s.cfg.UIPath, "/") + sub
	if enc := v.Encode(); enc != "" {
		return p + "?" + enc
	}
	return p
}

func (sc *scope) match(l CostLine) bool {
	q := sc.q
	if len(q.Accounts) > 0 && !slices.Contains(q.Accounts, l.Account) {
		return false
	}
	if len(q.Regions) > 0 && !slices.Contains(q.Regions, l.Region) {
		return false
	}
	if len(q.Teams) > 0 && !slices.Contains(q.Teams, sc.ownerOf(l).Name) {
		return false
	}
	return true
}

func (sc *scope) ownerOf(l CostLine) Owner {
	return sc.owners.ResolveShare(l.Account, l.Team, l.ResourceID, l.EstimatedTeam, sc.depth)
}

// teamSumEstimated reports whether a sum over whole accounts that depends
// on the team of the given lines is an estimate.
func (sc *scope) teamSumEstimated(anyEstimatedTeam bool) bool {
	return anyEstimatedTeam && !sc.st.TeamExactPerAccount
}

func (sc *scope) accountName(id string) string {
	if a, ok := sc.accounts[id]; ok {
		return a.DisplayName()
	}
	return id
}

func (sc *scope) getBridge() Bridge {
	if sc.bridge == nil {
		var b Bridge
		if sc.comp.Available {
			b = ComputeBridge(sc.complete, sc.period.Range, sc.comp.Range, BridgeOptions{MinDriver: sc.cfg.MinDriver})
		} else {
			for _, l := range sc.complete {
				if sc.period.Range.Contains(l.Day) {
					b.Total += l.NetAmortizedMicros
				}
			}
		}
		sc.bridge = &b
	}
	return *sc.bridge
}

func (sc *scope) dailyTotals(r DateRange, keep func(CostLine) bool) map[Date]Micros {
	out := map[Date]Micros{}
	for _, l := range sc.lines {
		if r.Contains(l.Day) && (keep == nil || keep(l)) {
			out[l.Day] += l.NetAmortizedMicros
		}
	}
	return out
}

func (sc *scope) periodTotal(keep func(CostLine) bool) Micros {
	var t Micros
	for _, l := range sc.complete {
		if sc.period.Range.Contains(l.Day) && (keep == nil || keep(l)) {
			t += l.NetAmortizedMicros
		}
	}
	return t
}

func (sc *scope) baseTotal(keep func(CostLine) bool) Micros {
	var t Micros
	for _, l := range sc.complete {
		if sc.comp.Range.Contains(l.Day) && (keep == nil || keep(l)) {
			t += l.NetAmortizedMicros
		}
	}
	return t
}

func (sc *scope) history(account string) Date {
	start := sc.st.HistoryStart
	if start.IsZero() {
		return start
	}
	if a, ok := sc.accounts[account]; ok && !a.FirstCost.IsZero() && a.FirstCost.After(start) {
		start = a.FirstCost
	}
	if lb := sc.st.LastCompleteDay.AddDays(-detectionLookbackDays); start.Before(lb) {
		start = lb
	}
	return start
}
