package agent

import (
	"path/filepath"
	"testing"
)

func TestCatalog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patterns.json")

	cat, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("initial load (no file) should succeed: %v", err)
	}
	if cat.Len() != 0 {
		t.Fatalf("expected empty catalog, got %d", cat.Len())
	}

	cat.Upsert("p-aaa", "user <*> failed login", "src1", 5, 0.2, "", "")
	cat.Upsert("p-bbb", "connection refused <*>", "src1", 12, 0.2, "", "")
	cat.Upsert("p-aaa", "user <*> failed login", "src1", 3, 0.2, "", "")

	if cat.Len() != 2 {
		t.Errorf("expected 2 patterns, got %d", cat.Len())
	}
	if !cat.Dirty() {
		t.Errorf("catalog should be dirty after upserts")
	}
	if got := cat.Get("p-aaa"); got == nil || got.Count != 8 {
		t.Errorf("expected p-aaa count=8, got %+v", got)
	}

	if err := cat.Persist(); err != nil {
		t.Fatalf("persist failed: %v", err)
	}
	if cat.Dirty() {
		t.Errorf("catalog should not be dirty immediately after persist")
	}

	// Reload from disk.
	cat2, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if cat2.Len() != 2 {
		t.Errorf("expected 2 patterns after reload, got %d", cat2.Len())
	}
	if got := cat2.Get("p-bbb"); got == nil || got.Count != 12 {
		t.Errorf("expected p-bbb count=12 after reload, got %+v", got)
	}
}

func TestCatalog_LabelAndDelete(t *testing.T) {
	dir := t.TempDir()
	cat, _ := LoadCatalog(filepath.Join(dir, "patterns.json"))
	cat.Upsert("p-x", "hello <*>", "src", 1, 0.2, "", "")

	if !cat.Label("p-x", "high", "auth-failure", []string{"auth", "noisy"}) {
		t.Fatalf("Label should return true for existing pattern")
	}
	if cat.Label("missing", "high", "x", nil) {
		t.Fatalf("Label should return false for missing pattern")
	}
	got := cat.Get("p-x")
	if got.Severity != "high" || got.Label != "auth-failure" || len(got.Tags) != 2 {
		t.Errorf("label not applied: %+v", got)
	}

	if !cat.Delete("p-x") {
		t.Fatalf("Delete should return true")
	}
	if cat.Len() != 0 {
		t.Errorf("expected empty catalog after delete")
	}
	if cat.Delete("p-x") {
		t.Errorf("Delete should return false for missing pattern")
	}
}

func TestCatalog_UpsertAppliesRegexTag(t *testing.T) {
	dir := t.TempDir()
	cat, _ := LoadCatalog(filepath.Join(dir, "patterns.json"))

	// First-seen with named rule -> label + severity stored.
	cat.Upsert("p-1", "Out of memory <*>", "src", 1, 0.2, "oom-killer", "critical")
	got := cat.Get("p-1")
	if got.Severity != "critical" || got.Label != "oom-killer" {
		t.Errorf("expected critical/oom-killer, got %+v", got)
	}

	// First-seen with default rule -> severity stored.
	cat.Upsert("p-2", "something <*>", "src", 1, 0.2, "default", "low")
	if got := cat.Get("p-2"); got.Severity != "low" || got.Label != "default" {
		t.Errorf("expected low/default, got %+v", got)
	}

	// Default rule first, then named rule -> promote.
	cat.Upsert("p-2", "something <*>", "src", 1, 0.2, "panic", "critical")
	if got := cat.Get("p-2"); got.Severity != "critical" || got.Label != "panic" {
		t.Errorf("expected promotion to critical/panic, got %+v", got)
	}

	// Named rule first, then default -> stay with the named one.
	cat.Upsert("p-1", "Out of memory <*>", "src", 1, 0.2, "default", "low")
	if got := cat.Get("p-1"); got.Severity != "critical" || got.Label != "oom-killer" {
		t.Errorf("named tag should not be downgraded, got %+v", got)
	}
}
