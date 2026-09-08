// Package tenancy defines tier-neutral organization scoping primitives.
package tenancy

// DefaultOrgID is the organization used when no explicit organization is
// supplied. It preserves the single-tenant OSS data layout.
const DefaultOrgID = "default"

// OrgScope is the ordered set of organizations a data read may see. Write is
// where new records go. Read is ordered by precedence, with Write first.
type OrgScope struct {
	Write string
	Read  []string
}

// ScopeSource exposes a trusted organization scope computed by a storage or
// tenancy boundary. Implementations must not derive it from request input.
type ScopeSource interface {
	OrgScope() OrgScope
}

// NormalizeOrgID returns a non-empty organization ID.
func NormalizeOrgID(orgID string) string {
	if orgID == "" {
		return DefaultOrgID
	}
	return orgID
}

// NewOrgScope constructs a normalized scope. The write organization is always
// first, blank read organizations normalize to default, and duplicates are
// removed without changing precedence.
func NewOrgScope(write string, read ...string) OrgScope {
	write = NormalizeOrgID(write)
	orgs := make([]string, 0, len(read)+1)
	seen := make(map[string]struct{}, len(read)+1)
	add := func(orgID string) {
		orgID = NormalizeOrgID(orgID)
		if _, ok := seen[orgID]; ok {
			return
		}
		seen[orgID] = struct{}{}
		orgs = append(orgs, orgID)
	}
	add(write)
	for _, orgID := range read {
		add(orgID)
	}
	return OrgScope{Write: write, Read: orgs}
}

// DefaultOrgScope returns the single-tenant OSS scope.
func DefaultOrgScope() OrgScope {
	return NewOrgScope(DefaultOrgID)
}

// Normalized returns a valid, independently owned copy of the scope.
func (s OrgScope) Normalized() OrgScope {
	return NewOrgScope(s.Write, s.Read...)
}

// OrgIDs returns the normalized ordered read organizations as a defensive copy.
func (s OrgScope) OrgIDs() []string {
	normalized := s.Normalized()
	return normalized.Read
}

// Contains reports whether orgID is in the normalized read scope.
func (s OrgScope) Contains(orgID string) bool {
	orgID = NormalizeOrgID(orgID)
	for _, candidate := range s.Normalized().Read {
		if candidate == orgID {
			return true
		}
	}
	return false
}

// ScopeForWrite returns source's trusted read scope when it is pinned to the
// requested write org. A missing or mismatched source fails closed to a
// single-org scope for writeOrg.
func ScopeForWrite(source any, writeOrg string) OrgScope {
	fallback := NewOrgScope(writeOrg)
	scoped, ok := source.(ScopeSource)
	if !ok || scoped == nil {
		return fallback
	}
	candidate := scoped.OrgScope().Normalized()
	if candidate.Write != fallback.Write {
		return fallback
	}
	return candidate
}
