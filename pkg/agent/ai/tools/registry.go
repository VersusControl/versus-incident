// Package tools defines shared metadata for read-only AI tool catalogs.
package tools

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// Group identifies a domain-scoped toolset.
type Group string

const (
	GroupCommon Group = "common"
	GroupVersus Group = "versus"
	GroupK8s    Group = "k8s"
)

// Entry binds a tool to the domain group that owns it.
type Entry struct {
	Group Group
	Tool  core.Tool
}

// Registry is an ordered collection of grouped tools.
type Registry []Entry

// RequirementKind identifies the external condition a tool needs before it can
// be offered to an agent.
type RequirementKind string

const (
	RequirementNone        RequirementKind = "none"
	RequirementDataSource  RequirementKind = "datasource"
	RequirementIntegration RequirementKind = "integration"
	RequirementCapability  RequirementKind = "capability"
)

// Requirement describes one prerequisite. Capability requirements may contain
// more than one capability; all listed capabilities are required.
type Requirement struct {
	Kind         RequirementKind `json:"kind"`
	SignalKind   string          `json:"signal_kind,omitempty"`
	Integration  string          `json:"integration,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
}

// Metadata is the stable, complete catalog record for a known tool. It is
// independent of runtime construction so missing dependencies remain visible.
type Metadata struct {
	Group       Group       `json:"group"`
	Name        string      `json:"name"`
	DisplayName string      `json:"display_name"`
	Description string      `json:"description"`
	DocsURL     string      `json:"docs_url,omitempty"`
	UIPath      string      `json:"ui_path,omitempty"`
	Requirement Requirement `json:"requirement"`
}

// CatalogSection identifies an operator-facing tool catalog section.
type CatalogSection string

const (
	SectionConnector  CatalogSection = "connector"
	SectionDataSource CatalogSection = "datasource"
	SectionCommon     CatalogSection = "common"
	SectionInternal   CatalogSection = "internal"
)

const (
	VisibilityAlways     = "always"
	VisibilityNonDefault = "non_default"
	VisibilityInternal   = "internal"
)

// ToolsetMetadata is the server-owned operator policy and presentation record.
// ToolNames remain hidden from the product API and are used for runtime policy.
type ToolsetMetadata struct {
	ID          string          `json:"id"`
	Section     CatalogSection  `json:"section"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	IconKey     string          `json:"icon_key"`
	ToolNames   []string        `json:"-"`
	Requirement Requirement     `json:"requirement"`
	DocsURL     string          `json:"docs_url,omitempty"`
	UIPath      string          `json:"ui_path,omitempty"`
	Visibility  string          `json:"visibility"`
	Permission  core.Permission `json:"permission,omitempty"`
}

const (
	docsTools      = "https://docs.versusincident.com/#/agent/tools/tools"
	docsVersus     = docsTools + "?id=versus-tools"
	docsKubernetes = "https://docs.versusincident.com/#/agent/tools/kubernetes"
)

// Catalog returns every known tool in UI order, including planned tools whose
// runtime implementation has not landed yet.
func Catalog() []Metadata {
	result := append([]Metadata(nil), catalog...)
	for index := range result {
		result[index].Requirement.Capabilities = append([]string(nil), result[index].Requirement.Capabilities...)
		result[index].DocsURL = safeDocsURL(result[index].DocsURL)
		result[index].UIPath = safeUIPath(result[index].UIPath)
	}
	return result
}

// Lookup returns the immutable catalog metadata for a known tool name.
func Lookup(name string) (Metadata, bool) {
	metadata, ok := metadataByName()[name]
	return metadata, ok
}

// Toolsets returns the normal operator cards in canonical section and card order.
func Toolsets() []ToolsetMetadata {
	result := append([]ToolsetMetadata(nil), toolsets...)
	for index := range result {
		result[index].ToolNames = append([]string(nil), result[index].ToolNames...)
		result[index].Requirement.Capabilities = append([]string(nil), result[index].Requirement.Capabilities...)
		result[index].DocsURL = safeDocsURL(result[index].DocsURL)
		result[index].UIPath = safeUIPath(result[index].UIPath)
	}
	return result
}

