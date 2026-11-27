package storage

import (
	"context"
	"encoding/json"
	"time"
)

// queryOccurrenceModels serves occurrence lists from the read models. A scope
// that has not finished its read-model conversion answers ErrEvidenceNotReady.
func (p *postgresProvider) queryOccurrenceModels(parent context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	pages, page, err := p.occurrenceModelPages(parent, scope, query, nil)
	if err != nil {
		return page, err
	}
	return pages[0], nil
}

// QueryEvidenceSubjects reads the first page of one occurrence read for every
// subject in one statement, as QueryEvidence reads it for each one.
func (p *postgresProvider) QueryEvidenceSubjects(parent context.Context, scope EvidenceScope, query EvidenceQuery, subjects []EvidenceSubject) ([]EvidencePage, error) {
	if !evidenceSubjectsBatchable(query) || len(subjects) == 0 {
		return nil, ErrInvalidEvidence
	}
	for _, subject := range subjects {
		if subject.RootUID == "" {
			return nil, ErrInvalidEvidence
		}
	}
	pages, _, err := p.occurrenceModelPages(parent, scope, query, subjects)
	return pages, err
}

// occurrenceModelPages reads one page of query, or with subjects one first
// page per subject. On failure it also returns the page as far as it was read.
func (p *postgresProvider) occurrenceModelPages(parent context.Context, scope EvidenceScope, query EvidenceQuery, subjects []EvidenceSubject) ([]EvidencePage, EvidencePage, error) {
	query, cursor, err := prepareEvidenceQuery(scope, query)
	if err != nil {
		return nil, EvidencePage{}, err
	}
	// Occurrence lists page by first_seen. A link of this list that pages in
	// another order, or predates links naming their order, restarts the list
	// from its first page.
	if query.Cursor != "" && cursor.Ordering != "first_seen" {
		return nil, EvidencePage{}, ErrEvidenceCursorExpired
	}
	cursor.Ordering = "first_seen"
	scopes, err := evidenceReadScopes(scope)
	if err != nil {
		return nil, EvidencePage{}, err
	}
	keys := make([]string, len(scopes))
	for i, scope := range scopes {
		keys[i], err = evidenceScopeKey(scope)
		if err != nil {
			return nil, EvidencePage{}, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := p.beginEvidenceRead(ctx)
	if err != nil {
		return nil, EvidencePage{}, err
	}
	defer tx.Rollback()
	page := EvidencePage{Since: query.Since, Until: query.Until, Policy: p.EvidencePolicy(), Counts: map[string]int{}}
	highs, generations := make([]uint64, len(keys)), make([]uint64, len(keys))
	prunes := make([]uint64, len(keys))
	thins, thinned := make([]uint64, len(keys)), make([]time.Time, len(keys))
	ready := true
	rows, err := tx.QueryContext(ctx, `SELECT scope_key,checkpoint,read_model_ready FROM kubernetes_evidence_scopes WHERE scope_key=ANY($1::text[])`, keys)
	if err != nil {
		return nil, page, err
	}
	for rows.Next() {
		var key string
		var raw []byte
		var cp EvidenceCheckpoint
		var modelReady bool
		if err = rows.Scan(&key, &raw, &modelReady); err != nil {
			rows.Close()
			return nil, page, err
		}
		ready = ready && modelReady
		if err = json.Unmarshal(raw, &cp); err != nil {
			rows.Close()
			return nil, page, err
		}
		for i, candidate := range keys {
			if candidate == key {
				highs[i], generations[i] = cp.OccurrenceRevision, cp.Generation
				prunes[i] = cp.PruneRevision
				thins[i], thinned[i] = cp.ThinRevision, cp.ThinnedThrough
			}
		}
		page.Checkpoint = mergeEvidenceCheckpoint(page.Checkpoint, cp)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, page, err
	}
	rows.Close()
	if !ready {
		return nil, page, ErrEvidenceNotReady
	}
	if query.Lookup {
		page, err = queryCurrentOccurrenceLookup(ctx, tx, keys, query, page)
		return []EvidencePage{page}, page, err
	}
	if query.Cursor == "" {
		cursor.OccurrenceHighs, cursor.Generations = highs, generations
		cursor.PruneRevisions = prunes
		cursor.ThinRevisions = thins
	} else {
		if len(cursor.OccurrenceHighs) != len(keys) || len(cursor.Generations) != len(keys) || !cursorThinRevisionsFit(cursor, len(keys)) {
			return nil, page, ErrInvalidEvidence
		}
		for i := range keys {
			if len(cursor.PruneRevisions) != len(keys) || cursor.OccurrenceHighs[i] > highs[i] && cursor.Generations[i] == generations[i] && cursor.PruneRevisions[i] == prunes[i] {
				return nil, page, ErrEvidenceConflict
			}
			if cursor.Generations[i] != generations[i] || cursor.PruneRevisions[i] != prunes[i] {
				// History was pruned or replaced since the cursor was issued.
				return nil, page, ErrEvidenceCursorExpired
			}
			if cursorSamplesThinned(cursor, i, thins[i]) && occurrenceThinningReaches(query.Until, thinned[i]) {
				// Samples were thinned where this read looks for them.
				return nil, page, ErrEvidenceCursorExpired
			}
		}
	}
	if subjects != nil {
		pages, err := p.queryOccurrenceSubjectPages(ctx, tx, keys, scope, query, cursor, page, subjects)
		return pages, page, err
	}
	page, err = p.queryOccurrenceReadPage(ctx, tx, keys, query, cursor, page)
	return []EvidencePage{page}, page, err
}
