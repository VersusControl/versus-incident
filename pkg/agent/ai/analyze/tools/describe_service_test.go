package tools

import (
	"context"
	"testing"
	"time"
)

func TestDescribeService_Metadata(t *testing.T) {
	tool := DescribeService{}
	if got := tool.Name(); got != "describe_service" {
		t.Errorf("Name() = %q, want describe_service", got)
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	schema := tool.ArgsSchema()
	req, ok := schema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "service" {
		t.Errorf("ArgsSchema required = %v, want [service]", schema["required"])
	}
}

func TestDescribeService_NilCatalog(t *testing.T) {
	tool := DescribeService{}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{Service: "api"})); err == nil {
		t.Fatal("expected error when catalog not configured")
	}
}

func TestDescribeService_BadArgs(t *testing.T) {
	tool := DescribeService{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), []byte("{bad")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestDescribeService_MissingService(t *testing.T) {
	tool := DescribeService{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{})); err == nil {
		t.Fatal("expected error when service is empty")
	}
}

func TestDescribeService_UnknownService(t *testing.T) {
	tool := DescribeService{Catalog: &fakeCatalog{}}
	res, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{Service: "ghost"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false for unknown service")
	}
	if res.Data["service"] != "ghost" {
		t.Errorf("service = %v, want ghost", res.Data["service"])
	}
	if res.Data["pattern_count"] != 0 {
		t.Errorf("pattern_count = %v, want 0", res.Data["pattern_count"])
	}
}

func TestDescribeService_FoundWithTopPatternsSorted(t *testing.T) {
	now := time.Now().UTC()
	cat := &fakeCatalog{
		services: map[string]ServiceInfo{"api": {FirstSeen: now.Add(-2 * time.Hour)}},
		all: []*PatternView{
			{ID: "p1", Service: "api", Template: "low", Count: 5, Baseline: 1},
			{ID: "p2", Service: "api", Template: "high", Count: 50, Baseline: 2, Verdict: "known"},
			{ID: "p3", Service: "api", Template: "mid", Count: 20, Baseline: 3},
			{ID: "x1", Service: "other", Template: "skip", Count: 999},
		},
	}
	tool := DescribeService{Catalog: cat}

	res, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{Service: "api", TopPatterns: 2}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Fatal("Found = false, want true")
	}
	if _, ok := res.Data["first_seen"]; !ok {
		t.Error("Data missing first_seen for known service")
	}
	top := res.Data["top_patterns"].([]describePatternEntry)
	if len(top) != 2 {
		t.Fatalf("len(top_patterns) = %d, want 2 (TopPatterns cap)", len(top))
	}
	// Sorted by count desc: p2 (50) then p3 (20). The other-service
	// pattern must be excluded despite its higher count.
	if top[0].ID != "p2" || top[1].ID != "p3" {
		t.Errorf("top_patterns order = [%s, %s], want [p2, p3]", top[0].ID, top[1].ID)
	}
}

func TestDescribeService_TopPatternsClamp(t *testing.T) {
	cat := &fakeCatalog{all: []*PatternView{{ID: "p1", Service: "api", Count: 1}}}
	tool := DescribeService{Catalog: cat}

	// top_patterns over cap (20) should not error and still return.
	res, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{Service: "api", TopPatterns: 9999}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Data["pattern_count"] != 1 {
		t.Errorf("pattern_count = %v, want 1", res.Data["pattern_count"])
	}
}

func TestDescribeService_IncludesLatestSample(t *testing.T) {
	cat := &fakeCatalog{
		all: []*PatternView{
			// Ring oldest→newest; the listing should carry only the latest one.
			{ID: "p1", Service: "api", Count: 5, Samples: []string{"old line", "newest redacted line"}},
			{ID: "p2", Service: "api", Count: 3}, // no samples → omitted
		},
	}
	tool := DescribeService{Catalog: cat}

	res, err := tool.Invoke(context.Background(), mustArgs(t, describeServiceArgs{Service: "api"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	top := res.Data["top_patterns"].([]describePatternEntry)
	byID := map[string]describePatternEntry{}
	for _, e := range top {
		byID[e.ID] = e
	}
	if got := byID["p1"].Sample; got != "newest redacted line" {
		t.Errorf("p1 sample = %q, want the latest ring entry", got)
	}
	if got := byID["p2"].Sample; got != "" {
		t.Errorf("p2 sample = %q, want empty (no ring)", got)
	}
}
