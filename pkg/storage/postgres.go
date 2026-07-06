package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	// pgx stdlib driver — registers "pgx" with database/sql.
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// PostgresOptions configures the Postgres backend. The DSN may be left
// empty in code when the factory will fall back to the POSTGRES_DSN
// environment variable.
type PostgresOptions struct {
	DSN string // postgres connection string or DSN URL
}

type postgresProvider struct {
	db *sql.DB
}

// DB implements the optional storage.SQLAccessor capability: it
// exposes the pooled *sql.DB so the enterprise module and the OSS Postgres
// catalog store can run their own migrations and typed queries on the same
// pool. The pool is owned by this provider — callers MUST NOT Close it.
func (p *postgresProvider) DB() *sql.DB { return p.db }

// NewPostgres opens a connection to Postgres, runs idempotent migrations,
// and returns a ready Provider. Callers must call Close when done.
func NewPostgres(opts PostgresOptions) (Provider, error) {
	if opts.DSN == "" {
		return nil, fmt.Errorf("storage: postgres DSN is required (set POSTGRES_DSN)")
	}
	db, err := sql.Open("pgx", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("storage: open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping postgres: %w", err)
	}
	p := &postgresProvider{db: db}
	if err := p.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: postgres migrate: %w", err)
	}
	return p, nil
}

// migrationAdvisoryLockKey is the fixed 64-bit key dedicated to serializing
// Versus schema-migration runs across every process sharing one Postgres. The
// value is ASCII "VERSUSM1" (Versus schema Migration, v1) and MUST stay
// constant forever: every Versus migrator — the OSS runner here and any
// caller of WithMigrationLock — locks on this same key, so exactly one
// migrates at a time and the rest wait. Changing it would silently let two
// migrators run concurrently again.
const migrationAdvisoryLockKey int64 = 0x5645525355534D31

// WithMigrationLock serializes a schema-migration run across every process
// that shares db's Postgres. It pins one pooled connection, takes a
// session-level pg_advisory_lock on the dedicated migrationAdvisoryLockKey,
// runs fn, then releases the lock — even if fn errors. When N replicas boot
// at once against a fresh database, exactly one acquires the lock and runs
// the DDL while the rest block on the lock; once it releases, the losers
// proceed and find every table already present (the migrations are
// idempotent), so concurrent first-boot is deterministic and never hits the
// pg_type / pg_class catalog races that bare `CREATE TABLE IF NOT EXISTS`
// suffers under concurrency.
//
// The lock is released two ways for safety: an explicit pg_advisory_unlock and
// `defer conn.Close()` — closing the session drops every session-level
// advisory lock it held, so the lock is freed even if the unlock call itself
// errors. A subsequent (sequential) call therefore always acquires cleanly and
// never deadlocks. The helper is generic OSS storage hardening: it carries no
// tier or migration-set knowledge, so an enterprise migration runner sharing
// the same pool can wrap its own run in the same WithMigrationLock(db, …) call
// and serialize against the OSS run on this one key.
func WithMigrationLock(db *sql.DB, fn func() error) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("storage: migration lock: acquire connection: %w", err)
	}
	// Closing the pinned session releases any session-level advisory lock it
	// still holds, so the lock can never leak past this function.
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockKey); err != nil {
		return fmt.Errorf("storage: migration lock: acquire: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockKey)
	}()

	return fn()
}

// ossMigrationLedger is the table RunSQLMigrations records applied OSS
// migration filenames in, so each file runs exactly once. It is a Go
// constant (never caller input), distinct from the enterprise ledger
// (versus_enterprise_schema_migrations) so the two migration sets track
// independently on the same pool.
const ossMigrationLedger = "versus_schema_migrations"

// migrate runs the OSS schema migrations under the shared migration advisory
// lock so concurrent first-boot replicas serialize instead of racing the
// Postgres catalog. The files are applied once each and tracked in the
// ledger by RunSQLMigrations, so a re-run is a no-op — which is REQUIRED now
// that migration 003 drops the whole-blob catalog table (an un-ledgered
// re-run would drop the typed tables on every boot).
func (p *postgresProvider) migrate() error {
	return WithMigrationLock(p.db, func() error {
		return RunSQLMigrations(p.db, migrationFiles, "migrations", ossMigrationLedger)
	})
}

// ---------------------------------------------------------------------------
// Blobs
//
// Each agent JSON document gets its own table so the schema mirrors the
// file backend's one-file-per-document layout (patterns.json → vs_patterns,
// shadow.json → vs_shadow, …). Any name without a dedicated table falls
// back to the shared vs_blobs table (e.g. the runtime AI cache). The table
// is always chosen from blobTables — a fixed allowlist — so it is never
// derived from caller input.
// ---------------------------------------------------------------------------

