package tools

import (
	"context"
	"testing"
	"time"
)

// fakePatternCatalog is a hand-rolled in-memory PatternCatalog for the
// pattern_search tests. We do NOT depend on pkg/agent here (cycle).
type fakePatternCatalog struct {
	patterns map[string]*PatternView
}

func newFakeCatalog(views ...*PatternView) *fakePatternCatalog {
	m := make(map[string]*PatternView, len(views))
	for _, v := range views {
		m[v.ID] = v
	}
	return &fakePatternCatalog{patterns: m}
}

func (f *fakePatternCatalog) Get(id string) *PatternView { return f.patterns[id] }

func (f *fakePatternCatalog) All() []*PatternView {
	out := make([]*PatternView, 0, len(f.patterns))
	for _, v := range f.patterns {
		out = append(out, v)
	}
	return out
}

func (f *fakePatternCatalog) AllServices() map[string]ServiceInfo { return nil }

// TestDefault_PatternSearchRegistration verifies pattern_search is wired
// whenever a catalog is present — alongside pattern_history — and omitted
// when no catalog is configured (same convention as the find_runbook
// registration test in tools_default_test.go).
func TestDefault_PatternSearchRegistration(t *testing.T) {
	withCat := Default(nil, newFakeCatalog(), nil, nil, nil, nil, nil, nil, nil)
	if !hasTool(withCat, "pattern_search") {
		t.Error("pattern_search not registered when catalog is present")
	}
	if !hasTool(withCat, "pattern_history") {
		t.Error("pattern_history missing — registration order broke")
	}

	noCat := Default(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if hasTool(noCat, "pattern_search") {
		t.Error("pattern_search registered without a catalog")
	}
}

func TestPatternSearch_Metadata(t *testing.T) {
	tool := PatternSearch{}
	if got := tool.Name(); got != "pattern_search" {
		t.Errorf("Name() = %q, want pattern_search", got)
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
		t.Fatalf("ArgsSchema properties missing")
	}
	for _, key := range []string{"query", "service", "verdict", "rule_name", "order_by", "limit"} {
		if _, ok := props[key]; !ok {
			t.Errorf("ArgsSchema missing property %q", key)
		}
	}
}

func TestPatternSearch_NilCatalog(t *testing.T) {
	if _, err := (PatternSearch{}).Invoke(context.Background(), nil); err == nil {
		t.Fatal("expected error when catalog not configured")
	}
}

func TestPatternSearch_BadArgs(t *testing.T) {
	tool := PatternSearch{Catalog: newFakeCatalog()}
	if _, err := tool.Invoke(context.Background(), []byte("{not json")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestPatternSearch_InvalidEnums(t *testing.T) {
	tool := PatternSearch{Catalog: newFakeCatalog()}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{Verdict: "bogus"})); err == nil {
		t.Fatal("expected error for unknown verdict")
	}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{OrderBy: "size_desc"})); err == nil {
		t.Fatal("expected error for unknown order_by")
	}
}

func seededCatalog() *fakePatternCatalog {
	now := time.Now().UTC()
	return newFakeCatalog(
		&PatternView{
			ID: "p-conn", Template: "Connection refused on <*>",
			Service: "orders", RuleName: "5xx-burst", Verdict: "unknown",
			Count: 12, Baseline: 4.2,
			FirstSeen: now.Add(-3 * time.Hour), LastSeen: now.Add(-5 * time.Minute),
		},
		&PatternView{
			ID: "p-panic", Template: "panic: runtime error <*>",
			Service: "orders", RuleName: "panic", Verdict: "spike",
			Count: 30, Baseline: 1.5,
			FirstSeen: now.Add(-30 * time.Minute), LastSeen: now.Add(-2 * time.Minute),
		},
		&PatternView{
			ID: "p-oom", Template: "Out of memory: Killed process <*>",
			Service: "billing", RuleName: "oom", Verdict: "known",
			Count: 1, Baseline: 1.0,
			FirstSeen: now.Add(-24 * time.Hour), LastSeen: now.Add(-1 * time.Hour),
		},
		&PatternView{
			ID: "p-info", Template: "user <*> logged in",
			Service: "auth", RuleName: "default", Verdict: "known",
			Count: 9001, Baseline: 200,
			FirstSeen: now.Add(-72 * time.Hour), LastSeen: now,
		},
	)
}

