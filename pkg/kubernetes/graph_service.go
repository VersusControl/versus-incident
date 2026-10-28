package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"unicode/utf8"

	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type GraphQuery struct {
	Namespace     string
	GroupBy       string
	MaxNodes      int
	ConnectedOnly bool
	Complete      bool
	Limit         int
	Cursor        string
	unpaged       bool
}

const maxCompleteGraphEdges = 20000
const maxCompleteGraphResponseBytes = 8 << 20

type NeighborhoodQuery struct {
	ResourceID string
	Kind       string
	Namespace  string
	Name       string
	Hops       int
	MaxNodes   int
}

var graphIndexKinds = []string{
	"Node", "Namespace", "Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet",
	"Job", "CronJob", "Service", "Ingress", "ConfigMap", "Secret", "PersistentVolumeClaim",
	"HorizontalPodAutoscaler",
	"HTTPRoute", "Gateway",
}

// Graph builds a bounded topology graph from the shared projected index.
func (service *Service) Graph(ctx context.Context, query GraphQuery) (kubegraph.Graph, error) {
	if query.Namespace != "" && !safeSegment(query.Namespace) || query.GroupBy != "" && query.GroupBy != "namespace" && query.GroupBy != "app" {
		return kubegraph.Graph{}, ErrInvalidArguments
	}
	if query.Complete {
		if query.Namespace == "" || query.Cursor != "" || query.Limit != 0 || query.MaxNodes != 0 {
			return kubegraph.Graph{}, ErrInvalidArguments
		}
		return service.completeGraph(ctx, query)
	}
	if len(query.Cursor) > 4096 {
		return kubegraph.Graph{}, ErrInvalidArguments
	}
	view, status, err := service.CachedIndexSnapshot(ctx, graphIndexKinds...)
	if err != nil {
		return kubegraph.Graph{}, err
	}
	if query.Namespace != "" {
		records := view.Records[:0]
		for _, record := range view.Records {
			if record.Namespace == query.Namespace {
				records = append(records, record)
			}
		}
		view.Records = records
	}
	buildLimit := 500
	if query.MaxNodes > 0 && query.Limit <= 0 {
		buildLimit = min(query.MaxNodes, 500)
	}
	result := kubegraph.Build(view, buildLimit)
	if query.ConnectedOnly {
		filterGraphConnected(&result)
	}
	pageLimit := query.Limit
	if pageLimit <= 0 && query.MaxNodes > 0 {
		pageLimit = query.MaxNodes
	}
	pageLimit = normalizePageLimit(pageLimit)
	if !query.unpaged {
		if err := pageGraphComponents(&result, pageLimit, query.Cursor); err != nil {
			return kubegraph.Graph{}, err
		}
	}
	if query.GroupBy == "namespace" {
		for position := range result.Nodes {
			result.Nodes[position].Group = result.Nodes[position].Namespace
		}
	}
	result.Sync = kubegraph.SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Partial: status.Partial || result.Sync.Partial, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}
	return result, nil
}

func (service *Service) completeGraph(ctx context.Context, query GraphQuery) (kubegraph.Graph, error) {
	view, err := service.completeTopology(ctx, query.Namespace)
	var pending graphPendingError
	if errors.As(err, &pending) {
		// A complete graph cannot be built until every kind it joins is listed;
		// report the kinds still loading instead of an incomplete graph.
		return kubegraph.Graph{Nodes: []kubegraph.Node{}, Edges: []kubegraph.Edge{}, Omitted: map[string]int{}, Sync: kubegraph.SyncStatus{State: "partial", Partial: true, Pending: pending.status.Pending, PendingKinds: pending.status.PendingKinds}}, nil
	}
	if err != nil {
		return kubegraph.Graph{}, err
	}
	if status := scopedIndexStatus(kubeindex.Status{Kinds: view.Kinds}, graphIndexKinds); status.State == "warming" {
		return kubegraph.Graph{Nodes: []kubegraph.Node{}, Edges: []kubegraph.Edge{}, Omitted: map[string]int{}, Sync: kubegraph.SyncStatus{State: "syncing", Progress: service.coldSyncProgress()}}, nil
	}
	result, withinEdgeLimit := kubegraph.BuildComplete(view, maxCompleteGraphEdges)
	if !withinEdgeLimit {
		return kubegraph.Graph{}, ErrCompleteGraphLimit
	}
	if result.Omitted["references"] > 0 {
		return kubegraph.Graph{}, ErrGraphIncomplete
	}
	if query.ConnectedOnly {
		filterGraphConnectedWithoutOmitted(&result)
	}
	if query.GroupBy == "namespace" {
		for position := range result.Nodes {
			result.Nodes[position].Group = result.Nodes[position].Namespace
		}
	}
	result.Sync = kubegraph.SyncStatus{State: "ready"}
	encodedSize, err := completeGraphResponseSize(result)
	if err != nil {
		return kubegraph.Graph{}, ErrGraphIncomplete
	}
	if encodedSize > maxCompleteGraphResponseBytes {
		return kubegraph.Graph{}, ErrCompleteGraphLimit
	}
	return result, nil
}

