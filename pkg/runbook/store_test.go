package runbook

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestStore_UpsertNormalizesOrgIDAndTime(t *testing.T) {
	s, err := LoadStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	s.Upsert(Record{ID: "r1", Title: "t"}) // blank org, zero time
	got := s.All()
	if len(got) != 1 {
		t.Fatalf("Len = %d, want 1", len(got))
	}
	if got[0].OrgID != storage.DefaultOrgID {
		t.Errorf("OrgID = %q, want %q", got[0].OrgID, storage.DefaultOrgID)
	}
	if got[0].UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped")
	}
}

func TestStore_UpsertSkipsBlankID(t *testing.T) {
	s, _ := LoadStore(storage.NewMemory())
	s.Upsert(Record{ID: "", Title: "no id"})
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0 (blank id skipped)", s.Len())
	}
}

func TestStore_AllSortedByID(t *testing.T) {
	s, _ := LoadStore(storage.NewMemory())
	s.Upsert(Record{ID: "c"})
	s.Upsert(Record{ID: "a"})
	s.Upsert(Record{ID: "b"})
	got := s.All()
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Errorf("order = %q,%q,%q, want a,b,c", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestStore_PersistAndReload(t *testing.T) {
	prov := storage.NewMemory()
	s, _ := LoadStore(prov)
	s.Upsert(Record{
		ID:        "r1",
		Title:     "Pool exhaustion",
		Services:  []string{"api"},
		Tags:      []string{"db"},
		Body:      "restart the pool",
		Source:    "r1.md",
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
		Vector:    []float32{0.1, 0.2},
	})
	if err := s.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	reloaded, err := LoadStore(prov)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded Len = %d, want 1", reloaded.Len())
	}
	r := reloaded.All()[0]
	if r.Title != "Pool exhaustion" || len(r.Vector) != 2 || r.Vector[1] != 0.2 {
		t.Errorf("roundtrip mismatch: %+v", r)
	}
	if r.OrgID != storage.DefaultOrgID {
		t.Errorf("OrgID = %q, want %q after reload", r.OrgID, storage.DefaultOrgID)
	}
}

func TestStore_PersistNilProviderNoop(t *testing.T) {
	s, _ := LoadStore(nil)
	s.Upsert(Record{ID: "r1"})
	if err := s.Persist(); err != nil {
		t.Errorf("Persist on memory-only store: %v", err)
	}
}

func TestStore_BuildIndexSkipsVectorless(t *testing.T) {
	s, _ := LoadStore(storage.NewMemory())
	s.Upsert(Record{ID: "withvec", Title: "t", Services: []string{"api"}, Body: "body", Vector: []float32{1, 0}})
	s.Upsert(Record{ID: "novec", Title: "t2", Body: "body2"}) // no vector -> skipped

	idx := s.BuildIndex(0)
	if idx.Len() != 1 {
		t.Fatalf("index Len = %d, want 1 (vectorless skipped)", idx.Len())
	}
	res := idx.Search([]float32{1, 0}, "", 5)
	if len(res) != 1 || res[0].ID != "withvec" {
		t.Errorf("search = %+v, want only withvec", res)
	}
	if res[0].Service != "api" {
		t.Errorf("primary service = %q, want api", res[0].Service)
	}
}

func TestStore_LoadEmpty(t *testing.T) {
	s, err := LoadStore(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadStore empty: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestExcerptTrimsToRuneBound(t *testing.T) {
	long := make([]rune, excerptMaxRunes+50)
	for i := range long {
		long[i] = 'x'
	}
	if got := excerpt(string(long)); len([]rune(got)) != excerptMaxRunes {
		t.Errorf("excerpt len = %d, want %d", len([]rune(got)), excerptMaxRunes)
	}
	short := "short body"
	if got := excerpt(short); got != short {
		t.Errorf("excerpt(short) = %q, want unchanged", got)
	}
}
