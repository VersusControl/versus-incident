package runbook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fakeEmbedder returns a fixed-dimension vector per input and records
// how many texts it was asked to embed.
type fakeEmbedder struct{ count int }

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.count += len(texts)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestIngestDir_ParsesAndPersists(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "frontmatter.md", "---\ntitle: Pool Exhaustion\nservice: api\ntags:\n  - db\n  - urgent\n---\n# Ignored Heading\nRestart the pool.\n")
	writeFile(t, dir, "heading.md", "# Disk Full\nFree up disk space.\n")
	writeFile(t, dir, "nested/plain.md", "no heading body only")
	writeFile(t, dir, "ignore.txt", "not markdown")

	prov := storage.NewMemory()
	s, _ := LoadStore(prov)
	emb := &fakeEmbedder{}

	n, err := IngestDir(context.Background(), s, emb, dir, "")
	if err != nil {
		t.Fatalf("IngestDir: %v", err)
	}
	if n != 3 {
		t.Fatalf("ingested %d, want 3 (txt ignored)", n)
	}
	if emb.count != 3 {
		t.Errorf("embedder saw %d texts, want 3", emb.count)
	}

	// Reload from storage to confirm persistence.
	reloaded, err := LoadStore(prov)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Len() != 3 {
		t.Fatalf("reloaded Len = %d, want 3", reloaded.Len())
	}

	byID := map[string]Record{}
	for _, r := range reloaded.All() {
		byID[r.ID] = r
	}

	fm := byID["frontmatter.md"]
	if fm.Title != "Pool Exhaustion" {
		t.Errorf("frontmatter title = %q, want Pool Exhaustion", fm.Title)
	}
	if len(fm.Services) != 1 || fm.Services[0] != "api" {
		t.Errorf("frontmatter services = %v, want [api]", fm.Services)
	}
	if len(fm.Tags) != 2 {
		t.Errorf("frontmatter tags = %v, want 2", fm.Tags)
	}
	if len(fm.Vector) == 0 {
		t.Error("frontmatter record has no vector")
	}
	if fm.OrgID != storage.DefaultOrgID {
		t.Errorf("OrgID = %q, want default", fm.OrgID)
	}

	if h := byID["heading.md"]; h.Title != "Disk Full" {
		t.Errorf("heading title = %q, want Disk Full (from H1)", h.Title)
	}

	// nested path becomes a slash ID; title falls back to filename.
	plain, ok := byID["nested/plain.md"]
	if !ok {
		t.Fatalf("nested record missing; ids = %v", byID)
	}
	if plain.Title != "plain" {
		t.Errorf("plain title = %q, want plain (filename fallback)", plain.Title)
	}
}

func TestIngestDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadStore(storage.NewMemory())
	n, err := IngestDir(context.Background(), s, &fakeEmbedder{}, dir, "")
	if err != nil {
		t.Fatalf("IngestDir empty: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestIngestDir_MissingDir(t *testing.T) {
	s, _ := LoadStore(storage.NewMemory())
	n, err := IngestDir(context.Background(), s, &fakeEmbedder{}, filepath.Join(t.TempDir(), "does-not-exist"), "")
	if err != nil {
		t.Fatalf("IngestDir missing dir: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestIngestDir_Incremental(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# A\nbody a")
	writeFile(t, dir, "b.md", "# B\nbody b")

	prov := storage.NewMemory()
	s, _ := LoadStore(prov)
	emb := &fakeEmbedder{}

	if _, err := IngestDir(context.Background(), s, emb, dir, ""); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if emb.count != 2 {
		t.Fatalf("first ingest embedded %d, want 2", emb.count)
	}

	// Re-ingest unchanged corpus from a freshly reloaded store: no
	// embedding calls because both runbooks reuse their cached vector.
	s2, _ := LoadStore(prov)
	emb2 := &fakeEmbedder{}
	if _, err := IngestDir(context.Background(), s2, emb2, dir, ""); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if emb2.count != 0 {
		t.Errorf("unchanged re-ingest embedded %d texts, want 0", emb2.count)
	}

	// Edit one runbook: only the changed file is re-embedded.
	writeFile(t, dir, "b.md", "# B\nbody b EDITED")
	s3, _ := LoadStore(prov)
	emb3 := &fakeEmbedder{}
	if _, err := IngestDir(context.Background(), s3, emb3, dir, ""); err != nil {
		t.Fatalf("third ingest: %v", err)
	}
	if emb3.count != 1 {
		t.Errorf("edited re-ingest embedded %d texts, want 1", emb3.count)
	}
}

func TestIngestDir_Guards(t *testing.T) {
	s, _ := LoadStore(storage.NewMemory())
	if _, err := IngestDir(context.Background(), nil, &fakeEmbedder{}, "x", ""); err == nil {
		t.Error("nil store: want error")
	}
	if _, err := IngestDir(context.Background(), s, nil, "x", ""); err == nil {
		t.Error("nil embedder: want error")
	}
	if _, err := IngestDir(context.Background(), s, &fakeEmbedder{}, "", ""); err == nil {
		t.Error("empty dir: want error")
	}
}

func TestIngestDir_OrgIDScope(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# A\nbody")
	s, _ := LoadStore(storage.NewMemory())
	if _, err := IngestDir(context.Background(), s, &fakeEmbedder{}, dir, "acme"); err != nil {
		t.Fatalf("IngestDir: %v", err)
	}
	if s.All()[0].OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme", s.All()[0].OrgID)
	}
}
