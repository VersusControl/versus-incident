package kubernetes

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

var breakReasonPattern = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
var revisionPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

func safeBreakReason(value string) string {
	if breakReasonPattern.MatchString(value) {
		return value
	}
	return ""
}

func breakTimestamp(value any) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, stringValue(value))
	return parsed.UTC()
}

func projectBreakSummary(kind string, metadata, status, summary map[string]any) {
	if kind == "Node" {
		pressure := []string{}
		for _, condition := range mapSlice(status["conditions"]) {
			name := stringValue(condition["type"])
			if (name == "MemoryPressure" || name == "DiskPressure" || name == "PIDPressure") && stringValue(condition["status"]) == "True" {
				pressure = append(pressure, boundString(name))
			}
		}
		summary["node_pressure"] = strings.Join(pressure, ",")
	}
	for _, condition := range mapSlice(status["conditions"]) {
		var prefix string
		switch stringValue(condition["type"]) {
		case "Ready":
			prefix = "ready"
		case "Progressing":
			prefix = "progressing"
		case "Available":
			prefix = "available"
		case "PodScheduled":
			if kind != "Pod" {
				continue
			}
			prefix = "scheduled"
		case "DisruptionTarget":
			if kind != "Pod" {
				continue
			}
			prefix = "disruption"
		case "Failed":
			if kind != "Job" {
				continue
			}
			prefix = "job_failed"
		default:
			continue
		}
		summary[prefix+"_status"] = boundString(stringValue(condition["status"]))
		summary[prefix+"_reason"] = safeBreakReason(stringValue(condition["reason"]))
		if at := breakTimestamp(condition["lastTransitionTime"]); !at.IsZero() {
			summary[prefix+"_transition_at"] = at.Format(time.RFC3339Nano)
		}
	}
	for _, field := range []string{"updateRevision", "currentRevision"} {
		if value := stringValue(status[field]); safeSegment(value) && len(value) <= 253 {
			summary[camelToSnake(field)] = value
		}
	}
	if kind == "Deployment" && summary["available_status"] == "False" {
		replicas, replicasKnown := status["replicas"].(float64)
		unavailable, unavailableKnown := status["unavailableReplicas"].(float64)
		_, availablePresent := status["availableReplicas"]
		_, readyPresent := status["readyReplicas"]
		if !availablePresent && !readyPresent && replicasKnown && unavailableKnown && replicas > 0 && replicas <= 2147483647 && replicas == float64(int32(replicas)) && unavailable == replicas {
			summary["available_replicas"] = float64(0)
		}
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if value := stringValue(annotations["deployment.kubernetes.io/revision"]); revisionPattern.MatchString(value) {
		summary["revision"] = value
	}
	if kind != "Pod" {
		return
	}
	summary["pod_reason"] = podStatusReason(stringValue(status["reason"]))
	containers := projectCrashContainers(status)
	if len(mapSlice(status["containerStatuses"]))+len(mapSlice(status["initContainerStatuses"])) > 8 {
		summary["break_evidence_partial"] = true
	}
	sort.SliceStable(containers, func(left, right int) bool {
		return containerEvidenceRank(containers[left]) < containerEvidenceRank(containers[right])
	})
	if len(containers) > 8 {
		containers = containers[:8]
	}
	summary["container_states"] = containers
}

func projectLogContainerSummary(metadata, spec, status, summary map[string]any) {
	containers := projectCrashContainers(status)
	annotations, _ := metadata["annotations"].(map[string]any)
	states := make(map[string]kubeindex.ContainerState, len(containers))
	for _, container := range containers {
		states[container.Name] = container
	}
	logContainers := []kubeindex.ContainerState{}
	for _, group := range []struct {
		field string
		init  bool
	}{{"containers", false}, {"initContainers", true}} {
		for _, raw := range mapSlice(spec[group.field]) {
			name := stringValue(raw["name"])
			if !validPodLogName(name, 63, false) {
				continue
			}
			if len(logContainers) >= 64 {
				summary["log_container_states_partial"] = true
				break
			}
			state := states[name]
			state.Name, state.Init = name, group.init
			logContainers = append(logContainers, state)
			if !group.init && summary["main_container"] == nil {
				summary["main_container"] = name
			}
		}
	}
	if preferred := stringValue(annotations["kubectl.kubernetes.io/default-container"]); preferred != "" {
		for _, container := range logContainers {
			if !container.Init && container.Name == preferred {
				summary["main_container"] = preferred
			}
		}
	}
	summary["log_container_states"] = logContainers
}

func projectCrashContainers(status map[string]any) []kubeindex.ContainerState {
	containers := []kubeindex.ContainerState{}
	for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
		for _, container := range mapSlice(status[field]) {
			name := stringValue(container["name"])
			if !validPodLogName(name, 63, false) {
				continue
			}
			item := kubeindex.ContainerState{Name: name, Init: field == "initContainerStatuses"}
			if value, ok := container["restartCount"].(float64); ok && value >= 0 && value <= 2147483647 {
				item.RestartCount = int32(value)
			}
			state, _ := container["state"].(map[string]any)
			item.Ready, _ = container["ready"].(bool)
			_, item.Running = state["running"]
			running, _ := state["running"].(map[string]any)
			item.StartedAt = breakTimestamp(running["startedAt"])
			item.StateKnown = len(state) > 0
			waiting, _ := state["waiting"].(map[string]any)
			item.WaitingReason = safeBreakReason(stringValue(waiting["reason"]))
			last, _ := container["lastState"].(map[string]any)
			terminated, _ := last["terminated"].(map[string]any)
			if current, ok := state["terminated"].(map[string]any); ok {
				terminated = current
				item.CurrentTermination = true
			}
			item.TerminatedReason = safeBreakReason(stringValue(terminated["reason"]))
			if value, ok := terminated["exitCode"].(float64); ok && value >= -2147483648 && value <= 2147483647 {
				item.ExitCode = int32(value)
			}
			if value, ok := terminated["signal"].(float64); ok && value >= 1 && value <= 64 && value == float64(int32(value)) {
				item.Signal = int32(value)
			}
			item.TerminatedAt = breakTimestamp(terminated["finishedAt"])
			containers = append(containers, item)
		}
	}
	return containers
}

