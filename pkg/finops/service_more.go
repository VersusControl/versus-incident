package finops

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Errors of the write methods.
var (
	ErrNoStore        = errors.New("finops: no store configured")
	ErrAlreadyFrozen  = errors.New("finops: the report for this month is already saved")
	ErrBillNotClosed  = errors.New("finops: the bill for this month is not closed yet")
	ErrReportNotMonth = errors.New("finops: a report covers one calendar month")
)

// dateTime converts a Date for the narrative; the zero Date is the zero
// time.
func dateTime(d Date) time.Time {
	if d.IsZero() {
		return time.Time{}
	}
	return d.Time()
}

// getIncreases reads the increases the detector last persisted and applies
// the time rules. It never detects: detection walks every usage series, so
// it runs on the sync cadence (SyncIncreases) and a page render only reads
// its result. It writes nothing.
func (s *Service) getIncreases(ctx context.Context, sc *scope) ([]IncreaseRecord, error) {
	if sc.incDone {
		return sc.increases, nil
	}
	var recs []IncreaseRecord
	if s.store != nil {
		var err error
		if recs, err = s.store.ListIncreases(ctx); err != nil {
			return nil, err
		}
	}
	billed := sc.billedAccounts()
	var out []IncreaseRecord
	for _, r := range recs {
		// A record whose account has no cost lines left is not listed: its
		// drill would have nothing behind it, so the list and the drill
		// agree that it is gone.
		if !billed[r.Key.Account] {
			continue
		}
		// A record carries no region or team, so a filter that narrows to
		// either cannot be checked against it without inventing an answer.
		if len(sc.q.Regions) > 0 || len(sc.q.Teams) > 0 ||
			(len(sc.q.Accounts) > 0 && !slices.Contains(sc.q.Accounts, r.Key.Account)) {
			continue
		}
		out = append(out, Reconcile(&r, r.Detected, sc.today, sc.now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := out[i].StartedOn.Compare(out[j].StartedOn); c != 0 {
			return c < 0
		}
		return out[i].ID < out[j].ID
	})
	sc.increases, sc.incDone = out, true
	return out, nil
}

// detectIncreases runs the detector over the loaded history and overlays the
// stored decisions. Only SyncIncreases calls it: the result is persisted and
// every view reads that.
func (s *Service) detectIncreases(ctx context.Context, sc *scope) ([]IncreaseRecord, error) {
	stored := map[string]IncreaseRecord{}
	if s.store != nil {
		recs, err := s.store.ListIncreases(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			stored[r.ID] = r
		}
	}
	billed := make(map[string]bool, len(sc.lines))
	for _, l := range sc.lines {
		billed[l.Account] = true
	}
	det := DetectIncreases(sc.complete, sc.st.LastCompleteDay, sc.history,
		DetectOptions{MinIncrease: s.cfg.MinIncrease})
	seen := map[string]bool{}
	var out []IncreaseRecord
	for _, d := range det {
		var prev *IncreaseRecord
		if r, ok := stored[d.ID]; ok {
			prev = &r
		}
		out = append(out, Reconcile(prev, d, sc.today, sc.now))
		seen[d.ID] = true
	}
	// Increases that started before the walked history keep their stored
	// record; the time rules still apply. A record whose account has no cost
	// lines left is not listed: its drill would have nothing behind it, so
	// the list and the drill agree that it is gone.
	lookback := sc.st.LastCompleteDay.AddDays(-detectionLookbackDays)
	for _, id := range sortedKeys(stored) {
		r := stored[id]
		if seen[id] || !billed[r.Key.Account] || !r.StartedOn.Before(lookback.AddDays(DefaultBaselineDays+DefaultTriggerDays)) {
			continue
		}
		if len(sc.q.Regions) > 0 || len(sc.q.Teams) > 0 ||
			(len(sc.q.Accounts) > 0 && !slices.Contains(sc.q.Accounts, r.Key.Account)) {
			continue
		}
		out = append(out, Reconcile(&r, r.Detected, sc.today, sc.now))
	}
	return out, nil
}

func (s *Service) suspectOf(sc *scope, rec IncreaseRecord) SuspectView {
	sv, _, _ := s.suspectAndSignal(sc, rec)
	return sv
}

// suspectAndSignal returns the top suspect, the ranked list (up to
// MaxSuspects, empty unless found) and the usage signal. A finder error
// says the history could not be read; it is never "no change found".
func (s *Service) suspectAndSignal(sc *scope, rec IncreaseRecord) (SuspectView, []SuspectView, *UsageSignal) {
	signal := sc.billedUsageSignal(rec)
	if a, ok := sc.accounts[rec.Key.Account]; ok && a.Kind == AccountCostOnly {
		return SuspectView{State: "account_not_connected", Note: "Why: account not connected"}, []SuspectView{}, signal
	}
	if s.suspect == nil {
		return SuspectView{State: "collectors_pending", Note: narrative.SuspectNote("collectors_pending")}, []SuspectView{}, signal
	}
	var list []SuspectView
	var sig *UsageSignal
	var err error
	if r, ok := s.suspect.(SuspectRanker); ok {
		list, sig, err = r.Suspects(sc.ctx, rec.Detected)
	} else {
		var sv SuspectView
		sv, sig, err = s.suspect.Suspect(sc.ctx, rec.Detected)
		list = []SuspectView{sv}
	}
	if sig == nil {
		sig = signal
	}
	if err != nil || len(list) == 0 || list[0].State == "" {
		return SuspectView{State: "history_incomplete", Note: narrative.SuspectNote("history_incomplete")}, []SuspectView{}, sig
	}
	for i := range list {
		if list[i].Confidence != "" {
			list[i].ConfidenceWord = strings.ToUpper(list[i].Confidence[:1]) + list[i].Confidence[1:]
		}
	}
	top := list[0]
	if top.State != "found" {
		if top.Note == "" {
			top.Note = narrative.SuspectNote(top.State)
		}
		return top, []SuspectView{}, sig
	}
	found := []SuspectView{}
	for _, sv := range list {
		if sv.State == "found" && len(found) < MaxSuspects {
			found = append(found, sv)
		}
	}
	return top, found, sig
}

// billedUsageSignal compares the line's billed quantity per day before and
// since the increase started, when the provider bills it in a unit (bytes
// through a NAT gateway, hours). Without a unit or a quantity it is absent.
func (sc *scope) billedUsageSignal(rec IncreaseRecord) *UsageSignal {
	var unit, label string
	before, after := map[Date]int64{}, map[Date]int64{}
	end := sc.st.LastCompleteDay
	if !rec.ResolvedOn.IsZero() {
		end = rec.ResolvedOn
	}
	baseStart := rec.StartedOn.AddDays(-DefaultBaselineDays)
	for _, l := range sc.complete {
		k := l.Key()
		k.ResourceID = ""
		if k != rec.Key || l.QuantityMicros == 0 {
			continue
		}
		if l.Unit != "" {
			unit = l.Unit
		}
		if l.UsageGroup != "" {
			label = l.UsageGroup
		}
		switch {
		case !l.Day.Before(baseStart) && l.Day.Before(rec.StartedOn):
			before[l.Day] += l.QuantityMicros
		case !l.Day.Before(rec.StartedOn) && !l.Day.After(end):
			after[l.Day] += l.QuantityMicros
		}
	}
	if unit == "" || len(before) == 0 || len(after) == 0 {
		return nil
	}
	avg := func(m map[Date]int64) int64 {
		var s int64
		for _, v := range m {
			s += v
		}
		return mulDiv(s, 1, int64(len(m)))
	}
	if label == "" {
		label = rec.Key.UsageType
	}
	// The label names the quantity ("instances") and the unit its measure
	// per day ("hours/day"), so the two read together as "instances, hours
	// per day" and never disagree.
	return &UsageSignal{Label: label, Before: avg(before), After: avg(after), Unit: billedUnitWord(unit) + "/day"}
}

// billedUnitWords spells the provider's short billing units as plain words.
var billedUnitWords = map[string]string{
	"hrs":      "hours",
	"hours":    "hours",
	"gb-mo":    "GB-months",
	"gb":       "GB",
	"requests": "requests",
	"lcu-hrs":  "LCU-hours",
	"lcu":      "LCUs",
}

// billedUnitWord is the plain word for a billing unit; an unknown unit is
// kept as the provider spells it.
func billedUnitWord(unit string) string {
	if w, ok := billedUnitWords[strings.ToLower(strings.TrimSpace(unit))]; ok {
		return w
	}
	return strings.TrimSpace(unit)
}

// lineDaily is the daily cost of one usage line (all resources).
func (sc *scope) lineDaily(k LineKey) map[Date]Micros {
	out := map[Date]Micros{}
	for _, l := range sc.lines {
		if l.Account == k.Account && l.Service == k.Service && l.UsageType == k.UsageType && l.recordType() == k.RecordType {
			out[l.Day] += l.NetAmortizedMicros
		}
	}
	return out
}

func (sc *scope) periodWord() string {
	switch {
	case sc.period.Key == PeriodLast30Days:
		return "in the last 30 days"
	case sc.period.Open:
		return "this month"
	}
	return "in " + sc.period.Short
}

func (s *Service) increaseView(sc *scope, rec IncreaseRecord, savings []SavingView) IncreaseView {
	acct := sc.accounts[rec.Key.Account]
	v := IncreaseView{
		ID: rec.ID, Account: rec.Key.Account, AccountName: sc.accountName(rec.Key.Account), AccountKind: acct.Kind,
		Service: rec.Key.Service, ServiceLabel: narrative.ServiceLabel(rec.Key.Service, rec.ServiceName, sc.depth),
		StartedOn: rec.StartedOn, DailyBefore: rec.DailyBefore, DailyNow: rec.DailyNow, Monthly: rec.MonthlyImpact,
		Status: rec.Status, Expected: rec.Expected, AlertedAt: rec.AlertedAt, ResolvedOn: rec.ResolvedOn,
		Owner: sc.owners.ResolveShare(rec.Key.Account, rec.Team, "", rec.TeamEstimated, sc.depth),
	}
	if sc.depth == narrative.Details {
		v.UsageType = rec.Key.UsageType
	}
	end := sc.st.LastCompleteDay
	if !rec.ResolvedOn.IsZero() {
		end = rec.ResolvedOn
	}
	var ownChange Micros
	for d, amt := range sc.lineDaily(rec.Key) {
		if d.Before(rec.StartedOn) || d.After(end) {
			continue
		}
		ownChange += amt - rec.DailyBefore
		if sc.period.Range.Contains(d) {
			v.PeriodChange += amt - rec.DailyBefore
		}
	}
	// The title states the period's change when the increase moved the
	// period's cost; otherwise (it started and ended outside the period,
	// or did not add cost inside it) it states the increase's own change
	// over its own days, so it never reads "went up $0 this month".
	change := narrative.Increase{Currency: sc.st.Currency, PeriodWord: sc.periodWord(), Amount: v.PeriodChange}
	if !narrative.ShowsPositive(v.PeriodChange) {
		change.Window, change.WindowDays, change.Amount = true, rec.StartedOn.DaysUntil(end)+1, ownChange
	}
	if !rec.ResolvedOn.IsZero() {
		change.ResolvedOn = dateTime(rec.ResolvedOn)
	}
	if narrative.ShowsPositive(change.Amount) {
		v.StatedChange = change.Amount
	}
	v.Cause = CauseUsageUp
	if rec.DailyBefore == 0 {
		v.Cause = CauseNewResource
	}
	if sc.comp.Available {
		var best Micros
		for _, d := range sc.getBridge().Drivers {
			if usageKey(d.Key) == usageKey(rec.Key) && d.Amount > best {
				best, v.Cause = d.Amount, d.Cause
			}
		}
	}
	sv, ranked, _ := s.suspectAndSignal(sc, rec)
	v.Suspect = sv
	v.Suspects = ranked
	if rec.AlertedAt != nil {
		v.Alert = &AlertView{Channel: "alerts", SentAt: *rec.AlertedAt}
	}
	v.Title = narrative.IncreaseChange(change)
	in := narrative.Increase{
		Currency: sc.st.Currency, ServiceKey: rec.Key.Service, ServiceName: rec.ServiceName,
		AccountName: v.AccountName, AccountID: rec.Key.Account, PeriodWord: change.PeriodWord,
		Amount: change.Amount, Window: change.Window, WindowDays: change.WindowDays, ResolvedOn: change.ResolvedOn,
		DailyBefore: rec.DailyBefore, DailyNow: rec.DailyNow,
		StartedOn: dateTime(rec.StartedOn), SuspectState: sv.State, SuspectSummary: sv.Summary, SuspectActor: sv.Actor,
		SuspectsTied: tiedCount(v.Suspects),
		Owner:        v.Owner.Name, OwnerRule: v.Owner.Rule, OwnerEstimated: v.Owner.Estimated, TagKey: sc.st.TeamTag.Key,
	}
	if sc.depth == narrative.Details {
		in.UsageType = rec.Key.UsageType
	}
	if rel := relatedSavings(rec, savings); len(rel) > 0 {
		in.SavingAction, in.SavingMonthly = lowerFirstWord(rel[0].Title), rel[0].Monthly
	}
	v.Sentence = narrative.IncreaseSentence(in, sc.depth)
	return v
}

// lowerFirstWord turns a title ("Add VPC endpoints") into a phrase that
// reads inside a sentence ("adding VPC endpoints").
func lowerFirstWord(title string) string {
	verb, rest, _ := strings.Cut(title, " ")
	switch verb {
	case "Add":
		verb = "adding"
	case "Remove":
		verb = "removing"
	case "Resize":
		verb = "resizing"
	case "Raise":
		verb = "raising"
	default:
		verb = strings.ToLower(verb)
	}
	return verb + " " + rest
}

// relatedSavings links open savings to an increase: same account and either
// a network-path fix for a network line or a shared resource.
func relatedSavings(rec IncreaseRecord, savings []SavingView) []SavingView {
	var out []SavingView
	for _, sv := range savings {
		if sv.Status != "open" || sv.Account != rec.Key.Account {
			continue
		}
		if sv.Kind == SavingNetworkPath && strings.HasPrefix(rec.Key.Service, "network.") {
			out = append(out, sv)
			continue
		}
		if rec.Key.ResourceID != "" && slices.Contains(sv.ResourceIDs, rec.Key.ResourceID) {
			out = append(out, sv)
		}
	}
	return out
}

// loadIncrease reads one increase's own lines: a narrow query asks the
// source for the increase's account and service, grouped by usage type, so a
// drill-down never walks the payer's whole history.
func (s *Service) loadIncrease(ctx context.Context, q Query, rec IncreaseRecord) (*scope, error) {
	return s.loadWindow(ctx, q, increaseDims, 0, TotalsQuery{Accounts: []string{rec.Key.Account}, Services: []string{rec.Key.Service}}, false)
}

// Increases lists increases with a status filter (default open). The list
// itself reads page granularity; each listed increase's title and cause read
// its own narrow usage-type series.
func (s *Service) Increases(ctx context.Context, q Query) (IncreasesView, error) {
	sc, err := s.loadFor(ctx, q, pageDims)
	if err != nil || !sc.header.Enabled {
		return IncreasesView{Header: headerOf(sc)}, err
	}
	status := sc.q.Status
	if status == "" {
		status = StatusOpen
	}
	v := IncreasesView{Header: sc.header, Status: status, Items: []IncreaseView{}, Learning: sc.header.Sync.Learning}
	if v.Learning {
		v.LearningNote = narrative.LearningNote(DefaultBaselineDays)
	}
	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return v, err
	}
	sav, err := s.getSavings(ctx, sc)
	if err != nil {
		return v, err
	}
	for _, r := range incs {
		if status != "all" && r.Status != status {
			continue
		}
		detail, err := s.loadIncrease(ctx, q, r)
		if err != nil {
			return v, err
		}
		v.Items = append(v.Items, s.increaseView(detail, r, sav))
	}
	sort.SliceStable(v.Items, func(i, j int) bool {
		if v.Items[i].Monthly != v.Items[j].Monthly {
			return v.Items[i].Monthly > v.Items[j].Monthly
		}
		return v.Items[i].ID < v.Items[j].ID
	})
	return v, nil
}

