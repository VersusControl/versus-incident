package kubernetes

import (
	"context"
	"time"

	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
)

type DiagnoseOptions struct {
	ResourceID string
	Kind       string
	Namespace  string
	Name       string
	LogTail      int
	ChangeWindow time.Duration
}

type Diagnosis struct {
	Workload      WorkloadDetail      `json:"workload"`
	WarningEvents []ProjectedResource `json:"warning_events,omitempty"`
	WorstPodLogs  *PodLogs         `json:"worst_pod_logs,omitempty"`
	Changes       []Change         `json:"changes"`
	Neighborhood  kubegraph.Graph  `json:"neighborhood"`
	Partial       []PartialFailure `json:"partial_failures,omitempty"`
	Omitted       []string         `json:"omitted_categories,omitempty"`
	Truncated     bool             `json:"truncated"`
	Sync          SyncStatus       `json:"sync"`
}

// DiagnoseWorkload combines bounded workload, warning-event and scrubbed log evidence.
func (service *Service) DiagnoseWorkload(ctx context.Context, options DiagnoseOptions) (Diagnosis, error) {
	if !safeSegment(options.Namespace) || !safeSegment(options.Name) {
		return Diagnosis{}, ErrInvalidArguments
	}
	if options.Kind == "" && options.ResourceID != "" {
		for _, resource := range workloadResources {
			if resource.ResourceID == options.ResourceID {
				options.Kind = resource.Kind
				break
			}
		}
	}
	if _, ok := workloadResourceID(options.Kind); !ok {
		return Diagnosis{}, ErrInvalidArguments
	}
	if options.ResourceID != "" {
		resourceID, _ := workloadResourceID(options.Kind)
		if resourceID != options.ResourceID {
			return Diagnosis{}, ErrInvalidArguments
		}
	}
	if options.LogTail <= 0 || options.LogTail > 200 {
		options.LogTail = 40
	}
	if options.ChangeWindow <= 0 || options.ChangeWindow > 2*time.Hour {
		options.ChangeWindow = time.Hour
	}
	ctx, cancel := ensureOperationBudgetRequests(ctx, defaultOperationRequests)
	defer cancel()
	workload, err := service.GetWorkload(ctx, options.Namespace, options.Kind, options.Name)
	if err != nil {
		return Diagnosis{}, err
	}
	result := Diagnosis{
		Workload: workload, Partial: append([]PartialFailure(nil), workload.Partial...),
		Omitted: []string{"health_findings"},
		Sync:    SyncStatus{State: "direct"},
	}
	events, eventsErr := service.ListEvents(ctx, EventOptions{Namespace: options.Namespace, Type: "Warning", Kind: options.Kind, Name: options.Name, Limit: 20})
	if eventsErr != nil {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~events", Class: errorClass(eventsErr)})
		result.Truncated = true
	} else {
		result.WarningEvents = events.Items
		result.Partial = append(result.Partial, events.Partial...)
		result.Truncated = result.Truncated || events.Truncated
		if events.Truncated {
			result.Omitted = appendUnique(result.Omitted, "warning_events")
		}
	}
	pods := append([]WorkloadPod(nil), workload.Pods...)
	prioritizeWorkloadLogPods(pods)
	if len(pods) > 0 {
		container := ""
		if len(workload.Containers) > 0 {
			container = workload.Containers[0].Name
		}
		logs, logErr := service.podLogs(ctx, options.Namespace, pods[0].Name, container, false, 0, options.LogTail, false)
		if logErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: "core~v1~pods", Scope: "logs", Class: errorClass(logErr)})
			result.Truncated = true
		} else {
			result.WorstPodLogs = &logs
			result.Truncated = result.Truncated || logs.Truncated
		}
	} else {
		result.Omitted = appendUnique(result.Omitted, "pod_logs")
	}
	until := time.Now().UTC()
	changes, changesErr := service.Changes(ctx, ChangeQuery{Since: until.Add(-options.ChangeWindow), Until: until, Namespace: options.Namespace, Kind: options.Kind, Name: options.Name, Limit: 20})
	if changesErr != nil {
		result.Partial = append(result.Partial, PartialFailure{Scope: "changes", Class: "unavailable"})
		result.Omitted = appendUnique(result.Omitted, "changes")
	} else {
		result.Changes = changes.Items
		result.Sync.Partial = result.Sync.Partial || changes.Sync.Partial || len(changes.Gaps) > 0
	}
	neighborhood, neighborhoodErr := service.Neighborhood(ctx, NeighborhoodQuery{Kind: options.Kind, Namespace: options.Namespace, Name: options.Name, Hops: 1, MaxNodes: 30})
	if neighborhoodErr != nil {
		result.Partial = append(result.Partial, PartialFailure{Scope: "neighborhood", Class: "unavailable"})
		result.Omitted = appendUnique(result.Omitted, "neighborhood")
	} else {
		result.Neighborhood = neighborhood
		result.Sync.Partial = result.Sync.Partial || neighborhood.Sync.Partial || len(neighborhood.Omitted) > 0
	}
	result.Sync.Partial = result.Sync.Partial || result.Truncated || len(result.Partial) > 0
	return result, nil
}
