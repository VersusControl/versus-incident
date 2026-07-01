package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestCatalog_CreateService_Manual proves a created service is recorded with the
// Manual flag and is visible via Service/AllServices.
func TestCatalog_CreateService_Manual(t *testing.T) {
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if err := cat.CreateService("payments"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	info, ok := cat.Service("payments")
	if !ok {
		t.Fatalf("service not found after create")
	}
	if !info.Manual {
		t.Errorf("Manual = false, want true for an operator-created service")
	}
	if _, ok := cat.AllServices()["payments"]; !ok {
		t.Errorf("service missing from AllServices()")
	}
}

// TestCatalog_CreateService_DuplicateRejected proves a duplicate create is an
// error (the controller maps it to 409).
func TestCatalog_CreateService_DuplicateRejected(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	if err := cat.CreateService("api"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := cat.CreateService("api"); err != ErrServiceExists {
		t.Fatalf("duplicate create err = %v, want ErrServiceExists", err)
	}
}

// TestCatalog_CreateService_PersistsAcrossReload proves a manual service is
// durable (no signal re-creates it, so it must persist immediately).
func TestCatalog_CreateService_PersistsAcrossReload(t *testing.T) {
	mem := storage.NewMemory()
	cat, _ := LoadCatalog(mem)
	if err := cat.CreateService("payments"); err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	reloaded, err := LoadCatalog(mem)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	info, ok := reloaded.Service("payments")
	if !ok {
		t.Fatalf("manual service lost across reload")
	}
	if !info.Manual {
		t.Errorf("Manual flag lost across reload")
	}
}

// TestCatalog_RenameService proves a manual service moves name while preserving
// its manual flag, and that validation errors surface.
func TestCatalog_RenameService(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	if err := cat.CreateService("old"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := cat.RenameService("old", "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, ok := cat.Service("old"); ok {
		t.Errorf("old name still present after rename")
	}
	info, ok := cat.Service("new")
	if !ok || !info.Manual {
		t.Fatalf("new name missing or lost manual flag: ok=%v manual=%v", ok, info.Manual)
	}
	if err := cat.RenameService("missing", "x"); err != ErrServiceNotFound {
		t.Errorf("rename of missing = %v, want ErrServiceNotFound", err)
	}
	_ = cat.CreateService("taken")
	if err := cat.RenameService("new", "taken"); err != ErrServiceExists {
		t.Errorf("rename onto existing = %v, want ErrServiceExists", err)
	}
}

// TestCatalog_DeleteService proves delete removes a manual service durably.
func TestCatalog_DeleteService(t *testing.T) {
	mem := storage.NewMemory()
	cat, _ := LoadCatalog(mem)
	_ = cat.CreateService("payments")
	if !cat.DeleteService("payments") {
		t.Fatalf("DeleteService returned false for existing manual service")
	}
	if _, ok := cat.Service("payments"); ok {
		t.Errorf("service still present after delete")
	}
	if cat.DeleteService("missing") {
		t.Errorf("DeleteService of missing returned true")
	}
	reloaded, _ := LoadCatalog(mem)
	if _, ok := reloaded.Service("payments"); ok {
		t.Errorf("deleted service resurrected across reload")
	}
}

// TestCatalog_AutoServiceNotManual proves an auto-discovered service (via
// RegisterService) is NOT flagged manual, so the controller can refuse to
// rename/delete it.
func TestCatalog_AutoServiceNotManual(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.RegisterService("auto")
	info, ok := cat.Service("auto")
	if !ok {
		t.Fatalf("auto service not found")
	}
	if info.Manual {
		t.Errorf("auto-discovered service flagged Manual, want false")
	}
}