// Increase returns one increase with its cause tree, suspect change, usage
// signal, owner rule, related savings and history.
func (s *Service) Increase(ctx context.Context, q Query, id string) (IncreaseDetailView, error) {
	sc, err := s.loadFor(ctx, q, pageDims)
	if err != nil || !sc.header.Enabled {
		return IncreaseDetailView{Header: headerOf(sc)}, err
	}
	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return IncreaseDetailView{Header: sc.header}, err
	}
	var rec *IncreaseRecord
	for i := range incs {
		if incs[i].ID == id {
			rec = &incs[i]
		}
	}
	if rec == nil {
		return IncreaseDetailView{Header: sc.header}, ErrNotFound
	}
	sav, err := s.getSavings(ctx, sc)
	if err != nil {
		return IncreaseDetailView{Header: sc.header}, err
	}
	detail, err := s.loadIncrease(ctx, q, *rec)
	if err != nil {
		return IncreaseDetailView{Header: sc.header}, err
	}
	v := IncreaseDetailView{Header: sc.header, Increase: s.increaseView(detail, *rec, sav),
		CauseTree: []CauseNode{}, RelatedSavings: relatedSavings(*rec, sav), History: rec.History}
	if v.RelatedSavings == nil {
		v.RelatedSavings = []SavingView{}
	}
	if v.History == nil {
		v.History = []StatusEvent{}
	}
	_, _, v.UsageSignal = s.suspectAndSignal(detail, *rec)

	end := detail.st.LastCompleteDay
	if !rec.ResolvedOn.IsZero() && rec.ResolvedOn.AddDays(DefaultTriggerDays).Before(end) {
		end = rec.ResolvedOn.AddDays(DefaultTriggerDays)
	}
	daily := detail.lineDaily(rec.Key)
	from := rec.StartedOn.AddDays(-DefaultBaselineDays)
	for d := from; !d.After(end); d = d.AddDays(1) {
		v.Series = append(v.Series, DayAmount{Day: d, Amount: daily[d]})
	}
	for _, d := range sc.header.Sync.PartialDays {
		if amt, ok := daily[d]; ok {
			v.Series = append(v.Series, DayAmount{Day: d, Amount: amt, Partial: true})
		}
	}

	// Cause tree: each usage type of the same account and service, by its
	// added cost since the start over its own baseline median.
	type ut struct {
		key   LineKey
		name  string
		daily map[Date]Micros
	}
	uts := map[string]*ut{}
	for _, l := range detail.complete {
		if l.Account != rec.Key.Account || l.Service != rec.Key.Service || l.recordType() != RecordUsage {
			continue
		}
		u := uts[l.UsageType]
		if u == nil {
			u = &ut{key: usageKey(l.Key()), daily: map[Date]Micros{}}
			uts[l.UsageType] = u
		}
		u.daily[l.Day] += l.NetAmortizedMicros
		if l.UsageGroup != "" {
			u.name = l.UsageGroup
		}
	}
	var resources map[string]map[string]Micros
	if sc.st.ResourceData {
		v.ResourceData = "available"
		rl, tooLarge, err := s.resourceLinesOf(ctx, DateRange{rec.StartedOn, end}, rec.Key.Account, rec.Key.Service)
		if err != nil {
			return v, err
		}
		if tooLarge {
			v.ResourceData, v.ResourceNote = "too_large", narrative.ResourceDetailTooLargeNote()
		}
		resources = map[string]map[string]Micros{}
		for _, l := range rl {
			if !l.Day.After(end) {
				if resources[l.UsageType] == nil {
					resources[l.UsageType] = map[string]Micros{}
				}
				resources[l.UsageType][l.ResourceID] += l.NetAmortizedMicros
			}
		}
	} else {
		v.ResourceData = "off"
		v.ResourceNote = narrative.ResourceDataOffNote(s.cfg.ProviderName)
	}
	for _, name := range sortedKeys(uts) {
		u := uts[name]
		var base []int64
		for d := rec.StartedOn.AddDays(-DefaultBaselineDays); d.Before(rec.StartedOn); d = d.AddDays(1) {
			base = append(base, u.daily[d])
		}
		med := median(base)
		var added Micros
		for d := rec.StartedOn; !d.After(end); d = d.AddDays(1) {
			added += u.daily[d] - med
		}
		if added <= 0 && name != rec.Key.UsageType {
			continue
		}
		label := u.name
		if label == "" || sc.depth == narrative.Details {
			label = name
		}
		node := CauseNode{UsageType: name, Label: label, Amount: added}
		for _, rid := range sortedKeys(resources[name]) {
			node.Resources = append(node.Resources, ResourceCost{ID: rid, Amount: resources[name][rid]})
		}
		sort.SliceStable(node.Resources, func(i, j int) bool { return node.Resources[i].Amount > node.Resources[j].Amount })
		v.CauseTree = append(v.CauseTree, node)
	}
	sort.SliceStable(v.CauseTree, func(i, j int) bool { return v.CauseTree[i].Amount > v.CauseTree[j].Amount })
	return v, nil
}

