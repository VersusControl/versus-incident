package fakekube

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

const maxGeneratedPods = 50000

type generatedPod struct {
	Kind     string               `json:"kind"`
	Metadata generatedPodMetadata `json:"metadata"`
	Spec     generatedPodSpec     `json:"spec"`
	Status   generatedPodStatus   `json:"status"`
}

type generatedPodMetadata struct {
	Name            string              `json:"name"`
	Namespace       string              `json:"namespace"`
	UID             string              `json:"uid"`
	ResourceVersion string              `json:"resourceVersion"`
	Labels          generatedPodLabels  `json:"labels"`
	OwnerReferences []generatedPodOwner `json:"ownerReferences"`
}

type generatedPodLabels struct {
	Application string `json:"app.kubernetes.io/name"`
	App         string `json:"app"`
}

type generatedPodOwner struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type generatedPodSpec struct {
	NodeName   string                  `json:"nodeName"`
	Containers []generatedPodContainer `json:"containers"`
}

type generatedPodContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type generatedPodStatus struct {
	Phase             string                        `json:"phase"`
	ContainerStatuses []generatedPodContainerStatus `json:"containerStatuses"`
}

type generatedPodContainerStatus struct {
	Name         string          `json:"name"`
	Ready        bool            `json:"ready"`
	RestartCount int             `json:"restartCount"`
	State        json.RawMessage `json:"state"`
}

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
	nodes := max(1, min(100, pods/1000))
	deployments := (pods + 24) / 25
	store.reserve(pods + namespaces + nodes + 2*deployments)
	containers := []generatedPodContainer{{Name: "app", Image: "example.invalid/checkout:v1"}}
	running := []generatedPodContainerStatus{{Name: "app", Ready: true, State: json.RawMessage(`{"running":{}}`)}}
	crashing := []generatedPodContainerStatus{{Name: "app", RestartCount: 6, State: json.RawMessage(`{"waiting":{"reason":"CrashLoopBackOff"}}`)}}
	for namespaceIndex := 0; namespaceIndex < namespaces; namespaceIndex++ {
		name := fmt.Sprintf("load-%04d", namespaceIndex)
		if err := putObject(store, "namespaces", "", name, map[string]any{"kind": "Namespace", "metadata": map[string]any{"labels": map[string]string{"fixture": "scale"}}}); err != nil {
			return err
		}
	}
	for nodeIndex := 0; nodeIndex < nodes; nodeIndex++ {
		name := fmt.Sprintf("node-%03d", nodeIndex)
		if err := putObject(store, "nodes", "", name, map[string]any{"kind": "Node", "metadata": map[string]any{}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}); err != nil {
			return err
		}
	}
	for deploymentIndex := 0; deploymentIndex < deployments; deploymentIndex++ {
		namespace := fmt.Sprintf("load-%04d", deploymentIndex%namespaces)
		deploymentName := fmt.Sprintf("app-%05d", deploymentIndex)
		replicasetName := deploymentName + "-rs"
		labels := map[string]string{"app.kubernetes.io/name": deploymentName, "app": deploymentName}
		owner := []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": deploymentName, "uid": "uid-" + deploymentName, "controller": true}}
		if err := putObject(store, "deployments", namespace, deploymentName, map[string]any{"kind": "Deployment", "metadata": map[string]any{"labels": labels, "uid": "uid-" + deploymentName, "generation": 1}, "spec": map[string]any{"replicas": min(25, pods-deploymentIndex*25), "selector": map[string]any{"matchLabels": labels}}, "status": map[string]any{"replicas": min(25, pods-deploymentIndex*25), "readyReplicas": min(25, pods-deploymentIndex*25), "availableReplicas": min(25, pods-deploymentIndex*25), "observedGeneration": 1}}); err != nil {
			return err
		}
		if err := putObject(store, "replicasets", namespace, replicasetName, map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"labels": labels, "uid": "uid-" + replicasetName, "ownerReferences": owner}, "spec": map[string]any{"replicas": min(25, pods-deploymentIndex*25)}}); err != nil {
			return err
		}
		count := min(25, pods-deploymentIndex*25)
		podOwners := []generatedPodOwner{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicasetName, UID: "uid-" + replicasetName, Controller: true}}
		for podIndex := 0; podIndex < count; podIndex++ {
			globalIndex := deploymentIndex*25 + podIndex
			podName := fmt.Sprintf("%s-%02d", replicasetName, podIndex)
			phase := "Running"
			containerStatuses := running
			if random.Intn(100) < 2 {
				phase = "Pending"
				containerStatuses = crashing
			}
			pod := generatedPod{
				Kind:     "Pod",
				Metadata: generatedPodMetadata{Name: podName, Namespace: namespace, UID: fmt.Sprintf("uid-pod-%08d", globalIndex), Labels: generatedPodLabels{Application: deploymentName, App: deploymentName}, OwnerReferences: podOwners},
				Spec:     generatedPodSpec{NodeName: fmt.Sprintf("node-%03d", globalIndex%nodes), Containers: containers},
				Status:   generatedPodStatus{Phase: phase, ContainerStatuses: containerStatuses},
			}
			if err := store.upsertGeneratedPod(pod); err != nil {
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
	if name == "fleet-east" || name == "fleet-west" {
		return seedFleetScenario(store, name)
	}
	if name == "bad-rollout" || name == "bad-rollout-before" {
		return seedBadRollout(store, time.Now().UTC().Truncate(time.Second), name == "bad-rollout")
	}
	if name == "crash-evidence" {
		return seedCrashEvidence(store, time.Now().UTC().Truncate(time.Second))
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
			{"replicasets", "payments", "checkout-api-rs", map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"labels": map[string]string{"app": "checkout"}, "uid": "replicaset-checkout", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout-api", "uid": "deployment-checkout", "controller": true}}}, "spec": map[string]any{"replicas": 3, "selector": map[string]any{"matchLabels": map[string]string{"app": "checkout"}}}}},
			{"pods", "payments", "checkout-api-0", map[string]any{"kind": "Pod", "metadata": map[string]any{"labels": map[string]string{"app": "checkout"}, "uid": "pod-checkout-0", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "checkout-api-rs", "uid": "replicaset-checkout", "controller": true}}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "api", "image": "example.invalid/checkout:v1", "env": []any{map[string]any{"name": "TOKEN", "value": "secret-token"}}, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "api-secret"}}, map[string]any{"configMapRef": map[string]any{"name": "api-config"}}}}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "api", "ready": false, "restartCount": 9, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}}},
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
	if name == "populated" {
		now := time.Now().UTC()
		fixtures[name] = append(fixtures["triage"], fixtures["helm"]...)
		fixtures[name] = append(fixtures[name], fixtures["gitops"]...)
		fixtures[name] = append(fixtures[name], fixtures["graph"]...)
		for _, namespace := range []string{"shop", "argocd", "flux-system"} {
			if err := putObject(store, "namespaces", "", namespace, map[string]any{"kind": "Namespace"}); err != nil {
				return err
			}
		}
		for _, fixture := range fixtures[name] {
			metadata, ok := fixture.object["metadata"].(map[string]any)
			if !ok {
				metadata = map[string]any{}
				fixture.object["metadata"] = metadata
			}
			metadata["creationTimestamp"] = now.Add(-20 * time.Minute).Format(time.RFC3339)
			if fixture.resource == "secrets" {
				delete(fixture.object, "data")
			}
			if fixture.resource == "configmaps" {
				fixture.object["data"] = map[string]string{"mode": "synthetic"}
			}
			if fixture.resource == "pods" && fixture.namespace == "payments" {
				containers := fixture.object["spec"].(map[string]any)["containers"].([]any)
				delete(containers[0].(map[string]any), "env")
			}
			if fixture.resource == "rollouts" {
				fixture.object["spec"] = map[string]any{"replicas": 4, "strategy": map[string]any{"canary": map[string]any{"steps": []any{map[string]any{"setWeight": 25}, map[string]any{"setWeight": 50}}}}}
				fixture.object["status"] = map[string]any{"phase": "Progressing", "currentStepIndex": 1, "stableRS": "checkout-stable", "canaryRS": "checkout-canary", "replicas": 4, "updatedReplicas": 2, "readyReplicas": 4, "availableReplicas": 4}
			}
		}
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
		if fixture.resource == "pods" && (name == "triage" || name == "populated" && fixture.namespace == "payments") {
			spec := fixture.object["spec"].(map[string]any)
			spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "sidecar", "image": "example.invalid/synthetic-sidecar:v1"})
			spec["initContainers"] = []any{map[string]any{"name": "setup", "image": "example.invalid/synthetic-setup:v1"}}
			spec["ephemeralContainers"] = []any{map[string]any{"name": "debug", "image": "example.invalid/synthetic-debug:v1"}}
			status := fixture.object["status"].(map[string]any)
			status["containerStatuses"] = append(status["containerStatuses"].([]any), map[string]any{"name": "sidecar", "ready": true, "restartCount": 1, "state": map[string]any{"running": map[string]any{}}})
			status["initContainerStatuses"] = []any{map[string]any{"name": "setup", "restartCount": 0, "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}}}
			status["ephemeralContainerStatuses"] = []any{map[string]any{"name": "debug", "restartCount": 0, "state": map[string]any{"running": map[string]any{}}}}
		}
		if err := putObject(store, fixture.resource, fixture.namespace, fixture.name, fixture.object); err != nil {
			return err
		}
	}
	return nil
}

