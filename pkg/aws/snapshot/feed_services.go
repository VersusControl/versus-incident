package snapshot

import (
	"regexp"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// MaxServiceFilter bounds how many services one filter names.
const MaxServiceFilter = 64

// serviceShape is a service ID as ServiceForSource gives it: the event
// source's first DNS label or a catalog ID.
var serviceShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ParseServices reads a comma-separated service filter. Empty means every
// service.
func ParseServices(text string) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	return NormalizeServices(strings.Split(text, ","))
}

// NormalizeServices trims, lower-cases and de-duplicates a service list,
// keeping its order. Names need not have changes stored: a service with
// none today stays in the filter. A malformed name or an oversized list is
// ErrBadFeedQuery.
func NormalizeServices(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, name := range list {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if !serviceShape.MatchString(name) {
			return nil, ErrBadFeedQuery
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > MaxServiceFilter {
		return nil, ErrBadFeedQuery
	}
	return out, nil
}

// serviceInFilter reports whether a service passes a filter; an empty
// filter passes every service.
func serviceInFilter(filter []string, service string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, name := range filter {
		if name == service {
			return true
		}
	}
	return false
}

// serviceFacet labels the stored per-service counts, most first.
func serviceFacet(counts []storage.AWSChangeServiceCount) []FeedServiceCount {
	out := make([]FeedServiceCount, 0, len(counts))
	for _, count := range counts {
		if count.Events <= 0 {
			continue
		}
		out = append(out, FeedServiceCount{Service: count.Service, Label: changes.ServiceLabel(count.Service), Count: count.Events})
	}
	return out
}
