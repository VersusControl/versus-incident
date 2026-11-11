package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// errSkipWrite rolls back a transaction that decided to write nothing.
var errSkipWrite = errors.New("storage: nothing to write")

var awsTemporalTables = []string{"aws_resources", "aws_health", "aws_security_flags", "aws_unit_generations"}

var awsResourceColumns = []string{"org_id", "account", "region", "service", "arn", "resource_type", "app", "name", "state", "tags", "relations", "attributes", "detail", "digest", "valid_from"}
var awsHealthColumns = []string{"org_id", "account", "region", "service", "arn", "kind", "ref", "state", "summary", "since", "children", "detail", "digest", "valid_from"}
var awsFlagColumns = []string{"org_id", "account", "region", "service", "arn", "flag", "summary", "detail", "digest", "valid_from"}
var awsChangeColumns = []string{"org_id", "event_id", "account", "region", "service", "event_time", "event_name", "actor_class", "actor", "resource_arn", "app", "summary", "fields", "detail"}

func encodeTableJSON(value any) any {
	switch typed := value.(type) {
	case map[string]string:
		if len(typed) == 0 {
			return nil
		}
	case []string:
		if len(typed) == 0 {
			return nil
		}
	case []AWSRelation:
		if len(typed) == 0 {
			return nil
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return string(data)
}

func decodeTableMap(data []byte) (map[string]string, error) {
	if emptyJSON(data) {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func decodeTableList[T any](data []byte) ([]T, error) {
	if emptyJSON(data) {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func scannedJSON(data []byte) json.RawMessage {
	if emptyJSON(data) {
		return nil
	}
	return append(json.RawMessage(nil), data...)
}

// lockAccount takes the account epoch row: shared for a guarded write,
// exclusive to serialize coverage merges. A purge holds it exclusively, so a
// guarded write either finishes before the purge deletes or sees the new
// epoch and is refused.
func (t *sqlTables) lockAccount(ctx context.Context, tx *sql.Tx, org, account string, guard *uint64, exclusive bool) error {
	if guard == nil && !exclusive {
		return nil
	}
	if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_account_epochs (org_id, account, epoch) VALUES (?, ?, 0) ON CONFLICT (org_id, account) DO NOTHING`), org, account); err != nil {
		return err
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, t.q(`SELECT epoch FROM aws_account_epochs WHERE org_id = ? AND account = ?`+t.lockClause(exclusive)), org, account).Scan(&epoch); err != nil {
		return err
	}
	if guard != nil && uint64(epoch) != *guard {
		return ErrAWSInventoryFenced
	}
	return nil
}

func (t *sqlTables) AWSAccountEpoch(ctx context.Context, scope tenancy.OrgScope, account string) (uint64, error) {
	if !validTableKey(account) {
		return 0, ErrInvalidAWSInventory
	}
	db, err := t.tablesDB()
	if err != nil {
		return 0, err
	}
	scope = inventoryScope(scope)
	var epoch int64
	err = db.QueryRowContext(ctx, t.q(`SELECT epoch FROM aws_account_epochs WHERE org_id = ? AND account = ?`), scope.Write, account).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return uint64(epoch), err
}

func unitArgs(org string, unit AWSUnit) []any {
	return []any{org, unit.Account, unit.Region, unit.Service}
}

const unitMatch = "org_id = ? AND account = ? AND region = ? AND service = ?"

func (t *sqlTables) CommitAWSUnit(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, candidate AWSCandidate, guard *uint64) (AWSCommit, error) {
	rows, err := prepareAWSCommit(unit, candidate)
	if err != nil {
		return AWSCommit{}, err
	}
	scope = inventoryScope(scope)
	org := scope.Write
	at := t.at(rows.at)
	var result AWSCommit
	err = t.write(ctx, func(tx *sql.Tx) error {
		if err := t.lockAccount(ctx, tx, org, unit.Account, guard, false); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_sync_units (org_id, account, region, service) VALUES (?, ?, ?, ?) ON CONFLICT (org_id, account, region, service) DO NOTHING`), unitArgs(org, unit)...); err != nil {
			return err
		}
		var syncGeneration int64
		if err := tx.QueryRowContext(ctx, t.q(`SELECT generation FROM aws_sync_units WHERE `+unitMatch+t.lockClause(true)), unitArgs(org, unit)...).Scan(&syncGeneration); err != nil {
			return err
		}
		var current AWSGeneration
		var committed, verified tableTimeValue
		var count int64
		var gaps []byte
		found := true
		err := tx.QueryRowContext(ctx, t.q(`SELECT seq, digest, valid_from, verified_at, resource_count, gaps FROM aws_unit_generations WHERE `+unitMatch+` AND valid_to IS NULL`), unitArgs(org, unit)...).
			Scan(&current.Seq, &current.Digest, &committed, &verified, &count, &gaps)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
		} else if err != nil {
			return err
		}
		// The candidate's time was read before this transaction took the row
		// lock, so a slower clock or a concurrent commit can place it before
		// the current generation began. Clamp it here, under the lock, so a
		// generation never reads as starting before the one it replaces.
		atTime := rows.at
		if found && committed.Time.After(atTime) {
			atTime = committed.Time
		}
		at = t.at(atTime)
		syncArgs := func(seq uint64) []any {
			return []any{AWSSyncOK, at, at, at, jsonParam(rows.gaps), int64(seq), org, unit.Account, unit.Region, unit.Service}
		}
		const syncUpdate = `UPDATE aws_sync_units SET state = ?, last_good_at = ?, attempted_at = ?, verified_at = ?, error_code = '', gaps = ?, generation = ? WHERE ` + unitMatch
		if found && current.Digest == candidate.Digest {
			if _, err := tx.ExecContext(ctx, t.q(`UPDATE aws_unit_generations SET verified_at = ? WHERE `+unitMatch+` AND seq = ?`), append([]any{at}, append(unitArgs(org, unit), int64(current.Seq))...)...); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, t.q(syncUpdate), syncArgs(current.Seq)...); err != nil {
				return err
			}
			current.CommittedAt, current.VerifiedAt, current.Count, current.Gaps = committed.Time, atTime, int(count), scannedJSON(gaps)
			result = AWSCommit{Generation: current, Unchanged: true}
			return nil
		}
		seq := uint64(syncGeneration)
		if found && current.Seq > seq {
			seq = current.Seq
		}
		seq++
		if found {
			if _, err := tx.ExecContext(ctx, t.q(`UPDATE aws_unit_generations SET valid_to = ? WHERE `+unitMatch+` AND valid_to IS NULL`), append([]any{at}, unitArgs(org, unit)...)...); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_unit_generations (org_id, account, region, service, seq, digest, verified_at, resource_count, gaps, valid_from) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			org, unit.Account, unit.Region, unit.Service, int64(seq), candidate.Digest, at, int64(len(rows.resources)), jsonParam(rows.gaps), at); err != nil {
			return err
		}
		resourceRows := make([][]any, len(rows.resources))
		for index, row := range rows.resources {
			resourceRows[index] = []any{org, unit.Account, unit.Region, unit.Service, row.ARN, row.Type, row.App, row.Name, row.State,
				encodeTableJSON(row.Tags), encodeTableJSON(row.Relations), encodeTableJSON(row.Attributes), jsonParam(row.Detail), rows.digests[0][index], at}
		}
		if err := t.swapRows(ctx, tx, "aws_resources", awsResourceColumns, org, unit, at, rows.digests[0], resourceRows); err != nil {
			return err
		}
		healthRows := make([][]any, len(rows.health))
		for index, row := range rows.health {
			healthRows[index] = []any{org, unit.Account, unit.Region, unit.Service, row.ARN, row.Kind, row.Ref, row.State, row.Summary,
				t.at(row.Since), encodeTableJSON(row.Children), jsonParam(row.Detail), rows.digests[1][index], at}
		}
		if err := t.swapRows(ctx, tx, "aws_health", awsHealthColumns, org, unit, at, rows.digests[1], healthRows); err != nil {
			return err
		}
		flagRows := make([][]any, len(rows.flags))
		for index, row := range rows.flags {
			flagRows[index] = []any{org, unit.Account, unit.Region, unit.Service, row.ARN, row.Flag, row.Summary, jsonParam(row.Detail), rows.digests[2][index], at}
		}
		if err := t.swapRows(ctx, tx, "aws_security_flags", awsFlagColumns, org, unit, at, rows.digests[2], flagRows); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.q(syncUpdate), syncArgs(seq)...); err != nil {
			return err
		}
		result = AWSCommit{Generation: AWSGeneration{Seq: seq, Digest: candidate.Digest, CommittedAt: atTime, VerifiedAt: atTime, Count: len(rows.resources), Gaps: cloneTableJSON(rows.gaps)}}
		return nil
	})
	if err != nil {
		return AWSCommit{}, err
	}
	return result, nil
}

