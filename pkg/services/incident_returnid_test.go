package services

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestCreateIncident_ReturnsPersistedID asserts CreateIncident returns the
// ID of the incident it persisted — non-empty and resolvable in the store.
// That ID is the seam an auto-analysis step needs to act on a freshly
// created incident; before this change the function returned only an error.
func TestCreateIncident_ReturnsPersistedID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalReturnIDConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	mem := storage.NewMemory()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(nil) })

	content := map[string]interface{}{"title": "disk space low on worker-04"}
	id, err := CreateIncident("", &content)
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty incident ID")
	}

	got, err := mem.GetIncident(id)
	if err != nil {
		t.Fatalf("stored incident not found by returned ID %q: %v", id, err)
	}
	if got.ID != id {
		t.Errorf("stored ID = %q, returned ID = %q", got.ID, id)
	}
}

// Minimal config: every channel disabled so the fan-out has zero providers
// (succeeds trivially) and on-call is off — isolating the return-value
// behavior from any network path.
const minimalReturnIDConfig = `
name: test
host: 127.0.0.1
port: 3000
public_host: ''
alert:
  debug_body: false
queue:
  enable: false
oncall:
  enable: false
storage:
  type: file
`
