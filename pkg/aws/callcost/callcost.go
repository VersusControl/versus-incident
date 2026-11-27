// Package callcost answers what Versus's reads cost the customer's account:
// the calls of the last 24 hours from the call ledger, the budgets they ran
// under, what the daily target slowed down, and a month's estimate split
// into what AWS bills directly, GuardDuty analysis and the customer's extra
// trail copies. It reads stores only and never calls AWS.
//
// A read call is almost never billed by its own service. Every call is a
// CloudTrail management event, and features the customer chose price
// events: Versus sets the count, the customer's features set the price. An
// estimate whose multiplier is not known is null, never zero.
package callcost

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// monthDays turns a day of calls into a month.
const monthDays = 30

// callsPerDaySource names the daily target in the budget list.
const callsPerDaySource = "calls_per_day"

// GuardDutyStatus says whether GuardDuty analyses an account's events:
// known is false when nothing has read it.
type GuardDutyStatus func(account string) (on, known bool)

// Inputs are the stores and settings one answer reads.
type Inputs struct {
	Ledger    storage.AWSCallCountStore
	Scope     tenancy.OrgScope
	Snapshots *snapshot.Store
	// Accounts are the connected accounts; nil takes the ledger's.
	Accounts []string
	// Budgets are the per-run budgets by source; CallsPerDay the daily
	// target per account.
	Budgets     map[string]int
	CallsPerDay int
	// GuardDuty reads each account's GuardDuty status; nil is unknown.
	GuardDuty GuardDutyStatus
	// Stretched returns the stretch each account's reads run at now, as
	// the scheduler holds it; nil works it out from the last 24 hours.
	Stretched func() map[string]int
	// LedgerFailedSince reports whether a ledger write failed since a
	// time, so the calls may be short; nil is never.
	LedgerFailedSince func(since time.Time) bool
	Now               time.Time
}

// Estimate is a month at the last 24 hours' rate. A nil amount is not
// known: its multiplier could not be read.
type Estimate struct {
	DirectUSD float64 `json:"direct_usd"`
	// GuardDutyUSD is what GuardDuty analysis of these events costs where
	// it is on. GuardDutyKnown is false while an account's GuardDuty
	// status is not known: the amount then is what it costs if it is on.
	GuardDutyUSD   *float64 `json:"guardduty_usd"`
	GuardDutyKnown bool     `json:"guardduty_known"`
	// ExtraTrailCopiesUSD is what the customer's trails after the first,
	// free copy cost. For the accounts in UnknownAccounts some trails
	// could not be read, so it is a lower bound; it is nil only when no
	// trail could be read at all.
	ExtraTrailCopiesUSD *float64 `json:"extra_trail_copies_usd"`
	// ExtraTrailCopies is the most extra copies any account is known to
	// keep; nil when nothing is known.
	ExtraTrailCopies *int     `json:"extra_trail_copies"`
	UnknownAccounts  []string `json:"unknown_accounts"`
	// LakeGB and SIEMGB are the CloudTrail volume Versus's calls add to a
	// CloudTrail Lake store or a log pipeline, for the user to price.
	LakeGB float64 `json:"lake_gb"`
	SIEMGB float64 `json:"siem_gb"`
}

// Stretched is one source the daily target slowed down for one account.
type Stretched struct {
	Account  string `json:"account"`
	Source   string `json:"source"`
	Factor   int    `json:"factor"`
	Calls24h int64  `json:"calls_24h"`
	Target   int    `json:"target"`
}

// AccountSource is one account's calls of one source in the last 24 hours.
type AccountSource struct {
	Account string `json:"account"`
	Source  string `json:"source"`
	Calls   int64  `json:"calls"`
	Metered int64  `json:"metered"`
}

// Budget is one budget and the most of it used. For a source it is the
// per-run budget and the largest run of the last 24 hours (pages for
// findings); for the daily target it is the target of every connected
// account and the calls of the last 24 hours.
//
// Reached is set when the budget stopped a read: the largest run used all
// of it, or a stored read names a capped gap of this source.
// CoverageSources are then the coverage page's sources (the unit
// services) of those capped reads, so a row there can be matched to its
// budget.
type Budget struct {
	Source          string   `json:"source"`
	Limit           int64    `json:"limit"`
	Used            int64    `json:"used"`
	Unit            string   `json:"unit"`
	Reached         bool     `json:"reached"`
	CoverageSources []string `json:"coverage_sources"`
}

// Optional is a billed feature Versus reads only once it is on, with AWS's
// price, and the accounts where it is known to be off.
type Optional struct {
	catalog.Optional
	// Known is false when no collector reads the feature's status yet.
	Known       bool     `json:"known"`
	AccountsOff []string `json:"accounts_off"`
}

