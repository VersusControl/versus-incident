package estate

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Search bounds.
const (
	// MinSearchText is the shortest text a search takes.
	MinSearchText = 2
	// MaxSearchText is the longest.
	MaxSearchText = 200
	// MaxSearchResults is how many resources one search returns.
	MaxSearchResults = 20
)

// SearchQuery finds resources to focus on, in a scope.
type SearchQuery struct {
	Text string
	Scope
}

// SearchItem is one resource a search found: Ref is what a Focus view
// takes.
type SearchItem struct {
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Service string `json:"service"`
	Account string `json:"account"`
	Region  string `json:"region"`
}

// SearchResult is at most MaxSearchResults resources; Capped says more
// matched.
type SearchResult struct {
	Enabled bool         `json:"enabled"`
	Items   []SearchItem `json:"items"`
	Capped  bool         `json:"capped"`
}

// Search finds the connected accounts' stored resources whose name, ID or
// ARN contains the text, ignoring case: names that start with it first,
// then by name and ARN. It reads the committed snapshots only.
func (s *Service) Search(ctx context.Context, query SearchQuery) (SearchResult, error) {
	out := SearchResult{Items: []SearchItem{}}
	text := strings.ToLower(strings.TrimSpace(query.Text))
	if len(text) < MinSearchText || len(text) > MaxSearchText {
		return out, ErrInvalidQuery
	}
	if err := query.Valid(); err != nil || s.snapshots == nil || s.scope == nil {
		return out, err
	}
	ctx = s.snapshots.ReadContext(ctx)
	expected, enabled, err := s.scope(ctx)
	if err != nil || !enabled {
		return out, err
	}
	out.Enabled = true
	connected := map[string]bool{}
	for _, entry := range expected {
		connected[entry.Account] = true
	}
	accounts := []string{""}
	if len(query.Accounts) > 0 {
		accounts = nil
		for _, account := range query.Accounts {
			if !connected[account] {
				return out, ErrNotFound
			}
			if !slices.Contains(accounts, account) {
				accounts = append(accounts, account)
			}
		}
	}
	type match struct {
		item   SearchItem
		prefix bool
	}
	var found []match
	seen := map[string]bool{}
	for _, account := range accounts {
		// The light read: the stored resources of the scope, nothing
		// joined, grouped or drawn.
		result, err := s.snapshots.Resources(ctx, snapshot.Query{Account: account})
		if err != nil {
			return out, err
		}
		for _, resource := range result.Resources {
			if !connected[resource.Account] || !query.inRegion(resource.Region) || seen[resource.ARN] || resource.ARN == "" ||
				resource.Type == model.TrailType || resource.Type == model.HealthEventType || resource.Type == "organization" {
				continue
			}
			name := strings.ToLower(resource.Name)
			if !strings.Contains(name, text) && !strings.Contains(strings.ToLower(model.ResourceID(resource.ARN)), text) &&
				!strings.Contains(strings.ToLower(resource.ARN), text) {
				continue
			}
			seen[resource.ARN] = true
			found = append(found, match{item: SearchItem{Ref: resource.ARN, Name: resource.Name, Type: resource.Type, Service: resource.Service,
				Account: resource.Account, Region: resource.Region}, prefix: strings.HasPrefix(name, text)})
		}
	}
	sort.Slice(found, func(left, right int) bool {
		a, b := found[left], found[right]
		switch {
		case a.prefix != b.prefix:
			return a.prefix
		case a.item.Name != b.item.Name:
			return a.item.Name < b.item.Name
		}
		return a.item.Ref < b.item.Ref
	})
	if len(found) > MaxSearchResults {
		found, out.Capped = found[:MaxSearchResults], true
	}
	for _, entry := range found {
		out.Items = append(out.Items, entry.item)
	}
	return out, nil
}
