package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
)

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
		result, err = tool.service.Workloads(ctx, kubernetes.WorkloadListOptions{Namespace: args.Namespace, Kind: args.Kind, Query: args.Q, Limit: args.Limit, Cursor: args.Cursor})
		data = compactWorkloadPage(result, args.Cursor)
		found = len(result.Items) > 0
	case "get_workload":
		if _, ok := workloadResourceID(args.Kind); !ok || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			data, err = tool.service.GetWorkload(ctx, args.Namespace, args.Kind, args.Name)
		}
	case "list_k8s_events":
		var result kubernetes.EventPage
		limit := args.Limit
		if limit <= 0 || limit > 5 {
			limit = 5
		}
		result, err = tool.service.ListEvents(ctx, kubernetes.EventOptions{Namespace: args.Namespace, Type: args.EventType, Kind: args.ObjectKind, Name: args.ObjectName, UID: args.ObjectUID, Continue: args.Continue, Cursor: args.Cursor, Limit: limit})
		data = compactEventPage(result)
		found = len(result.Items) > 0
	case "get_pod_logs":
		if args.Namespace == "" || args.Name == "" || args.SinceSeconds < 0 || args.SinceSeconds > 86400 || args.TailLines < 0 || args.TailLines > 5000 {
			err = kubernetes.ErrInvalidArguments
		} else {
			data, err = tool.service.PodLogs(ctx, args.Namespace, args.Name, args.Container, args.Previous, args.SinceSeconds, args.TailLines)
		}
	case "get_workload_logs":
		if args.Namespace == "" || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.WorkloadLogs
			result, err = tool.service.WorkloadLogs(ctx, kubernetes.WorkloadLogOptions{
				Namespace: args.Namespace, Kind: args.Kind, Name: args.Name, Pod: args.Pod, Container: args.Container,
				Previous: args.Previous, SinceSeconds: args.SinceSeconds, TailLines: args.TailLines, Grep: args.Grep, MaxPods: args.MaxPods,
			})
			if err == nil {
				data = compactWorkloadLogs(result, tool.service.Scope(), args.Namespace, args.Kind, args.Name)
				found = len(result.Lines) > 0
			}
		}
	case "list_k8s_issues":
		var result kubernetes.IssuePage
		result, err = tool.service.Issues(ctx, kubernetes.IssueOptions{Namespace: args.Namespace, Severity: args.Severity, Kind: args.Kind, Limit: args.Limit, Cursor: args.Cursor})
		if err == nil {
			data = compactIssues(result, tool.service.Scope(), args.Cursor)
			found = len(result.Items) > 0
		}
	case "get_k8s_changes":
		minutes := args.SinceMinutes
		if minutes == 0 {
			minutes = 60
		}
		if minutes < 1 || minutes > 1440 {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.ChangePage
			result, err = tool.service.Changes(ctx, kubernetes.ChangeQuery{
				Since: time.Now().UTC().Add(-time.Duration(minutes) * time.Minute), Until: time.Now().UTC(),
				Namespace: args.Namespace, Kind: args.Kind, Name: args.Name, Type: kubernetes.ChangeType(args.Type),
				Limit: args.Limit, Cursor: args.Cursor,
			})
			if err == nil {
				data = compactChanges(result, tool.service.Scope(), args.Cursor)
				found = len(result.Items) > 0
			}
		}
	case "get_k8s_neighborhood":
		mode := args.Mode
		if mode == "" {
			mode = "focus"
		}
		var result kubegraph.Graph
		if mode == "namespace" {
			if args.Namespace == "" {
				err = kubernetes.ErrInvalidArguments
			} else {
				result, err = tool.service.Graph(ctx, kubernetes.GraphQuery{Namespace: args.Namespace, GroupBy: "app", MaxNodes: args.MaxNodes, Limit: args.MaxNodes, Cursor: args.Cursor, ConnectedOnly: args.ConnectedOnly})
			}
		} else if mode != "focus" || args.Name == "" || args.Kind == "" && args.ResourceID == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			result, err = tool.service.Neighborhood(ctx, kubernetes.NeighborhoodQuery{ResourceID: args.ResourceID, Kind: args.Kind, Namespace: args.Namespace, Name: args.Name, Hops: args.Hops, MaxNodes: args.MaxNodes})
		}
		if err == nil {
			data = compactGraph(result, tool.service.Scope())
			found = len(result.Nodes) > 0
			if args.IncludeTraffic {
				traffic, trafficErr := tool.service.Traffic(ctx, kubernetes.TrafficQuery{Namespace: args.Namespace, Source: args.Source, Window: args.Window})
				if trafficErr != nil {
					err = trafficErr
				} else {
					data = compactGraphTraffic(data.(map[string]any), compactTraffic(traffic))
					found = traffic.Available || found
				}
			}
		}
	case "top_k8s_resources":
		var result kubernetes.TopPage
		result, err = tool.service.Top(ctx, kubernetes.TopOptions{Kind: args.Kind, Namespace: args.Namespace, Sort: args.Sort, Limit: args.Limit})
		if err == nil {
			data = compactTop(result, tool.service.Scope())
			found = len(result.Items) > 0
		}
	case "list_gitops_apps":
		var result kubernetes.GitOpsAppPage
		result, err = tool.service.GitOpsApps(ctx, kubernetes.GitOpsAppOptions{Namespace: args.Namespace, Tool: args.Tool, Status: args.Status, Limit: args.Limit, Cursor: args.Cursor})
		if err == nil {
			data = compactGitOpsApps(result, tool.service.Scope(), args.Cursor)
			found = result.Available && len(result.Items) > 0
		}
	case "get_rollout_status":
		if args.Namespace == "" || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.Rollout
			result, err = tool.service.Rollout(ctx, args.Namespace, args.Name)
			if err == nil {
				data = compactRolloutStatus(result, tool.service.Scope())
			}
		}
	case "list_rollouts":
		var result kubernetes.RolloutPage
		limit := args.Limit
		if limit <= 0 || limit > 3 {
			limit = 3
		}
		result, err = tool.service.Rollouts(ctx, kubernetes.RolloutListOptions{Namespace: args.Namespace, Limit: limit, Cursor: args.Cursor})
		if err == nil {
			data = compactRollouts(result, tool.service.Scope())
			found = result.Available && len(result.Items) > 0
		}
	case "list_helm_releases":
		var result kubernetes.HelmReleasePage
		result, err = tool.service.Releases(ctx, kubernetes.HelmReleaseOptions{Namespace: args.Namespace, Status: args.Status, Limit: args.Limit, Cursor: args.Cursor})
		if err == nil {
			data = compactHelmReleasePage(result, tool.service.Scope(), args.Cursor)
			found = len(result.Items) > 0
		}
	case "get_helm_release":
		if args.Namespace == "" || args.Name == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.HelmRelease
			result, err = tool.service.Release(ctx, args.Namespace, args.Name)
			if err == nil {
				data = compactHelmRelease(result, tool.service.Scope())
			}
		}
	case "diagnose_k8s_workload":
		if args.Namespace == "" || args.Name == "" || args.Kind == "" && args.ResourceID == "" {
			err = kubernetes.ErrInvalidArguments
		} else {
			var result kubernetes.Diagnosis
			result, err = tool.service.DiagnoseWorkload(ctx, kubernetes.DiagnoseOptions{ResourceID: args.ResourceID, Kind: args.Kind, Namespace: args.Namespace, Name: args.Name, LogTail: args.LogTail, ChangeWindow: time.Duration(args.ChangeWindowMinutes) * time.Minute})
			if err == nil {
				data = compactDiagnosis(result, tool.service.Scope())
			}
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
	payload := compactModelView(data, args, tool.service.Scope())
	return &core.ToolResult{Tool: tool.name, Found: found, Data: payload}, nil
}