// swapRows makes rows the unit's current rows in table: current rows whose
// digest is not wanted are closed at at, and wanted rows not already current
// are inserted. Unchanged rows keep their history.
func (t *sqlTables) swapRows(ctx context.Context, tx *sql.Tx, table string, columns []string, org string, unit AWSUnit, at any, digests []string, rows [][]any) error {
	existing := map[string]bool{}
	result, err := tx.QueryContext(ctx, t.q(`SELECT digest FROM `+table+` WHERE `+unitMatch+` AND valid_to IS NULL`), unitArgs(org, unit)...)
	if err != nil {
		return err
	}
	for result.Next() {
		var digest string
		if err := result.Scan(&digest); err != nil {
			result.Close()
			return err
		}
		existing[digest] = true
	}
	if err := result.Close(); err != nil {
		return err
	}
	if err := result.Err(); err != nil {
		return err
	}
	wanted := make(map[string]bool, len(digests))
	inserts := make([][]any, 0, len(rows))
	for index, digest := range digests {
		wanted[digest] = true
		if !existing[digest] {
			inserts = append(inserts, rows[index])
		}
	}
	closing := make([]string, 0)
	for digest := range existing {
		if !wanted[digest] {
			closing = append(closing, digest)
		}
	}
	sort.Strings(closing)
	err = inChunks(closing, func(chunk []string) error {
		args := append([]any{at}, unitArgs(org, unit)...)
		args = append(args, stringArgs(chunk)...)
		_, err := tx.ExecContext(ctx, t.q(`UPDATE `+table+` SET valid_to = ? WHERE `+unitMatch+` AND valid_to IS NULL AND digest IN (`+placeholders(len(chunk))+`)`), args...)
		return err
	})
	if err != nil {
		return err
	}
	return t.insertRows(ctx, tx, table, columns, inserts, "")
}