func TestPatternSearch_QuerySubstringCaseInsensitive(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{Query: "PANIC"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Data["patterns"].([]patternSearchItem)
	if len(out) != 1 || out[0].ID != "p-panic" {
		t.Errorf("query=PANIC -> %+v, want only p-panic", out)
	}
}

func TestPatternSearch_ServiceAndVerdictFilters(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	// orders + unknown -> only p-conn
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{
		Service: "ORDERS",
		Verdict: "unknown",
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Data["patterns"].([]patternSearchItem)
	if len(out) != 1 || out[0].ID != "p-conn" {
		t.Errorf("service=orders + verdict=unknown -> %+v, want only p-conn", out)
	}
}

func TestPatternSearch_RuleNameExactMatch(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{RuleName: "oom"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Data["patterns"].([]patternSearchItem)
	if len(out) != 1 || out[0].ID != "p-oom" {
		t.Errorf("rule_name=oom -> %+v, want only p-oom", out)
	}
	// rule_name match is case-SENSITIVE on purpose (rule names are operator-authored ids).
	res, err = tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{RuleName: "OOM"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["count"]; got != 0 {
		t.Errorf("rule_name=OOM -> count=%v, want 0 (exact match)", got)
	}
}

func TestPatternSearch_OrderByCountDescDefault(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Data["patterns"].([]patternSearchItem)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 (no filters)", len(out))
	}
	wantOrder := []string{"p-info", "p-panic", "p-conn", "p-oom"} // counts: 9001 > 30 > 12 > 1
	for i, id := range wantOrder {
		if out[i].ID != id {
			t.Errorf("order[%d] = %q, want %q", i, out[i].ID, id)
		}
	}
}

func TestPatternSearch_OrderByLastSeenDesc(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{OrderBy: "last_seen_desc"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Data["patterns"].([]patternSearchItem)
	// last_seen: p-info (now) > p-panic (-2m) > p-conn (-5m) > p-oom (-1h)
	wantOrder := []string{"p-info", "p-panic", "p-conn", "p-oom"}
	for i, id := range wantOrder {
		if out[i].ID != id {
			t.Errorf("last_seen order[%d] = %q, want %q", i, out[i].ID, id)
		}
	}
}

func TestPatternSearch_LimitTruncation(t *testing.T) {
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{Limit: 2}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["count"]; got != 2 {
		t.Errorf("count = %v, want 2", got)
	}
	if got := res.Data["total_matched"]; got != 4 {
		t.Errorf("total_matched = %v, want 4", got)
	}
	if got := res.Data["truncated"]; got != true {
		t.Errorf("truncated = %v, want true", got)
	}
}

func TestPatternSearch_LimitClamp(t *testing.T) {
	// 150 generated patterns -> limit=9999 should clamp to 100.
	views := make([]*PatternView, 0, 150)
	for i := 0; i < 150; i++ {
		views = append(views, &PatternView{
			ID:       fakeID(i),
			Template: "noise",
			Count:    150 - i, // deterministic, distinct counts
		})
	}
	tool := PatternSearch{Catalog: newFakeCatalog(views...)}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{Limit: 9999}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["count"]; got != 100 {
		t.Errorf("limit 9999 -> count=%v, want clamped to 100", got)
	}
}

func TestPatternSearch_EmptyResultStillFound(t *testing.T) {
	// "Found" stays true for list-style tools even with zero matches —
	// matches the convention used by recent_incidents / recent_changes.
	tool := PatternSearch{Catalog: seededCatalog()}
	res, err := tool.Invoke(context.Background(), mustArgs(t, patternSearchArgs{Query: "no-such-substring"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Error("Found = false; list-style tools should keep Found=true with empty list")
	}
	if got := res.Data["count"]; got != 0 {
		t.Errorf("count = %v, want 0", got)
	}
	if got := res.Data["truncated"]; got != false {
		t.Errorf("truncated = %v, want false", got)
	}
}

func fakeID(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "p-0"
	}
	out := []byte("p-")
	rev := []byte{}
	for i > 0 {
		rev = append(rev, digits[i%10])
		i /= 10
	}
	for j := len(rev) - 1; j >= 0; j-- {
		out = append(out, rev[j])
	}
	return string(out)
}
