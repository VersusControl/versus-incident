// Package tools contains read-only AI tools backed by Versus state.
package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	describeServiceSampleLimit = 1
	patternHistorySampleLimit  = 3
	alertDecisionEvidenceLimit = 10
	reliabilityItemLimit       = 20
	discoveryDefaultLimit      = 20
	discoveryMaxLimit          = 100
	discoveryFallbackLimit     = 500
	modelTextLimit             = 512
)

// PatternCatalog is the read-only catalog surface used by Versus tools.
type PatternCatalog interface {
	Get(id string) *PatternView
	All() []*PatternView
	AllServices() map[string]ServiceInfo
}

// PagedPatternCatalog is the optional bounded catalog capability used by
// discovery tools. It is separate from PatternCatalog so existing point-read
// consumers and adapters do not have to fork their shared contract.
type PagedPatternCatalog interface {
	PatternsPage(CatalogPageOptions) ([]*PatternView, int, error)
	ServicesPage(CatalogPageOptions) ([]ServiceRow, int, error)
}

// CatalogPageOptions is one bounded catalog read. Total values returned by
// PatternCatalog page methods are exact after applying Search.
type CatalogPageOptions struct {
	Offset  int
	Limit   int
	Search  string
	Service string
}

// ServiceRow is one stable, ordered service catalog row.
type ServiceRow struct {
	Name string
	Info ServiceInfo
}

// PatternView contains the catalog fields exposed to AI tools.
type PatternView struct {
	ID        string
	Template  string
	Source    string
	Service   string
	RuleName  string
	Verdict   string
	Tags      []string
	Count     int
	Baseline  float64
	FirstSeen time.Time
	LastSeen  time.Time
	Samples   []string
	// AutoPromoteAfter is the effective sighting threshold used by the
	// owning catalog. Zero means the threshold is unavailable.
	AutoPromoteAfter int
}

// CapabilityStatus is one model-safe feature/source availability record.
type CapabilityStatus struct {
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	Configured  bool   `json:"configured"`
	Licensed    string `json:"licensed"`
	Available   string `json:"available"`
	Reason      string `json:"reason,omitempty"`
	Observation string `json:"observation,omitempty"`
	SetupAction string `json:"setup_action,omitempty"`
}

const (
	CapabilityStatusTrue    = "true"
	CapabilityStatusFalse   = "false"
	CapabilityStatusUnknown = "unknown"
)

// CapabilityStatusProvider reports bounded, model-safe, provider-neutral state.
// Implementations must not include credentials, backend errors, or secret values.
type CapabilityStatusProvider interface {
	CapabilityStatuses(tenancy.OrgScope) []CapabilityStatus
}

// ServiceReliabilityProvider is the optional read-only reliability seam.
type ServiceReliabilityProvider interface {
	ServiceReliability(context.Context, tenancy.OrgScope, string) (ServiceReliabilitySnapshot, error)
}

// ServiceReliabilitySnapshot contains provider-neutral service reliability data.
type ServiceReliabilitySnapshot struct {
	Service     string             `json:"service"`
	SLIs        []ServiceIndicator `json:"slis"`
	SLOs        []ServiceObjective `json:"slos"`
	Source      string             `json:"source,omitempty"`
	Observation string             `json:"observation,omitempty"`
	Configured  bool               `json:"configured"`
	Licensed    string             `json:"licensed"`
	Available   string             `json:"available"`
	Reason      string             `json:"reason,omitempty"`
	SetupAction string             `json:"setup_action,omitempty"`
}

type ServiceIndicator struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind,omitempty"`
	Description string   `json:"description,omitempty"`
	Current     *float64 `json:"current,omitempty"`
	Unit        string   `json:"unit,omitempty"`
}

type ServiceObjective struct {
	Name       string  `json:"name"`
	SLI        string  `json:"sli"`
	Target     float64 `json:"target"`
	WindowDays int     `json:"window_days,omitempty"`
	Derived    bool    `json:"derived,omitempty"`
}

// AlertDecisionProvider is the optional read-only alert decision seam.
type AlertDecisionProvider interface {
	AlertDecision(context.Context, tenancy.OrgScope, string) (AlertDecisionSnapshot, error)
}

// AlertDecisionSnapshot is one provider-neutral decision record.
type AlertDecisionSnapshot struct {
	Identifier   string     `json:"identifier"`
	Found        bool       `json:"found"`
	DecisionType string     `json:"decision_type,omitempty"`
	Status       string     `json:"status,omitempty"`
	Action       string     `json:"action,omitempty"`
	Outcome      string     `json:"outcome,omitempty"`
	ReasonKnown  bool       `json:"reason_known"`
	Reason       string     `json:"reason,omitempty"`
	Evidence     []string   `json:"evidence,omitempty"`
	Provenance   string     `json:"provenance,omitempty"`
	Timestamp    *time.Time `json:"timestamp,omitempty"`
	Rule         string     `json:"rule,omitempty"`
	Fingerprint  string     `json:"fingerprint,omitempty"`
}

