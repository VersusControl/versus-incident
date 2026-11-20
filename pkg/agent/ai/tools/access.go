package tools

import (
	"context"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// accessGroups assigns every built-in tool to the access group a caller
// restriction names. Groups are finer than the catalog groups, so a caller can
// be given cost tools without incident, service or pattern history.
var accessGroups = map[string]string{
	"get_system_overview":   core.ToolGroupOverview,
	"list_capabilities":     core.ToolGroupCommon,
	"get_detection_health":  core.ToolGroupCommon,
	"list_services":         core.ToolGroupServices,
	"get_service":           core.ToolGroupServices,
	"describe_dependencies": core.ToolGroupServices,
	"describe_baseline":     core.ToolGroupServices,
	"get_incident":          core.ToolGroupIncidents,
	"search_incidents":      core.ToolGroupIncidents,
	"list_analyses":         core.ToolGroupIncidents,
	"get_alert_decision":    core.ToolGroupIncidents,
	"list_patterns":         core.ToolGroupPatterns,
	"get_pattern":           core.ToolGroupPatterns,
	"get_related_logs":      core.ToolGroupLogs,
	"discover_log_fields":   core.ToolGroupLogs,
	"read_log_records":      core.ToolGroupLogs,
	"list_log_indices":      core.ToolGroupLogs,
	"get_log_mappings":      core.ToolGroupLogs,
	"search_logs":           core.ToolGroupLogs,
	"get_log_shards":        core.ToolGroupLogs,
	"discover_metrics":      core.ToolGroupMetrics,
	"read_metric_series":    core.ToolGroupMetrics,
	"discover_trace_fields": core.ToolGroupTraces,
	"read_trace_spans":      core.ToolGroupTraces,
	"find_runbook":          core.ToolGroupRunbooks,
	"recent_changes":        core.ToolGroupChanges,
	"get_aws_connection":    core.ToolGroupAWS,
	"get_aws_changes":       core.ToolGroupAWSChanges,
}

// AccessGroup returns the access group of a tool: the group the tool names
// itself, else the built-in assignment for its name. An unknown tool has no
// group, so a restricted caller never sees it.
func AccessGroup(tool core.Tool) string {
	if tool == nil {
		return ""
	}
	if grouper, ok := tool.(core.ToolGrouper); ok {
		if group := grouper.ToolGroup(); group != "" {
			return group
		}
	}
	return AccessGroupForName(tool.Name())
}

// AccessGroupForName returns the built-in access group for a tool name, or ""
// for a name outside the built-in catalog.
func AccessGroupForName(name string) string {
	if group, ok := accessGroups[name]; ok {
		return group
	}
	if metadata, ok := Lookup(name); ok {
		switch metadata.Group {
		case GroupK8s:
			return core.ToolGroupKubernetes
		case GroupFinOps:
			return core.ToolGroupFinOps
		}
	}
	return ""
}

// CallerMayUseTool reports whether the caller's tool restriction, if any,
// allows tool.
func CallerMayUseTool(ctx context.Context, tool core.Tool) bool {
	return core.CallerToolGroupAllowed(ctx, AccessGroup(tool))
}

// FilterCallerTools drops the tools the caller's tool restriction does not
// allow. A caller without a restriction gets tools unchanged.
func FilterCallerTools(ctx context.Context, tools []core.Tool) []core.Tool {
	if core.CallerToolGroups(ctx) == nil {
		return tools
	}
	result := make([]core.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil && CallerMayUseTool(ctx, tool) {
			result = append(result, tool)
		}
	}
	return result
}

// CallerToolDenied is the result a tool call outside the caller's allowed
// groups returns instead of running.
func CallerToolDenied(name string) *core.ToolResult {
	return core.UnavailableToolResult(name, "this tool is not available to the current user")
}
