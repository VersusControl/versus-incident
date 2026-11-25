//go:build integration

package kubernetes

// Live-cluster series recorder. For every cluster in a connectors file it
// records, over one window, the raw time series a sibling, version, trend and
// grouping comparison would read: metrics.k8s.io Pod usage per container at a
// short interval, and once a minute the Pods, workloads, autoscalers, node
// usage and Warning events. Analysis happens offline on the recorded file, so
// a rule can be replayed without touching the cluster again.
//
// Read-only: GET lists and bounded pods/log reads. Log content is counted and
// never stored. Clusters run in parallel, one subtest each; a cluster that
// cannot be reached fails only its own subtest and leaves a status file with
// the error class.
//
// It skips unless KUBE_SAMPLE_CONNECTORS names the connectors file and
// HARNESS_DATA_DIR and HARNESS_RESULTS_DIR name the output directories.
// KUBE_SAMPLE_CLUSTERS (comma-separated cluster IDs) narrows the set.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/redaction"
)

// ---------------------------------------------------------------- wire types

type seriesMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	Generation        int64             `json:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []sampleOwner     `json:"ownerReferences,omitempty"`
}

func (meta seriesMeta) controller() (sampleOwner, bool) {
	for _, owner := range meta.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			return owner, true
		}
	}
	if len(meta.OwnerReferences) == 1 {
		return meta.OwnerReferences[0], true
	}
	return sampleOwner{}, false
}

type seriesTerminated struct {
	Reason     string     `json:"reason"`
	ExitCode   int        `json:"exitCode"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

