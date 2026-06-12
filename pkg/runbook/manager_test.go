package runbook

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func newTestManager(t *testing.T, embedder *fakeEmbedder) (*Manager, storage.Provider) {
	t.Helper()
	prov := storage.NewMemory()
	s, err := LoadStore(prov)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	var emb *fakeEmbedder
	if embedder != nil {
		emb = embedder
	}
	if emb == nil {
		return NewManager(s, nil), prov
	}
	return NewManager(s, emb), prov
}

func TestManager_UploadListGetDelete(t *testing.T) {
	emb := &fakeEmbedder{}
	mgr, prov := newTestManager(t, emb)

	files := []UploadFile{
		{Name: "pool.md", Content: []byte("---\ntitle: Pool Exhaustion\nservice: api\n---\nRestart the pool.")},
		{Name: "disk.md", Content: []byte("# Disk Full\nFree space.")},
	}
	n, err := mgr.Upload(context.Background(), files, "")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if n != 2 {
		t.Fatalf("ingested %d, want 2", n)
	}
	if emb.count != 2 {
		t.Errorf("embedder saw %d, want 2", emb.count)
	}

	// List returns both, ordered by ID.
	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}

	// Get one returns the body + title.
	rec, err := mgr.Get("pool.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Title != "Pool Exhaustion" {
		t.Errorf("title = %q, want Pool Exhaustion", rec.Title)
	}
	if len(rec.Vector) == 0 {
		t.Error("uploaded record has no vector despite embedder")
	}

	// Persistence: reload the corpus from storage.
	reloaded, err := LoadStore(prov)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Len() != 2 {
		t.Errorf("reloaded Len = %d, want 2", reloaded.Len())
	}

	// Delete removes it and rebuilds the index.
	if err := mgr.Delete("pool.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := mgr.Get("pool.md"); err != ErrNotFound {
		t.Errorf("Get after delete err = %v, want ErrNotFound", err)
	}
	if got := len(mgr.List()); got != 1 {
		t.Errorf("List len after delete = %d, want 1", got)
	}
}

func TestManager_DeleteNotFound(t *testing.T) {
	mgr, _ := newTestManager(t, &fakeEmbedder{})
	if err := mgr.Delete("nope.md"); err != ErrNotFound {
		t.Errorf("Delete missing err = %v, want ErrNotFound", err)
	}
}

func TestManager_GetNotFound(t *testing.T) {
	mgr, _ := newTestManager(t, &fakeEmbedder{})
	if _, err := mgr.Get("nope.md"); err != ErrNotFound {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestManager_IndexSearchableAfterUpload(t *testing.T) {
	emb := &fakeEmbedder{}
	mgr, _ := newTestManager(t, emb)

	if _, err := mgr.Upload(context.Background(), []UploadFile{
		{Name: "api.md", Content: []byte("---\ntitle: API Restart\nservice: api\n---\nbody")},
	}, ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The fake embedder returns {1,0,0}; query with the same vector.
	hits := mgr.Index().Search([]float32{1, 0, 0}, "", 5)
	if len(hits) != 1 {
		t.Fatalf("search hits = %d, want 1", len(hits))
	}
	if hits[0].ID != "api.md" {
		t.Errorf("hit ID = %q, want api.md", hits[0].ID)
	}

	// A second upload is immediately searchable (live index swap).
	if _, err := mgr.Upload(context.Background(), []UploadFile{
		{Name: "db.md", Content: []byte("# DB\nbody")},
	}, ""); err != nil {
		t.Fatalf("Upload 2: %v", err)
	}
	if got := len(mgr.Index().Search([]float32{1, 0, 0}, "", 5)); got != 2 {
		t.Errorf("search hits after 2nd upload = %d, want 2", got)
	}
}

func TestManager_UploadWithoutEmbedder(t *testing.T) {
	mgr, _ := newTestManager(t, nil)
	if mgr.HasEmbedder() {
		t.Fatal("HasEmbedder = true, want false")
	}

	n, err := mgr.Upload(context.Background(), []UploadFile{
		{Name: "x.md", Content: []byte("# X\nbody")},
	}, "")
	if err != nil {
		t.Fatalf("Upload without embedder: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested %d, want 1", n)
	}

	// Stored but without a vector → not searchable yet.
	rec, err := mgr.Get("x.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Vector) != 0 {
		t.Errorf("record has a vector with no embedder configured")
	}
	if got := len(mgr.Index().Search([]float32{1, 0, 0}, "", 5)); got != 0 {
		t.Errorf("search hits = %d, want 0 (no vectors)", got)
	}
}

func TestManager_IngestDirNoopWithoutEmbedder(t *testing.T) {
	mgr, _ := newTestManager(t, nil)
	n, err := mgr.IngestDir(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("IngestDir: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 (no embedder)", n)
	}
}

func TestManager_UploadNameSanitizesAndAddsExt(t *testing.T) {
	mgr, _ := newTestManager(t, &fakeEmbedder{})
	// Path traversal + missing extension are normalized to a safe
	// basename with a .md suffix.
	if _, err := mgr.Upload(context.Background(), []UploadFile{
		{Name: "../../etc/passwd", Content: []byte("# Evil\nbody")},
		{Name: "noext", Content: []byte("# NoExt\nbody")},
	}, ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := mgr.Get("passwd.md"); err != nil {
		t.Errorf("expected sanitized id passwd.md, got err %v", err)
	}
	if _, err := mgr.Get("noext.md"); err != nil {
		t.Errorf("expected id noext.md, got err %v", err)
	}
}

func TestUploadName(t *testing.T) {
	cases := map[string]string{
		"plain.md":         "plain.md",
		"nested/dir/rb.md": "rb.md",
		"../../escape.md":  "escape.md",
		"noext":            "noext.md",
		"UPPER.MD":         "UPPER.MD",
		"":                 "runbook.md",
	}
	for in, want := range cases {
		if got := uploadName(in); got != want {
			t.Errorf("uploadName(%q) = %q, want %q", in, got, want)
		}
	}
}