func (t *sqlTables) UpdateAWSSync(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, guard *uint64, update func(state *AWSSyncState, found bool) bool) error {
	if !validAWSUnit(unit) || update == nil {
		return ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	org := scope.Write
	err := t.write(ctx, func(tx *sql.Tx) error {
		if err := t.lockAccount(ctx, tx, org, unit.Account, guard, false); err != nil {
			return err
		}
		inserted, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_sync_units (org_id, account, region, service) VALUES (?, ?, ?, ?) ON CONFLICT (org_id, account, region, service) DO NOTHING`), unitArgs(org, unit)...)
		if err != nil {
			return err
		}
		created, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		var state AWSSyncState
		var generation int64
		var lastGood, attempted, verified tableTimeValue
		var gaps []byte
		if err := tx.QueryRowContext(ctx, t.q(`SELECT generation, state, last_good_at, attempted_at, verified_at, error_code, gaps FROM aws_sync_units WHERE `+unitMatch+t.lockClause(true)), unitArgs(org, unit)...).
			Scan(&generation, &state.State, &lastGood, &attempted, &verified, &state.ErrorCode, &gaps); err != nil {
			return err
		}
		state.Generation = uint64(generation)
		state.LastGoodAt, state.AttemptedAt, state.VerifiedAt, state.Gaps = lastGood.Time, attempted.Time, verified.Time, scannedJSON(gaps)
		if !update(&state, created == 0) {
			return errSkipWrite
		}
		if !validTableStrings(state.State, state.ErrorCode) || !validTableJSON(state.Gaps) {
			return ErrInvalidAWSInventory
		}
		_, err = tx.ExecContext(ctx, t.q(`UPDATE aws_sync_units SET state = ?, last_good_at = ?, attempted_at = ?, verified_at = ?, error_code = ?, gaps = ? WHERE `+unitMatch),
			state.State, t.at(state.LastGoodAt), t.at(state.AttemptedAt), t.at(state.VerifiedAt), state.ErrorCode, jsonParam(state.Gaps), org, unit.Account, unit.Region, unit.Service)
		return err
	})
	if errors.Is(err, errSkipWrite) {
		return nil
	}
	return err
}

func (t *sqlTables) AppendAWSChanges(ctx context.Context, scope tenancy.OrgScope, account, region string, changes []AWSChange, covered []AWSInterval, guard *uint64) error {
	if !validTableKey(account) || !validTableKey(region) {
		return ErrInvalidAWSInventory
	}
	byID := make(map[string]AWSChange, len(changes))
	for _, change := range changes {
		cleaned, err := cleanAWSChange(account, region, change)
		if err != nil {
			return err
		}
		byID[cleaned.EventID] = cleaned
	}
	scope = inventoryScope(scope)
	org := scope.Write
	return t.write(ctx, func(tx *sql.Tx) error {
		if err := t.lockAccount(ctx, tx, org, account, guard, len(covered) > 0); err != nil {
			return err
		}
		rows := make([][]any, 0, len(byID))
		for _, id := range sortedKeysOf(byID) {
			change := byID[id]
			rows = append(rows, []any{org, change.EventID, change.Account, change.Region, change.Service, t.at(change.EventTime), change.EventName, change.ActorClass,
				change.Actor, change.ResourceARN, change.App, change.Summary, encodeTableJSON(change.Fields), jsonParam(change.Detail)})
		}
		const upsert = ` ON CONFLICT (org_id, event_id) DO UPDATE SET account = excluded.account, region = excluded.region, service = excluded.service,
 event_time = excluded.event_time, event_name = excluded.event_name, actor_class = excluded.actor_class, actor = excluded.actor,
 resource_arn = excluded.resource_arn, app = excluded.app, summary = excluded.summary, fields = excluded.fields, detail = excluded.detail`
		if err := t.insertRows(ctx, tx, "aws_changes", awsChangeColumns, rows, upsert); err != nil {
			return err
		}
		if len(covered) == 0 {
			return nil
		}
		existing, err := t.pairCoverage(ctx, tx, org, account, region)
		if err != nil {
			return err
		}
		merged := mergeAWSIntervals(append(existing, covered...))
		if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ?`), org, account, region); err != nil {
			return err
		}
		coverage := make([][]any, len(merged))
		for index, interval := range merged {
			coverage[index] = []any{org, account, region, t.at(interval.From), t.at(interval.To)}
		}
		return t.insertRows(ctx, tx, "aws_change_coverage", []string{"org_id", "account", "region", "covered_from", "covered_until"}, coverage, "")
	})
}

