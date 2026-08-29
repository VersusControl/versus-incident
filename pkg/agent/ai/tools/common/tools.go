// Package tools contains cross-cutting read-only AI tools backed by external sources.
package tools

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// SignalReader is the read-only slice of configured signal sources used by tools.
type SignalReader interface {
	Sources() []string
	Pull(ctx context.Context, source string, since time.Time) ([]core.Signal, error)
}

// LineRedactor scrubs sensitive substrings before they reach a model.
type LineRedactor interface {
	Scrub(s string) string
}

// ServiceExtractor extracts an operator-configured service name from a message.
type ServiceExtractor interface {
	Extract(message string) string
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
