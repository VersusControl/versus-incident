package breaks

import (
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

const evidenceLimit = 16
const maxEvidenceCount = 2147483647

type terminationKey struct {
	reason       string
	exit, signal int32
	init         bool
}
type waitingKey struct {
	reason string
	init   bool
}
type accumulator struct {
	terms                  map[terminationKey]Termination
	waits                  map[waitingKey]WaitingCount
	podReasons             map[string]int
	nodes                  map[string]int
	warnings               map[string]int
	rootConditions         []ConditionReason
	restarts, warningTotal int
	partial                bool
	from                   time.Time
}

func (acc *accumulator) sum(current, added int) int {
	if added < 0 {
		acc.partial = true
		return current
	}
	if added > maxEvidenceCount-current {
		acc.partial = true
		return maxEvidenceCount
	}
	return current + added
}

func (acc *accumulator) reason(counts *map[string]int, reason string) {
	if reason == "" {
		return
	}
	if *counts == nil {
		*counts = map[string]int{}
	}
	if _, ok := (*counts)[reason]; !ok && len(*counts) >= evidenceLimit {
		acc.partial = true
		return
	}
	(*counts)[reason] = acc.sum((*counts)[reason], 1)
}

func countableTermination(container index.ContainerState, pod index.Record, observed time.Time) bool {
	return container.TerminatedReason != "" && container.TerminatedReason != "Completed" && (container.ExitCode != 0 || container.TerminatedReason == "OOMKilled" || container.Signal > 0) && !container.TerminatedAt.IsZero() && !container.TerminatedAt.After(observed) && !container.TerminatedAt.Before(observed.Add(-24*time.Hour)) && (pod.ReadyTransitionAt.IsZero() || !container.TerminatedAt.Before(pod.ReadyTransitionAt) || pod.Phase == "Failed" && container.CurrentTermination)
}

func (acc *accumulator) addPod(pod index.Record, nodes map[string]index.Record, observed time.Time) {
	acc.partial = acc.partial || pod.Summary["break_evidence_partial"] == "true"
	seenTerms := map[terminationKey]bool{}
	seenWaits := map[waitingKey]bool{}
	if acc.terms == nil {
		acc.terms = map[terminationKey]Termination{}
		acc.waits = map[waitingKey]WaitingCount{}
	}
	containers := pod.Containers
	if len(containers) > 8 {
		acc.partial = true
		containers = containers[:8]
	}
	for _, container := range containers {
		acc.restarts = acc.sum(acc.restarts, int(container.RestartCount))
		if reason := container.WaitingReason; reason != "" && reason != "ContainerCreating" && reason != "PodInitializing" {
			key := waitingKey{reason, container.Init}
			if !seenWaits[key] {
				seenWaits[key] = true
				item, ok := acc.waits[key]
				if !ok && len(acc.waits) >= evidenceLimit {
					acc.partial = true
				} else {
					item.Reason, item.Init, item.Count = reason, container.Init, acc.sum(item.Count, 1)
					acc.waits[key] = item
				}
			}
		}
		if !countableTermination(container, pod, observed) {
			continue
		}
		key := terminationKey{container.TerminatedReason, container.ExitCode, container.Signal, container.Init}
		item, ok := acc.terms[key]
		if !ok && len(acc.terms) >= evidenceLimit {
			acc.partial = true
			continue
		}
		item.Reason, item.ExitCode, item.Init = key.reason, key.exit, key.init
		item.Signal = signalName(key.signal)
		if key.signal == 0 {
			item.ExitHint = exitHint(key.exit)
		}
		if !seenTerms[key] {
			item.Count = acc.sum(item.Count, 1)
			seenTerms[key] = true
		}
		item.Restarts = acc.sum(item.Restarts, int(container.RestartCount))
		if container.TerminatedAt.After(item.Last) {
			item.Last = container.TerminatedAt
		}
		acc.terms[key] = item
		if acc.from.IsZero() || container.TerminatedAt.Before(acc.from) {
			acc.from = container.TerminatedAt
		}
	}
	acc.reason(&acc.podReasons, pod.Summary["pod_reason"])
	if pod.Summary["disruption_status"] == "True" && pod.Summary["disruption_reason"] != pod.Summary["pod_reason"] {
		acc.reason(&acc.podReasons, pod.Summary["disruption_reason"])
	}
	if pod.Phase == "Pending" && pod.Summary["scheduled_status"] == "False" && pod.Summary["scheduled_reason"] == "Unschedulable" && pod.Summary["pod_reason"] != "Unschedulable" {
		acc.reason(&acc.podReasons, "Unschedulable")
	}
	if node, ok := nodes[pod.NodeName]; ok {
		if node.ReadyStatus != "" && node.ReadyStatus != "True" {
			acc.reason(&acc.nodes, "NotReady")
		}
		for _, reason := range []string{"MemoryPressure", "DiskPressure", "PIDPressure"} {
			for _, pressure := range strings.Split(node.Summary["node_pressure"], ",") {
				if pressure == reason {
					acc.reason(&acc.nodes, reason)
					break
				}
			}
		}
	}
}

func (acc *accumulator) warning(reason string, count int) {
	if acc.warnings == nil {
		acc.warnings = map[string]int{}
	}
	reason = warningReason(reason)
	if _, exists := acc.warnings[reason]; !exists && reason != "Other" && len(acc.warnings) >= evidenceLimit-1 {
		acc.partial = true
		reason = "Other"
	}
	accepted := min(count, maxEvidenceCount-acc.warningTotal)
	if accepted < count {
		acc.partial = true
	}
	acc.warningTotal += accepted
	acc.warnings[reason] += accepted
}

func (acc *accumulator) conditions(root index.Record) {
	if root.Kind == "Job" && root.Summary["job_failed_status"] == "True" && root.Summary["job_failed_reason"] != "" {
		acc.rootConditions = append(acc.rootConditions, ConditionReason{"Failed", root.Summary["job_failed_reason"]})
	}
	if root.ProgressingStatus == "False" && root.ProgressingReason != "" {
		acc.rootConditions = append(acc.rootConditions, ConditionReason{"Progressing", root.ProgressingReason})
	}
	if root.Kind == "Node" && root.ReadyStatus != "True" && root.Summary["ready_reason"] != "" {
		acc.rootConditions = append(acc.rootConditions, ConditionReason{"Ready", root.Summary["ready_reason"]})
	}
}

func reasonCounts(counts map[string]int) []ReasonCount {
	var result []ReasonCount
	for reason, count := range counts {
		result = append(result, ReasonCount{reason, count})
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Count != result[right].Count {
			return result[left].Count > result[right].Count
		}
		return result[left].Reason < result[right].Reason
	})
	return result
}