func (service *Service) OverviewGraph(ctx context.Context) (kubegraph.Graph, error) {
	view, status, err := service.CachedIndexSnapshot(ctx, graphIndexKinds...)
	if err != nil {
		return kubegraph.Graph{}, err
	}
	result, _ := kubegraph.BuildComplete(view, 0)
	filterGraphConnectedWithoutOmitted(&result)
	for position := range result.Nodes {
		result.Nodes[position].Group = result.Nodes[position].Namespace
	}
	result.Sync = kubegraph.SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Partial: status.Partial || result.Sync.Partial, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}
	return result, nil
}

func (service *Service) completeTopology(ctx context.Context, namespace string) (kubeindex.Snapshot, error) {
	kinds := []string{}
	for _, kind := range graphIndexKinds {
		if kind != "Node" && kind != "Namespace" {
			kinds = append(kinds, kind)
		}
	}
	view, status, err := service.CachedNamespaceSnapshot(ctx, namespace, kinds...)
	if err != nil {
		return kubeindex.Snapshot{}, err
	}
	if len(status.Pending) > 0 {
		return kubeindex.Snapshot{}, graphPendingError{status: status}
	}
	if status.Partial {
		for _, state := range status.Kinds {
			if state.Error == "forbidden" {
				return kubeindex.Snapshot{}, ErrForbidden
			}
			if state.Error == "unauthorized" {
				return kubeindex.Snapshot{}, ErrUnauthorized
			}
		}
		return kubeindex.Snapshot{}, ErrGraphIncomplete
	}
	records := make([]kubeindex.Record, 0)
	for _, record := range view.Records {
		if record.Kind != "Node" && record.Kind != "Namespace" && (namespace == "" || record.Namespace == namespace) {
			records = append(records, record)
		}
	}
	return kubeindex.Snapshot{Records: records, Kinds: view.Kinds}, nil
}

type graphPendingError struct{ status kubeindex.Status }

func (graphPendingError) Error() string { return "kubernetes: graph kinds are still loading" }

func completeGraphResponseSize(graph kubegraph.Graph) (int, error) {
	size := len(`{"nodes":[`)
	for index, node := range graph.Nodes {
		if index > 0 {
			size++
		}
		nodeSize := 2
		first := true
		nodeSize = addJSONFieldSize(nodeSize, &first, "id", node.ID)
		nodeSize = addJSONFieldSize(nodeSize, &first, "kind", node.Kind)
		if node.Namespace != "" {
			nodeSize = addJSONFieldSize(nodeSize, &first, "namespace", node.Namespace)
		}
		nodeSize = addJSONFieldSize(nodeSize, &first, "name", node.Name)
		if node.Health != "" {
			nodeSize = addJSONFieldSize(nodeSize, &first, "health", node.Health)
		}
		nodeSize = addJSONFieldSize(nodeSize, &first, "group", node.Group)
		size += nodeSize
	}
	size += len(`],"edges":[`)
	for index, edge := range graph.Edges {
		if index > 0 {
			size++
		}
		edgeSize := 2
		first := true
		edgeSize = addJSONFieldSize(edgeSize, &first, "from", edge.From)
		edgeSize = addJSONFieldSize(edgeSize, &first, "to", edge.To)
		edgeSize = addJSONFieldSize(edgeSize, &first, "type", string(edge.Type))
		size += edgeSize
	}
	size += len(`],"omitted":`)
	if graph.Omitted == nil {
		size += len("null")
	} else {
		size++
		keys := make([]string, 0, len(graph.Omitted))
		for key := range graph.Omitted {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for index, key := range keys {
			if index > 0 {
				size++
			}
			size += jsonStringEncodedSize(key) + 1 + len(strconv.Itoa(graph.Omitted[key]))
		}
		size++
	}
	if graph.Next != "" {
		size += len(`,"next":`) + jsonStringEncodedSize(graph.Next)
	}
	size += len(`,"truncated":`)
	if graph.Truncated {
		size += len("true")
	} else {
		size += len("false")
	}
	size += len(`,"sync":`)
	encodedSync, err := json.Marshal(graph.Sync)
	if err != nil {
		return 0, err
	}
	return size + len(encodedSync) + 1, nil
}

func addJSONFieldSize(size int, first *bool, key, value string) int {
	if !*first {
		size++
	}
	*first = false
	return size + jsonStringEncodedSize(key) + 1 + jsonStringEncodedSize(value)
}

func jsonStringEncodedSize(value string) int {
	size := 2
	for len(value) > 0 {
		r, width := utf8.DecodeRuneInString(value)
		value = value[width:]
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			size += 2
		case '<', '>', '&', '\u2028', '\u2029':
			size += 6
		default:
			if r < 0x20 || r == utf8.RuneError && width == 1 {
				size += 6
			} else {
				size += width
			}
		}
	}
	return size
}