// getSavings evaluates the savings rules and overlays dismissals.
func (s *Service) getSavings(ctx context.Context, sc *scope) ([]SavingView, error) {
	if sc.savDone {
		return sc.savings, nil
	}
	in, err := s.savingsInputs(ctx, sc)
	if err != nil {
		return nil, err
	}
	sc.savingGaps = append([]SavingsGap{}, in.Gaps...)
	states := map[string]SavingState{}
	if s.store != nil {
		if states, err = s.store.SavingStates(ctx); err != nil {
			return nil, err
		}
	}
	var out []SavingView
	for _, it := range EvaluateSavings(in, SavingsOptions{TargetCoverageBP: s.cfg.TargetCoverageBP}) {
		if len(sc.q.Accounts) > 0 && !slices.Contains(sc.q.Accounts, it.Account) {
			continue
		}
		if len(sc.q.Regions) > 0 && !slices.Contains(sc.q.Regions, it.Region) {
			continue
		}
		owner := sc.owners.Resolve(it.Account, it.Team, "", sc.depth)
		if len(sc.q.Teams) > 0 && !slices.Contains(sc.q.Teams, owner.Name) {
			continue
		}
		v := SavingView{Saving: it, Owner: owner, Status: "open",
			Line: narrative.SavingLine(narrative.Saving{Currency: sc.st.Currency, Title: it.Title, Monthly: it.Monthly, Effort: it.Effort,
				Risk: it.Risk, UpTo: it.UpTo, BeforeOtherSavings: it.BeforeOtherSavings}, sc.depth)}
		if st, ok := states[it.ID]; ok && st.Dismissed {
			st := st
			v.Status, v.Dismissed = "dismissed", &st
		}
		out = append(out, v)
	}
	sc.savings, sc.savDone = out, true
	return out, nil
}

