package runbook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// embedBatchMax bounds how many runbook bodies are sent to the embedder
// in a single call, keeping request sizes sane for large corpora.
const embedBatchMax = 64

// contentHash returns the SHA-256 of a runbook's indexed text, used to
// detect unchanged runbooks and skip re-embedding them.
func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// frontMatter is the optional YAML front-matter an operator may put at
// the top of a runbook file (delimited by `---` lines) to enrich the
// record. Every field is optional.
type frontMatter struct {
	Title    string   `yaml:"title"`
	Service  string   `yaml:"service"`
	Services []string `yaml:"services"`
	Tags     []string `yaml:"tags"`
}

// IngestDir scans dir (recursively) for `*.md` runbook files, embeds
// each one through embedder, and writes the resulting records to store,
// then persists the corpus. It is the WRITE path — it lives here in
// pkg/runbook, OUTSIDE pkg/agent/ai/analyze/tools, so the analyze
// read-only import-graph guard stays green.
//
// Ingestion is incremental: a runbook whose indexed text is unchanged
// since the last ingest reuses its cached vector instead of calling the
// embedder, so re-running ingest (including the server's boot-time
// auto-ingest) over an unchanged corpus makes no embedding calls. A
// missing dir is treated as an empty corpus (0 ingested, no error).
//
// orgID scopes every ingested record (blank ⇒ storage.DefaultOrgID).
// Returns the number of records ingested. The embeddings made here are
// over operator-authored runbook content, the accepted trust boundary
// documented in the runbook-RAG docs.
func IngestDir(ctx context.Context, store *Store, embedder core.Embedder, dir, orgID string) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("runbook: nil store")
	}
	if embedder == nil {
		return 0, fmt.Errorf("runbook: nil embedder")
	}
	if dir == "" {
		return 0, fmt.Errorf("runbook: empty source dir")
	}
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		return 0, nil // no runbooks directory yet — nothing to ingest
	}
	orgID = storage.NormalizeOrgID(orgID)

	var items []ingestItem

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(path) // #nosec G304 — operator-supplied corpus dir
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = filepath.Base(path)
		}
		rec := parseRunbook(raw, rel, orgID)
		items = append(items, ingestItem{rec: rec, text: rec.Title + "\n\n" + rec.Body})
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("runbook: scan %q: %w", dir, err)
	}
	return ingestItems(ctx, store, embedder, items)
}

// ingestItem pairs a parsed runbook Record with the text that should be
// embedded for it (title + body).
type ingestItem struct {
	rec  Record
	text string
}

// ingestItems embeds (incrementally) and persists a batch of parsed
// runbook records, returning the number written. It is the shared core
// of both directory ingestion and UI upload.
//
// A runbook whose indexed text is unchanged since the last ingest reuses
// its cached vector instead of calling the embedder. When embedder is nil
// the records are still stored (so the corpus and the runbooks UI work
// without embeddings configured) but without vectors — they become
// searchable only once embeddings are configured and the corpus is
// re-ingested.
func ingestItems(ctx context.Context, store *Store, embedder core.Embedder, items []ingestItem) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("runbook: nil store")
	}
	if len(items) == 0 {
		return 0, nil
	}

	// Stamp each item with its content hash and reuse the cached vector
	// for any runbook whose indexed text is unchanged since last ingest,
	// so only new or edited runbooks reach the embedder.
	var toEmbed []int
	for i := range items {
		h := contentHash(items[i].text)
		items[i].rec.ContentHash = h
		if prev, ok := store.lookup(items[i].rec.ID); ok && prev.ContentHash == h && len(prev.Vector) > 0 {
			items[i].rec.Vector = prev.Vector
			continue
		}
		if embedder != nil {
			toEmbed = append(toEmbed, i)
		}
	}

	// Embed only the changed/new items, in bounded batches.
	for start := 0; start < len(toEmbed); start += embedBatchMax {
		end := start + embedBatchMax
		if end > len(toEmbed) {
			end = len(toEmbed)
		}
		batch := toEmbed[start:end]
		texts := make([]string, 0, len(batch))
		for _, idx := range batch {
			texts = append(texts, items[idx].text)
		}
		vecs, embErr := embedder.Embed(ctx, texts)
		if embErr != nil {
			return 0, fmt.Errorf("runbook: embed batch: %w", embErr)
		}
		if len(vecs) != len(batch) {
			return 0, fmt.Errorf("runbook: embedder returned %d vectors for %d inputs", len(vecs), len(batch))
		}
		for j, idx := range batch {
			items[idx].rec.Vector = vecs[j]
		}
	}

	for _, it := range items {
		store.Upsert(it.rec)
	}
	if persistErr := store.Persist(); persistErr != nil {
		return 0, persistErr
	}
	return len(items), nil
}

// parseRunbook turns one runbook file into a Record. It honours optional
// YAML front-matter (delimited by leading/closing `---` lines) for
// title/services/tags; otherwise it derives the title from the first
// Markdown H1 or the filename. rel is the path relative to the corpus
// root, used as both the stable ID and the Source.
func parseRunbook(raw []byte, rel, orgID string) Record {
	content := string(raw)
	fm, body := splitFrontMatter(content)

	title := strings.TrimSpace(fm.Title)
	if title == "" {
		title = firstHeading(body)
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	}

	services := make([]string, 0, len(fm.Services)+1)
	if fm.Service != "" {
		services = append(services, fm.Service)
	}
	for _, s := range fm.Services {
		if s != "" {
			services = append(services, s)
		}
	}

	return Record{
		ID:        filepath.ToSlash(rel),
		OrgID:     orgID,
		Title:     title,
		Services:  services,
		Tags:      fm.Tags,
		Body:      strings.TrimSpace(body),
		Source:    filepath.ToSlash(rel),
		UpdatedAt: time.Now().UTC(),
	}
}

// splitFrontMatter peels an optional leading YAML front-matter block
// (between an opening `---` line and the next `---` line) off the
// content, returning the parsed front-matter and the remaining body. A
// missing or malformed block yields a zero frontMatter and the original
// content unchanged.
func splitFrontMatter(content string) (frontMatter, string) {
	var fm frontMatter
	trimmed := strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return fm, content
	}
	rest := trimmed[strings.Index(trimmed, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, content
	}
	header := rest[:end]
	body := rest[end:]
	// Drop the closing delimiter line.
	if nl := strings.Index(body[1:], "\n"); nl >= 0 {
		body = body[1+nl+1:]
	} else {
		body = ""
	}
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return frontMatter{}, content
	}
	return fm, body
}

// firstHeading returns the text of the first Markdown H1 (`# ...`) line,
// or "" when there is none.
func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "# "))
		}
	}
	return ""
}