type arguments struct {
	ResourceID          string `json:"resource_id"`
	Namespace           string `json:"namespace"`
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	Container           string `json:"container"`
	Labels              string `json:"labels"`
	Fields              string `json:"fields"`
	Continue            string `json:"continue"`
	Limit               int    `json:"limit"`
	Previous            bool   `json:"previous"`
	SinceSeconds        int    `json:"since_seconds"`
	TailLines           int    `json:"tail_lines"`
	Diagnostic          bool   `json:"diagnostic"`
	Query               string `json:"query"`
	Q                   string `json:"q"`
	PerKindLimit        int    `json:"per_kind_limit"`
	Category            string `json:"category"`
	ObjectKind          string `json:"object_kind"`
	ObjectName          string `json:"object_name"`
	ObjectUID           string `json:"object_uid"`
	EventType           string `json:"event_type"`
	Pod                 string `json:"pod"`
	Grep                string `json:"grep"`
	MaxPods             int    `json:"max_pods"`
	Cursor              string `json:"cursor"`
	SinceMinutes        int    `json:"since_minutes"`
	Mode                string `json:"mode"`
	Hops                int    `json:"hops"`
	MaxNodes            int    `json:"max_nodes"`
	ChangeWindowMinutes int    `json:"change_window_minutes"`
	Severity            string `json:"severity"`
	Type                string `json:"type"`
	Sort                string `json:"sort"`
	Tool                string `json:"tool"`
	Status              string `json:"status"`
	LogTail             int    `json:"log_tail"`
	IncludeTraffic      bool   `json:"include_traffic"`
	ConnectedOnly       bool   `json:"connected_only"`
	Source              string `json:"source"`
	Window              string `json:"window"`
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

func compactWorkloadLogs(result kubernetes.WorkloadLogs, scope kubernetes.Scope, namespace, kind, name string) map[string]any {
	lines := append([]kubernetes.WorkloadLogLine(nil), result.Lines...)
	sort.SliceStable(lines, func(i, j int) bool {
		leftError := logLineLooksImportant(lines[i].Text)
		rightError := logLineLooksImportant(lines[j].Text)
		if leftError != rightError {
			return leftError
		}
		if !lines[i].At.Equal(lines[j].At) {
			return lines[i].At.After(lines[j].At)
		}
		if lines[i].Pod != lines[j].Pod {
			return lines[i].Pod < lines[j].Pod
		}
		return lines[i].Text < lines[j].Text
	})
	if len(lines) > 20 {
		lines = lines[:20]
	}
	baseTruncated := result.Truncated || len(lines) < len(result.Lines)
	for {
		omittedLines := len(result.Lines) - len(lines)
		data := map[string]any{
			"pods": result.Pods, "lines": lines, "truncated": baseTruncated || omittedLines > 0,
			"omitted_pods": result.OmittedPods, "partial_failures": result.Partial,
			"omitted_lines": omittedLines, "sync": map[string]any{"state": "direct", "partial": len(result.Partial) > 0 || baseTruncated},
			"ref":   "k8s://" + scope.ClusterID + "/" + namespace + "/" + kind + "/" + name,
			"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(kind+"/"+namespace+"/"+name)},
			"hint":  "Narrow with pod, grep, or since_seconds for more context.",
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(lines) == 0 {
			return data
		}
		lines = lines[:len(lines)-1]
	}
}

func compactWorkloadPage(result kubernetes.WorkloadPage, cursor string) map[string]any {
	items := append([]kubernetes.ProjectedResource(nil), result.Items...)
	truncated := result.Truncated || len(items) > 15
	if len(items) > 15 {
		items = items[:15]
	}
	for {
		data := map[string]any{"items": items, "counts": result.Counts, "truncated": truncated}
		if len(result.Partial) > 0 {
			data["partial_failures"] = result.Partial
		}
		if len(result.Omitted) > 0 {
			data["omitted_categories"] = result.Omitted
		}
		next := result.Next
		if len(items) < len(result.Items) {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		}
		if next != "" {
			data["next"] = next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

func compactEventPage(result kubernetes.EventPage) map[string]any {
	items := make([]map[string]any, 0, len(result.Items))
	for _, event := range result.Items {
		summary := make(map[string]any)
		for _, key := range []string{"type", "reason", "message", "count", "involved_object", "lastTimestamp", "firstTimestamp"} {
			if value, exists := event.Summary[key]; exists {
				summary[key] = value
			}
		}
		items = append(items, map[string]any{"namespace": event.Namespace, "name": event.Name, "summary": summary})
	}
	data := map[string]any{"items": items, "truncated": result.Truncated}
	if result.Next != "" {
		data["next"] = result.Next
	}
	if len(result.Partial) > 0 {
		data["partial_failures"] = result.Partial
	}
	return data
}

func compactIssues(result kubernetes.IssuePage, scope kubernetes.Scope, cursor string) map[string]any {
	items := append([]kubernetes.Issue(nil), result.Items...)
	truncated := result.Truncated || len(items) > 15
	if len(items) > 15 {
		items = items[:15]
	}
	for index := range items {
		if len(items[index].Examples) > 2 {
			items[index].Examples = items[index].Examples[:2]
		}
	}
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, issue := range items {
			ref := "k8s://" + scope.ClusterID + "/" + issue.Root.Namespace + "/" + issue.Root.Kind + "/" + issue.Root.Name
			modelItems = append(modelItems, map[string]any{
				"root": issue.Root, "rule": issue.Rule, "severity": issue.Severity, "count": issue.Count,
				"examples": issue.Examples, "first_seen": issue.FirstSeen, "last_seen": issue.LastSeen,
				"ref": ref, "links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(issue.Root.Kind+"/"+issue.Root.Namespace+"/"+issue.Root.Name)},
			})
		}
		next := result.Next
		if len(items) < len(result.Items) {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		}
		data := map[string]any{
			"items": modelItems, "totals": result.Totals, "truncated": truncated,
			"partial_failures": result.Partial, "sync": result.Sync,
			"hint": "Use diagnose_k8s_workload to inspect one root workload.",
		}
		if next != "" {
			data["next"] = next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		removedExample := false
		for index := len(items) - 1; index >= 0; index-- {
			if len(items[index].Examples) > 0 {
				items[index].Examples = items[index].Examples[:len(items[index].Examples)-1]
				removedExample = true
				break
			}
		}
		if !removedExample {
			items = items[:len(items)-1]
			truncated = true
		}
	}
}

func compactChanges(result kubernetes.ChangePage, scope kubernetes.Scope, cursor string) map[string]any {
	items := append([]kubernetes.Change(nil), result.Items...)
	truncated := result.Truncated || result.Next != ""
	if len(items) > 12 {
		items = items[:12]
		truncated = true
	}
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, change := range items {
			fields := append([]kubernetes.ChangeFieldChange(nil), change.Fields...)
			if len(fields) > 2 {
				fields = fields[:2]
			}
			modelItems = append(modelItems, map[string]any{
				"at": change.At, "kind": change.Kind, "namespace": change.Namespace, "name": change.Name,
				"type": change.Type, "fields": fields, "service": change.Service,
				"ref":   "k8s://" + scope.ClusterID + "/" + change.Namespace + "/" + change.Kind + "/" + change.Name,
				"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(change.Kind+"/"+change.Namespace+"/"+change.Name)},
			})
		}
		next := result.Next
		if len(items) < len(result.Items) {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		}
		data := map[string]any{"items": modelItems, "gaps": result.Gaps, "sync": result.Sync, "truncated": truncated, "hint": "Use diagnose_k8s_workload for one object and its surrounding evidence."}
		if next != "" {
			data["next"] = next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		if len(items) > 0 && len(items[len(items)-1].Fields) > 0 {
			items[len(items)-1].Fields = items[len(items)-1].Fields[:len(items[len(items)-1].Fields)-1]
		} else {
			items = items[:len(items)-1]
		}
		truncated = true
	}
}

func compactGraph(result kubegraph.Graph, scope kubernetes.Scope) map[string]any {
	nodes := append([]kubegraph.Node(nil), result.Nodes...)
	truncated := len(nodes) > 18 || len(result.Omitted) > 0
	if len(nodes) > 18 {
		nodes = nodes[:18]
	}
	for {
		selected := make(map[string]bool, len(nodes))
		modelNodes := make([]map[string]any, 0, len(nodes))
		for _, node := range nodes {
			selected[node.ID] = true
			modelNodes = append(modelNodes, map[string]any{
				"id": node.ID, "kind": node.Kind, "namespace": node.Namespace, "name": node.Name,
				"health": node.Health, "group": node.Group,
				"ref":   "k8s://" + scope.ClusterID + "/" + node.Namespace + "/" + node.Kind + "/" + node.Name,
				"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(node.Kind+"/"+node.Namespace+"/"+node.Name)},
			})
		}
		modelEdges := make([]kubegraph.Edge, 0, min(len(result.Edges), 24))
		for _, edge := range result.Edges {
			if selected[edge.From] && selected[edge.To] && len(modelEdges) < 24 {
				modelEdges = append(modelEdges, edge)
			}
		}
		data := map[string]any{"nodes": modelNodes, "edges": modelEdges, "omitted": result.Omitted, "sync": result.Sync, "truncated": truncated || result.Truncated, "hint": "Use focus mode on a related node to expand one bounded neighborhood."}
		if result.Next != "" {
			data["next"] = result.Next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(nodes) == 0 {
			return data
		}
		nodes = nodes[:len(nodes)-1]
		truncated = true
	}
}

func compactModelView(data any, args arguments, scope kubernetes.Scope) map[string]any {
	encoded, err := json.Marshal(data)
	if err != nil {
		return map[string]any{"status": "unavailable", "sync": map[string]any{"state": "direct", "partial": true}}
	}
	var view map[string]any
	if err := json.Unmarshal(encoded, &view); err != nil || view == nil {
		view = map[string]any{"result": json.RawMessage(encoded)}
	}
	partial, _ := view["truncated"].(bool)
	if _, ok := view["sync"]; !ok {
		view["sync"] = map[string]any{"state": "direct", "partial": partial}
	}
	kind, namespace, resourceName := args.Kind, args.Namespace, args.Name
	if kind == "" {
		kind = kindFromResourceID(args.ResourceID)
	}
	if kind != "" && resourceName != "" {
		view["ref"] = kubeResourceRef(scope, namespace, kind, resourceName)
		view["links"] = map[string]string{"ui": kubeResourceLink(kind, namespace, resourceName)}
	}
	addResourceLinks(view, scope)
	if modelViewSize(view) <= 6<<10 {
		return view
	}
	view["truncated"] = true
	if _, ok := view["next"]; !ok {
		next, _ := view["continue"].(string)
		if next == "" {
			next, _ = view["cursor"].(string)
		}
		if next == "" {
			next = "narrow filters or request one resource"
		}
		view["next"] = next
	}
	for modelViewSize(view) > 6<<10 {
		if trimOneModelArray(view) || trimOneModelString(view) || removeOneModelField(view) {
			continue
		}
		return minimalModelView(view)
	}
	return view
}

func modelViewSize(view map[string]any) int {
	encoded, _ := json.Marshal(view)
	return len(encoded)
}

func trimOneModelArray(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		keys := sortedMapKeys(current)
		for _, key := range keys {
			if items, ok := current[key].([]any); ok && len(items) > 0 {
				current[key] = items[:len(items)-1]
				return true
			}
			if trimOneModelArray(current[key]) {
				return true
			}
		}
	case []any:
		for _, item := range current {
			if trimOneModelArray(item) {
				return true
			}
		}
	}
	return false
}

func trimOneModelString(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		keys := sortedMapKeys(current)
		for _, key := range keys {
			if text, ok := current[key].(string); ok && len(text) > 128 && key != "ref" {
				runes := []rune(text)
				current[key] = string(runes[:len(runes)/2])
				return true
			}
			if trimOneModelString(current[key]) {
				return true
			}
		}
	case []any:
		for _, item := range current {
			if trimOneModelString(item) {
				return true
			}
		}
	}
	return false
}

func removeOneModelField(view map[string]any) bool {
	for _, key := range sortedMapKeys(view) {
		switch key {
		case "sync", "ref", "links", "hint", "truncated", "next", "totals", "total", "count":
			continue
		default:
			delete(view, key)
			omitted, _ := view["omitted_categories"].([]any)
			view["omitted_categories"] = append(omitted, key)
			return true
		}
	}
	return false
}

func minimalModelView(view map[string]any) map[string]any {
	minimal := map[string]any{
		"sync": view["sync"], "truncated": true,
		"omitted_categories": []string{"model_view_budget"},
		"hint":               "Use narrower filters or request one resource.",
	}
	if ref, ok := view["ref"]; ok {
		minimal["ref"] = ref
	}
	if links, ok := view["links"]; ok {
		minimal["links"] = links
	}
	return minimal
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func addResourceLinks(view map[string]any, scope kubernetes.Scope) {
	for _, key := range []string{"items", "resources", "pods", "nodes", "events"} {
		items, ok := view[key].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			resource, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := resource["kind"].(string)
			namespace, _ := resource["namespace"].(string)
			resourceName, _ := resource["name"].(string)
			if kind == "" || resourceName == "" {
				continue
			}
			resource["ref"] = kubeResourceRef(scope, namespace, kind, resourceName)
			resource["links"] = map[string]string{"ui": kubeResourceLink(kind, namespace, resourceName)}
		}
	}
}

func kindFromResourceID(resourceID string) string {
	for kind, candidate := range map[string]string{
		"Pod": "core~v1~pods", "Node": "core~v1~nodes", "Deployment": "apps~v1~deployments",
		"ReplicaSet": "apps~v1~replicasets", "StatefulSet": "apps~v1~statefulsets", "DaemonSet": "apps~v1~daemonsets",
		"Job": "batch~v1~jobs", "CronJob": "batch~v1~cronjobs", "Service": "core~v1~services",
	} {
		if resourceID == candidate {
			return kind
		}
	}
	return ""
}

func kubeResourceRef(scope kubernetes.Scope, namespace, kind, name string) string {
	return "k8s://" + scope.ClusterID + "/" + namespace + "/" + kind + "/" + name
}

func kubeResourceLink(kind, namespace, name string) string {
	return "/agent/kubernetes?r=" + url.QueryEscape(kind+"/"+namespace+"/"+name)
}

func compactTraffic(result kubernetes.Traffic) map[string]any {
	edges := make([]map[string]any, 0, min(len(result.Edges), 10))
	for _, edge := range result.Edges[:min(len(result.Edges), 10)] {
		edges = append(edges, map[string]any{
			"from": edge.From, "to": edge.To, "rate_per_sec": edge.RatePerSec,
			"error_rate": edge.ErrorRate, "p95_ms": edge.P95Ms, "bytes_per_sec": edge.BytesPerSec,
		})
	}
	return map[string]any{
		"available": result.Available, "reason": result.Reason, "source": result.Source,
		"window": result.Window, "observed_at": result.ObservedAt, "edges": edges,
		"unmapped": result.Unmapped, "external": result.External,
		"truncated": result.Truncated || len(result.Edges) > len(edges),
	}
}

func compactGraphTraffic(graph, traffic map[string]any) map[string]any {
	for {
		combined := make(map[string]any, len(graph)+1)
		for key, value := range graph {
			combined[key] = value
		}
		combined["traffic"] = traffic
		encoded, _ := json.Marshal(combined)
		if len(encoded) <= 6<<10 {
			return combined
		}
		if edges, ok := traffic["edges"].([]map[string]any); ok && len(edges) > 0 {
			traffic["edges"] = edges[:len(edges)-1]
			traffic["truncated"] = true
			continue
		}
		if nodes, ok := graph["nodes"].([]map[string]any); ok && len(nodes) > 0 {
			nodes = nodes[:len(nodes)-1]
			graph["nodes"] = nodes
			graph["truncated"] = true
			included := make(map[string]bool, len(nodes))
			for _, node := range nodes {
				if id, ok := node["id"].(string); ok {
					included[id] = true
				}
			}
			if edges, ok := graph["edges"].([]kubegraph.Edge); ok {
				filtered := edges[:0]
				for _, edge := range edges {
					if included[edge.From] && included[edge.To] {
						filtered = append(filtered, edge)
					}
				}
				graph["edges"] = filtered
			}
			continue
		}
		return map[string]any{"traffic": map[string]any{"available": false, "truncated": true}, "truncated": true}
	}
}

func compactTop(result kubernetes.TopPage, scope kubernetes.Scope) map[string]any {
	items := append([]kubernetes.TopItem(nil), result.Items...)
	truncated := result.Truncated || len(items) > 15
	if len(items) > 15 {
		items = items[:15]
	}
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, item := range items {
			kind := "Pod"
			if strings.Contains(strings.ToLower(item.Kind), "node") {
				kind = "Node"
			}
			ref := "k8s://" + scope.ClusterID + "/" + item.Namespace + "/" + kind + "/" + item.Name
			modelItems = append(modelItems, map[string]any{
				"kind": kind, "namespace": item.Namespace, "name": item.Name, "cpu": item.CPU, "memory": item.Memory,
				"request_cpu": item.RequestCPU, "request_memory": item.RequestMemory, "owner": item.Owner,
				"ref": ref, "links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(kind+"/"+item.Namespace+"/"+item.Name)},
			})
		}
		data := map[string]any{
			"items": modelItems, "total": result.Total, "truncated": truncated, "availability": result.Availability,
			"fresh": result.Fresh, "partial_failures": result.Partial, "sync": result.Sync,
			"hint": "Use get_workload for rollout details or get_k8s_resource for one resource.",
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

func compactGitOpsApps(result kubernetes.GitOpsAppPage, scope kubernetes.Scope, cursor string) map[string]any {
	items := append([]kubernetes.GitOpsApp(nil), result.Items...)
	truncated := result.Truncated || len(items) > 15
	if len(items) > 15 {
		items = items[:15]
	}
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, app := range items {
			kind := app.Kind
			if kind == "" {
				kind = "Application"
			}
			modelItems = append(modelItems, map[string]any{
				"tool": app.Tool, "kind": kind, "namespace": app.Namespace, "name": app.Name, "sync": app.Sync,
				"health": app.Health, "revision": app.Revision, "last_sync_at": app.LastSyncAt,
				"message": app.Message, "source": app.Source, "suspended": app.Suspended,
				"ref":   "k8s://" + scope.ClusterID + "/" + app.Namespace + "/" + kind + "/" + app.Name,
				"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(kind+"/"+app.Namespace+"/"+app.Name)},
			})
		}
		data := map[string]any{
			"items": modelItems, "available": result.Available, "reason": result.Reason,
			"truncated": truncated, "partial_failures": result.Partial,
			"sync": map[string]any{"state": "direct", "partial": len(result.Partial) > 0 || truncated},
			"hint": "Use get_k8s_resource for one discovered GitOps object.",
		}
		next := result.Next
		if len(items) < len(result.Items) {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		}
		if next != "" {
			data["next"] = next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

func compactRolloutStatus(result kubernetes.Rollout, scope kubernetes.Scope) map[string]any {
	kind, namespace, name := "Rollout", result.Namespace, result.Name
	return map[string]any{
		"rollout": result, "sync": map[string]any{"state": "direct", "partial": false},
		"ref":   "k8s://" + scope.ClusterID + "/" + namespace + "/" + kind + "/" + name,
		"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(kind+"/"+namespace+"/"+name)},
		"hint":  "Use diagnose_k8s_workload for related events, logs and workload context.",
	}
}

func compactRollouts(result kubernetes.RolloutPage, scope kubernetes.Scope) map[string]any {
	items := append([]kubernetes.Rollout(nil), result.Items...)
	truncated := result.Truncated
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, rollout := range items {
			modelItems = append(modelItems, map[string]any{
				"namespace": rollout.Namespace, "name": rollout.Name, "phase": rollout.Phase, "strategy": rollout.Strategy,
				"step": rollout.Step, "total_steps": rollout.TotalSteps, "weight": rollout.Weight,
				"ref": "k8s://" + scope.ClusterID + "/" + rollout.Namespace + "/Rollout/" + rollout.Name,
			})
		}
		data := map[string]any{
			"items": modelItems, "available": result.Available, "reason": result.Reason,
			"truncated": truncated, "partial_failures": result.Partial,
			"sync": map[string]any{"state": "direct", "partial": len(result.Partial) > 0 || truncated},
		}
		if result.Next != "" {
			data["next"] = result.Next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

func compactHelmReleasePage(result kubernetes.HelmReleasePage, scope kubernetes.Scope, cursor string) map[string]any {
	items := append([]kubernetes.HelmRelease(nil), result.Items...)
	truncated := result.Truncated || len(items) > 12
	if len(items) > 12 {
		items = items[:12]
	}
	for index := range items {
		if len(items[index].History) > 5 {
			items[index].History = items[index].History[:5]
			truncated = true
		}
	}
	for {
		modelItems := make([]map[string]any, 0, len(items))
		for _, release := range items {
			modelItems = append(modelItems, helmReleaseModel(release, scope))
		}
		data := map[string]any{"items": modelItems, "truncated": truncated}
		if len(result.Partial) > 0 {
			data["partial_failures"] = result.Partial
		}
		next := result.Next
		if len(items) < len(result.Items) {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		} else if next == "" && truncated {
			start, _ := strconv.Atoi(cursor)
			next = strconv.Itoa(start + len(items))
		}
		if next != "" {
			data["next"] = next
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(items) == 0 {
			return data
		}
		items = items[:len(items)-1]
		truncated = true
	}
}

func compactHelmRelease(release kubernetes.HelmRelease, scope kubernetes.Scope) map[string]any {
	if len(release.History) > 20 {
		release.History = release.History[:20]
	}
	return map[string]any{
		"release": helmReleaseModel(release, scope), "sync": map[string]any{"state": "direct", "partial": len(release.History) == 20},
		"hint": "Release status and history are derived from Secret metadata labels only.",
	}
}

func compactHelmReleases(releases []kubernetes.HelmRelease, scope kubernetes.Scope) map[string]any {
	return compactHelmReleasePage(kubernetes.HelmReleasePage{Items: releases}, scope, "")
}

func helmReleaseModel(release kubernetes.HelmRelease, scope kubernetes.Scope) map[string]any {
	return map[string]any{
		"namespace": release.Namespace, "name": release.Name, "current": release.Current,
		"history": release.History, "health": release.Health,
		"ref":   "k8s://" + scope.ClusterID + "/" + release.Namespace + "/Release/" + release.Name,
		"links": map[string]string{"ui": "/agent/kubernetes?tab=releases&namespace=" + url.QueryEscape(release.Namespace)},
	}
}

func compactDiagnosis(result kubernetes.Diagnosis, scope kubernetes.Scope) map[string]any {
	logs := []string{}
	if result.WorstPodLogs != nil {
		logs = strings.Split(strings.TrimSpace(result.WorstPodLogs.Text), "\n")
		if len(logs) > 40 {
			logs = logs[len(logs)-40:]
		}
		sort.SliceStable(logs, func(i, j int) bool { return logLineLooksImportant(logs[i]) && !logLineLooksImportant(logs[j]) })
	}
	events := make([]map[string]any, 0, min(len(result.WarningEvents), 8))
	for _, event := range result.WarningEvents[:min(len(result.WarningEvents), 8)] {
		events = append(events, map[string]any{"name": event.Name, "reason": event.Summary["reason"], "count": event.Summary["count"], "last_seen": event.Summary["lastTimestamp"]})
	}
	pods := append([]kubernetes.WorkloadPod(nil), result.Workload.Pods...)
	if len(pods) > 10 {
		pods = pods[:10]
	}
	worstPod := ""
	if result.WorstPodLogs != nil {
		worstPod = result.WorstPodLogs.Pod
	}
	changes := append([]kubernetes.Change(nil), result.Changes...)
	if len(changes) > 3 {
		changes = changes[:3]
	}
	for index := range changes {
		if len(changes[index].Fields) > 1 {
			changes[index].Fields = changes[index].Fields[:1]
		}
		for fieldIndex := range changes[index].Fields {
			changes[index].Fields[fieldIndex].From = boundedChangeField(changes[index].Fields[fieldIndex].From)
			changes[index].Fields[fieldIndex].To = boundedChangeField(changes[index].Fields[fieldIndex].To)
		}
	}
	neighborhood := compactGraph(result.Neighborhood, scope)
	var neighborhoodNodes []map[string]any
	if nodes, ok := neighborhood["nodes"].([]map[string]any); ok {
		neighborhoodNodes = nodes
	}
	if len(neighborhoodNodes) > 6 {
		neighborhoodNodes = neighborhoodNodes[:6]
		neighborhood["nodes"] = neighborhoodNodes
		neighborhood["truncated"] = true
	}
	if edges, ok := neighborhood["edges"].([]kubegraph.Edge); ok && len(edges) > 8 {
		neighborhood["edges"] = edges[:8]
		neighborhood["truncated"] = true
	}
	truncated := result.Truncated
	syncPartial := result.Sync.Partial
	for {
		data := map[string]any{
			"workload": map[string]any{
				"kind": result.Workload.Kind, "namespace": result.Workload.Namespace, "name": result.Workload.Name,
				"desired": result.Workload.Desired, "ready": result.Workload.Ready, "available": result.Workload.Available,
				"conditions": result.Workload.Conditions, "pods": pods,
			},
			"warning_events": events, "worst_pod": worstPod, "changes": changes,
			"neighborhood": neighborhood,
			"logs":         logs, "truncated": truncated, "omitted_categories": result.Omitted,
			"partial_failures": result.Partial, "sync": map[string]any{"state": result.Sync.State, "partial": syncPartial},
			"ref":   "k8s://" + scope.ClusterID + "/" + result.Workload.Namespace + "/" + result.Workload.Kind + "/" + result.Workload.Name,
			"links": map[string]string{"ui": "/agent/kubernetes?r=" + url.QueryEscape(result.Workload.Kind+"/"+result.Workload.Namespace+"/"+result.Workload.Name)},
			"hint":  "Use get_workload_logs for more log lines; this bundle contains evidence only.",
		}
		encoded, _ := json.Marshal(data)
		if len(encoded) <= 6<<10 || len(logs) == 0 && len(events) == 0 && len(pods) == 0 && len(changes) == 0 && len(neighborhoodNodes) == 0 {
			return data
		}
		if len(logs) > 0 {
			logs = logs[:len(logs)-1]
			truncated, syncPartial = true, true
		} else if len(events) > 0 {
			events = events[:len(events)-1]
			truncated, syncPartial = true, true
		} else if len(pods) > 0 {
			pods = pods[:len(pods)-1]
			truncated, syncPartial = true, true
		} else {
			if len(changes) > 0 {
				changes = changes[:len(changes)-1]
			} else {
				neighborhoodNodes = neighborhoodNodes[:len(neighborhoodNodes)-1]
				neighborhood["nodes"] = neighborhoodNodes
				included := make(map[string]bool, len(neighborhoodNodes))
				for _, node := range neighborhoodNodes {
					if id, ok := node["id"].(string); ok {
						included[id] = true
					}
				}
				if edges, ok := neighborhood["edges"].([]kubegraph.Edge); ok {
					filtered := edges[:0]
					for _, edge := range edges {
						if included[edge.From] && included[edge.To] {
							filtered = append(filtered, edge)
						}
					}
					neighborhood["edges"] = filtered
				}
			}
			truncated, syncPartial = true, true
		}
	}
}

func boundedChangeField(value string) string {
	runes := []rune(value)
	if len(runes) > 128 {
		runes = runes[:128]
	}
	return string(runes)
}

func logLineLooksImportant(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{"error", "panic", "exception", "fatal"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func lookupDisplay(name string) (string, bool) { value, ok := displayNames[name]; return value, ok }