// Savings lists savings; the total counts open items only.
func (s *Service) Savings(ctx context.Context, q Query) (SavingsView, error) {
	sc, err := s.load(ctx, q)
	if err != nil || !sc.header.Enabled {
		return SavingsView{Header: headerOf(sc)}, err
	}
	items, err := s.getSavings(ctx, sc)
	if err != nil {
		return SavingsView{Header: sc.header}, err
	}
	if items == nil {
		items = []SavingView{}
	}
	gaps := sc.savingGaps
	if gaps == nil {
		gaps = []SavingsGap{}
	}
	return SavingsView{Header: sc.header, SavingsBody: SavingsBody{Total: savingsTotal(items), Items: items, Gaps: gaps}}, nil
}

// savingsWindow is the 30 complete days the savings facts read.
func savingsWindow(sc *scope) DateRange {
	end := sc.st.LastCompleteDay
	return DateRange{Start: end.AddDays(-29), End: end}
}

// savingsInputs asks for the neutral savings facts. A source that sums in
// the store is handed the window and, when the load already counted it, the
// number of days that billed — so the facts cost neither a 30-day line scan
// nor a second payer × day aggregation. A source with only the line-based
// answer keeps it, unchanged.
func (s *Service) savingsInputs(ctx context.Context, sc *scope) (SavingsInputs, error) {
	w := savingsWindow(sc)
	if st, ok := s.src.(SavingsFactsSource); ok {
		return st.SavingsInputsFromFacts(ctx, SavingsFactsQuery{
			Range: w, BilledDays: sc.savingsDays, DaysKnown: sc.savingsDaysKnown,
		})
	}
	if st, ok := s.src.(SavingsTotalsSource); ok {
		return st.SavingsInputsFromTotals(ctx, w)
	}
	return s.src.SavingsInputs(ctx)
}

