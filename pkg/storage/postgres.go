package storage

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
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
	DSN          string // postgres connection string or DSN URL
	MaxIncidents int    // rolling cap; default MaxIncidentsDefault
}

type postgresProvider struct {
	db           *sql.DB
	maxIncidents int
}

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
	max := opts.MaxIncidents
	if max <= 0 {
		max = MaxIncidentsDefault
	}
	p := &postgresProvider{db: db, maxIncidents: max}
	if err := p.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: postgres migrate: %w", err)
	}
	return p, nil
}

// migrate applies every migrations/*.sql file in lexical order. Each
// file is idempotent (IF NOT EXISTS), so re-running on an existing
// database is a no-op.
func (p *postgresProvider) migrate() error {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := p.db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
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
var blobTables = map[string]string{
	"patterns": "vs_patterns",
	"shadow":   "vs_shadow",
	"detect":   "vs_detect",
	"members":  "vs_members",
	"teams":    "vs_teams",
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
	// Trim to maxIncidents: drop the oldest records beyond the cap.
	_, err = p.db.Exec(`
		DELETE FROM vs_incidents
		WHERE id IN (
			SELECT id FROM vs_incidents
			ORDER BY created_at ASC
			OFFSET $1
		)
	`, p.maxIncidents)
	return err
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
