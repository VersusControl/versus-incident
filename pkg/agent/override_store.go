package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// override_store.go — the concrete OSS manual-attribution override store.
//
// It is the reusable "override logic" the ServiceOverride seam points at: a
// per-org set of override rules (log | metric | trace), persisted through the
// storage.Provider (never os.WriteFile), matched on the hot path with a set
// lookup + compiled-pattern scan. OSS installs ONE instance at boot as the
// process-wide ServiceOverride, so logs override works with no enterprise
// present; the enterprise metric/trace brains resolve through the SAME
// installed instance (agent.ResolveServiceOverride), so there is no duplicated
// override logic and no enterprise import into OSS.
//
// Tenant isolation is structural: every rule carries an OrgID and every
// read/write filters on it, so org A's rules can never resolve for org B. A
// single-tenant OSS deployment keys everything under storage.DefaultOrgID
// exactly like the pattern catalog; a multi-tenant consumer stamps the org
// onto the worker's Run context with ContextWithOverrideOrg.

// Override source types. A rule only matches an input of the SAME type, so a
// metric override can never re-label a log and vice-versa.
const (
	OverrideSourceLog    = "log"
	OverrideSourceMetric = "metric"
	OverrideSourceTrace  = "trace"
)

const (
	// overrideBlobName is the storage.Provider blob the rule set persists to.
	overrideBlobName = "service-overrides"
	// overrideFileVersion versions the on-disk schema.
	overrideFileVersion = 1
	// overrideMaxRulesPerOrg bounds one org's rule set so a runaway API caller
	// can never grow the blob (or the in-memory cache) without limit.
	overrideMaxRulesPerOrg = 5000
	// overrideMaxMatchLen / overrideMaxServiceLen bound one entry's length.
	overrideMaxMatchLen   = 512
	overrideMaxServiceLen = 256
)

// OverrideRule maps a signal to an operator-chosen service. It is one durable
// correction: the match is a source-appropriate key (a log Pattern identity or
// message substring; a metric/trace signal name — exact or `*`/`?` glob) and
// Service is the attribution it forces.
type OverrideRule struct {
	// ID is a stable, server-assigned identifier used for delete addressing.
	ID string `json:"id"`
	// OrgID scopes the rule to one organization. Defaults to
	// storage.DefaultOrgID for single-tenant OSS.
	OrgID string `json:"org_id,omitempty"`
	// SourceType is one of OverrideSourceLog / OverrideSourceMetric /
	// OverrideSourceTrace.
	SourceType string `json:"source_type"`
	// Match is the source-appropriate match key: a log pattern id / message
	// substring, or a metric/trace signal name (exact or `*`/`?` glob).
	Match string `json:"match"`
	// Service is the operator-chosen attribution this rule forces.
	Service string `json:"service"`
	// CreatedAt is when the rule was created.
	CreatedAt time.Time `json:"created_at"`
	// CreatedBy is a best-effort actor label for the audit seam (empty in OSS).
	CreatedBy string `json:"created_by,omitempty"`
}

