package fakekube

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
)

const maxGeneratedPods = 50000

func Generate(store *Store, pods, namespaces int, seed int64) error {
	if store == nil || pods < 0 || pods > maxGeneratedPods || namespaces < 1 || namespaces > 2000 {
		return ErrInvalidRequest
	}
	if pods == 0 {
		return nil
	}
	if seed == 0 {
		seed = 1
	}
	random := rand.New(rand.NewSource(seed))
	for namespaceIndex := 0; namespaceIndex < namespaces; namespaceIndex++ {
		name := fmt.Sprintf("load-%04d", namespaceIndex)
		if err := putObject(store, "namespaces", "", name, map[string]any{"kind": "Namespace", "metadata": map[string]any{"labels": map[string]string{"fixture": "scale"}}}); err != nil {
			return err
		}
	}
	nodes := max(1, min(100, pods/1000))
	for nodeIndex := 0; nodeIndex < nodes; nodeIndex++ {
		name := fmt.Sprintf("node-%03d", nodeIndex)
		if err := putObject(store, "nodes", "", name, map[string]any{"kind": "Node", "metadata": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}); err != nil {
			return err
		}
	}
	deployments := (pods + 24) / 25
	for deploymentIndex := 0; deploymentIndex < deployments; deploymentIndex++ {
		namespace := fmt.Sprintf("load-%04d", deploymentIndex%namespaces)
		deploymentName := fmt.Sprintf("app-%05d", deploymentIndex)
		replicasetName := deploymentName + "-rs"
		labels := map[string]string{"app.kubernetes.io/name": deploymentName, "app": deploymentName}
		owner := []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": deploymentName, "uid": "uid-" + deploymentName}}
		if err := putObject(store, "deployments", namespace, deploymentName, map[string]any{"kind": "Deployment", "metadata": map[string]any{"labels": labels, "uid": "uid-" + deploymentName, "generation": 1}, "spec": map[string]any{"replicas": min(25, pods-deploymentIndex*25), "selector": map[string]any{"matchLabels": labels}}, "status": map[string]any{"replicas": min(25, pods-deploymentIndex*25), "readyReplicas": min(25, pods-deploymentIndex*25), "availableReplicas": min(25, pods-deploymentIndex*25), "observedGeneration": 1}}); err != nil {
			return err
		}
		if err := putObject(store, "replicasets", namespace, replicasetName, map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"labels": labels, "uid": "uid-" + replicasetName, "ownerReferences": owner}, "spec": map[string]any{"replicas": min(25, pods-deploymentIndex*25)}}); err != nil {
			return err
		}
		count := min(25, pods-deploymentIndex*25)
		for podIndex := 0; podIndex < count; podIndex++ {
			globalIndex := deploymentIndex*25 + podIndex
			podName := fmt.Sprintf("%s-%02d", replicasetName, podIndex)
			phase := "Running"
			containerStatus := map[string]any{"name": "app", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{}}}
			if random.Intn(100) < 2 {
				phase = "Pending"
				containerStatus = map[string]any{"name": "app", "ready": false, "restartCount": 6, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}
			}
			if err := putObject(store, "pods", namespace, podName, map[string]any{
				"kind": "Pod", "metadata": map[string]any{"labels": labels, "uid": fmt.Sprintf("uid-pod-%08d", globalIndex), "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": replicasetName, "uid": "uid-" + replicasetName}}},
				"spec":   map[string]any{"nodeName": fmt.Sprintf("node-%03d", globalIndex%nodes), "containers": []any{map[string]any{"name": "app", "image": "example.invalid/checkout:v1"}}},
				"status": map[string]any{"phase": phase, "containerStatuses": []any{containerStatus}},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func SeedScenario(store *Store, name string, seed int64) error {
	if store == nil {
		return ErrInvalidRequest
	}
	fixtures := map[string][]struct {
		resource  string
		namespace string
		name      string
		object    map[string]any
	}{
		"triage": {
			{"namespaces", "", "payments", map[string]any{"kind": "Namespace", "metadata": map[string]any{}}},
			{"namespaces", "", "inventory-only", map[string]any{"kind": "Namespace", "metadata": map[string]any{}}},
			{"services", "payments", "checkout-api", map[string]any{"kind": "Service", "metadata": map[string]any{}, "spec": map[string]any{"selector": map[string]string{"app": "checkout"}, "ports": []any{map[string]any{"port": 80}}}}},
			{"deployments", "payments", "checkout-api", map[string]any{"kind": "Deployment", "metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": "checkout"}, "uid": "deployment-checkout"}, "spec": map[string]any{"replicas": 3, "selector": map[string]any{"matchLabels": map[string]string{"app": "checkout"}}}, "status": map[string]any{"replicas": 3, "readyReplicas": 1, "availableReplicas": 1, "observedGeneration": 1, "conditions": []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}}}},
			{"pods", "payments", "checkout-api-0", map[string]any{"kind": "Pod", "metadata": map[string]any{"labels": map[string]string{"app": "checkout"}, "uid": "pod-checkout-0", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout-api", "uid": "deployment-checkout"}}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "api", "image": "example.invalid/checkout:v1", "env": []any{map[string]any{"name": "TOKEN", "value": "secret-token"}}, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "api-secret"}}, map[string]any{"configMapRef": map[string]any{"name": "api-config"}}}}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "api", "ready": false, "restartCount": 9, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}}},
			{"secrets", "payments", "api-secret", map[string]any{"kind": "Secret", "type": "Opaque", "metadata": map[string]any{}, "data": map[string]string{"token": "c2VjcmV0LXRva2Vu"}}},
			{"configmaps", "payments", "api-config", map[string]any{"kind": "ConfigMap", "metadata": map[string]any{}, "data": map[string]string{"config.yaml": "token: must-not-cross"}}},
			{"jobs", "payments", "settlement", map[string]any{"kind": "Job", "metadata": map[string]any{}, "status": map[string]any{"failed": 1, "conditions": []any{map[string]any{"type": "Failed", "status": "True"}}}}},
			{"persistentvolumeclaims", "payments", "database", map[string]any{"kind": "PersistentVolumeClaim", "metadata": map[string]any{}, "status": map[string]any{"phase": "Pending"}}},
			{"events", "payments", "checkout-warning", map[string]any{"kind": "Event", "type": "Warning", "reason": "BackOff", "message": "container restart backoff", "metadata": map[string]any{}}},
			{"nodes", "", "node-a", map[string]any{"kind": "Node", "metadata": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}}},
		},
		"helm": {
			{"secrets", "shop", "sh.helm.release.v1.checkout.v1", map[string]any{"kind": "Secret", "metadata": map[string]any{"labels": map[string]string{"owner": "helm", "name": "checkout", "status": "failed", "version": "1"}}, "data": map[string]string{"release": "c2VjcmV0LWNhbmFyeS1oZWxtLXZhbHVlcw=="}}},
			{"secrets", "shop", "sh.helm.release.v1.checkout.v2", map[string]any{"kind": "Secret", "metadata": map[string]any{"labels": map[string]string{"owner": "helm", "name": "checkout", "status": "deployed", "version": "2"}}, "data": map[string]string{"release": "c2VjcmV0LWNhbmFyeS1oZWxtLW1hbmlmZXN0"}}},
		},
		"gitops": {
			{"applications", "argocd", "checkout", map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{}, "status": map[string]any{"sync": map[string]any{"status": "OutOfSync", "revision": "abc123"}, "health": map[string]any{"status": "Degraded"}}}},
			{"kustomizations", "flux-system", "checkout", map[string]any{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": map[string]any{}, "status": map[string]any{"lastAppliedRevision": "def456", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}},
			{"rollouts", "shop", "checkout", map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout", "metadata": map[string]any{}, "spec": map[string]any{"strategy": map[string]any{"canary": map[string]any{"steps": []any{map[string]any{"setWeight": 50}}}}}, "status": map[string]any{"phase": "Progressing", "currentStepIndex": 1, "stepCount": 2}}},
		},
		"graph": {
			{"services", "shop", "checkout", map[string]any{"kind": "Service", "metadata": map[string]any{"labels": map[string]string{"app": "checkout"}}, "spec": map[string]any{"selector": map[string]string{"app": "checkout"}, "ports": []any{map[string]any{"port": 80}}}}},
			{"ingresses", "shop", "checkout", map[string]any{"kind": "Ingress", "metadata": map[string]any{}, "spec": map[string]any{"rules": []any{map[string]any{"http": map[string]any{"paths": []any{map[string]any{"backend": map[string]any{"service": map[string]any{"name": "checkout"}}}}}}}}}},
			{"configmaps", "shop", "checkout-config", map[string]any{"kind": "ConfigMap", "metadata": map[string]any{}, "data": map[string]string{"mode": "safe"}}},
			{"secrets", "shop", "checkout-secret", map[string]any{"kind": "Secret", "metadata": map[string]any{}, "data": map[string]string{"password": "fakekube-canary-never-project"}}},
			{"pods", "shop", "checkout-0", map[string]any{"kind": "Pod", "metadata": map[string]any{"labels": map[string]string{"app": "checkout"}}, "spec": map[string]any{"nodeName": "node-a"}, "status": map[string]any{"phase": "Running"}}},
		},
		"limited-rbac": {},
		"scale":        {},
	}
	switch name {
	case "fault-forbidden-secret", "fault-transient-pods", "fault-partial-pods":
		name = "triage"
	}
	objects, ok := fixtures[name]
	if !ok {
		return errors.New("fakekube: unknown scenario")
	}
	if seed == 0 {
		seed = 1
	}
	_ = rand.New(rand.NewSource(seed))
	for _, fixture := range objects {
		if err := putObject(store, fixture.resource, fixture.namespace, fixture.name, fixture.object); err != nil {
			return err
		}
	}
	return nil
}

func putObject(store *Store, resource, namespace, name string, object any) error {
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return store.Upsert(resource, namespace, name, encoded)
}
