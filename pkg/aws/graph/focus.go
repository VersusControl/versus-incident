package graph

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// MaxFanOut is how many new neighbours one node adds to a focus view per
// hop. A transit gateway or a shared role can link to thousands of
// resources; past this many the rest are counted on the node, not drawn.
const MaxFanOut = 40

// FocusView is what Focus draws: the nodes and links within reach, or, when
// more than the node cap are within reach, nothing but the counts that say
// how to narrow it. Hidden counts, per node, the neighbours the fan-out cap
// left out from it and no other node drew. FitsHops is the largest hop count whose view fits the cap (zero
// when even one hop does not). Reached is how many nodes the walk reached
// before it stopped: when TooMany it is a lower bound.
type FocusView struct {
	Graph    Graph          `json:"graph"`
	Hidden   map[string]int `json:"hidden,omitempty"`
	TooMany  bool           `json:"too_many"`
	Reached  int            `json:"reached"`
	FitsHops int            `json:"fits_hops"`
	// Expanded counts the nodes whose links the walk followed: the work
	// it did, bounded by the node cap whatever the graph's size.
	Expanded int `json:"-"`
}

// Focus walks up to hops links from ref, in either direction, over the
// links keep admits (nil admits every one). Each node adds at most fanOut
// new neighbours per hop (MaxFanOut when zero or less); the rest are
// counted in Hidden. The walk stops as soon as more than maxNodes nodes are
// reached, before any link is drawn: the view is then TooMany and holds no
// node, never a silently cut graph. A ref not in the graph reaches nothing.
func Focus(g Graph, ref string, hops, maxNodes, fanOut int, keep func(Edge) bool) FocusView {
	hops = min(max(hops, 1), MaxHops)
	if maxNodes <= 0 || maxNodes > MaxNodes {
		maxNodes = MaxNodes
	}
	if fanOut <= 0 {
		fanOut = MaxFanOut
	}
	view := FocusView{Graph: Graph{Nodes: []Node{}, Edges: []Edge{}, Accounts: []AccountCard{}}}
	nodes := make(map[string]Node, len(g.Nodes))
	for _, node := range g.Nodes {
		nodes[node.ID] = node
	}
	if _, ok := nodes[ref]; !ok {
		return view
	}
	edges := make([]Edge, 0, len(g.Edges))
	for _, edge := range g.Edges {
		if keep == nil || keep(edge) {
			edges = append(edges, edge)
		}
	}
	// A member of a fold of wide grants reaches the fold, and its grants,
	// through the membership; the fold is walked to from a member only, so
	// it never floods a view with every member.
	for _, node := range g.Nodes {
		if node.Type != NodeGroup {
			continue
		}
		for _, member := range node.Members {
			edges = append(edges, Edge{From: member, To: node.ID, Type: model.RelationMemberOf, Kind: model.RelationConfigured, Count: 1})
		}
	}
	adjacent := map[string][]string{}
	for _, edge := range edges {
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
		if nodes[edge.To].Type != NodeGroup || edge.Type != model.RelationMemberOf {
			adjacent[edge.To] = append(adjacent[edge.To], edge.From)
		}
	}

	distance := map[string]int{ref: 0}
	queue := []string{ref}
	// skipped holds, per node, the neighbours the fan-out cap did not add
	// from it; those another node drew are not hidden.
	skipped := map[string][]string{}
	view.FitsHops = hops
	for cursor := 0; cursor < len(queue) && !view.TooMany; cursor++ {
		current := queue[cursor]
		if distance[current] >= hops {
			continue
		}
		view.Expanded++
		neighbours := adjacent[current]
		sort.Strings(neighbours)
		added := 0
		for index, next := range neighbours {
			if index > 0 && neighbours[index-1] == next {
				continue
			}
			if _, seen := distance[next]; seen {
				continue
			}
			if added == fanOut {
				skipped[current] = append(skipped[current], next)
				continue
			}
			added++
			distance[next] = distance[current] + 1
			queue = append(queue, next)
			if len(queue) > maxNodes {
				view.TooMany = true
				view.FitsHops = distance[current]
				break
			}
		}
	}
	view.Reached = len(queue)
	if view.TooMany {
		return view
	}
	selected := make(map[string]bool, len(queue))
	for _, id := range queue {
		selected[id] = true
	}
	for id, list := range skipped {
		for _, next := range list {
			if !selected[next] {
				if view.Hidden == nil {
					view.Hidden = map[string]int{}
				}
				view.Hidden[id]++
			}
		}
	}
	accounts := map[string]bool{}
	for _, id := range queue {
		node := nodes[id]
		view.Graph.Nodes = append(view.Graph.Nodes, node)
		accounts[node.Account] = true
	}
	for _, edge := range edges {
		if selected[edge.From] && selected[edge.To] {
			view.Graph.Edges = append(view.Graph.Edges, edge)
		}
	}
	for _, card := range g.Accounts {
		if accounts[card.ID] {
			view.Graph.Accounts = append(view.Graph.Accounts, card)
		}
	}
	sort.Slice(view.Graph.Nodes, func(left, right int) bool { return view.Graph.Nodes[left].ID < view.Graph.Nodes[right].ID })
	sortEdges(view.Graph.Edges)
	return view
}
