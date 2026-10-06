package graph

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type EdgeType string

const (
	Manages    EdgeType = "manages"
	Exposes    EdgeType = "exposes"
	RoutesTo   EdgeType = "routes-to"
	Uses       EdgeType = "uses"
	Configures EdgeType = "configures"
	Scales     EdgeType = "scales"
)

type Node struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Health    string `json:"health,omitempty"`
	Group     string `json:"group"`
}

type Edge struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Type EdgeType `json:"type"`
}

type Graph struct {
	Nodes     []Node         `json:"nodes"`
	Edges     []Edge         `json:"edges"`
	Omitted   map[string]int `json:"omitted"`
	Next      string         `json:"next,omitempty"`
	Truncated bool           `json:"truncated"`
	Sync      SyncStatus     `json:"sync"`
}

type SyncStatus struct {
	State      string  `json:"state"`
	AgeSeconds float64 `json:"age_s"`
	Partial    bool    `json:"partial"`
}

type Ref struct {
	UID       string
	Kind      string
	Namespace string
	Name      string
}

// Build constructs a bounded relationship graph without Kubernetes API reads.
func Build(snapshot index.Snapshot, maxNodes int) Graph {
	if maxNodes <= 0 || maxNodes > 500 {
		maxNodes = 500
	}
	graph, _ := build(snapshot, maxNodes, 0)
	return graph
}

// BuildComplete constructs a graph without the paging-oriented node cap and
// returns false rather than truncating when the edge safety limit is exceeded.
func BuildComplete(snapshot index.Snapshot, maxEdges int) (Graph, bool) {
	return build(snapshot, 0, maxEdges)
}

func build(snapshot index.Snapshot, maxNodes, maxEdges int) (Graph, bool) {
	result := Graph{Nodes: []Node{}, Edges: []Edge{}, Omitted: map[string]int{}}
	records := append([]index.Record(nil), snapshot.Records...)
	sort.Slice(records, func(left, right int) bool {
		if records[left].Kind != records[right].Kind {
			return records[left].Kind < records[right].Kind
		}
		if records[left].Namespace != records[right].Namespace {
			return records[left].Namespace < records[right].Namespace
		}
		return records[left].Name < records[right].Name
	})
	capacity := len(records)
	if maxNodes > 0 {
		capacity = min(capacity, maxNodes)
	}
	selected := make(map[string]index.Record, capacity)
	byName := make(map[string]string, capacity)
	for _, record := range records {
		if maxNodes > 0 && len(selected) >= maxNodes {
			result.Omitted[record.Kind]++
			continue
		}
		if record.UID == "" {
			result.Omitted[record.Kind]++
			continue
		}
		group := record.Namespace
		if app := record.Labels["app.kubernetes.io/name"]; app != "" {
			group += "/" + app
		} else if app := record.Labels["app"]; app != "" {
			group += "/" + app
		}
		selected[record.UID] = record
		byName[record.Kind+"\x00"+record.Namespace+"\x00"+record.Name] = record.UID
		result.Nodes = append(result.Nodes, Node{ID: record.UID, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, Health: record.Health, Group: group})
	}
	edges := make(map[string]Edge)
	limited := false
	add := func(from, to string, kind EdgeType) {
		if from == "" || to == "" || from == to || selected[from].UID == "" || selected[to].UID == "" {
			return
		}
		key := from + "\x00" + to + "\x00" + string(kind)
		if _, exists := edges[key]; exists {
			return
		}
		if maxEdges > 0 && len(edges) >= maxEdges {
			limited = true
			return
		}
		edges[key] = Edge{From: from, To: to, Type: kind}
	}
	for _, record := range selected {
		for _, owner := range record.Owners {
			uid := owner.UID
			if uid == "" {
				uid = byName[owner.Kind+"\x00"+owner.Namespace+"\x00"+owner.Name]
			}
			add(uid, record.UID, Manages)
		}
		if record.Target != nil {
			target := record.Target
			uid := target.UID
			if uid == "" {
				uid = byName[target.Kind+"\x00"+target.Namespace+"\x00"+target.Name]
			}
			if record.Kind == "HorizontalPodAutoscaler" {
				add(record.UID, uid, Scales)
			}
		}
		for _, name := range record.References["services"] {
			target := byName["Service\x00"+record.Namespace+"\x00"+name]
			if record.Kind == "Ingress" || record.Kind == "HTTPRoute" || record.Kind == "Gateway" {
				add(record.UID, target, RoutesTo)
			}
		}
		for _, name := range record.References["config_maps"] {
			add(byName["ConfigMap\x00"+record.Namespace+"\x00"+name], record.UID, Configures)
		}
		for _, name := range record.References["secrets"] {
			add(byName["Secret\x00"+record.Namespace+"\x00"+name], record.UID, Configures)
		}
		for _, name := range record.References["persistent_volume_claims"] {
			add(record.UID, byName["PersistentVolumeClaim\x00"+record.Namespace+"\x00"+name], Uses)
		}
		for _, name := range record.References["pods"] {
			add(record.UID, byName["Pod\x00"+record.Namespace+"\x00"+name], Exposes)
		}
		if record.Kind == "Service" && len(record.Selector) > 0 {
			for _, candidate := range selected {
				if candidate.Kind == "Pod" && candidate.Namespace == record.Namespace && labelsMatch(candidate.Labels, record.Selector) {
					add(record.UID, candidate.UID, Exposes)
				}
				if limited {
					break
				}
			}
		}
		if limited {
			return Graph{}, false
		}
	}
	for _, edge := range edges {
		result.Edges = append(result.Edges, edge)
	}
	sort.Slice(result.Edges, func(left, right int) bool {
		if result.Edges[left].From != result.Edges[right].From {
			return result.Edges[left].From < result.Edges[right].From
		}
		if result.Edges[left].To != result.Edges[right].To {
			return result.Edges[left].To < result.Edges[right].To
		}
		return result.Edges[left].Type < result.Edges[right].Type
	})
	return result, true
}