func sortedKeysOf[T any](values map[string]T) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (t *sqlTables) pairCoverage(ctx context.Context, tx *sql.Tx, org, account, region string) ([]AWSInterval, error) {
	rows, err := tx.QueryContext(ctx, t.q(`SELECT covered_from, covered_until FROM aws_change_coverage WHERE org_id = ? AND account = ? AND region = ? ORDER BY covered_from`), org, account, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AWSInterval
	for rows.Next() {
		var from, to tableTimeValue
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		out = append(out, AWSInterval{From: from.Time, To: to.Time})
	}
	return out, rows.Err()
}

var awsAccountTables = []string{"aws_resources", "aws_health", "aws_security_flags", "aws_unit_generations", "aws_sync_units", "aws_changes", "aws_change_coverage"}

func (t *sqlTables) PurgeAWSAccount(ctx context.Context, scope tenancy.OrgScope, account, region string) error {
	if !validTableKey(account) || region != "" && !validTableKey(region) {
		return ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	org := scope.Write
	return t.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_account_epochs (org_id, account, epoch) VALUES (?, ?, 1)
 ON CONFLICT (org_id, account) DO UPDATE SET epoch = aws_account_epochs.epoch + 1`), org, account); err != nil {
			return err
		}
		for _, table := range awsAccountTables {
			statement := `DELETE FROM ` + table + ` WHERE org_id = ? AND account = ?`
			args := []any{org, account}
			if region != "" {
				statement += ` AND region = ?`
				args = append(args, region)
			}
			if _, err := tx.ExecContext(ctx, t.q(statement), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *sqlTables) PruneAWSHistory(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	var total int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		for _, table := range awsTemporalTables {
			result, err := tx.ExecContext(ctx, t.q(`DELETE FROM `+table+` WHERE org_id = ? AND valid_to IS NOT NULL AND valid_to < ?`), scope.Write, t.at(before))
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			total += count
		}
		return nil
	})
	return total, err
}

func (t *sqlTables) PruneAWSUnitHistory(ctx context.Context, scope tenancy.OrgScope, unit AWSUnit, keep int) (int64, error) {
	if !validAWSUnit(unit) || keep < 1 {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	org := scope.Write
	var total int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		var seq int64
		var oldest tableTimeValue
		err := tx.QueryRowContext(ctx, t.q(`SELECT seq, valid_from FROM aws_unit_generations WHERE `+unitMatch+` ORDER BY seq DESC LIMIT 1 OFFSET ?`),
			append(unitArgs(org, unit), keep-1)...).Scan(&seq, &oldest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_unit_generations WHERE `+unitMatch+` AND seq < ?`), append(unitArgs(org, unit), seq)...)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		total += count
		for _, table := range []string{"aws_resources", "aws_health", "aws_security_flags"} {
			result, err := tx.ExecContext(ctx, t.q(`DELETE FROM `+table+` WHERE `+unitMatch+` AND valid_to IS NOT NULL AND valid_to <= ?`), append(unitArgs(org, unit), t.at(oldest.Time))...)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			total += count
		}
		return nil
	})
	return total, err
}

func (t *sqlTables) PruneAWSChanges(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	org, cutoff := scope.Write, t.at(before)
	var total int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_changes WHERE org_id = ? AND event_time < ?`), org, cutoff)
		if err != nil {
			return err
		}
		if total, err = result.RowsAffected(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_change_coverage WHERE org_id = ? AND covered_until <= ?`), org, cutoff); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, t.q(`UPDATE aws_change_coverage SET covered_from = ? WHERE org_id = ? AND covered_from < ?`), cutoff, org, cutoff)
		return err
	})
	return total, err
}

