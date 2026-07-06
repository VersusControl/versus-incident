package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// catalog_pg_store.go — X28 Phase A (Core, OSS): the Postgres-explicit
// implementation of the agent.CatalogStore seam (X9-T13) over the typed
// signal tables (migration 003): the vs_patterns catalog root, its vs_logs
// 1:1 child, and the catalog-scoped vs_services entity.
//
// It replaces the whole-blob "patterns" catalog on the Postgres backend so
// the log catalog SCALES and is SEARCHABLE (real columns + indexes, not an
// opaque JSON document). It is OSS-owned because patterns/logs/services are
// OSS-usable — a single-tenant OSS Postgres deployment gets the searchable
// catalog immediately, with no enterprise license. It is installed at boot
// only when the storage.Provider satisfies storage.SQLAccessor (Postgres);
// the file backend keeps the inline whole-blob patterns.json path
// byte-for-byte unchanged (nil store ⇒ inline path).
//
// Partition-aware, tier-neutral. Every learned row carries an instance_index
// ordinal in the vs_logs PK: OSS single-instance always writes 0 (one row per
// pattern), so the store names no HA policy. The enterprise HA install (Phase
// B) constructs the SAME store with the instance ordinal, making each
// instance the single writer of its own instance_index rows; the fleet read
// (Snapshot) SUMs across partitions in SQL (SUM … GROUP BY), which is the
// exact disjoint-stream union with no lost updates and no advisory lock.
//
// LEARNED vs CURATED state are separated exactly as the enterprise blob store
// did, but by COLUMN rather than by document: vs_logs holds only learned
// state (template/count/baseline/samples/seen), and the vs_patterns root
// holds the fleet-wide operator-curated overlay (verdict/tags/tombstone/
// service reassignment). Persist writes learned rows; Curate writes the root
// curated columns and the vs_services state. Reads fold the two together.

// Table names are Go CONSTANTS — never interpolated from input — so every
// query below is SQLi-safe (values are always bound as $N parameters).
const (
	tblPatterns = "vs_patterns"
	tblLogs     = "vs_logs"
	tblServices = "vs_services"

	// pgPatternKindLog is the vs_patterns.kind the OSS catalog store owns.
	// The enterprise intel store (Phase B) owns 'metric'/'trace' on the same
	// root; the id namespacing keeps them from colliding under (org_id, id).
	pgPatternKindLog = "log"
	// pgVerdictKnown is the OSS brain's only alert-suppression verdict. A
	// delete-tombstone folds onto it at Load so a suppressed pattern is not
	// re-alerted even while live mining keeps re-learning it.
	pgVerdictKnown = "known"
	// pgServiceUnknown is the OSS sentinel for an unattributed signal. A
	// pattern carrying it (or "") has no real attribution, so it must never
	// clobber a real service in the real-wins upsert.
	pgServiceUnknown = "_unknown"
)

// ---------------------------------------------------------------------------
// Query text (package-scoped constants so the SQLi-safety / query-construction
// unit tests can assert on them without a live database).
// ---------------------------------------------------------------------------

