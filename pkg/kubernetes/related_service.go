package kubernetes

import (
	"context"
	"sort"

	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// Related follows ownership from the selected root, not from every node it
// discovers. Config, identity, and placement are terminal leaves; walking back
// through them would incorrectly include unrelated users of shared resources.
func (service *Service) Related(ctx context.Context, query NeighborhoodQuery) (kubegraph.Graph, error) {
	kind := indexedKind(query.ResourceID)
	if kind == "" || !safeSegment(query.Name) || query.Namespace != "" && !safeSegment(query.Namespace) {
		return kubegraph.Graph{}, ErrInvalidArguments
	}
	kinds := append(append([]string(nil), graphIndexKinds...), "ServiceAccount")
	view, status, err := service.CachedNamespaceSnapshot(ctx, query.Namespace, kinds...)
	if err != nil {
		return kubegraph.Graph{}, err
	}
	result := directedRelated(view, kind, query.Namespace, query.Name)
	result.Sync = kubegraph.SyncStatus{State: status.State, AgeSeconds: status.AgeSeconds, Partial: status.Partial || result.Sync.Partial, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}
	if len(result.Nodes) == 0 && status.State != "syncing" {
		return result, ErrNotFound
	}
	return result, nil
}

func directedRelated(view kubeindex.Snapshot, kind, namespace, name string) kubegraph.Graph {
	result := kubegraph.Graph{Nodes: []kubegraph.Node{}, Edges: []kubegraph.Edge{}, Omitted: map[string]int{}}
	byUID := make(map[string]kubeindex.Record, len(view.Records))
	byName := make(map[string]string, len(view.Records))
	children := make(map[string][]string)
	key := func(kind, ns, name string) string { return kind + "\x00" + ns + "\x00" + name }
	for _, record := range view.Records {
		byUID[record.UID] = record
		byName[key(record.Kind, record.Namespace, record.Name)] = record.UID
	}
	ownerUID := func(owner kubeindex.OwnerRef, ns string) string {
		if owner.UID != "" {
			return owner.UID
		}
		if owner.Namespace == "" {
			owner.Namespace = ns
		}
		return byName[key(owner.Kind, owner.Namespace, owner.Name)]
	}
	for _, record := range view.Records {
		for _, owner := range record.Owners {
			if !owner.ControllerKnown || owner.Controller {
				uid := ownerUID(owner, record.Namespace)
				children[uid] = append(children[uid], record.UID)
			}
		}
	}
	root := byName[key(kind, namespace, name)]
	if root == "" {
		return result
	}
	selected := map[string]bool{}
	workloads := map[string]bool{}
	edges := map[kubegraph.Edge]bool{}
	add := func(uid string) bool {
		if uid == "" || byUID[uid].UID == "" || selected[uid] {
			return false
		}
		selected[uid] = true
		return true
	}
	link := func(from, to string, edgeType kubegraph.EdgeType) {
		if from == "" || to == "" || byUID[from].UID == "" || byUID[to].UID == "" {
			return
		}
		add(from)
		add(to)
		edges[kubegraph.Edge{From: from, To: to, Type: edgeType}] = true
	}
	// Descend only the root's ownership subtree. Ancestors added below never
	// become new descent roots (a selected Pod must not bring in sibling Pods).
	queue := []string{root}
	seen := map[string]bool{}
	for len(queue) > 0 {
		uid := queue[0]
		queue = queue[1:]
		if seen[uid] {
			continue
		}
		seen[uid] = true
		add(uid)
		record := byUID[uid]
		if record.Kind == "HorizontalPodAutoscaler" && record.Target != nil {
			target := ownerUID(*record.Target, record.Namespace)
			link(uid, target, kubegraph.Scales)
			if target != "" {
				queue = append(queue, target)
			}
		}
		if record.Kind == "Ingress" || record.Kind == "HTTPRoute" || record.Kind == "Gateway" {
			for _, ref := range record.References["services"] {
				target := byName[key("Service", record.Namespace, ref)]
				link(uid, target, kubegraph.RoutesTo)
				if target != "" {
					queue = append(queue, target)
				}
			}
		}
		if isWorkloadKind(record.Kind) || record.Kind == "ReplicaSet" {
			workloads[uid] = true
			for _, child := range children[uid] {
				link(uid, child, kubegraph.Manages)
				queue = append(queue, child)
			}
		}
		if record.Kind == "Service" && len(record.Selector) > 0 {
			for _, pod := range view.Records {
				if pod.Kind == "Pod" && pod.Namespace == record.Namespace && labelsMatch(pod.Labels, record.Selector) {
					link(uid, pod.UID, kubegraph.Exposes)
					queue = append(queue, pod.UID)
				}
			}
		}
	}
	queue = queue[:0]
	for uid := range workloads {
		queue = append(queue, uid)
	}
	seen = map[string]bool{}
	for len(queue) > 0 {
		uid := queue[0]
		queue = queue[1:]
		if seen[uid] {
			continue
		}
		seen[uid] = true
		record := byUID[uid]
		for _, owner := range record.Owners {
			if owner.ControllerKnown && !owner.Controller {
				continue
			}
			parent := ownerUID(owner, record.Namespace)
			if byUID[parent].UID != "" {
				link(parent, uid, kubegraph.Manages)
				workloads[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	for uid := range workloads {
		record := byUID[uid]
		for reference, targetKind := range map[string]string{"config_maps": "ConfigMap", "secrets": "Secret", "persistent_volume_claims": "PersistentVolumeClaim", "service_accounts": "ServiceAccount"} {
			for _, ref := range record.References[reference] {
				target := byName[key(targetKind, record.Namespace, ref)]
				if reference == "config_maps" || reference == "secrets" {
					link(target, uid, kubegraph.Configures)
				} else {
					link(uid, target, kubegraph.Uses)
				}
			}
		}
		if record.NodeName != "" {
			link(uid, byName[key("Node", "", record.NodeName)], kubegraph.Uses)
		}
	}
	services := map[string]bool{}
	for uid := range selected {
		if byUID[uid].Kind == "Service" {
			services[uid] = true
		}
	}
	for _, record := range view.Records {
		if record.Kind == "HorizontalPodAutoscaler" && record.Target != nil {
			target := ownerUID(*record.Target, record.Namespace)
			if workloads[target] {
				link(record.UID, target, kubegraph.Scales)
			}
		}
		if record.Kind == "Service" && len(record.Selector) > 0 {
			for uid := range workloads {
				candidate := byUID[uid]
				labels := candidate.Labels
				if candidate.Kind != "Pod" {
					labels = candidate.Selector
				}
				if candidate.Namespace == record.Namespace && labelsMatch(labels, record.Selector) {
					link(record.UID, uid, kubegraph.Exposes)
					services[record.UID] = true
				}
			}
		}
	}
	for _, record := range view.Records {
		if record.Kind != "Ingress" && record.Kind != "HTTPRoute" && record.Kind != "Gateway" {
			continue
		}
		for _, ref := range record.References["services"] {
			target := byName[key("Service", record.Namespace, ref)]
			if services[target] {
				link(record.UID, target, kubegraph.RoutesTo)
			}
		}
	}
	for uid := range selected {
		record := byUID[uid]
		partial := record.Summary["references_partial"] == "true"
		result.Sync.Partial = result.Sync.Partial || partial
		result.Nodes = append(result.Nodes, kubegraph.Node{ID: uid, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, Health: record.Health, Group: record.Namespace, ReferencesPartial: partial})
	}
	for edge := range edges {
		result.Edges = append(result.Edges, edge)
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].ID < result.Nodes[j].ID })
	sort.Slice(result.Edges, func(i, j int) bool {
		a, b := result.Edges[i], result.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Type < b.Type
	})
	return result
}
