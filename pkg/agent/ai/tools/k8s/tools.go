package k8s

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

var toolNames = []string{"get_cluster_overview", "discover_k8s_resources", "query_k8s_resources", "get_k8s_resource", "list_workloads", "get_workload", "get_k8s_topology", "list_k8s_events", "get_pod_logs"}

// New constructs all Kubernetes model tools over one shared application service.
func New(service *kubernetes.Service) []core.Tool {
	if service == nil {
		return nil
	}
	result := make([]core.Tool, 0, len(toolNames))
	for _, name := range toolNames {
		result = append(result, &tool{name: name, service: service})
	}
	return result
}

// FilterAuthorized removes Kubernetes tools from unauthorized model catalogs.
func FilterAuthorized(ctx context.Context, tools []core.Tool) []core.Tool {
	if core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return tools
	}
	result := make([]core.Tool, 0, len(tools))
	for _, candidate := range tools {
		if candidate == nil || !isKubernetesTool(candidate.Name()) {
			result = append(result, candidate)
		}
	}
	return result
}

func isKubernetesTool(name string) bool {
	for _, candidate := range toolNames {
		if candidate == name {
			return true
		}
	}
	return false
}

type tool struct {
	name    string
	service *kubernetes.Service
}

func (tool *tool) Name() string { return tool.name }
func (tool *tool) DisplayName() string {
	if metadata, ok := lookupDisplay(tool.name); ok {
		return metadata
	}
	return tool.name
}
func (tool *tool) Description() string { return descriptions[tool.name] }
func (tool *tool) ArgsSchema() map[string]any {
	schema := map[string]any{"type": "object", "properties": schemas[tool.name], "additionalProperties": false}
	if required := requiredArguments[tool.name]; len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func (tool *tool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(tool.name, "infrastructure:view permission is required"), nil
	}
	var args arguments
	if len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid Kubernetes tool arguments", err)
		}
	}
	var data any
	found := true
	var err error
	switch tool.name {
	case "get_cluster_overview":
		data, err = tool.service.Overview(ctx)
	case "discover_k8s_resources":
		var result kubernetes.Discovery
		result, err = tool.service.Discover(ctx)
		data, found = result, len(result.Resources) > 0
	case "query_k8s_resources":
		if args.Query != "" {
			var result kubernetes.SearchResult
			result, err = tool.service.Search(ctx, kubernetes.SearchOptions{Query: args.Query, Namespace: args.Namespace, Category: args.Category, Labels: args.Labels, Fields: args.Fields, PerKindLimit: args.PerKindLimit, TotalLimit: args.Limit})
			data, found = result, len(result.Items) > 0
		} else if args.ResourceID == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.ResourcePage
			result, err = tool.service.List(ctx, kubernetes.ListOptions{ResourceID: args.ResourceID, Namespace: args.Namespace, Labels: args.Labels, Fields: args.Fields, Continue: args.Continue, Limit: args.Limit})
			data, found = result, len(result.Items) > 0
		}
	case "get_k8s_resource":
		if args.ResourceID == "" || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else if args.Diagnostic {
			data, err = tool.service.Describe(ctx, args.ResourceID, args.Namespace, args.Name)
		} else {
			data, err = tool.service.Get(ctx, args.ResourceID, args.Namespace, args.Name)
		}
	case "list_workloads":
		var result kubernetes.WorkloadPage
		result, err = tool.service.ListWorkloads(ctx, args.Namespace, args.Kind, args.Limit)
		data, found = result, len(result.Items) > 0
	case "get_workload":
		if _, ok := workloadResourceID(args.Kind); !ok || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			data, err = tool.service.GetWorkload(ctx, args.Namespace, args.Kind, args.Name)
		}
	case "get_k8s_topology":
		var result kubernetes.Topology
		result, err = tool.service.Topology(ctx, args.Namespace, args.NodeCap, args.EdgeCap)
		data, found = result, len(result.Nodes) > 0
	case "list_k8s_events":
		var result kubernetes.ResourcePage
		result, err = tool.service.ListEvents(ctx, kubernetes.EventOptions{Namespace: args.Namespace, Type: args.EventType, Kind: args.ObjectKind, Name: args.ObjectName, UID: args.ObjectUID, Continue: args.Continue, Limit: args.Limit})
		data, found = result, len(result.Items) > 0
	case "get_pod_logs":
		if args.Namespace == "" || args.Name == "" || args.SinceSeconds < 0 || args.SinceSeconds > 86400 || args.TailLines < 0 || args.TailLines > 5000 {
			err = kubernetes.ErrInvalidArguments
		} else {
			data, err = tool.service.PodLogs(ctx, args.Namespace, args.Name, args.Container, args.Previous, args.SinceSeconds, args.TailLines)
		}
	default:
		return nil, core.NewToolError(core.ToolErrorInternal, "unknown Kubernetes tool", nil)
	}
	if err != nil {
		if errors.Is(err, kubernetes.ErrForbidden) {
			return &core.ToolResult{Tool: tool.name, Found: false, Data: map[string]any{"status": "forbidden", "partial": true}}, nil
		}
		if errors.Is(err, kubernetes.ErrNotFound) {
			return &core.ToolResult{Tool: tool.name, Found: false, Data: map[string]any{"status": "unavailable"}}, nil
		}
		return nil, safeToolError(err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "Kubernetes result unavailable", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, core.NewToolError(core.ToolErrorInternal, "Kubernetes result unavailable", err)
	}
	return &core.ToolResult{Tool: tool.name, Found: found, Data: payload}, nil
}

