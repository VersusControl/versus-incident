package storage

import (
	"context"
	"encoding/json"
	"time"
)

// queryOccurrenceModels serves occurrence lists from the read models. A scope
// that has not finished its read-model conversion answers ErrEvidenceNotReady.
func (p *postgresProvider) queryOccurrenceModels(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return EvidencePage{}, err
	}
	// Occurrence lists page by first_seen; any other cursor layout restarts
	// from the first page.
	if query.Cursor != "" && cursor.Ordering != "first_seen" {
		return EvidencePage{}, ErrEvidenceConflict
	}
	cursor.Ordering = "first_seen"
	scopes, err := evidenceReadScopes(scope)
	if err != nil {
		return EvidencePage{}, err
	}
	keys := make([]string, len(scopes))
	for i, scope := range scopes {
		keys[i], err = evidenceScopeKey(scope)
		if err != nil {
			return EvidencePage{}, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := p.beginEvidenceRead(ctx)
	if err != nil {
		return EvidencePage{}, err
	}
	defer tx.Rollback()
	page := EvidencePage{Since: query.Since, Until: query.Until, Policy: p.EvidencePolicy(), Counts: map[string]int{}}
	highs, generations := make([]uint64, len(keys)), make([]uint64, len(keys))
	prunes := make([]uint64, len(keys))
	ready := true
	rows, err := tx.QueryContext(ctx, `SELECT scope_key,checkpoint,read_model_ready FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[])`, keys)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var key string
		var raw []byte
		var cp EvidenceCheckpoint
		var modelReady bool
		if err = rows.Scan(&key, &raw, &modelReady); err != nil {
			rows.Close()
			return page, err
		}
		ready = ready && modelReady
		if err = json.Unmarshal(raw, &cp); err != nil {
			rows.Close()
			return page, err
		}
		for i, candidate := range keys {
			if candidate == key {
				highs[i], generations[i] = cp.OccurrenceRevision, cp.Generation
				prunes[i] = cp.PruneRevision
			}
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, cp)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	if !ready {
		return page, ErrEvidenceNotReady
	}
	if query.Lookup {
		return queryCurrentOccurrenceLookup(ctx, tx, keys, query, page)
	}
	if query.Cursor == "" {
		cursor.OccurrenceHighs, cursor.Generations = highs, generations
		cursor.PruneRevisions = prunes
	} else {
		if len(cursor.OccurrenceHighs) != len(keys) || len(cursor.Generations) != len(keys) {
			return page, ErrInvalidEvidence
		}
		for i := range keys {
			if cursor.Generations[i] != generations[i] || cursor.OccurrenceHighs[i] > highs[i] || len(cursor.PruneRevisions) != len(keys) || cursor.PruneRevisions[i] != prunes[i] {
				return page, ErrEvidenceConflict
			}
		}
	}
	return p.queryOccurrenceReadPage(ctx, tx, keys, query, cursor, page)
}
