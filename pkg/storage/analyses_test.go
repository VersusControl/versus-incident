package storage_test

import (
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// runAnalysisCRUD exercises Save/Get/List/Delete against any Provider
// implementation. Shared so file + memory backends stay in lockstep.
func runAnalysisCRUD(t *testing.T, p storage.Provider) {
	t.Helper()

	a1 := &storage.AnalysisRecord{
		ID:          "an-1",
		IncidentID:  "inc-A",
		Status:      "ok",
		Model:       "gpt-4o-mini",
		DurationMs:  100,
		RequestedAt: time.Now().UTC().Add(-time.Minute),
		Finding:     &core.AIFinding{Title: "x", Summary: "y", Severity: "low"},
	}
	a2 := &storage.AnalysisRecord{
		ID:          "an-2",
		IncidentID:  "inc-A",
		Status:      "error",
		Error:       "boom",
		RequestedAt: time.Now().UTC(),
	}
	a3 := &storage.AnalysisRecord{
		ID:          "an-3",
		IncidentID:  "inc-B",
		Status:      "ok",
		RequestedAt: time.Now().UTC(),
	}

	for _, a := range []*storage.AnalysisRecord{a1, a2, a3} {
		if err := p.SaveAnalysis(a); err != nil {
			t.Fatalf("SaveAnalysis(%s): %v", a.ID, err)
		}
	}

	got, err := p.GetAnalysis("an-1")
	if err != nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if got.Finding == nil || got.Finding.Title != "x" {
		t.Fatalf("finding round-trip wrong: %+v", got.Finding)
	}

	list, err := p.ListAnalysesByIncident("inc-A", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("inc-A analyses len=%d, want 2", len(list))
	}

	if err := p.DeleteAnalysis("an-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.GetAnalysis("an-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := p.DeleteAnalysis("missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing, got %v", err)
	}
}

func TestMemoryAnalysisCRUD(t *testing.T) {
	runAnalysisCRUD(t, storage.NewMemory())
}

func TestFileAnalysisCRUD(t *testing.T) {
	dir := t.TempDir()
	p, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	runAnalysisCRUD(t, p)

	// Reload from disk to confirm persistence.
	p2, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list, err := p2.ListAnalysesByIncident("inc-A", 0)
	if err != nil {
		t.Fatalf("reopen list: %v", err)
	}
	if len(list) != 1 { // an-1 was deleted; an-2 remains
		t.Fatalf("reopen len=%d, want 1", len(list))
	}
}