// SweepAWSRetention deletes expired inventory rows in bounded batches. Each
// batch is its own statement, so a sweep never holds a table lock for the
// whole backlog; Cursor resumes the walk. Current rows are never deleted:
// every history predicate requires valid_to IS NOT NULL.
func (t *sqlTables) SweepAWSRetention(ctx context.Context, sweep AWSRetentionSweep) (AWSRetentionResult, error) {
	if sweep.KeepGenerations < 1 {
		return AWSRetentionResult{}, ErrInvalidAWSInventory
	}
	if _, err := t.tablesDB(); err != nil {
		return AWSRetentionResult{}, err
	}
	scope := inventoryScope(sweep.Scope)
	org := scope.Write
	batch := sweep.Batch
	if batch <= 0 {
		batch = DefaultAWSRetentionBatch
	}
	expired := func() bool {
		return !sweep.Deadline.IsZero() && !time.Now().Before(sweep.Deadline)
	}
	result := AWSRetentionResult{Cursor: sweep.Cursor}
	stage := sweep.Cursor.Stage
	if stage == "" {
		stage = "units"
	}

	if stage == "units" {
		for {
			if expired() {
				result.More, result.Cursor.Stage = true, "units"
				return result, nil
			}
			units, err := t.sweepUnitPage(ctx, org, result.Cursor.After, batch)
			if err != nil {
				return result, err
			}
			if len(units) == 0 {
				break
			}
			for _, unit := range units {
				if expired() {
					result.More, result.Cursor.Stage = true, "units"
					return result, nil
				}
				generations, history, complete, err := t.sweepUnit(ctx, org, unit, sweep.KeepGenerations, batch, expired)
				if err != nil {
					return result, err
				}
				result.Generations += generations
				result.History += history
				if !complete {
					result.More, result.Cursor.Stage = true, "units"
					return result, nil
				}
				result.Units++
				result.Cursor.After = unit
			}
			if len(units) < batch {
				break
			}
		}
		stage = "history"
	}

	if stage == "history" {
		if !sweep.HistoryBefore.IsZero() {
			cutoff := t.at(sweep.HistoryBefore)
			for _, table := range awsTemporalTables {
				for {
					removed, err := t.sweepDelete(ctx, table, `org_id = ? AND valid_to IS NOT NULL AND valid_to < ?`, []any{org, cutoff}, batch)
					if err != nil {
						return result, err
					}
					result.History += removed
					if removed < int64(batch) {
						break
					}
					if expired() {
						result.More, result.Cursor.Stage = true, "history"
						return result, nil
					}
				}
			}
		}
		stage = "changes"
	}

	if stage == "changes" {
		if !sweep.ChangeBefore.IsZero() {
			cutoff := t.at(sweep.ChangeBefore)
			for {
				removed, err := t.sweepDelete(ctx, "aws_changes", `org_id = ? AND event_time < ?`, []any{org, cutoff}, batch)
				if err != nil {
					return result, err
				}
				result.Changes += removed
				if removed < int64(batch) {
					break
				}
				if expired() {
					result.More, result.Cursor.Stage = true, "changes"
					return result, nil
				}
			}
			if err := t.sweepChangeCoverage(ctx, org, cutoff); err != nil {
				return result, err
			}
		}
	}

	result.Cursor = AWSRetentionCursor{}
	return result, nil
}

// sweepUnitPage returns the next units after the cursor, ordered so the page
// is stable across passes.
func (t *sqlTables) sweepUnitPage(ctx context.Context, org string, after AWSUnit, limit int) ([]AWSUnit, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, t.q(`SELECT account, region, service FROM aws_sync_units
 WHERE org_id = ? AND (account, region, service) > (?, ?, ?)
 ORDER BY account, region, service LIMIT ?`), org, after.Account, after.Region, after.Service, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var units []AWSUnit
	for rows.Next() {
		var unit AWSUnit
		if err := rows.Scan(&unit.Account, &unit.Region, &unit.Service); err != nil {
			return nil, err
		}
		units = append(units, unit)
	}
	return units, rows.Err()
}

// sweepUnit keeps a unit's newest generations and deletes the rows that
// stopped being current before the oldest kept generation. Both deletes run
// in bounded batches; complete is false when the time budget stopped it
// part-way, so the caller repeats the unit next pass.
func (t *sqlTables) sweepUnit(ctx context.Context, org string, unit AWSUnit, keep, batch int, expired func() bool) (generations, history int64, complete bool, err error) {
	db, err := t.tablesDB()
	if err != nil {
		return 0, 0, false, err
	}
	var seq int64
	var oldest tableTimeValue
	err = db.QueryRowContext(ctx, t.q(`SELECT seq, valid_from FROM aws_unit_generations WHERE `+unitMatch+` ORDER BY seq DESC LIMIT 1 OFFSET ?`),
		append(unitArgs(org, unit), keep-1)...).Scan(&seq, &oldest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, true, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	for {
		removed, err := t.sweepDelete(ctx, "aws_unit_generations", unitMatch+` AND seq < ?`, append(unitArgs(org, unit), seq), batch)
		generations += removed
		if err != nil {
			return generations, history, false, err
		}
		if removed < int64(batch) {
			break
		}
		if expired() {
			return generations, history, false, nil
		}
	}
	cutoff := t.at(oldest.Time)
	for _, table := range []string{"aws_resources", "aws_health", "aws_security_flags"} {
		for {
			removed, err := t.sweepDelete(ctx, table, unitMatch+` AND valid_to IS NOT NULL AND valid_to <= ?`, append(unitArgs(org, unit), cutoff), batch)
			history += removed
			if err != nil {
				return generations, history, false, err
			}
			if removed < int64(batch) {
				break
			}
			if expired() {
				return generations, history, false, nil
			}
		}
	}
	return generations, history, true, nil
}

// sweepDelete removes at most batch rows matching condition. It addresses
// rows by their physical locator (ctid on Postgres, rowid in the SQLite
// index) because neither dialect accepts DELETE ... LIMIT.
func (t *sqlTables) sweepDelete(ctx context.Context, table, condition string, args []any, batch int) (int64, error) {
	if batch <= 0 {
		batch = DefaultAWSRetentionBatch
	}
	locator := "rowid"
	if t.postgres {
		locator = "ctid"
	}
	statement := t.q(`DELETE FROM ` + table + ` WHERE ` + locator + ` IN (SELECT ` + locator + ` FROM ` + table + ` WHERE ` + condition + ` LIMIT ` + strconv.Itoa(batch) + `)`)
	var removed int64
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		removed, err = result.RowsAffected()
		return err
	})
	return removed, err
}

