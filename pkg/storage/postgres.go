package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	// pgx stdlib driver — registers "pgx" with database/sql.
	"github.com/jackc/pgx/v5/pgconn"
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

// defaultConnectBudget bounds the total time NewPostgres will spend retrying
// the initial Postgres reachability check before giving up. It is overridable
// per deployment via the POSTGRES_CONNECT_TIMEOUT env var.
const defaultConnectBudget = 60 * time.Second

// perPingTimeout bounds a single reachability probe so a black-holed host
// (dropped packets, wrong host, firewall) can never block on the OS TCP
// connect timeout — the probe fails fast and the retry loop logs and backs off.
const perPingTimeout = 5 * time.Second

// connectBudget resolves the total connect budget from POSTGRES_CONNECT_TIMEOUT
// (a Go duration string such as "90s"), falling back to defaultConnectBudget
// when the var is unset, unparseable, or non-positive.
func connectBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv("POSTGRES_CONNECT_TIMEOUT"))
	if raw == "" {
		return defaultConnectBudget
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultConnectBudget
	}
	return d
}

// redactDSN returns a safe "host:port/dbname" summary of a Postgres DSN for
// logs and errors. It NEVER returns the password: URL-form userinfo is stripped
// of its password and keyword-form drops the password= token entirely. If the
// DSN cannot be understood it returns the constant "postgres" rather than
// risk leaking any raw connection string.
func redactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "postgres"
	}
	// URL form: postgres://user:pass@host:5432/db?...
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil || u.Host == "" {
			return "postgres"
		}
		hostPort := u.Host
		db := strings.TrimPrefix(u.Path, "/")
		summary := hostPort
		if db != "" {
			summary = hostPort + "/" + db
		}
		if u.User != nil {
			if name := u.User.Username(); name != "" {
				summary = name + "@" + summary
			}
		}
		return summary
	}
	// Keyword form: host=... port=... dbname=... password=...
	var host, port, dbname string
	for _, field := range strings.Fields(dsn) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.ToLower(kv[0]) {
		case "host":
			host = kv[1]
		case "port":
			port = kv[1]
		case "dbname":
			dbname = kv[1]
		}
	}
	if host == "" {
		return "postgres"
	}
	summary := host
	if port != "" {
		summary = host + ":" + port
	}
	if dbname != "" {
		summary = summary + "/" + dbname
	}
	return summary
}

// dsnDBName extracts just the database name from a Postgres DSN (URL form
// postgres://…/dbname or keyword form dbname=…), returning "" when it cannot
// be determined. It shares the URL/keyword shapes with redactDSN but yields
// only the dbname so pgSetupHint can name the real database in its guide
// without ever touching the password.
func dsnDBName(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(u.Path, "/")
	}
	for _, field := range strings.Fields(dsn) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) == 2 && strings.ToLower(kv[0]) == "dbname" {
			return kv[1]
		}
	}
	return ""
}

// pgSetupHint returns an actionable, operator-facing provisioning guide when
// err is a Postgres login/permission failure, or "" for anything else. It
// inspects for the SQLSTATEs an operator hits when the role can't log in
// (28P01/28000), the database is missing (3D000), or the role lacks the
// GRANTs needed to migrate (42501/3F000 — the PostgreSQL 15+ "permission
// denied for schema public" case). The guide is STATIC text plus dbName only,
// so it can never leak a password or the DSN. dbName falls back to
// "versus_enterprise" when the connection string didn't name a database.
func pgSetupHint(err error, dbName string) string {
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	switch pgErr.Code {
	case "28P01", "28000", // auth failed: invalid_password / invalid_authorization
		"3D000",          // database does not exist: invalid_catalog_name
		"42501", "3F000": // missing GRANTs: insufficient_privilege / invalid_schema_name
	default:
		return ""
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = "versus_enterprise"
	}
	return fmt.Sprintf(`postgres rejected the connection: the role/database may be missing or lack privileges.
Provision it once as a superuser (psql), substituting your own db / user / password:

  CREATE DATABASE %[1]s;
  CREATE USER versus WITH PASSWORD 'your_strong_password';
  GRANT ALL PRIVILEGES ON DATABASE %[1]s TO versus;
  \c %[1]s
  GRANT ALL ON SCHEMA public TO versus;   -- required on PostgreSQL 15+

On PostgreSQL 15+ the public schema no longer allows CREATE by default, so the
final grant (run while connected to %[1]s) is the usual fix for
"permission denied for schema public" during migration.`, dbName)
}

