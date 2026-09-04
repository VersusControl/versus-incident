package storage_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresDetectionNotificationOutcomeMigrationFreshAndUpgrade(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	dropAllVersusTables(t, dsn)

	fresh, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres(fresh): %v", err)
	}
	assertDetectionNotificationOutcomeSchema(t, fresh.(storage.SQLAccessor).DB(), 15)
	if err := fresh.Close(); err != nil {
		t.Fatalf("Close(fresh): %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE vs_detection_episodes DROP COLUMN last_notification_outcome`); err != nil {
		t.Fatalf("drop outcome column: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM versus_schema_migrations WHERE filename = '015_detection_notification_outcome.sql'`); err != nil {
		t.Fatalf("rewind migration ledger: %v", err)
	}
	assertDetectionNotificationOutcomeSchema(t, db, 14)

	upgraded, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres(upgrade): %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	assertDetectionNotificationOutcomeSchema(t, upgraded.(storage.SQLAccessor).DB(), 15)
}

func assertDetectionNotificationOutcomeSchema(t *testing.T, db *sql.DB, wantMigrations int) {
	t.Helper()
	var columnExists bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'vs_detection_episodes'
		  AND column_name = 'last_notification_outcome'
	)`).Scan(&columnExists); err != nil {
		t.Fatalf("inspect outcome column: %v", err)
	}
	var migrationCount int
	if err := db.QueryRow(`SELECT count(*) FROM versus_schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("inspect migration ledger: %v", err)
	}
	if columnExists != (wantMigrations == 15) || migrationCount != wantMigrations {
		t.Fatalf("outcome column exists=%v migrations=%d, want exists=%v migrations=%d",
			columnExists, migrationCount, wantMigrations == 15, wantMigrations)
	}
}
