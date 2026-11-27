package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AWSCallCount is how many AWS requests the collectors made for one
// account, Region and source in one hour. Every request is a CloudTrail
// management event in the customer's account, so the count is what event
// priced features (GuardDuty, an extra trail copy, a log pipeline) bill
// for. Metered is the part AWS bills directly per request; Runs is how many
// unit runs made the calls.
type AWSCallCount struct {
	AccountID string    `json:"account_id"`
	Region    string    `json:"region"`
	Source    string    `json:"source"`
	Hour      time.Time `json:"hour"`
	Calls     int64     `json:"calls"`
	Metered   int64     `json:"metered"`
	Runs      int64     `json:"runs"`
	// Peak is the most calls one run made in the hour.
	Peak int64 `json:"peak"`
}

// AWSCallCountKey is the row a count adds to.
type AWSCallCountKey struct {
	AccountID string
	Region    string
	Source    string
	Hour      time.Time
}

func (count AWSCallCount) key() AWSCallCountKey {
	return AWSCallCountKey{AccountID: count.AccountID, Region: count.Region, Source: count.Source, Hour: count.Hour}
}

// AWSCallCountQuery selects hours in [Since, Until) for one account, or for
// every account when AccountID is empty. A zero Until has no upper bound.
type AWSCallCountQuery struct {
	AccountID string
	Since     time.Time
	Until     time.Time
}

// DefaultAWSCallCountRetention is how long the call ledger is kept.
const DefaultAWSCallCountRetention = 14 * 24 * time.Hour

// ErrInvalidAWSCallCount is a count with a malformed account, Region or
// source, or a negative number.
var ErrInvalidAWSCallCount = errors.New("storage: invalid aws call count")

// AWSCallCountStore keeps the call ledger. AddAWSCallCounts adds each count
// to its hour's row in the write organization, creating the row when it is
// new, so every run writes once and replicas add up. Reads sum the read
// scope: a call made before a licensed upgrade was still made. It is
// optional beside AWSInventoryStore; every backend with the inventory
// tables has it.
type AWSCallCountStore interface {
	AddAWSCallCounts(ctx context.Context, scope tenancy.OrgScope, counts []AWSCallCount) error
	AWSCallCounts(ctx context.Context, scope tenancy.OrgScope, query AWSCallCountQuery) ([]AWSCallCount, error)
	// PruneAWSCallCounts deletes the write organization's hours before
	// before.
	PruneAWSCallCounts(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error)
}

var (
	_ AWSCallCountStore = (*sqlTables)(nil)
	_ AWSCallCountStore = (*memoryTables)(nil)
)

// AWSCallCountsOf returns the provider's call ledger, if it has one.
func AWSCallCountsOf(provider Provider) (AWSCallCountStore, bool) {
	store, ok := provider.(AWSCallCountStore)
	return store, ok
}

// cleanAWSCallCounts validates counts and folds them per row, with the hour
// truncated. A count of nothing is dropped.
func cleanAWSCallCounts(counts []AWSCallCount) ([]AWSCallCount, error) {
	folded := map[AWSCallCountKey]AWSCallCount{}
	for _, count := range counts {
		if !awsAccountIDPattern.MatchString(count.AccountID) || !validTableKey(count.Region) || !validTableKey(count.Source) ||
			len(count.Region) > 64 || len(count.Source) > 64 || count.Hour.IsZero() ||
			count.Calls < 0 || count.Metered < 0 || count.Runs < 0 || count.Metered > count.Calls || count.Peak < 0 || count.Peak > count.Calls {
			return nil, ErrInvalidAWSCallCount
		}
		if count.Calls == 0 && count.Runs == 0 {
			continue
		}
		count.Hour = tableTime(count.Hour).Truncate(time.Hour)
		kept := folded[count.key()]
		count.Calls += kept.Calls
		count.Metered += kept.Metered
		count.Runs += kept.Runs
		count.Peak = max(count.Peak, kept.Peak)
		folded[count.key()] = count
	}
	out := make([]AWSCallCount, 0, len(folded))
	for _, count := range folded {
		out = append(out, count)
	}
	sortAWSCallCounts(out)
	return out, nil
}

// sortAWSCallCounts orders counts by hour, then account, Region and source.
func sortAWSCallCounts(counts []AWSCallCount) {
	sort.Slice(counts, func(left, right int) bool {
		a, b := counts[left], counts[right]
		switch {
		case !a.Hour.Equal(b.Hour):
			return a.Hour.Before(b.Hour)
		case a.AccountID != b.AccountID:
			return a.AccountID < b.AccountID
		case a.Region != b.Region:
			return a.Region < b.Region
		}
		return a.Source < b.Source
	})
}

func validAWSCallCountQuery(query AWSCallCountQuery) bool {
	return query.AccountID == "" || awsAccountIDPattern.MatchString(query.AccountID)
}

// ---- SQL (Postgres and the local SQLite tables) ----

