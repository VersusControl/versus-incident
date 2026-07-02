package controllers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// rewindSpySource is a minimal SignalSource that records whether Rewind (the
// core.SourceRewinder seam) was invoked. It stands in for the file source.
type rewindSpySource struct {
	name     string
	rewound  int
	failWith error
}

func (s *rewindSpySource) Name() string { return s.name }
func (s *rewindSpySource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	return nil, since, nil
}
func (s *rewindSpySource) Rewind(_ context.Context) error {
	s.rewound++
	return s.failWith
}

// plainSource has no Rewind — the controller must simply skip it.
type plainSource struct{ name string }

func (s *plainSource) Name() string { return s.name }
func (s *plainSource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	return nil, since, nil
}

// TestClearPatterns_RewindsOwnPositionSources proves DELETE /api/agent/patterns
// rewinds the internal read position of every wired source that keeps its own
// (a core.SourceRewinder — the file source). This is the second cursor of truth
// the poll-cursor reset cannot reach: a file source ignores the poll cursor, so
// without this rewind it stays pinned at EOF and the SAME running worker never
// re-emits its backlog after a clear.
func TestClearPatterns_RewindsOwnPositionSources(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-api", "api failed to <*>", "file:app", 7, 0.2, "default", "api")

	rewinder := &rewindSpySource{name: "file:app"}
	plain := &plainSource{name: "es:prod"}

	ctrl := NewAgentController(cat, agent.NewMiner(0.4, 4, 100), nil, nil, nil, false).
		SetCursorStore(agent.NewCursorStore(nil)).
		SetSources([]core.SignalSource{rewinder, plain})
	app := fiber.New()
	app.Delete("/api/agent/patterns", ctrl.clearPatterns)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/patterns", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if rewinder.rewound != 1 {
		t.Errorf("own-position source rewound %d times, want 1 (clear did not reconcile the file source's byte offset)", rewinder.rewound)
	}
	if cat.Len() != 0 {
		t.Errorf("patterns not cleared: %d remain", cat.Len())
	}
}

// TestClearPatterns_SourceRewindFailureAbortsClear proves a source-rewind
// failure aborts the clear BEFORE the catalog is wiped, leaving state
// consistent (the worker keeps its still-present patterns, no half-clear).
func TestClearPatterns_SourceRewindFailureAbortsClear(t *testing.T) {
	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-api", "api failed to <*>", "file:app", 7, 0.2, "default", "api")

	rewinder := &rewindSpySource{name: "file:app", failWith: context.DeadlineExceeded}
	ctrl := NewAgentController(cat, agent.NewMiner(0.4, 4, 100), nil, nil, nil, false).
		SetCursorStore(agent.NewCursorStore(nil)).
		SetSources([]core.SignalSource{rewinder})
	app := fiber.New()
	app.Delete("/api/agent/patterns", ctrl.clearPatterns)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/api/agent/patterns", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if cat.Len() != 1 {
		t.Errorf("catalog wiped despite a rewind failure: %d patterns remain, want 1 (clear must abort consistently)", cat.Len())
	}
}
