package graph

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// A neighbour the fan-out cap skipped from one node but another node drew
// is not counted as hidden: only what the view leaves out is.
func TestFocusHiddenCountsOnlyNeighboursNotDrawn(t *testing.T) {
	node := func(id string) Node { return Node{ID: id, Account: "123400000001", Type: "function", Name: id} }
	link := func(from, to string) Edge {
		return Edge{From: from, To: to, Type: model.RelationRoutesTo, Kind: model.RelationConfigured, Count: 1}
	}
	g := Graph{Nodes: []Node{node("root"), node("a"), node("b"), node("c"), node("d")},
		Edges: []Edge{link("root", "a"), link("root", "b"), link("root", "c"), link("root", "d"), link("a", "c")}}
	// The cap of two adds a and b from root; c and d are skipped there, and
	// a draws c at the next hop.
	view := Focus(g, "root", 2, MaxNodes, 2, nil)
	if view.TooMany || len(view.Graph.Nodes) != 4 {
		t.Fatalf("view = %d nodes, too many %v; want root, a, b, c", len(view.Graph.Nodes), view.TooMany)
	}
	if view.Hidden["root"] != 1 {
		t.Fatalf("root hides %d; want 1 (d), since a drew c", view.Hidden["root"])
	}
}
