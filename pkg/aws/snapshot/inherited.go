package snapshot

import (
	"context"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// ConnectedAccountsFunc answers the accounts connected now. ok is false when
// the list cannot be read (the connector is off, or its settings are
// unreadable); the store then reads everything it would without it.
type ConnectedAccountsFunc func(ctx context.Context) (accounts map[string]bool, ok bool)

// WithConnectedAccounts limits what the store reads from the organizations
// its scope inherits to the accounts connected now. A deployment that adds a
// licence reads the open-source default organization after its own; an
// account removed under the licence keeps rows there that the removal
// cannot delete, and they must not show. The write organization's rows are
// always read, and a scope without inherited organizations is unaffected.
func WithConnectedAccounts(read ConnectedAccountsFunc) Option {
	return func(store *Store) { store.connected = read }
}

// inheritedFilter is one read's view of which accounts keep their inherited
// rows. A nil connected set keeps every row.
type inheritedFilter struct {
	connected map[string]bool
}

// inheritedFilterKey carries one read's filter in its context.
type inheritedFilterKey struct{ store *Store }

// withInherited reads the connected accounts once for a read that reaches
// the store several times, and carries the answer in the context it
// returns so every step of the read uses the same one.
func (store *Store) withInherited(ctx context.Context) (context.Context, inheritedFilter) {
	filter := store.inherited(ctx)
	return context.WithValue(ctx, inheritedFilterKey{store}, filter), filter
}

// ReadContext reads the connected accounts once and carries them in the
// context it returns, so a page that reads the store several times (the
// coverage, the resources, the change feed) filters inherited rows by one
// answer and asks for it once.
func (store *Store) ReadContext(ctx context.Context) context.Context {
	ctx, _ = store.withInherited(ctx)
	return ctx
}

// inherited is the read's filter: the one its context carries, else the
// connected accounts read now.
func (store *Store) inherited(ctx context.Context) inheritedFilter {
	if filter, ok := ctx.Value(inheritedFilterKey{store}).(inheritedFilter); ok {
		return filter
	}
	if store.connected == nil || len(store.scope.Read) < 2 {
		return inheritedFilter{}
	}
	accounts, ok := store.connected(ctx)
	if !ok {
		return inheritedFilter{}
	}
	if accounts == nil {
		accounts = map[string]bool{}
	}
	return inheritedFilter{connected: accounts}
}

// withheld reports whether the account's inherited rows are hidden.
func (filter inheritedFilter) withheld(account string) bool {
	return filter.connected != nil && !filter.connected[account]
}

// limit is the storage form of the filter for the change feed.
func (filter inheritedFilter) limit() *storage.AWSInheritedLimit {
	if filter.connected == nil {
		return nil
	}
	accounts := make([]string, 0, len(filter.connected))
	for account, connected := range filter.connected {
		if connected {
			accounts = append(accounts, account)
		}
	}
	sort.Strings(accounts)
	return &storage.AWSInheritedLimit{Accounts: accounts}
}

// keepsUnit reports whether a unit status is read: every unit of an
// account whose inherited rows are kept, and a unit the write organization
// answers for otherwise.
func (store *Store) keepsUnit(filter inheritedFilter, unit storage.AWSUnitStatus) bool {
	return !filter.withheld(unit.Unit.Account) || unit.Org == store.scope.Write
}

// keepUnits drops the unit statuses keepsUnit hides.
func (store *Store) keepUnits(filter inheritedFilter, units []storage.AWSUnitStatus) []storage.AWSUnitStatus {
	if filter.connected == nil {
		return units
	}
	kept := make([]storage.AWSUnitStatus, 0, len(units))
	for _, unit := range units {
		if store.keepsUnit(filter, unit) {
			kept = append(kept, unit)
		}
	}
	return kept
}

// writeAnswered lists, for each account whose inherited rows are hidden,
// the units the write organization answers for. Reads whose rows carry no
// organization (pairs, history depth) keep such an account's rows only for
// these units.
func (store *Store) writeAnswered(ctx context.Context, filter inheritedFilter, accounts []string) (map[storage.AWSUnit]bool, error) {
	out := map[storage.AWSUnit]bool{}
	seen := map[string]bool{}
	for _, account := range accounts {
		if seen[account] || !filter.withheld(account) {
			continue
		}
		seen[account] = true
		units, err := store.inventory.AWSUnits(ctx, store.scope, storage.AWSUnitQuery{Account: account})
		if err != nil {
			return nil, err
		}
		for _, unit := range units {
			if unit.Org == store.scope.Write {
				out[unit.Unit] = true
			}
		}
	}
	return out, nil
}

// changesByID loads change rows by ID. A row of an account whose inherited
// rows are hidden is kept only when the write organization holds it, which
// the change feed answers under the same limit.
func (store *Store) changesByID(ctx context.Context, feeds storage.AWSChangeFeedStore, ids []string) ([]storage.AWSChange, error) {
	rows, err := feeds.AWSChangesByID(ctx, store.scope, ids)
	if err != nil {
		return nil, err
	}
	filter := store.inherited(ctx)
	if filter.connected == nil {
		return rows, nil
	}
	kept := make([]storage.AWSChange, 0, len(rows))
	withheld := map[string][]storage.AWSChange{}
	for _, row := range rows {
		if filter.withheld(row.Account) {
			withheld[row.Account] = append(withheld[row.Account], row)
			continue
		}
		kept = append(kept, row)
	}
	if len(withheld) == 0 {
		return rows, nil
	}
	limit := filter.limit()
	for account, candidates := range withheld {
		since, until := candidates[0].EventTime, candidates[0].EventTime
		want := map[string]bool{}
		for _, row := range candidates {
			want[row.EventID] = true
			if row.EventTime.Before(since) {
				since = row.EventTime
			}
			if row.EventTime.After(until) {
				until = row.EventTime
			}
		}
		own, err := feeds.AWSChangeFeed(ctx, store.scope, storage.AWSChangeFeedQuery{Account: account, Since: since, Until: until.Add(time.Microsecond),
			IncludeDetail: true, RowLimit: storage.DefaultAWSChangeReadCap, Inherited: limit})
		if err != nil {
			return nil, err
		}
		for _, row := range own.Rows {
			if want[row.EventID] {
				kept = append(kept, row)
			}
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].EventTime.Equal(kept[j].EventTime) {
			return kept[i].EventTime.After(kept[j].EventTime)
		}
		return kept[i].EventID > kept[j].EventID
	})
	return kept, nil
}

// keepHistory drops the history depth of units whose inherited rows are
// hidden and which the write organization does not answer for.
func (store *Store) keepHistory(ctx context.Context, units []storage.AWSUnitHistory) ([]storage.AWSUnitHistory, error) {
	filter := store.inherited(ctx)
	if filter.connected == nil {
		return units, nil
	}
	accounts := make([]string, 0, len(units))
	for _, unit := range units {
		accounts = append(accounts, unit.Unit.Account)
	}
	own, err := store.writeAnswered(ctx, filter, accounts)
	if err != nil {
		return nil, err
	}
	kept := make([]storage.AWSUnitHistory, 0, len(units))
	for _, unit := range units {
		if !filter.withheld(unit.Unit.Account) || own[unit.Unit] {
			kept = append(kept, unit)
		}
	}
	return kept, nil
}