type arguments struct {
	ResourceID   string `json:"resource_id"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Container    string `json:"container"`
	Labels       string `json:"labels"`
	Fields       string `json:"fields"`
	Continue     string `json:"continue"`
	Limit        int    `json:"limit"`
	NodeCap      int    `json:"node_cap"`
	EdgeCap      int    `json:"edge_cap"`
	Previous     bool   `json:"previous"`
	SinceSeconds int    `json:"since_seconds"`
	TailLines    int    `json:"tail_lines"`
	Diagnostic   bool   `json:"diagnostic"`
	Query        string `json:"query"`
	PerKindLimit int    `json:"per_kind_limit"`
	Category     string `json:"category"`
	ObjectKind   string `json:"object_kind"`
	ObjectName   string `json:"object_name"`
	ObjectUID    string `json:"object_uid"`
	EventType    string `json:"event_type"`
}

var descriptions = map[string]string{
	"get_cluster_overview": "Summarize Kubernetes cluster health and capacity.", "discover_k8s_resources": "Discover readable Kubernetes resources and canonical resource IDs.",
	"query_k8s_resources": "Search or list one discovered Kubernetes resource.", "get_k8s_resource": "Get one safely projected Kubernetes resource.",
	"list_workloads": "List bounded Kubernetes workloads.", "get_workload": "Inspect one safely projected Kubernetes workload.",
	"get_k8s_topology": "Inspect bounded Kubernetes ownership and scheduling topology.", "list_k8s_events": "List bounded Kubernetes events.", "get_pod_logs": "Read bounded logs for one pod, optionally selecting a container.",
}
var schemas = map[string]map[string]any{
	"get_cluster_overview": {}, "discover_k8s_resources": {},
	"query_k8s_resources": {"query": stringProperty(), "resource_id": stringProperty(), "namespace": stringProperty(), "category": map[string]any{"type": "string", "enum": []string{"workload", "pod", "node", "network", "storage", "configuration", "access", "event", "other"}}, "labels": stringProperty(), "fields": stringProperty(), "continue": stringProperty(), "limit": integerProperty(), "per_kind_limit": integerProperty()},
	"get_k8s_resource":    {"resource_id": stringProperty(), "namespace": namespacedResourceProperty(), "name": stringProperty(), "diagnostic": map[string]any{"type": "boolean"}},
	"list_workloads":      {"namespace": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "limit": integerProperty()},
	"get_workload":        {"namespace": stringProperty(), "kind": map[string]any{"type": "string", "enum": []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"}}, "name": stringProperty()},
	"get_k8s_topology":    {"namespace": stringProperty(), "node_cap": integerProperty(), "edge_cap": integerProperty()},
	"list_k8s_events":     {"namespace": stringProperty(), "event_type": map[string]any{"type": "string", "enum": []string{"Warning", "Normal"}}, "object_kind": stringProperty(), "object_name": stringProperty(), "object_uid": stringProperty(), "continue": stringProperty(), "limit": integerProperty()},
	"get_pod_logs":        {"namespace": stringProperty(), "name": stringProperty(), "container": map[string]any{"type": "string", "maxLength": 253, "description": "Optional for single-container pods; required for multi-container pods."}, "previous": map[string]any{"type": "boolean"}, "since_seconds": integerProperty(), "tail_lines": integerProperty()},
}

var requiredArguments = map[string][]string{
	"get_k8s_resource": {"resource_id", "name"},
	"get_workload":     {"namespace", "kind", "name"},
	"get_pod_logs":     {"namespace", "name"},
}

func stringProperty() map[string]any { return map[string]any{"type": "string", "maxLength": 253} }
func namespacedResourceProperty() map[string]any {
	return map[string]any{"type": "string", "maxLength": 253, "description": "Required when the discovered resource is namespaced."}
}
func integerProperty() map[string]any { return map[string]any{"type": "integer", "minimum": 1} }
func workloadResourceID(kind string) (string, bool) {
	switch kind {
	case "StatefulSet":
		return "apps~v1~statefulsets", true
	case "DaemonSet":
		return "apps~v1~daemonsets", true
	case "Job":
		return "batch~v1~jobs", true
	case "CronJob":
		return "batch~v1~cronjobs", true
	case "Pod":
		return "core~v1~pods", true
	case "Deployment":
		return "apps~v1~deployments", true
	}
	return "", false
}
func safeToolError(err error) error {
	if errors.Is(err, kubernetes.ErrInvalidArguments) || errors.Is(err, kubernetes.ErrInvalidEndpoint) {
		return core.NewToolError(core.ToolErrorInvalidArguments, "invalid Kubernetes tool arguments", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kubernetes.ErrResponseTooLarge) {
		return core.NewToolError(core.ToolErrorTimeout, "Kubernetes read exceeded its bound", err)
	}
	if errors.Is(err, kubernetes.ErrNotFound) {
		return core.NewToolError(core.ToolErrorBackend, "Kubernetes resource is unavailable", err)
	}
	return core.NewToolError(core.ToolErrorBackend, "Kubernetes read failed", err)
}
func lookupDisplay(name string) (string, bool) { value, ok := displayNames[name]; return value, ok }

var displayNames = map[string]string{"get_cluster_overview": "Cluster overview", "discover_k8s_resources": "Discover Kubernetes resources", "query_k8s_resources": "Query Kubernetes resources", "get_k8s_resource": "Kubernetes resource details", "list_workloads": "List workloads", "get_workload": "Workload details", "get_k8s_topology": "Kubernetes topology", "list_k8s_events": "Kubernetes events", "get_pod_logs": "Pod logs"}
