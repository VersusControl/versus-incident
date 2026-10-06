package k8s

type toolSpec struct {
	Name        string
	Display     string
	Description string
	Properties  map[string]any
	Required    []string
}

var specs = []toolSpec{
	{Name: "get_cluster_overview", Display: "Cluster overview", Description: "Summarize Kubernetes cluster health and capacity."},
	{Name: "discover_k8s_resources", Display: "Discover Kubernetes resources", Description: "Discover readable Kubernetes resources and canonical resource IDs."},
	{Name: "query_k8s_resources", Display: "Query Kubernetes resources", Description: "Search or list one discovered Kubernetes resource.", Properties: map[string]any{"query": stringProperty(), "resource_id": stringProperty(), "namespace": stringProperty(), "category": map[string]any{"type": "string", "enum": []string{"workload", "pod", "node", "network", "storage", "configuration", "access", "event", "other"}}, "labels": stringProperty(), "fields": stringProperty(), "continue": stringProperty(), "limit": integerProperty(), "per_kind_limit": integerProperty()}},
	{Name: "get_k8s_resource", Display: "Kubernetes resource details", Description: "Get one safely projected Kubernetes resource.", Properties: map[string]any{"resource_id": stringProperty(), "namespace": namespacedResourceProperty(), "name": stringProperty(), "diagnostic": map[string]any{"type": "boolean"}}, Required: []string{"resource_id", "name"}},
	{Name: "list_workloads", Display: "List workloads", Description: "List bounded Kubernetes workloads.", Properties: map[string]any{"namespace": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "q": stringProperty(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}, "cursor": stringProperty()}},
	{Name: "get_workload", Display: "Workload details", Description: "Inspect one safely projected Kubernetes workload.", Properties: map[string]any{"namespace": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "name": stringProperty()}, Required: []string{"namespace", "kind", "name"}},
	{Name: "list_k8s_events", Display: "Kubernetes events", Description: "List bounded Kubernetes events.", Properties: map[string]any{"namespace": stringProperty(), "event_type": map[string]any{"type": "string", "enum": []string{"Warning", "Normal"}}, "object_kind": stringProperty(), "object_name": stringProperty(), "object_uid": stringProperty(), "continue": stringProperty(), "cursor": stringProperty(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 5}}},
	{Name: "get_pod_logs", Display: "Pod logs", Description: "Read bounded logs for one pod, optionally selecting a container.", Properties: map[string]any{"namespace": stringProperty(), "name": stringProperty(), "container": map[string]any{"type": "string", "maxLength": 253, "description": "Optional for single-container pods; required for multi-container pods."}, "previous": map[string]any{"type": "boolean"}, "since_seconds": integerProperty(), "tail_lines": integerProperty()}, Required: []string{"namespace", "name"}},
	{Name: "get_workload_logs", Display: "Workload logs", Description: "Read scrubbed, bounded logs from the least healthy pods in a Kubernetes workload.", Properties: map[string]any{"namespace": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "name": stringProperty(), "pod": stringProperty(), "container": stringProperty(), "previous": map[string]any{"type": "boolean"}, "since_seconds": integerProperty(), "tail_lines": integerProperty(), "grep": map[string]any{"type": "string", "maxLength": 256}, "max_pods": map[string]any{"type": "integer", "minimum": 1, "maximum": 10}}, Required: []string{"namespace", "kind", "name"}},
	{Name: "list_k8s_issues", Display: "Kubernetes issues", Description: "Group projected Kubernetes health findings by root workload.", Properties: map[string]any{"namespace": stringProperty(), "severity": map[string]any{"type": "string", "enum": []string{"critical", "warning", "info"}}, "kind": stringProperty(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}, "cursor": stringProperty()}},
	{Name: "get_k8s_changes", Display: "Kubernetes changes", Description: "Read recent bounded Kubernetes object changes from the shared cluster index.", Properties: map[string]any{"since_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": 1440}, "namespace": stringProperty(), "kind": stringProperty(), "name": stringProperty(), "type": map[string]any{"type": "string", "enum": []string{"created", "deleted", "image_changed", "replicas_changed", "spec_changed"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}, "cursor": stringProperty()}},
	{Name: "get_k8s_neighborhood", Display: "Kubernetes neighborhood", Description: "Inspect bounded relationships around one resource or summarize a namespace topology, optionally with observed traffic.", Properties: map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"focus", "namespace"}}, "resource_id": stringProperty(), "kind": stringProperty(), "namespace": stringProperty(), "name": stringProperty(), "hops": map[string]any{"type": "integer", "minimum": 0, "maximum": 2}, "max_nodes": map[string]any{"type": "integer", "minimum": 1, "maximum": 150}, "cursor": stringProperty(), "connected_only": map[string]any{"type": "boolean"}, "include_traffic": map[string]any{"type": "boolean"}, "source": stringProperty(), "window": map[string]any{"type": "string", "enum": []string{"5m", "15m", "1h"}}}},
	{Name: "top_k8s_resources", Display: "Top Kubernetes resources", Description: "Rank pod or node resource usage from current metrics.", Properties: map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"pod", "node"}}, "namespace": stringProperty(), "sort": map[string]any{"type": "string", "enum": []string{"cpu", "memory"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50}}, Required: []string{"kind"}},
	{Name: "list_gitops_apps", Display: "GitOps applications", Description: "List discovered Argo CD and Flux application status without exposing manifests or credentials.", Properties: map[string]any{"namespace": stringProperty(), "tool": map[string]any{"type": "string", "enum": []string{"argocd", "flux"}}, "status": stringProperty(), "limit": integerProperty(), "cursor": stringProperty()}},
	{Name: "get_rollout_status", Display: "Rollout status", Description: "Inspect bounded Argo Rollouts phase and canary progress.", Properties: map[string]any{"namespace": stringProperty(), "name": stringProperty()}, Required: []string{"namespace", "name"}},
	{Name: "list_rollouts", Display: "List rollouts", Description: "List discovered Argo Rollouts and bounded canary progress.", Properties: map[string]any{"namespace": stringProperty(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 3}, "cursor": stringProperty()}},
	{Name: "list_helm_releases", Display: "Helm releases", Description: "List Helm release status from Secret metadata labels only.", Properties: map[string]any{"namespace": stringProperty(), "status": stringProperty(), "limit": integerProperty(), "cursor": stringProperty()}},
	{Name: "get_helm_release", Display: "Helm release details", Description: "Inspect one Helm release revision history from Secret metadata labels only.", Properties: map[string]any{"namespace": stringProperty(), "name": stringProperty()}, Required: []string{"namespace", "name"}},
	{Name: "diagnose_k8s_workload", Display: "Diagnose Kubernetes workload", Description: "Collect bounded workload, warning-event, scrubbed pod-log, change and relationship evidence.", Properties: map[string]any{"resource_id": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "namespace": stringProperty(), "name": stringProperty(), "log_tail": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}, "change_window_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": 120}}, Required: []string{"namespace", "name"}},
}

var (
	toolNames         []string
	descriptions      = make(map[string]string, len(specs))
	schemas           = make(map[string]map[string]any, len(specs))
	requiredArguments = make(map[string][]string, len(specs))
	displayNames      = make(map[string]string, len(specs))
)

func init() {
	for _, spec := range specs {
		toolNames = append(toolNames, spec.Name)
		descriptions[spec.Name] = spec.Description
		schemas[spec.Name] = spec.Properties
		displayNames[spec.Name] = spec.Display
		if len(spec.Required) > 0 {
			requiredArguments[spec.Name] = spec.Required
		}
	}
}
