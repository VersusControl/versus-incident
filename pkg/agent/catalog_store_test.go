package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// catalog_store_test.go — proves the CatalogStore seam: nil-default is the
// inline whole-blob path byte-for-byte, and an installed store is consulted at
// exactly the routed call sites while the brain hot path never touches it.

// fakeCatalogStore records which seam methods were invoked and serves a fixed
// working-set / read view. It is the stand-in for the enterprise strategy.
type fakeCatalogStore struct {
	mu       sync.Mutex
	loads    int
	persists int
	snaps    int
	curates  []CatalogEdit

	patterns map[string]*Pattern
	services map[string]*ServiceInfo
}

func (f *fakeCatalogStore) Load() (map[string]*Pattern, map[string]*ServiceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	return f.patterns, f.services, nil
}

func (f *fakeCatalogStore) Persist(patterns map[string]*Pattern, services map[string]*ServiceInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persists++
	return nil
}

func (f *fakeCatalogStore) Snapshot() ([]*Pattern, map[string]ServiceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps++
	out := make([]*Pattern, 0, len(f.patterns))
	for _, p := range f.patterns {
		cp := *p
		out = append(out, &cp)
	}
	svc := make(map[string]ServiceInfo, len(f.services))
	for k, v := range f.services {
		svc[k] = *v
	}
	return out, svc, nil
}

func (f *fakeCatalogStore) Curate(edit CatalogEdit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.curates = append(f.curates, edit)
	return nil
}

func (f *fakeCatalogStore) counts() (loads, persists, snaps, curates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads, f.persists, f.snaps, len(f.curates)
}

// TestCatalog_NilStore_GoldenBlobUnchanged proves that with NO store installed
// the load/persist/list/label cycle runs the exact inline whole-blob path: the
// persisted "patterns" blob is the canonical catalogFile encoding and reloads
// byte-for-byte to the same working set.
func TestCatalog_NilStore_GoldenBlobUnchanged(t *testing.T) {
	// Defensive: no store installed (the default). Guard against leakage from
	// any other test in the package.
	SetCatalogStore(nil)

	store := newTestStore(t)
	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	cat.Upsert("p-aaa", "user <*> failed login", "src1", 5, 0.2, "auth-rule", "auth")
	cat.Upsert("p-bbb", "connection refused <*>", "src1", 12, 0.2, "", "")
	cat.RegisterService("auth")
	if !cat.Label("p-aaa", "known", []string{"reviewed"}) {
		t.Fatalf("Label should succeed for existing pattern")
	}
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Golden-blob assertion: the persisted bytes are the canonical encoding of
	// a catalogFile — re-marshalling the parsed file reproduces them exactly.
	raw, err := store.ReadBlob("patterns")
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	var parsed catalogFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	reMarshalled, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(reMarshalled) != string(raw) {
		t.Fatalf("persisted blob is not the canonical catalogFile encoding:\n got: %s\nwant: %s", raw, reMarshalled)
	}
	if parsed.Version != catalogFileVersion {
		t.Fatalf("blob version = %d, want %d", parsed.Version, catalogFileVersion)
	}
	if len(parsed.Patterns) != 2 || len(parsed.Services) != 1 {
		t.Fatalf("blob shape changed: patterns=%d services=%d", len(parsed.Patterns), len(parsed.Services))
	}
	if got := parsed.Patterns["p-aaa"]; got == nil || got.Verdict != "known" || got.Count != 5 {
		t.Fatalf("p-aaa not persisted verbatim: %+v", got)
	}

	// Round-trip: a fresh load yields the same working set.
	reloaded, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Len() != 2 {
		t.Fatalf("reloaded patterns = %d, want 2", reloaded.Len())
	}
	if p := reloaded.Get("p-aaa"); p == nil || p.Verdict != "known" || p.RuleName != "auth-rule" {
		t.Fatalf("reloaded p-aaa mismatch: %+v", p)
	}
}

