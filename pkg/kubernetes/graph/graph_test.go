package graph

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestBuildRelationshipsAndSelectorMatching(t *testing.T) {
	view := index.Snapshot{Records: []index.Record{
		{UID: "svc", Kind: "Service", Namespace: "shop", Name: "checkout", Selector: map[string]string{"app": "checkout"}},
		{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "checkout-1", Labels: map[string]string{"app": "checkout"}},
		{UID: "deploy", Kind: "Deployment", Namespace: "shop", Name: "checkout", References: map[string][]string{"config_maps": {"settings"}, "persistent_volume_claims": {"data"}}},
		{UID: "config", Kind: "ConfigMap", Namespace: "shop", Name: "settings"},
		{UID: "claim", Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data"},
		{UID: "hpa", Kind: "HorizontalPodAutoscaler", Namespace: "shop", Name: "checkout", Target: &index.OwnerRef{UID: "deploy", Kind: "Deployment", Namespace: "shop", Name: "checkout"}},
	}}
	graph := Build(view, 500)
	want := map[EdgeType]string{
		Exposes: "svc->pod", Configures: "config->deploy", Uses: "deploy->claim", Scales: "hpa->deploy",
	}
	for _, edge := range graph.Edges {
		if expected, ok := want[edge.Type]; ok {
			got := edge.From + "->" + edge.To
			if got != expected {
				t.Errorf("%s edge=%s, want %s", edge.Type, got, expected)
			}
			delete(want, edge.Type)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing graph edges: %v; graph=%#v", want, graph.Edges)
	}
	neighborhood := Neighborhood(graph, Ref{UID: "svc"}, 1, 10)
	if len(neighborhood.Nodes) != 2 || len(neighborhood.Edges) != 1 || neighborhood.Edges[0].Type != Exposes {
		t.Fatalf("service neighborhood=%#v", neighborhood)
	}
}

func TestBuildAndNeighborhoodRespectBounds(t *testing.T) {
	records := make([]index.Record, 20)
	for i := range records {
		records[i] = index.Record{UID: string(rune('a' + i)), Kind: "Pod", Namespace: "shop", Name: "pod" + string(rune('a'+i))}
	}
	graph := Build(index.Snapshot{Records: records}, 5)
	if len(graph.Nodes) != 5 || graph.Omitted["Pod"] != 15 {
		t.Fatalf("bounded graph nodes=%d omitted=%v", len(graph.Nodes), graph.Omitted)
	}
	neighborhood := Neighborhood(graph, Ref{UID: graph.Nodes[0].ID}, 9, 1)
	if len(neighborhood.Nodes) != 1 || neighborhood.Nodes[0].ID != graph.Nodes[0].ID {
		t.Fatalf("bounded neighborhood=%#v", neighborhood)
	}
}
