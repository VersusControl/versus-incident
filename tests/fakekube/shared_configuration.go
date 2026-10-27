package fakekube

import "fmt"

// Seed the whole relationship set before a consumer starts its first sync.
func seedSharedConfiguration(store *Store) error {
	const namespace = "relation-acceptance"
	put := func(resource, kind, ns, name string, spec map[string]any, labels map[string]string, owners []any) error {
		metadata := map[string]any{"uid": "acceptance-" + name}
		if labels != nil {
			metadata["labels"] = labels
		}
		if owners != nil {
			metadata["ownerReferences"] = owners
		}
		return putObject(store, resource, ns, name, map[string]any{"kind": kind, "metadata": metadata, "spec": spec})
	}
	for _, item := range []struct{ resource, kind, ns, name string }{
		{"namespaces", "Namespace", "", namespace}, {"nodes", "Node", "", "related-node"},
		{"secrets", "Secret", namespace, "shared-secret"}, {"configmaps", "ConfigMap", namespace, "shared-config"},
	} {
		if err := put(item.resource, item.kind, item.ns, item.name, nil, nil, nil); err != nil {
			return err
		}
	}
	podSpec := func() map[string]any {
		return map[string]any{"nodeName": "related-node", "containers": []any{map[string]any{
			"name": "app", "image": "example.invalid/fixture:v1",
			"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "shared-config"}},
				map[string]any{"secretRef": map[string]any{"name": "shared-secret"}}},
		}}}
	}
	for index := 0; index <= 10; index++ {
		name := "related-primary"
		if index > 0 {
			name = fmt.Sprintf("related-sibling-%d", index)
		}
		labels := map[string]string{"app": name}
		spec := map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec()}}
		if err := put("deployments", "Deployment", namespace, name, spec, labels, nil); err != nil {
			return err
		}
		owner := []any{map[string]any{"kind": "Deployment", "name": name, "uid": "acceptance-" + name, "controller": true}}
		if err := put("replicasets", "ReplicaSet", namespace, name+"-rs", map[string]any{"replicas": 1}, labels, owner); err != nil {
			return err
		}
		owner = []any{map[string]any{"kind": "ReplicaSet", "name": name + "-rs", "uid": "acceptance-" + name + "-rs", "controller": true}}
		if err := put("pods", "Pod", namespace, name+"-pod", podSpec(), labels, owner); err != nil {
			return err
		}
	}
	if err := put("services", "Service", namespace, "related-service",
		map[string]any{"selector": map[string]string{"app": "related-primary"}, "ports": []any{map[string]any{"port": 80}}}, nil, nil); err != nil {
		return err
	}
	labels := map[string]string{"app": "related-stateful"}
	if err := put("statefulsets", "StatefulSet", namespace, "related-stateful",
		map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}}, labels, nil); err != nil {
		return err
	}
	return put("pods", "Pod", namespace, "related-stateful-0", podSpec(), labels,
		[]any{map[string]any{"kind": "StatefulSet", "name": "related-stateful", "uid": "acceptance-related-stateful", "controller": true}})
}
