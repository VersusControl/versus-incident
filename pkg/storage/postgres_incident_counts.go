package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// incidentCountGrace is how long the caller keeps waiting after the
// database's own limit has passed. The database normally ends the count on
// statement_timeout and answers with a clean error on a reusable connection;
// the caller's deadline is only the backstop for a database that stops
// answering altogether.
const incidentCountGrace = time.Second

var _ ScopedContextIncidentWindowCounter = (*postgresProvider)(nil)

// countTimeout is the database limit for one incident count.
func (p *postgresProvider) countTimeout() time.Duration {
	if p.incidentCountTimeout > 0 {
		return p.incidentCountTimeout
	}
	return IncidentCountTimeout
}

// listTimeout is the database limit for one incident list or search page.
func (p *postgresProvider) listTimeout() time.Duration {
	if p.incidentListTimeout > 0 {
		return p.incidentListTimeout
	}
	return IncidentListTimeout
}

// withCountDeadline runs one incident count under the count limit; a count
// that ran out of time wraps ErrCountTimeout.
func (p *postgresProvider) withCountDeadline(parent context.Context, what string, run func(ctx context.Context, tx *sql.Tx) error) error {
	return p.withReadDeadline(parent, what, p.countTimeout(), ErrCountTimeout, run)
}

// withListDeadline runs one incident page read under the list limit; a read
// that ran out of time wraps ErrListTimeout.
func (p *postgresProvider) withListDeadline(parent context.Context, what string, run func(ctx context.Context, tx *sql.Tx) error) error {
	return p.withReadDeadline(parent, what, p.listTimeout(), ErrListTimeout, run)
}

// withReadDeadline runs one incident read in a short read-only transaction
// whose statement_timeout ends it inside Postgres. Cancelling only the Go
// context would close the connection and leave the server still reading, so
// the database limit is what stops a read stuck behind a slow or locked
// incident table from holding a pool connection past its limit. what names
// the read in the returned error; a read that ran out of time wraps timedOut.
func (p *postgresProvider) withReadDeadline(parent context.Context, what string, limit time.Duration, timedOut error, run func(ctx context.Context, tx *sql.Tx) error) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, limit+incidentCountGrace)
	defer cancel()
	err := func() error {
		tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout='%dms'`, limit.Milliseconds())); err != nil {
			return err
		}
		if err := run(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err == nil {
		return nil
	}
	if statementTimedOut(err) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("storage: %s: %w after %s: %w", what, timedOut, limit, err)
	}
	return fmt.Errorf("storage: %s: %w", what, err)
}

// CountIncidentsByStatusSinceContext implements ContextIncidentWindowCounter.
func (p *postgresProvider) CountIncidentsByStatusSinceContext(ctx context.Context, since time.Time) (IncidentStatusCounts, error) {
	return p.CountIncidentsByStatusSinceForScopeContext(ctx, tenancy.DefaultOrgScope(), since)
}

// CountIncidentsByStatusSinceForScopeContext counts each status across the
// read scope for incidents created at or after since; a zero since counts
// the whole history. It stops when ctx ends and never runs in the database
// past the count limit.
func (p *postgresProvider) CountIncidentsByStatusSinceForScopeContext(ctx context.Context, scope tenancy.OrgScope, since time.Time) (IncidentStatusCounts, error) {
	const columns = `
		SELECT
			COUNT(*) FILTER (WHERE resolved = false AND acked_at IS NULL)     AS open_total,
			COUNT(*) FILTER (WHERE resolved = false AND acked_at IS NOT NULL) AS acked_total,
			COUNT(*) FILTER (WHERE resolved = true)                           AS resolved_total,
			COUNT(*)                                                          AS all_total,
			COUNT(*) FILTER (WHERE origin = 'ai_detect' AND resolved = false AND acked_at IS NULL)     AS open_ai,
			COUNT(*) FILTER (WHERE origin = 'ai_detect' AND resolved = false AND acked_at IS NOT NULL) AS acked_ai,
			COUNT(*) FILTER (WHERE origin = 'ai_detect' AND resolved = true)                           AS resolved_ai,
			COUNT(*) FILTER (WHERE origin = 'ai_detect')                                               AS all_ai
		FROM vs_incidents
		WHERE org_id = ANY($1)`
	what := "count incidents by status since"
	q, args := columns+` AND created_at >= $2`, []any{scope.OrgIDs(), since.UTC()}
	if since.IsZero() {
		what = "count incidents by status"
		q, args = columns, []any{scope.OrgIDs()}
	}
	var total, ai StatusCounts
	err := p.withCountDeadline(ctx, what, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, q, args...).Scan(
			&total.Open, &total.Acked, &total.Resolved, &total.Total,
			&ai.Open, &ai.Acked, &ai.Resolved, &ai.Total,
		)
	})
	if err != nil {
		return IncidentStatusCounts{}, err
	}
	return AssembleStatusCounts(total, ai), nil
}

var (
	_ ScopedContextIncidentPager       = (*postgresProvider)(nil)
	_ ScopedContextIncidentSearchPager = (*postgresProvider)(nil)
	_ ContextIncidentPager             = (*postgresProvider)(nil)
	_ ContextIncidentSearchPager       = (*postgresProvider)(nil)
)