const (
	// Load: this instance's partition rows joined to the curated root.
	sqlCatalogLoadLogs = `
		SELECT p.id, COALESCE(p.service, ''), COALESCE(p.verdict, ''), p.tags, p.deleted,
		       l.template, COALESCE(l.source, ''), COALESCE(l.rule_name, ''),
		       l.count, l.baseline_frequency, l.samples, l.first_seen, l.last_seen
		FROM vs_logs l
		JOIN vs_patterns p
		  ON p.org_id = l.org_id AND p.id = l.pattern_id AND p.kind = 'log'
		WHERE l.org_id = $1 AND l.instance_index = $2`

	// Load / Snapshot: every non-deleted service for the org.
	sqlCatalogSelectServices = `
		SELECT name, manual, first_seen
		FROM vs_services
		WHERE org_id = $1 AND deleted = FALSE`

	// Persist: upsert the identity root, updating ONLY service (real-wins) —
	// verdict/tags/deleted are curated columns Persist must never touch.
	sqlCatalogUpsertRoot = `
		INSERT INTO vs_patterns (org_id, id, kind, service)
		VALUES ($1, $2, 'log', $3)
		ON CONFLICT (org_id, id) DO UPDATE
		SET service = CASE
		        WHEN EXCLUDED.service <> '' AND EXCLUDED.service <> '_unknown'
		            THEN EXCLUDED.service
		        ELSE vs_patterns.service
		    END,
		    updated_at = NOW()`

	// Persist: upsert THIS instance's learned log row (single-writer per
	// partition). first_seen is kept on conflict (earliest sighting wins).
	sqlCatalogUpsertLog = `
		INSERT INTO vs_logs
		    (org_id, pattern_id, instance_index, template, source, rule_name,
		     count, baseline_frequency, samples, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (org_id, pattern_id, instance_index) DO UPDATE
		SET template            = EXCLUDED.template,
		    source              = EXCLUDED.source,
		    rule_name           = EXCLUDED.rule_name,
		    count               = EXCLUDED.count,
		    baseline_frequency  = EXCLUDED.baseline_frequency,
		    samples             = EXCLUDED.samples,
		    last_seen           = EXCLUDED.last_seen`

	// Persist: convergent service discovery — insert only if absent so an
	// operator's curated grace/manual/delete state is never clobbered.
	sqlCatalogInsertServiceIfAbsent = `
		INSERT INTO vs_services (org_id, name, manual, first_seen)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, name) DO NOTHING`

	// Snapshot: fleet-wide read view — SUM/MIN/MAX across partitions, joined
	// to the curated root, scalar attributes from the lowest-index partition.
	sqlCatalogSnapshotLogs = `
		SELECT p.id, COALESCE(p.service, ''), COALESCE(p.verdict, ''), p.tags,
		       agg.total_count, agg.first_seen, agg.last_seen, agg.total_baseline,
		       lo.template, COALESCE(lo.source, ''), COALESCE(lo.rule_name, ''), lo.samples
		FROM vs_patterns p
		JOIN LATERAL (
		    SELECT SUM(count) AS total_count, MIN(first_seen) AS first_seen,
		           MAX(last_seen) AS last_seen, SUM(baseline_frequency) AS total_baseline
		    FROM vs_logs
		    WHERE org_id = p.org_id AND pattern_id = p.id
		) agg ON agg.total_count IS NOT NULL
		JOIN LATERAL (
		    SELECT template, source, rule_name, samples
		    FROM vs_logs
		    WHERE org_id = p.org_id AND pattern_id = p.id
		    ORDER BY instance_index ASC
		    LIMIT 1
		) lo ON TRUE
		WHERE p.org_id = $1 AND p.kind = 'log' AND p.deleted = FALSE`

	// Curate — one statement per operator mutation (all values bound).
	sqlCurateVerdict        = `UPDATE vs_patterns SET verdict = $3, updated_at = NOW() WHERE org_id = $1 AND id = $2 AND kind = 'log'`
	sqlCurateTags           = `UPDATE vs_patterns SET tags = $3, updated_at = NOW() WHERE org_id = $1 AND id = $2 AND kind = 'log'`
	sqlCurateMarkKnown      = `UPDATE vs_patterns SET verdict = 'known', updated_at = NOW() WHERE org_id = $1 AND id = $2 AND kind = 'log' AND COALESCE(verdict, '') <> 'known'`
	sqlCurateRepointService = `UPDATE vs_patterns SET service = $3, updated_at = NOW() WHERE org_id = $1 AND id = $2 AND kind = 'log'`
	sqlCurateDelete         = `UPDATE vs_patterns SET deleted = TRUE, updated_at = NOW() WHERE org_id = $1 AND id = $2 AND kind = 'log'`
	sqlCurateResetPatterns  = `DELETE FROM vs_patterns WHERE org_id = $1 AND kind = 'log'`
	sqlCurateResetServices  = `DELETE FROM vs_services WHERE org_id = $1`
	sqlCurateEndGrace       = `UPDATE vs_services SET first_seen = $3, updated_at = NOW() WHERE org_id = $1 AND name = $2`
	sqlCurateRestartGrace   = `UPDATE vs_services SET first_seen = NOW(), updated_at = NOW() WHERE org_id = $1 AND name = $2`
	sqlCurateCreateService  = `INSERT INTO vs_services (org_id, name, manual, first_seen) VALUES ($1, $2, TRUE, NOW()) ON CONFLICT (org_id, name) DO UPDATE SET manual = TRUE, deleted = FALSE, first_seen = EXCLUDED.first_seen, updated_at = NOW()`
	sqlCurateDeleteService  = `UPDATE vs_services SET deleted = TRUE, updated_at = NOW() WHERE org_id = $1 AND name = $2`
	sqlRenameSelectService  = `SELECT first_seen, manual FROM vs_services WHERE org_id = $1 AND name = $2 AND deleted = FALSE`
	sqlRenameTombstoneOld   = `UPDATE vs_services SET deleted = TRUE, updated_at = NOW() WHERE org_id = $1 AND name = $2`
	sqlRenameUpsertNewSvc   = `INSERT INTO vs_services (org_id, name, manual, first_seen) VALUES ($1, $2, $3, $4) ON CONFLICT (org_id, name) DO UPDATE SET manual = EXCLUDED.manual, deleted = FALSE, first_seen = EXCLUDED.first_seen, updated_at = NOW()`
)

