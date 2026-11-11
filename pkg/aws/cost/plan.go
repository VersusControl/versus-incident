package cost

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Kind is what started a refresh.
type Kind string

const (
	// KindScheduled is the once-a-day refresh: first setup when a payer has
	// no history, else the last days, plus the month close when it is due.
	KindScheduled Kind = "scheduled"
	// KindManual is "Refresh now": the account and usage tables only, at
	// most once a day.
	KindManual Kind = "manual"
)

// Paid call names. They are the names the page lists when a call is
// skipped.
const (
	callAccount   = "cost_by_account"
	callUsage     = "cost_by_usage"
	callTeam      = "cost_by_team"
	callRecord    = "cost_by_record"
	callForecast  = "forecast"
	callCoverage  = "discount_coverage"
	callResources = "resource_detail"
	callDrill     = "drill_down"
)

// Refresh modes; each is one commit group per payer.
const (
	modeSetup    = "setup"
	modeDaily    = "daily"
	modeBackfill = "backfill"
	modeClose    = "close"
	modeManual   = "manual"
)

const (
	// dailyLookbackDays re-reads recent days so AWS restatements land.
	dailyLookbackDays = 3
	// shortWindowDays is the longest window the unlearned page estimate
	// treats as one page per call.
	shortWindowDays = dailyLookbackDays + 1
	// billCloseDay is the day of the next month AWS has closed the bill.
	billCloseDay = 5
	// accountsPerPage is the rough number of linked accounts one page of an
	// account-grouped daily request holds.
	accountsPerPage = 50
	// coverageDays is the window of the discount coverage figure.
	coverageDays = 30
	// resourceDays is the resource-level window Cost Explorer keeps.
	resourceDays = 14
	// resourceRetryDays waits before asking again for resource-level cost
	// after the payer answered that it is off.
	resourceRetryDays = 7
	// drillCooldownDays keeps a drilled pair from being drilled again soon.
	drillCooldownDays = 7
)

// Drop order of optional calls when the plan does not fit: resource detail
// first, then drill-downs, then discount coverage, then the forecast.
const (
	dropResources = 1 + iota
	dropDrill
	dropCoverage
	dropForecast
)

// groupSpec is one commit group: a payer, a mode and its window.
type groupSpec struct {
	payer  string
	mode   string
	window window
}

func groupName(payer, mode string) string { return payer + "|" + mode }

// planned is the full, unpriced plan and its groups.
type planned struct {
	calls  []finops.Call
	groups map[string]groupSpec
}

func prevMonth(today finops.Date) finops.Date { return today.MonthStart().AddDays(-1).MonthStart() }

func closeDue(p *payerState, today finops.Date) bool {
	prev := prevMonth(today)
	return p.hasData() && today.Day >= billCloseDay && p.ClosedThrough < prev.MonthKey() && !p.HistoryStart.After(prev)
}

// historyTarget is the first day a payer's daily history should reach.
func historyTarget(today finops.Date) finops.Date { return prevMonth(today) }

func pagesFor(accounts int, long bool) int {
	n := 1 + max(0, accounts-1)/accountsPerPage
	if long {
		n *= 2
	}
	return n
}

// requiredCalls are the four tables that make a day.
var requiredCalls = []string{callAccount, callUsage, callTeam, callRecord}

// pageRate prices one call for a window from the pages it last used. Until
// a call has run, it borrows the account table's rate when the call is
// also per account, else a fixed guess. A count cut short by the cap is a
// lower bound, so it is doubled.
func pageRate(ps *payerState, organization bool, call string, days int) int {
	st, ok := ps.Pages[call]
	if !ok && (call == callRecord || (call == callTeam && organization)) {
		st, ok = ps.Pages[callAccount]
	}
	if !ok || st.Pages <= 0 || st.Days <= 0 {
		accounts := 1
		if call == callAccount || call == callRecord || (call == callTeam && organization) {
			accounts = max(1, len(ps.Accounts))
		}
		return pagesFor(accounts, days > shortWindowDays)
	}
	pages := st.Pages
	if !st.Done {
		pages *= 2
	}
	if call == callDrill {
		// A drill always reads the same two-month window.
		return max(1, pages)
	}
	return max(1, (pages*days+st.Days-1)/st.Days)
}

func requiredEstimate(ps *payerState, organization bool, days int) int {
	n := 0
	for _, c := range requiredCalls {
		n += pageRate(ps, organization, c, days)
	}
	return n
}

// fitWindow shortens a window from its start until its four tables fit the
// budget, keeping the newest days. It keeps at least one day; a day that
// still does not fit is dropped by the plan and named as skipped.
func fitWindow(ps *payerState, organization bool, w window, budget int) window {
	for w.Start.Before(w.End) && requiredEstimate(ps, organization, window{w.Start, w.End}.days()) > budget {
		w.Start = w.Start.AddDays(1)
	}
	return w
}

func (w window) days() int { return w.Start.DaysUntil(w.End) + 1 }