// TestCatalog_InstalledStore_RoutesAtCallSites proves an installed store is
// consulted at exactly the boot-load / persist / bulk-read / curation sites.
func TestCatalog_InstalledStore_RoutesAtCallSites(t *testing.T) {
	fake := &fakeCatalogStore{
		patterns: map[string]*Pattern{
			"p-seed": {ID: "p-seed", Template: "seeded <*>", Count: 7},
		},
		services: map[string]*ServiceInfo{
			"svc-seed": {FirstSeen: time.Unix(1000, 0).UTC()},
		},
	}
	SetCatalogStore(fake)
	t.Cleanup(func() { SetCatalogStore(nil) })

	// Boot load routes through Load: the passed storage.Provider is ignored.
	cat, err := LoadCatalog(nil)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if loads, _, _, _ := fake.counts(); loads != 1 {
		t.Fatalf("Load calls = %d, want 1", loads)
	}
	if cat.Len() != 1 || cat.Get("p-seed") == nil {
		t.Fatalf("working set not seeded from store.Load")
	}

	// Mutate in memory (hot path) then Persist — Persist routes through the store.
	cat.Upsert("p-new", "new <*>", "src", 3, 0.2, "", "")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, persists, _, _ := fake.counts(); persists != 1 {
		t.Fatalf("Persist calls = %d, want 1", persists)
	}

	// Bulk reads route through Snapshot.
	_ = cat.All()
	_ = cat.AllServices()
	if _, _, snaps, _ := fake.counts(); snaps != 2 {
		t.Fatalf("Snapshot calls = %d, want 2 (All + AllServices)", snaps)
	}

	// Curation routes through Curate — one per call, correct Kind/fields.
	cat.Label("p-seed", "known", []string{"t1"})
	cat.MarkKnown("p-seed")
	cat.Delete("p-seed")
	cat.EndServiceGrace("svc-seed")
	cat.RestartServiceGrace("svc-seed")

	fake.mu.Lock()
	edits := append([]CatalogEdit(nil), fake.curates...)
	fake.mu.Unlock()
	if len(edits) != 5 {
		t.Fatalf("Curate calls = %d, want 5", len(edits))
	}
	want := []CatalogEdit{
		{Kind: CatalogEditLabel, PatternID: "p-seed", Verdict: "known", Tags: []string{"t1"}},
		{Kind: CatalogEditMarkKnown, PatternID: "p-seed"},
		{Kind: CatalogEditDelete, PatternID: "p-seed"},
		{Kind: CatalogEditEndServiceGrace, Service: "svc-seed"},
		{Kind: CatalogEditRestartServiceGrace, Service: "svc-seed"},
	}
	for i, w := range want {
		if edits[i].Kind != w.Kind || edits[i].PatternID != w.PatternID ||
			edits[i].Verdict != w.Verdict || edits[i].Service != w.Service {
			t.Fatalf("edit[%d] = %+v, want %+v", i, edits[i], w)
		}
	}
	if len(edits[0].Tags) != 1 || edits[0].Tags[0] != "t1" {
		t.Fatalf("label edit tags = %v, want [t1]", edits[0].Tags)
	}
}

// TestCatalog_InstalledStore_HotPathNeverCallsStore proves Get/Upsert and the
// log brain's Expected stay in-memory and partition-local: they never consult
// the store.
func TestCatalog_InstalledStore_HotPathNeverCallsStore(t *testing.T) {
	fake := &fakeCatalogStore{
		patterns: map[string]*Pattern{
			"p1": {ID: "p1", Template: "tpl <*>", Count: 2, BaselineFrequency: 4},
		},
	}
	SetCatalogStore(fake)
	t.Cleanup(func() { SetCatalogStore(nil) })

	cat, err := LoadCatalog(nil)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Snapshot of counters after the single boot Load.
	loads0, persists0, snaps0, curates0 := fake.counts()

	// Hot path: Upsert, Get, and the log brain's Expected.
	cat.Upsert("p1", "tpl <*>", "src", 1, 0.2, "", "")
	cat.Upsert("p2", "other <*>", "src", 1, 0.2, "", "")
	_ = cat.Get("p1")
	_ = cat.Get("missing")

	brain := newLogBrain("src", nil, cat, nil, nil, 0.2, config.AgentCatalogConfig{})
	_, _, _ = brain.Expected(context.Background(), "p1", time.Now())
	_, _, _ = brain.Expected(context.Background(), "missing", time.Now())

	loads1, persists1, snaps1, curates1 := fake.counts()
	if loads1 != loads0 || persists1 != persists0 || snaps1 != snaps0 || curates1 != curates0 {
		t.Fatalf("hot path touched the store: before=(%d,%d,%d,%d) after=(%d,%d,%d,%d)",
			loads0, persists0, snaps0, curates0, loads1, persists1, snaps1, curates1)
	}
}

// TestSetCatalogStore_NilIsDefault proves the installer's nil-clear restores
// the inline path (the default community guarantee).
func TestSetCatalogStore_NilIsDefault(t *testing.T) {
	fake := &fakeCatalogStore{}
	SetCatalogStore(fake)
	if catalogStore() == nil {
		t.Fatalf("store not installed")
	}
	SetCatalogStore(nil)
	if catalogStore() != nil {
		t.Fatalf("nil did not clear the slot")
	}
}
