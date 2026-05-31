package tools

import (
	"context"
	"testing"
	"time"
)

// fakeCatalog is a scripted PatternCatalog for tests. Declaring it in
// the tools package keeps the import graph one-directional (no
// dependency on pkg/agent).
type fakeCatalog struct {
	byID     map[string]*PatternView
	all      []*PatternView
	services map[string]ServiceInfo
}

func (f *fakeCatalog) Get(id string) *PatternView { return f.byID[id] }
func (f *fakeCatalog) All() []*PatternView        { return f.all }
func (f *fakeCatalog) AllServices() map[string]ServiceInfo {
	if f.services == nil {
		return map[string]ServiceInfo{}
	}
	return f.services
}

func TestPatternHistory_Metadata(t *testing.T) {
	tool := PatternHistory{}
	if got := tool.Name(); got != "pattern_history" {
		t.Errorf("Name() = %q, want pattern_history", got)
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	schema := tool.ArgsSchema()
	req, ok := schema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "pattern_id" {
		t.Errorf("ArgsSchema required = %v, want [pattern_id]", schema["required"])
	}
}

func TestPatternHistory_NilCatalog(t *testing.T) {
	tool := PatternHistory{}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"})); err == nil {
		t.Fatal("expected error when catalog not configured")
	}
}

func TestPatternHistory_BadArgs(t *testing.T) {
	tool := PatternHistory{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), []byte("{bad")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestPatternHistory_MissingPatternID(t *testing.T) {
	tool := PatternHistory{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{})); err == nil {
		t.Fatal("expected error when pattern_id is empty")
	}
}

func TestPatternHistory_NotFound(t *testing.T) {
	tool := PatternHistory{Catalog: &fakeCatalog{byID: map[string]*PatternView{}}}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "missing"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false for unknown pattern")
	}
	if res.Data["pattern_id"] != "missing" {
		t.Errorf("pattern_id = %v, want missing", res.Data["pattern_id"])
	}
}

func TestPatternHistory_Found(t *testing.T) {
	now := time.Now().UTC()
	pat := &PatternView{
		ID:        "p1",
		Template:  "connection refused <*>",
		Source:    "file:app",
		Service:   "api",
		RuleName:  "errors",
		Verdict:   "known",
		Tags:      []string{"net"},
		Count:     42,
		Baseline:  3.14,
		FirstSeen: now.Add(-time.Hour),
		LastSeen:  now,
	}
	cat := &fakeCatalog{byID: map[string]*PatternView{"p1": pat}}
	tool := PatternHistory{Catalog: cat}

	res, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Fatal("Found = false, want true")
	}
	checks := map[string]any{
		"pattern_id": "p1",
		"template":   "connection refused <*>",
		"service":    "api",
		"rule_name":  "errors",
		"verdict":    "known",
		"count":      42,
		"baseline":   3.14,
	}
	for k, want := range checks {
		if got := res.Data[k]; got != want {
			t.Errorf("Data[%q] = %v, want %v", k, got, want)
		}
	}
}