// View is the cost answer.
type View struct {
	Calls24h      int64       `json:"calls_24h"`
	Events24h     int64       `json:"events_24h"`
	Metered24h    int64       `json:"metered_24h"`
	EstimateMonth Estimate    `json:"estimate_month"`
	SourcesCapped int         `json:"sources_capped"`
	Stretched     []Stretched `json:"stretched"`
	// StretchedAccounts counts the accounts whose reads the daily target
	// slowed down.
	StretchedAccounts int             `json:"stretched_accounts"`
	ByAccountSource   []AccountSource `json:"by_account_source"`
	Budgets           []Budget        `json:"budgets"`
	OptionalOff       []Optional      `json:"optional_off"`
	// LedgerIncomplete is set when a ledger write failed in the last 24
	// hours: the counts it kept are added on a later write, so the calls
	// may read short until then.
	LedgerIncomplete bool `json:"ledger_incomplete"`
}

// WindowStart is where the last 24 hours start. The scheduler's daily
// target reads the same window.
func WindowStart(now time.Time) time.Time { return now.UTC().Add(-24 * time.Hour) }

// windowRows reads the ledger hours that overlap the last 24 hours. The
// ledger keeps whole hours, so the hour the window starts in counts only
// for its part inside the window, and the window is a true 24 hours.
func windowRows(ctx context.Context, ledger storage.AWSCallCountStore, scope tenancy.OrgScope, now time.Time) ([]storage.AWSCallCount, error) {
	start := WindowStart(now)
	rows, err := ledger.AWSCallCounts(ctx, scope, storage.AWSCallCountQuery{Since: start.Truncate(time.Hour)})
	if err != nil {
		return nil, err
	}
	for index := range rows {
		row := &rows[index]
		if !row.Hour.Before(start) {
			continue
		}
		share := float64(row.Hour.Add(time.Hour).Sub(start)) / float64(time.Hour)
		row.Calls = int64(math.Round(float64(row.Calls) * share))
		row.Metered = min(int64(math.Round(float64(row.Metered)*share)), row.Calls)
	}
	return rows, nil
}

// Totals sums each account's calls in the last 24 hours.
func Totals(ctx context.Context, ledger storage.AWSCallCountStore, scope tenancy.OrgScope, now time.Time) (map[string]int64, error) {
	rows, err := windowRows(ctx, ledger, scope, now)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, row := range rows {
		out[row.AccountID] += row.Calls
	}
	return out, nil
}