// Neighborhood returns a capped BFS slice from the same shared graph view.
func (service *Service) Neighborhood(ctx context.Context, query NeighborhoodQuery) (kubegraph.Graph, error) {
	if query.ResourceID != "" {
		for kind, resourceID := range indexedResourceIDs {
			if query.ResourceID == resourceID {
				query.Kind = kind
				break
			}
		}
	}
	if query.Kind == "" || query.Namespace != "" && !safeSegment(query.Namespace) || !safeSegment(query.Name) {
		return kubegraph.Graph{}, ErrInvalidArguments
	}
	whole, err := service.Graph(ctx, GraphQuery{MaxNodes: 500, unpaged: true})
	if err != nil {
		return kubegraph.Graph{}, err
	}
	result := kubegraph.Neighborhood(whole, kubegraph.Ref{Kind: query.Kind, Namespace: query.Namespace, Name: query.Name}, query.Hops, query.MaxNodes)
	result.Sync = whole.Sync
	return result, nil
}

func filterGraphConnected(graph *kubegraph.Graph) {
	filterGraphConnectedMode(graph, true)
}

func filterGraphConnectedWithoutOmitted(graph *kubegraph.Graph) {
	filterGraphConnectedMode(graph, false)
}

func filterGraphConnectedMode(graph *kubegraph.Graph, reportOmitted bool) {
	connected := make(map[string]bool, len(graph.Nodes))
	for _, edge := range graph.Edges {
		connected[edge.From] = true
		connected[edge.To] = true
	}
	nodes := graph.Nodes[:0]
	for _, node := range graph.Nodes {
		if connected[node.ID] {
			nodes = append(nodes, node)
		} else if reportOmitted {
			graph.Omitted[node.Kind]++
		}
	}
	graph.Nodes = nodes
}

func pageGraphComponents(graph *kubegraph.Graph, limit int, cursor string) error {
	components := graphComponents(*graph)
	start := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 || parsed > len(components) {
			return ErrInvalidArguments
		}
		start = parsed
	}
	selected := make(map[string]bool)
	end, selectedCount := start, 0
	for end < len(components) {
		componentSize := len(components[end])
		if selectedCount > 0 && selectedCount+componentSize > limit {
			break
		}
		for _, id := range components[end] {
			selected[id] = true
		}
		selectedCount += componentSize
		end++
	}
	nodes := graph.Nodes[:0]
	for _, node := range graph.Nodes {
		if selected[node.ID] {
			nodes = append(nodes, node)
		}
	}
	graph.Nodes = nodes
	edges := graph.Edges[:0]
	for _, edge := range graph.Edges {
		if selected[edge.From] && selected[edge.To] {
			edges = append(edges, edge)
		}
	}
	graph.Edges = edges
	if end < len(components) {
		graph.Next = strconv.Itoa(end)
	}
	graph.Truncated = graph.Next != "" || len(graph.Omitted) > 0
	return nil
}

func graphComponents(graph kubegraph.Graph) [][]string {
	nodes := make(map[string]bool, len(graph.Nodes))
	adjacent := make(map[string][]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = true
	}
	for _, edge := range graph.Edges {
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
		adjacent[edge.To] = append(adjacent[edge.To], edge.From)
	}
	seen := make(map[string]bool, len(nodes))
	components := make([][]string, 0, len(nodes))
	for id := range nodes {
		if seen[id] {
			continue
		}
		component := []string{}
		queue := []string{id}
		seen[id] = true
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			component = append(component, current)
			for _, next := range adjacent[current] {
				if nodes[next] && !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		sort.Strings(component)
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		if len(components[i]) != len(components[j]) {
			return len(components[i]) > len(components[j])
		}
		return components[i][0] < components[j][0]
	})
	return components
}