// blobTables maps a blob name to its dedicated Postgres table. Names not
// present here are stored in the shared vs_blobs table.
//
// "patterns" is deliberately ABSENT: the log-pattern catalog no longer
// rides a whole-blob table on Postgres. Migration 003 drops the old
// whole-blob vs_patterns and replaces it with the typed signal tables
// (vs_patterns root + vs_logs + vs_services); the Postgres catalog store
// (agent.NewPostgresCatalogStore) reads/writes those typed tables instead.
// Any residual whole-blob write under the "patterns" name (e.g. the file
// backend's inline path exercised on a Postgres deployment that has not
// installed the catalog store) falls back to vs_blobs, so nothing crashes.
var blobTables = map[string]string{
	"shadow":  "vs_shadow",
	"detect":  "vs_detect",
	"members": "vs_members",
	"teams":   "vs_teams",
}

// blobTable returns the table backing name. The result is always one of
// the constant table names above (or "vs_blobs"), so callers may safely
// interpolate it into a query.
func blobTable(name string) string {
	if t, ok := blobTables[name]; ok {
		return t
	}
	return "vs_blobs"
}

func (p *postgresProvider) ReadBlob(name string) ([]byte, error) {
	var data []byte
	q := fmt.Sprintf(`SELECT data FROM %s WHERE name = $1`, blobTable(name))
	err := p.db.QueryRow(q, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // missing blob → nil,nil per Provider contract
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read blob %q: %w", name, err)
	}
	return data, nil
}

func (p *postgresProvider) WriteBlob(name string, data []byte) error {
	q := fmt.Sprintf(`
		INSERT INTO %s (name, data, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (name) DO UPDATE
		SET data = EXCLUDED.data, updated_at = NOW()
	`, blobTable(name))
	if _, err := p.db.Exec(q, name, data); err != nil {
		return fmt.Errorf("storage: write blob %q: %w", name, err)
	}
	return nil
}

// CreateBlobIfAbsent implements the optional storage.BlobCreator capability
// (X9-T11) on the shared, multi-writer Postgres backend — the HA substrate.
// It is the atomic single-writer election: `INSERT … ON CONFLICT DO NOTHING`
// inserts only when the row is absent, so among N replicas racing to
// generate the same secret exactly one INSERT affects a row (written==true)
// and the rest no-op (written==false). The read-after-write confirms the
// authoritative row is durably present before returning, so the loser of the
// race can immediately ReadBlob(name) and adopt the winner's bytes. The table
// is chosen from the fixed blobTables allowlist (never caller input) and the
// name/data are bound as parameters, matching the package's parameterized-SQL
// convention.
func (p *postgresProvider) CreateBlobIfAbsent(name string, data []byte) (bool, error) {
	ins := fmt.Sprintf(`
		INSERT INTO %s (name, data, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (name) DO NOTHING
	`, blobTable(name))
	res, err := p.db.Exec(ins, name, data)
	if err != nil {
		return false, fmt.Errorf("storage: create blob %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: create blob %q rows: %w", name, err)
	}
	written := n == 1

	// Read-after-write: the authoritative row MUST exist now (we either
	// inserted it or a concurrent writer did). Confirming it before returning
	// guarantees the post-condition that ReadBlob(name) observes the one
	// surviving value for every caller.
	sel := fmt.Sprintf(`SELECT 1 FROM %s WHERE name = $1`, blobTable(name))
	var present int
	if err := p.db.QueryRow(sel, name).Scan(&present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("storage: create blob %q: row absent after write", name)
		}
		return false, fmt.Errorf("storage: create blob %q read-after-write: %w", name, err)
	}
	return written, nil
}

// ListBlobs returns every blob whose name begins with prefix. A model-state
// namespace (models/<org>/<agent>/…) falls back to vs_blobs, but the scan
// spans every physical blob table so the enumeration is correct for any
// prefix. LIKE wildcards in the prefix are escaped so a literal '%' or '_'
// in a blob name (e.g. an org id) matches literally.
func (p *postgresProvider) ListBlobs(prefix string) ([]Blob, error) {
	like := escapeLike(prefix) + "%"
	var out []Blob
	for _, table := range allBlobTables() {
		q := fmt.Sprintf(`SELECT name, data FROM %s WHERE name LIKE $1 ESCAPE '\'`, table)
		rows, err := p.db.Query(q, like)
		if err != nil {
			return nil, fmt.Errorf("storage: list blobs %s: %w", table, err)
		}
		for rows.Next() {
			var name string
			var data []byte
			if err := rows.Scan(&name, &data); err != nil {
				rows.Close()
				return nil, fmt.Errorf("storage: scan blob in %s: %w", table, err)
			}
			out = append(out, Blob{Name: name, Data: data})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("storage: list blobs %s: %w", table, err)
		}
	}
	return out, nil
}