func seedCrashEvidence(store *Store, now time.Time) error {
	transition := now.Add(-4 * time.Minute).Format(time.RFC3339)
	finished := now.Add(-3 * time.Minute).Format(time.RFC3339)
	condition := func(kind, status, reason string) map[string]any {
		return map[string]any{"type": kind, "status": status, "reason": reason, "lastTransitionTime": transition, "message": "CANARY_CONDITION"}
	}
	for _, namespace := range []string{"config", "sched", "evict", "batch", "probe", "nodes"} {
		if err := putObject(store, "namespaces", "", namespace, map[string]any{"kind": "Namespace"}); err != nil {
			return err
		}
	}
	if err := putObject(store, "secrets", "config", "missing-key", map[string]any{"kind": "Secret", "type": "Opaque", "data": map[string]string{"present": "c3ludGhldGlj"}}); err != nil {
		return err
	}
	if err := putObject(store, "nodes", "", "pressure-node", map[string]any{"kind": "Node", "status": map[string]any{"conditions": []any{condition("Ready", "True", "KubeletReady"), condition("MemoryPressure", "True", "KubeletHasInsufficientMemory")}}}); err != nil {
		return err
	}
	for _, fixture := range []struct {
		namespace, name, waiting, phase, podReason, event string
		healthy                                           bool
	}{
		{"config", "config-api", "CreateContainerConfigError", "Pending", "", "Failed", false},
		{"sched", "sched-api", "", "Pending", "", "FailedScheduling", false},
		{"evict", "evict-api", "", "Failed", "Evicted", "Evicted", false},
		{"evict", "healthy-api", "", "Failed", "Evicted", "Evicted", true},
		{"batch", "batch-job", "", "Failed", "", "BackoffLimitExceeded", false},
		{"probe", "probe-api", "", "Running", "", "Unhealthy", false},
		{"nodes", "nodes-api", "CrashLoopBackOff", "Running", "", "BackOff", false},
	} {
		uid := "deployment-" + fixture.name
		kind, resource, apiVersion := "Deployment", "deployments", "apps/v1"
		replicas, available := 1, 0
		if fixture.healthy {
			available = 1
		}
		if fixture.namespace == "nodes" {
			replicas = 3
		}
		status := map[string]any{"replicas": replicas, "availableReplicas": available, "readyReplicas": available, "conditions": []any{condition("Available", "False", "MinimumReplicasUnavailable")}}
		if fixture.healthy {
			status["conditions"] = []any{condition("Available", "True", "MinimumReplicasAvailable")}
		}
		if fixture.namespace == "batch" {
			kind, resource, apiVersion, uid = "Job", "jobs", "batch/v1", "job-"+fixture.name
			status = map[string]any{"failed": 1, "conditions": []any{condition("Failed", "True", "BackoffLimitExceeded")}}
		}
		labels := map[string]string{"app": fixture.name}
		spec := map[string]any{"replicas": replicas, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "example.invalid/fixture:v1"}}}}}
		owner := map[string]any{"apiVersion": apiVersion, "kind": kind, "name": fixture.name, "uid": uid, "controller": true}
		if kind == "Deployment" {
			rsName, rsUID := fixture.name+"-rs", "rs-"+fixture.name
			if err := putObject(store, "replicasets", fixture.namespace, rsName, map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"uid": rsUID, "ownerReferences": []any{owner}}, "spec": map[string]any{"replicas": replicas}, "status": map[string]any{"replicas": replicas, "availableReplicas": available, "readyReplicas": available}}); err != nil {
				return err
			}
			owner = map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": rsName, "uid": rsUID, "controller": true}
		}
		podUID := "pod-" + fixture.name
		container := map[string]any{"name": "app", "restartCount": 0, "ready": false, "state": map[string]any{"running": map[string]any{}}}
		if fixture.phase == "Failed" {
			container["state"] = map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": 1, "finishedAt": finished, "message": "CANARY_TERMINATION"}}
		}
		if fixture.waiting != "" {
			container["state"] = map[string]any{"waiting": map[string]any{"reason": fixture.waiting, "message": "CANARY_WAITING"}}
		}
		podStatus := map[string]any{"phase": fixture.phase, "reason": fixture.podReason, "message": "CANARY_POD", "conditions": []any{condition("Ready", "False", "ContainersNotReady")}, "containerStatuses": []any{container}}
		podSpec := map[string]any{"containers": []any{map[string]any{"name": "app", "image": "example.invalid/fixture:v1"}}}
		if fixture.namespace == "sched" {
			podStatus["conditions"] = []any{condition("PodScheduled", "False", "Unschedulable")}
		}
		if fixture.namespace == "config" {
			podSpec["containers"] = []any{map[string]any{"name": "app", "image": "example.invalid/fixture:v1", "env": []any{map[string]any{"name": "CONFIG", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "missing-key", "key": "missing"}}}}}}
		}
		if fixture.namespace == "probe" {
			podSpec["containers"] = []any{map[string]any{"name": "app", "image": "example.invalid/fixture:v1", "readinessProbe": map[string]any{"exec": map[string]any{"command": []string{"false"}}}}}
		}
		if fixture.namespace == "nodes" {
			podSpec["nodeName"] = "pressure-node"
			container["restartCount"] = 6
			container["lastState"] = map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137, "signal": 9, "finishedAt": finished, "message": "CANARY_TERMINATION"}}
		}
		if fixture.namespace == "batch" {
			podSpec["restartPolicy"] = "Never"
			spec = map[string]any{"backoffLimit": 1, "template": map[string]any{"spec": podSpec}}
		} else {
			spec["template"].(map[string]any)["spec"] = podSpec
		}
		if err := putObject(store, resource, fixture.namespace, fixture.name, map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"uid": uid, "labels": labels, "creationTimestamp": now.Add(-time.Hour).Format(time.RFC3339)}, "spec": spec, "status": status}); err != nil {
			return err
		}
		if err := putObject(store, "pods", fixture.namespace, fixture.name+"-0", map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": podUID, "labels": labels, "ownerReferences": []any{owner}}, "spec": podSpec, "status": podStatus}); err != nil {
			return err
		}
		if fixture.namespace == "nodes" {
			runningStatus := map[string]any{"phase": "Running", "conditions": []any{condition("Ready", "False", "ContainersNotReady")}, "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 8, "state": map[string]any{"running": map[string]any{}}, "lastState": container["lastState"]}}}
			if err := putObject(store, "pods", fixture.namespace, fixture.name+"-between", map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": podUID + "-between", "labels": labels, "ownerReferences": []any{owner}}, "spec": podSpec, "status": runningStatus}); err != nil {
				return err
			}
			initStatus := map[string]any{"phase": "Pending", "conditions": []any{condition("Ready", "False", "ContainersNotReady")}, "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 0, "state": map[string]any{"waiting": map[string]any{"reason": "PodInitializing"}}}}, "initContainerStatuses": []any{map[string]any{"name": "setup", "restartCount": 3, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "CANARY_INIT"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": 1, "finishedAt": finished, "message": "CANARY_INIT_EXIT"}}}}}
			initSpec := map[string]any{"nodeName": "pressure-node", "containers": podSpec["containers"], "initContainers": []any{map[string]any{"name": "setup", "image": "example.invalid/setup:v1"}}}
			if err := putObject(store, "pods", fixture.namespace, fixture.name+"-init", map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": podUID + "-init", "labels": labels, "ownerReferences": []any{owner}}, "spec": initSpec, "status": initStatus}); err != nil {
				return err
			}
		}
		if fixture.healthy {
			if err := putObject(store, "pods", fixture.namespace, fixture.name+"-ready", map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": podUID + "-ready", "labels": labels, "ownerReferences": []any{owner}}, "spec": podSpec, "status": map[string]any{"phase": "Running", "conditions": []any{condition("Ready", "True", "ContainersReady")}, "containerStatuses": []any{map[string]any{"name": "app", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{}}}}}}); err != nil {
				return err
			}
		}
		if err := putObject(store, "events", fixture.namespace, fixture.name+"-warning", map[string]any{"kind": "Event", "type": "Warning", "reason": fixture.event, "message": "CANARY_EVENT", "count": 4, "firstTimestamp": now.Add(-2 * time.Minute).Format(time.RFC3339), "lastTimestamp": now.Format(time.RFC3339), "involvedObject": map[string]any{"kind": "Pod", "name": fixture.name + "-0", "namespace": fixture.namespace, "uid": podUID}}); err != nil {
			return err
		}
	}
	return nil
}