// pgCatalogStore implements agent.CatalogStore over the typed signal tables.
type pgCatalogStore struct {
	db            *sql.DB
	orgID         string
	instanceIndex int

	// scrub re-scrubs each learned sample at the STORAGE boundary on Persist
	// (defence in depth), symmetric with the enterprise typed baseline store
	// (intel.pgBaselineStore re-scrubs via rescrubSamples on every put). The
	// samples ring is already scrubbed at the LEARN boundary
	// (Catalog.RecordSample → PushSample), and Signal.Raw is never a sample
	// source, so this is NOT a leak fix — it makes both signal-table write
	// paths defence-in-depth-equal so a future refactor that composed a sample
	// from an un-scrubbed source still cannot land a secret in a signal table.
	// nil (the default, and the file backend) leaves the ring byte-for-byte
	// unchanged; it is threaded on the write path only via SetSampleScrubber.
	scrub core.Scrubber

	// mu guards markedKnown only (the *sql.DB is concurrency-safe on its own).
	mu sync.Mutex
	// markedKnown caps the auto-promotion churn: the brain re-issues MarkKnown
	// every tick a known pattern is observed (its in-memory verdict is not
	// re-read from the root at runtime), so without this an UPDATE would run
	// every tick. We record ids this process has already marked so the second
	// and later calls short-circuit with no DB I/O — one write per known
	// pattern per process (mirrors the enterprise partition store).
	markedKnown map[string]struct{}
}

// NewPostgresCatalogStore builds the OSS Postgres catalog store over the typed
// signal tables. db is obtained via storage.SQLAccessor.DB(); orgID is the
// boot-pinned deployment org (storage.DefaultOrgID for single-tenant OSS);
// instanceIndex is the write-shard ordinal (0 for OSS single-instance; the
// enterprise HA install passes the cluster ordinal). Install it via
// agent.SetCatalogStore at boot, before the worker starts and before
// LoadCatalog. This is the seam the Enterprise Engineer's Phase B rides: the
// enterprise intel store adds vs_metrics/vs_traces on the SAME vs_patterns
// root, and the enterprise HA install constructs THIS store with the ordinal.
func NewPostgresCatalogStore(db *sql.DB, orgID string, instanceIndex int) CatalogStore {
	return &pgCatalogStore{
		db:            db,
		orgID:         storage.NormalizeOrgID(orgID),
		instanceIndex: instanceIndex,
		markedKnown:   make(map[string]struct{}),
	}
}

// SetSampleScrubber threads the pipeline redactor onto the store so the samples
// ring is re-scrubbed at the storage boundary on Persist (defence in depth),
// symmetric with the enterprise intel store's Store.SetSampleScrubber. It is a
// no-op for a nil scrubber (the ring is left unchanged), so community and file
// backend behaviour is byte-for-byte unchanged. Called on the write path only
// at boot (before the worker starts); read-only callers leave it unset.
func (s *pgCatalogStore) SetSampleScrubber(scrub core.Scrubber) {
	s.scrub = scrub
}