var toolsets = []ToolsetMetadata{
	{ID: "kubernetes", Section: SectionConnector, DisplayName: "Kubernetes", Description: "Inspect cluster health, workloads, resources, topology, events, and bounded pod logs.", IconKey: "kubernetes", ToolNames: []string{"get_cluster_overview", "discover_k8s_resources", "query_k8s_resources", "get_k8s_resource", "list_workloads", "get_workload", "get_k8s_topology", "list_k8s_events", "get_pod_logs"}, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}, DocsURL: docsKubernetes, UIPath: "/agent/kubernetes", Visibility: VisibilityAlways, Permission: core.PermissionInfrastructureView},
	{ID: "source-control", Section: SectionConnector, DisplayName: "Source control", Description: "Read recent source changes to correlate deployments with incidents.", IconKey: "git", ToolNames: []string{"recent_changes"}, Requirement: Requirement{Kind: RequirementIntegration, Integration: "github"}, DocsURL: "https://docs.versusincident.com/#/agent/tools/recent-changes", Visibility: VisibilityAlways},
	{ID: "logs", Section: SectionDataSource, DisplayName: "Logs", Description: "Read bounded redacted logs related to a concrete service and time window.", IconKey: "logs", ToolNames: []string{"get_related_logs"}, Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "logs"}, DocsURL: "https://docs.versusincident.com/#/agent/data-sources", UIPath: "/agent/logs", Visibility: VisibilityAlways},
	{ID: "metrics", Section: SectionDataSource, DisplayName: "Metrics", Description: "Summarize metric series for a concrete service and time window.", IconKey: "metrics", ToolNames: []string{"query_metrics"}, Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "metrics"}, DocsURL: "https://docs.versusincident.com/#/agent/data-sources/prometheus", UIPath: "/agent/metrics", Visibility: VisibilityAlways},
	{ID: "traces", Section: SectionDataSource, DisplayName: "Traces", Description: "Inspect bounded distributed traces for a concrete service and time window.", IconKey: "traces", ToolNames: []string{"query_traces"}, Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "traces"}, DocsURL: "https://docs.versusincident.com/#/agent/data-sources/traces", UIPath: "/agent/traces", Visibility: VisibilityAlways},
	{ID: "find_runbook", Section: SectionCommon, DisplayName: "Find runbook", Description: "Search indexed runbooks for operational guidance relevant to a service.", IconKey: "runbook", ToolNames: []string{"find_runbook"}, Requirement: Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}, DocsURL: "https://docs.versusincident.com/#/agent/tools/find-runbook", UIPath: "/agent/runbooks", Visibility: VisibilityAlways},
	{ID: "describe_dependencies", Section: SectionCommon, DisplayName: "Describe dependencies", Description: "Inspect the configured service dependency graph.", IconKey: "dependencies", ToolNames: []string{"describe_dependencies"}, Requirement: Requirement{Kind: RequirementCapability, Capabilities: []string{"dependency_graph"}}, DocsURL: docsTools + "?id=describe_dependencies", Visibility: VisibilityAlways},
}

