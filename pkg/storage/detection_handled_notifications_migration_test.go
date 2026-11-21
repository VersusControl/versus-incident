package storage_test

import (
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const handledNotificationsMigration = "040_detection_handled_notifications.sql"

// TestPostgresDetectionHandledNotificationsMigrationFreshAndUpgrade applies
// migration 040 on a fresh database and on a database built by the shipped
// migrations up to 031 that already holds detection episodes. The upgraded
// rows keep their data and read handled_notifications = 0; the real store then
// numbers an escalation of a handled episode 2 and a replay of a claim the old
// release never finished 1, and counts them when they complete.
func TestPostgresDetectionHandledNotificationsMigrationFreshAndUpgrade(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}

	t.Run("fresh", func(t *testing.T) {
		dropAllVersusTables(t, dsn)
		fresh, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatalf("NewPostgres(fresh): %v", err)
		}
		defer fresh.Close()
		assertHandledNotificationsColumn(t, fresh.(storage.SQLAccessor).DB(), true)
	})

	t.Run("upgrade_from_031", func(t *testing.T) {
		dropAllVersusTables(t, dsn)
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer db.Close()

		upTo031 := fstest.MapFS{}
		entries, err := fs.ReadDir(os.DirFS("migrations"), ".")
		if err != nil {
			t.Fatalf("read migrations: %v", err)
		}
		for _, e := range entries {
			if e.IsDir() || e.Name() > "031_~" {
				continue
			}
			data, err := os.ReadFile("migrations/" + e.Name())
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			upTo031["migrations/"+e.Name()] = &fstest.MapFile{Data: data}
		}
		if _, ok := upTo031["migrations/031_kubernetes_evidence_issue_indexes.sql"]; !ok {
			t.Fatal("migration 031 not found in migrations/")
		}
		if err := storage.WithMigrationLock(db, func() error {
			return storage.RunSQLMigrations(db, upTo031, "migrations", "versus_schema_migrations")
		}); err != nil {
			t.Fatalf("migrate up to 031: %v", err)
		}
		assertHandledNotificationsColumn(t, db, false)

		// Two episodes written before 040: one whose first notification was
		// handled, and one whose initial claim the old release never finished.
		identity := func(service string) storage.DetectionIdentity {
			return storage.DetectionIdentity{AgentKind: "kubernetes", Source: "kubernetes:alpha", Service: service, SignalKind: "kubernetes_issue", ConditionKey: "pod.crash_loop"}
		}
		insert := func(episodeID, incidentID, service, extraCols, extraVals string) {
			t.Helper()
			fingerprint, err := storage.DetectionIdentityFingerprint(identity(service))
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO vs_detection_episodes (
				episode_id, org_id, identity_version, agent_kind, source, service, signal_kind,
				condition_key, fingerprint, incident_id, occurrence_count, first_seen, last_seen,
				highest_observed_severity`+extraCols+`)
				VALUES ($1, 'default', 'v1', 'kubernetes', 'kubernetes:alpha', $2, 'kubernetes_issue',
				'pod.crash_loop', $3, $4, 3, NOW() - INTERVAL '1 hour', NOW() - INTERVAL '1 minute',
				'medium'`+extraVals+`)`, episodeID, service, fingerprint, incidentID); err != nil {
				t.Fatalf("insert pre-040 episode %s: %v", episodeID, err)
			}
		}
		insert("ep-handled-040", "inc-handled-040", "shop/deployment/handled",
			", highest_handled_severity, highest_notified_severity, last_completed_token, last_notification_outcome",
			", 'medium', 'medium', 'old-token', 'sent'")
		insert("ep-pending-040", "inc-pending-040", "shop/deployment/pending",
			", pending_kind, pending_severity, pending_token, pending_owner, pending_expires_at",
			", 'initial', 'medium', 'dead-token', 'writer-a', NOW() - INTERVAL '1 second'")

		upgraded, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatalf("NewPostgres(upgrade): %v", err)
		}
		defer upgraded.Close()
		udb := upgraded.(storage.SQLAccessor).DB()
		assertHandledNotificationsColumn(t, udb, true)

		handledCount := func(episodeID string) int64 {
			t.Helper()
			var handled, occurrences int64
			if err := udb.QueryRow(`SELECT handled_notifications, occurrence_count FROM vs_detection_episodes WHERE episode_id = $1`, episodeID).Scan(&handled, &occurrences); err != nil {
				t.Fatalf("read upgraded episode %s: %v", episodeID, err)
			}
			if occurrences < 3 {
				t.Fatalf("upgraded episode %s lost occurrences: %d", episodeID, occurrences)
			}
			return handled
		}
		if got := handledCount("ep-handled-040"); got != 0 {
			t.Fatalf("upgraded handled episode handled_notifications=%d, want 0 (column default)", got)
		}

		// The real store on the upgraded rows: an escalation of the handled
		// episode is notification 2, the replay of the unfinished claim keeps 1.
		store := upgraded.(storage.DetectionEpisodeStore)
		for _, id := range []string{"inc-handled-040", "inc-pending-040"} {
			if err := upgraded.SaveIncident(&storage.IncidentRecord{ID: id, CreatedAt: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatalf("SaveIncident(%s): %v", id, err)
			}
		}
		record := func(service, severity string) storage.DetectionEpisodeDecision {
			t.Helper()
			decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity(service), Frequency: 1, Severity: severity, ReceivedAt: time.Now(), ClaimOwner: "writer-b", ClaimLease: time.Minute,
			})
			if err != nil {
				t.Fatalf("RecordDetectionOccurrence(%s): %v", service, err)
			}
			return decision
		}
		complete := func(decision storage.DetectionEpisodeDecision) {
			t.Helper()
			if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{EpisodeID: decision.EpisodeID, ClaimToken: decision.ClaimToken, Outcome: "sent", CompletedAt: time.Now()}); err != nil {
				t.Fatalf("CompleteDetectionNotification(%s): %v", decision.EpisodeID, err)
			}
		}
		escalation := record("shop/deployment/handled", "critical")
		if !escalation.NotificationClaimed || escalation.EpisodeID != "ep-handled-040" || escalation.DeliveryID() != "ep-handled-040-2" {
			t.Fatalf("escalation of a pre-040 handled episode = %q (episode %q, claimed %t), want ep-handled-040-2", escalation.DeliveryID(), escalation.EpisodeID, escalation.NotificationClaimed)
		}
		complete(escalation)
		if got := handledCount("ep-handled-040"); got != 2 {
			t.Fatalf("handled_notifications after the escalation = %d, want 2", got)
		}
		replay := record("shop/deployment/pending", "medium")
		if !replay.NotificationClaimed || replay.EpisodeID != "ep-pending-040" || replay.DeliveryID() != "ep-pending-040-1" {
			t.Fatalf("replay of a pre-040 unfinished claim = %q (episode %q, claimed %t), want ep-pending-040-1", replay.DeliveryID(), replay.EpisodeID, replay.NotificationClaimed)
		}
		complete(replay)
		if got := handledCount("ep-pending-040"); got != 1 {
			t.Fatalf("handled_notifications after the replay = %d, want 1", got)
		}
	})
}

func assertHandledNotificationsColumn(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var exists bool
	var nullable, def sql.NullString
	err := db.QueryRow(`SELECT true, is_nullable, column_default FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'vs_detection_episodes'
		  AND column_name = 'handled_notifications'`).Scan(&exists, &nullable, &def)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("inspect handled_notifications column: %v", err)
	}
	var ledgered bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM versus_schema_migrations WHERE filename = $1)`,
		handledNotificationsMigration).Scan(&ledgered); err != nil {
		t.Fatalf("inspect migration ledger: %v", err)
	}
	if exists != want || ledgered != want {
		t.Fatalf("handled_notifications exists=%v ledgered=%v, want %v", exists, ledgered, want)
	}
	if want && (nullable.String != "NO" || def.String != "0") {
		t.Fatalf("handled_notifications nullable=%q default=%q, want NO and 0", nullable.String, def.String)
	}
}
