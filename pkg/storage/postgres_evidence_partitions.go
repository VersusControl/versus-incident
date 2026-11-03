package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// partitionEvidenceBatch copies a bounded prefix while triggers mirror concurrent
// writes and deletions. The final short lock only swaps names after both prefixes
// are caught up. Retired originals remain available for operator rollback.
func (p *postgresProvider) partitionEvidenceBatch(ctx context.Context) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'`); err != nil {
		return false, err
	}
	var locked bool
	if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(6216456209771226)`).Scan(&locked); err != nil || !locked {
		return false, err
	}
	var high, revision int64
	var scope, occurrence string
	var complete bool
	if err = tx.QueryRowContext(ctx, `SELECT event_sequence,observation_scope,observation_occurrence,observation_revision,complete FROM kubernetes_evidence_partition_migration WHERE singleton FOR UPDATE`).Scan(&high, &scope, &occurrence, &revision, &complete); err != nil {
		return false, err
	}
	if complete {
		return false, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,event_at FROM kubernetes_evidence_events WHERE sequence>$1 ORDER BY sequence LIMIT 256 FOR SHARE`, high)
	if err != nil {
		return false, err
	}
	type eventRow struct {
		sequence int64
		at       time.Time
	}
	events := []eventRow{}
	for rows.Next() {
		var row eventRow
		if err = rows.Scan(&row.sequence, &row.at); err != nil {
			rows.Close()
			return false, err
		}
		events = append(events, row)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, row := range events {
		if _, err = tx.ExecContext(ctx, `SELECT versus_ensure_evidence_partition('kubernetes_evidence_events_partitioned',$1)`, row.at); err != nil {
			return false, err
		}
	}
	if len(events) > 0 {
		next := events[len(events)-1].sequence
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_events_partitioned SELECT * FROM kubernetes_evidence_events WHERE sequence>$1 AND sequence<=$2 ON CONFLICT DO NOTHING`, high, next); err != nil {
			return false, err
		}
		high = next
	}
	rows, err = tx.QueryContext(ctx, `SELECT scope_key,occurrence_id,revision,observed_at FROM kubernetes_evidence_occurrence_observations WHERE (scope_key,occurrence_id,revision)>($1,$2,$3) ORDER BY scope_key,occurrence_id,revision LIMIT 256 FOR SHARE`, scope, occurrence, revision)
	if err != nil {
		return false, err
	}
	type observationRow struct {
		scope, occurrence string
		revision          int64
		at                time.Time
	}
	observations := []observationRow{}
	for rows.Next() {
		var row observationRow
		if err = rows.Scan(&row.scope, &row.occurrence, &row.revision, &row.at); err != nil {
			rows.Close()
			return false, err
		}
		observations = append(observations, row)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, row := range observations {
		if _, err = tx.ExecContext(ctx, `SELECT versus_ensure_evidence_partition('kubernetes_evidence_observations_partitioned',$1)`, row.at); err != nil {
			return false, err
		}
	}
	if len(observations) > 0 {
		next := observations[len(observations)-1]
		if _, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_observations_partitioned SELECT * FROM kubernetes_evidence_occurrence_observations WHERE (scope_key,occurrence_id,revision)>($1,$2,$3) AND (scope_key,occurrence_id,revision)<=($4,$5,$6) ON CONFLICT DO NOTHING`, scope, occurrence, revision, next.scope, next.occurrence, next.revision); err != nil {
			return false, err
		}
		scope, occurrence, revision = next.scope, next.occurrence, next.revision
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_partition_migration SET event_sequence=$1,observation_scope=$2,observation_occurrence=$3,observation_revision=$4 WHERE singleton`, high, scope, occurrence, revision); err != nil {
		return false, err
	}
	if len(events) == 0 && len(observations) == 0 {
		// Avoid a lock upgrade deadlock: this transaction has read no source
		// rows. If active requests prevent the lock, retry another bounded batch.
		if _, err = tx.ExecContext(ctx, `LOCK TABLE kubernetes_evidence_events,kubernetes_evidence_occurrence_observations IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
			return false, err
		}
		var pending bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_events WHERE sequence>$1) OR EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations WHERE (scope_key,occurrence_id,revision)>($2,$3,$4))`, high, scope, occurrence, revision).Scan(&pending); err != nil {
			return false, err
		}
		if !pending {
			// Function bodies resolve shadow names dynamically, so drop the
			// mirroring triggers before renaming their destinations.
			if _, err = tx.ExecContext(ctx, `
