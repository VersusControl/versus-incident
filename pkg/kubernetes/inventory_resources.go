package kubernetes

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Unknown API kinds use the same safe allowlist projection as generic reads.
// They are indexed by canonical resource ID, so equal kind names in different
// API groups do not overwrite each other.
func (service *Service) loadAdditionalInventory(ctx context.Context, definition ResourceDefinition, namespaces ...string) (recordsResult []ProjectedResource, resultErr error) {
	defer func() { recordInventoryMaterialResult(ctx, resultErr) }()
	ctx, cancel := context.WithTimeout(ctx, inventoryLoadTimeout)
	defer cancel()
	namespace := ""
	if len(namespaces) > 0 {
		namespace = namespaces[0]
	}
	apiPath, err := resourcePath(definition, namespace, "")
	if err != nil {
		return nil, err
	}
	result := []ProjectedResource{}
	token := ""
	seen := map[string]bool{}
	limit := kubernetesIndexPageSize
	for {
		query := url.Values{"limit": {strconv.Itoa(limit)}}
		if token != "" {
			query.Set("continue", token)
		}
		var page struct {
			Items    []map[string]any `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		if err := service.inventoryListRead(ctx, apiPath+"?"+query.Encode(), &page, false); err != nil {
			if errors.Is(err, ErrResponseTooLarge) && limit > minimumPageSize {
				limit = max(minimumPageSize, limit/2)
				continue
			}
			return nil, err
		}
		for _, raw := range page.Items {
			projected := service.projectResource(definition, raw)
			if definition.Namespaced && (!safeSegment(projected.Namespace) || namespace != "" && namespace != projected.Namespace) {
				return nil, ErrGraphIncomplete
			}
			result = append(result, projected)
		}
		token = page.Metadata.Continue
		if token == "" {
			return result, nil
		}
		if len(token) > 4096 || seen[token] {
			return nil, ErrInvalidArguments
		}
		seen[token] = true
	}
}

func cachedSelectorMatches(item ProjectedResource, labels, fields string) (bool, error) {
	for _, selection := range []struct {
		text   string
		values map[string]string
	}{
		{labels, item.Labels},
		{fields, map[string]string{"metadata.name": item.Name, "metadata.namespace": item.Namespace, "status.phase": summaryString(item.Summary, "phase"), "spec.nodeName": summaryString(item.Summary, "node")}},
	} {
		if !safeSelector(selection.text) {
			return false, ErrInvalidArguments
		}
		if selection.text == "" {
			continue
		}
		for _, clause := range strings.Split(selection.text, ",") {
			clause = strings.TrimSpace(clause)
			operator := ""
			for _, candidate := range []string{"!=", "==", "="} {
				if strings.Contains(clause, candidate) {
					operator = candidate
					break
				}
			}
			if operator == "" {
				if strings.ContainsAny(clause, " ()") {
					return false, ErrInvalidArguments
				}
				if strings.HasPrefix(clause, "!") {
					if _, exists := selection.values[strings.TrimPrefix(clause, "!")]; exists {
						return false, nil
					}
				} else if _, exists := selection.values[clause]; !exists {
					return false, nil
				}
				continue
			}
			parts := strings.SplitN(clause, operator, 2)
			value, exists := selection.values[strings.TrimSpace(parts[0])]
			if operator == "!=" {
				if exists && value == strings.TrimSpace(parts[1]) {
					return false, nil
				}
			} else if !exists || value != strings.TrimSpace(parts[1]) {
				return false, nil
			}
		}
	}
	return true, nil
}