// ServiceInfo contains service fields exposed to AI tools.
type ServiceInfo struct {
	FirstSeen time.Time
}

// LineRedactor scrubs a catalog sample before it reaches a model.
type LineRedactor interface {
	Scrub(string) string
}

// DetectionHealthReader exposes passive source wiring/runtime observations.
// Implementations must never pull a source to answer this read.
type DetectionHealthReader interface {
	DetectionHealth() DetectionHealthSnapshot
}

// DetectionHealthSnapshot is the configured source and signal-category view.
type DetectionHealthSnapshot struct {
	Sources     []SourceHealth   `json:"sources"`
	Categories  []CategoryHealth `json:"categories"`
	Observation string           `json:"observation"`
}

// SourceHealth describes one configured source without endpoint or secret data.
type SourceHealth struct {
	Name               string     `json:"name"`
	Kind               string     `json:"kind"`
	Configured         bool       `json:"configured"`
	Observation        string     `json:"observation"`
	LastSuccessfulPull *time.Time `json:"last_successful_pull,omitempty"`
	ErrorClass         string     `json:"error_class,omitempty"`
}

// CategoryHealth distinguishes configured categories from dark ones.
type CategoryHealth struct {
	Kind       string `json:"kind"`
	Configured bool   `json:"configured"`
	Dark       bool   `json:"dark"`
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return discoveryDefaultLimit
	}
	if limit > discoveryMaxLimit {
		return discoveryMaxLimit
	}
	return limit
}

func validateOffset(offset int) (int, error) {
	if offset < 0 {
		return 0, nil
	}
	if offset > discoveryFallbackLimit {
		return 0, fmt.Errorf("offset must be at most %d", discoveryFallbackLimit)
	}
	return offset, nil
}

func boundedIncidentsForScope(store storage.Provider, scope tenancy.OrgScope, limit int) ([]*storage.IncidentRecord, bool, error) {
	if limit <= 0 || limit > discoveryFallbackLimit {
		limit = discoveryFallbackLimit
	}
	if scoped, ok := store.(storage.ScopedIncidentPager); ok {
		records, err := scoped.ListIncidentsPageForScope(scope.Normalized(), "", 0, limit+1)
		if err != nil {
			return nil, false, err
		}
		truncated := len(records) > limit
		if truncated {
			records = records[:limit]
		}
		return records, truncated, nil
	}
	records, err := store.ListIncidents(limit + 1)
	if err != nil {
		return nil, false, err
	}
	allowed := make(map[string]struct{}, len(scope.OrgIDs()))
	for _, orgID := range scope.Normalized().OrgIDs() {
		allowed[orgID] = struct{}{}
	}
	out := make([]*storage.IncidentRecord, 0, limit)
	truncated := len(records) > limit
	for _, record := range records {
		if _, ok := allowed[storage.NormalizeOrgID(record.OrgID)]; !ok {
			continue
		}
		if len(out) == limit {
			truncated = true
			continue
		}
		out = append(out, record)
	}
	return out, truncated, nil
}

func incidentsForScope(store storage.Provider, scope tenancy.OrgScope) ([]*storage.IncidentRecord, error) {
	scope = scope.Normalized()
	if scoped, ok := store.(storage.ScopedRangeLister); ok {
		return scoped.ListIncidentsInRangeForScope(scope, time.Time{}, time.Time{}, 0)
	}
	all, err := store.ListIncidents(0)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(scope.Read))
	for _, orgID := range scope.OrgIDs() {
		allowed[orgID] = struct{}{}
	}
	out := make([]*storage.IncidentRecord, 0, len(all))
	for _, record := range all {
		if _, ok := allowed[storage.NormalizeOrgID(record.OrgID)]; ok {
			out = append(out, record)
		}
	}
	return out, nil
}

func latestSamples(ring []string, limit int) []string {
	if limit <= 0 || len(ring) == 0 {
		return nil
	}
	if len(ring) > limit {
		ring = ring[len(ring)-limit:]
	}
	return append([]string(nil), ring...)
}

func scrubModelText(value string, redactor LineRedactor) string {
	if redactor == nil {
		return ""
	}
	runes := []rune(redactor.Scrub(value))
	if len(runes) > modelTextLimit {
		runes = runes[:modelTextLimit]
	}
	return string(runes)
}
