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
// a smaller part can avoid.
func evidenceSaveTimedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pgErr) && pgErr.Code == "57014"
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

// writeRetainedEvidenceEvents writes the records retention would keep. A
// record older than the cutoff would be removed by the next retention pass,
// and writing it would re-create a month retention already dropped, so it is
// counted as expired and not written.
func writeRetainedEvidenceEvents(ctx context.Context, tx *sql.Tx, key string, records []EvidenceRecord, heartbeat time.Duration, cutoff time.Time) (evidenceWriteStats, error) {
	retained := records
	if !cutoff.IsZero() {
		retained = make([]EvidenceRecord, 0, len(records))
		for _, record := range records {
			if record.At.Before(cutoff) {
				continue
			}
			retained = append(retained, record)
		}
	}
	stats, err := writeEvidenceEvents(ctx, tx, key, retained, heartbeat)
	stats.Offered = len(records)
	stats.Expired = len(records) - len(retained)
	return stats, err
}