type seriesContainerState struct {
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Running *struct {
		StartedAt *time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *seriesTerminated `json:"terminated"`
}

type seriesContainerStatus struct {
	Name         string               `json:"name"`
	Ready        bool                 `json:"ready"`
	RestartCount int                  `json:"restartCount"`
	State        seriesContainerState `json:"state"`
	LastState    seriesContainerState `json:"lastState"`
}

type seriesPod struct {
	Metadata seriesMeta `json:"metadata"`
	Spec     struct {
		NodeName       string            `json:"nodeName"`
		Containers     []sampleContainer `json:"containers"`
		InitContainers []sampleContainer `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase             string                  `json:"phase"`
		Reason            string                  `json:"reason"`
		Conditions        []sampleCondition       `json:"conditions"`
		ContainerStatuses []seriesContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

func (pod seriesPod) key() string { return pod.Metadata.Namespace + "/" + pod.Metadata.Name }

func (pod seriesPod) readySince() (bool, *time.Time) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True", condition.LastTransitionTime
		}
	}
	return false, nil
}

// version is the identity sibling Pods share: the ReplicaSet name for a
// Deployment Pod, the controller revision for the other kinds.
func (pod seriesPod) version() string {
	owner, ok := pod.Metadata.controller()
	if !ok {
		return ""
	}
	if owner.Kind == "ReplicaSet" || owner.Kind == "Job" {
		return owner.Name
	}
	return pod.Metadata.Labels["controller-revision-hash"]
}

type seriesCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	LastUpdateTime     *time.Time `json:"lastUpdateTime,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

type seriesRolling struct {
	MaxSurge       json.RawMessage `json:"maxSurge"`
	MaxUnavailable json.RawMessage `json:"maxUnavailable"`
	Partition      *int32          `json:"partition"`
}

type seriesWorkload struct {
	Metadata seriesMeta `json:"metadata"`
	Spec     struct {
		Replicas                *int32 `json:"replicas"`
		Paused                  bool   `json:"paused"`
		MinReadySeconds         int32  `json:"minReadySeconds"`
		ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds"`
		PodManagementPolicy     string `json:"podManagementPolicy"`
		Strategy                struct {
			Type          string         `json:"type"`
			RollingUpdate *seriesRolling `json:"rollingUpdate"`
		} `json:"strategy"`
		UpdateStrategy struct {
			Type          string         `json:"type"`
			RollingUpdate *seriesRolling `json:"rollingUpdate"`
		} `json:"updateStrategy"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string   `json:"name"`
					Image string   `json:"image"`
					Args  []string `json:"args"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas               int32             `json:"replicas"`
		ReadyReplicas          int32             `json:"readyReplicas"`
		UpdatedReplicas        int32             `json:"updatedReplicas"`
		AvailableReplicas      int32             `json:"availableReplicas"`
		UnavailableReplicas    int32             `json:"unavailableReplicas"`
		ObservedGeneration     int64             `json:"observedGeneration"`
		CurrentRevision        string            `json:"currentRevision"`
		UpdateRevision         string            `json:"updateRevision"`
		DesiredNumberScheduled int32             `json:"desiredNumberScheduled"`
		NumberReady            int32             `json:"numberReady"`
		UpdatedNumberScheduled int32             `json:"updatedNumberScheduled"`
		NumberAvailable        int32             `json:"numberAvailable"`
		Conditions             []seriesCondition `json:"conditions"`
	} `json:"status"`
}

type seriesAutoscaler struct {
	Metadata seriesMeta `json:"metadata"`
	Spec     struct {
		ScaleTargetRef struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"scaleTargetRef"`
		MinReplicas *int32 `json:"minReplicas"`
		MaxReplicas int32  `json:"maxReplicas"`
		Metrics     []struct {
			Type     string `json:"type"`
			Resource *struct {
				Name string `json:"name"`
			} `json:"resource"`
		} `json:"metrics"`
	} `json:"spec"`
	Status struct {
		CurrentReplicas int32             `json:"currentReplicas"`
		DesiredReplicas int32             `json:"desiredReplicas"`
		LastScaleTime   *time.Time        `json:"lastScaleTime"`
		Conditions      []seriesCondition `json:"conditions"`
	} `json:"status"`
}

type seriesNode struct {
	Metadata seriesMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []seriesCondition `json:"conditions"`
	} `json:"status"`
}

type seriesUsage struct {
	Metadata   seriesMeta        `json:"metadata"`
	Timestamp  time.Time         `json:"timestamp"`
	Window     string            `json:"window"`
	Usage      map[string]string `json:"usage"` // node metrics
	Containers []struct {
		Name  string            `json:"name"`
		Usage map[string]string `json:"usage"`
	} `json:"containers"`
}

type seriesEvent struct {
	Metadata       seriesMeta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"involvedObject"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Type           string     `json:"type"`
	Count          int64      `json:"count"`
	FirstTimestamp *time.Time `json:"firstTimestamp"`
	LastTimestamp  *time.Time `json:"lastTimestamp"`
	Series         *struct {
		Count int64 `json:"count"`
	} `json:"series"`
}

// ---------------------------------------------------------------- recorder

type seriesRecorder struct {
	probe   *sampleProbe
	mu      sync.Mutex
	file    *os.File
	out     *bufio.Writer
	counts  map[string]int
	errors  map[string]int
	started time.Time

	stateMu   sync.Mutex
	lastPods  []seriesPod
	seenEvent map[string]bool
}

var seriesNodeHost = regexp.MustCompile(`ip-\d+-\d+-\d+-\d+\.[a-z0-9.-]*(?:internal|amazonaws\.com)`)

// emit writes one record as a JSON line. The whole line passes through the
// product redactor and the endpoint mask before it reaches the disk.
func (recorder *seriesRecorder) emit(kind string, fields map[string]any) {
	fields["t"] = kind
	if _, ok := fields["at"]; !ok {
		fields["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return
	}
	// Node host names carry addresses and appear in event messages and lease
	// holders: they leave as the same alias the node rows use.
	line := seriesNodeHost.ReplaceAllStringFunc(recorder.probe.scrub(string(encoded)), seriesNodeAlias)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.counts[kind]++
	_, _ = recorder.out.WriteString(line)
	_ = recorder.out.WriteByte('\n')
}

func (recorder *seriesRecorder) fail(what string, err error) {
	class := seriesErrorClass(err)
	recorder.mu.Lock()
	recorder.errors[what+": "+class]++
	recorder.mu.Unlock()
	recorder.emit("error", map[string]any{"what": what, "class": class})
	recorder.probe.logf("%s failed: %s", what, class)
}

// seriesErrorClass names the kind of failure without repeating its text.
func seriesErrorClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrInvalidEndpoint):
		return "endpoint refused by the client"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "401") || strings.Contains(text, "unauthorized"):
		return "unauthorized"
	case strings.Contains(text, "403") || strings.Contains(text, "forbidden"):
		return "forbidden"
	case strings.Contains(text, "404") || strings.Contains(text, "not found"):
		return "not found"
	case strings.Contains(text, "429") || strings.Contains(text, "too many requests"):
		return "throttled"
	case strings.Contains(text, "deadline") || strings.Contains(text, "timeout") || strings.Contains(text, "timed out"):
		return "timeout"
	case strings.Contains(text, "no such host"):
		return "name resolution"
	case strings.Contains(text, "connection refused") || strings.Contains(text, "connection reset") || strings.Contains(text, "unreachable"):
		return "network"
	case strings.Contains(text, "x509") || strings.Contains(text, "certificate"):
		return "certificate"
	case strings.Contains(text, "credential") || strings.Contains(text, "token") || strings.Contains(text, "sts"):
		return "credentials"
	case strings.Contains(text, "too large"):
		return "response too large"
	case strings.Contains(text, "503") || strings.Contains(text, "unavailable"):
		return "unavailable"
	}
	return "other"
}

func seriesTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}

func seriesIntOrString(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

func seriesConditions(conditions []seriesCondition) []map[string]any {
	out := make([]map[string]any, 0, len(conditions))
	for _, condition := range conditions {
		out = append(out, map[string]any{"type": condition.Type, "status": condition.Status, "reason": condition.Reason,
			"updated": seriesTime(condition.LastUpdateTime), "transition": seriesTime(condition.LastTransitionTime)})
	}
	return out
}

func seriesMilli(value string) any {
	number, ok := sampleQuantity(value)
	if !ok {
		return nil
	}
	return number * 1000
}

func seriesBytes(value string) any {
	number, ok := sampleQuantity(value)
	if !ok {
		return nil
	}
	return number
}

func seriesList[T any](recorder *seriesRecorder, name, apiPath string, limit int) ([]T, sampleListStat, bool) {
	raw, stat, _, err := recorder.probe.list(name, apiPath, limit)
	if err != nil {
		recorder.fail(name, err)
		return nil, stat, false
	}
	items, bad := sampleDecode[T](raw)
	if bad > 0 {
		recorder.probe.logf("%s: %d items did not decode", name, bad)
	}
	return items, stat, true
}

func seriesCallFields(stat sampleListStat) map[string]any {
	return map[string]any{"latency_ms": stat.LatencyMS, "bytes": stat.Bytes, "items": stat.Items, "pages": stat.Pages}
}

// ---------------------------------------------------------------- snapshots

func (recorder *seriesRecorder) pods() {
	pods, stat, ok := seriesList[seriesPod](recorder, "pods", "/api/v1/pods", 500)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(pods))
	for _, pod := range pods {
		owner, _ := pod.Metadata.controller()
		ready, since := pod.readySince()
		statuses := map[string]seriesContainerStatus{}
		for _, status := range pod.Status.ContainerStatuses {
			statuses[status.Name] = status
		}
		containers := make([]map[string]any, 0, len(pod.Spec.Containers))
		for _, container := range pod.Spec.Containers {
			status := statuses[container.Name]
			row := map[string]any{"n": container.Name,
				"lim_cpu_m": seriesMilli(container.Resources.Limits["cpu"]), "lim_mem": seriesBytes(container.Resources.Limits["memory"]),
				"req_cpu_m": seriesMilli(container.Resources.Requests["cpu"]), "req_mem": seriesBytes(container.Resources.Requests["memory"]),
				"restarts": status.RestartCount, "ready": status.Ready}
			if status.State.Waiting != nil {
				row["waiting"] = status.State.Waiting.Reason
			}
			if status.State.Running != nil {
				row["started"] = seriesTime(status.State.Running.StartedAt)
			}
			if status.State.Terminated != nil {
				row["terminated"] = status.State.Terminated.Reason
			}
			if last := status.LastState.Terminated; last != nil {
				row["last_term"] = last.Reason
				row["last_term_exit"] = last.ExitCode
				row["last_term_at"] = seriesTime(last.FinishedAt)
			}
			containers = append(containers, row)
		}
		sidecars := 0
		for _, container := range pod.Spec.InitContainers {
			if container.RestartPolicy == "Always" {
				sidecars++
			}
		}
		rows = append(rows, map[string]any{"ns": pod.Metadata.Namespace, "name": pod.Metadata.Name, "key": seriesKey(pod.Metadata.UID),
			"node": seriesNodeAlias(pod.Spec.NodeName), "phase": pod.Status.Phase, "reason": pod.Status.Reason,
			"created": pod.Metadata.CreationTimestamp.UTC().Format(time.RFC3339), "deleting": pod.Metadata.DeletionTimestamp != nil,
			"ready": ready, "ready_since": seriesTime(since), "owner_kind": owner.Kind, "owner": owner.Name, "version": pod.version(),
			"labels": pod.Metadata.Labels, "native_sidecars": sidecars, "c": containers})
	}
	fields := seriesCallFields(stat)
	fields["pods"] = rows
	recorder.emit("pods", fields)
	recorder.stateMu.Lock()
	recorder.lastPods = pods
	recorder.stateMu.Unlock()
}

func (recorder *seriesRecorder) workloads() {
	deployments, stat, ok := seriesList[seriesWorkload](recorder, "deployments", "/apis/apps/v1/deployments", 500)
	if ok {
		rows := make([]map[string]any, 0, len(deployments))
		for _, workload := range deployments {
			row := map[string]any{"ns": workload.Metadata.Namespace, "name": workload.Metadata.Name, "key": seriesKey(workload.Metadata.UID),
				"created":    workload.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
				"generation": workload.Metadata.Generation, "observed": workload.Status.ObservedGeneration,
				"revision": workload.Metadata.Annotations["deployment.kubernetes.io/revision"],
				"spec":     workload.Spec.Replicas, "paused": workload.Spec.Paused, "strategy": workload.Spec.Strategy.Type,
				"min_ready_s": workload.Spec.MinReadySeconds, "progress_deadline_s": workload.Spec.ProgressDeadlineSeconds,
				"replicas": workload.Status.Replicas, "updated": workload.Status.UpdatedReplicas, "ready": workload.Status.ReadyReplicas,
				"available": workload.Status.AvailableReplicas, "unavailable": workload.Status.UnavailableReplicas,
				"conditions": seriesConditions(workload.Status.Conditions)}
			if rolling := workload.Spec.Strategy.RollingUpdate; rolling != nil {
				row["max_surge"], row["max_unavailable"] = seriesIntOrString(rolling.MaxSurge), seriesIntOrString(rolling.MaxUnavailable)
			}
			// The metrics pipeline's own settings decide the sample window.
			if strings.Contains(workload.Metadata.Name, "metrics-server") {
				for _, container := range workload.Spec.Template.Spec.Containers {
					image := container.Image
					if at := strings.LastIndex(image, "/"); at >= 0 {
						image = image[at+1:]
					}
					row["metrics_server_image"], row["metrics_server_args"] = image, container.Args
				}
			}
			rows = append(rows, row)
		}
		fields := seriesCallFields(stat)
		fields["items_list"] = rows
		recorder.emit("deployments", fields)
	}
	statefulSets, stat, ok := seriesList[seriesWorkload](recorder, "statefulsets", "/apis/apps/v1/statefulsets", 500)
	if ok {
		rows := make([]map[string]any, 0, len(statefulSets))
		for _, workload := range statefulSets {
			row := map[string]any{"ns": workload.Metadata.Namespace, "name": workload.Metadata.Name, "key": seriesKey(workload.Metadata.UID),
				"generation": workload.Metadata.Generation, "observed": workload.Status.ObservedGeneration,
				"spec": workload.Spec.Replicas, "strategy": workload.Spec.UpdateStrategy.Type, "pod_management": workload.Spec.PodManagementPolicy,
				"replicas": workload.Status.Replicas, "updated": workload.Status.UpdatedReplicas, "ready": workload.Status.ReadyReplicas,
				"current_revision": workload.Status.CurrentRevision, "update_revision": workload.Status.UpdateRevision}
			if rolling := workload.Spec.UpdateStrategy.RollingUpdate; rolling != nil {
				row["partition"], row["max_unavailable"] = rolling.Partition, seriesIntOrString(rolling.MaxUnavailable)
			}
			rows = append(rows, row)
		}
		fields := seriesCallFields(stat)
		fields["items_list"] = rows
		recorder.emit("statefulsets", fields)
	}
	daemonSets, stat, ok := seriesList[seriesWorkload](recorder, "daemonsets", "/apis/apps/v1/daemonsets", 500)
	if ok {
		rows := make([]map[string]any, 0, len(daemonSets))
		for _, workload := range daemonSets {
			row := map[string]any{"ns": workload.Metadata.Namespace, "name": workload.Metadata.Name, "key": seriesKey(workload.Metadata.UID),
				"generation": workload.Metadata.Generation, "observed": workload.Status.ObservedGeneration,
				"strategy": workload.Spec.UpdateStrategy.Type, "desired": workload.Status.DesiredNumberScheduled,
				"ready": workload.Status.NumberReady, "updated": workload.Status.UpdatedNumberScheduled, "available": workload.Status.NumberAvailable}
			if rolling := workload.Spec.UpdateStrategy.RollingUpdate; rolling != nil {
				row["max_surge"], row["max_unavailable"] = seriesIntOrString(rolling.MaxSurge), seriesIntOrString(rolling.MaxUnavailable)
			}
			rows = append(rows, row)
		}
		fields := seriesCallFields(stat)
		fields["items_list"] = rows
		recorder.emit("daemonsets", fields)
	}
	autoscalers, stat, ok := seriesList[seriesAutoscaler](recorder, "horizontalpodautoscalers", "/apis/autoscaling/v2/horizontalpodautoscalers", 500)
	if ok {
		rows := make([]map[string]any, 0, len(autoscalers))
		for _, autoscaler := range autoscalers {
			metrics := []string{}
			for _, metric := range autoscaler.Spec.Metrics {
				name := metric.Type
				if metric.Resource != nil {
					name += ":" + metric.Resource.Name
				}
				metrics = append(metrics, name)
			}
			rows = append(rows, map[string]any{"ns": autoscaler.Metadata.Namespace, "name": autoscaler.Metadata.Name,
				"target_kind": autoscaler.Spec.ScaleTargetRef.Kind, "target": autoscaler.Spec.ScaleTargetRef.Name,
				"min": autoscaler.Spec.MinReplicas, "max": autoscaler.Spec.MaxReplicas, "metrics": metrics,
				"current": autoscaler.Status.CurrentReplicas, "desired": autoscaler.Status.DesiredReplicas,
				"last_scale": seriesTime(autoscaler.Status.LastScaleTime), "conditions": seriesConditions(autoscaler.Status.Conditions)})
		}
		fields := seriesCallFields(stat)
		fields["items_list"] = rows
		recorder.emit("autoscalers", fields)
	}
}

func (recorder *seriesRecorder) replicaSets() {
	replicaSets, stat, ok := seriesList[seriesWorkload](recorder, "replicasets", "/apis/apps/v1/replicasets", 500)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(replicaSets))
	for _, workload := range replicaSets {
		owner, _ := workload.Metadata.controller()
		rows = append(rows, map[string]any{"ns": workload.Metadata.Namespace, "name": workload.Metadata.Name, "key": seriesKey(workload.Metadata.UID),
			"created":    workload.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			"owner_kind": owner.Kind, "owner": owner.Name, "revision": workload.Metadata.Annotations["deployment.kubernetes.io/revision"],
			"spec": workload.Spec.Replicas, "replicas": workload.Status.Replicas, "ready": workload.Status.ReadyReplicas})
	}
	fields := seriesCallFields(stat)
	fields["items_list"] = rows
	recorder.emit("replicasets", fields)
}

// seriesNodeAlias keeps node identity stable across records and runs without
// keeping the node's address in the output.
func seriesNodeAlias(name string) string {
	if name == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	return "node-" + hex.EncodeToString(sum[:4])
}

// seriesKey is a short stable key for an object UID. The redactor masks
// UUIDs, so the UID itself would not survive as a join key.
func seriesKey(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return "k" + hex.EncodeToString(sum[:5])
}

func (recorder *seriesRecorder) nodes() {
	nodes, stat, ok := seriesList[seriesNode](recorder, "nodes", "/api/v1/nodes", 500)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		labels := node.Metadata.Labels
		taints := []string{}
		for _, taint := range node.Spec.Taints {
			taints = append(taints, taint.Key+":"+taint.Effect)
		}
		pool := labels["eks.amazonaws.com/nodegroup"]
		if pool == "" {
			pool = labels["karpenter.sh/nodepool"]
		}
		rows = append(rows, map[string]any{"alias": seriesNodeAlias(node.Metadata.Name),
			"created": node.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			"zone":    labels["topology.kubernetes.io/zone"], "instance_type": labels["node.kubernetes.io/instance-type"],
			"capacity_type": labels["eks.amazonaws.com/capacityType"] + labels["karpenter.sh/capacity-type"], "pool": pool,
			"arch": labels["kubernetes.io/arch"], "unschedulable": node.Spec.Unschedulable, "taints": taints,
			"alloc_cpu_m": seriesMilli(node.Status.Allocatable["cpu"]), "alloc_mem": seriesBytes(node.Status.Allocatable["memory"]),
			"alloc_pods": seriesBytes(node.Status.Allocatable["pods"]), "conditions": seriesConditions(node.Status.Conditions)})
	}
	fields := seriesCallFields(stat)
	fields["items_list"] = rows
	recorder.emit("nodes", fields)
}

func (recorder *seriesRecorder) nodeUsage() {
	usage, stat, ok := seriesList[seriesUsage](recorder, "metrics.k8s.io nodes", "/apis/metrics.k8s.io/v1beta1/nodes", 0)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(usage))
	for _, item := range usage {
		rows = append(rows, map[string]any{"alias": seriesNodeAlias(item.Metadata.Name), "ts": item.Timestamp.UTC().Format(time.RFC3339), "window": item.Window,
			"cpu_m": seriesMilli(item.Usage["cpu"]), "mem": seriesBytes(item.Usage["memory"])})
	}
	fields := seriesCallFields(stat)
	fields["items_list"] = rows
	recorder.emit("node_usage", fields)
}

// leases records who holds each leader-election lease, so workloads whose
// Pods differ on purpose (one leader, idle followers) can be told apart.
func (recorder *seriesRecorder) leases() {
	type lease struct {
		Metadata seriesMeta `json:"metadata"`
		Spec     struct {
			HolderIdentity string     `json:"holderIdentity"`
			RenewTime      *time.Time `json:"renewTime"`
		} `json:"spec"`
	}
	leases, stat, ok := seriesList[lease](recorder, "leases", "/apis/coordination.k8s.io/v1/leases", 500)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(leases))
	for _, item := range leases {
		// Node heartbeats are leases too; they say nothing about leaders.
		if item.Metadata.Namespace == "kube-node-lease" {
			continue
		}
		rows = append(rows, map[string]any{"ns": item.Metadata.Namespace, "name": item.Metadata.Name,
			"holder": item.Spec.HolderIdentity, "renewed": seriesTime(item.Spec.RenewTime)})
	}
	fields := seriesCallFields(stat)
	fields["items_list"] = rows
	recorder.emit("leases", fields)
}

// podUsage records one metrics.k8s.io Pod list, per container.
func (recorder *seriesRecorder) podUsage(limit int, kind string) {
	started := time.Now().UTC()
	usage, stat, ok := seriesList[seriesUsage](recorder, "metrics.k8s.io pods", "/apis/metrics.k8s.io/v1beta1/pods", limit)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(usage))
	for _, item := range usage {
		containers := make([]map[string]any, 0, len(item.Containers))
		for _, container := range item.Containers {
			containers = append(containers, map[string]any{"n": container.Name, "cpu_m": seriesMilli(container.Usage["cpu"]), "mem": seriesBytes(container.Usage["memory"])})
		}
		rows = append(rows, map[string]any{"ns": item.Metadata.Namespace, "name": item.Metadata.Name,
			"ts": item.Timestamp.UTC().Format(time.RFC3339), "window": item.Window, "c": containers})
	}
	fields := seriesCallFields(stat)
	fields["at"] = started.Format(time.RFC3339Nano)
	fields["limit"] = limit
	fields["items_list"] = rows
	recorder.emit(kind, fields)
}

// events records an event list. The message is kept, scrubbed and cut short,
// only the first time an event is seen.
func (recorder *seriesRecorder) events(kind, selector string) {
	apiPath := "/api/v1/events"
	if selector != "" {
		apiPath += "?fieldSelector=" + url.QueryEscape(selector)
	}
	events, stat, ok := seriesList[seriesEvent](recorder, "events "+kind, apiPath, 500)
	if !ok {
		return
	}
	rows := make([]map[string]any, 0, len(events))
	recorder.stateMu.Lock()
	for _, event := range events {
		count := event.Count
		if event.Series != nil && event.Series.Count > count {
			count = event.Series.Count
		}
		if count == 0 {
			count = 1
		}
		name := event.InvolvedObject.Name
		if event.InvolvedObject.Kind == "Node" {
			name = seriesNodeAlias(name)
		}
		row := map[string]any{"key": seriesKey(event.Metadata.UID), "type": event.Type, "kind": event.InvolvedObject.Kind,
			"ns": event.InvolvedObject.Namespace, "name": name, "reason": event.Reason, "count": count,
			"series": event.Series != nil, "first": seriesTime(event.FirstTimestamp), "last": seriesTime(event.LastTimestamp),
			"created": event.Metadata.CreationTimestamp.UTC().Format(time.RFC3339)}
		if !recorder.seenEvent[event.Metadata.UID] {
			recorder.seenEvent[event.Metadata.UID] = true
			message := event.Message
			if len(message) > 240 {
				message = message[:240]
			}
			row["message"] = message
		}
		rows = append(rows, row)
	}
	recorder.stateMu.Unlock()
	fields := seriesCallFields(stat)
	fields["items_list"] = rows
	recorder.emit(kind, fields)
}

// ---------------------------------------------------------------- logs

type seriesLogTarget struct {
	Namespace, Pod, Container, Group string
}

func seriesMainContainer(pod seriesPod) string {
	if preferred := pod.Metadata.Annotations["kubectl.kubernetes.io/default-container"]; preferred != "" {
		return preferred
	}
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Name
}

func (pod seriesPod) settled(now time.Time) bool {
	ready, _ := pod.readySince()
	return ready && pod.Metadata.DeletionTimestamp == nil && pod.Status.Phase == "Running" && now.Sub(pod.Metadata.CreationTimestamp) >= 5*time.Minute
}

// spreadLogTargets picks one settled Pod per workload, round-robin over the
// namespaces, so the sample is not one busy namespace.
func (recorder *seriesRecorder) spreadLogTargets(limit int) []seriesLogTarget {
	recorder.stateMu.Lock()
	pods := append([]seriesPod(nil), recorder.lastPods...)
	recorder.stateMu.Unlock()
	sort.Slice(pods, func(i, j int) bool { return pods[i].key() < pods[j].key() })
	now := time.Now()
	taken := map[string]bool{}
	byNamespace := map[string][]seriesLogTarget{}
	namespaces := []string{}
	for _, pod := range pods {
		owner, ok := pod.Metadata.controller()
		if !ok || owner.Kind == "Job" || !pod.settled(now) || seriesMainContainer(pod) == "" {
			continue
		}
		group := owner.Kind + "/" + pod.Metadata.Namespace + "/" + owner.Name
		if taken[group] {
			continue
		}
		taken[group] = true
		if _, seen := byNamespace[pod.Metadata.Namespace]; !seen {
			namespaces = append(namespaces, pod.Metadata.Namespace)
		}
		byNamespace[pod.Metadata.Namespace] = append(byNamespace[pod.Metadata.Namespace],
			seriesLogTarget{Namespace: pod.Metadata.Namespace, Pod: pod.Metadata.Name, Container: seriesMainContainer(pod), Group: group})
	}
	targets := []seriesLogTarget{}
	for round := 0; round < 20 && len(targets) < limit; round++ {
		for _, namespace := range namespaces {
			if round < len(byNamespace[namespace]) && len(targets) < limit {
				targets = append(targets, byNamespace[namespace][round])
			}
		}
	}
	return targets
}

// siblingLogTargets picks Pods of groups with at least four settled Pods of
// one version: every Pod of a ReplicaSet or StatefulSet group up to perGroup,
// and a smaller sample of each DaemonSet.
func (recorder *seriesRecorder) siblingLogTargets(perGroup, perDaemonSet, limit int) []seriesLogTarget {
	recorder.stateMu.Lock()
	pods := append([]seriesPod(nil), recorder.lastPods...)
	recorder.stateMu.Unlock()
	sort.Slice(pods, func(i, j int) bool { return pods[i].key() < pods[j].key() })
	now := time.Now()
	groups := map[string][]seriesPod{}
	kinds := map[string]string{}
	for _, pod := range pods {
		owner, ok := pod.Metadata.controller()
		if !ok || owner.Kind == "Job" || !pod.settled(now) || pod.version() == "" || seriesMainContainer(pod) == "" {
			continue
		}
		group := owner.Kind + "/" + pod.Metadata.Namespace + "/" + owner.Name + "@" + pod.version()
		groups[group] = append(groups[group], pod)
		kinds[group] = owner.Kind
	}
	names := []string{}
	for group, members := range groups {
		if len(members) >= 4 {
			names = append(names, group)
		}
	}
	// ReplicaSet and StatefulSet groups first: DaemonSets are the fallback.
	sort.Slice(names, func(i, j int) bool {
		left, right := kinds[names[i]] == "DaemonSet", kinds[names[j]] == "DaemonSet"
		if left != right {
			return !left
		}
		return names[i] < names[j]
	})
	targets := []seriesLogTarget{}
	for _, group := range names {
		take := perGroup
		if kinds[group] == "DaemonSet" {
			take = perDaemonSet
		}
		for index, pod := range groups[group] {
			if index >= take || len(targets) >= limit {
				break
			}
			targets = append(targets, seriesLogTarget{Namespace: pod.Metadata.Namespace, Pod: pod.Metadata.Name, Container: seriesMainContainer(pod), Group: group})
		}
	}
	return targets
}

// readLogCounts reads one bounded log slice and keeps only counts: lines,
// bytes, error lines and, with timestamps, the per-minute split. No log text
// is stored.
func (recorder *seriesRecorder) readLogCounts(ctx context.Context, kind string, targets []seriesLogTarget, sinceSeconds int, maxBytes int64) {
	probe := recorder.probe
	work := make(chan seriesLogTarget)
	var workers sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range work {
				query := url.Values{"container": {target.Container}, "sinceSeconds": {strconv.Itoa(sinceSeconds)},
					"limitBytes": {strconv.FormatInt(maxBytes, 10)}, "timestamps": {"true"}}
				apiPath := "/api/v1/namespaces/" + url.PathEscape(target.Namespace) + "/pods/" + url.PathEscape(target.Pod) + "/log?" + query.Encode()
				readAt := time.Now().UTC()
				data, truncated, latency, err := probe.get(ctx, "pods/log counts", apiPath, "*/*", maxBytes)
				call := sampleCall{Name: "pods/log sinceSeconds=" + strconv.Itoa(sinceSeconds), Page: 1, LatencyMS: float64(latency.Microseconds()) / 1000, Bytes: len(data)}
				row := map[string]any{"ns": target.Namespace, "pod": target.Pod, "container": target.Container, "group": target.Group,
					"since_s": sinceSeconds, "latency_ms": call.LatencyMS, "at": readAt.Format(time.RFC3339Nano)}
				if err != nil {
					call.Error = seriesErrorClass(err)
					row["error"] = call.Error
					probe.record(call)
					recorder.emit(kind, row)
					continue
				}
				probe.record(call)
				lines, bytes, errorLines, traceHeads, frames, recognized := 0, 0, 0, 0, 0, 0
				perMinuteLines := map[int]int{}
				perMinuteErrors := map[int]int{}
				for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
					if line == "" {
						continue
					}
					stamp, text, found := strings.Cut(line, " ")
					minute := -1
					if !found {
						text = line
					} else if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
						minute = int(readAt.Sub(at).Seconds()) / 60
					} else {
						text = line
					}
					lines++
					bytes += len(text) + 1
					level, _, _, exception := sampleDetectLevel(text)
					if level != "" {
						recognized++
					}
					// A stack trace is one error: its head counts, its indented frames do not.
					isFrame := exception && level == "" && (strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\t") || strings.HasPrefix(text, "Caused by: "))
					isError := level == "error" || level == "fatal"
					switch {
					case isFrame:
						frames++
					case isError:
						errorLines++
					case exception && level == "":
						traceHeads++
						isError = true
					}
					if minute >= 0 {
						perMinuteLines[minute]++
						if isError {
							perMinuteErrors[minute]++
						}
					}
				}
				row["lines"], row["bytes"], row["error_lines"], row["trace_heads"], row["stack_frames"], row["recognized"], row["truncated"] = lines, bytes, errorLines, traceHeads, frames, recognized, truncated || int64(len(data)) >= maxBytes
				row["lines_by_minute_ago"], row["errors_by_minute_ago"] = perMinuteLines, perMinuteErrors
				recorder.emit(kind, row)
			}
		}()
	}
	for _, target := range targets {
		work <- target
	}
	close(work)
	workers.Wait()
}

// ---------------------------------------------------------------- test

func TestLiveClusterSeriesRecord(t *testing.T) {
	connectorsPath := os.Getenv("KUBE_SAMPLE_CONNECTORS")
	if connectorsPath == "" {
		t.Skip("KUBE_SAMPLE_CONNECTORS is not set; this recorder runs only against an authorised real cluster")
	}
	dataDir, resultsDir := os.Getenv("HARNESS_DATA_DIR"), os.Getenv("HARNESS_RESULTS_DIR")
	if dataDir == "" || resultsDir == "" {
		t.Skip("HARNESS_DATA_DIR and HARNESS_RESULTS_DIR must name the output directories")
	}
	window := sampleEnvDuration("KUBE_SAMPLE_WINDOW", 10*time.Minute)
	usageInterval := sampleEnvDuration("KUBE_SAMPLE_USAGE_INTERVAL", 15*time.Second)
	snapshotInterval := sampleEnvDuration("KUBE_SAMPLE_INTERVAL", 60*time.Second)
	logPods := sampleEnvInt("KUBE_SAMPLE_LOG_PODS", 20)
	wanted := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("KUBE_SAMPLE_CLUSTERS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}

	absolute, err := filepath.Abs(connectorsPath)
	if err != nil {
		t.Fatalf("connectors path: %v", err)
	}
	// The config loader reads connectors.yaml beside config.yaml. The link is
	// removed once loaded so the run folder holds no copy of the endpoints.
	configDir, err := os.MkdirTemp(dataDir, "config-")
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	defer os.RemoveAll(configDir)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("# live-cluster series recorder: built-in defaults plus the sibling connectors.yaml\n"), 0o600); err != nil {
		t.Fatalf("config file: %v", err)
	}
	if err := os.Symlink(absolute, filepath.Join(configDir, "connectors.yaml")); err != nil {
		t.Fatalf("connectors link: %v", err)
	}
	if err := config.LoadConfig(filepath.Join(configDir, "config.yaml")); err != nil {
		t.Fatalf("load config (message withheld if it may carry values): %T", err)
	}
	_ = os.RemoveAll(configDir)
	connector := config.GetConfig().Connectors.Kubernetes
	redactor, redactorErrs := redaction.NewRedactor(false, nil)
	if len(redactorErrs) > 0 {
		t.Fatalf("redactor: %d rule errors", len(redactorErrs))
	}

	var statusMu sync.Mutex
	status := map[string]any{}
	t.Cleanup(func() {
		statusMu.Lock()
		defer statusMu.Unlock()
		encoded, _ := json.MarshalIndent(status, "", "  ")
		name := "live-cluster-series-status.json"
		if len(wanted) == 1 {
			// one recorder per cluster may share a results folder
			for id := range wanted {
				name = "live-cluster-series-status-" + id + ".json"
			}
		}
		_ = os.WriteFile(filepath.Join(resultsDir, name), []byte(redactor.Scrub(string(encoded))+"\n"), 0o600)
	})
	t.Run("clusters", func(t *testing.T) {
		for index, entry := range connector.Resolved() {
			cfg := entry.KubernetesToolConfig
			clusterID := strings.TrimSpace(cfg.ClusterID)
			if clusterID == "" {
				clusterID = "cluster-" + strconv.Itoa(index)
			}
			if len(wanted) > 0 && !wanted[clusterID] {
				continue
			}
			t.Run(clusterID, func(t *testing.T) {
				t.Parallel()
				if cfg.Auth.Mode != "eks" || cfg.Actions.Enable {
					t.Fatalf("recorder refuses anything but auth.mode eks with actions disabled")
				}
				probe := &sampleProbe{t: t, cluster: cfg.Auth.EKS.ClusterName, redactor: redactor, dataDir: filepath.Join(dataDir, clusterID)}
				if parsed, err := url.Parse(cfg.Endpoint); err == nil {
					probe.hide = []string{cfg.Endpoint, parsed.Host, parsed.Hostname()}
				}
				if err := os.MkdirAll(probe.dataDir, 0o700); err != nil {
					t.Fatalf("data dir: %v", err)
				}
				outcome := seriesRecordCluster(probe, cfg, clusterID, window, usageInterval, snapshotInterval, logPods)
				_ = os.WriteFile(filepath.Join(probe.dataDir, "recorder.log"), []byte(strings.Join(probe.logLines, "\n")+"\n"), 0o600)
				statusMu.Lock()
				status[clusterID] = outcome
				statusMu.Unlock()
				if outcome["reached"] != true {
					t.Fatalf("cluster %s not reached: %v at %v", clusterID, outcome["error_class"], outcome["stage"])
				}
			})
		}
	})
}

// seriesConnect builds the client through the product's own auth path, as
// pkg/agent.NewKubernetesService maps the connector config.
func seriesConnect(probe *sampleProbe, cfg config.KubernetesToolConfig, clusterID string) (stage string, cancel context.CancelFunc, err error) {
	timeout, _ := time.ParseDuration(cfg.Timeout)
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	auth := cfg.Auth
	clientConfig, err := ResolveAuthentication(AuthOptions{
		Mode: auth.Mode, Endpoint: cfg.Endpoint, CAFile: cfg.CAFile, CAData: cfg.CAData, ServerName: cfg.ServerName,
		EKSClusterName: auth.EKS.ClusterName, EKSRegion: auth.EKS.Region, EKSRoleARN: auth.EKS.RoleARN, EKSProfile: auth.EKS.Profile,
	})
	if err != nil {
		return "resolve authentication", nil, err
	}
	clientConfig.Timeout = timeout
	clientConfig.MaxBodyBytes = 32 << 20
	clientConfig.AllowLoopback = cfg.AllowLoopback
	clientConfig.AllowPrivateNetworks = cfg.AllowPrivateNetworks
	clientConfig.EndpointCIDRs = cfg.EndpointCIDRs
	client, err := NewClient(clientConfig)
	if err != nil {
		return "new client", nil, err
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: clusterID, CredentialID: cfg.CredentialID}, 0)
	probe.client, probe.service = client, service
	base, cancelBase := service.credentialContext(context.Background())
	probe.ctx = base
	if err := service.validateCredential(base); err != nil {
		cancelBase()
		return "credential validation", nil, err
	}
	return "", cancelBase, nil
}

func seriesRecordCluster(probe *sampleProbe, cfg config.KubernetesToolConfig, clusterID string, window, usageInterval, snapshotInterval time.Duration, logPods int) map[string]any {
	started := time.Now()
	outcome := map[string]any{"cluster_id": clusterID, "cluster_name": cfg.Auth.EKS.ClusterName, "region": cfg.Auth.EKS.Region, "reached": false}
	stage, cancel, err := seriesConnect(probe, cfg, clusterID)
	if err != nil {
		outcome["stage"], outcome["error_class"] = stage, seriesErrorClass(err)
		probe.logf("not reached at %s: %s", stage, seriesErrorClass(err))
		// The product folds every cause into "credential unavailable". One raw
		// request with the minted token says which cause it was: only the HTTP
		// status or the transport error class leaves this block.
		if probe.client != nil && probe.client.credentials != nil && probe.client.http != nil && probe.client.base != nil {
			plain, stop := context.WithTimeout(context.Background(), 20*time.Second) // testlint:allow the product's own request timeout, against a live cluster and identity provider, not a fixture
			header, tokenErr := probe.client.credentials.Authorization(plain)
			diagnosis := ""
			switch {
			case tokenErr != nil:
				diagnosis = "token not minted: " + seriesErrorClass(tokenErr)
			default:
				target := *probe.client.base
				target.Path = strings.TrimRight(target.Path, "/") + "/version"
				request, requestErr := http.NewRequestWithContext(plain, http.MethodGet, target.String(), nil)
				if requestErr != nil {
					diagnosis = "request not built"
					break
				}
				request.Header.Set("Authorization", header)
				response, sendErr := probe.client.http.Do(request)
				if sendErr != nil {
					var urlErr *url.Error
					class := seriesErrorClass(sendErr)
					if errors.As(sendErr, &urlErr) && urlErr.Timeout() {
						class = "timeout"
					}
					diagnosis = "token minted; API server not reached: " + class
					break
				}
				_ = response.Body.Close()
				diagnosis = "token minted; API server answered HTTP " + strconv.Itoa(response.StatusCode)
				// Is the signing identity itself still good? The token is a
				// presigned identity request: its answer says so. Only the
				// status and the provider's error code are kept; the request
				// and any error text stay in this block.
				if source, ok := probe.client.credentials.(*eksCredentialSource); ok && source.identity != nil && source.identity.client != nil {
					if target, valid := source.identity.presignedRequest(strings.TrimPrefix(header, "Bearer ")); valid {
						identity, buildErr := http.NewRequestWithContext(plain, http.MethodGet, target.String(), nil)
						if buildErr == nil {
							identity.Header.Set("x-k8s-aws-id", source.identity.clusterName)
							identity.Header.Set("Accept", "application/json")
							answer, sendErr := source.identity.client.Do(identity)
							switch {
							case sendErr != nil:
								diagnosis += "; identity provider not reached"
							default:
								raw, _ := io.ReadAll(io.LimitReader(answer.Body, 16<<10))
								_ = answer.Body.Close()
								diagnosis += "; identity provider answered HTTP " + strconv.Itoa(answer.StatusCode)
								if code := stsErrorCode(raw); code != "" && answer.StatusCode != http.StatusOK {
									diagnosis += " " + code
								}
								if answer.StatusCode == http.StatusOK {
									// The identity is good, so the cluster does not know it.
									// Only the kind of principal is kept: an access entry
									// names an IAM role or user, never a session.
									var caller struct {
										Response struct {
											Result struct {
												Arn string `json:"Arn"`
											} `json:"GetCallerIdentityResult"`
										} `json:"GetCallerIdentityResponse"`
									}
									if json.Unmarshal(raw, &caller) == nil {
										resource := caller.Response.Result.Arn
										if at := strings.LastIndex(resource, ":"); at >= 0 {
											resource = resource[at+1:]
										}
										kind, name, _ := strings.Cut(resource, "/")
										switch {
										case kind == "assumed-role" && strings.HasPrefix(name, "AWSReservedSSO_"):
											diagnosis += "; principal: session of a single sign-on role"
										case kind == "assumed-role":
											diagnosis += "; principal: session of an IAM role"
										case kind == "user":
											diagnosis += "; principal: IAM user"
										case kind != "":
											diagnosis += "; principal: " + kind
										}
									}
								}
							}
						}
					}
				}
			}
			stop()
			outcome["diagnosis"] = diagnosis
			probe.logf("diagnosis: %s", diagnosis)
		}
		return outcome
	}
	defer cancel()
	// One cheap read proves the cluster answers before the window starts.
	versionBody, _, _, err := probe.get(probe.ctx, "version", "/version", "application/json", 1<<20)
	if err != nil {
		outcome["stage"], outcome["error_class"] = "first read", seriesErrorClass(err)
		probe.logf("not reached at first read: %s", seriesErrorClass(err))
		return outcome
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	_ = json.Unmarshal(versionBody, &version)
	outcome["reached"] = true

	file, err := os.OpenFile(filepath.Join(probe.dataDir, "series.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		outcome["stage"], outcome["error_class"] = "open output", "local file"
		return outcome
	}
	defer file.Close()
	recorder := &seriesRecorder{probe: probe, file: file, out: bufio.NewWriterSize(file, 1<<20), counts: map[string]int{}, errors: map[string]int{}, started: started, seenEvent: map[string]bool{}}
	defer recorder.out.Flush()

	recorder.emit("header", map[string]any{"cluster_id": clusterID, "cluster_name": cfg.Auth.EKS.ClusterName, "region": cfg.Auth.EKS.Region,
		"server_version": version.GitVersion, "window_s": window.Seconds(), "usage_interval_s": usageInterval.Seconds(), "snapshot_interval_s": snapshotInterval.Seconds()})
	probe.logf("reached: server %s", version.GitVersion)

	// ---- before the window
	recorder.nodes()
	recorder.replicaSets()
	recorder.leases()
	recorder.events("events_all", "")
	// Does the metrics API honour a page size? One small-limit read answers it.
	recorder.podUsage(50, "pod_usage_limit_probe")
	recorder.pods()
	recorder.workloads()
	recorder.nodeUsage()
	recorder.events("events_warning", "type=Warning")
	spread := recorder.spreadLogTargets(logPods)
	recorder.readLogCounts(probe.ctx, "log_sample", spread, 60, 1<<20)

	// ---- the window: usage at the short interval, snapshots once a minute
	windowStart := time.Now()
	ctx, stop := context.WithTimeout(probe.ctx, window+90*time.Second) // testlint:allow the recording window itself, set by the caller, plus the time to finish the last reads
	defer stop()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		for index := 0; index <= int(window/usageInterval); index++ {
			sampleSleep(ctx, time.Until(windowStart.Add(time.Duration(index)*usageInterval)))
			if ctx.Err() != nil {
				return
			}
			recorder.podUsage(500, "pod_usage")
		}
	}()
	go func() {
		defer loops.Done()
		for index := 1; index <= int(window/snapshotInterval); index++ {
			sampleSleep(ctx, time.Until(windowStart.Add(time.Duration(index)*snapshotInterval)))
			if ctx.Err() != nil {
				return
			}
			recorder.pods()
			recorder.workloads()
			recorder.nodeUsage()
			recorder.events("events_warning", "type=Warning")
			if index%5 == 0 {
				recorder.replicaSets()
			}
			probe.logf("snapshot %d/%d", index, int(window/snapshotInterval))
		}
	}()
	loops.Wait()

	// ---- after the window
	recorder.nodes()
	recorder.events("events_all", "")
	recorder.readLogCounts(probe.ctx, "log_sample", spread, 60, 1<<20)
	siblings := recorder.siblingLogTargets(12, 6, 160)
	recorder.readLogCounts(probe.ctx, "log_siblings", siblings, int(window.Seconds()), 2<<20)
	probe.writeData("calls.json", probe.calls)

	recorder.mu.Lock()
	outcome["records"], outcome["read_errors"] = recorder.counts, recorder.errors
	recorder.mu.Unlock()
	outcome["seconds"] = time.Since(started).Seconds()
	outcome["log_sample_pods"], outcome["log_sibling_pods"] = len(spread), len(siblings)
	probe.logf("done in %.0fs: %s", time.Since(started).Seconds(), fmt.Sprint(outcome["records"]))
	return outcome
}