// NewPostgres opens a connection to Postgres, runs idempotent migrations,
// and returns a ready Provider. Callers must call Close when done.
//
// The initial reachability check is bounded, retried, and logged so an
// unreachable or slow-to-start database can never turn into a silent restart
// loop: each probe is capped at perPingTimeout, the loop retries with backoff
// until the connectBudget is exhausted, and every attempt logs to stderr
// (captured by `docker logs`) using a redacted host:port/dbname target that
// never contains the password.
func NewPostgres(opts PostgresOptions) (Provider, error) {
	if opts.DSN == "" {
		return nil, fmt.Errorf("storage: postgres DSN is required (set POSTGRES_DSN)")
	}
	db, err := sql.Open("pgx", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("storage: open postgres: %w", err)
	}

	redacted := redactDSN(opts.DSN)
	dbName := dsnDBName(opts.DSN)
	budget := connectBudget()
	deadline := time.Now().Add(budget)

	log.Printf("storage: connecting to postgres at %s", redacted)

	var lastErr error
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), perPingTimeout)
		lastErr = db.PingContext(ctx)
		cancel()
		if lastErr == nil {
			log.Printf("storage: connected to postgres at %s", redacted)
			break
		}
		// Backoff grows linearly from ~1s and is capped at ~5s, but never
		// past the remaining budget.
		backoff := time.Duration(attempt) * time.Second
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
		if time.Now().Add(backoff).After(deadline) {
			if hint := pgSetupHint(lastErr, dbName); hint != "" {
				log.Printf("%s", hint)
			}
			_ = db.Close()
			return nil, fmt.Errorf("storage: cannot reach postgres at %s after %s: %w", redacted, budget, lastErr)
		}
		log.Printf("storage: cannot reach postgres at %s (attempt %d): %v; retrying in %s", redacted, attempt, lastErr, backoff)
		time.Sleep(backoff)
	}

	p := &postgresProvider{db: db}
	if err := p.migrate(); err != nil {
		if hint := pgSetupHint(err, dbName); hint != "" {
			log.Printf("%s", hint)
		}
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
// on the shared, multi-writer Postgres backend — the HA substrate.
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
//
// Every IncidentRecord property is persisted in its own typed column on
// vs_incidents — the legacy `data` JSONB blob is neither read nor written by
// this backend. The write path is a full-column upsert; every read selects the
// explicit columns and scans them back into the record. String slices are
// stored in native TEXT[] columns and the arbitrary Content map in a JSONB
// column.
// ---------------------------------------------------------------------------

// incidentColumns is the SELECT list every incident read uses. The two array
// columns are wrapped in to_jsonb(...) so the pgx stdlib driver returns them as
// JSON bytes we unmarshal into []string (the driver otherwise surfaces a
// TEXT[] as its raw Postgres text form). Content is already JSONB. The order
// here MUST match the Scan order in scanIncidentRow.
const incidentColumns = `id, created_at, acked_at, org_id, team_id, title,
	source, service, origin, resolved,
	to_jsonb(channels_enabled)    AS channels_enabled,
	to_jsonb(channels_notified)   AS channels_notified,
	oncall_triggered, oncall_error, notify_status, notify_error,
	resolved_at, content, assigned_team_id,
	to_jsonb(assigned_member_ids) AS assigned_member_ids`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so scanIncidentRow
// serves the single-row GetIncident path and the multi-row list/search paths
// without duplication.
type rowScanner interface {
	Scan(dest ...any) error
}

// utcPtr returns a UTC copy of t, or nil when t is nil, so a nil *time.Time
// binds as SQL NULL and a set one binds as a normalized timestamptz.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// textArrayParam prepares a string slice for binding to a TEXT[] column. An
// empty or nil slice binds as SQL NULL (via an untyped nil) so it round-trips
// back to a nil slice; a non-empty slice is passed straight through and the
// pgx driver encodes it as a Postgres array.
func textArrayParam(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}

// marshalIncidentContent renders the Content map for the JSONB column. An
// empty or nil map binds as SQL NULL so it reads back as a nil map.
func marshalIncidentContent(m map[string]interface{}) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// jsonStringSlice decodes a to_jsonb(TEXT[]) column back into a string slice.
// A NULL column (raw is empty) yields a nil slice.
func jsonStringSlice(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// unmarshalIncidentContent decodes the JSONB content column. A NULL column or
// a JSON null yields a nil map.
func unmarshalIncidentContent(raw []byte) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *postgresProvider) SaveIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveIncident: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	content, err := marshalIncidentContent(rec.Content)
	if err != nil {
		return fmt.Errorf("storage: marshal incident content: %w", err)
	}
	// Full-column upsert: this is the one incident write path (create, resolve,
	// and ack all funnel through it), so every column is (re)written from the
	// record ON CONFLICT and the row never drifts. Origin is persisted as the
	// EffectiveOrigin so a legacy-derived origin is stamped explicitly.
	_, err = p.db.Exec(`
		INSERT INTO vs_incidents (
			id, created_at, acked_at, org_id, team_id, title, source, service,
			origin, resolved, channels_enabled, channels_notified,
			oncall_triggered, oncall_error, notify_status, notify_error,
			resolved_at, content, assigned_team_id, assigned_member_ids
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, $19, $20
		)
		ON CONFLICT (id) DO UPDATE SET
			created_at          = EXCLUDED.created_at,
			acked_at            = EXCLUDED.acked_at,
			org_id              = EXCLUDED.org_id,
			team_id             = EXCLUDED.team_id,
			title               = EXCLUDED.title,
			source              = EXCLUDED.source,
			service             = EXCLUDED.service,
			origin              = EXCLUDED.origin,
			resolved            = EXCLUDED.resolved,
			channels_enabled    = EXCLUDED.channels_enabled,
			channels_notified   = EXCLUDED.channels_notified,
			oncall_triggered    = EXCLUDED.oncall_triggered,
			oncall_error        = EXCLUDED.oncall_error,
			notify_status       = EXCLUDED.notify_status,
			notify_error        = EXCLUDED.notify_error,
			resolved_at         = EXCLUDED.resolved_at,
			content             = EXCLUDED.content,
			assigned_team_id    = EXCLUDED.assigned_team_id,
			assigned_member_ids = EXCLUDED.assigned_member_ids
	`,
		rec.ID, rec.CreatedAt.UTC(), utcPtr(rec.AckedAt), rec.OrgID, rec.TeamID,
		rec.Title, rec.Source, rec.Service, rec.EffectiveOrigin(), rec.Resolved,
		textArrayParam(rec.ChannelsEnabled), textArrayParam(rec.ChannelsNotified),
		rec.OnCallTriggered, rec.OnCallError, rec.NotifyStatus, rec.NotifyError,
		utcPtr(rec.ResolvedAt), content, rec.AssignedTeamID,
		textArrayParam(rec.AssignedMemberIDs),
	)
	if err != nil {
		return fmt.Errorf("storage: save incident: %w", err)
	}
	// The database keeps incident history unbounded; retention is a
	// deliberate policy applied through the storage.Lifecycle purge
	// primitive rather than an implicit drop on every save.
	return nil
}

