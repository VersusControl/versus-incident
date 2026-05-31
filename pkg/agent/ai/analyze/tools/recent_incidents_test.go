package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func newStoreWithIncidents(t *testing.T, recs ...*storage.IncidentRecord) storage.Provider {
	t.Helper()
	store := storage.NewMemory()
	for _, rec := range recs {
		if err := store.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	return store
}

func TestRecentIncidents_Metadata(t *testing.T) {
	tool := RecentIncidents{}
	if got := tool.Name(); got != "recent_incidents" {
		t.Errorf("Name() = %q, want recent_incidents", got)
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	schema := tool.ArgsSchema()
	if schema["type"] != "object" {
		t.Errorf("ArgsSchema type = %v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("ArgsSchema properties missing or wrong type")
	}
	for _, key := range []string{"window_minutes", "service", "limit"} {
		if _, ok := props[key]; !ok {
			t.Errorf("ArgsSchema missing property %q", key)
		}
	}
}

func TestRecentIncidents_NilStore(t *testing.T) {
	tool := RecentIncidents{}
	if _, err := tool.Invoke(context.Background(), nil); err == nil {
		t.Fatal("expected error when storage not configured")
	}
}

func TestRecentIncidents_BadArgs(t *testing.T) {
	tool := RecentIncidents{Store: storage.NewMemory()}
	if _, err := tool.Invoke(context.Background(), []byte("{not json")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestRecentIncidents_WindowFilter(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "recent", Title: "fresh", Service: "api", CreatedAt: now.Add(-10 * time.Minute)},
		&storage.IncidentRecord{ID: "old", Title: "stale", Service: "api", CreatedAt: now.Add(-5 * time.Hour)},
	)
	tool := RecentIncidents{Store: store}

	res, err := tool.Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{WindowMinutes: 60}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Error("Found = false, want true")
	}
	if got := res.Data["count"]; got != 1 {
		t.Errorf("count = %v, want 1 (old incident excluded)", got)
	}
	incidents := res.Data["incidents"].([]recentIncidentItem)
	if len(incidents) != 1 || incidents[0].ID != "recent" {
		t.Errorf("incidents = %+v, want only the recent one", incidents)
	}
}

func TestRecentIncidents_ServiceFilter(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "a", Service: "api", CreatedAt: now.Add(-time.Minute)},
		&storage.IncidentRecord{ID: "b", Service: "worker", CreatedAt: now.Add(-time.Minute)},
	)
	tool := RecentIncidents{Store: store}

	res, err := tool.Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{Service: "WORKER"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	incidents := res.Data["incidents"].([]recentIncidentItem)
	if len(incidents) != 1 || incidents[0].ID != "b" {
		t.Errorf("incidents = %+v, want only service=worker (case-insensitive)", incidents)
	}
}

func TestRecentIncidents_LimitCapAndDefaults(t *testing.T) {
	now := time.Now().UTC()
	recs := make([]*storage.IncidentRecord, 0, 150)
	for i := 0; i < 150; i++ {
		recs = append(recs, &storage.IncidentRecord{
			ID:        fmt.Sprintf("inc-%d", i),
			Service:   "api",
			CreatedAt: now.Add(-time.Duration(i) * time.Second),
		})
	}
	tool := RecentIncidents{Store: newStoreWithIncidents(t, recs...)}

	// limit over cap → clamp to 100; window over cap → clamp to 1440.
	res, err := tool.Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{
		WindowMinutes: 99999,
		Limit:         9999,
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["window_minutes"]; got != 1440 {
		t.Errorf("window_minutes = %v, want 1440", got)
	}
	incidents := res.Data["incidents"].([]recentIncidentItem)
	if len(incidents) != 100 {
		t.Errorf("len(incidents) = %d, want 100 (limit cap)", len(incidents))
	}
}