// allBudgets merges the configured, provider and stored budgets.
func (s *Service) allBudgets(ctx context.Context, sc *scope) ([]Budget, error) {
	var out []Budget
	add := func(b Budget, source string) {
		b.Source = source
		if b.ID == "" {
			b.ID = BudgetID(b.Scope, b.Value)
		}
		out = append(out, b)
	}
	for _, b := range s.cfg.Budgets {
		add(b, BudgetFromConfig)
	}
	if s.store != nil {
		ui, err := s.store.Budgets(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range ui {
			add(b, BudgetFromUI)
		}
	}
	for _, b := range sc.st.Budgets {
		add(b, BudgetFromProvider)
	}
	return out, nil
}

// forecast is the deployment forecast (keep nil) or a scoped estimate.
func (s *Service) forecast(sc *scope, keep func(CostLine) bool) Forecast {
	L := sc.st.LastCompleteDay
	daily := map[Date]Micros{}
	if from := sc.st.HistoryStart; !from.IsZero() {
		if lb := L.AddDays(-40); from.Before(lb) {
			from = lb
		}
		daily = sc.dailyTotals(DateRange{from, L}, keep)
	}
	var pf *ProviderForecast
	if keep == nil && !sc.filtered {
		pf = sc.st.Forecast
	}
	skipped := slices.Contains(sc.st.SkippedCalls, "forecast")
	return MakeForecast(pf, L, sc.st.HistoryStart, func(d Date) Micros { return daily[d] }, skipped)
}

func (s *Service) budgetLine(sc *scope, b Budget) BudgetLine {
	var keep func(CostLine) bool
	if b.Scope != ScopeDeployment {
		keep = func(l CostLine) bool { return BudgetMatches(b, l.Account, sc.ownerOf(l).Name) }
	}
	fc := s.forecast(sc, keep)
	L := sc.st.LastCompleteDay
	var actual Micros
	for _, amt := range sc.dailyTotals(DateRange{ForecastMonth(L), L}, keep) {
		actual += amt
	}
	st := BudgetStatus(b.Monthly, actual, fc)
	bl := BudgetLine{Budget: b, ReadOnly: b.ReadOnly(), Actual: actual, Forecast: fc.Amount,
		ForecastSource: fc.Source, Status: st, StatusWord: narrative.BudgetStatusWord(st)}
	for _, l := range sc.lines {
		if keep != nil && !keep(l) {
			continue
		}
		if (b.Scope == ScopeTeam && sc.teamSumEstimated(l.EstimatedTeam)) || (len(sc.q.Regions) > 0 && l.EstimatedRegion) {
			bl.Estimated = true
			break
		}
	}
	if fc.Source != ForecastNone {
		bl.ForecastDiff = fc.Amount - b.Monthly
	}
	return bl
}

// Budget returns the forecast and every budget with its status.
func (s *Service) Budget(ctx context.Context, q Query) (BudgetView, error) {
	sc, err := s.load(ctx, q)
	if err != nil || !sc.header.Enabled {
		return BudgetView{Header: headerOf(sc)}, err
	}
	v := BudgetView{Header: sc.header, Forecast: s.forecast(sc, nil), Budgets: []BudgetLine{}, Skipped: sc.st.SkippedBudgets}
	bs, err := s.allBudgets(ctx, sc)
	if err != nil {
		return v, err
	}
	for _, b := range bs {
		v.Budgets = append(v.Budgets, s.budgetLine(sc, b))
	}
	return v, nil
}

// Accounts returns the Accounts tab: one row per account with status,
// group, cost, change, top driver, top team and a 30-day sparkline.
func (s *Service) Accounts(ctx context.Context, q Query) (AccountsView, error) {
	// A row's top driver is the bridge at line identity: usage types split a
	// new resource from a price change, so this view reads them.
	sc, err := s.loadFor(ctx, q, detailDims)
	if err != nil || !sc.header.Enabled {
		return AccountsView{Header: headerOf(sc)}, err
	}
	v := AccountsView{Header: sc.header, Accounts: []AccountRow{}, Groups: []AccountGroupRow{}}
	ids := map[string]bool{}
	for id := range sc.accounts {
		if len(sc.q.Accounts) == 0 || slices.Contains(sc.q.Accounts, id) {
			ids[id] = true
		}
	}
	for _, l := range sc.complete {
		ids[l.Account] = true
	}
	b := sc.getBridge()
	L := sc.st.LastCompleteDay
	spark := DateRange{L.AddDays(-29), L}
	groups := map[string]*AccountGroupRow{}
	for _, id := range sortedKeys(ids) {
		a, known := sc.accounts[id]
		if !known {
			a = Account{ID: id, Kind: AccountCostOnly, Readable: true, Group: s.cfg.AccountGroups[id]}
		}
		mine := func(l CostLine) bool { return l.Account == id }
		row := AccountRow{ID: id, Name: a.DisplayName(), Kind: a.Kind, Group: a.Group, ErrorCode: a.ErrorCode,
			Cost: sc.periodTotal(mine), FirstCost: a.FirstCost, ClosedOn: a.ClosedOn}
		if row.Kind == "" {
			row.Kind = AccountConnected
		}
		switch {
		case !a.Readable:
			row.Status = AccountNotReadable
		case !a.ClosedOn.IsZero():
			row.Status = AccountClosed
		case !a.FirstCost.IsZero() && sc.period.Range.Contains(a.FirstCost):
			row.Status = AccountNew
		default:
			row.Status = AccountOK
		}
		if sc.comp.Available && a.Readable {
			ch := row.Cost - sc.baseTotal(mine)
			row.Change = &ch
		}
		var top *Driver
		for i, d := range b.Drivers {
			if d.Key.Account == id && (top == nil || absMicros(d.Amount) > absMicros(top.Amount)) {
				top = &b.Drivers[i]
			}
		}
		if top != nil {
			row.TopDriver = &TopDriver{Label: narrative.DriverLine(s.phrase(sc, *top, nil), sc.depth), Amount: top.Amount}
		}
		teams := map[string]Micros{}
		estTeam := map[string]bool{}
		for _, l := range sc.complete {
			if l.Account == id && sc.period.Range.Contains(l.Day) && l.Team != "" && sc.st.TeamTag.Allowlisted {
				teams[l.Team] += l.NetAmortizedMicros
				estTeam[l.Team] = estTeam[l.Team] || l.EstimatedTeam
			}
		}
		var best Micros
		for _, t := range sortedKeys(teams) {
			if row.TopTeam == "" || teams[t] > best {
				row.TopTeam, best = t, teams[t]
			}
		}
		row.TopTeamEstimated = row.TopTeam != "" && sc.teamSumEstimated(estTeam[row.TopTeam])
		daily := sc.dailyTotals(spark, mine)
		spark.Each(func(d Date) { row.Sparkline = append(row.Sparkline, daily[d]) })
		v.Accounts = append(v.Accounts, row)
		v.Total += row.Cost

		gname := row.Group
		if gname == "" {
			gname = "Ungrouped"
		}
		g := groups[gname]
		if g == nil {
			g = &AccountGroupRow{Name: gname}
			if sc.comp.Available {
				g.Change = new(Micros)
			}
			groups[gname] = g
		}
		g.Cost += row.Cost
		g.Count++
		if g.Change != nil && row.Change != nil {
			*g.Change += *row.Change
		}
	}
	sort.SliceStable(v.Accounts, func(i, j int) bool { return v.Accounts[i].Cost > v.Accounts[j].Cost })
	for _, name := range sortedKeys(groups) {
		v.Groups = append(v.Groups, *groups[name])
	}
	return v, nil
}

// Report returns the report model for one month: the frozen copy when the
// month was saved, else a live model.
func (s *Service) Report(ctx context.Context, q Query) (ReportView, error) {
	// The report embeds the bridge, whose rows carry line identity.
	sc, err := s.loadFor(ctx, q, detailDims)
	if err != nil || !sc.header.Enabled {
		return ReportView{Header: headerOf(sc)}, err
	}
	if !sc.period.month {
		return ReportView{Header: sc.header}, ErrReportNotMonth
	}
	month := sc.period.Calendar.Start.MonthKey()
	if s.store != nil {
		fr, err := s.store.GetReport(ctx, month)
		if err != nil {
			return ReportView{Header: sc.header}, err
		}
		if fr != nil {
			return s.readFrozen(*fr, sc)
		}
	}
	return s.buildReport(ctx, sc)
}

func (s *Service) buildReport(ctx context.Context, sc *scope) (ReportView, error) {
	r := ReportView{Header: sc.header, GeneratedAt: sc.now}
	var err error
	if r.Summary, err = s.summaryBody(ctx, sc); err != nil {
		return r, err
	}
	if r.Bridge, err = s.bridgeBody(ctx, sc); err != nil {
		return r, err
	}
	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return r, err
	}
	sav, err := s.getSavings(ctx, sc)
	if err != nil {
		return r, err
	}
	byID := map[string]IncreaseRecord{}
	for _, in := range incs {
		byID[in.ID] = in
	}
	var labels []string
	for i := range r.Bridge.Rows {
		row := &r.Bridge.Rows[i]
		if i < 3 {
			labels = append(labels, row.Label)
			if rec, ok := byID[row.IncreaseID]; ok {
				row.Sentence = s.increaseView(sc, rec, sav).Sentence
			}
		}
	}
	svc, err := s.breakdownBody(ctx, sc, GroupService)
	if err != nil {
		return r, err
	}
	r.SpendByService = svc.Rows
	for _, o := range svc.Options {
		if !o.Available {
			continue
		}
		bd, err := s.breakdownBody(ctx, sc, o.GroupBy)
		if err != nil {
			return r, err
		}
		switch o.GroupBy {
		case GroupAccount:
			r.SpendByAccount = bd.Rows
		case GroupTeam:
			r.SpendByTeam = bd.Rows
		}
	}
	r.Savings.Total = savingsTotal(sav)
	r.Savings.Top = []SavingView{}
	for _, it := range sav {
		if it.Status == "open" && len(r.Savings.Top) < 5 {
			r.Savings.Top = append(r.Savings.Top, it)
		}
	}
	r.Notes.Expected = []IncreaseView{}
	for _, in := range incs {
		if in.Status == StatusExpected {
			r.Notes.Expected = append(r.Notes.Expected, s.increaseView(sc, in, sav))
		}
	}
	sy := sc.header.Sync
	r.Notes.Coverage = CoverageNote{LastCompleteDay: sy.LastCompleteDay, MeasureLabel: "What you actually pay"}
	if sy.AccountsReadable < sy.AccountsTotal {
		r.Notes.Coverage.Accounts = narrative.AccountsNote(sy.AccountsReadable, sy.AccountsTotal)
	} else {
		r.Notes.Coverage.Accounts = narrative.AccountsCount(sy.AccountsTotal)
	}
	r.Notes.Missing = append([]string{}, sy.Notes...)
	r.CopySummary = narrative.CopySummary(r.Summary.Answer, labels, r.Summary.Savings.Monthly, r.Summary.Savings.Count, sc.st.Currency)
	closed := MonthClosed(sc.period.Calendar.Start, sc.today, s.cfg.BillCloseDay)
	r.CanFreeze = !sc.period.Open && closed
	r.NotFinal = !r.CanFreeze
	if !r.CanFreeze {
		r.FreezeNote = narrative.BillOpenNote(sc.period.Calendar.Start.Month, s.cfg.ProviderName, s.cfg.BillCloseDay)
	}
	return r, nil
}