// escapeLike escapes the SQL LIKE metacharacters in s so it is matched as a
// literal prefix. Pairs with the `ESCAPE '\'` clause in ListBlobs.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---------------------------------------------------------------------------
// Incidents
// ---------------------------------------------------------------------------

func (p *postgresProvider) SaveIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveIncident: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("storage: marshal incident: %w", err)
	}
	var ackedAt *time.Time
	if rec.AckedAt != nil {
		t := rec.AckedAt.UTC()
		ackedAt = &t
	}
	_, err = p.db.Exec(`
		INSERT INTO vs_incidents (id, data, created_at, acked_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		SET data = EXCLUDED.data, acked_at = EXCLUDED.acked_at
	`, rec.ID, data, rec.CreatedAt.UTC(), ackedAt)
	if err != nil {
		return fmt.Errorf("storage: save incident: %w", err)
	}
	// The database keeps incident history unbounded; retention is a
	// deliberate policy applied through the storage.Lifecycle purge
	// primitive rather than an implicit drop on every save.
	return nil
}

func (p *postgresProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	tx, err := p.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw []byte
	err = tx.QueryRow(
		`SELECT data FROM vs_incidents WHERE id = $1 FOR UPDATE`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("storage: ack fetch: %w", err)
	}

	var rec IncidentRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("storage: ack unmarshal: %w", err)
	}
	t := ackedAt.UTC()
	rec.AckedAt = &t

	updated, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("storage: ack re-marshal: %w", err)
	}
	_, err = tx.Exec(
		`UPDATE vs_incidents SET data = $2, acked_at = $3 WHERE id = $1`,
		id, updated, t,
	)
	if err != nil {
		return fmt.Errorf("storage: ack update: %w", err)
	}
	return tx.Commit()
}

func (p *postgresProvider) GetIncident(id string) (*IncidentRecord, error) {
	var raw []byte
	err := p.db.QueryRow(
		`SELECT data FROM vs_incidents WHERE id = $1`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get incident: %w", err)
	}
	var rec IncidentRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("storage: unmarshal incident: %w", err)
	}
	return &rec, nil
}

func (p *postgresProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(
			`SELECT data FROM vs_incidents ORDER BY created_at DESC LIMIT $1`, limit,
		)
	} else {
		rows, err = p.db.Query(
			`SELECT data FROM vs_incidents ORDER BY created_at DESC`,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list incidents: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
}

func scanIncidentRows(rows *sql.Rows) ([]*IncidentRecord, error) {
	var out []*IncidentRecord
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("storage: scan incident row: %w", err)
		}
		var rec IncidentRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("storage: unmarshal incident row: %w", err)
		}
		out = append(out, &rec)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Analyses
// ---------------------------------------------------------------------------

func (p *postgresProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveAnalysis: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("storage: marshal analysis: %w", err)
	}
	_, err = p.db.Exec(`
		INSERT INTO vs_analyses (id, incident_id, data, requested_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		SET data         = EXCLUDED.data,
		    incident_id  = EXCLUDED.incident_id,
		    requested_at = EXCLUDED.requested_at
	`, rec.ID, rec.IncidentID, data, rec.RequestedAt.UTC())
	if err != nil {
		return fmt.Errorf("storage: save analysis: %w", err)
	}
	return nil
}

func (p *postgresProvider) GetAnalysis(id string) (*AnalysisRecord, error) {
	var raw []byte
	err := p.db.QueryRow(
		`SELECT data FROM vs_analyses WHERE id = $1`, id,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get analysis: %w", err)
	}
	var rec AnalysisRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("storage: unmarshal analysis: %w", err)
	}
	return &rec, nil
}

func (p *postgresProvider) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(
			`SELECT data FROM vs_analyses
			 WHERE incident_id = $1
			 ORDER BY requested_at DESC
			 LIMIT $2`,
			incidentID, limit,
		)
	} else {
		rows, err = p.db.Query(
			`SELECT data FROM vs_analyses
			 WHERE incident_id = $1
			 ORDER BY requested_at DESC`,
			incidentID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list analyses by incident: %w", err)
	}
	defer rows.Close()
	return scanAnalysisRows(rows)
}

func (p *postgresProvider) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(
			`SELECT data FROM vs_analyses ORDER BY requested_at DESC LIMIT $1`, limit,
		)
	} else {
		rows, err = p.db.Query(
			`SELECT data FROM vs_analyses ORDER BY requested_at DESC`,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list analyses: %w", err)
	}
	defer rows.Close()
	return scanAnalysisRows(rows)
}

