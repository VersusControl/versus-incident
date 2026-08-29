package tools

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func assertUnavailable(t *testing.T, result *core.ToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result == nil || result.IsAvailable() || result.Reason == "" {
		t.Fatalf("result = %+v, want unavailable with reason", result)
	}
}

func newStoreWithIncidents(t *testing.T, records ...*storage.IncidentRecord) storage.Provider {
	t.Helper()
	store := storage.NewMemory()
	for _, record := range records {
		if err := store.SaveIncident(record); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	return store
}
