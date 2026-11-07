package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// evidenceJournalMonths lists the start of every monthly partition of the
// journal, oldest first. A journal that is not partitioned yet has none.
func evidenceJournalMonths(ctx context.Context, tx *sql.Tx) ([]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent=to_regclass('kubernetes_evidence_events') AND c.relname LIKE 'evidence_event_month_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	months := []time.Time{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		month, parseErr := time.Parse("200601", strings.TrimPrefix(name, "evidence_event_month_"))
		if parseErr == nil {
			months = append(months, month)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(months, func(i, j int) bool { return months[i].Before(months[j]) })
	return months, nil
}

// scanObservedLag reads one month of one scope's history written before the
// scope's observed lag was kept, newest month first, and folds the longest
// delay between a crash or coverage-gap record's time and its observation
// into it. When the oldest month is read the lag is complete and reads may
// bound what they search by it. Each step reads one month through the event
// type index and is planned for that month alone.
func (p *postgresProvider) scanObservedLag(ctx context.Context) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'; SET LOCAL plan_cache_mode=force_custom_plan`); err != nil {
		return false, err
	}
	var key string
	var cursor time.Time
	err = tx.QueryRowContext(ctx, `SELECT scope_key,observed_lag_scan FROM kubernetes_evidence_scopes WHERE observed_lag_scan IS NOT NULL ORDER BY scope_key LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&key, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		var locked bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_scopes WHERE observed_lag_scan IS NOT NULL)`).Scan(&locked); err != nil {
			return false, err
		}
		return locked, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	months, err := evidenceJournalMonths(ctx, tx)
	if err != nil || len(months) == 0 {
		// Before the journal is partitioned there is nothing to bound.
		return false, err
	}
	var month time.Time
	for i := len(months) - 1; i >= 0; i-- {
		if months[i].Before(cursor) {
			month = months[i]
			break
		}
	}
	if month.IsZero() {
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET observed_lag_scan=NULL WHERE scope_key=$1`, key); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	var lag sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT MAX(observed_at-event_at)::text FROM kubernetes_evidence_events
 WHERE scope_key=$1 AND event_type IN('crash','coverage_gap') AND event_at>=$2 AND event_at<$3`, key, month, month.AddDate(0, 1, 0)).Scan(&lag); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET observed_lag=GREATEST(observed_lag,COALESCE($2::interval,'0')),observed_lag_scan=$3 WHERE scope_key=$1`, key, lag, month); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// evidenceObservedLag is the longest delay between a crash or coverage-gap
// record's time and its observation over the scopes, and whether it is known
// for all of them.
func evidenceObservedLag(ctx context.Context, tx *sql.Tx, keys []string) (time.Duration, bool, error) {
	var seconds sql.NullFloat64
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXTRACT(EPOCH FROM MAX(observed_lag))::float8,BOOL_OR(observed_lag_scan IS NOT NULL) FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[])`, keys).Scan(&seconds, &pending); err != nil {
		return 0, false, err
	}
	return time.Duration(seconds.Float64 * float64(time.Second)), !pending, nil
}
