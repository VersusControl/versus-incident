package storage_test

// lifecycle_test.go — covers the optional storage.Lifecycle capability:
// the mechanical, tier-neutral purge/delete primitive the
// enterprise retention engine consumes. Exercised against the memory
// backend; the Postgres backend is covered by the integration parity
// suite when a real database is configured.

import (
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestLifecycle_PurgeOlderThan_Incidents(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()

	lc, ok := p.(storage.Lifecycle)
	if !ok {
		t.Fatal("memory backend must implement storage.Lifecycle")
	}

	now := time.Now().UTC()
	old := &storage.IncidentRecord{ID: "old", CreatedAt: now.Add(-48 * time.Hour)}
	fresh := &storage.IncidentRecord{ID: "fresh", CreatedAt: now.Add(-1 * time.Hour)}
	for _, rec := range []*storage.IncidentRecord{old, fresh} {
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}

	n, err := lc.PurgeOlderThan(storage.DomainIncidents, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged = %d, want 1", n)
	}
	if _, err := p.GetIncident("old"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old incident should be gone, got err=%v", err)
	}
	if _, err := p.GetIncident("fresh"); err != nil {
		t.Fatalf("fresh incident should survive: %v", err)
	}
}

func TestLifecycle_PurgeOlderThan_Analyses(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()
	lc := p.(storage.Lifecycle)

	now := time.Now().UTC()
	if err := p.SaveAnalysis(&storage.AnalysisRecord{ID: "a-old", IncidentID: "i", Status: "ok", RequestedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	if err := p.SaveAnalysis(&storage.AnalysisRecord{ID: "a-new", IncidentID: "i", Status: "ok", RequestedAt: now}); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}

	n, err := lc.PurgeOlderThan(storage.DomainAnalyses, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged = %d, want 1", n)
	}
	if _, err := p.GetAnalysis("a-old"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a-old should be gone, got %v", err)
	}
	if _, err := p.GetAnalysis("a-new"); err != nil {
		t.Fatalf("a-new should survive: %v", err)
	}
}

func TestLifecycle_PurgeOlderThan_Blobs(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()
	lc := p.(storage.Lifecycle)

	if err := p.WriteBlob("k1", []byte("v1")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	// cutoff in the future ⇒ every blob is "older than" it and purges.
	n, err := lc.PurgeOlderThan(storage.DomainBlobs, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged = %d, want 1", n)
	}
	got, _ := p.ReadBlob("k1")
	if got != nil {
		t.Fatalf("blob should be purged, got %q", got)
	}
}

func TestLifecycle_DeleteByID(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()
	lc := p.(storage.Lifecycle)

	if err := p.SaveIncident(&storage.IncidentRecord{ID: "x", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	if err := lc.DeleteByID(storage.DomainIncidents, "x"); err != nil {
		t.Fatalf("DeleteByID: %v", err)
	}
	if err := lc.DeleteByID(storage.DomainIncidents, "x"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("second delete should be ErrNotFound, got %v", err)
	}
}

func TestLifecycle_UnknownDomain(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()
	lc := p.(storage.Lifecycle)

	if _, err := lc.PurgeOlderThan("nope", time.Now()); !errors.Is(err, storage.ErrUnknownDomain) {
		t.Fatalf("PurgeOlderThan unknown domain = %v, want ErrUnknownDomain", err)
	}
	if err := lc.DeleteByID("nope", "id"); !errors.Is(err, storage.ErrUnknownDomain) {
		t.Fatalf("DeleteByID unknown domain = %v, want ErrUnknownDomain", err)
	}
}
