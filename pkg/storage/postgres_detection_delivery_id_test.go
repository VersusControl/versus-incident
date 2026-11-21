package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// An episode row written by a release that kept no handled-notification
// count reads handled_notifications = 0 (the column default) while its
// highest_handled_severity says one notification already came to an end.
// On Postgres a replay or escalation after the upgrade must still carry a
// non-empty delivery ID that does not reuse the first notification's number,
// and completing it must count the notification the old release handled.
// The database is an in-code fake that serves the stored row; no DSN.
func TestPostgresDetectionDeliveryIDForAnEpisodeRecordedBeforeTheCount(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	identity := DetectionIdentity{AgentKind: "kubernetes", Source: "kubernetes:alpha", Service: "shop/Deployment/api", SignalKind: "kubernetes_issue", ConditionKey: "pod.crash_loop"}
	stored := func(mutate func(*fakeDetectionRow)) *fakeDetectionRow {
		row := &fakeDetectionRow{
			episodeID: "episode-pre", incidentID: "incident-pre", occurrences: 3,
			firstSeen: now.Add(-5 * time.Minute), lastSeen: now.Add(-time.Minute),
			observed: "medium", handled: "medium", notified: "medium",
			completed: "old-token", outcome: "sent",
		}
		if mutate != nil {
			mutate(row)
		}
		return row
	}
	record := func(t *testing.T, row *fakeDetectionRow, severity string) DetectionEpisodeDecision {
		t.Helper()
		provider := &postgresProvider{db: openFakeDetectionDB(t, row)}
		decision, err := provider.RecordDetectionOccurrenceForOrg(DefaultOrgID, DetectionOccurrence{
			Identity: identity, Frequency: 1, Severity: severity, ReceivedAt: now, ClaimOwner: "writer-b", ClaimLease: time.Minute,
		})
		if err != nil {
			t.Fatalf("RecordDetectionOccurrence: %v", err)
		}
		return decision
	}

	t.Run("escalation of a handled episode", func(t *testing.T) {
		decision := record(t, stored(nil), "critical")
		if !decision.NotificationClaimed || decision.NotificationKind != DetectionNotificationEscalation || decision.DeliveryID() != "episode-pre-2" {
			t.Fatalf("escalation after the upgrade = %q (%s, claimed %t), want episode-pre-2", decision.DeliveryID(), decision.NotificationKind, decision.NotificationClaimed)
		}
	})

	t.Run("replay of an initial claim the old release never finished", func(t *testing.T) {
		row := stored(func(row *fakeDetectionRow) {
			row.handled, row.notified, row.completed, row.outcome = "", "", "", ""
			row.pendingKind, row.pendingSeverity, row.pendingToken, row.pendingOwner = string(DetectionNotificationInitial), "medium", "dead-token", "writer-a"
			row.pendingExpires = now.Add(-time.Second)
		})
		decision := record(t, row, "medium")
		if !decision.NotificationClaimed || decision.ClaimToken == "dead-token" || decision.DeliveryID() != "episode-pre-1" {
			t.Fatalf("replay after the upgrade = %q (claimed %t), want episode-pre-1", decision.DeliveryID(), decision.NotificationClaimed)
		}
	})

	t.Run("replay of an escalation claim the old release never finished", func(t *testing.T) {
		row := stored(func(row *fakeDetectionRow) {
			row.observed = "critical"
			row.pendingKind, row.pendingSeverity, row.pendingToken, row.pendingOwner = string(DetectionNotificationEscalation), "critical", "dead-token", "writer-a"
			row.pendingExpires = now.Add(-time.Second)
		})
		decision := record(t, row, "critical")
		if !decision.NotificationClaimed || decision.NotificationKind != DetectionNotificationEscalation || decision.DeliveryID() != "episode-pre-2" {
			t.Fatalf("escalation replay after the upgrade = %q (%s), want episode-pre-2", decision.DeliveryID(), decision.NotificationKind)
		}
	})

	t.Run("completion counts the notification the old release handled", func(t *testing.T) {
		row := stored(func(row *fakeDetectionRow) {
			row.observed = "critical"
			row.pendingKind, row.pendingSeverity, row.pendingToken, row.pendingOwner = string(DetectionNotificationEscalation), "critical", "claim", "writer-b"
			row.pendingExpires = now.Add(time.Minute)
		})
		db := openFakeDetectionDB(t, row)
		provider := &postgresProvider{db: db}
		if err := provider.CompleteDetectionNotificationForOrg(DefaultOrgID, DetectionNotificationCompletion{EpisodeID: "episode-pre", ClaimToken: "claim", Outcome: "sent", CompletedAt: now}); err != nil {
			t.Fatalf("CompleteDetectionNotification: %v", err)
		}
		update, ok := row.exec("handled_notifications = $7")
		if !ok || len(update) < 7 {
			t.Fatalf("completion did not update the episode: %v", row.execs)
		}
		if handled, _ := update[6].(int64); handled != 2 {
			t.Fatalf("handled_notifications after completing the escalation = %v, want 2 (the old release's notification and this one)", update[6])
		}
	})
}

