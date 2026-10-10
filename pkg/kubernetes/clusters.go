package kubernetes

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"
)

var clusterVersionPattern = regexp.MustCompile(`^v[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}([+.-][A-Za-z0-9.-]{1,96})?$`)

func (service *Service) clusterVersion(ctx context.Context) string {
	data, truncated, err := service.client.getBounded(ctx, "/version", "application/json", 4096)
	if err != nil || truncated {
		return ""
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if json.Unmarshal(data, &version) != nil || !clusterVersionPattern.MatchString(version.GitVersion) {
		return ""
	}
	if service.scrubber != nil {
		version.GitVersion = service.scrubber.Scrub(version.GitVersion)
	}
	if !clusterVersionPattern.MatchString(version.GitVersion) {
		return ""
	}
	return version.GitVersion
}

type ClusterSummary struct {
	ClusterInfo
	Version     string       `json:"version,omitempty"`
	Health      string       `json:"health"`
	Nodes       int          `json:"nodes"`
	ReadyNodes  int          `json:"ready_nodes"`
	Pods        int          `json:"pods"`
	RunningPods int          `json:"running_pods"`
	Warnings    int          `json:"warnings"`
	Issues      *int         `json:"issues,omitempty"`
	Sync        SyncStatus   `json:"sync"`
	ObservedAt  time.Time    `json:"observed_at"`
	Error       *ErrorDetail `json:"error,omitempty"`
}

type clusterSummaryCache struct {
	value   ClusterSummary
	expires time.Time
}

func ClusterHealth(overview Overview) string {
	if overview.Truncated || len(overview.Omitted) > 0 || len(overview.Partial) > 0 {
		return "partial"
	}
	if overview.ReadyNodes < overview.Nodes || overview.Warnings > 0 {
		return "attention"
	}
	if overview.Nodes > 0 {
		return "healthy"
	}
	return "unknown"
}

func (registry *ServiceRegistry) Summaries(ctx context.Context, orgID string, allow func(string) bool) []ClusterSummary {
	result := []ClusterSummary{}
	if registry == nil {
		return result
	}
	ctx, cancelRequest := context.WithTimeout(ctx, 4*registry.summaryTimeout)
	defer cancelRequest()
	visible := []ClusterInfo{}
	for _, info := range registry.Clusters() {
		if allow == nil || allow(info.ID) {
			visible = append(visible, info)
		}
	}
	result = make([]ClusterSummary, len(visible))
	var workers sync.WaitGroup
	for index, info := range visible {
		workers.Add(1)
		go func(index int, info ClusterInfo) {
			defer workers.Done()
			unreachable := ClusterSummary{ClusterInfo: info, Health: "unreachable", ObservedAt: time.Now().UTC(), Sync: SyncStatus{State: "unavailable"}, Error: &ErrorDetail{Code: "connector_unreachable", Message: "The cluster did not respond.", Retryable: true}}
			select {
			case registry.summarySlots <- struct{}{}:
				defer func() { <-registry.summarySlots }()
			case <-ctx.Done():
				result[index] = unreachable
				return
			}
			readCtx, cancel := context.WithTimeout(ctx, registry.summaryTimeout)
			defer cancel()
			key := orgID + "\x00" + info.ID
			registry.mu.Lock()
			cached, ok := registry.summaries[key]
			registry.mu.Unlock()
			if ok && time.Now().Before(cached.expires) {
				result[index] = cached.value
				return
			}
			value := unreachable
			service, err := registry.ResolveCluster(orgID, info.ID)
			if err == nil {
				overview, readErr := service.Overview(readCtx)
				if readErr == nil && readCtx.Err() == nil {
					value = ClusterSummary{ClusterInfo: info, Health: ClusterHealth(overview), Nodes: overview.Nodes, ReadyNodes: overview.ReadyNodes, Pods: overview.Pods, RunningPods: overview.RunningPods, Warnings: overview.Warnings, Sync: overview.Sync, ObservedAt: overview.ObservedAt}
					value.Version = service.clusterVersion(readCtx)
					issues, issueErr := service.Issues(readCtx, IssueOptions{Limit: minimumPageSize})
					if issueErr == nil && readCtx.Err() == nil && !issues.Sync.Partial && len(issues.Partial) == 0 {
						count := 0
						for _, total := range issues.Totals {
							count += total
						}
						value.Issues = &count
						if count > 0 && (value.Health == "healthy" || value.Health == "unknown") {
							value.Health = "attention"
						}
					}
				}
			}
			if ctx.Err() == nil && value.Error == nil {
				registry.mu.Lock()
				if len(registry.summaries) >= 256 {
					for cacheKey, entry := range registry.summaries {
						if time.Now().After(entry.expires) {
							delete(registry.summaries, cacheKey)
						}
					}
				}
				if len(registry.summaries) < 256 {
					registry.summaries[key] = clusterSummaryCache{value: value, expires: time.Now().Add(30 * time.Second)}
				}
				registry.mu.Unlock()
			}
			result[index] = value
		}(index, info)
	}
	workers.Wait()
	return result
}
