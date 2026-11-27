package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// defaultEvidenceSaveChunk is the most journal records one save
	// transaction writes unless configured otherwise.
	defaultEvidenceSaveChunk = 500
	// minEvidenceSaveChunk is the smallest part a timed-out save is split to.
	minEvidenceSaveChunk = 25
	// maxEvidenceSavePartBytes bounds the payload bytes of one save part.
	maxEvidenceSavePartBytes = 4 << 20
	// defaultEvidenceSaveTimeout bounds each save transaction.
	defaultEvidenceSaveTimeout = 20 * time.Second
)

func (stats *evidenceWriteStats) add(other evidenceWriteStats) {
	stats.High = max(stats.High, other.High)
	stats.Inserted += other.Inserted
	stats.Refreshed += other.Refreshed
	stats.Expired += other.Expired
}

// latestEvidenceRecords keeps one record per event, the one a single save
// would keep: highest priority, then latest observation. A capture split across
// transactions must not append two versions of one event.
func latestEvidenceRecords(records []EvidenceRecord) []EvidenceRecord {
	position := make(map[string]int, len(records))
	kept := make([]EvidenceRecord, 0, len(records))
	for _, record := range records {
		index, seen := position[record.ID]
		if !seen {
			position[record.ID] = len(kept)
			kept = append(kept, record)
			continue
		}
		current := kept[index]
		if record.Priority > current.Priority || record.Priority == current.Priority && record.ObservedAt.After(current.ObservedAt) {
			kept[index] = record
		}
	}
	return kept
}

// evidencePartEnd ends a part at size records or at the part byte bound,
// whichever comes first; a part always holds at least one record.
func evidencePartEnd(records []EvidenceRecord, start, size int) int {
	end, bytes := start, 0
	for end < len(records) && end-start < size {
		if end > start && bytes+len(records[end].Payload) > maxEvidenceSavePartBytes {
			break
		}
		bytes += len(records[end].Payload)
		end++
	}
	return end
}

// evidenceSaveTimedOut reports a save transaction that ran out of time, which
// a smaller part can avoid: its deadline, the database's statement timeout,
// or its lock timeout.
func evidenceSaveTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03")
}

// evidenceSaveError types the database's refusal of text it cannot hold. The
// boundary normalizes such text first, so this is a backstop: a refusal the
// same batch will always get must never read as an outage that is retried
// every cycle.
func evidenceSaveError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22P05", "22021", "22P02":
			return fmt.Errorf("%w (SQLSTATE %s)", ErrEvidenceUnstorable, pgErr.Code)
		}
	}
	return err
}

// evidenceCutoff is the instant before which retention removes journal rows,
// or zero when retention is off.
func (p *postgresProvider) evidenceCutoff() time.Time {
	if p.evidenceRetention <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(-p.evidenceRetention)
}

// catchUpWithoutMonth keeps catch-up resolutions out of months whose
// partition is gone: retention dropped it, or it was never made. A catch-up
// resolution is dated just after the issue's last sighting, which can be in
// such a month. The issue still resolves, but its journal record is left out
// and its closing observation is not written, so the save never recreates
// the month. It answers the records to write and the state whose
// observations are written; the full state still links the issue's versions.
func catchUpWithoutMonth(ctx context.Context, tx *sql.Tx, state evidenceState, records []EvidenceRecord) ([]EvidenceRecord, evidenceState, error) {
	if len(state.caughtUp) == 0 {
		return records, state, nil
	}
	dropped := map[string]bool{}
	for _, closed := range state.caughtUp {
		month := closed.UTC().Format("200601")
		if _, checked := dropped[month]; checked {
			continue
		}
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT bool_and(COALESCE((SELECT relkind<>'p' FROM pg_class WHERE oid=to_regclass(parent)),TRUE) OR to_regclass(child) IS NOT NULL)
 FROM (VALUES ('kubernetes_evidence_events','evidence_event_month_'||$1::text),('kubernetes_evidence_occurrence_observations','evidence_observation_month_'||$1::text)) AS months(parent,child)`, month).Scan(&present); err != nil {
			return nil, state, err
		}
		dropped[month] = !present
	}
	kept := records[:0:0]
	for _, record := range records {
		if record.Type == EvidenceIssueCaughtUp && dropped[record.At.UTC().Format("200601")] {
			continue
		}
		kept = append(kept, record)
	}
	written := state
	written.Observations = make([]occurrenceObservation, 0, len(state.Observations))
	for _, observation := range state.Observations {
		if closed, caught := state.caughtUp[observation.OccurrenceID]; caught && observation.At.Equal(closed) && dropped[closed.UTC().Format("200601")] {
			continue
		}
		written.Observations = append(written.Observations, observation)
	}
	return kept, written, nil
}
