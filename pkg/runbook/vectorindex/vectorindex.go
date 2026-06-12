// Package vectorindex is the swappable vector-search seam behind the
// runbook-RAG find_runbook tool. The OSS build ships an in-memory cosine
// top-K index (Memory) that holds the whole runbook corpus in process —
// no external vector database, so an operator's data never leaves the
// box. The Index interface is the seam an enterprise hosted/scalable
// vector backend swaps in without forking the tool.
package vectorindex

import (
	"math"
	"sort"
	"strings"
)

// Doc is one indexed runbook excerpt plus its cached embedding vector.
// The vector is computed once at ingestion and reloaded at boot, so the
// index never needs to re-embed the corpus.
type Doc struct {
	ID      string
	Title   string
	Service string
	Source  string
	Excerpt string
	Vector  []float32
}

// Result is one search hit: the matched Doc plus its cosine similarity
// score in [-1, 1] (higher is more similar).
type Result struct {
	Doc
	Score float32
}

// Index is the read-only search seam the find_runbook tool depends on
// (via a local interface in the tools package). Implementations return
// the top-`limit` documents most similar to `query`, optionally
// restricted to a single service. A nil/empty index yields no results
// and no error.
type Index interface {
	Search(query []float32, service string, limit int) []Result
}

// Memory is the in-memory cosine top-K index. It is safe to build once
// at boot and read concurrently afterwards; it is not written to after
// construction (ingestion rewrites the backing store and the index is
// rebuilt at the next boot).
type Memory struct {
	docs    []Doc
	maxDocs int
}

const defaultMaxDocs = 5000

// NewMemory returns an empty in-memory index bounded at maxDocs
// documents (Add silently drops documents beyond the bound so a runaway
// corpus cannot exhaust memory). A non-positive maxDocs applies the
// built-in default.
func NewMemory(maxDocs int) *Memory {
	if maxDocs <= 0 {
		maxDocs = defaultMaxDocs
	}
	return &Memory{maxDocs: maxDocs}
}

// Add appends a document to the index. Documents with an empty vector
// are skipped (they could never match). Documents beyond the configured
// bound are dropped.
func (m *Memory) Add(d Doc) {
	if m == nil || len(d.Vector) == 0 {
		return
	}
	if len(m.docs) >= m.maxDocs {
		return
	}
	m.docs = append(m.docs, d)
}

// Len reports how many documents are indexed.
func (m *Memory) Len() int {
	if m == nil {
		return 0
	}
	return len(m.docs)
}

// Search implements Index. It scores every document by cosine
// similarity against query, optionally filters by service
// (case-insensitive exact match), sorts by descending score, and
// returns at most limit results. A non-positive limit applies a default
// of 5. An empty index or empty query yields no results.
func (m *Memory) Search(query []float32, service string, limit int) []Result {
	if m == nil || len(m.docs) == 0 || len(query) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 5
	}

	qn := norm(query)
	if qn == 0 {
		return nil
	}

	out := make([]Result, 0, len(m.docs))
	for _, d := range m.docs {
		if service != "" && !strings.EqualFold(d.Service, service) {
			continue
		}
		score := cosine(query, qn, d.Vector)
		out = append(out, Result{Doc: d, Score: score})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// cosine returns the cosine similarity of a and b. qn is the
// precomputed L2 norm of a (the query) so it is not recomputed per
// document. Vectors of differing length, or a zero-norm document, score
// 0 rather than panicking.
func cosine(a []float32, qn float32, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, bn float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		bn += float64(b[i]) * float64(b[i])
	}
	denom := float64(qn) * math.Sqrt(bn)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}

// norm returns the L2 norm of v.
func norm(v []float32) float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return float32(math.Sqrt(s))
}
