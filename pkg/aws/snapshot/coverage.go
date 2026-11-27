package snapshot

import (
	"context"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// regionDisabledCode is the unit error code for a Region the account has not
// turned on. Its unit-level gap is a Region that is off, not a service.
const regionDisabledCode = "region_disabled"

// CoverageScope narrows the coverage read. Account and Region filter the
// units; a Region filter keeps the global units, which serve every Region.
//
// Expected is the configured accounts with their Regions, from settings,
// and ExpectedKnown says it was read. When known, it is the scope: units of
// an account no longer configured, or in a Region the account no longer
// selects, are left out (global units always stay), and a configured
// account with nothing read, or failing as a whole, is a Not read row.
// Unknown reads the stored units alone.
type CoverageScope struct {
	Account       string
	Region        string
	Expected      []ScopeAccount
	ExpectedKnown bool
}

// AccountSource is the source of a row about a whole account rather than
// one service: the account is configured but nothing could be read from it.
const AccountSource = "account"

// Details of an account row: no read has finished in the account yet, or
// the account's own failure (account_disconnected, credentials_invalid,
// assume_role_failed, account_mismatch) from settings.
const AccountNotReadYet = "not_read_yet"

// AccountFirstReadInProgress is the detail of an account row while the
// account's first read runs: some units committed, others never attempted
// yet. Its counts are lower bounds until every unit has read once.
const AccountFirstReadInProgress = "first_read_in_progress"

// CoverageRow is one source and operation that did not read in full,
// across every account it affects.
type CoverageRow struct {
	// Source is the unit service, or AccountSource for a whole account;
	// Operation the detail call, empty when the whole unit did not read.
	Source    string            `json:"source"`
	Operation string            `json:"operation,omitempty"`
	Outcome   model.ReadOutcome `json:"outcome"`
	Reason    model.GapReason   `json:"reason"`
	// Detail names why an account row is not read; empty on other rows.
	Detail   string   `json:"detail,omitempty"`
	Accounts []string `json:"accounts"`
	// Seen and Total add up the per-item counts of every account: "28 of
	// 35 functions". They are set only when every gap behind the row was
	// counted. A capped row with Total zero reads "more than Seen".
	Seen  int `json:"seen,omitempty"`
	Total int `json:"total,omitempty"`
	// LastGoodAt is the oldest last good read among the affected gaps.
	// NeverRead says one of the affected units has never read at all. Both
	// empty means the last good read is not known: a detail call that
	// failed on a unit which otherwise committed, whose earlier detail
	// reads the store does not date.
	LastGoodAt time.Time `json:"last_good_at,omitzero"`
	NeverRead  bool      `json:"never_read,omitempty"`
	// Fix is the IAM actions that allow the denied read: the detail
	// call's own action, or the source's when the whole unit was denied.
	// Empty for any other reason.
	Fix     []string `json:"fix,omitempty"`
	Hides   string   `json:"hides,omitempty"`
	CantRun string   `json:"cant_run,omitempty"`
}

// CoverageSummary is the summary card of the coverage page.
type CoverageSummary struct {
	AccountsRead  int `json:"accounts_read"`
	AccountsTotal int `json:"accounts_total"`
	NotRead       int `json:"not_read"`
	Capped        int `json:"capped"`
	OffInAWS      int `json:"off_in_aws"`
	// ResourcesNotRead counts the resources whose detail a call budget
	// skipped on their last read: their flags, grants and target health
	// are not read, never clear.
	ResourcesNotRead int `json:"resources_not_read"`
}

// CoverageNotGaps counts answers that leave nothing missing: services AWS
// does not offer in a Region (each service and Region once) and Regions
// the accounts have not turned on (each Region once).
//
// NotSet counts the settings AWS answered as not set (no bucket policy, no
// public access block, no tag set). The collectors do not record those
// answers yet, so it is null: not counted, never zero.
type CoverageNotGaps struct {
	ServicesNotOffered int  `json:"services_not_offered"`
	RegionsNotEnabled  int  `json:"regions_not_enabled"`
	NotSet             *int `json:"not_set"`
}

// Account read states: every page that shows an account says one of
// these, from the coverage read.
const (
	// AccountRead: every source of the account read.
	AccountRead = "read"
	// AccountPartial: the account read, but some sources did not, were
	// capped, or have not had their first read yet; its counts are lower
	// bounds.
	AccountPartial = "partial"
	// AccountNotRead: nothing was read from the account.
	AccountNotRead = "not_read"
)

// CoverageAccount is one account's read state. Reason says why it is not
// read (an outage detail, not_read_yet, or the cause class of its first
// source that failed), and why a partial account is partial while its
// first read runs.
type CoverageAccount struct {
	Account string `json:"account"`
	Read    string `json:"read"`
	Reason  string `json:"reason,omitempty"`
}

// CoverageView is what the stored reads cover: the gaps grouped by outcome
// and the answers that are not gaps. Accounts are the read states of the
// accounts in scope and Regions the Regions the scope reads, so every page
// counts accounts and Regions alike.
type CoverageView struct {
	Accounts []CoverageAccount `json:"accounts"`
	Regions  []string          `json:"regions"`
	Summary  CoverageSummary   `json:"summary"`
	NotRead  []CoverageRow     `json:"not_read"`
	Capped   []CoverageRow     `json:"capped"`
	OffInAWS []CoverageRow     `json:"off_in_aws"`
	NotGaps  CoverageNotGaps   `json:"not_gaps"`
}

// WarningLine is the one line a summary card adds when data is incomplete:
// how many sources were not read.
type WarningLine struct {
	NotRead int `json:"not_read"`
}

// Coverage reads the stored unit states, never AWS, and groups every gap
// per source, operation and reason across accounts, so one denial in four
// accounts is one row naming four accounts. A unit that has never been
// attempted is still on its first read and is not listed on its own; an
// account with nothing attempted yet is one account row.
func (store *Store) Coverage(ctx context.Context, scope CoverageScope) (CoverageView, error) {
	view := CoverageView{NotRead: []CoverageRow{}, Capped: []CoverageRow{}, OffInAWS: []CoverageRow{}, Accounts: []CoverageAccount{}, Regions: []string{}}
	filter := store.inherited(ctx)
	units, err := store.units(filter, scope.Account, "", "", time.Time{})
	if err != nil {
		return view, err
	}

	type rowKey struct {
		source, operation, detail string
		reason                    model.GapReason
	}
	type group struct {
		row       CoverageRow
		accounts  map[string]bool
		neverRead bool
		// undated counts gaps with no last good time on a unit that has
		// read; counted and uncounted count gaps with and without Seen or
		// Total; totalKnown is false when a counted gap had no total.
		undated, counted, uncounted int
		totalKnown                  bool
	}
	groups := make(map[rowKey]*group)
	add := func(key rowKey, outcome model.ReadOutcome) *group {
		entry := groups[key]
		if entry == nil {
			entry = &group{
				row:        CoverageRow{Source: key.source, Operation: key.operation, Detail: key.detail, Outcome: outcome, Reason: key.reason},
				accounts:   make(map[string]bool),
				totalKnown: true,
			}
			groups[key] = entry
		}
		return entry
	}
	accounts := make(map[string]bool)
	readAccounts := make(map[string]bool)
	attempted := make(map[string]bool)
	// everRead holds the accounts with a unit that read, whatever their
	// account rows say.
	everRead := make(map[string]bool)
	// pending holds the accounts with a unit never attempted yet.
	pending := make(map[string]bool)
	notOffered := make(map[string]bool)
	regionsOff := make(map[string]bool)
	expected := make(map[string]ScopeAccount)
	configuredRegions := make(map[string]map[string]bool)

	for _, entry := range scope.Expected {
		if scope.Account != "" && entry.Account != scope.Account {
			continue
		}
		accounts[entry.Account] = true
		expected[entry.Account] = entry
		if len(entry.Regions)+len(entry.NotEnabled) > 0 {
			regions := make(map[string]bool, len(entry.Regions)+len(entry.NotEnabled))
			for _, region := range entry.Regions {
				regions[region] = true
			}
			for _, region := range entry.NotEnabled {
				regions[region] = true
			}
			configuredRegions[entry.Account] = regions
		}
		for _, region := range entry.NotEnabled {
			if scope.Region == "" || region == scope.Region {
				regionsOff[region] = true
			}
		}
	}

	for _, unit := range units {
		status, gaps := statusAt(unit, time.Time{})
		account, unitRegion := status.Unit.Account, status.Unit.Region
		if scope.Region != "" && unitRegion != scope.Region && unitRegion != model.GlobalRegion {
			continue
		}
		if scope.ExpectedKnown {
			if _, ok := expected[account]; !ok {
				continue
			}
			if regions, ok := configuredRegions[account]; ok && unitRegion != model.GlobalRegion && !regions[unitRegion] {
				continue
			}
		}
		accounts[account] = true
		unitRead := !status.Sync.LastGoodAt.IsZero() || status.HasData
		if unitRead {
			readAccounts[account] = true
			everRead[account] = true
		}
		if status.Sync.State == model.SyncPending || status.Sync.State == "" {
			pending[account] = true
			continue
		}
		attempted[account] = true
		for _, gap := range gaps {
			if gap.Reason == model.GapNotEnabled && gap.Operation == "" && status.Sync.ErrorCode == regionDisabledCode {
				regionsOff[unitRegion] = true
				continue
			}
			outcome := gap.Outcome()
			if outcome == model.OutcomeNotOffered {
				notOffered[status.Unit.Service+"/"+unitRegion] = true
				continue
			}
			entry := add(rowKey{source: status.Unit.Service, operation: gap.Operation, reason: gap.Reason}, outcome)
			entry.accounts[account] = true
			if gap.Seen > 0 || gap.Total > 0 {
				entry.counted++
				entry.row.Seen += gap.Seen
				entry.row.Total += gap.Total
				if gap.Total == 0 {
					entry.totalKnown = false
				}
			} else {
				entry.uncounted++
			}
			switch last := gap.LastGoodAt; {
			case !last.IsZero():
				if entry.row.LastGoodAt.IsZero() || last.Before(entry.row.LastGoodAt) {
					entry.row.LastGoodAt = last
				}
			case unitRead:
				entry.undated++
			default:
				entry.neverRead = true
			}
		}
	}

	// A configured account that is failing as a whole, or has nothing
	// attempted yet in a Region it can read, is not read, whatever its
	// units say.
	if scope.ExpectedKnown {
		for account, entry := range expected {
			detail := ""
			switch {
			case entry.Outage != "":
				detail = string(entry.Outage)
			case entry.Disconnected:
				detail = string(GapAccountDisconnected)
			case !attempted[account] && len(entry.Regions) == 0 && len(entry.NotEnabled) > 0:
				// Every selected Region is off, so nothing is read there:
				// Regions that are off are not gaps.
				continue
			case !attempted[account]:
				detail = AccountNotReadYet
			case pending[account]:
				// The first read runs: what it committed is read, the
				// rest is not yet, so the account still counts as read
				// and the row raises the warning.
				group := add(rowKey{source: AccountSource, detail: AccountFirstReadInProgress, reason: model.GapNotChecked}, model.OutcomeNotRead)
				group.accounts[account] = true
				group.uncounted++
				group.undated++
				continue
			default:
				continue
			}
			group := add(rowKey{source: AccountSource, detail: detail, reason: model.GapNotChecked}, model.OutcomeNotRead)
			group.accounts[account] = true
			group.uncounted++
			if readAccounts[account] {
				group.undated++
			} else {
				group.neverRead = true
			}
			delete(readAccounts, account)
		}
	}

	for _, entry := range groups {
		row := entry.row
		switch {
		case entry.neverRead:
			row.LastGoodAt, row.NeverRead = time.Time{}, true
		case entry.undated > 0:
			row.LastGoodAt = time.Time{}
		}
		// A count covers the row only when every gap behind it was counted.
		if entry.uncounted > 0 {
			row.Seen, row.Total = 0, 0
		} else if !entry.totalKnown {
			row.Total = 0
		}
		row.Accounts = make([]string, 0, len(entry.accounts))
		for account := range entry.accounts {
			row.Accounts = append(row.Accounts, account)
		}
		sort.Strings(row.Accounts)
		impact := catalog.ImpactFor(row.Source, row.Operation)
		switch row.Outcome {
		case model.OutcomeOffInAWS:
			row.CantRun = impact.CantRun
		default:
			row.Hides = impact.Hides
		}
		if row.Reason == model.GapDenied {
			row.Fix = catalog.FixFor(row.Source, row.Operation)
		}
		switch row.Outcome {
		case model.OutcomeCapped:
			view.Capped = append(view.Capped, row)
		case model.OutcomeOffInAWS:
			view.OffInAWS = append(view.OffInAWS, row)
		default:
			view.NotRead = append(view.NotRead, row)
		}
	}
	for _, rows := range [][]CoverageRow{view.NotRead, view.Capped, view.OffInAWS} {
		sortCoverageRows(rows)
	}

	for account := range readAccounts {
		if accounts[account] {
			view.Summary.AccountsRead++
		}
	}
	view.Accounts = accountStates(accounts, everRead, view)
	view.Regions = scopeRegions(scope)
	view.Summary.AccountsTotal = len(accounts)
	view.Summary.NotRead = len(view.NotRead)
	view.Summary.Capped = len(view.Capped)
	view.Summary.OffInAWS = len(view.OffInAWS)
	resources, err := store.resources(filter, Query{Account: scope.Account, Region: scope.Region, PolicyEntries: true}, false)
	if err != nil {
		return view, err
	}
	for _, resource := range resources.Resources {
		if len(model.NotReadOperations(resource)) > 0 {
			view.Summary.ResourcesNotRead++
		}
	}
	view.NotGaps.ServicesNotOffered = len(notOffered)
	view.NotGaps.RegionsNotEnabled = len(regionsOff)
	return view, nil
}

// Warning is the summary-card warning line for the scope: nil when every
// source read, otherwise how many sources were not read. Capped and Off in
// AWS rows do not raise it.
func (store *Store) Warning(ctx context.Context, scope CoverageScope) (*WarningLine, error) {
	view, err := store.Coverage(ctx, scope)
	if err != nil {
		return nil, err
	}
	if view.Summary.NotRead == 0 {
		return nil, nil
	}
	return &WarningLine{NotRead: view.Summary.NotRead}, nil
}

// sortCoverageRows puts the widest gaps first, then orders by source,
// operation and reason so the page is stable.
func sortCoverageRows(rows []CoverageRow) {
	sort.Slice(rows, func(left, right int) bool {
		a, b := rows[left], rows[right]
		if len(a.Accounts) != len(b.Accounts) {
			return len(a.Accounts) > len(b.Accounts)
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		if a.Detail != b.Detail {
			return a.Detail < b.Detail
		}
		return a.Reason < b.Reason
	})
}

// accountStates is each account's read state from one coverage read.
func accountStates(accounts, everRead map[string]bool, view CoverageView) []CoverageAccount {
	detail := map[string]string{}
	failedAs := map[string]string{}
	partial := map[string]bool{}
	for _, rows := range [][]CoverageRow{view.NotRead, view.Capped} {
		for _, row := range rows {
			for _, account := range row.Accounts {
				partial[account] = true
				if row.Source == AccountSource {
					detail[account] = row.Detail
				} else if _, ok := failedAs[account]; !ok && row.Reason != "" {
					failedAs[account] = string(row.Reason)
				}
			}
		}
	}
	ids := make([]string, 0, len(accounts))
	for account := range accounts {
		ids = append(ids, account)
	}
	sort.Strings(ids)
	out := make([]CoverageAccount, 0, len(ids))
	for _, account := range ids {
		state := CoverageAccount{Account: account, Read: AccountRead}
		switch {
		case !everRead[account]:
			state.Read, state.Reason = AccountNotRead, AccountNotReadYet
			switch {
			case detail[account] != "" && detail[account] != AccountNotReadYet:
				state.Reason = detail[account]
			case failedAs[account] != "":
				state.Reason = failedAs[account]
			}
		case partial[account]:
			state.Read = AccountPartial
			if detail[account] == AccountFirstReadInProgress {
				state.Reason = AccountFirstReadInProgress
			}
		}
		out = append(out, state)
	}
	return out
}

// scopeRegions lists the Regions the scope reads: each expected account's
// selected Regions, those AWS reports as not enabled included, within the
// Region filter. The global Region is not one.
func scopeRegions(scope CoverageScope) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range scope.Expected {
		if scope.Account != "" && entry.Account != scope.Account {
			continue
		}
		for _, list := range [][]string{entry.Regions, entry.NotEnabled} {
			for _, region := range list {
				if region == model.GlobalRegion || seen[region] || scope.Region != "" && region != scope.Region {
					continue
				}
				seen[region] = true
				out = append(out, region)
			}
		}
	}
	sort.Strings(out)
	return out
}
