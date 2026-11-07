package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'; `+evidenceHeadsWriter); err != nil {
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
		// The read-model conversion of a scope from before read models looks
		// each event's next version up across the whole journal. On the
		// unpartitioned journal that is one index probe; across years of
		// monthly partitions it is one per month and may not fit a batch.
		// The copy therefore waits for every scope to finish converting.
		var converting bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubernetes_evidence_scopes WHERE NOT read_model_ready)`).Scan(&converting); err != nil {
			return false, err
		}
		if converting {
			p.maintenanceLogMu.Lock()
			due := time.Since(p.swapWaitLoggedAt) >= 10*time.Minute
			if due {
				p.swapWaitLoggedAt = time.Now()
			}
			p.maintenanceLogMu.Unlock()
			if due {
				log.Printf("storage: evidence partition switch waits for read-model conversion to finish")
			}
			return false, tx.Commit()
		}
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
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='100ms'; `+evidenceHeadsWriter); err != nil {
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
	var prunedScopes []string
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
		// Only scopes with rows in this month lose history, so only their
		// cursors are invalidated. The set is read inside the same transaction,
		// before the drop, so a scope that had rows is never missed.
		if prunedScopes, err = evidenceScopesIn(ctx, tx, name); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+name); err != nil {
			return false, err
		}
		if prefix == "evidence_event_month_" {
			// Heads must not point at rows retention has removed.
			if _, err = tx.ExecContext(ctx, `DELETE FROM kubernetes_evidence_event_heads WHERE event_at<$1`, month.AddDate(0, 1, 0)); err != nil {
				return false, err
			}
		}
		pruned = month.AddDate(0, 1, 0)
		break
	}
	if !pruned.IsZero() {
		// Every scope that lost rows is invalidated, even one already pruned
		// through this month: rows can arrive late in a month retention dropped.
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{pruned_through}',to_jsonb(GREATEST(COALESCE((checkpoint->>'pruned_through')::timestamptz,'epoch'),$1::timestamptz))),'{prune_revision}',to_jsonb(COALESCE((checkpoint->>'prune_revision')::bigint,0)+1)) WHERE scope_key=ANY($2::text[])`, pruned, prunedScopes); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	// Preserve the opening observation of every payload version and one actual
	// observation per hour. Historical reads report the retained sample itself,
	// never a synthetic interpolated freshness value. The opening observation's
	// revision is read from the version's read model, so a candidate never
	// searches every monthly partition for an older sample. The later sample
	// of the same hour is looked up once per candidate (OFFSET 0 keeps it from
	// becoming a join), so only the candidate hour's month is searched rather
	// than every sample the occurrence has in every month. Every statement
	// names the expiry bound itself, so it is planned for the old months only.
	expired := time.Now().UTC().Add(-p.evidenceRetention)
	if sevenDays := time.Now().UTC().Add(-7 * 24 * time.Hour); expired.After(sevenDays) {
		expired = sevenDays
	}
	result, err := tx.QueryContext(ctx, `WITH candidates AS (
		 SELECT o.tableoid AS relation,o.ctid AS tuple FROM kubernetes_evidence_occurrence_observations o
		 WHERE o.observed_at < $1
		 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_observations n WHERE n.scope_key=o.scope_key AND n.occurrence_id=o.occurrence_id AND n.payload_version=o.payload_version AND n.observed_at>=date_trunc('hour',o.observed_at) AND n.observed_at<date_trunc('hour',o.observed_at)+INTERVAL '1 hour' AND n.observed_at<$2 AND n.revision>o.revision OFFSET 0)
		 AND EXISTS(SELECT 1 FROM kubernetes_evidence_occurrence_read_versions opening WHERE opening.scope_key=o.scope_key AND opening.payload_version=o.payload_version AND opening.occurrence_id=o.occurrence_id AND opening.revision<o.revision)
		 LIMIT 256 FOR UPDATE OF o SKIP LOCKED)
		 DELETE FROM kubernetes_evidence_occurrence_observations o USING candidates c WHERE o.tableoid=c.relation AND o.ctid=c.tuple AND o.observed_at<$1
		 RETURNING o.scope_key`, expired, expired.Add(time.Hour))
	if err != nil {
		return false, err
	}
	// Thinning removes samples only. It is counted apart from pruning, so lists
	// that never read samples keep their page links, and an issue list ends
	// its links only when its read reaches the thinned samples.
	compacted := map[string]bool{}
	for result.Next() {
		var scope string
		if err = result.Scan(&scope); err != nil {
			result.Close()
			return false, err
		}
		compacted[scope] = true
	}
	if err = result.Err(); err != nil {
		result.Close()
		return false, err
	}
	result.Close()
	if len(compacted) > 0 {
		scopes := make([]string, 0, len(compacted))
		for scope := range compacted {
			scopes = append(scopes, scope)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=jsonb_set(jsonb_set(checkpoint,'{thin_revision}',to_jsonb(COALESCE((checkpoint->>'thin_revision')::bigint,0)+1)),'{thinned_through}',to_jsonb(GREATEST(COALESCE((checkpoint->>'thinned_through')::timestamptz,'epoch'),$1::timestamptz))) WHERE scope_key=ANY($2::text[])`, expired, scopes); err != nil {
			return false, err
		}
	}
	return len(compacted) > 0, tx.Commit()
}

// evidenceScopesIn lists the scopes holding rows in one month partition. A
// deployment has few scopes, so each one is probed for a single row through a
// partition index that leads with scope_key, instead of reading the month.
// Sequential scans are disabled for the probe alone: on a month without
// planner statistics a scan could run past the statement timeout and stall
// retention.
func evidenceScopesIn(ctx context.Context, tx *sql.Tx, partition string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, evidenceScopesProbeSettings); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, evidenceScopesProbe(partition))
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			rows.Close()
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan TO DEFAULT`); err != nil {
		return nil, err
	}
	return scopes, nil
}

const evidenceScopesProbeSettings = `SET LOCAL enable_seqscan=off`

func evidenceScopesProbe(partition string) string {
	return "SELECT s.scope_key FROM kubernetes_evidence_scopes s WHERE EXISTS(SELECT 1 FROM " + partition + " p WHERE p.scope_key=s.scope_key)"
}

// prepareEvidencePartitions ensures the month partitions a save writes into.
// A journal month that ends at or before the retention cutoff is never
// created: retention has dropped or will drop it, and its records are not
// written.
func prepareEvidencePartitions(ctx context.Context, tx *sql.Tx, batch EvidenceBatch, cutoff time.Time) error {
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
			month := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
			if entry.parent == "kubernetes_evidence_events" && !cutoff.IsZero() && !month.AddDate(0, 1, 0).After(cutoff) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `SELECT versus_ensure_evidence_partition(to_regclass($1::text),$2)`, entry.parent, at); err != nil {
				return fmt.Errorf("storage: prepare evidence partition: %w", err)
			}
		}
	}
	return nil
}