// planCalls describes every call the refresh wants and prices it from the
// pages each call used before. Payers run in account order; within a payer
// the four daily tables come first. A first setup reads the current month
// for an Organization (the previous month for one account too), shortened
// to the budget; earlier days are backfilled on later runs with what is
// left after the day's own calls.
func planCalls(o Options, st *statusRecord, removed map[string]bool, kind Kind, today finops.Date, remaining int) planned {
	pl := planned{groups: map[string]groupSpec{}}
	payers := make([]Payer, 0, len(o.Payers))
	for _, p := range o.Payers {
		if !removed[p.AccountID] {
			payers = append(payers, p)
		}
	}
	sort.Slice(payers, func(i, j int) bool { return payers[i].AccountID < payers[j].AccountID })
	budget := remaining
	type later struct {
		p    Payer
		ps   *payerState
		base int
	}
	var backfills []later
	for i, p := range payers {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			ps = &payerState{}
		}
		// A payer whose whole work already committed today is not paid for
		// again when a later tick resumes a partial refresh of the same
		// kind. A manual run never satisfies the scheduled plan. Once the
		// whole refresh succeeded, its day mark keeps the estimate
		// describing a normal run.
		if kind == KindScheduled && st.ScheduledOn != today && ps.ScheduledRefreshedOn == today {
			continue
		}
		if kind == KindManual && st.ManualOn != today && ps.ManualRefreshedOn == today {
			continue
		}
		org := p.Organization
		base := i * 100
		add := func(group, name string, prio, requests int, required bool, drop int) {
			pl.calls = append(pl.calls, finops.Call{Name: name, Group: group, Priority: base + prio, Requests: requests, Required: required, DropOrder: drop})
			budget -= requests
		}
		addTables := func(group string, prio int, w window, calls []string) {
			for k, c := range calls {
				add(group, c, prio+k, pageRate(ps, org, c, w.days()), true, 0)
			}
		}
		if kind == KindManual {
			g := groupName(p.AccountID, modeManual)
			w := window{today.AddDays(-dailyLookbackDays), today}
			pl.groups[g] = groupSpec{payer: p.AccountID, mode: modeManual, window: w}
			addTables(g, 1, w, []string{callAccount, callUsage})
			continue
		}
		mode, w := modeDaily, window{today.AddDays(-dailyLookbackDays), today}
		if !ps.hasData() {
			mode, w = modeSetup, window{historyTarget(today), today}
			if org {
				w.Start = today.MonthStart()
			}
			w = fitWindow(ps, org, w, budget)
		}
		g := groupName(p.AccountID, mode)
		pl.groups[g] = groupSpec{payer: p.AccountID, mode: mode, window: w}
		addTables(g, 1, w, requiredCalls)
		if org {
			add(g, callDrill, 5, o.MaxDrillDowns, false, dropDrill)
		}
		add(g, callForecast, 6, 1, false, dropForecast)
		add(g, callCoverage, 7, 1, false, dropCoverage)
		// Resource-level cost for services with an open increase (the first
		// optional call dropped), unless it failed this week: a payer
		// without resource-level data turned on would pay for nothing.
		resourceOff := !ps.ResourceFailedOn.IsZero() && ps.ResourceFailedOn.AddDays(resourceRetryDays).After(today)
		if !org && !resourceOff && o.ResourceServices != nil && len(o.ResourceServices()) > 0 {
			add(g, callResources, 8, 1, false, dropResources)
		}
		if mode == modeDaily && closeDue(ps, today) {
			prev := prevMonth(today)
			cg := groupName(p.AccountID, modeClose)
			cw := window{prev, prev.MonthEnd()}
			pl.groups[cg] = groupSpec{payer: p.AccountID, mode: modeClose, window: cw}
			addTables(cg, 20, cw, requiredCalls)
		}
		if mode == modeDaily && ps.HistoryStart.After(historyTarget(today)) {
			backfills = append(backfills, later{p, ps, base})
		}
	}
	for _, b := range backfills {
		end := b.ps.HistoryStart.AddDays(-1)
		w := window{historyTarget(today), end}
		if requiredEstimate(b.ps, b.p.Organization, 1) > budget {
			continue
		}
		w = fitWindow(b.ps, b.p.Organization, w, budget)
		g := groupName(b.p.AccountID, modeBackfill)
		pl.groups[g] = groupSpec{payer: b.p.AccountID, mode: modeBackfill, window: w}
		for k, c := range requiredCalls {
			n := pageRate(b.ps, b.p.Organization, c, w.days())
			pl.calls = append(pl.calls, finops.Call{Name: c, Group: g, Priority: b.base + 40 + k, Requests: n, Required: true})
			budget -= n
		}
	}
	return pl
}

// suggestedCap is what a normal day costs every payer with full detail,
// priced from the pages each call used: the daily tables, drill-downs,
// forecast, coverage and resource detail.
func suggestedCap(o Options, st *statusRecord) int {
	n := 0
	for _, p := range o.Payers {
		ps := st.Payers[p.AccountID]
		if ps == nil {
			ps = &payerState{}
		}
		n += requiredEstimate(ps, p.Organization, dailyLookbackDays+1) + 2
		if p.Organization {
			n += o.MaxDrillDowns
		} else if o.ResourceServices != nil && len(o.ResourceServices()) > 0 {
			n++
		}
	}
	return n
}