func (acc *accumulator) evidence(from, to time.Time) Evidence {
	if !acc.from.IsZero() && acc.from.Before(from) {
		from = acc.from
	}
	result := Evidence{PodReasons: reasonCounts(acc.podReasons), Nodes: reasonCounts(acc.nodes), Conditions: acc.rootConditions, WarningReasons: reasonCounts(acc.warnings), Warnings: acc.warningTotal, Restarts: acc.restarts, Partial: acc.partial, From: from, To: to}
	for _, item := range acc.terms {
		result.Terminations = append(result.Terminations, item)
	}
	for _, item := range acc.waits {
		result.Waiting = append(result.Waiting, item)
	}
	sort.Slice(result.Terminations, func(left, right int) bool {
		first, second := result.Terminations[left], result.Terminations[right]
		if first.Count != second.Count {
			return first.Count > second.Count
		}
		if first.Reason != second.Reason {
			return first.Reason < second.Reason
		}
		if first.ExitCode != second.ExitCode {
			return first.ExitCode < second.ExitCode
		}
		if first.Init != second.Init {
			return !first.Init
		}
		return first.Signal < second.Signal
	})
	sort.Slice(result.Waiting, func(left, right int) bool {
		first, second := result.Waiting[left], result.Waiting[right]
		if first.Count != second.Count {
			return first.Count > second.Count
		}
		if first.Reason != second.Reason {
			return first.Reason < second.Reason
		}
		return !first.Init && second.Init
	})
	return result
}