func (p *postgresProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	t := ackedAt.UTC()
	res, err := p.db.Exec(
		`UPDATE vs_incidents SET acked_at = $2 WHERE id = $1`, id, t,
	)
	if err != nil {
		return fmt.Errorf("storage: ack update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *postgresProvider) GetIncident(id string) (*IncidentRecord, error) {
	row := p.db.QueryRow(
		`SELECT `+incidentColumns+` FROM vs_incidents WHERE id = $1`, id,
	)
	rec, err := scanIncidentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get incident: %w", err)
	}
	return rec, nil
}

func (p *postgresProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = p.db.Query(
			`SELECT `+incidentColumns+` FROM vs_incidents ORDER BY created_at DESC LIMIT $1`, limit,
		)
	} else {
		rows, err = p.db.Query(
			`SELECT ` + incidentColumns + ` FROM vs_incidents ORDER BY created_at DESC`,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list incidents: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
}

// scanIncidentRow reads one row selected via incidentColumns into an
// IncidentRecord. Nullable columns scan through sql.Null* / []byte holders so
// a NULL becomes the field's zero value (empty string, nil slice, nil map).
func scanIncidentRow(sc rowScanner) (*IncidentRecord, error) {
	var (
		rec         IncidentRecord
		ackedAt     sql.NullTime
		resolvedAt  sql.NullTime
		teamID      sql.NullString
		title       sql.NullString
		source      sql.NullString
		service     sql.NullString
		origin      sql.NullString
		oncallTrig  sql.NullBool
		oncallErr   sql.NullString
		notifyStat  sql.NullString
		notifyErr   sql.NullString
		assignTeam  sql.NullString
		chEnabled   []byte
		chNotified  []byte
		assignedIDs []byte
		content     []byte
	)
	if err := sc.Scan(
		&rec.ID, &rec.CreatedAt, &ackedAt, &rec.OrgID, &teamID, &title,
		&source, &service, &origin, &rec.Resolved,
		&chEnabled, &chNotified,
		&oncallTrig, &oncallErr, &notifyStat, &notifyErr,
		&resolvedAt, &content, &assignTeam, &assignedIDs,
	); err != nil {
		return nil, err
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	if ackedAt.Valid {
		t := ackedAt.Time.UTC()
		rec.AckedAt = &t
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time.UTC()
		rec.ResolvedAt = &t
	}
	rec.TeamID = teamID.String
	rec.Title = title.String
	rec.Source = source.String
	rec.Service = service.String
	rec.Origin = origin.String
	rec.OnCallTriggered = oncallTrig.Bool
	rec.OnCallError = oncallErr.String
	rec.NotifyStatus = notifyStat.String
	rec.NotifyError = notifyErr.String
	rec.AssignedTeamID = assignTeam.String

	var err error
	if rec.ChannelsEnabled, err = jsonStringSlice(chEnabled); err != nil {
		return nil, fmt.Errorf("decode channels_enabled: %w", err)
	}
	if rec.ChannelsNotified, err = jsonStringSlice(chNotified); err != nil {
		return nil, fmt.Errorf("decode channels_notified: %w", err)
	}
	if rec.AssignedMemberIDs, err = jsonStringSlice(assignedIDs); err != nil {
		return nil, fmt.Errorf("decode assigned_member_ids: %w", err)
	}
	if rec.Content, err = unmarshalIncidentContent(content); err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}
	return &rec, nil
}

func scanIncidentRows(rows *sql.Rows) ([]*IncidentRecord, error) {
	var out []*IncidentRecord
	for rows.Next() {
		rec, err := scanIncidentRow(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan incident row: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountIncidents implements the optional storage.IncidentPager capability:
// the per-origin tally and grand total of UNRESOLVED (open) incidents in one
// COUNT query, without shipping a single row to Go. Counts reflect open work,
// so resolved incidents are excluded (WHERE resolved = false); the badge shows
// the backlog, not the full history. The per-origin split reads the promoted
// origin column, and the unresolved predicate is served by the partial
// unresolved index so the count stays index-backed on a large history.
func (p *postgresProvider) CountIncidents() (IncidentCounts, error) {
	const q = `
		SELECT
			COUNT(*) FILTER (WHERE origin = 'ai_detect') AS ai,
			COUNT(*) FILTER (WHERE origin = 'webhook')   AS webhook,
			COUNT(*)                                      AS total
		FROM vs_incidents
		WHERE resolved = false`
	var c IncidentCounts
	if err := p.db.QueryRow(q).Scan(&c.AIDetect, &c.Webhook, &c.Total); err != nil {
		return IncidentCounts{}, fmt.Errorf("storage: count incidents: %w", err)
	}
	return c, nil
}

// ListIncidentsPage implements the optional storage.IncidentPager
// capability: one bounded, newest-first page pushed entirely into SQL
// (ORDER BY created_at DESC LIMIT/OFFSET). When origin is one of the known
// values the promoted origin column filters the page in SQL so a filtered
// page is bounded too — it never fetches the whole feed to filter in Go. The
// page lists ALL incidents (resolved and open alike) so resolved incidents
// remain reachable; only the counts are unresolved-only.
func (p *postgresProvider) ListIncidentsPage(origin string, offset, limit int) ([]*IncidentRecord, error) {
	if limit <= 0 {
		limit = DefaultIncidentPageSize
	}
	if offset < 0 {
		offset = 0
	}
	var (
		rows *sql.Rows
		err  error
	)
	if origin == OriginAIDetect || origin == OriginWebhook {
		rows, err = p.db.Query(`
			SELECT `+incidentColumns+` FROM vs_incidents
			WHERE origin = $1
			ORDER BY created_at DESC
			LIMIT $2 OFFSET $3`, origin, limit, offset)
	} else {
		rows, err = p.db.Query(
			`SELECT `+incidentColumns+` FROM vs_incidents ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
			limit, offset,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list incidents page: %w", err)
	}
	defer rows.Close()
	return scanIncidentRows(rows)
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

// CountAnalyses implements the optional storage.AnalysisPager capability: the
// total number of stored analyses in one COUNT query, without shipping a row
// to Go so a large vs_analyses never has to be loaded to render a count.
func (p *postgresProvider) CountAnalyses() (int, error) {
	var n int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM vs_analyses`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count analyses: %w", err)
	}
	return n, nil
}

// ListAnalysesPage implements the optional storage.AnalysisPager capability:
// one bounded, newest-first page pushed entirely into SQL (ORDER BY
// requested_at DESC LIMIT/OFFSET), so a large vs_analyses is never fetched
// whole to render one page.
func (p *postgresProvider) ListAnalysesPage(offset, limit int) ([]*AnalysisRecord, error) {
	if limit <= 0 {
		limit = DefaultAnalysisPageSize
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := p.db.Query(
		`SELECT data FROM vs_analyses ORDER BY requested_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list analyses page: %w", err)
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
// title/service/source columns and, as a fallback, the content JSON body.
// An empty query degrades to ListIncidents. Results are newest first.
func (p *postgresProvider) SearchIncidents(query string, limit int) ([]*IncidentRecord, error) {
	if query == "" {
		return p.ListIncidents(limit)
	}
	pattern := "%" + query + "%"
	base := `
		SELECT ` + incidentColumns + ` FROM vs_incidents
		WHERE ` + searchIncidentsWhereSQL + `
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

// searchIncidentsWhereSQL is the shared ILIKE predicate for incident search:
// it matches the query against the title/service/source columns and, as a
// fallback, the content JSON body. The pattern binds as $1. Kept as one
// constant so the count and page queries search the exact same columns as
// SearchIncidents.
const searchIncidentsWhereSQL = `title      ILIKE $1
		   OR service    ILIKE $1
		   OR source     ILIKE $1
		   OR content::text ILIKE $1`

// CountIncidentsMatching implements the optional storage.IncidentSearchPager
// capability: the per-origin tally and total of UNRESOLVED (open) search
// matches in one COUNT query, without materializing rows. Counts are
// unresolved-only (WHERE resolved = false) to match CountIncidents, so the
// badge over a filtered feed still reflects open work. An empty query degrades
// to counting every unresolved incident, matching CountIncidents.
func (p *postgresProvider) CountIncidentsMatching(query string) (IncidentCounts, error) {
	if query == "" {
		return p.CountIncidents()
	}
	pattern := "%" + query + "%"
	q := fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE origin = 'ai_detect') AS ai,
			COUNT(*) FILTER (WHERE origin = 'webhook')   AS webhook,
			COUNT(*)                                      AS total
		FROM vs_incidents
		WHERE (%[1]s)
		  AND resolved = false`, searchIncidentsWhereSQL)
	var c IncidentCounts
	if err := p.db.QueryRow(q, pattern).Scan(&c.AIDetect, &c.Webhook, &c.Total); err != nil {
		return IncidentCounts{}, fmt.Errorf("storage: count matching incidents: %w", err)
	}
	return c, nil
}

// SearchIncidentsPage implements the optional storage.IncidentSearchPager
// capability: one bounded, newest-first page of search matches, with the
// query, the origin filter, the ordering, and the LIMIT/OFFSET all pushed
// into SQL so a broad query over a large history never loads the whole match
// set. The page lists ALL matching incidents (resolved and open alike); only
// the counts are unresolved-only. An empty query degrades to ListIncidentsPage.
func (p *postgresProvider) SearchIncidentsPage(query, origin string, offset, limit int) ([]*IncidentRecord, error) {
	if query == "" {
		return p.ListIncidentsPage(origin, offset, limit)
	}
	if limit <= 0 {
		limit = DefaultIncidentPageSize
	}
	if offset < 0 {
		offset = 0
	}
	pattern := "%" + query + "%"
	if origin == OriginAIDetect || origin == OriginWebhook {
		q := fmt.Sprintf(`
			SELECT %[2]s FROM vs_incidents
			WHERE (%[1]s)
			  AND origin = $2
			ORDER BY created_at DESC
			LIMIT $3 OFFSET $4`, searchIncidentsWhereSQL, incidentColumns)
		rows, err := p.db.Query(q, pattern, origin, limit, offset)
		if err != nil {
			return nil, fmt.Errorf("storage: search incidents page: %w", err)
		}
		defer rows.Close()
		return scanIncidentRows(rows)
	}
	q := fmt.Sprintf(`
		SELECT %[2]s FROM vs_incidents
		WHERE %[1]s
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, searchIncidentsWhereSQL, incidentColumns)
	rows, err := p.db.Query(q, pattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storage: search incidents page: %w", err)
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
// Lifecycle (implements the optional storage.Lifecycle capability)
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
