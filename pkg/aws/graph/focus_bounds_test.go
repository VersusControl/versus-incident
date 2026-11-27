package graph

import (
	"fmt"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// tree builds a hub with children, each child with grandchildren, each
// grandchild with one leaf. Links point both ways across the tree, so the
// walk must follow incoming links as well as outgoing ones.
func tree(children, grandchildren int) Graph {
	node := func(id string) Node {
		return Node{ID: id, Account: "123400000001", Type: "network-interface", Name: id}
	}
	link := func(from, to string) Edge {
		return Edge{From: from, To: to, Type: model.RelationAttachedTo, Kind: model.RelationConfigured, Count: 1}
	}
	g := Graph{Nodes: []Node{node("hub")}}
	for child := range children {
		childID := fmt.Sprintf("c%04d", child)
		g.Nodes = append(g.Nodes, node(childID))
		// Half the attachments point at the hub, half from it.
		if child%2 == 0 {
			g.Edges = append(g.Edges, link(childID, "hub"))
		} else {
			g.Edges = append(g.Edges, link("hub", childID))
		}
		for grandchild := range grandchildren {
			id := fmt.Sprintf("%s-g%02d", childID, grandchild)
			g.Nodes = append(g.Nodes, node(id), node(id+"-leaf"))
			g.Edges = append(g.Edges, link(childID, id), link(id+"-leaf", id))
		}
	}
	return g
}

// The node cap is exact: a view of 300 nodes is drawn whole; one of 301 is
// too many, holds no node and no link, and says which hop count fits.
func TestFocusNodeCapIsExactAtThreeHundred(t *testing.T) {
	fits := Focus(tree(MaxNodes-1, 0), "hub", 1, MaxNodes, MaxNodes, nil)
	if fits.TooMany || len(fits.Graph.Nodes) != MaxNodes || len(fits.Graph.Edges) != MaxNodes-1 || fits.Reached != MaxNodes {
		t.Fatalf("300 nodes: too many %v, %d nodes, %d links, reached %d; want all drawn", fits.TooMany, len(fits.Graph.Nodes),
			len(fits.Graph.Edges), fits.Reached)
	}
	over := Focus(tree(MaxNodes, 0), "hub", 1, MaxNodes, MaxNodes+1, nil)
	if !over.TooMany || len(over.Graph.Nodes) != 0 || len(over.Graph.Edges) != 0 || over.Reached != MaxNodes+1 || over.FitsHops != 0 {
		t.Fatalf("301 nodes: too many %v, %d nodes, %d links, reached %d, fits %d; want too many, empty, fits 0",
			over.TooMany, len(over.Graph.Nodes), len(over.Graph.Edges), over.Reached, over.FitsHops)
	}
	// A cap above MaxNodes, or none, is MaxNodes.
	if loose := Focus(tree(MaxNodes, 0), "hub", 1, 10*MaxNodes, MaxNodes+1, nil); !loose.TooMany {
		t.Fatal("a caller's cap above MaxNodes drew more than MaxNodes nodes")
	}
}

// One hop fits and two do not: the view is too many and its hint says one
// hop fits; the same at three hops says two fit.
func TestFocusTooManySaysWhichHopsFit(t *testing.T) {
	// 1 + 40 + 40*7 = 321 nodes at two hops.
	two := Focus(tree(MaxFanOut, 7), "hub", 2, MaxNodes, 0, nil)
	if !two.TooMany || two.FitsHops != 1 || len(two.Graph.Nodes) != 0 {
		t.Fatalf("two hops over the cap: too many %v, fits %d, %d nodes; want too many, fits 1", two.TooMany, two.FitsHops, len(two.Graph.Nodes))
	}
	// 1 + 40 + 240 = 281 at two hops, 521 at three.
	three := Focus(tree(MaxFanOut, 6), "hub", 3, MaxNodes, 0, nil)
	if !three.TooMany || three.FitsHops != 2 {
		t.Fatalf("three hops over the cap: too many %v, fits %d; want too many, fits 2", three.TooMany, three.FitsHops)
	}
	if fits := Focus(tree(MaxFanOut, 6), "hub", 2, MaxNodes, 0, nil); fits.TooMany || len(fits.Graph.Nodes) != 281 {
		t.Fatalf("two hops = too many %v, %d nodes; want 281 drawn", fits.TooMany, len(fits.Graph.Nodes))
	}
}

// A hub with 5,000 attachments draws MaxFanOut of them at every hop count
// and counts the rest as hidden on the hub: each attachment reaches its
// own grandchildren, so 1 + 40 + 40 + 40 nodes at three hops.
func TestFocusFanOutCapHoldsAtEveryHop(t *testing.T) {
	g := tree(5000, 1)
	for hops, want := range map[int]int{1: 1 + MaxFanOut, 2: 1 + 2*MaxFanOut, 3: 1 + 3*MaxFanOut} {
		view := Focus(g, "hub", hops, MaxNodes, 0, nil)
		if view.TooMany || len(view.Graph.Nodes) != want {
			t.Fatalf("hops %d: too many %v, %d nodes; want %d", hops, view.TooMany, len(view.Graph.Nodes), want)
		}
		if view.Hidden["hub"] != 5000-MaxFanOut {
			t.Fatalf("hops %d: the hub hides %d; want %d", hops, view.Hidden["hub"], 5000-MaxFanOut)
		}
		if view.Expanded > MaxNodes+1 {
			t.Fatalf("hops %d: the walk expanded %d nodes", hops, view.Expanded)
		}
	}
	// The cap counts new neighbours per node: a node with exactly
	// MaxFanOut neighbours hides none.
	exact := Focus(tree(MaxFanOut, 0), "hub", 1, MaxNodes, 0, nil)
	if len(exact.Hidden) != 0 || len(exact.Graph.Nodes) != MaxFanOut+1 {
		t.Fatalf("exactly %d neighbours: hidden %v, %d nodes", MaxFanOut, exact.Hidden, len(exact.Graph.Nodes))
	}
	// Hops out of range are clamped to 1..MaxHops.
	if view := Focus(g, "hub", 0, MaxNodes, 0, nil); len(view.Graph.Nodes) != 1+MaxFanOut {
		t.Fatalf("hops 0 drew %d nodes; want one hop", len(view.Graph.Nodes))
	}
	if view := Focus(g, "hub", 9, MaxNodes, 0, nil); len(view.Graph.Nodes) != 1+3*MaxFanOut {
		t.Fatalf("hops 9 drew %d nodes; want three hops", len(view.Graph.Nodes))
	}
}