// sweepChangeCoverage trims coverage so it never claims a deleted stretch.
func (t *sqlTables) sweepChangeCoverage(ctx context.Context, org string, cutoff any) error {
	return t.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_change_coverage WHERE org_id = ? AND covered_until <= ?`), org, cutoff); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, t.q(`UPDATE aws_change_coverage SET covered_from = ? WHERE org_id = ? AND covered_from < ?`), cutoff, org, cutoff)
		return err
	})
}

// tablesQuerier is the part of a *sql.DB or *sql.Tx a table read needs, so
// one snapshot's reads can share a transaction.
type tablesQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (t *sqlTables) AWSUnits(ctx context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitStatus, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsUnits(ctx, db, scope, query)
}

func (t *sqlTables) awsUnits(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitStatus, error) {
	scope = inventoryScope(scope)
	temporal, args := t.temporal("g", query.AsOf)
	var where sqlWhere
	condition, visibleArgs := visible(scope.Read, "u", "aws_sync_units", "account", "region", "service")
	where.add(condition, visibleArgs...)
	where.equal("u.account", query.Account)
	where.equal("u.region", query.Region)
	where.equal("u.service", query.Service)
	args = append(args, where.args...)
	rows, err := q.QueryContext(ctx, t.q(`SELECT u.org_id, u.account, u.region, u.service, u.generation, u.state, u.last_good_at, u.attempted_at, u.verified_at, u.error_code, u.gaps,
 g.seq, g.digest, g.valid_from, g.verified_at, g.resource_count, g.gaps
 FROM aws_sync_units u LEFT JOIN aws_unit_generations g
 ON g.org_id = u.org_id AND g.account = u.account AND g.region = u.region AND g.service = u.service AND `+temporal+`
 WHERE `+where.String()+` ORDER BY u.account, u.region, u.service`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSUnitStatus, 0)
	for rows.Next() {
		var status AWSUnitStatus
		var generation int64
		var lastGood, attempted, verified, committed, genVerified tableTimeValue
		var gaps, genGaps []byte
		var seq, count sql.NullInt64
		var digest sql.NullString
		if err := rows.Scan(&status.Org, &status.Unit.Account, &status.Unit.Region, &status.Unit.Service, &generation, &status.Sync.State, &lastGood, &attempted, &verified,
			&status.Sync.ErrorCode, &gaps, &seq, &digest, &committed, &genVerified, &count, &genGaps); err != nil {
			return nil, err
		}
		status.Sync.Generation = uint64(generation)
		status.Sync.LastGoodAt, status.Sync.AttemptedAt, status.Sync.VerifiedAt, status.Sync.Gaps = lastGood.Time, attempted.Time, verified.Time, scannedJSON(gaps)
		if seq.Valid {
			status.Generation = &AWSGeneration{Seq: uint64(seq.Int64), Digest: digest.String, CommittedAt: committed.Time, VerifiedAt: genVerified.Time, Count: int(count.Int64), Gaps: scannedJSON(genGaps)}
		}
		out = append(out, status)
	}
	return out, rows.Err()
}

// AWSAccountView reads one account's units and rows in one transaction, so a
// concurrent commit or purge can never pair one generation's status with
// another generation's resources, health or flags.
func (t *sqlTables) AWSAccountView(ctx context.Context, scope tenancy.OrgScope, account string, asOf time.Time) (AWSSnapshotView, error) {
	db, err := t.tablesDB()
	if err != nil {
		return AWSSnapshotView{}, err
	}
	options := &sql.TxOptions{}
	if t.postgres {
		// Repeatable read keeps every statement on one snapshot even if a
		// commit lands between them; the transaction writes nothing.
		options.Isolation = sql.LevelRepeatableRead
		options.ReadOnly = true
	}
	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return AWSSnapshotView{}, err
	}
	defer func() { _ = tx.Rollback() }()
	view := AWSSnapshotView{}
	if view.Units, err = t.awsUnits(ctx, tx, scope, AWSUnitQuery{Account: account, AsOf: asOf}); err != nil {
		return AWSSnapshotView{}, err
	}
	if view.Resources, err = t.awsResources(ctx, tx, scope, AWSResourceQuery{Account: account, AsOf: asOf}); err != nil {
		return AWSSnapshotView{}, err
	}
	if view.Health, err = t.awsHealth(ctx, tx, scope, AWSHealthQuery{Account: account, AsOf: asOf}); err != nil {
		return AWSSnapshotView{}, err
	}
	if view.Flags, err = t.awsSecurityFlags(ctx, tx, scope, AWSFlagQuery{Account: account, AsOf: asOf}); err != nil {
		return AWSSnapshotView{}, err
	}
	if err := tx.Commit(); err != nil {
		return AWSSnapshotView{}, err
	}
	return view, nil
}

// RegisterAWSPending inserts every unit that has no sync state yet as
// pending, in one transaction, so no reader sees a partial plan.
func (t *sqlTables) RegisterAWSPending(ctx context.Context, scope tenancy.OrgScope, units []AWSUnit, guard *uint64) error {
	if len(units) == 0 {
		return nil
	}
	for _, unit := range units {
		if !validAWSUnit(unit) {
			return ErrInvalidAWSInventory
		}
	}
	scope = inventoryScope(scope)
	org := scope.Write
	return t.write(ctx, func(tx *sql.Tx) error {
		for _, unit := range units {
			if err := t.lockAccount(ctx, tx, org, unit.Account, guard, false); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_sync_units (org_id, account, region, service, state) VALUES (?, ?, ?, ?, ?) ON CONFLICT (org_id, account, region, service) DO NOTHING`),
				org, unit.Account, unit.Region, unit.Service, AWSSyncPending); err != nil {
				return err
			}
		}
		return nil
	})
}

