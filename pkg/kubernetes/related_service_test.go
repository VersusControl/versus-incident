package kubernetes

import (
	"strconv"
	"testing"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestRelatedDoesNotTraverseSharedConfigOrPlacement(t *testing.T) {
	record := func(kind, uid string) kubeindex.Record {
		return kubeindex.Record{Kind: kind, UID: uid, Namespace: "shop", Name: uid}
	}
	deployment, replica, pod := record("Deployment", "deployment"), record("ReplicaSet", "replica"), record("Pod", "pod")
	replica.Owners = []kubeindex.OwnerRef{{Kind: "Deployment", UID: deployment.UID}}
	pod.Owners = []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: replica.UID}}
	pod.Labels, pod.NodeName = map[string]string{"app": "api"}, "shared-node"
	pod.References = map[string][]string{"config_maps": {"config"}, "secrets": {"secret"}, "service_accounts": {"account"}, "persistent_volume_claims": {"claim"}}
	otherPod := pod
	otherPod.UID, otherPod.Name = "other-pod", "other-pod"
	otherPod.Labels = map[string]string{"app": "other"}
	otherPod.Owners = []kubeindex.OwnerRef{{Kind: "Deployment", UID: "other-deployment"}}
	sibling := pod
	sibling.UID, sibling.Name = "sibling", "sibling"
	stateful, statefulPod := record("StatefulSet", "stateful"), record("Pod", "stateful-pod")
	statefulPod.Owners = []kubeindex.OwnerRef{{Kind: "StatefulSet", UID: "stateful"}}
	statefulPod.References, statefulPod.NodeName = pod.References, pod.NodeName
	service := record("Service", "service")
	service.Selector = map[string]string{"app": "api"}
	ingress := record("Ingress", "ingress")
	ingress.References = map[string][]string{"services": {"service"}}
	hpa := record("HorizontalPodAutoscaler", "hpa")
	hpa.Target = &kubeindex.OwnerRef{Kind: "Deployment", UID: "deployment"}
	node := record("Node", "shared-node")
	node.Namespace = ""
	view := kubeindex.Snapshot{Records: []kubeindex.Record{deployment, replica, pod, sibling, otherPod, record("Deployment", "other-deployment"), stateful, statefulPod, service, ingress, hpa, node, record("ConfigMap", "config"), record("Secret", "secret"), record("ServiceAccount", "account"), record("PersistentVolumeClaim", "claim")}}
	for _, test := range []struct {
		kind, root      string
		present, absent []string
	}{
		{"Deployment", "deployment", []string{"deployment", "replica", "pod", "sibling", "service", "ingress", "hpa", "config", "secret", "account", "claim", "shared-node"}, []string{"other-deployment", "other-pod", "stateful", "stateful-pod"}},
		{"Pod", "pod", []string{"deployment", "replica", "pod", "service", "ingress", "hpa", "config", "secret", "account", "claim", "shared-node"}, []string{"sibling", "other-pod", "other-deployment", "stateful-pod"}},
		{"StatefulSet", "stateful", []string{"stateful", "stateful-pod", "config", "secret", "account", "claim", "shared-node"}, []string{"deployment", "pod", "other-pod", "service"}},
		{"Service", "service", []string{"service", "ingress", "deployment", "replica", "pod", "sibling", "hpa", "config", "secret", "shared-node"}, []string{"other-pod", "other-deployment", "stateful-pod"}},
		{"ConfigMap", "config", []string{"config"}, []string{"pod", "deployment", "other-pod", "other-deployment", "stateful-pod", "shared-node"}},
		{"Secret", "secret", []string{"secret"}, []string{"pod", "other-pod", "stateful-pod"}},
	} {
		t.Run(test.kind, func(t *testing.T) {
			graph := directedRelated(view, test.kind, "shop", test.root)
			got := map[string]bool{}
			for _, node := range graph.Nodes {
				got[node.ID] = true
			}
			for _, uid := range test.present {
				if !got[uid] {
					t.Errorf("missing %s", uid)
				}
			}
			for _, uid := range test.absent {
				if got[uid] {
					t.Errorf("unrelated %s included", uid)
				}
			}
			if graph.Truncated || graph.Next != "" {
				t.Fatal("related graph was capped")
			}
		})
	}
}

func TestRelatedIncludesSixtyThousandOwnedResources(t *testing.T) {
	records := make([]kubeindex.Record, 1, 60001)
	records[0] = kubeindex.Record{UID: "root", Kind: "Deployment", Namespace: "shop", Name: "api"}
	for i := 0; i < 60000; i++ {
		uid := "pod-" + strconv.Itoa(i)
		records = append(records, kubeindex.Record{UID: uid, Kind: "Pod", Namespace: "shop", Name: uid, Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "root"}}})
	}
	graph := directedRelated(kubeindex.Snapshot{Records: records}, "Deployment", "shop", "api")
	if len(graph.Nodes) != 60001 || len(graph.Edges) != 60000 || graph.Truncated || graph.Next != "" {
		t.Fatalf("related nodes=%d edges=%d truncated=%v next=%q", len(graph.Nodes), len(graph.Edges), graph.Truncated, graph.Next)
	}
}