// overrideFile is the on-disk schema: every org's rules in one versioned blob.
type overrideFile struct {
	Version   int            `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
	Rules     []OverrideRule `json:"rules"`
}

// overrideOrgCtxKey carries the deployment org into ResolveService for a
// multi-tenant consumer. The OSS worker passes its Run ctx straight through, so
// a consumer stamps the org with ContextWithOverrideOrg before the worker
// starts; the OSS single-tenant path carries none and falls back to
// storage.DefaultOrgID.
type overrideOrgCtxKey struct{}

// ContextWithOverrideOrg returns a copy of ctx carrying org for the worker's
// Run context. ResolveService reads it; absent, it resolves the default org.
func ContextWithOverrideOrg(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, overrideOrgCtxKey{}, org)
}

// overrideOrgFromContext extracts the org stamped by ContextWithOverrideOrg,
// falling back to storage.DefaultOrgID so the single-tenant OSS path resolves
// under the same org the pattern catalog uses.
func overrideOrgFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(overrideOrgCtxKey{}).(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return storage.NormalizeOrgID(s)
			}
		}
	}
	return storage.DefaultOrgID
}

// ServiceOverrideStore is the process-wide, per-org override rule store. It
// implements the ServiceOverride seam (ResolveService) AND the CRUD the admin
// API drives (List / Put / Delete). All methods are safe for concurrent use.
type ServiceOverrideStore struct {
	mu       sync.RWMutex
	store    storage.Provider
	blobName string
	// rules is org -> (id -> rule). Compiled globs are rebuilt lazily per
	// resolve; the rule set is small and mutated at admin frequency only.
	rules map[string]map[string]*OverrideRule
}

// compile-time proof the store satisfies the seam.
var _ ServiceOverride = (*ServiceOverrideStore)(nil)

// LoadServiceOverrideStore opens the override blob from the provider (or starts
// empty when none exists / store is nil). A parse error returns the empty store
// plus the error so boot can log-and-continue, exactly like LoadCatalog.
func LoadServiceOverrideStore(store storage.Provider) (*ServiceOverrideStore, error) {
	s := &ServiceOverrideStore{
		store:    store,
		blobName: overrideBlobName,
		rules:    make(map[string]map[string]*OverrideRule),
	}
	if store == nil {
		return s, nil
	}
	data, err := store.ReadBlob(s.blobName)
	if err != nil {
		return s, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var f overrideFile
	if err := json.Unmarshal(data, &f); err != nil {
		return s, fmt.Errorf("parse service overrides: %w", err)
	}
	for i := range f.Rules {
		r := f.Rules[i]
		org := storage.NormalizeOrgID(r.OrgID)
		r.OrgID = org
		if s.rules[org] == nil {
			s.rules[org] = make(map[string]*OverrideRule)
		}
		cp := r
		s.rules[org][r.ID] = &cp
	}
	return s, nil
}

// ResolveService implements ServiceOverride. It returns the operator-chosen
// service for the first rule that matches in.SourceType + match key, or
// ("", false) when none applies. HOT PATH: an RLock read of the org's rule set
// (no storage I/O) + a match scan. The org is deployment-supplied via ctx
// (ContextWithOverrideOrg), never caller-supplied.
func (s *ServiceOverrideStore) ResolveService(ctx context.Context, in ServiceOverrideInput) (string, bool) {
	if s == nil {
		return "", false
	}
	org := overrideOrgFromContext(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.rules[org] {
		if overrideMatches(r, in) {
			return r.Service, true
		}
	}
	return "", false
}

// List returns a stable, sorted snapshot of one org's rules (newest first).
func (s *ServiceOverrideStore) List(org string) []OverrideRule {
	org = storage.NormalizeOrgID(org)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]OverrideRule, 0, len(s.rules[org]))
	for _, r := range s.rules[org] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Put validates and persists one new override rule for org, assigning a stable
// ID. It rejects an unknown source type, a blank match/service, an over-long
// entry, and an over-full rule set. A rule with the SAME (source_type, match)
// as an existing one REPLACES it (last correction wins) so a re-reassign does
// not stack duplicates. The stored rule (with its assigned ID) is returned.
func (s *ServiceOverrideStore) Put(org string, in OverrideRule) (OverrideRule, error) {
	org = storage.NormalizeOrgID(org)
	sourceType := strings.TrimSpace(in.SourceType)
	match := strings.TrimSpace(in.Match)
	service := strings.TrimSpace(in.Service)
	if !validOverrideSource(sourceType) {
		return OverrideRule{}, fmt.Errorf("invalid source_type %q", in.SourceType)
	}
	if match == "" || service == "" {
		return OverrideRule{}, fmt.Errorf("match and service are required")
	}
	if len(match) > overrideMaxMatchLen || len(service) > overrideMaxServiceLen {
		return OverrideRule{}, fmt.Errorf("entry exceeds maximum length")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rules[org] == nil {
		s.rules[org] = make(map[string]*OverrideRule)
	}
	// Replace an existing (source_type, match) rule in place so corrections
	// don't stack; otherwise enforce the per-org cap.
	var existingID string
	for id, r := range s.rules[org] {
		if r.SourceType == sourceType && r.Match == match {
			existingID = id
			break
		}
	}
	if existingID == "" && len(s.rules[org]) >= overrideMaxRulesPerOrg {
		return OverrideRule{}, fmt.Errorf("too many override rules")
	}

	rule := OverrideRule{
		ID:         existingID,
		OrgID:      org,
		SourceType: sourceType,
		Match:      match,
		Service:    service,
		CreatedAt:  time.Now().UTC(),
		CreatedBy:  strings.TrimSpace(in.CreatedBy),
	}
	if rule.ID == "" {
		rule.ID = newOverrideID()
	}
	cp := rule
	s.rules[org][rule.ID] = &cp
	if err := s.persistLocked(); err != nil {
		delete(s.rules[org], rule.ID)
		return OverrideRule{}, err
	}
	return rule, nil
}

// Delete removes one rule by id within org. Returns false when the rule does
// not exist (or belongs to another org).
func (s *ServiceOverrideStore) Delete(org, id string) (bool, error) {
	org = storage.NormalizeOrgID(org)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rules[org] == nil {
		return false, nil
	}
	removed, ok := s.rules[org][id]
	if !ok {
		return false, nil
	}
	delete(s.rules[org], id)
	if err := s.persistLocked(); err != nil {
		s.rules[org][id] = removed // roll back on persist failure
		return false, err
	}
	return true, nil
}

// CountForService reports how many of org's rules target the named service. The
// service-delete path uses it to refuse orphaning overrides.
func (s *ServiceOverrideStore) CountForService(org, service string) int {
	org = storage.NormalizeOrgID(org)
	service = strings.TrimSpace(service)
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.rules[org] {
		if r.Service == service {
			n++
		}
	}
	return n
}

// RepointService moves every rule targeting oldService to newService within
// org (used when a manual service is renamed so its overrides never dangle).
// Returns the number of rules repointed.
func (s *ServiceOverrideStore) RepointService(org, oldService, newService string) (int, error) {
	org = storage.NormalizeOrgID(org)
	oldService = strings.TrimSpace(oldService)
	newService = strings.TrimSpace(newService)
	if oldService == "" || newService == "" || oldService == newService {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rules[org] {
		if r.Service == oldService {
			r.Service = newService
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.persistLocked(); err != nil {
		// Roll back the in-memory change so cache and disk stay consistent.
		for _, r := range s.rules[org] {
			if r.Service == newService {
				r.Service = oldService
			}
		}
		return 0, err
	}
	return n, nil
}

// persistLocked writes the whole rule set to the provider. The caller must hold
// s.mu. A nil provider is a no-op (in-memory only, e.g. tests).
func (s *ServiceOverrideStore) persistLocked() error {
	if s.store == nil {
		return nil
	}
	f := overrideFile{Version: overrideFileVersion, UpdatedAt: time.Now().UTC()}
	for _, byID := range s.rules {
		for _, r := range byID {
			f.Rules = append(f.Rules, *r)
		}
	}
	sort.Slice(f.Rules, func(i, j int) bool {
		if f.Rules[i].OrgID != f.Rules[j].OrgID {
			return f.Rules[i].OrgID < f.Rules[j].OrgID
		}
		return f.Rules[i].ID < f.Rules[j].ID
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal service overrides: %w", err)
	}
	return s.store.WriteBlob(s.blobName, data)
}

// validOverrideSource reports whether t is a known override source type.
func validOverrideSource(t string) bool {
	switch t {
	case OverrideSourceLog, OverrideSourceMetric, OverrideSourceTrace:
		return true
	default:
		return false
	}
}

// overrideMatches reports whether rule applies to in. A rule only ever matches
// an input of the SAME source type. Log rules match on the mined Pattern
// identity (exact) or a Message substring; metric/trace rules match the Signal
// name exactly or by a `*`/`?` glob. It is a pure function so the OSS and (via
// the seam) enterprise paths — and the UI mirror — agree exactly.
func overrideMatches(rule *OverrideRule, in ServiceOverrideInput) bool {
	if rule == nil || rule.SourceType != in.SourceType || rule.Match == "" || rule.Service == "" {
		return false
	}
	switch in.SourceType {
	case OverrideSourceLog:
		if in.Pattern != "" && rule.Match == in.Pattern {
			return true
		}
		return in.Message != "" && strings.Contains(in.Message, rule.Match)
	case OverrideSourceMetric, OverrideSourceTrace:
		return matchSignalGlob(in.Signal, rule.Match)
	default:
		return false
	}
}

// signalGlobMeta reports whether entry carries a `*`/`?` glob metacharacter.
var signalGlobMeta = regexp.MustCompile(`[*?]`)

// matchSignalGlob reports whether a metric/trace signal name matches a rule
// entry: an exact name, or a `*`/`?` glob anchored at both ends, case-sensitive.
// It mirrors the enterprise learn-exclude matcher and the UI serviceOverride.ts
// glob so a client checkbox reflects the exact server decision.
func matchSignalGlob(signal, entry string) bool {
	entry = strings.TrimSpace(entry)
	if entry == "" || signal == "" {
		return false
	}
	if !signalGlobMeta.MatchString(entry) {
		return signal == entry
	}
	var b strings.Builder
	b.WriteByte('^')
	for _, ch := range entry {
		switch ch {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteByte('$')
	re, err := regexp.Compile(b.String())
	if err != nil {
		return signal == entry
	}
	return re.MatchString(signal)
}

// newOverrideID returns a random, URL-safe rule id.
func newOverrideID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a timestamp id; collisions are astronomically unlikely
		// and a duplicate would only replace, never corrupt.
		return fmt.Sprintf("ovr-%d", time.Now().UnixNano())
	}
	return "ovr-" + hex.EncodeToString(b[:])
}