func containerEvidenceRank(item kubeindex.ContainerState) int {
	switch {
	case item.WaitingReason == "CrashLoopBackOff":
		return 0
	case kubebreaks.IsImagePullReason(item.WaitingReason), kubebreaks.IsConfigErrorReason(item.WaitingReason):
		return 1
	}
	if item.TerminatedReason != "" {
		return 2
	}
	return 3
}

func podStatusReason(value string) string {
	if strings.HasPrefix(value, "OutOf") && len(value) <= 253 && !strings.ContainsAny(value, " \t\r\n") {
		switch value {
		case "OutOfcpu", "OutOfmemory", "OutOfpods":
			return value
		default:
			return "OutOfResource"
		}
	}
	if reason := safeBreakReason(value); reason != "" {
		return reason
	}
	return ""
}

func indexedTimestamp(summary map[string]string, key string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, summary[key])
	return parsed
}

func indexedContainers(value any) []kubeindex.ContainerState {
	return indexedContainerLimit(value, 8)
}

func indexedLogContainers(value any) []kubeindex.ContainerState {
	return indexedContainerLimit(value, 64)
}

func indexedContainerLimit(value any, limit int) []kubeindex.ContainerState {
	if values, ok := value.([]kubeindex.ContainerState); ok {
		return append([]kubeindex.ContainerState(nil), values[:min(len(values), limit)]...)
	}
	var result []kubeindex.ContainerState
	for _, item := range mapSlice(value) {
		if len(result) == limit {
			break
		}
		init, _ := item["init"].(bool)
		current, _ := item["current_termination"].(bool)
		ready, _ := item["ready"].(bool)
		running, _ := item["running"].(bool)
		known, _ := item["state_known"].(bool)
		result = append(result, kubeindex.ContainerState{Name: stringValue(item["name"]), Init: init, Ready: ready, Running: running, StartedAt: breakTimestamp(item["started_at"]), StateKnown: known, CurrentTermination: current, WaitingReason: safeBreakReason(stringValue(item["waiting_reason"])), TerminatedReason: safeBreakReason(stringValue(item["last_terminated_reason"])), ExitCode: int32(triageSummaryIntValue(item, "last_terminated_exit_code")), Signal: int32(triageSummaryIntValue(item, "last_terminated_signal")), RestartCount: int32(triageSummaryIntValue(item, "restart_count")), TerminatedAt: breakTimestamp(item["last_terminated_at"])})
	}
	return result
}

func recordProjection(record kubeindex.Record) ProjectedResource {
	summary := make(map[string]any, len(record.Summary))
	for key, value := range record.Summary {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			summary[key] = parsed
		} else {
			summary[key] = value
		}
	}
	summary["container_states"] = record.Containers
	summary["log_container_states"] = record.LogContainers
	if record.MainContainer != "" {
		summary["main_container"] = record.MainContainer
	}
	if record.LogContainersPartial {
		summary["log_container_states_partial"] = true
	}
	if record.EventType != "" && record.Target != nil {
		summary["involved_object"] = ObjectRef{Kind: record.Target.Kind, Namespace: record.Target.Namespace, Name: record.Target.Name, UID: record.Target.UID}
	}
	result := ProjectedResource{APIVersion: record.APIVersion, Kind: record.Kind, Name: record.Name, Namespace: record.Namespace, UID: record.UID, Summary: summary, Labels: record.Labels}
	result.CreatedAt = record.CreatedAt
	for _, owner := range record.Owners {
		result.Owners = append(result.Owners, ObjectRef{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: owner.Namespace, Name: owner.Name, UID: owner.UID})
	}
	if record.Kind == "Node" {
		result.Conditions = []Condition{{Type: "Ready", Status: record.ReadyStatus, TransitionAt: record.ReadyTransitionAt}}
		for _, name := range strings.Split(record.Summary["node_pressure"], ",") {
			if name != "" {
				result.Conditions = append(result.Conditions, Condition{Type: name, Status: "True"})
			}
		}
	}
	return result
}
