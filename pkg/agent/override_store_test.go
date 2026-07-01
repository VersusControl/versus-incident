package agent

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestServiceOverrideStore_LogPatternMatchWins proves a log override keyed on a
// mined pattern identity re-labels a signal that fields into that pattern.
func TestServiceOverrideStore_LogPatternMatchWins(t *testing.T) {
	s, err := LoadServiceOverrideStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-42", Service: "payments",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok := s.ResolveService(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown", Pattern: "p-42",
	})
	if !ok || got != "payments" {
		t.Fatalf("resolve = (%q,%v), want (payments,true)", got, ok)
	}
}

// TestServiceOverrideStore_LogMessageSubstringMatch proves a log override keyed
// on a message substring matches when the raw message contains it.
func TestServiceOverrideStore_LogMessageSubstringMatch(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	if _, err := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "checkout-svc", Service: "checkout",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := s.ResolveService(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown",
		Pattern: "p-9", Message: "error in checkout-svc handler",
	})
	if !ok || got != "checkout" {
		t.Fatalf("substring resolve = (%q,%v), want (checkout,true)", got, ok)
	}
}

// TestServiceOverrideStore_SourceTypeIsolation proves a metric rule never
// re-labels a log input and vice-versa.
func TestServiceOverrideStore_SourceTypeIsolation(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	if _, err := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceMetric, Match: "http_5xx", Service: "api",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A log input carrying the same string as the metric rule's match must NOT
	// resolve — different source type.
	if _, ok := s.ResolveService(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown", Pattern: "http_5xx",
	}); ok {
		t.Fatalf("metric rule leaked into log resolution")
	}
	// The metric input resolves.
	if got, ok := s.ResolveService(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceMetric, Service: "_unknown", Signal: "http_5xx",
	}); !ok || got != "api" {
		t.Fatalf("metric resolve = (%q,%v), want (api,true)", got, ok)
	}
}

// TestServiceOverrideStore_MetricGlobMatch proves a metric/trace override
// supports `*`/`?` globs on the signal name.
func TestServiceOverrideStore_MetricGlobMatch(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	if _, err := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceTrace, Match: "GET /orders/*", Service: "orders",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	cases := map[string]bool{
		"GET /orders/123":  true,
		"GET /orders/":     true,
		"POST /orders/123": false,
		"GET /users/1":     false,
	}
	for signal, want := range cases {
		_, ok := s.ResolveService(context.Background(), ServiceOverrideInput{
			SourceType: OverrideSourceTrace, Service: "_unknown", Signal: signal,
		})
		if ok != want {
			t.Errorf("glob %q matched=%v, want %v", signal, ok, want)
		}
	}
}

// TestServiceOverrideStore_PerOrgIsolation proves org A's rule can never
// resolve for org B.
func TestServiceOverrideStore_PerOrgIsolation(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	if _, err := s.Put("org-a", OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-1", Service: "svc-a",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctxB := ContextWithOverrideOrg(context.Background(), "org-b")
	if _, ok := s.ResolveService(ctxB, ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown", Pattern: "p-1",
	}); ok {
		t.Fatalf("org-a rule leaked into org-b resolution")
	}
	ctxA := ContextWithOverrideOrg(context.Background(), "org-a")
	if got, ok := s.ResolveService(ctxA, ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown", Pattern: "p-1",
	}); !ok || got != "svc-a" {
		t.Fatalf("org-a resolve = (%q,%v), want (svc-a,true)", got, ok)
	}
}

// TestServiceOverrideStore_PutReplacesSameKey proves re-reassigning the same
// (source_type, match) replaces the rule in place instead of stacking.
func TestServiceOverrideStore_PutReplacesSameKey(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	first, _ := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-1", Service: "old",
	})
	second, _ := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-1", Service: "new",
	})
	if first.ID != second.ID {
		t.Errorf("replace assigned a new ID: %q != %q", first.ID, second.ID)
	}
	rules := s.List(storage.DefaultOrgID)
	if len(rules) != 1 {
		t.Fatalf("rule count = %d, want 1 (replace, not stack)", len(rules))
	}
	if rules[0].Service != "new" {
		t.Errorf("service = %q, want new (last correction wins)", rules[0].Service)
	}
}