func seedFleetScenario(store *Store, scenario string) error {
	namespace, workload, nodeCount, podCount := "east-shop", "east-api", 1, 2
	if scenario == "fleet-west" {
		namespace, workload, nodeCount, podCount = "west-billing", "west-worker", 2, 3
	}
	if err := putObject(store, "namespaces", "", namespace, map[string]any{"kind": "Namespace", "status": map[string]any{"phase": "Active"}}); err != nil {
		return err
	}
	for index := 0; index < nodeCount; index++ {
		if err := putObject(store, "nodes", "", fmt.Sprintf("%s-node-%d", scenario, index), map[string]any{"kind": "Node", "status": map[string]any{"nodeInfo": map[string]any{"kubeletVersion": "v1.30.4"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}); err != nil {
			return err
		}
	}
	labels := map[string]string{"app": workload}
	if err := putObject(store, "deployments", namespace, workload, map[string]any{"kind": "Deployment", "metadata": map[string]any{"labels": labels}, "spec": map[string]any{"replicas": podCount, "selector": map[string]any{"matchLabels": labels}}, "status": map[string]any{"replicas": podCount, "readyReplicas": podCount, "availableReplicas": podCount, "observedGeneration": 1}}); err != nil {
		return err
	}
	for index := 0; index < podCount; index++ {
		pod := fmt.Sprintf("%s-%d", workload, index)
		if err := putObject(store, "pods", namespace, pod, map[string]any{"kind": "Pod", "metadata": map[string]any{"labels": labels}, "spec": map[string]any{"nodeName": fmt.Sprintf("%s-node-%d", scenario, index%nodeCount), "containers": []any{map[string]any{"name": "app", "image": "example.invalid/" + workload + ":v1"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{}}}}}}); err != nil {
			return err
		}
	}
	return nil
}

func putObject(store *Store, resource, namespace, name string, object map[string]any) error {
	return store.upsertObject(resource, namespace, name, object)
}

func seedBadRollout(store *Store, now time.Time, broken bool) error {
	for _, namespace := range []string{"shop", "payments", "kube-system"} {
		if err := putObject(store, "namespaces", "", namespace, map[string]any{"kind": "Namespace"}); err != nil {
			return err
		}
	}
	if err := putObject(store, "nodes", "", "node-a", map[string]any{"kind": "Node", "metadata": map[string]any{"uid": "node-a"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}); err != nil {
		return err
	}
	for _, workload := range []struct {
		namespace, name    string
		desired, available int
		image              string
	}{{"shop", "checkout", 7, 0, "v42"}, {"payments", "ledger-api", 4, 2, "v2"}, {"shop", "cart", 1, 1, "v1"}} {
		if !broken && workload.name == "checkout" {
			workload.desired, workload.available, workload.image = 6, 6, "v41"
		}
		if !broken && workload.name == "ledger-api" {
			workload.available, workload.image = 4, "v1"
		}
		labels := map[string]string{"app": workload.name}
		condition := []any{}
		if broken && workload.name != "cart" {
			condition = append(condition, map[string]any{"type": "Available", "status": "False", "lastTransitionTime": now.Add(-4 * time.Minute).Format(time.RFC3339)})
			if workload.name == "ledger-api" {
				condition = append(condition, map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded", "lastTransitionTime": now.Add(-4 * time.Minute).Format(time.RFC3339)})
			}
		}
		if err := putObject(store, "deployments", workload.namespace, workload.name, map[string]any{"kind": "Deployment", "metadata": map[string]any{"uid": "deployment-" + workload.name, "labels": labels, "generation": 2}, "spec": map[string]any{"replicas": workload.desired, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "example.invalid/" + workload.name + ":" + workload.image, "envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": "checkout-cfg"}}}}}}}}, "status": map[string]any{"replicas": workload.desired, "readyReplicas": workload.available, "availableReplicas": workload.available, "updatedReplicas": workload.desired, "observedGeneration": 2, "conditions": condition}}); err != nil {
			return err
		}
		current := workload.name + "-current"
		if workload.name == "checkout" {
			current = "checkout-v41"
			if broken {
				current = "checkout-v42"
			}
			for _, revision := range []struct {
				name, version, image string
				minutes              int
			}{{"checkout-v41", "41", "v41", 12}, {"checkout-v42", "42", "v42", 8}} {
				if !broken && revision.version == "42" {
					continue
				}
				if err := putObject(store, "replicasets", workload.namespace, revision.name, map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"uid": revision.name, "annotations": map[string]any{"deployment.kubernetes.io/revision": revision.version}, "creationTimestamp": now.Add(-time.Duration(revision.minutes) * time.Minute).Format(time.RFC3339), "ownerReferences": []any{map[string]any{"kind": "Deployment", "name": "checkout", "uid": "deployment-checkout", "controller": true}}}, "spec": map[string]any{"replicas": workload.desired, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "example.invalid/checkout:" + revision.image, "env": []any{map[string]any{"name": "PRIVATE", "value": "CANARY_REVISION_ENV"}}}}}}}}); err != nil {
					return err
				}
			}
		} else {
			if err := putObject(store, "replicasets", workload.namespace, current, map[string]any{"kind": "ReplicaSet", "metadata": map[string]any{"uid": current, "ownerReferences": []any{map[string]any{"kind": "Deployment", "name": workload.name, "uid": "deployment-" + workload.name, "controller": true}}}}); err != nil {
				return err
			}
		}
		for position := 0; position < workload.desired; position++ {
			name := fmt.Sprintf("%s-%d", workload.name, position)
			state := map[string]any{"running": map[string]any{}}
			phase, ready := "Running", true
			last := map[string]any{}
			if broken && workload.name == "checkout" {
				ready = false
				state = map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "CANARY_WAITING"}}
				if position < 6 {
					last = map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137, "finishedAt": now.Add(-4 * time.Minute).Format(time.RFC3339), "message": "CANARY_TERMINATION"}}
				} else {
					phase = "Failed"
					state = map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": 1, "finishedAt": now.Add(-4 * time.Minute).Format(time.RFC3339)}}
				}
			}
			if broken && workload.name == "ledger-api" && position < 2 {
				ready = false
				phase = "Pending"
				state = map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}}
			}
			readyStatus := "True"
			if !ready {
				readyStatus = "False"
			}
			if err := putObject(store, "pods", workload.namespace, name, map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": "pod-" + name, "labels": labels, "ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "name": current, "uid": current, "controller": true}}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "app", "image": "example.invalid/" + workload.name + ":" + workload.image}}}, "status": map[string]any{"phase": phase, "conditions": []any{map[string]any{"type": "Ready", "status": readyStatus, "lastTransitionTime": now.Add(-4 * time.Minute).Format(time.RFC3339)}}, "containerStatuses": []any{map[string]any{"name": "app", "ready": ready, "restartCount": 6, "state": state, "lastState": last}}}}); err != nil {
				return err
			}
		}
	}
	if err := putObject(store, "configmaps", "shop", "checkout-cfg", map[string]any{"kind": "ConfigMap", "metadata": map[string]any{"uid": "checkout-cfg"}, "data": map[string]any{"private": "CANARY_CONFIG_VALUE"}}); err != nil {
		return err
	}
	desired := 6
	if broken {
		desired = 7
	}
	if err := putObject(store, "horizontalpodautoscalers", "shop", "checkout", map[string]any{"kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"uid": "checkout-hpa"}, "spec": map[string]any{"scaleTargetRef": map[string]any{"kind": "Deployment", "name": "checkout"}, "minReplicas": 6, "maxReplicas": 7}, "status": map[string]any{"desiredReplicas": desired}}); err != nil {
		return err
	}
	nodeReady := 12
	if broken {
		nodeReady = 5
	}
	if err := putObject(store, "daemonsets", "kube-system", "node-agent", map[string]any{"kind": "DaemonSet", "metadata": map[string]any{"uid": "node-agent"}, "status": map[string]any{"desiredNumberScheduled": 12, "numberReady": nodeReady}}); err != nil {
		return err
	}
	return putObject(store, "events", "shop", "cart-warning", map[string]any{"kind": "Event", "type": "Warning", "count": 1, "firstTimestamp": now.Add(-time.Minute).Format(time.RFC3339), "lastTimestamp": now.Add(-time.Minute).Format(time.RFC3339), "involvedObject": map[string]any{"kind": "Deployment", "name": "cart", "namespace": "shop", "uid": "deployment-cart"}})
}

func trafficMetrics(elapsed time.Duration) string {
	seconds := max(1, elapsed.Seconds())
	var output strings.Builder
	labels := `reporter="destination",request_protocol="http",source_workload="checkout",source_workload_namespace="shop",destination_workload="checkout-api",destination_workload_namespace="payments"`
	fmt.Fprintf(&output, "# TYPE istio_requests_total counter\nistio_requests_total{%s,response_code=\"200\"} %.3f\nistio_requests_total{%s,response_code=\"500\"} %.3f\n", labels, 196*seconds, labels, 4*seconds)
	output.WriteString("# TYPE istio_request_duration_milliseconds histogram\n")
	for _, bucket := range []struct {
		bound string
		rate  float64
	}{{"50", 100}, {"100", 170}, {"250", 190}, {"500", 198}, {"+Inf", 200}} {
		fmt.Fprintf(&output, "istio_request_duration_milliseconds_bucket{%s,le=\"%s\"} %.3f\n", labels, bucket.bound, bucket.rate*seconds)
	}
	fmt.Fprintf(&output, "istio_request_duration_milliseconds_sum{%s} %.3f\nistio_request_duration_milliseconds_count{%s} %.3f\n", labels, 24000*seconds, labels, 200*seconds)
	return output.String()
}