// inventoryWhere starts the conditions every unit-owned row read shares.
func (t *sqlTables) inventoryWhere(scope tenancy.OrgScope, alias, account, region, service string, asOf time.Time) sqlWhere {
	var where sqlWhere
	condition, args := visible(scope.Read, alias, "aws_sync_units", "account", "region", "service")
	where.add(condition, args...)
	temporal, temporalArgs := t.temporal(alias, asOf)
	where.add(temporal, temporalArgs...)
	where.equal(alias+".account", account)
	where.equal(alias+".region", region)
	where.equal(alias+".service", service)
	return where
}

func limitClause(limit int) string {
	if limit <= 0 {
		return ""
	}
	return " LIMIT " + strconv.Itoa(limit)
}

// resourcesSQL is the statement AWSResources runs.
func (t *sqlTables) resourcesSQL(scope tenancy.OrgScope, query AWSResourceQuery) (string, []any) {
	where := t.inventoryWhere(scope, "r", query.Account, query.Region, query.Service, query.AsOf)
	where.equal("r.resource_type", query.Type)
	where.equal("r.app", query.App)
	where.equal("r.arn", query.ARN)
	if query.NamePrefix != "" {
		prefix := asciiLower(query.NamePrefix)
		where.add("lower(r.name) >= ? AND lower(r.name) < ?", prefix, prefixUpperBound(prefix))
	}
	return t.q(`SELECT r.account, r.region, r.service, r.arn, r.resource_type, r.app, r.name, r.state, r.tags, r.relations, r.attributes, r.detail
 FROM aws_resources r WHERE ` + where.String() + ` ORDER BY r.account, r.region, r.service, r.arn` + limitClause(query.Limit)), where.args
}

func (t *sqlTables) AWSResources(ctx context.Context, scope tenancy.OrgScope, query AWSResourceQuery) ([]AWSResource, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsResources(ctx, db, scope, query)
}