// Reports lists frozen monthly reports.
func (s *Service) Reports(ctx context.Context) ([]ReportRef, error) {
	if s.store == nil {
		return []ReportRef{}, nil
	}
	return s.store.ListReports(ctx)
}

// FreezeReport saves a closed month's report so its numbers never shift.
func (s *Service) FreezeReport(ctx context.Context, month, by string) (ReportView, error) {
	if s.store == nil {
		return ReportView{}, ErrNoStore
	}
	if !monthKey.MatchString(month) {
		return ReportView{}, ErrReportNotMonth
	}
	sc, err := s.loadFor(ctx, Query{Period: month}, detailDims)
	if err != nil {
		return ReportView{}, err
	}
	if !sc.header.Enabled {
		return ReportView{Header: sc.header}, nil
	}
	if fr, err := s.store.GetReport(ctx, month); err != nil {
		return ReportView{}, err
	} else if fr != nil {
		out, err := s.readFrozen(*fr, sc)
		if err != nil {
			return ReportView{}, err
		}
		return out, ErrAlreadyFrozen
	}
	r, err := s.buildReport(ctx, sc)
	if err != nil {
		return r, err
	}
	if !r.CanFreeze {
		return r, ErrBillNotClosed
	}
	r.Frozen = &Frozen{At: sc.now, By: by}
	r.CanFreeze = false
	return r, s.store.PutReport(ctx, r)
}

