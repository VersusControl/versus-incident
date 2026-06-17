package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCatalog_Sweep_Retention(t *testing.T) {
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("old", "t", "src", 1, 0.2, "default", "svc")
	cat.Upsert("fresh", "t", "src", 1, 0.2, "default", "svc")
	cat.Upsert("curated", "t", "src", 1, 0.2, "default", "svc")
	cat.patterns["old"].LastSeen = time.Now().Add(-800 * time.Hour)
	cat.patterns["curated"].LastSeen = time.Now().Add(-800 * time.Hour)
	cat.Label("curated", "known", nil) // curated → never auto-evicted

	if removed := cat.Sweep(0, 720*time.Hour); removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the uncurated stale pattern)", removed)
	}
	if cat.Get("old") != nil {
		t.Error("stale uncurated pattern should be evicted")
	}
	if cat.Get("fresh") == nil {
		t.Error("fresh pattern should survive")
	}
	if cat.Get("curated") == nil {
		t.Error("curated pattern must never be auto-evicted")
	}
}

func TestCatalog_Sweep_Cap(t *testing.T) {
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	base := time.Now()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("p%d", i)
		cat.Upsert(id, "t", "src", 1, 0.2, "default", "svc")
		cat.patterns[id].LastSeen = base.Add(time.Duration(i) * time.Minute) // p0 oldest
	}
	cat.Label("p0", "known", nil) // curated oldest must survive the cap

	if removed := cat.Sweep(3, 0); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if cat.Len() != 3 {
		t.Fatalf("len = %d, want 3", cat.Len())
	}
	if cat.Get("p0") == nil {
		t.Error("curated oldest must survive the cap")
	}
	if cat.Get("p1") != nil || cat.Get("p2") != nil {
		t.Error("oldest uncurated patterns should be evicted under the cap")
	}
	if cat.Get("p3") == nil || cat.Get("p4") == nil {
		t.Error("newest patterns should survive")
	}
}

func TestCatalog_Sweep_DisabledByZero(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.Upsert("a", "t", "src", 1, 0.2, "default", "svc")
	cat.patterns["a"].LastSeen = time.Now().Add(-9000 * time.Hour)
	if removed := cat.Sweep(0, 0); removed != 0 {
		t.Errorf("cap=0 retention=0 must evict nothing, removed %d", removed)
	}
}

func TestCatalog_PurgePatterns(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.Upsert("a", "t", "src", 1, 0.2, "default", "svc-a")
	cat.Upsert("b", "t", "src", 1, 0.2, "default", "svc-b")
	cat.Label("a", "known", nil) // explicit purge ignores curation

	if n := cat.PurgePatterns("svc-a", 0); n != 1 {
		t.Fatalf("purge by service removed %d, want 1", n)
	}
	if cat.Get("a") != nil {
		t.Error("svc-a pattern should be purged even though curated (explicit action)")
	}
	if cat.Get("b") == nil {
		t.Error("svc-b pattern should remain")
	}
}

func TestCatalog_PurgePatterns_OlderThan(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.Upsert("old", "t", "src", 1, 0.2, "default", "svc")
	cat.Upsert("new", "t", "src", 1, 0.2, "default", "svc")
	cat.patterns["old"].LastSeen = time.Now().Add(-48 * time.Hour)

	if n := cat.PurgePatterns("", 24*time.Hour); n != 1 {
		t.Fatalf("purge older_than removed %d, want 1", n)
	}
	if cat.Get("old") != nil || cat.Get("new") == nil {
		t.Error("only patterns older than the cutoff should be purged")
	}
}

func TestCatalog_DeleteService(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.RegisterService("svc-x")

	if !cat.DeleteService("svc-x") {
		t.Error("DeleteService should return true for a known service")
	}
	if cat.DeleteService("svc-x") {
		t.Error("DeleteService should return false once removed")
	}
	if cat.DeleteService("never-seen") {
		t.Error("DeleteService should return false for an unknown service")
	}
}