// TestServiceOverrideStore_Validation rejects bad input.
func TestServiceOverrideStore_Validation(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	bad := []OverrideRule{
		{SourceType: "bogus", Match: "x", Service: "y"},
		{SourceType: OverrideSourceLog, Match: "", Service: "y"},
		{SourceType: OverrideSourceLog, Match: "x", Service: ""},
	}
	for _, r := range bad {
		if _, err := s.Put(storage.DefaultOrgID, r); err == nil {
			t.Errorf("Put(%+v) = nil error, want rejection", r)
		}
	}
}

// TestServiceOverrideStore_DeleteAndCount proves Delete removes a rule and
// CountForService reports rules targeting a service.
func TestServiceOverrideStore_DeleteAndCount(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	r, _ := s.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-1", Service: "payments",
	})
	if n := s.CountForService(storage.DefaultOrgID, "payments"); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	ok, err := s.Delete(storage.DefaultOrgID, r.ID)
	if err != nil || !ok {
		t.Fatalf("delete = (%v,%v), want (true,nil)", ok, err)
	}
	if n := s.CountForService(storage.DefaultOrgID, "payments"); n != 0 {
		t.Fatalf("count after delete = %d, want 0", n)
	}
	if ok, _ := s.Delete(storage.DefaultOrgID, "missing"); ok {
		t.Errorf("delete of missing id returned true")
	}
}

// TestServiceOverrideStore_RepointService proves rename repoints rules.
func TestServiceOverrideStore_RepointService(t *testing.T) {
	s, _ := LoadServiceOverrideStore(storage.NewMemory())
	_, _ = s.Put(storage.DefaultOrgID, OverrideRule{SourceType: OverrideSourceLog, Match: "p-1", Service: "old"})
	_, _ = s.Put(storage.DefaultOrgID, OverrideRule{SourceType: OverrideSourceMetric, Match: "m1", Service: "old"})
	n, err := s.RepointService(storage.DefaultOrgID, "old", "new")
	if err != nil || n != 2 {
		t.Fatalf("repoint = (%d,%v), want (2,nil)", n, err)
	}
	if got := s.CountForService(storage.DefaultOrgID, "old"); got != 0 {
		t.Errorf("old still targeted by %d rules", got)
	}
	if got := s.CountForService(storage.DefaultOrgID, "new"); got != 2 {
		t.Errorf("new targeted by %d rules, want 2", got)
	}
}

// TestServiceOverrideStore_Persistence proves rules survive a reload from the
// same provider (durable through storage.Provider, never os.WriteFile).
func TestServiceOverrideStore_Persistence(t *testing.T) {
	mem := storage.NewMemory()
	s1, _ := LoadServiceOverrideStore(mem)
	if _, err := s1.Put(storage.DefaultOrgID, OverrideRule{
		SourceType: OverrideSourceLog, Match: "p-1", Service: "payments",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	s2, err := LoadServiceOverrideStore(mem)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := s2.ResolveService(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "_unknown", Pattern: "p-1",
	})
	if !ok || got != "payments" {
		t.Fatalf("after reload resolve = (%q,%v), want (payments,true)", got, ok)
	}
}

// TestMatchSignalGlob covers the exact/glob matcher the UI mirror must agree
// with.
func TestMatchSignalGlob(t *testing.T) {
	cases := []struct {
		signal, entry string
		want          bool
	}{
		{"http_5xx", "http_5xx", true},
		{"http_5xx", "http_4xx", false},
		{"http_5xx", "http_*", true},
		{"http_5xx", "http_?xx", true},
		{"http_50x", "http_?xx", false},
		{"", "http_*", false},
		{"http_5xx", "", false},
		{"a.b.c", "a.*.c", true},
		{"a.b.c", "a.b.d", false},
	}
	for _, c := range cases {
		if got := matchSignalGlob(c.signal, c.entry); got != c.want {
			t.Errorf("matchSignalGlob(%q,%q) = %v, want %v", c.signal, c.entry, got, c.want)
		}
	}
}