var catalog = []Metadata{
	{Group: GroupVersus, Name: "get_system_overview", DisplayName: "System overview", Description: "Summarize incidents, services, patterns, and source coverage known to Versus.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "list_services", DisplayName: "List services", Description: "List the services Versus has observed and their incident activity.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "get_service", DisplayName: "Service details", Description: "Inspect one known service and its bounded reliability context.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "get_incident", DisplayName: "Incident details", Description: "Inspect one incident and its bounded analysis history.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "search_incidents", DisplayName: "Search incidents", Description: "Search the incident history visible to the current organization.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "list_patterns", DisplayName: "List patterns", Description: "List learned signal patterns and their current status.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "get_pattern", DisplayName: "Pattern details", Description: "Inspect one learned pattern with bounded redacted samples.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "list_analyses", DisplayName: "List analyses", Description: "List prior AI analyses for an incident or service.", DocsURL: docsVersus, UIPath: "/analyses", Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "get_alert_decision", DisplayName: "Alert decision", Description: "Explain the latest provider-neutral alert decision and evidence.", DocsURL: docsVersus, UIPath: "/agent/decisions", Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "list_capabilities", DisplayName: "List capabilities", Description: "List configured and available Versus capabilities and setup actions.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},
	{Group: GroupVersus, Name: "get_detection_health", DisplayName: "Detection health", Description: "Summarize configured signal coverage and dark signal categories.", DocsURL: docsVersus, Requirement: Requirement{Kind: RequirementNone}},

	{Group: GroupCommon, Name: "get_related_logs", DisplayName: "Related logs", Description: "Read bounded redacted logs related to a concrete service and time window.", DocsURL: "https://docs.versusincident.com/#/agent/data-sources", UIPath: "/agent/logs", Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "logs"}},
	{Group: GroupCommon, Name: "query_metrics", DisplayName: "Query metrics", Description: "Summarize metric series for a concrete service and time window.", DocsURL: "https://docs.versusincident.com/#/agent/data-sources/prometheus", UIPath: "/agent/metrics", Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "metrics"}},
	{Group: GroupCommon, Name: "query_traces", DisplayName: "Query traces", Description: "Inspect bounded distributed traces for a concrete service and time window.", DocsURL: "https://docs.versusincident.com/#/agent/data-sources/traces", UIPath: "/agent/traces", Requirement: Requirement{Kind: RequirementDataSource, SignalKind: "traces"}},
	{Group: GroupCommon, Name: "find_runbook", DisplayName: "Find runbook", Description: "Search indexed runbooks for operational guidance relevant to a service.", DocsURL: "https://docs.versusincident.com/#/agent/tools/find-runbook", UIPath: "/agent/runbooks", Requirement: Requirement{Kind: RequirementCapability, Capabilities: []string{"ai_embedder", "runbook_index"}}},
	{Group: GroupCommon, Name: "recent_changes", DisplayName: "Recent changes", Description: "Read recent source changes to correlate deployments with incidents.", DocsURL: "https://docs.versusincident.com/#/agent/tools/recent-changes", Requirement: Requirement{Kind: RequirementIntegration, Integration: "github"}},
	{Group: GroupCommon, Name: "describe_dependencies", DisplayName: "Describe dependencies", Description: "Inspect the configured service dependency graph.", DocsURL: docsTools + "?id=describe_dependencies", Requirement: Requirement{Kind: RequirementCapability, Capabilities: []string{"dependency_graph"}}},

	{Group: GroupK8s, Name: "get_cluster_overview", DisplayName: "Cluster overview", Description: "Summarize Kubernetes cluster health and capacity.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "discover_k8s_resources", DisplayName: "Discover Kubernetes resources", Description: "Discover readable Kubernetes API resources, versions, aliases, and scope.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "query_k8s_resources", DisplayName: "Query Kubernetes resources", Description: "Search or list bounded Kubernetes resources through discovered resource identifiers.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "get_k8s_resource", DisplayName: "Kubernetes resource details", Description: "Inspect one safely projected Kubernetes resource with optional diagnostic context.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "list_workloads", DisplayName: "List workloads", Description: "List bounded Kubernetes workloads by cluster, namespace, kind, and status.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "get_workload", DisplayName: "Workload details", Description: "Inspect one Kubernetes workload and its rollout and placement state.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "get_k8s_topology", DisplayName: "Kubernetes topology", Description: "Inspect a bounded Kubernetes ownership and routing topology.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "list_k8s_events", DisplayName: "Kubernetes events", Description: "Read bounded recent Kubernetes events with redacted messages.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
	{Group: GroupK8s, Name: "get_pod_logs", DisplayName: "Pod logs", Description: "Read bounded recent or previous logs for one Kubernetes pod, optionally selecting a container.", DocsURL: docsKubernetes, Requirement: Requirement{Kind: RequirementIntegration, Integration: "kubernetes"}},
}

func init() {
	for _, metadata := range catalog {
		if safeDocsURL(metadata.DocsURL) != metadata.DocsURL || safeUIPath(metadata.UIPath) != metadata.UIPath {
			panic(fmt.Sprintf("invalid catalog destination for %s", metadata.Name))
		}
	}
	if err := validateToolsetCatalog(); err != nil {
		panic(err)
	}
}

func validateToolsetCatalog() error {
	known := metadataByName()
	owned := make(map[string]string)
	for _, toolset := range toolsets {
		if toolset.ID == "" || toolset.DisplayName == "" || toolset.Description == "" || toolset.IconKey == "" || toolset.Visibility != VisibilityAlways {
			return fmt.Errorf("invalid toolset metadata for %q", toolset.ID)
		}
		if safeDocsURL(toolset.DocsURL) != toolset.DocsURL || safeUIPath(toolset.UIPath) != toolset.UIPath {
			return fmt.Errorf("invalid toolset destination for %s", toolset.ID)
		}
		for _, name := range toolset.ToolNames {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("%w: toolset %s references %s", ErrCatalogDrift, toolset.ID, name)
			}
			if owner, duplicate := owned[name]; duplicate {
				return fmt.Errorf("%w: %s belongs to %s and %s", ErrCatalogDrift, name, owner, toolset.ID)
			}
			owned[name] = toolset.ID
		}
	}
	for _, metadata := range catalog {
		if metadata.Group == GroupVersus {
			continue
		}
		if _, ok := owned[metadata.Name]; !ok {
			return fmt.Errorf("%w: %s has no toolset", ErrCatalogDrift, metadata.Name)
		}
	}
	return nil
}
