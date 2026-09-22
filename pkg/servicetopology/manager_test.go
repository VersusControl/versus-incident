package servicetopology

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type topologyProviderFunc func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error)

func (provider topologyProviderFunc) ServiceTopology(ctx context.Context, request core.ServiceTopologyRequest) (core.ServiceTopology, error) {
	return provider(ctx, request)
}

type staticFixture struct{ topology core.ServiceTopology }

func (fixture staticFixture) Topology(_, _ int) core.ServiceTopology { return fixture.topology }

type nilStaticGraph struct{}

func (*nilStaticGraph) Topology(_, _ int) core.ServiceTopology {
	panic("typed-nil graph must not be called")
}

func TestNewManagerNormalizesTypedNilDependencies(t *testing.T) {
	var graph *nilStaticGraph
	var provider topologyProviderFunc
	got := NewManager(graph, provider).Snapshot(context.Background(), "acme")
	if got.Availability != core.HealthNotConfigured {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotProviderFailurePreservesStaticGraph(t *testing.T) {
	static := core.ServiceTopology{Availability: core.HealthReady, Provenance: []string{"operator_config"}, Nodes: []core.ServiceTopologyNode{{Service: "api"}}}
	manager := NewManager(staticFixture{topology: static}, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{}, errors.New("provider failed")
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if got.Availability != core.HealthPartial || len(got.Nodes) != 1 || got.Nodes[0].Service != "api" || len(got.Provenance) != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotProviderFailureWithoutStaticGraphIsUnavailable(t *testing.T) {
	manager := NewManager(nil, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{}, errors.New("provider failed")
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if got.Availability != core.HealthError || len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotProviderMustAuthorizeRequestOrg(t *testing.T) {
	provider := topologyProviderFunc(func(_ context.Context, request core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		if request.OrgID != "acme" {
			return core.ServiceTopology{Availability: core.HealthRestricted}, nil
		}
		return core.ServiceTopology{Availability: core.HealthReady, Provenance: []string{"authorized_extension"}, Nodes: []core.ServiceTopologyNode{{Service: "private"}}}, nil
	})
	static := core.ServiceTopology{Availability: core.HealthReady, Provenance: []string{"operator_config"}, Nodes: []core.ServiceTopologyNode{{Service: "api"}}}
	manager := NewManager(staticFixture{topology: static}, provider)
	if got := manager.Snapshot(context.Background(), "other"); len(got.Nodes) != 1 || got.Nodes[0].Service != "api" || got.Availability != core.HealthReady || got.Extensions == nil || got.Extensions.Availability != core.HealthRestricted {
		t.Fatalf("cross-org snapshot = %#v", got)
	}
	if got := manager.Snapshot(context.Background(), "acme"); len(got.Nodes) != 2 || got.Nodes[0].Service != "api" || got.Nodes[1].Service != "private" {
		t.Fatalf("authorized snapshot = %#v", got)
	}
}

func TestSnapshotNoExtensionCoveragePreservesCompleteStaticTopology(t *testing.T) {
	for _, availability := range []core.HealthState{core.HealthNotConfigured, core.HealthCollecting, core.HealthNoData, core.HealthRestricted, core.HealthUnsupported} {
		t.Run(string(availability), func(t *testing.T) {
			static := core.ServiceTopology{Availability: core.HealthReady, Provenance: []string{"operator_config"}, Nodes: []core.ServiceTopologyNode{{Service: "api"}}}
			manager := NewManager(staticFixture{topology: static}, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
				return core.ServiceTopology{Availability: availability}, nil
			}))
			got := manager.Snapshot(context.Background(), "acme")
			if got.Availability != core.HealthReady || len(got.Nodes) != 1 || got.Nodes[0].Service != "api" || got.Extensions == nil || got.Extensions.Availability != availability {
				t.Fatalf("snapshot = %#v", got)
			}
		})
	}
}

func TestSnapshotDropsProviderEdgesWithoutFinalEndpoints(t *testing.T) {
	manager := NewManager(staticFixture{topology: core.ServiceTopology{Availability: core.HealthReady, Nodes: []core.ServiceTopologyNode{{Service: "api"}}}}, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{Availability: core.HealthReady, Edges: []core.ServiceTopologyEdge{{Service: "ghost-a", DependsOn: "ghost-b", Source: "extension"}}}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if len(got.Edges) != 0 || got.OmittedEdges != 1 || got.Availability != core.HealthPartial {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotDropsEdgeToProviderNodeRemovedByNodeCap(t *testing.T) {
	staticNodes := make([]core.ServiceTopologyNode, MaxNodes-1)
	for index := range staticNodes {
		staticNodes[index].Service = fmt.Sprintf("static-%03d", index)
	}
	manager := NewManager(staticFixture{topology: core.ServiceTopology{Availability: core.HealthReady, Nodes: staticNodes}}, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{
			Availability: core.HealthReady,
			Nodes:        []core.ServiceTopologyNode{{Service: "zulu"}, {Service: "zeta"}},
			Edges:        []core.ServiceTopologyEdge{{Service: "zeta", DependsOn: "zulu", Source: "extension"}},
		}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if len(got.Nodes) != MaxNodes || got.Nodes[MaxNodes-1].Service != "zeta" || len(got.Edges) != 0 || got.OmittedNodes != 1 || got.OmittedEdges != 1 || got.Availability != core.HealthPartial {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotPreservesExtensionPartialAndOmissions(t *testing.T) {
	manager := NewManager(nil, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{Availability: core.HealthPartial, OmittedNodes: 12, Nodes: []core.ServiceTopologyNode{{Service: "api"}}}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if got.Availability != core.HealthPartial || got.OmittedNodes != 12 || got.Extensions == nil || got.Extensions.Availability != core.HealthPartial {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotCallsProviderWithZeroRemainingBudget(t *testing.T) {
	staticNodes := make([]core.ServiceTopologyNode, MaxNodes)
	for index := range staticNodes {
		staticNodes[index].Service = fmt.Sprintf("static-%03d", index)
	}
	called := false
	manager := NewManager(staticFixture{topology: core.ServiceTopology{Availability: core.HealthPartial, Nodes: staticNodes, OmittedNodes: 1}}, topologyProviderFunc(func(_ context.Context, request core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		called = true
		if request.MaxNodes != 0 || request.MaxEdges != MaxEdges {
			t.Fatalf("request = %#v", request)
		}
		return core.ServiceTopology{Availability: core.HealthPartial, OmittedNodes: 2}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if !called || got.Availability != core.HealthPartial || got.OmittedNodes != 3 {
		t.Fatalf("called = %t, snapshot = %#v", called, got)
	}
}

func TestSnapshotValidatesAndDeterministicallyBoundsProviderOutput(t *testing.T) {
	manager := NewManager(nil, topologyProviderFunc(func(_ context.Context, request core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{
			Availability: core.HealthReady,
			Nodes:        []core.ServiceTopologyNode{{Service: " b "}, {Service: "a"}, {Service: "a"}, {Service: " "}},
			Edges: []core.ServiceTopologyEdge{
				{Service: "b", DependsOn: "a", Source: " extension "},
				{Service: "a", DependsOn: "a", Source: "extension"},
				{Service: "a", DependsOn: "missing", Source: "extension"},
				{Service: "b", DependsOn: "a", Source: "duplicate"},
			},
		}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if fmt.Sprint(got.Nodes) != "[{a} {b}]" || fmt.Sprint(got.Edges) != "[{b a duplicate}]" || got.OmittedNodes != 1 || got.OmittedEdges != 2 || got.Availability != core.HealthPartial {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotDeterministicallySelectsDuplicateExtensionEdgeSource(t *testing.T) {
	edges := []core.ServiceTopologyEdge{
		{Service: "api", DependsOn: "database", Source: "zeta"},
		{Service: "api", DependsOn: "database", Source: "alpha"},
	}
	snapshot := func(providerEdges []core.ServiceTopologyEdge) (core.ServiceTopology, []byte) {
		t.Helper()
		manager := NewManager(nil, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
			return core.ServiceTopology{
				Availability: core.HealthReady,
				Nodes:        []core.ServiceTopologyNode{{Service: "api"}, {Service: "database"}, {Service: "api"}},
				Edges:        providerEdges,
			}, nil
		}))
		got := manager.Snapshot(context.Background(), "acme")
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		return got, encoded
	}
	got, forward := snapshot(edges)
	_, reversed := snapshot([]core.ServiceTopologyEdge{edges[1], edges[0]})
	if !bytes.Equal(forward, reversed) {
		t.Fatalf("forward = %s, reversed = %s", forward, reversed)
	}
	if got.Availability != core.HealthReady || got.OmittedNodes != 0 || got.OmittedEdges != 0 || len(got.Nodes) != 2 || len(got.Edges) != 1 || got.Edges[0].Source != "alpha" {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSnapshotStaticEdgeWinsExtensionDuplicate(t *testing.T) {
	static := core.ServiceTopology{
		Availability: core.HealthReady,
		Nodes:        []core.ServiceTopologyNode{{Service: "api"}, {Service: "database"}},
		Edges:        []core.ServiceTopologyEdge{{Service: "api", DependsOn: "database", Source: "static"}},
	}
	manager := NewManager(staticFixture{topology: static}, topologyProviderFunc(func(context.Context, core.ServiceTopologyRequest) (core.ServiceTopology, error) {
		return core.ServiceTopology{
			Availability: core.HealthReady,
			Edges:        []core.ServiceTopologyEdge{{Service: "api", DependsOn: "database", Source: "alpha-extension"}},
		}, nil
	}))
	got := manager.Snapshot(context.Background(), "acme")
	if len(got.Edges) != 1 || got.Edges[0].Source != "static" || got.OmittedEdges != 0 || got.Availability != core.HealthReady {
		t.Fatalf("snapshot = %#v", got)
	}
}
