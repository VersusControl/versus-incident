package kubernetes

import (
	"testing"

	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// A Gateway, an HTTPRoute attached to it and the Service the route sends to,
// in one namespace, are connected in the complete graph: Gateway to route, and
// route to Service, so a view of connected resources keeps all three.
func TestGatewayRouteAndServiceAreConnected(t *testing.T) {
	t.Parallel()
	service := NewService(nil, Scope{ClusterID: "routes"}, 0)
	objects := []struct {
		definition ResourceDefinition
		raw        map[string]any
	}{
		{ResourceDefinition{ID: "gateway.networking.k8s.io~v1~gateways", Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways", Kind: "Gateway", Namespaced: true},
			map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]any{"name": "edge", "namespace": "shop", "uid": "gateway-edge"}, "spec": map[string]any{"gatewayClassName": "example"}}},
		{ResourceDefinition{ID: "gateway.networking.k8s.io~v1~httproutes", Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes", Kind: "HTTPRoute", Namespaced: true},
			map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": map[string]any{"name": "web", "namespace": "shop", "uid": "route-web"}, "spec": map[string]any{
				"parentRefs": []any{map[string]any{"name": "edge"}},
				"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"name": "web", "port": 80}}}},
			}}},
		{ResourceDefinition{ID: "core~v1~services", Version: "v1", Resource: "services", Kind: "Service", Namespaced: true},
			map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "web", "namespace": "shop", "uid": "service-web"}, "spec": map[string]any{"selector": map[string]any{"app": "web"}}}},
	}
	snapshot := kubeindex.Snapshot{}
	for _, object := range objects {
		snapshot.Records = append(snapshot.Records, indexRecord(service.projectResource(object.definition, object.raw)))
	}
	graph, complete := kubegraph.BuildComplete(snapshot, 100)
	if !complete {
		t.Fatal("the graph was not built in full")
	}
	edges := map[string]bool{}
	for _, edge := range graph.Edges {
		edges[edge.From+">"+edge.To+":"+string(edge.Type)] = true
	}
	for _, want := range []string{"gateway-edge>route-web:routes-to", "route-web>service-web:routes-to"} {
		if !edges[want] {
			t.Fatalf("edge %s is missing; edges=%v", want, edges)
		}
	}
}
