package controllers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// nonPagerStore wraps a Provider through the interface type so the optional
// storage.AnalysisPager methods are NOT promoted — a value of this type never
// satisfies AnalysisPager even when its dynamic backend does. It forces the
// listAllAnalyses fallback path (bounded ListAnalyses), mirroring the redis
// stub that has no pager capability.
type nonPagerStore struct {
	storage.Provider
}

func seedControllerAnalyses(t *testing.T, store storage.Provider, n int) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < n; i++ {
		rec := &storage.AnalysisRecord{
			ID:          "an-" + string(rune('a'+i)),
			IncidentID:  "inc-1",
			RequestedAt: base.Add(time.Duration(i) * time.Minute),
			Status:      "ok",
		}
		if err := store.SaveAnalysis(rec); err != nil {
			t.Fatalf("SaveAnalysis %d: %v", i, err)
		}
	}
}

// analysesResp is the paged list response shape the endpoint returns.
type analysesResp struct {
	Analyses   []storage.AnalysisRecord `json:"analyses"`
	Total      int                      `json:"total"`
	Offset     int                      `json:"offset"`
	PageSize   int                      `json:"page_size"`
	NextOffset *int                     `json:"next_offset"`
}

func getAnalyses(t *testing.T, app *fiber.App, url string) analysesResp {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got analysesResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return got
}

// TestListAllAnalysesPaged proves the pager path serves a bounded page plus the
// whole-set total, newest first, with offset continuation.
func TestListAllAnalysesPaged(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	store := storage.NewMemory()
	seedControllerAnalyses(t, store, 5)
	services.SetStorage(store)

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/analyses", ctrl.listAllAnalyses)

	// First page of 2 rows: total is the whole set (5), next_offset points past
	// this page since it is full.
	got := getAnalyses(t, app, "/analyses?page_size=2")
	if got.Total != 5 {
		t.Fatalf("total = %d, want 5", got.Total)
	}
	if len(got.Analyses) != 2 {
		t.Fatalf("page rows = %d, want 2", len(got.Analyses))
	}
	if got.PageSize != 2 {
		t.Fatalf("page_size = %d, want 2", got.PageSize)
	}
	if got.NextOffset == nil || *got.NextOffset != 2 {
		t.Fatalf("next_offset = %v, want 2", got.NextOffset)
	}
	// Newest first: the last-seeded id leads.
	if got.Analyses[0].ID != "an-e" {
		t.Fatalf("first row = %q, want an-e (newest)", got.Analyses[0].ID)
	}

	// Last page (offset 4) is underfull, so next_offset is null (end reached).
	last := getAnalyses(t, app, "/analyses?page_size=2&offset=4")
	if len(last.Analyses) != 1 {
		t.Fatalf("last page rows = %d, want 1", len(last.Analyses))
	}
	if last.NextOffset != nil {
		t.Fatalf("next_offset = %v, want null at end", last.NextOffset)
	}
}

// TestListAllAnalysesFallback proves a backend without the pager capability
// still serves a bounded list via ListAnalyses, and reports a total.
func TestListAllAnalysesFallback(t *testing.T) {
	t.Cleanup(func() { services.SetStorage(nil) })
	inner := storage.NewMemory()
	seedControllerAnalyses(t, inner, 3)
	store := nonPagerStore{Provider: inner}
	if _, ok := interface{}(store).(storage.AnalysisPager); ok {
		t.Fatal("nonPagerStore unexpectedly satisfies AnalysisPager")
	}
	services.SetStorage(store)

	ctrl := NewIncidentAdminController()
	app := fiber.New()
	app.Get("/analyses", ctrl.listAllAnalyses)

	got := getAnalyses(t, app, "/analyses")
	if len(got.Analyses) != 3 {
		t.Fatalf("fallback rows = %d, want 3", len(got.Analyses))
	}
	if got.Total != 3 {
		t.Fatalf("fallback total = %d, want 3", got.Total)
	}
}