// SetIncreaseStatus marks an increase expected (reason and end date
// required) or open again.
func (s *Service) SetIncreaseStatus(ctx context.Context, id string, ch StatusChange, by string) (IncreaseView, error) {
	if s.store == nil {
		return IncreaseView{}, ErrNoStore
	}
	sc, err := s.load(ctx, Query{})
	if err != nil {
		return IncreaseView{}, err
	}
	if !sc.header.Enabled {
		return IncreaseView{}, ErrNotFound
	}
	incs, err := s.getIncreases(ctx, sc)
	if err != nil {
		return IncreaseView{}, err
	}
	for _, rec := range incs {
		if rec.ID != id {
			continue
		}
		next, err := ApplyStatusChange(rec, ch, by, sc.today, sc.now)
		if err != nil {
			return IncreaseView{}, err
		}
		if err := s.store.PutIncrease(ctx, next); err != nil {
			return IncreaseView{}, err
		}
		sav, err := s.getSavings(ctx, sc)
		if err != nil {
			return IncreaseView{}, err
		}
		return s.increaseView(sc, next, sav), nil
	}
	return IncreaseView{}, ErrNotFound
}

// DismissSaving dismisses a savings item with a reason.
func (s *Service) DismissSaving(ctx context.Context, id, reason, by string) error {
	if s.store == nil {
		return ErrNoStore
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrReasonRequired
	}
	sc, err := s.load(ctx, Query{})
	if err != nil {
		return err
	}
	if !sc.header.Enabled {
		return ErrNotFound
	}
	items, err := s.getSavings(ctx, sc)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.ID == id {
			return s.store.PutSavingState(ctx, id, SavingState{Dismissed: true, Reason: reason, By: by, At: sc.now.UTC(),
				Account: it.Account, Kind: it.Kind, Title: it.Title})
		}
	}
	return ErrNotFound
}