func (t *sqlTables) AddAWSCallCounts(ctx context.Context, scope tenancy.OrgScope, counts []AWSCallCount) error {
	counts, err := cleanAWSCallCounts(counts)
	if err != nil || len(counts) == 0 {
		return err
	}
	scope = inventoryScope(scope)
	return t.write(ctx, func(tx *sql.Tx) error {
		statement := t.q(`INSERT INTO aws_call_counts (org_id, account_id, region, source, hour, calls, metered, runs, peak)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (org_id, account_id, region, source, hour) DO UPDATE SET
 calls = aws_call_counts.calls + excluded.calls, metered = aws_call_counts.metered + excluded.metered,
 runs = aws_call_counts.runs + excluded.runs,
 peak = CASE WHEN excluded.peak > aws_call_counts.peak THEN excluded.peak ELSE aws_call_counts.peak END`)
		for _, count := range counts {
			if _, err := tx.ExecContext(ctx, statement, scope.Write, count.AccountID, count.Region, count.Source,
				t.at(count.Hour), count.Calls, count.Metered, count.Runs, count.Peak); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *sqlTables) AWSCallCounts(ctx context.Context, scope tenancy.OrgScope, query AWSCallCountQuery) ([]AWSCallCount, error) {
	if !validAWSCallCountQuery(query) {
		return nil, ErrInvalidAWSCallCount
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	var where sqlWhere
	where.add(`org_id IN (`+placeholders(len(scope.Read))+`)`, stringArgs(scope.Read)...)
	where.equal("account_id", query.AccountID)
	if !query.Since.IsZero() {
		where.add("hour >= ?", t.at(query.Since))
	}
	if !query.Until.IsZero() {
		where.add("hour < ?", t.at(query.Until))
	}
	rows, err := db.QueryContext(ctx, t.q(`SELECT account_id, region, source, hour, CAST(SUM(calls) AS BIGINT), CAST(SUM(metered) AS BIGINT), CAST(SUM(runs) AS BIGINT), CAST(MAX(peak) AS BIGINT) FROM aws_call_counts
 WHERE `+where.String()+` GROUP BY account_id, region, source, hour ORDER BY hour, account_id, region, source`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSCallCount, 0)
	for rows.Next() {
		var count AWSCallCount
		var hour tableTimeValue
		if err := rows.Scan(&count.AccountID, &count.Region, &count.Source, &hour, &count.Calls, &count.Metered, &count.Runs, &count.Peak); err != nil {
			return nil, err
		}
		count.Hour = hour.Time
		out = append(out, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *sqlTables) PruneAWSCallCounts(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	scope = inventoryScope(scope)
	var removed int64
	for {
		batch, err := t.sweepDelete(ctx, "aws_call_counts", `org_id = ? AND hour < ?`, []any{scope.Write, t.at(before)}, DefaultAWSRetentionBatch)
		if err != nil {
			return removed, err
		}
		removed += batch
		if batch < DefaultAWSRetentionBatch {
			return removed, nil
		}
	}
}

// ---- Memory ----

func (t *memoryTables) AddAWSCallCounts(_ context.Context, scope tenancy.OrgScope, counts []AWSCallCount) error {
	counts, err := cleanAWSCallCounts(counts)
	if err != nil || len(counts) == 0 {
		return err
	}
	scope = inventoryScope(scope)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.callCounts == nil {
		t.callCounts = map[string]map[AWSCallCountKey]AWSCallCount{}
	}
	own := t.callCounts[scope.Write]
	if own == nil {
		own = map[AWSCallCountKey]AWSCallCount{}
		t.callCounts[scope.Write] = own
	}
	for _, count := range counts {
		kept := own[count.key()]
		count.Calls += kept.Calls
		count.Metered += kept.Metered
		count.Runs += kept.Runs
		count.Peak = max(count.Peak, kept.Peak)
		own[count.key()] = count
	}
	return nil
}

func (t *memoryTables) AWSCallCounts(_ context.Context, scope tenancy.OrgScope, query AWSCallCountQuery) ([]AWSCallCount, error) {
	if !validAWSCallCountQuery(query) {
		return nil, ErrInvalidAWSCallCount
	}
	scope = inventoryScope(scope)
	since, until := tableTime(query.Since), tableTime(query.Until)
	t.mu.RLock()
	defer t.mu.RUnlock()
	summed := map[AWSCallCountKey]AWSCallCount{}
	for _, org := range scope.Read {
		for key, count := range t.callCounts[org] {
			if query.AccountID != "" && key.AccountID != query.AccountID ||
				!since.IsZero() && key.Hour.Before(since) || !until.IsZero() && !key.Hour.Before(until) {
				continue
			}
			kept := summed[key]
			count.Calls += kept.Calls
			count.Metered += kept.Metered
			count.Runs += kept.Runs
			count.Peak = max(count.Peak, kept.Peak)
			summed[key] = count
		}
	}
	out := make([]AWSCallCount, 0, len(summed))
	for _, count := range summed {
		out = append(out, count)
	}
	sortAWSCallCounts(out)
	return out, nil
}

func (t *memoryTables) PruneAWSCallCounts(_ context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	scope = inventoryScope(scope)
	before = tableTime(before)
	t.mu.Lock()
	defer t.mu.Unlock()
	var removed int64
	for key := range t.callCounts[scope.Write] {
		if key.Hour.Before(before) {
			delete(t.callCounts[scope.Write], key)
			removed++
		}
	}
	return removed, nil
}