// Read builds the answer from the ledger and the snapshot store.
func Read(ctx context.Context, in Inputs) (View, error) {
	view := View{Stretched: []Stretched{}, ByAccountSource: []AccountSource{}, Budgets: []Budget{}, OptionalOff: []Optional{}}
	var rows []storage.AWSCallCount
	if in.Ledger != nil {
		var err error
		rows, err = windowRows(ctx, in.Ledger, in.Scope, in.Now)
		if err != nil {
			return View{}, err
		}
	}
	accounts := map[string]bool{}
	for _, account := range in.Accounts {
		accounts[account] = true
	}
	if in.Accounts == nil {
		for _, row := range rows {
			accounts[row.AccountID] = true
		}
	}

	type sourceKey struct{ account, source string }
	bySource := map[sourceKey]*AccountSource{}
	perAccount := map[string]int64{}
	perRegion := map[[2]string]int64{}
	peak := map[string]int64{}
	var directUSD float64
	for _, row := range rows {
		if !accounts[row.AccountID] {
			continue
		}
		source := collect.SourceOf(row.Source)
		view.Calls24h += row.Calls
		view.Metered24h += row.Metered
		perAccount[row.AccountID] += row.Calls
		perRegion[[2]string{row.AccountID, row.Region}] += row.Calls
		directUSD += float64(row.Metered) * catalog.MeteredPriceUSD(row.Source)
		key := sourceKey{row.AccountID, source}
		if bySource[key] == nil {
			bySource[key] = &AccountSource{Account: row.AccountID, Source: source}
		}
		bySource[key].Calls += row.Calls
		bySource[key].Metered += row.Metered
		peak[source] = max(peak[source], row.Peak)
	}
	// Every call is one management event.
	view.Events24h = view.Calls24h
	for _, entry := range bySource {
		view.ByAccountSource = append(view.ByAccountSource, *entry)
	}
	sort.Slice(view.ByAccountSource, func(left, right int) bool {
		a, b := view.ByAccountSource[left], view.ByAccountSource[right]
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		return a.Source < b.Source
	})

	monthEvents := float64(view.Events24h * monthDays)
	view.EstimateMonth.DirectUSD = roundCents(directUSD * monthDays)
	gigabytes := math.Round(monthEvents*catalog.EventBytes/1e9*100) / 100
	view.EstimateMonth.LakeGB, view.EstimateMonth.SIEMGB = gigabytes, gigabytes
	view.EstimateMonth.GuardDutyUSD, view.EstimateMonth.GuardDutyKnown = guardDuty(in.GuardDuty, perAccount)
	if in.LedgerFailedSince != nil {
		view.LedgerIncomplete = in.LedgerFailedSince(WindowStart(in.Now))
	}

	var statuses []snapshot.UnitStatus
	if in.Snapshots != nil {
		var err error
		statuses, err = in.Snapshots.Sync(ctx, snapshot.Query{})
		if err != nil {
			return View{}, err
		}
	}
	copies, err := trailCopies(ctx, in.Snapshots, statuses, accounts)
	if err != nil {
		return View{}, err
	}
	view.EstimateMonth.ExtraTrailCopiesUSD, view.EstimateMonth.ExtraTrailCopies, view.EstimateMonth.UnknownAccounts = extraCopies(copies, perRegion)
	view.SourcesCapped = sourcesCapped(statuses, accounts)

	target := in.CallsPerDay
	ids := make([]string, 0, len(accounts))
	for account := range accounts {
		ids = append(ids, account)
	}
	sort.Strings(ids)
	var held map[string]int
	if in.Stretched != nil {
		held = in.Stretched()
	}
	for _, account := range ids {
		factor := collect.StretchFactor(perAccount[account], target)
		if held != nil {
			factor = held[account]
		}
		if factor <= 1 {
			continue
		}
		for _, source := range collect.StretchedSources() {
			view.Stretched = append(view.Stretched, Stretched{Account: account, Source: source, Factor: factor, Calls24h: perAccount[account], Target: target})
		}
	}

	stretchedAccounts := map[string]bool{}
	for _, entry := range view.Stretched {
		stretchedAccounts[entry.Account] = true
	}
	view.StretchedAccounts = len(stretchedAccounts)
	daily := int64(target) * int64(len(accounts))
	view.Budgets = append(view.Budgets, Budget{Source: callsPerDaySource, Limit: daily, Used: view.Calls24h, Unit: "calls",
		Reached: daily > 0 && view.Calls24h >= daily, CoverageSources: []string{}})
	cappedBy := cappedSources(statuses, accounts)
	for _, source := range collect.Sources() {
		unit := "calls"
		if source == collect.SourceFindings {
			unit = "pages"
		}
		limit := int64(in.Budgets[source])
		covered := cappedBy[source]
		if covered == nil {
			covered = []string{}
		}
		view.Budgets = append(view.Budgets, Budget{Source: source, Limit: limit, Used: peak[source], Unit: unit,
			Reached: limit > 0 && peak[source] >= limit || len(covered) > 0, CoverageSources: covered})
	}
	view.OptionalOff = optionalOff(statuses, accounts)
	return view, nil
}

func roundCents(value float64) float64 { return math.Round(value*100) / 100 }

// guardDuty prices the events GuardDuty analyses: every account known to
// have it on, and every account whose status is not known as if it were
// on, with known false. An account known to have it off costs nothing.
func guardDuty(status GuardDutyStatus, perAccount map[string]int64) (*float64, bool) {
	var usd float64
	known := status != nil
	for account, calls := range perAccount {
		on, read := true, false
		if status != nil {
			on, read = status(account)
		}
		if !read {
			known, on = false, true
		}
		if on {
			usd += float64(calls*monthDays) * catalog.PriceGuardDutyEventUSD
		}
	}
	usd = roundCents(usd)
	return &usd, known
}

// regionCopies is what the trails unit of one account and Region found:
// the trails that record read events, and whether any is unknown.
type regionCopies struct {
	recording int
	unknown   int
}

