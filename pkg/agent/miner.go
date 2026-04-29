package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
)

// Miner is a small, dependency-free, Drain-style log clusterer.
//
// The classic Drain algorithm uses a fixed-depth prefix tree where the first
// few tokens index into buckets, and each leaf holds a list of templates. To
// classify an incoming log line:
//
//  1. Tokenize the message and replace tokens that look like variables
//     (numbers, IPs, UUIDs, hex, IDs) with the wildcard `<*>` so that
//     "user_id=42" and "user_id=99" share a structural shape.
//  2. Walk the tree using `len(tokens)` as the first bucket key (so messages
//     of different lengths never collide), then the first N non-wildcard
//     tokens as inner bucket keys.
//  3. At the leaf, pick the existing template with the highest token-by-token
//     similarity. If similarity ≥ threshold, merge (replace differing tokens
//     with `<*>`) and return that template's ID. Otherwise, register a new
//     template.
//
// This implementation is intentionally simpler than full Drain3 (no
// parameter masking learning, no parser tree compaction) but covers the
// 95% case for production logs. ADR-0001 tracks the longer-term plan.
type Miner struct {
	mu                  sync.Mutex
	tree                map[int]*minerNode // first key: token count
	depth               int                // tree depth (default 4)
	maxChildren         int
	similarityThreshold float64
	clusters            map[string]*MinerCluster // patternID → cluster
}

type minerNode struct {
	children map[string]*minerNode
	clusters []*MinerCluster
}

// MinerCluster is one learned log template plus its observation count.
type MinerCluster struct {
	ID     string   // "p-<sha1[:12]>"
	Tokens []string // current template tokens (with `<*>` for variables)
	Size   int      // total observations matched into this cluster
}

// NewMiner builds a Miner from config. Sensible defaults are applied when
// values are zero.
func NewMiner(similarityThreshold float64, depth, maxChildren int) *Miner {
	if similarityThreshold <= 0 {
		similarityThreshold = 0.4
	}
	if depth <= 0 {
		depth = 4
	}
	if maxChildren <= 0 {
		maxChildren = 100
	}
	return &Miner{
		tree:                make(map[int]*minerNode),
		depth:               depth,
		maxChildren:         maxChildren,
		similarityThreshold: similarityThreshold,
		clusters:            make(map[string]*MinerCluster),
	}
}

// AddCluster pre-registers a known cluster (used when loading a catalog from
// disk so mining can resume against existing patterns).
func (m *Miner) AddCluster(id string, template string, size int) {
	tokens := tokenize(template)
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := &MinerCluster{ID: id, Tokens: tokens, Size: size}
	m.clusters[id] = cl
	m.indexCluster(cl)
}

// Cluster classifies a single message and returns the matched (or newly
// created) cluster ID, the (post-merge) template tokens joined by space,
// and a flag indicating whether this was the first time we saw the pattern.
func (m *Miner) Cluster(message string) (id string, template string, isNew bool) {
	tokens := tokenize(message)
	if len(tokens) == 0 {
		return "", "", false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	leaf := m.descend(tokens, true)
	best := -1.0
	var match *MinerCluster
	for _, c := range leaf.clusters {
		sim := tokenSimilarity(c.Tokens, tokens)
		if sim > best {
			best = sim
			match = c
		}
	}
	if match != nil && best >= m.similarityThreshold {
		// merge
		match.Tokens = mergeTemplates(match.Tokens, tokens)
		match.Size++
		return match.ID, strings.Join(match.Tokens, " "), false
	}

	// new cluster
	cl := &MinerCluster{
		ID:     newPatternID(tokens),
		Tokens: append([]string(nil), tokens...),
		Size:   1,
	}
	leaf.clusters = append(leaf.clusters, cl)
	if len(leaf.clusters) > m.maxChildren {
		// LRU-ish eviction: drop the smallest-Size cluster
		minIdx := 0
		for i, c := range leaf.clusters {
			if c.Size < leaf.clusters[minIdx].Size {
				minIdx = i
			}
		}
		evicted := leaf.clusters[minIdx]
		leaf.clusters = append(leaf.clusters[:minIdx], leaf.clusters[minIdx+1:]...)
		delete(m.clusters, evicted.ID)
	}
	m.clusters[cl.ID] = cl
	return cl.ID, strings.Join(cl.Tokens, " "), true
}

// Snapshot returns a copy of every learned cluster — used for catalog
// persistence and admin endpoints.
func (m *Miner) Snapshot() []MinerCluster {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MinerCluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, MinerCluster{
			ID:     c.ID,
			Tokens: append([]string(nil), c.Tokens...),
			Size:   c.Size,
		})
	}
	return out
}