// Load returns this instance's partition working set (its own learned log
// rows), with the curated root columns folded in so the brain sees fleet-wide
// curation (verdict / tombstone-suppression / service attribution) from boot.
func (s *pgCatalogStore) Load() (map[string]*Pattern, map[string]*ServiceInfo, error) {
	patterns := make(map[string]*Pattern)
	services := make(map[string]*ServiceInfo)

	rows, err := s.db.Query(sqlCatalogLoadLogs, s.orgID, s.instanceIndex)
	if err != nil {
		return patterns, services, fmt.Errorf("agent: pg catalog load logs: %w", err)
	}
	for rows.Next() {
		var (
			id, service, verdict       string
			tagsRaw, samplesRaw        []byte
			deleted                    bool
			template, source, ruleName string
			count                      int64
			baselineFreq               float64
			firstSeen, lastSeen        time.Time
		)
		if err := rows.Scan(&id, &service, &verdict, &tagsRaw, &deleted,
			&template, &source, &ruleName, &count, &baselineFreq,
			&samplesRaw, &firstSeen, &lastSeen); err != nil {
			rows.Close()
			return patterns, services, fmt.Errorf("agent: pg catalog scan log: %w", err)
		}
		p := &Pattern{
			ID:                id,
			OrgID:             s.orgID,
			Template:          template,
			FirstSeen:         firstSeen,
			LastSeen:          lastSeen,
			Count:             int(count),
			BaselineFrequency: baselineFreq,
			Verdict:           verdict,
			RuleName:          ruleName,
			Source:            source,
			Service:           service,
			Tags:              decodeStringSlice(tagsRaw),
			Samples:           decodeStringSlice(samplesRaw),
		}
		// Fold a tombstone onto the brain's suppression verdict so a deleted
		// pattern is not re-alerted even while live mining re-learns it.
		if deleted {
			p.Verdict = pgVerdictKnown
		}
		patterns[id] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return patterns, services, fmt.Errorf("agent: pg catalog load logs rows: %w", err)
	}
	rows.Close()

	if err := s.scanServices(services); err != nil {
		return patterns, services, err
	}
	return patterns, services, nil
}

// scanServices loads every non-deleted service into dst.
func (s *pgCatalogStore) scanServices(dst map[string]*ServiceInfo) error {
	rows, err := s.db.Query(sqlCatalogSelectServices, s.orgID)
	if err != nil {
		return fmt.Errorf("agent: pg catalog load services: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name      string
			manual    bool
			firstSeen time.Time
		)
		if err := rows.Scan(&name, &manual, &firstSeen); err != nil {
			return fmt.Errorf("agent: pg catalog scan service: %w", err)
		}
		dst[name] = &ServiceInfo{OrgID: s.orgID, FirstSeen: firstSeen, Manual: manual}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent: pg catalog load services rows: %w", err)
	}
	return nil
}

