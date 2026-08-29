// Package tools contains read-only AI tools backed by Versus state.
package tools

import (
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	describeServiceSampleLimit = 1
	patternHistorySampleLimit  = 3
)

// PatternCatalog is the read-only catalog surface used by Versus tools.
type PatternCatalog interface {
	Get(id string) *PatternView
	All() []*PatternView
	AllServices() map[string]ServiceInfo
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
}

// ServiceInfo contains service fields exposed to AI tools.
type ServiceInfo struct {
	FirstSeen time.Time
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
