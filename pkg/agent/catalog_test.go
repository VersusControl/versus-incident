package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestCatalog_ConcurrentGetUpsertNoRace catches the data race where
// Catalog.Get returned a live *Pattern that callers could read while
// a concurrent Upsert was writing to the same struct. Run with -race
// to verify; without -race the test will pass even on the buggy code.
func TestCatalog_ConcurrentGetUpsertNoRace(t *testing.T) {
	store := newTestStore(t)
	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Seed one pattern that both goroutines hammer.
	cat.Upsert("p1", "tpl", "src", 1, 0.2, "rule", "svc")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				cat.Upsert("p1", "tpl", "src", 2, 0.2, "rule", "svc")
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if p := cat.Get("p1"); p != nil {
					// Touch a few fields; under the buggy code these reads
					// race with the writer's Upsert.
					_ = p.Count
					_ = p.BaselineFrequency
					_ = p.Template
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// newTestStore returns a file-backed storage provider rooted in t.TempDir.
// Used by tests that need to persist and reload across catalog instances.
func newTestStore(t *testing.T) storage.Provider {
	t.Helper()
	s, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return s
}

func TestCatalog_RoundTrip(t *testing.T) {
	store := newTestStore(t)

	cat, err := LoadCatalog(store)
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

	// Reload from the same store.
	cat2, err := LoadCatalog(store)
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
	cat, _ := LoadCatalog(storage.NewMemory())
	cat.Upsert("p-x", "hello <*>", "src", 1, 0.2, "", "")

	if !cat.Label("p-x", "known", []string{"auth", "noisy"}) {
		t.Fatalf("Label should return true for existing pattern")
	}
	if cat.Label("missing", "known", nil) {
		t.Fatalf("Label should return false for missing pattern")
	}
	got := cat.Get("p-x")
	if got.Verdict != "known" || len(got.Tags) != 2 {
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

// TestCatalog_Reset_NilStore proves the whole-catalog wipe on the default
// (nil-store) inline path: every pattern AND service is removed, the correct
// counts are returned, the empty catalog is persisted (a fresh reload sees
// nothing), and an unrelated blob in the same store is untouched (the reset
// only rewrites the "patterns" blob — namespace isolation).
func TestCatalog_Reset_NilStore(t *testing.T) {
	SetCatalogStore(nil)
	store := newTestStore(t)

	// An unrelated blob that MUST survive the reset.
	if err := store.WriteBlob("unrelated", []byte("keep-me")); err != nil {
		t.Fatalf("seed unrelated blob: %v", err)
	}

	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.Upsert("p-a", "a <*>", "src", 3, 0.2, "", "svc-a")
	cat.Upsert("p-b", "b <*>", "src", 5, 0.2, "", "svc-b")
	cat.RegisterService("svc-a")
	cat.RegisterService("svc-b")
	cat.RegisterService("svc-c")

	patterns, services, err := cat.Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if patterns != 2 {
		t.Errorf("patterns cleared = %d, want 2", patterns)
	}
	if services != 3 {
		t.Errorf("services cleared = %d, want 3", services)
	}
	if cat.Len() != 0 {
		t.Errorf("catalog not empty after reset: %d patterns", cat.Len())
	}
	if n := len(cat.AllServices()); n != 0 {
		t.Errorf("services not empty after reset: %d", n)
	}

	// Persisted empty: a fresh reload from the same store sees nothing.
	reloaded, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Len() != 0 {
		t.Errorf("reloaded catalog has %d patterns, want 0 (reset must persist empty)", reloaded.Len())
	}
	if n := len(reloaded.AllServices()); n != 0 {
		t.Errorf("reloaded catalog has %d services, want 0", n)
	}

	// Isolation: the unrelated blob is untouched.
	got, err := store.ReadBlob("unrelated")
	if err != nil {
		t.Fatalf("ReadBlob(unrelated): %v", err)
	}
	if string(got) != "keep-me" {
		t.Errorf("unrelated blob = %q, want %q (reset must not touch other blobs)", got, "keep-me")
	}
}

// TestCatalog_Reset_RoutesThroughStore proves that when a CatalogStore is
// installed the wipe routes through it as a single CatalogEditReset (so a
// fleet-wide read view is cleared, not just this instance), the in-memory
// working set is emptied, and the returned counts reflect the store's
// (fleet-wide) snapshot rather than the local map.
func TestCatalog_Reset_RoutesThroughStore(t *testing.T) {
	fake := &fakeCatalogStore{
		patterns: map[string]*Pattern{
			"p-1": {ID: "p-1", Template: "one <*>", Count: 4},
			"p-2": {ID: "p-2", Template: "two <*>", Count: 9},
		},
		services: map[string]*ServiceInfo{
			"svc-a": {FirstSeen: time.Now().UTC()},
		},
	}
	SetCatalogStore(fake)
	t.Cleanup(func() { SetCatalogStore(nil) })

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	patterns, services, err := cat.Reset()
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if patterns != 2 {
		t.Errorf("patterns cleared = %d, want 2 (from store snapshot)", patterns)
	}
	if services != 1 {
		t.Errorf("services cleared = %d, want 1 (from store snapshot)", services)
	}

	_, _, _, curates := fake.counts()
	if curates != 1 {
		t.Fatalf("store curate calls = %d, want exactly 1", curates)
	}
	if got := fake.curates[0].Kind; got != CatalogEditReset {
		t.Errorf("curate kind = %q, want %q", got, CatalogEditReset)
	}
}

func TestCatalog_UpsertAppliesRegexTag(t *testing.T) {
	cat, _ := LoadCatalog(storage.NewMemory())

	// First-seen with named rule -> RuleName stored.
	cat.Upsert("p-1", "Out of memory <*>", "src", 1, 0.2, "oom-killer", "")
	if got := cat.Get("p-1"); got.RuleName != "oom-killer" {
		t.Errorf("expected oom-killer, got %+v", got)
	}

	// First-seen with default rule -> RuleName=default.
	cat.Upsert("p-2", "something <*>", "src", 1, 0.2, "default", "")
	if got := cat.Get("p-2"); got.RuleName != "default" {
		t.Errorf("expected default, got %+v", got)
	}

	// Default rule first, then named rule -> promote.
	cat.Upsert("p-2", "something <*>", "src", 1, 0.2, "panic", "")
	if got := cat.Get("p-2"); got.RuleName != "panic" {
		t.Errorf("expected promotion to panic, got %+v", got)
	}

	// Named rule first, then default -> stay with the named one.
	cat.Upsert("p-1", "Out of memory <*>", "src", 1, 0.2, "default", "")
	if got := cat.Get("p-1"); got.RuleName != "oom-killer" {
		t.Errorf("named tag should not be downgraded, got %+v", got)
	}
}

func TestCatalog_ServiceTracking(t *testing.T) {
	store := newTestStore(t)
	cat, _ := LoadCatalog(store)

	// First registration returns true.
	if !cat.RegisterService("checkout-v2") {
		t.Fatal("expected new registration")
	}
	// Duplicate returns false.
	if cat.RegisterService("checkout-v2") {
		t.Fatal("expected duplicate registration to return false")
	}

	// Grace check: 1h grace → service should be in grace.
	if !cat.IsServiceInGrace("checkout-v2", 1*time.Hour) {
		t.Error("service should be in grace within 1h window")
	}
	// Grace disabled (0) → never in grace.
	if cat.IsServiceInGrace("checkout-v2", 0) {
		t.Error("grace=0 should always return false")
	}

	// Unknown service auto-registered by IsServiceInGrace.
	if !cat.IsServiceInGrace("api-gateway", 1*time.Hour) {
		t.Error("unknown service should be auto-registered and in grace")
	}
	svcs := cat.AllServices()
	if _, ok := svcs["api-gateway"]; !ok {
		t.Error("auto-registered service should appear in AllServices")
	}

	// EndServiceGrace → no longer in grace.
	if !cat.EndServiceGrace("checkout-v2") {
		t.Fatal("EndServiceGrace should succeed for existing service")
	}
	if cat.IsServiceInGrace("checkout-v2", 1*time.Hour) {
		t.Error("service should not be in grace after EndServiceGrace")
	}
	if cat.EndServiceGrace("nonexistent") {
		t.Error("EndServiceGrace should fail for unknown service")
	}

	// RestartServiceGrace → back in grace.
	if !cat.RestartServiceGrace("checkout-v2") {
		t.Fatal("RestartServiceGrace should succeed")
	}
	if !cat.IsServiceInGrace("checkout-v2", 1*time.Hour) {
		t.Error("service should be in grace after restart")
	}
	if cat.RestartServiceGrace("nonexistent") {
		t.Error("RestartServiceGrace should fail for unknown service")
	}

	// Services survive persist + reload.
	if err := cat.Persist(); err != nil {
		t.Fatalf("persist failed: %v", err)
	}
	cat2, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	svcs2 := cat2.AllServices()
	if len(svcs2) != 2 {
		t.Errorf("expected 2 services after reload, got %d", len(svcs2))
	}
	if _, ok := svcs2["checkout-v2"]; !ok {
		t.Error("checkout-v2 should survive persist+reload")
	}
}

func TestServiceMatcher_CommonFormats(t *testing.T) {
	// Exercise the same regexes shipped in config/config.yaml against
	// representative log lines from the languages/libraries documented there.
	// NOTE: no ^ anchors on bracket/syslog patterns because sig.Message
	// starts with the level prefix (e.g. "ERROR [django.request] [orders]").
	// Order matters: syslog before single-bracket so nginx[1234]: → "nginx".
	patterns := []string{
		`(?i)\bservice[._-]?name["\s:=]+"?([A-Za-z0-9._-]+)`,
		`(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)`,
		`(?i)"(?:service|svc|app|component)"\s*:\s*"([A-Za-z0-9._-]+)"`,
		`^\s*\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?\s+([A-Za-z][A-Za-z0-9._-]*)\s+\[`,
		`\[\s*([A-Za-z][A-Za-z0-9._-]*)\s*,\s*(?i:request[_-]?id|trace[_-]?id|span[_-]?id)\b`,
		`\[[^\]]+\]\s+\[([A-Za-z0-9._-]+)\]`,
		`---\s+\[[^\]]*\]\s+\[([A-Za-z0-9._-]+)\]`,
		`([A-Za-z0-9._-]+)\[\d+\]:`,
		`\[([A-Za-z0-9._-]+)\]`,
	}
	m, errs := NewServiceMatcher(patterns)
	if len(errs) != 0 {
		t.Fatalf("sample patterns should compile cleanly, got %v", errs)
	}

	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"logfmt service=", `time=2026-05-02 level=error service=db-pool msg="connection refused"`, "db-pool"},
		{"logfmt svc=", `level=info svc=checkout-v2 user=42`, "checkout-v2"},
		{"logfmt app=", `app=api-gateway path=/healthz status=200`, "api-gateway"},
		{"json service", `{"@timestamp":"2026-05-02T00:00:00Z","service":"worker","msg":"panic"}`, "worker"},
		{"ECS service.name json", `{"service.name":"foo-svc","level":"error"}`, "foo-svc"},
		{"ECS service.name logfmt", `service.name=foo-svc level=error`, "foo-svc"},
		{"bracket prefix", `[scheduler] cron job failed`, "scheduler"},
		{"bracket with level prefix", `ERROR [scheduler] cron job failed`, "scheduler"},
		{"django two-bracket", `ERROR [django.request] [orders] Internal Server Error`, "orders"},
		{"spring boot", `ERROR 1 --- [main] [payments-service] c.e.p.PaymentCtrl : upstream error`, "payments-service"},
		{"spring boot console (ansi service before thread)", "2026-07-01 05:08:14.502  \x1b[34mlead-service\x1b[m  \x1b[33;1m[consumer-0-C-1]\x1b[m WARN k.c.NetworkClient : boom", "lead-service"},
		{"logback mdc bracket", `[ 2026-07-01 05:08:04:661 ] [ DEBUG ] [ account-service , requestID = , traceID = x ] rest`, "account-service"},
		{"syslog prefix", `nginx[1234]: GET /healthz 200`, "nginx"},
		{"syslog with level", `ERROR nginx[1234]: GET /healthz 500`, "nginx"},
		{"no match", `random unstructured line`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.Extract(tc.msg); got != tc.want {
				t.Errorf("Extract(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

func TestServiceMatcher_EmptyDisablesDetection(t *testing.T) {
	// No defaults: an empty/nil patterns list means "service detection off".
	for _, in := range [][]string{nil, {}, {""}} {
		m, errs := NewServiceMatcher(in)
		if len(errs) != 0 {
			t.Errorf("expected no errors for empty input %v, got %v", in, errs)
		}
		if got := m.Extract(`service=foo`); got != "" {
			t.Errorf("empty matcher should not extract anything, got %q", got)
		}
	}
}

func TestServiceMatcher_CustomPatterns(t *testing.T) {
	m, errs := NewServiceMatcher([]string{`pod=([a-z0-9-]+)`})
	if len(errs) != 0 {
		t.Fatalf("compile errs: %v", errs)
	}
	if got := m.Extract(`pod=worker-7 phase=running`); got != "worker-7" {
		t.Errorf("custom pattern failed: got %q", got)
	}
	// No fallback to defaults when custom list is provided.
	if got := m.Extract(`service=foo`); got != "" {
		t.Errorf("custom-only matcher should not fall back to defaults, got %q", got)
	}
}

func TestServiceMatcher_BadPattern(t *testing.T) {
	// One bad regex + one good one — bad is reported, good still works.
	m, errs := NewServiceMatcher([]string{`(unclosed`, `app=(\w+)`})
	if len(errs) != 1 {
		t.Fatalf("expected 1 compile error, got %d: %v", len(errs), errs)
	}
	if got := m.Extract(`app=foo`); got != "foo" {
		t.Errorf("good pattern should still match, got %q", got)
	}

	// Pattern without capture group is rejected.
	_, errs = NewServiceMatcher([]string{`app=\w+`})
	if len(errs) != 1 {
		t.Fatalf("expected 'missing capture group' error, got %v", errs)
	}
}