// PutBudgets replaces the UI budgets. Config and provider budgets are
// read-only; a UI budget may not repeat one of their scopes.
func (s *Service) PutBudgets(ctx context.Context, bs []Budget) ([]Budget, error) {
	if s.store == nil {
		return nil, ErrNoStore
	}
	out := make([]Budget, len(bs))
	for i, b := range bs {
		b.Name = strings.TrimSpace(b.Name)
		b.Value = strings.TrimSpace(b.Value)
		b.Source = BudgetFromUI
		b.ID = BudgetID(b.Scope, b.Value)
		out[i] = b
	}
	all := append(append([]Budget{}, s.cfg.Budgets...), out...)
	if err := ValidateBudgets(all); err != nil {
		return nil, err
	}
	return out, s.store.PutBudgets(ctx, out)
}

// SyncSavingDecisions brings the stored savings decisions in step with the
// savings the cache produces now: a decision whose item is no longer
// produced is marked orphaned, and one whose item is back is cleared. A
// decision stored without its subject (made before the subject was kept, or
// before its payer was known) takes it from the item while the item is
// still produced, so deleting the payer later deletes the decision. It runs
// on the refresh cadence, so a page read never writes. A decision only ever
// applies to an item that is listed, so an orphaned decision is never shown
// as active; when its item returns, the decision applies again. An idle
// saving's ID names its resources, so one that returns with other resources
// is a new item and is listed as new.
func (s *Service) SyncSavingDecisions(ctx context.Context) error {
	if s.store == nil {
		return ErrNoStore
	}
	sc, err := s.load(ctx, Query{})
	if err != nil || !sc.header.Enabled {
		return err
	}
	items, err := s.getSavings(ctx, sc)
	if err != nil {
		return err
	}
	present := make([]Saving, 0, len(items))
	for _, it := range items {
		present = append(present, it.Saving)
	}
	return s.store.SyncSavingStates(ctx, present, sc.now.UTC())
}

// SyncIncreases runs the detector over the cached history, persists the
// current detection (with the time rules applied) and returns the increases
// that must be alerted now. This is the only detection on the read side of
// the service: pages read what it stored. Callers send each alert through the
// alert path and then call MarkIncreaseAlerted.
func (s *Service) SyncIncreases(ctx context.Context) ([]IncreaseRecord, error) {
	if s.store == nil {
		return nil, ErrNoStore
	}
	sc, err := s.loadDetection(ctx)
	if err != nil || !sc.header.Enabled {
		return nil, err
	}
	incs, err := s.detectIncreases(ctx, sc)
	if err != nil {
		return nil, err
	}
	var alert []IncreaseRecord
	for _, r := range incs {
		if err := s.store.PutIncrease(ctx, r); err != nil {
			return nil, err
		}
		if ShouldAlert(r) {
			alert = append(alert, r)
		}
	}
	return alert, nil
}

// MarkIncreaseAlerted records that an increase was alerted.
func (s *Service) MarkIncreaseAlerted(ctx context.Context, id string) error {
	if s.store == nil {
		return ErrNoStore
	}
	recs, err := s.store.ListIncreases(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.ID == id {
			return s.store.PutIncrease(ctx, MarkAlerted(r, s.now()))
		}
	}
	return ErrNotFound
}

func tiedCount(list []SuspectView) int {
	n := 0
	for _, s := range list {
		if s.Tied {
			n++
		}
	}
	return n
}
