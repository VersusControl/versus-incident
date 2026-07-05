package storage_test

// incident_cap_test.go — the incident-retention split between backends:
//
//   * the FILE backend keeps a rolling cap (MaxIncidents) because its whole
//     history lives in one JSON file that must stay small; the oldest record
//     is dropped on save once the cap is exceeded.
//   * the POSTGRES backend keeps history UNBOUNDED — a database has no reason
//     to drop rows on write, and retention is a deliberate policy applied via
//     storage.Lifecycle, not an implicit trim.
//
// Postgres is gated on TEST_POSTGRES_DSN (skipped when unset), matching the
// rest of the parity suite.

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestFileIncidentCapStillApplies proves the file backend keeps dropping the
// oldest record once MaxIncidents is exceeded.
func TestFileIncidentCapStillApplies(t *testing.T) {
	dir := t.TempDir()
	p, err := storage.NewFile(storage.FileOptions{DataDir: dir, MaxIncidents: 3})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		rec := &storage.IncidentRecord{
			ID:        string(rune('a' + i)),
			Title:     "incident",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
	}

	list, err := p.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("file backend kept %d incidents, want 3 (cap)", len(list))
	}
	// The two oldest ("a", "b") must have been dropped; the three newest survive.
	for _, id := range []string{"a", "b"} {
		if _, err := p.GetIncident(id); err == nil {
			t.Errorf("oldest incident %q should have been dropped by the cap", id)
		}
	}
	for _, id := range []string{"c", "d", "e"} {
		if _, err := p.GetIncident(id); err != nil {
			t.Errorf("recent incident %q should survive the cap: %v", id, err)
		}
	}
}

// TestPostgresIncidentUnbounded proves the Postgres backend never drops
// incidents on save — every record persists regardless of how many precede
// it (the old rolling cap is gone). Gated on a real Postgres.
func TestPostgresIncidentUnbounded(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset

	const n = 25
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := 0; i < n; i++ {
		rec := &storage.IncidentRecord{
			ID:        "unbounded-" + time.Duration(i).String(),
			Title:     "incident",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
	}

	list, err := p.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(list) != n {
		t.Fatalf("postgres kept %d incidents, want %d (no cap — nothing should be dropped)", len(list), n)
	}
}
