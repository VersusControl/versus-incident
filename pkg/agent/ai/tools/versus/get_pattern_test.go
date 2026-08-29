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

func TestGetPatternMetadata(t *testing.T) {
	tool := GetPattern{}
	if got := tool.Name(); got != "get_pattern" {
		t.Errorf("Name() = %q, want get_pattern", got)
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

func TestGetPatternNilCatalog(t *testing.T) {
	tool := GetPattern{}
	result, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	assertUnavailable(t, result, err)
}

func TestGetPatternBadArgs(t *testing.T) {
	tool := GetPattern{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), []byte("{bad")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestGetPatternMissingPatternID(t *testing.T) {
	tool := GetPattern{Catalog: &fakeCatalog{}}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{})); err == nil {
		t.Fatal("expected error when pattern_id is empty")
	}
}

func TestGetPatternNotFound(t *testing.T) {
	tool := GetPattern{Catalog: &fakeCatalog{byID: map[string]*PatternView{}}}
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

func TestGetPatternReadyThresholdDoesNotInferProvenance(t *testing.T) {
	now := time.Now().UTC()
	pat := &PatternView{
		ID:               "p1",
		Template:         "connection refused <*>",
		Source:           "file:app",
		Service:          "api",
		RuleName:         "errors",
		Verdict:          "known",
		Tags:             []string{"net"},
		Count:            42,
		Baseline:         3.14,
		AutoPromoteAfter: 40,
		FirstSeen:        now.Add(-time.Hour),
		LastSeen:         now,
	}
	cat := &fakeCatalog{byID: map[string]*PatternView{"p1": pat}}
	tool := GetPattern{Catalog: cat, Redactor: secretRedactor{}}

	res, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Fatal("Found = false, want true")
	}
	checks := map[string]any{
		"pattern_id":                   "p1",
		"template":                     "connection refused <*>",
		"service":                      "api",
		"rule_name":                    "errors",
		"verdict":                      "known",
		"count":                        42,
		"baseline":                     3.14,
		"ready":                        true,
		"effective_auto_promote_after": 40,
		"reason_known":                 false,
		"provenance":                   "unknown",
	}
	for k, want := range checks {
		if got := res.Data[k]; got != want {
			t.Errorf("Data[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestGetPatternKnownBelowThresholdHasUnknownProvenance(t *testing.T) {
	pattern := &PatternView{ID: "operator-or-legacy", Verdict: "known", Count: 3, AutoPromoteAfter: 40}
	result, err := (GetPattern{Catalog: &fakeCatalog{byID: map[string]*PatternView{pattern.ID: pattern}}}).Invoke(
		context.Background(), mustArgs(t, patternHistoryArgs{PatternID: pattern.ID}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Data["reason_known"] != false || result.Data["provenance"] != "unknown" {
		t.Fatalf("reason provenance = known:%v provenance:%v", result.Data["reason_known"], result.Data["provenance"])
	}
}

func TestGetPatternOmitsSamplesWithoutRedactor(t *testing.T) {
	cat := &fakeCatalog{byID: map[string]*PatternView{"p1": {ID: "p1", Samples: []string{"secret"}}}}
	res, err := (GetPattern{Catalog: cat}).Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, ok := res.Data["samples"]; ok {
		t.Fatalf("samples = %v, want omitted", res.Data["samples"])
	}
}

func TestGetPatternSamplesLatestThree(t *testing.T) {
	pat := &PatternView{
		ID:      "p1",
		Service: "api",
		// Ring is oldest→newest; the tool should return only the latest 3.
		Samples: []string{"s1", "s2", "s3", "s4", "s5"},
	}
	cat := &fakeCatalog{byID: map[string]*PatternView{"p1": pat}}
	tool := GetPattern{Catalog: cat, Redactor: secretRedactor{}}

	res, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got, ok := res.Data["samples"].([]string)
	if !ok {
		t.Fatalf("Data[samples] = %v (%T), want []string", res.Data["samples"], res.Data["samples"])
	}
	want := []string{"s3", "s4", "s5"}
	if len(got) != len(want) {
		t.Fatalf("samples = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("samples[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if res.Data["samples_total"] != 5 || res.Data["samples_truncated"] != true {
		t.Errorf("sample metadata = total:%v truncated:%v", res.Data["samples_total"], res.Data["samples_truncated"])
	}
}

func TestGetPatternNoSamplesOmitsKey(t *testing.T) {
	pat := &PatternView{ID: "p1", Service: "api"}
	cat := &fakeCatalog{byID: map[string]*PatternView{"p1": pat}}
	tool := GetPattern{Catalog: cat}

	res, err := tool.Invoke(context.Background(), mustArgs(t, patternHistoryArgs{PatternID: "p1"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, present := res.Data["samples"]; present {
		t.Errorf("samples key must be omitted when the ring is empty, got %v", res.Data["samples"])
	}
}