// Neighborhood returns an undirected BFS slice with the requested hop and node bounds.
func Neighborhood(graph Graph, ref Ref, hops, maxNodes int) Graph {
	if hops < 0 {
		hops = 0
	}
	if hops > 2 {
		hops = 2
	}
	if maxNodes <= 0 || maxNodes > 150 {
		maxNodes = 150
	}
	result := Graph{Nodes: []Node{}, Edges: []Edge{}, Omitted: map[string]int{}}
	nodes := make(map[string]Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	root := ref.UID
	if _, exists := nodes[root]; !exists {
		root = ""
		for _, node := range graph.Nodes {
			if node.Kind == ref.Kind && node.Namespace == ref.Namespace && node.Name == ref.Name {
				root = node.ID
				break
			}
		}
	}
	if root == "" {
		result.Omitted["not_found"] = 1
		return result
	}
	adjacent := make(map[string][]string)
	for _, edge := range graph.Edges {
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
		adjacent[edge.To] = append(adjacent[edge.To], edge.From)
	}
	distance := map[string]int{root: 0}
	queue := []string{root}
	for cursor := 0; cursor < len(queue); cursor++ {
		current := queue[cursor]
		if distance[current] >= hops {
			continue
		}
		for _, next := range adjacent[current] {
			if _, visited := distance[next]; !visited {
				distance[next] = distance[current] + 1
				queue = append(queue, next)
			}
		}
	}
	selected := make(map[string]bool, min(len(distance), maxNodes))
	for _, id := range queue {
		level := distance[id]
		if level <= hops && len(selected) < maxNodes {
			selected[id] = true
		} else if level <= hops {
			result.Omitted[nodes[id].Kind]++
		}
	}
	for id := range selected {
		result.Nodes = append(result.Nodes, nodes[id])
	}
	for _, edge := range graph.Edges {
		if selected[edge.From] && selected[edge.To] {
			result.Edges = append(result.Edges, edge)
		}
	}
	for kind, count := range graph.Omitted {
		result.Omitted[kind] += count
	}
	sort.Slice(result.Nodes, func(left, right int) bool { return result.Nodes[left].ID < result.Nodes[right].ID })
	sort.Slice(result.Edges, func(left, right int) bool {
		if result.Edges[left].From != result.Edges[right].From {
			return result.Edges[left].From < result.Edges[right].From
		}
		return result.Edges[left].To < result.Edges[right].To
	})
	return result
}

func labelsMatch(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}