func scanAnalysisRows(rows *sql.Rows) ([]*AnalysisRecord, error) {
	var out []*AnalysisRecord
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("storage: scan analysis row: %w", err)
		}
		var rec AnalysisRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("storage: unmarshal analysis row: %w", err)
		}
		out = append(out, &rec)
	}
	return out, rows.Err()
}

func (p *postgresProvider) DeleteAnalysis(id string) error {
	res, err := p.db.Exec(`DELETE FROM vs_analyses WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("storage: delete analysis: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Search (implements the optional storage.Searcher capability)
// ---------------------------------------------------------------------------

// SearchIncidents matches the query (case-insensitive) against the
// title/service/source fields and, as a fallback, the whole JSON body.
// An empty query degrades to ListIncidents. Results are newest first.
func (p *postgresProvider) SearchIncidents(query string, limit int) ([]*IncidentRecord, error) {
	if query == "" {
		return p.ListIncidents(limit)
	}
	pattern := "%" + query + "%"
	base := `
		SELECT data FROM vs_incidents
		WHERE data->>'title'   ILIKE $1
		   OR data->>'service' ILIKE $1
		   OR data->>'source'  ILIKE $1
		   OR data::text       ILIKE $1
		ORDER BY created_at DESC`
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(base+` LIMIT $2`, pattern, limit)
	} else {
		rows, err = p.db.Query(base, pattern)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: search incidents: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
}

// SearchAnalyses matches the query (case-insensitive) against the whole
// analysis JSON body, newest first.
func (p *postgresProvider) SearchAnalyses(query string, limit int) ([]*AnalysisRecord, error) {
	if query == "" {
		return p.ListAnalyses(limit)
	}
	pattern := "%" + query + "%"
	base := `
		SELECT data FROM vs_analyses
		WHERE data::text ILIKE $1
		ORDER BY requested_at DESC`
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(base+` LIMIT $2`, pattern, limit)
	} else {
		rows, err = p.db.Query(base, pattern)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: search analyses: %w", err)
	}
	defer rows.Close()
	return scanAnalysisRows(rows)
}

func (p *postgresProvider) Close() error {
	return p.db.Close()
}

// ---------------------------------------------------------------------------
// Lifecycle (implements the optional storage.Lifecycle capability — X1-T7)
//
// A mechanical delete primitive with no org/policy concept. The blob
// domain spans every physical blob table (the fixed allowlist + vs_blobs),
// so model-state artifacts under the models/ namespace (which fall back to
// vs_blobs) purge here too.
// ---------------------------------------------------------------------------

// allBlobTables returns the fixed set of physical blob tables: the shared
// vs_blobs plus every dedicated table in the blobTables allowlist. The
// result is a constant set (never derived from caller input), so each name
// is safe to interpolate into a query.
func allBlobTables() []string {
	tables := []string{"vs_blobs"}
	seen := map[string]bool{"vs_blobs": true}
	for _, t := range blobTables {
		if !seen[t] {
			tables = append(tables, t)
			seen[t] = true
		}
	}
	return tables
}

func (p *postgresProvider) PurgeOlderThan(domain string, cutoff time.Time) (int, error) {
	cut := cutoff.UTC()
	switch domain {
	case DomainIncidents:
		res, err := p.db.Exec(`DELETE FROM vs_incidents WHERE created_at < $1`, cut)
		if err != nil {
			return 0, fmt.Errorf("storage: purge incidents: %w", err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	case DomainAnalyses:
		res, err := p.db.Exec(`DELETE FROM vs_analyses WHERE requested_at < $1`, cut)
		if err != nil {
			return 0, fmt.Errorf("storage: purge analyses: %w", err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	case DomainBlobs:
		total := 0
		for _, table := range allBlobTables() {
			res, err := p.db.Exec(
				fmt.Sprintf(`DELETE FROM %s WHERE updated_at < $1`, table), cut,
			)
			if err != nil {
				return total, fmt.Errorf("storage: purge blobs %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			total += int(n)
		}
		return total, nil
	default:
		return 0, ErrUnknownDomain
	}
}

func (p *postgresProvider) DeleteByID(domain, id string) error {
	var (
		res sql.Result
		err error
	)
	switch domain {
	case DomainIncidents:
		res, err = p.db.Exec(`DELETE FROM vs_incidents WHERE id = $1`, id)
	case DomainAnalyses:
		res, err = p.db.Exec(`DELETE FROM vs_analyses WHERE id = $1`, id)
	case DomainBlobs:
		// The table is chosen from the fixed allowlist by name, so it is
		// safe to interpolate; the name itself is bound as a parameter.
		res, err = p.db.Exec(
			fmt.Sprintf(`DELETE FROM %s WHERE name = $1`, blobTable(id)), id,
		)
	default:
		return ErrUnknownDomain
	}
	if err != nil {
		return fmt.Errorf("storage: delete %s %q: %w", domain, id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
