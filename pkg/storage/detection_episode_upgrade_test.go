package storage_test

import (
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestPostgresDetectionEpisodeUpgradePreservesLegacyIncident(t *testing.T) {
	// This test needs an externally prepared pre-episode schema and seeded
	// legacy incident. The normal TEST_POSTGRES_DSN database is auto-migrated on
	// first open, so sharing it would erase the upgrade boundary being tested.
	dsn := os.Getenv("TEST_POSTGRES_UPGRADE_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_UPGRADE_DSN not set; skipping pre-episode upgrade test")
	}

	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres(upgrade): %v", err)
	}
	assertDetectionEpisodeUpgradeState(t, provider)

	decision, err := provider.(storage.DetectionEpisodeStore).RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "upgrade-probe", Service: "checkout",
			SignalKind: "logs", ConditionKey: "post-upgrade-condition",
		},
		Frequency: 3, Severity: "medium", ReceivedAt: time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(after upgrade): %v", err)
	}
	if decision.OccurrenceCount != 3 || decision.EpisodeID == "" || decision.IncidentID == "" {
		t.Fatalf("post-upgrade episode decision = %+v", decision)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close(first boot): %v", err)
	}

	restarted, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres(restart): %v", err)
	}
	defer restarted.Close()
	assertDetectionEpisodeUpgradeState(t, restarted)

	continued, err := restarted.(storage.DetectionEpisodeStore).RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "upgrade-probe", Service: "checkout",
			SignalKind: "logs", ConditionKey: "post-upgrade-condition",
		},
		Frequency: 2, Severity: "medium", ReceivedAt: time.Date(2026, 9, 3, 13, 0, 1, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(after restart): %v", err)
	}
	if continued.EpisodeID != decision.EpisodeID || continued.IncidentID != decision.IncidentID || continued.OccurrenceCount != 5 {
		t.Fatalf("restart episode decision = %+v, initial = %+v", continued, decision)
	}
	t.Logf("legacy_id=legacy-before-episodes episode_id=%s incident_id=%s occurrence_count=%d", continued.EpisodeID, continued.IncidentID, continued.OccurrenceCount)
}

func assertDetectionEpisodeUpgradeState(t *testing.T, provider storage.Provider) {
	t.Helper()
	legacy, err := provider.GetIncident("legacy-before-episodes")
	if err != nil {
		t.Fatalf("GetIncident(legacy-before-episodes): %v", err)
	}
	if legacy.Title != "Legacy checkout incident" || legacy.Service != "checkout" || legacy.Content["marker"] != "pre-episode" {
		t.Fatalf("legacy incident changed across upgrade: %+v", legacy)
	}
	if legacy.DetectionEpisodeID != "" || legacy.OccurrenceCount != 0 || legacy.DetectionFirstSeen != nil || legacy.DetectionLastSeen != nil {
		t.Fatalf("legacy incident received fabricated detection state: %+v", legacy)
	}

	db := provider.(storage.SQLAccessor).DB()
	var migrationCount, totalMigrations int
	if err := db.QueryRow(`SELECT count(*) FROM versus_schema_migrations WHERE filename IN ('013_detection_episodes.sql', '014_incident_detection_columns.sql', '015_detection_notification_outcome.sql')`).Scan(&migrationCount); err != nil {
		t.Fatalf("query episode migration ledger: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM versus_schema_migrations`).Scan(&totalMigrations); err != nil {
		t.Fatalf("query total migration ledger: %v", err)
	}
	if migrationCount != 3 || totalMigrations != 15 {
		t.Fatalf("migration ledger episode=%d total=%d, want 3/15", migrationCount, totalMigrations)
	}

	var episodeTable string
	if err := db.QueryRow(`SELECT to_regclass('vs_detection_episodes')::text`).Scan(&episodeTable); err != nil {
		t.Fatalf("query episode table: %v", err)
	}
	if episodeTable != "vs_detection_episodes" {
		t.Fatalf("episode table = %q", episodeTable)
	}
	var legacyJSON bool
	if err := db.QueryRow(`SELECT data->>'legacy' = 'true' FROM vs_incidents WHERE id = 'legacy-before-episodes'`).Scan(&legacyJSON); err != nil {
		t.Fatalf("query legacy JSON: %v", err)
	}
	if !legacyJSON {
		t.Fatal("legacy data JSON changed across upgrade")
	}
}