// fakeDetectionRow is one vs_detection_episodes row served by the fake
// database, with the statements the provider executed against it.
type fakeDetectionRow struct {
	episodeID, incidentID                                    string
	occurrences                                              int64
	firstSeen, lastSeen                                      time.Time
	observed, handled, notified                              string
	pendingKind, pendingSeverity, pendingToken, pendingOwner string
	pendingExpires                                           time.Time
	completed, outcome                                       string
	handledCount                                             int64

	mu    sync.Mutex
	execs []fakeDetectionExec
}

type fakeDetectionExec struct {
	query string
	args  []driver.Value
}

func (row *fakeDetectionRow) exec(fragment string) ([]driver.Value, bool) {
	row.mu.Lock()
	defer row.mu.Unlock()
	for _, exec := range row.execs {
		if strings.Contains(exec.query, fragment) {
			return exec.args, true
		}
	}
	return nil, false
}

func (row *fakeDetectionRow) values(fingerprint string) []driver.Value {
	text := func(value string) driver.Value {
		if value == "" {
			return nil
		}
		return value
	}
	var expires driver.Value
	if !row.pendingExpires.IsZero() {
		expires = row.pendingExpires
	}
	return []driver.Value{row.episodeID, fingerprint, row.incidentID, row.occurrences, row.firstSeen, row.lastSeen,
		row.observed, text(row.handled), text(row.notified), text(row.pendingKind), text(row.pendingSeverity),
		text(row.pendingToken), text(row.pendingOwner), expires, text(row.completed), text(row.outcome), row.handledCount}
}

func openFakeDetectionDB(t *testing.T, row *fakeDetectionRow) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeDetectionConnector{row: row})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeDetectionConnector struct{ row *fakeDetectionRow }

func (c fakeDetectionConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeDetectionConn{row: c.row}, nil
}

func (c fakeDetectionConnector) Driver() driver.Driver { return fakeDetectionDriver{c} }

type fakeDetectionDriver struct{ connector fakeDetectionConnector }

func (d fakeDetectionDriver) Open(string) (driver.Conn, error) {
	return d.connector.Connect(context.Background())
}

type fakeDetectionConn struct{ row *fakeDetectionRow }

func (c *fakeDetectionConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake detection database: prepared statements are not supported")
}
func (c *fakeDetectionConn) Close() error              { return nil }
func (c *fakeDetectionConn) Begin() (driver.Tx, error) { return fakeDetectionTx{}, nil }
func (c *fakeDetectionConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeDetectionTx{}, nil
}

func (c *fakeDetectionConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	c.row.mu.Lock()
	c.row.execs = append(c.row.execs, fakeDetectionExec{query: query, args: values})
	c.row.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func (c *fakeDetectionConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "FROM vs_detection_episodes") && strings.Contains(query, "AND fingerprint = $2 AND closed_at IS NULL"):
		fingerprint, _ := args[1].Value.(string)
		return &fakeDetectionRows{rows: [][]driver.Value{c.row.values(fingerprint)}}, nil
	case strings.Contains(query, "FROM vs_detection_episodes WHERE org_id = $1 AND episode_id = $2 FOR UPDATE"):
		return &fakeDetectionRows{rows: [][]driver.Value{c.row.values("fingerprint")}}, nil
	case strings.Contains(query, "SELECT resolved FROM vs_incidents"):
		return &fakeDetectionRows{rows: [][]driver.Value{{false}}}, nil
	}
	return &fakeDetectionRows{}, nil
}

type fakeDetectionTx struct{}

func (fakeDetectionTx) Commit() error   { return nil }
func (fakeDetectionTx) Rollback() error { return nil }

type fakeDetectionRows struct {
	rows [][]driver.Value
	next int
}

func (r *fakeDetectionRows) Columns() []string {
	if len(r.rows) == 0 {
		return nil
	}
	return make([]string, len(r.rows[0]))
}
func (r *fakeDetectionRows) Close() error { return nil }
func (r *fakeDetectionRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}
