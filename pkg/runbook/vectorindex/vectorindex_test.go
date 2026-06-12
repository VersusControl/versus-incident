package vectorindex

import "testing"

func TestMemory_AddSkipsEmptyVectorAndBounds(t *testing.T) {
	m := NewMemory(2)
	m.Add(Doc{ID: "a", Vector: []float32{1, 0}})
	m.Add(Doc{ID: "empty"}) // no vector -> skipped
	m.Add(Doc{ID: "b", Vector: []float32{0, 1}})
	m.Add(Doc{ID: "c", Vector: []float32{1, 1}}) // beyond bound -> dropped
	if m.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (empty skipped, bound enforced)", m.Len())
	}
}

func TestMemory_NewMemoryDefaultBound(t *testing.T) {
	m := NewMemory(0)
	if m.maxDocs != defaultMaxDocs {
		t.Errorf("maxDocs = %d, want default %d", m.maxDocs, defaultMaxDocs)
	}
}

func TestMemory_SearchRanksByCosine(t *testing.T) {
	m := NewMemory(0)
	m.Add(Doc{ID: "near", Vector: []float32{1, 0, 0}})
	m.Add(Doc{ID: "ortho", Vector: []float32{0, 1, 0}})
	m.Add(Doc{ID: "far", Vector: []float32{-1, 0, 0}})

	res := m.Search([]float32{1, 0, 0}, "", 0)
	if len(res) != 3 {
		t.Fatalf("len(res) = %d, want 3", len(res))
	}
	if res[0].ID != "near" {
		t.Errorf("top hit = %q, want near", res[0].ID)
	}
	if res[2].ID != "far" {
		t.Errorf("worst hit = %q, want far", res[2].ID)
	}
	if res[0].Score <= res[1].Score || res[1].Score <= res[2].Score {
		t.Errorf("scores not descending: %v", []float32{res[0].Score, res[1].Score, res[2].Score})
	}
}

func TestMemory_SearchServiceFilter(t *testing.T) {
	m := NewMemory(0)
	m.Add(Doc{ID: "api", Service: "API", Vector: []float32{1, 0}})
	m.Add(Doc{ID: "db", Service: "db", Vector: []float32{1, 0}})

	res := m.Search([]float32{1, 0}, "api", 0) // case-insensitive
	if len(res) != 1 || res[0].ID != "api" {
		t.Fatalf("res = %+v, want only api", res)
	}
}

func TestMemory_SearchLimit(t *testing.T) {
	m := NewMemory(0)
	for _, id := range []string{"a", "b", "c", "d"} {
		m.Add(Doc{ID: id, Vector: []float32{1, 0}})
	}
	if got := len(m.Search([]float32{1, 0}, "", 2)); got != 2 {
		t.Errorf("len = %d, want 2 (limit)", got)
	}
}

func TestMemory_SearchEdgeCases(t *testing.T) {
	m := NewMemory(0)
	m.Add(Doc{ID: "a", Vector: []float32{1, 0}})

	if res := m.Search(nil, "", 5); res != nil {
		t.Errorf("empty query: res = %v, want nil", res)
	}
	if res := m.Search([]float32{0, 0}, "", 5); res != nil {
		t.Errorf("zero-norm query: res = %v, want nil", res)
	}
	var nilIdx *Memory
	if res := nilIdx.Search([]float32{1, 0}, "", 5); res != nil {
		t.Errorf("nil index: res = %v, want nil", res)
	}
}

func TestMemory_SatisfiesIndex(t *testing.T) {
	var _ Index = NewMemory(0)
}

func TestCosine_LengthMismatch(t *testing.T) {
	got := cosine([]float32{1, 0}, norm([]float32{1, 0}), []float32{1, 0, 0})
	if got != 0 {
		t.Errorf("cosine(mismatched lengths) = %v, want 0", got)
	}
}