DROP TRIGGER evidence_event_partition_mirror ON kubernetes_evidence_events;
DROP TRIGGER evidence_observation_partition_mirror ON kubernetes_evidence_occurrence_observations;
ALTER TABLE kubernetes_evidence_events RENAME TO kubernetes_evidence_events_retired;
ALTER TABLE kubernetes_evidence_events_partitioned RENAME TO kubernetes_evidence_events;
ALTER SEQUENCE kubernetes_evidence_events_sequence_seq OWNED BY kubernetes_evidence_events.sequence;
ALTER TABLE kubernetes_evidence_occurrence_observations RENAME TO kubernetes_evidence_observations_retired;
ALTER TABLE kubernetes_evidence_observations_partitioned RENAME TO kubernetes_evidence_occurrence_observations;
UPDATE kubernetes_evidence_partition_migration SET complete=TRUE WHERE singleton;`); err != nil {
				return false, err
			}
		}

	}
	return true, tx.Commit()
}

func (p *postgresProvider) compactEvidenceBatch(ctx context.Context) (bool, error) {
	// Retention disabled also disables compaction: no automatic data deletion
	// occurs merely because an installation upgraded.
	if p.evidenceRetention <= 0 {
		return false, nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(6216456209771226)`); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'`); err != nil {
		return false, err
	}
	var complete bool
	if err = tx.QueryRowContext(ctx, `SELECT complete FROM kubernetes_evidence_partition_migration WHERE singleton FOR UPDATE`).Scan(&complete); err != nil {
		return false, err
	}
	if !complete {
		return false, tx.Commit()
	}
	// Physical originals are rollback artifacts, not live query models. Only
	// an explicit retention policy permits their automatic disposal.
	for _, name := range []string{"kubernetes_evidence_observations_retired", "kubernetes_evidence_events_retired"} {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			if _, err = tx.ExecContext(ctx, "DROP TABLE "+name); err != nil {
				return false, err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent IN('kubernetes_evidence_events'::regclass,'kubernetes_evidence_occurrence_observations'::regclass) AND c.relkind='r' ORDER BY c.relname`)
	if err != nil {
		return false, err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return false, err
		}
		names = append(names, name)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	cutoff := time.Now().UTC().Add(-p.evidenceRetention)
	var pruned time.Time
	for _, name := range names {
		prefix := ""
		for _, candidate := range []string{"evidence_event_month_", "evidence_observation_month_"} {
			if len(name) == len(candidate)+6 && name[:len(candidate)] == candidate {
				prefix = candidate
				break
			}
		}
		if prefix == "" {
			continue
		}
		month, parseErr := time.Parse("200601", name[len(prefix):])
		if parseErr != nil || month.AddDate(0, 1, 0).After(cutoff) {
			continue
		}
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+name); err != nil {
			return false, err
		}
		pruned = month.AddDate(0, 1, 0)
		break
	}
	if !pruned.IsZero() {
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{pruned_through}',to_jsonb($1::timestamptz)),'{prune_revision}',to_jsonb(COALESCE((checkpoint->>'prune_revision')::bigint,0)+1)) WHERE COALESCE((checkpoint->>'pruned_through')::timestamptz,'epoch')<$1`, pruned); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	// Preserve the opening observation of every payload version and one actual
	// observation per hour. Historical reads report the retained sample itself,
	// never a synthetic interpolated freshness value.
	expired := time.Now().UTC().Add(-p.evidenceRetention)
	if sevenDays := time.Now().UTC().Add(-7 * 24 * time.Hour); expired.After(sevenDays) {
		expired = sevenDays
	}
	result, err := tx.ExecContext(ctx, `WITH candidates AS (
		 SELECT o.tableoid AS relation,o.ctid AS tuple FROM kubernetes_evidence_occurrence_observations o
		 WHERE o.observed_at < $1
		 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations n WHERE n.scope_key=o.scope_key AND n.occurrence_id=o.occurrence_id AND n.payload_version=o.payload_version AND n.observed_at>=date_trunc('hour',o.observed_at) AND n.observed_at<date_trunc('hour',o.observed_at)+INTERVAL '1 hour' AND n.revision>o.revision)
		 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations first_sample WHERE first_sample.scope_key=o.scope_key AND first_sample.occurrence_id=o.occurrence_id AND first_sample.payload_version=o.payload_version AND first_sample.revision<o.revision)
		 LIMIT 256 FOR UPDATE OF o SKIP LOCKED)
		 DELETE FROM kubernetes_evidence_occurrence_observations o USING candidates c WHERE o.tableoid=c.relation AND o.ctid=c.tuple`, expired)
	if err != nil {
		return false, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if removed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{prune_revision}',to_jsonb(COALESCE((checkpoint->>'prune_revision')::bigint,0)+1)),'{pruned_through}',to_jsonb(GREATEST(COALESCE((checkpoint->>'pruned_through')::timestamptz,'epoch'),$1::timestamptz)))`, expired); err != nil {
			return false, err
		}
	}
	return removed > 0, tx.Commit()
}

func prepareEvidencePartitions(ctx context.Context, tx *sql.Tx, batch EvidenceBatch) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(6216456209771226)`); err != nil {
		return err
	}
	eventTimes, observationTimes := map[string]time.Time{}, map[string]time.Time{}
	for _, record := range batch.Records {
		eventTimes[record.At.UTC().Format("2006-01")] = record.At
	}
	eventTimes[batch.At.UTC().Format("2006-01")] = batch.At
	observationTimes[batch.At.UTC().Format("2006-01")] = batch.At
	for _, issue := range batch.Issues {
		if !issue.LastSeen.IsZero() {
			eventTimes[issue.LastSeen.UTC().Format("2006-01")] = issue.LastSeen
			observationTimes[issue.LastSeen.UTC().Format("2006-01")] = issue.LastSeen
		}
	}
	if !batch.CoveredThrough.IsZero() {
		eventTimes[batch.CoveredThrough.UTC().Format("2006-01")] = batch.CoveredThrough
		observationTimes[batch.CoveredThrough.UTC().Format("2006-01")] = batch.CoveredThrough
	}
	for _, entry := range []struct {
		parent string
		times  map[string]time.Time
	}{{"kubernetes_evidence_events", eventTimes}, {"kubernetes_evidence_occurrence_observations", observationTimes}} {
		for _, at := range entry.times {
			if _, err := tx.ExecContext(ctx, `SELECT versus_ensure_evidence_partition(to_regclass($1::text),$2)`, entry.parent, at); err != nil {
				return fmt.Errorf("storage: prepare evidence partition: %w", err)
			}
		}
	}
	return nil
}