// trailCopies reads the trails units: per account and Region, how many
// trails keep a copy of each read call.
func trailCopies(ctx context.Context, store *snapshot.Store, statuses []snapshot.UnitStatus, accounts map[string]bool) (map[[2]string]regionCopies, error) {
	out := map[[2]string]regionCopies{}
	if store == nil {
		return out, nil
	}
	for _, status := range statuses {
		if status.Unit.Service == collect.TrailsService && accounts[status.Unit.Account] && status.HasData {
			out[[2]string{status.Unit.Account, status.Unit.Region}] = regionCopies{}
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	result, err := store.Resources(ctx, snapshot.Query{Service: collect.TrailsService, Trails: true})
	if err != nil {
		return nil, err
	}
	for _, resource := range result.Resources {
		key := [2]string{resource.Account, resource.Region}
		entry, ok := out[key]
		if !ok {
			continue
		}
		switch resource.Attributes[model.AttrReadEvents] {
		case collect.ReadEventsYes:
			entry.recording++
		case collect.ReadEventsNo:
		default:
			entry.unknown++
		}
		out[key] = entry
	}
	return out, nil
}

// extraCopies prices the copies after the first, free one: each Region's
// calls times its extra copies. Global calls land in us-east-1; an account
// that does not read there takes its largest Region, since a multi-Region
// trail shows in every Region.
//
// A Region is known to keep no extra copy when its trails that record
// reads and its unreadable ones are one at most: the organization trail a
// member cannot read is then the free copy. With more, the unreadable ones
// may or may not add copies, so the known extra copies are a lower bound
// and the account is listed as unknown; so is an account whose trails were
// never read. Only when no Region with calls had its trails read is the
// amount nil.
func extraCopies(copies map[[2]string]regionCopies, perRegion map[[2]string]int64) (*float64, *int, []string) {
	var usd float64
	most, read := 0, false
	unknown := map[string]bool{}
	for key, calls := range perRegion {
		if calls == 0 {
			continue
		}
		account, region := key[0], key[1]
		if region == model.GlobalRegion {
			region = catalog.GlobalHomeRegion
		}
		entry, ok := copies[[2]string{account, region}]
		if !ok && key[1] == model.GlobalRegion {
			entry, ok = largestRegion(copies, account)
		}
		if !ok {
			unknown[account] = true
			continue
		}
		read = true
		if entry.unknown > 0 && entry.recording+entry.unknown > 1 {
			unknown[account] = true
		}
		extra := max(entry.recording-1, 0)
		most = max(most, extra)
		usd += float64(calls*monthDays*int64(extra)) * catalog.PriceTrailCopyEventUSD
	}
	accounts := make([]string, 0, len(unknown))
	for account := range unknown {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	if !read {
		return nil, nil, accounts
	}
	usd = roundCents(usd)
	return &usd, &most, accounts
}

func largestRegion(copies map[[2]string]regionCopies, account string) (regionCopies, bool) {
	var best regionCopies
	found := false
	for key, entry := range copies {
		if key[0] != account {
			continue
		}
		found = true
		best.unknown = max(best.unknown, entry.unknown)
		best.recording = max(best.recording, entry.recording)
	}
	return best, found
}

// sourcesCapped counts the account sources whose last read was capped.
func sourcesCapped(statuses []snapshot.UnitStatus, accounts map[string]bool) int {
	capped := map[[2]string]bool{}
	for _, status := range statuses {
		if !accounts[status.Unit.Account] {
			continue
		}
		for _, gap := range status.Sync.Gaps {
			if gap.Reason == model.GapCapped {
				capped[[2]string{status.Unit.Account, status.Unit.Service}] = true
			}
		}
	}
	return len(capped)
}

// cappedSources lists, per budget source, the unit services whose stored
// reads name a capped gap: the coverage page's capped rows.
func cappedSources(statuses []snapshot.UnitStatus, accounts map[string]bool) map[string][]string {
	seen := map[string]map[string]bool{}
	for _, status := range statuses {
		if !accounts[status.Unit.Account] {
			continue
		}
		for _, gap := range status.Sync.Gaps {
			if gap.Reason != model.GapCapped {
				continue
			}
			source := collect.SourceOf(status.Unit.Service)
			if seen[source] == nil {
				seen[source] = map[string]bool{}
			}
			seen[source][status.Unit.Service] = true
		}
	}
	out := map[string][]string{}
	for source, services := range seen {
		for service := range services {
			out[source] = append(out[source], service)
		}
		sort.Strings(out[source])
	}
	return out
}

// optionalOff lists the billed features, each with the accounts a stored
// read shows it off in.
func optionalOff(statuses []snapshot.UnitStatus, accounts map[string]bool) []Optional {
	out := []Optional{}
	for _, feature := range catalog.Optionals() {
		entry := Optional{Optional: feature, Known: feature.Unit != "", AccountsOff: []string{}}
		if feature.Unit != "" {
			off := map[string]bool{}
			for _, status := range statuses {
				if status.Unit.Service != feature.Unit || !accounts[status.Unit.Account] {
					continue
				}
				if reason := model.GapReasonForCode(status.Sync.ErrorCode); status.Sync.ErrorCode != "" && (reason == model.GapNotOnPlan || reason == model.GapNotEnabled) {
					off[status.Unit.Account] = true
				}
				for _, gap := range status.Sync.Gaps {
					if gap.Reason == model.GapNotOnPlan || gap.Reason == model.GapNotEnabled {
						off[status.Unit.Account] = true
					}
				}
			}
			for account := range off {
				entry.AccountsOff = append(entry.AccountsOff, account)
			}
			sort.Strings(entry.AccountsOff)
		}
		out = append(out, entry)
	}
	return out
}