func (t *sqlTables) awsResources(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSResourceQuery) ([]AWSResource, error) {
	statement, args := t.resourcesSQL(inventoryScope(scope), query)
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSResource, 0)
	for rows.Next() {
		var row AWSResource
		var tags, relations, attributes, detail []byte
		if err := rows.Scan(&row.Account, &row.Region, &row.Service, &row.ARN, &row.Type, &row.App, &row.Name, &row.State, &tags, &relations, &attributes, &detail); err != nil {
			return nil, err
		}
		if row.Tags, err = decodeTableMap(tags); err != nil {
			return nil, err
		}
		if row.Relations, err = decodeTableList[AWSRelation](relations); err != nil {
			return nil, err
		}
		if row.Attributes, err = decodeTableMap(attributes); err != nil {
			return nil, err
		}
		row.Detail = scannedJSON(detail)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (t *sqlTables) AWSHealth(ctx context.Context, scope tenancy.OrgScope, query AWSHealthQuery) ([]AWSHealth, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsHealth(ctx, db, scope, query)
}

func (t *sqlTables) awsHealth(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSHealthQuery) ([]AWSHealth, error) {
	scope = inventoryScope(scope)
	where := t.inventoryWhere(scope, "h", query.Account, query.Region, query.Service, query.AsOf)
	where.equal("h.state", query.State)
	if query.ARN != "" {
		where.add("(h.arn = ? OR h.ref = ?)", query.ARN, query.ARN)
	}
	rows, err := q.QueryContext(ctx, t.q(`SELECT h.account, h.region, h.service, h.arn, h.kind, h.ref, h.state, h.summary, h.since, h.children, h.detail
 FROM aws_health h WHERE `+where.String()+` ORDER BY h.account, h.region, h.service, h.arn, h.kind, h.ref`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSHealth, 0)
	for rows.Next() {
		var row AWSHealth
		var since tableTimeValue
		var children, detail []byte
		if err := rows.Scan(&row.Account, &row.Region, &row.Service, &row.ARN, &row.Kind, &row.Ref, &row.State, &row.Summary, &since, &children, &detail); err != nil {
			return nil, err
		}
		row.Since = since.Time
		if row.Children, err = decodeTableList[string](children); err != nil {
			return nil, err
		}
		row.Detail = scannedJSON(detail)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (t *sqlTables) AWSSecurityFlags(ctx context.Context, scope tenancy.OrgScope, query AWSFlagQuery) ([]AWSSecurityFlag, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsSecurityFlags(ctx, db, scope, query)
}

func (t *sqlTables) awsSecurityFlags(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSFlagQuery) ([]AWSSecurityFlag, error) {
	scope = inventoryScope(scope)
	where := t.inventoryWhere(scope, "f", query.Account, query.Region, query.Service, query.AsOf)
	where.equal("f.flag", query.Flag)
	where.equal("f.arn", query.ARN)
	rows, err := q.QueryContext(ctx, t.q(`SELECT f.account, f.region, f.service, f.arn, f.flag, f.summary, f.detail
 FROM aws_security_flags f WHERE `+where.String()+` ORDER BY f.account, f.region, f.service, f.arn, f.flag`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSSecurityFlag, 0)
	for rows.Next() {
		var row AWSSecurityFlag
		var detail []byte
		if err := rows.Scan(&row.Account, &row.Region, &row.Service, &row.ARN, &row.Flag, &row.Summary, &detail); err != nil {
			return nil, err
		}
		row.Detail = scannedJSON(detail)
		out = append(out, row)
	}
	return out, rows.Err()
}

// changesSQL is the statement AWSChanges runs.
func (t *sqlTables) changesSQL(scope tenancy.OrgScope, query AWSChangeQuery) (string, []any) {
	var where sqlWhere
	condition, args := visible(scope.Read, "c", "aws_change_coverage", "account", "region")
	where.add(condition, args...)
	where.equal("c.account", query.Account)
	where.equal("c.region", query.Region)
	where.equal("c.service", query.Service)
	where.equal("c.resource_arn", query.ResourceARN)
	where.equal("c.app", query.App)
	if !query.Since.IsZero() {
		where.add("c.event_time >= ?", t.at(query.Since))
	}
	if !query.Until.IsZero() {
		where.add("c.event_time < ?", t.at(query.Until))
	}
	return t.q(`SELECT c.event_id, c.account, c.region, c.service, c.event_time, c.event_name, c.actor_class, c.actor, c.resource_arn, c.app, c.summary, c.fields, c.detail
 FROM aws_changes c WHERE ` + where.String() + ` ORDER BY c.event_time, c.event_id` + limitClause(query.Limit)), where.args
}

func (t *sqlTables) AWSChanges(ctx context.Context, scope tenancy.OrgScope, query AWSChangeQuery) ([]AWSChange, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsChanges(ctx, db, scope, query)
}

func (t *sqlTables) awsChanges(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, query AWSChangeQuery) ([]AWSChange, error) {
	statement, args := t.changesSQL(inventoryScope(scope), query)
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSChange, 0)
	for rows.Next() {
		var row AWSChange
		var at tableTimeValue
		var fields, detail []byte
		if err := rows.Scan(&row.EventID, &row.Account, &row.Region, &row.Service, &at, &row.EventName, &row.ActorClass, &row.Actor, &row.ResourceARN, &row.App, &row.Summary, &fields, &detail); err != nil {
			return nil, err
		}
		row.EventTime = at.Time
		if row.Fields, err = decodeTableMap(fields); err != nil {
			return nil, err
		}
		row.Detail = scannedJSON(detail)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (t *sqlTables) AWSChangeCoverage(ctx context.Context, scope tenancy.OrgScope, account, region string) ([]AWSPairCoverage, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsChangeCoverage(ctx, db, scope, account, region)
}

func (t *sqlTables) awsChangeCoverage(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope, account, region string) ([]AWSPairCoverage, error) {
	scope = inventoryScope(scope)
	var where sqlWhere
	condition, args := visible(scope.Read, "c", "aws_change_coverage", "account", "region")
	where.add(condition, args...)
	where.equal("c.account", account)
	where.equal("c.region", region)
	rows, err := q.QueryContext(ctx, t.q(`SELECT c.org_id, c.account, c.region, c.covered_from, c.covered_until FROM aws_change_coverage c
 WHERE `+where.String()+` ORDER BY c.account, c.region, c.covered_from`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSPairCoverage, 0)
	for rows.Next() {
		var org, rowAccount, rowRegion string
		var from, to tableTimeValue
		if err := rows.Scan(&org, &rowAccount, &rowRegion, &from, &to); err != nil {
			return nil, err
		}
		if last := len(out) - 1; last < 0 || out[last].Account != rowAccount || out[last].Region != rowRegion {
			out = append(out, AWSPairCoverage{Org: org, Account: rowAccount, Region: rowRegion})
		}
		out[len(out)-1].Intervals = append(out[len(out)-1].Intervals, AWSInterval{From: from.Time, To: to.Time})
	}
	return out, rows.Err()
}

func (t *sqlTables) AWSPairs(ctx context.Context, scope tenancy.OrgScope) ([]AWSPair, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	return t.awsPairs(ctx, db, scope)
}

func (t *sqlTables) awsPairs(ctx context.Context, q tablesQuerier, scope tenancy.OrgScope) ([]AWSPair, error) {
	scope = inventoryScope(scope)
	rows, err := q.QueryContext(ctx, t.q(`SELECT account, region FROM aws_sync_units WHERE org_id = ?
 UNION SELECT account, region FROM aws_change_coverage WHERE org_id = ? ORDER BY 1, 2`), scope.Write, scope.Write)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSPair, 0)
	for rows.Next() {
		var pair AWSPair
		if err := rows.Scan(&pair.Account, &pair.Region); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