// Persist writes this instance's working set: the identity root (service
// real-wins, curated columns untouched) + this instance's learned log rows +
// convergent service discovery. Deletes/labels/resets do NOT ride Persist —
// they route through Curate — so Persist is purely additive/updating.
func (s *pgCatalogStore) Persist(patterns map[string]*Pattern, services map[string]*ServiceInfo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("agent: pg catalog persist begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for id, p := range patterns {
		if p == nil {
			continue
		}
		if _, err := tx.Exec(sqlCatalogUpsertRoot, s.orgID, id, p.Service); err != nil {
			return fmt.Errorf("agent: pg catalog persist root %q: %w", id, err)
		}
		if _, err := tx.Exec(sqlCatalogUpsertLog,
			s.orgID, id, s.instanceIndex, p.Template, nullIfEmpty(p.Source),
			nullIfEmpty(p.RuleName), int64(p.Count), p.BaselineFrequency,
			encodeStringSlice(rescrubSamples(p.Samples, s.scrub)), utcOrNow(p.FirstSeen), utcOrNow(p.LastSeen),
		); err != nil {
			return fmt.Errorf("agent: pg catalog persist log %q: %w", id, err)
		}
	}
	for name, svc := range services {
		if svc == nil {
			continue
		}
		if _, err := tx.Exec(sqlCatalogInsertServiceIfAbsent,
			s.orgID, name, svc.Manual, utcOrNow(svc.FirstSeen),
		); err != nil {
			return fmt.Errorf("agent: pg catalog persist service %q: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent: pg catalog persist commit: %w", err)
	}
	return nil
}

// Snapshot returns the unified fleet-wide read view: counts SUMmed across
// partitions, scalar attributes from the lowest-index partition, curated root
// columns applied, tombstoned patterns/services excluded.
func (s *pgCatalogStore) Snapshot() ([]*Pattern, map[string]ServiceInfo, error) {
	rows, err := s.db.Query(sqlCatalogSnapshotLogs, s.orgID)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: pg catalog snapshot: %w", err)
	}
	var out []*Pattern
	for rows.Next() {
		var (
			id, service, verdict       string
			tagsRaw, samplesRaw        []byte
			totalCount                 int64
			firstSeen, lastSeen        time.Time
			totalBaseline              float64
			template, source, ruleName string
		)
		if err := rows.Scan(&id, &service, &verdict, &tagsRaw,
			&totalCount, &firstSeen, &lastSeen, &totalBaseline,
			&template, &source, &ruleName, &samplesRaw); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("agent: pg catalog snapshot scan: %w", err)
		}
		out = append(out, &Pattern{
			ID:                id,
			OrgID:             s.orgID,
			Template:          template,
			FirstSeen:         firstSeen,
			LastSeen:          lastSeen,
			Count:             int(totalCount),
			BaselineFrequency: totalBaseline,
			Verdict:           verdict,
			RuleName:          ruleName,
			Source:            source,
			Service:           service,
			Tags:              decodeStringSlice(tagsRaw),
			Samples:           decodeStringSlice(samplesRaw),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("agent: pg catalog snapshot rows: %w", err)
	}
	rows.Close()

	services := make(map[string]ServiceInfo)
	svcPtrs := make(map[string]*ServiceInfo)
	if err := s.scanServices(svcPtrs); err != nil {
		return nil, nil, err
	}
	for name, svc := range svcPtrs {
		services[name] = *svc
	}
	return out, services, nil
}

// Curate applies one operator mutation to the curated root columns or the
// vs_services state. Learned counts never route here.
func (s *pgCatalogStore) Curate(edit CatalogEdit) error {
	switch edit.Kind {
	case CatalogEditLabel:
		if edit.Verdict != nil {
			if _, err := s.db.Exec(sqlCurateVerdict, s.orgID, edit.PatternID, *edit.Verdict); err != nil {
				return fmt.Errorf("agent: pg catalog label verdict: %w", err)
			}
			// A verdict change away from "known" (a clear, or a set to some
			// other value) must let this process re-promote later, so drop the
			// mark-known churn-cache entry.
			if *edit.Verdict != pgVerdictKnown {
				s.forgetMarkedKnown(edit.PatternID)
			}
		}
		if edit.Tags != nil {
			if _, err := s.db.Exec(sqlCurateTags, s.orgID, edit.PatternID, encodeStringSlice(edit.Tags)); err != nil {
				return fmt.Errorf("agent: pg catalog label tags: %w", err)
			}
		}
		return nil

	case CatalogEditMarkKnown:
		// Cap the per-tick auto-promotion re-issue to one DB write per pattern.
		s.mu.Lock()
		_, already := s.markedKnown[edit.PatternID]
		s.mu.Unlock()
		if already {
			return nil
		}
		if _, err := s.db.Exec(sqlCurateMarkKnown, s.orgID, edit.PatternID); err != nil {
			return fmt.Errorf("agent: pg catalog mark known: %w", err)
		}
		s.mu.Lock()
		s.markedKnown[edit.PatternID] = struct{}{}
		s.mu.Unlock()
		return nil

	case CatalogEditRepointService:
		if _, err := s.db.Exec(sqlCurateRepointService, s.orgID, edit.PatternID, edit.Service); err != nil {
			return fmt.Errorf("agent: pg catalog repoint service: %w", err)
		}
		return nil

	case CatalogEditDelete:
		if _, err := s.db.Exec(sqlCurateDelete, s.orgID, edit.PatternID); err != nil {
			return fmt.Errorf("agent: pg catalog delete: %w", err)
		}
		return nil

	case CatalogEditEndServiceGrace:
		// Zero-time first_seen ⇒ grace ended (always in the past).
		if _, err := s.db.Exec(sqlCurateEndGrace, s.orgID, edit.Service, time.Time{}); err != nil {
			return fmt.Errorf("agent: pg catalog end grace: %w", err)
		}
		return nil

	case CatalogEditRestartServiceGrace:
		if _, err := s.db.Exec(sqlCurateRestartGrace, s.orgID, edit.Service); err != nil {
			return fmt.Errorf("agent: pg catalog restart grace: %w", err)
		}
		return nil

	case CatalogEditCreateService:
		if _, err := s.db.Exec(sqlCurateCreateService, s.orgID, edit.Service); err != nil {
			return fmt.Errorf("agent: pg catalog create service: %w", err)
		}
		return nil

	case CatalogEditRenameService:
		return s.renameService(edit.Service, edit.NewService)

	case CatalogEditDeleteService:
		if _, err := s.db.Exec(sqlCurateDeleteService, s.orgID, edit.Service); err != nil {
			return fmt.Errorf("agent: pg catalog delete service: %w", err)
		}
		return nil

	case CatalogEditResetPatterns:
		// FK ON DELETE CASCADE drops the vs_logs child rows with the roots.
		if _, err := s.db.Exec(sqlCurateResetPatterns, s.orgID); err != nil {
			return fmt.Errorf("agent: pg catalog reset patterns: %w", err)
		}
		s.mu.Lock()
		s.markedKnown = make(map[string]struct{})
		s.mu.Unlock()
		return nil

	case CatalogEditResetServices:
		if _, err := s.db.Exec(sqlCurateResetServices, s.orgID); err != nil {
			return fmt.Errorf("agent: pg catalog reset services: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("agent: pg catalog unknown edit kind %q", edit.Kind)
	}
}

// renameService moves a manual service old→new: tombstone the old name and
// upsert the new one, preserving the grace anchor + manual flag. A missing
// old name is a tolerant no-op (the admin controller validates existence).
func (s *pgCatalogStore) renameService(oldName, newName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("agent: pg catalog rename begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		firstSeen time.Time
		manual    bool
	)
	err = tx.QueryRow(sqlRenameSelectService, s.orgID, oldName).Scan(&firstSeen, &manual)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nothing to move
	}
	if err != nil {
		return fmt.Errorf("agent: pg catalog rename select: %w", err)
	}
	if _, err := tx.Exec(sqlRenameTombstoneOld, s.orgID, oldName); err != nil {
		return fmt.Errorf("agent: pg catalog rename tombstone old: %w", err)
	}
	if _, err := tx.Exec(sqlRenameUpsertNewSvc, s.orgID, newName, manual, firstSeen); err != nil {
		return fmt.Errorf("agent: pg catalog rename upsert new: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent: pg catalog rename commit: %w", err)
	}
	return nil
}

// forgetMarkedKnown drops a pattern from the mark-known churn cache so a later
// count-based auto-promotion is not short-circuited into a no-op.
func (s *pgCatalogStore) forgetMarkedKnown(id string) {
	s.mu.Lock()
	delete(s.markedKnown, id)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// JSONB / value helpers
// ---------------------------------------------------------------------------

// rescrubSamples re-runs each ring entry through scrub via the OSS PushSample
// seam (which scrubs → one-lines → caps each entry → caps the ring), so the
// persisted ring is re-scrubbed at the storage boundary and no secret can
// survive to a signal table even if a future refactor composed a sample from
// an un-scrubbed source. A nil scrub (the file backend, or a read-only caller)
// returns the ring unchanged — the samples were already scrubbed at the learn
// boundary (RecordSample → PushSample), so community/file behaviour is
// byte-for-byte unchanged. It never mutates the caller's slice (PushSample
// appends into a fresh ring). This mirrors intel.rescrubSamples in the
// enterprise typed baseline store so both signal-table write paths are
// defence-in-depth-equal.
func rescrubSamples(samples []string, scrub core.Scrubber) []string {
	if scrub == nil || len(samples) == 0 {
		return samples
	}
	var out []string
	for _, sample := range samples {
		out = PushSample(out, sample, scrub)
	}
	return out
}

// encodeStringSlice marshals a string slice to a JSONB array payload,
// emitting "[]" for an empty/nil slice so the column never stores SQL NULL or
// a JSON "null" (both would break the NOT NULL / DEFAULT '[]' contract).
func encodeStringSlice(v []string) []byte {
	if len(v) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// decodeStringSlice unmarshals a JSONB array payload back to a string slice,
// normalizing empty/"[]"/"null" to nil so the round-trip matches the
// whole-blob path's omitempty semantics (len-0 ⇒ absent field).
func decodeStringSlice(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// nullIfEmpty maps "" to a SQL NULL for the nullable source/rule_name columns
// so an empty attribution reads back as "" via COALESCE.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// utcOrNow returns t in UTC, substituting NOW() (the caller's clock) only when
// t is the zero value — a learned row always carries a real first/last seen,
// so this is a defensive floor, not a routine path.
func utcOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}
