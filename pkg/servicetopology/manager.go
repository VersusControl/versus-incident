// Package servicetopology combines the operator-authored OSS graph with optional authorized extensions.
package servicetopology

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	MaxNodes = 500
	MaxEdges = 1000
)

// StaticGraph is the bounded projection implemented by the graph backing describe_dependencies.
type StaticGraph interface {
	Topology(maxNodes, maxEdges int) core.ServiceTopology
}

// Manager serves one independent topology graph without coupling it to Service Health collection.
type Manager struct {
	static   StaticGraph
	provider core.ServiceTopologyProvider
}

// NewManager constructs a topology manager. A nil graph and provider are a valid unavailable setup.
func NewManager(static StaticGraph, provider core.ServiceTopologyProvider) *Manager {
	if isNilInterface(static) {
		static = nil
	}
	if isNilInterface(provider) {
		provider = nil
	}
	return &Manager{static: static, provider: provider}
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

// Snapshot returns static configuration plus authorized provider additions.
// Provider failures never remove or alter static graph data.
func (manager *Manager) Snapshot(ctx context.Context, orgID string) core.ServiceTopology {
	result := emptyTopology()
	if manager == nil {
		return result
	}
	if manager.static != nil {
		result = manager.static.Topology(MaxNodes, MaxEdges)
	}
	if manager.provider == nil {
		return result
	}
	extension, err := manager.provider.ServiceTopology(ctx, core.ServiceTopologyRequest{
		OrgID: orgID, MaxNodes: remaining(MaxNodes, len(result.Nodes)), MaxEdges: remaining(MaxEdges, len(result.Edges)),
	})
	if err != nil {
		result.Extensions = &core.TopologyExtension{Availability: core.HealthError}
		if len(result.Nodes) > 0 || len(result.Edges) > 0 {
			result.Availability = core.HealthPartial
		} else {
			result.Availability = core.HealthError
		}
		return result
	}
	if extension.Availability != core.HealthReady && extension.Availability != core.HealthPartial {
		result.Extensions = &core.TopologyExtension{Availability: extension.Availability}
		if noExtensionCoverage(extension.Availability) && result.Availability == core.HealthReady && result.OmittedNodes == 0 && result.OmittedEdges == 0 {
			return result
		}
		if len(result.Nodes) > 0 || len(result.Edges) > 0 {
			result.Availability = core.HealthPartial
		} else {
			result.Availability = extension.Availability
		}
		return result
	}
	return merge(result, extension)
}

func noExtensionCoverage(availability core.HealthState) bool {
	switch availability {
	case core.HealthNotConfigured, core.HealthNoData, core.HealthRestricted:
		return true
	default:
		return false
	}
}

func remaining(limit, used int) int {
	if used >= limit {
		return 0
	}
	return limit - used
}

func emptyTopology() core.ServiceTopology {
	return core.ServiceTopology{Availability: core.HealthNotConfigured, Provenance: []string{}, Nodes: []core.ServiceTopologyNode{}, Edges: []core.ServiceTopologyEdge{}}
}

func merge(base, extension core.ServiceTopology) core.ServiceTopology {
	base.Extensions = &core.TopologyExtension{Availability: extension.Availability}
	base.OmittedNodes += extension.OmittedNodes
	base.OmittedEdges += extension.OmittedEdges
	nodes := make(map[string]struct{}, len(base.Nodes)+len(extension.Nodes))
	baseNodes := make([]core.ServiceTopologyNode, 0, len(base.Nodes))
	for _, node := range base.Nodes {
		node.Service = strings.TrimSpace(node.Service)
		if node.Service == "" {
			base.OmittedNodes++
			continue
		}
		if _, exists := nodes[node.Service]; exists {
			continue
		}
		nodes[node.Service] = struct{}{}
		baseNodes = append(baseNodes, node)
	}
	sort.Slice(baseNodes, func(i, j int) bool { return baseNodes[i].Service < baseNodes[j].Service })
	if len(baseNodes) > MaxNodes {
		base.OmittedNodes += len(baseNodes) - MaxNodes
		baseNodes = baseNodes[:MaxNodes]
	}
	base.Nodes = baseNodes
	nodes = make(map[string]struct{}, len(base.Nodes)+len(extension.Nodes))
	for _, node := range base.Nodes {
		nodes[node.Service] = struct{}{}
	}
	extensionNodes := make([]core.ServiceTopologyNode, 0, len(extension.Nodes))
	for _, node := range extension.Nodes {
		node.Service = strings.TrimSpace(node.Service)
		if node.Service == "" {
			base.OmittedNodes++
			continue
		}
		if _, exists := nodes[node.Service]; exists {
			continue
		}
		nodes[node.Service] = struct{}{}
		extensionNodes = append(extensionNodes, node)
	}
	sort.Slice(extensionNodes, func(i, j int) bool { return extensionNodes[i].Service < extensionNodes[j].Service })
	remainingNodes := MaxNodes - len(base.Nodes)
	if len(extensionNodes) > remainingNodes {
		base.OmittedNodes += len(extensionNodes) - remainingNodes
		extensionNodes = extensionNodes[:remainingNodes]
	}
	base.Nodes = append(base.Nodes, extensionNodes...)
	nodes = make(map[string]struct{}, len(base.Nodes))
	for _, node := range base.Nodes {
		nodes[node.Service] = struct{}{}
	}

	type edgeCandidate struct {
		edge   core.ServiceTopologyEdge
		static bool
	}
	candidates := make([]edgeCandidate, 0, len(base.Edges)+len(extension.Edges))
	for _, edge := range base.Edges {
		candidates = append(candidates, edgeCandidate{edge: edge, static: true})
	}
	for _, edge := range extension.Edges {
		candidates = append(candidates, edgeCandidate{edge: edge})
	}
	for index := range candidates {
		candidates[index].edge.Service = strings.TrimSpace(candidates[index].edge.Service)
		candidates[index].edge.DependsOn = strings.TrimSpace(candidates[index].edge.DependsOn)
		candidates[index].edge.Source = strings.TrimSpace(candidates[index].edge.Source)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].edge.Service != candidates[j].edge.Service {
			return candidates[i].edge.Service < candidates[j].edge.Service
		}
		if candidates[i].edge.DependsOn != candidates[j].edge.DependsOn {
			return candidates[i].edge.DependsOn < candidates[j].edge.DependsOn
		}
		return candidates[i].edge.Source < candidates[j].edge.Source
	})
	edges := make(map[string]int, len(candidates))
	validEdges := make([]core.ServiceTopologyEdge, 0, len(base.Edges)+len(extension.Edges))
	selectedStatic := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		edge := candidate.edge
		key := edge.Service + "\x00" + edge.DependsOn
		if edge.Service == "" || edge.DependsOn == "" || edge.Service == edge.DependsOn || edge.Source == "" {
			base.OmittedEdges++
			continue
		}
		if _, exists := nodes[edge.Service]; !exists {
			base.OmittedEdges++
			continue
		}
		if _, exists := nodes[edge.DependsOn]; !exists {
			base.OmittedEdges++
			continue
		}
		if index, exists := edges[key]; exists {
			if candidate.static && !selectedStatic[key] {
				validEdges[index] = edge
				selectedStatic[key] = true
			}
			continue
		}
		edges[key] = len(validEdges)
		selectedStatic[key] = candidate.static
		validEdges = append(validEdges, edge)
	}
	if len(validEdges) > MaxEdges {
		base.OmittedEdges += len(validEdges) - MaxEdges
		validEdges = validEdges[:MaxEdges]
	}
	base.Edges = validEdges
	for _, provenance := range extension.Provenance {
		provenance = strings.TrimSpace(provenance)
		if provenance != "" && !contains(base.Provenance, provenance) {
			base.Provenance = append(base.Provenance, provenance)
		}
	}
	sort.Strings(base.Provenance)
	if extension.Availability == core.HealthPartial || base.OmittedNodes > 0 || base.OmittedEdges > 0 {
		base.Availability = core.HealthPartial
	} else if len(base.Nodes) > 0 || len(base.Edges) > 0 || extension.Availability == core.HealthReady {
		base.Availability = core.HealthReady
	}
	return base
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
