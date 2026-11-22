package graph

import (
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// AccountLink summarises the links of one kind from one account to
// another: how many there are and how many were seen from both sides. To
// is an account ID or an organization target; when the far end cannot be
// attributed to an account (a host name no connected account answers to, a
// deleted principal, anyone) To is empty and Far says what it is.
type AccountLink struct {
	From       string             `json:"from"`
	To         string             `json:"to,omitempty"`
	Far        string             `json:"far,omitempty"`
	Type       model.RelationType `json:"type"`
	Kind       model.RelationKind `json:"kind"`
	Source     string             `json:"source,omitempty"`
	Count      int                `json:"count"`
	BothSides  int                `json:"both_sides"`
	LastReadAt time.Time          `json:"last_read_at,omitzero"`
}

// AccountsView is the Accounts level: one card per account, and the links
// between accounts summarised by kind.
type AccountsView struct {
	Accounts []AccountCard `json:"accounts"`
	Links    []AccountLink `json:"links"`
}

// Accounts summarises the graph at the Accounts level. A link inside one
// account is not shown there.
func Accounts(g Graph) AccountsView {
	nodes := make(map[string]Node, len(g.Nodes))
	for _, node := range g.Nodes {
		nodes[node.ID] = node
	}
	type key struct {
		from, to, far string
		kind          model.RelationType
		how           model.RelationKind
		source        string
	}
	links := map[key]*AccountLink{}
	for _, edge := range g.Edges {
		from, to := nodes[edge.From], nodes[edge.To]
		if from.Account == "" || from.Account == to.Account {
			continue
		}
		k := key{from: from.Account, to: to.Account, kind: edge.Type, how: edge.Kind, source: edge.Source}
		switch {
		case to.Type == NodeOrganization:
			k.to = model.OrgTarget(to.Name)
		case to.Account == "":
			k.far = to.Type
		}
		link, ok := links[k]
		if !ok {
			link = &AccountLink{From: k.from, To: k.to, Type: edge.Type, Kind: edge.Kind, Source: edge.Source}
			if k.far != "" {
				link.Far = to.Description
				if to.Type == NodeHost {
					link.Far = "accounts not connected"
				}
			}
			links[k] = link
		}
		link.Count += max(edge.Count, 1)
		if edge.Sides == BothSides {
			link.BothSides += max(edge.Count, 1)
		}
		if edge.LastReadAt.After(link.LastReadAt) {
			link.LastReadAt = edge.LastReadAt
		}
	}
	out := AccountsView{Accounts: append([]AccountCard{}, g.Accounts...), Links: make([]AccountLink, 0, len(links))}
	for _, link := range links {
		out.Links = append(out.Links, *link)
	}
	sort.Slice(out.Links, func(left, right int) bool {
		a, b := out.Links[left], out.Links[right]
		switch {
		case a.From != b.From:
			return a.From < b.From
		case a.To != b.To:
			return a.To < b.To
		case a.Far != b.Far:
			return a.Far < b.Far
		case a.Type != b.Type:
			return a.Type < b.Type
		}
		return a.Source < b.Source
	})
	return out
}

// Focus bounds. A view holds at most MaxNodes nodes and reaches at most
// MaxHops links from where it starts.
const (
	MaxHops  = 3
	MaxNodes = 300
)

// Neighborhood returns what is up to hops links from ref, in either
// direction, keeping at most maxNodes nodes, nearest first. ref is a node
// ID (an ARN); a resource folded into a group of wide grants reaches its
// grants through the group, one hop away. Truncated says nodes in reach
// were left out. A ref that is not in the graph answers an empty graph.
func Neighborhood(g Graph, ref string, hops, maxNodes int) Graph {
	hops = min(max(hops, 0), MaxHops)
	if maxNodes <= 0 || maxNodes > MaxNodes {
		maxNodes = MaxNodes
	}
	out := Graph{Nodes: []Node{}, Edges: []Edge{}, Accounts: []AccountCard{}}
	nodes := make(map[string]Node, len(g.Nodes))
	root := ""
	for _, node := range g.Nodes {
		nodes[node.ID] = node
		if node.ID == ref {
			root = ref
		}
	}
	if root == "" {
		return out
	}
	adjacent := map[string][]string{}
	for _, edge := range g.Edges {
		adjacent[edge.From] = append(adjacent[edge.From], edge.To)
		adjacent[edge.To] = append(adjacent[edge.To], edge.From)
	}
	// A resource folded into a group of wide grants reaches its grants
	// through the group: the membership is one more link to walk and draw,
	// walked from the member only, so a group never floods a view with
	// every member.
	edges := append([]Edge(nil), g.Edges...)
	for _, node := range g.Nodes {
		if node.Type != NodeGroup {
			continue
		}
		for _, member := range node.Members {
			edges = append(edges, Edge{From: member, To: node.ID, Type: model.RelationMemberOf, Kind: model.RelationConfigured, Count: 1})
			adjacent[member] = append(adjacent[member], node.ID)
		}
	}
	for id := range adjacent {
		sort.Strings(adjacent[id])
	}
	distance := map[string]int{root: 0}
	queue := []string{root}
	for cursor := 0; cursor < len(queue); cursor++ {
		current := queue[cursor]
		if distance[current] >= hops {
			continue
		}
		for _, next := range adjacent[current] {
			if _, seen := distance[next]; !seen {
				distance[next] = distance[current] + 1
				queue = append(queue, next)
			}
		}
	}
	selected := map[string]bool{}
	for _, id := range queue {
		if len(selected) >= maxNodes {
			out.Truncated = true
			break
		}
		selected[id] = true
	}
	accounts := map[string]bool{}
	for id := range selected {
		node := nodes[id]
		out.Nodes = append(out.Nodes, node)
		accounts[node.Account] = true
	}
	for _, edge := range edges {
		if selected[edge.From] && selected[edge.To] {
			out.Edges = append(out.Edges, edge)
		}
	}
	for _, card := range g.Accounts {
		if accounts[card.ID] {
			out.Accounts = append(out.Accounts, card)
		}
	}
	sort.Slice(out.Nodes, func(left, right int) bool { return out.Nodes[left].ID < out.Nodes[right].ID })
	sortEdges(out.Edges)
	return out
}