// descend walks (and lazily builds) the bucket tree to a leaf.
func (m *Miner) descend(tokens []string, create bool) *minerNode {
	n := m.tree[len(tokens)]
	if n == nil {
		if !create {
			return nil
		}
		n = &minerNode{children: make(map[string]*minerNode)}
		m.tree[len(tokens)] = n
	}
	for i := 0; i < m.depth && i < len(tokens); i++ {
		key := tokens[i]
		if isWildcard(key) || isVariable(key) {
			key = "<*>"
		}
		child := n.children[key]
		if child == nil {
			if !create {
				return n
			}
			child = &minerNode{children: make(map[string]*minerNode)}
			n.children[key] = child
		}
		n = child
	}
	return n
}

// indexCluster places an existing cluster into the tree (used by AddCluster).
func (m *Miner) indexCluster(c *MinerCluster) {
	leaf := m.descend(c.Tokens, true)
	leaf.clusters = append(leaf.clusters, c)
}

// -----------------------------------------------------------------------------
// Tokenization & similarity helpers
// -----------------------------------------------------------------------------

// Variable detectors used during tokenization. Anything matching becomes <*>.
var (
	reNumber = regexp.MustCompile(`^[\-+]?[0-9]+(\.[0-9]+)?$`)
	reHex    = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	reUUID   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reIPv4   = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}(:\d+)?$`)
	reLong   = regexp.MustCompile(`^[A-Fa-f0-9]{16,}$`) // long hashes / IDs
	// REDACTED tokens emitted by the redactor are treated as wildcards.
	reRedacted = regexp.MustCompile(`^<REDACTED:[^>]+>$`)
)

// tokenize splits on whitespace and a small set of delimiters that frequently
// glue variables to keywords (`=`, `:`, `,`, parens, brackets) and replaces
// variable-looking tokens with `<*>`.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	s = stripControlChars(s)
	// Insert spaces around common delimiters so the splitter sees them.
	for _, d := range []string{"=", ",", "(", ")", "[", "]", "{", "}", "\""} {
		s = strings.ReplaceAll(s, d, " "+d+" ")
	}
	raw := strings.Fields(s)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if isVariable(t) {
			out = append(out, "<*>")
			continue
		}
		out = append(out, t)
	}
	return out
}

func isVariable(t string) bool {
	if t == "" {
		return false
	}
	if reRedacted.MatchString(t) {
		return true
	}
	if reNumber.MatchString(t) || reHex.MatchString(t) || reUUID.MatchString(t) ||
		reIPv4.MatchString(t) || reLong.MatchString(t) {
		return true
	}
	return false
}

func isWildcard(t string) bool { return t == "<*>" }

// tokenSimilarity returns the fraction of positions where two token slices
// of equal length agree (wildcards count as a match against anything).
func tokenSimilarity(a, b []string) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	matches := 0
	for i := range a {
		if a[i] == b[i] || isWildcard(a[i]) || isWildcard(b[i]) {
			matches++
		}
	}
	return float64(matches) / float64(len(a))
}

// mergeTemplates returns a new template where positions that disagree become
// `<*>`. Length must match; caller guarantees this via the bucket tree.
func mergeTemplates(existing, incoming []string) []string {
	out := make([]string, len(existing))
	for i := range existing {
		if existing[i] == incoming[i] {
			out[i] = existing[i]
		} else {
			out[i] = "<*>"
		}
	}
	return out
}

// newPatternID derives a stable-ish ID for a freshly-minted cluster from the
// initial token list. Stability isn't required (the catalog stores it), but a
// content-derived ID makes catalog diffs easier to read across runs.
func newPatternID(tokens []string) string {
	h := sha1.New()
	h.Write([]byte(strings.Join(tokens, " ")))
	return "p-" + hex.EncodeToString(h.Sum(nil))[:12]
}
