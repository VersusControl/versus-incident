package breaks

import (
	"sort"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
)

type Termination struct {
	Reason   string    `json:"reason"`
	ExitCode int32     `json:"exit_code"`
	Count    int       `json:"count"`
	Last     time.Time `json:"last"`
}
type WaitingCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}
type Evidence struct {
	Terminations []Termination  `json:"terminations,omitempty"`
	Waiting      []WaitingCount `json:"waiting,omitempty"`
	Warnings     int            `json:"warning_events"`
	From         time.Time      `json:"from"`
	To           time.Time      `json:"to"`
}
type Break struct {
	Root      kref.Ref
	Rule      string
	Severity  string
	Count     int
	Of        int
	Examples  []kref.Ref
	BrokeAt   time.Time
	FirstSeen time.Time
	Source    string
	Evidence  Evidence
}

func Ref(record index.Record) kref.Ref {
	return kref.Ref{APIVersion: record.APIVersion, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, UID: record.UID}
}

func Root(record index.Record, byUID map[string]index.Record) kref.Ref {
	root := Ref(record)
	seen := map[string]bool{record.UID: true}
	for depth := 0; depth < 16 && len(record.Owners) > 0; depth++ {
		owner, controlled := controllingOwner(record.Owners)
		if !controlled {
			break
		}
		if owner.Namespace == "" {
			owner.Namespace = record.Namespace
		}
		if owner.Namespace != record.Namespace || owner.UID == "" || seen[owner.UID] {
			break
		}
		root = kref.Ref{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: owner.Namespace, Name: owner.Name, UID: owner.UID}
		seen[owner.UID] = true
		next, exists := byUID[owner.UID]
		if !exists || next.Kind != owner.Kind || next.Namespace != owner.Namespace {
			break
		}
		record = next
		if root.APIVersion == "" {
			root.APIVersion = next.APIVersion
		}
	}
	return root
}

func controllingOwner(owners []index.OwnerRef) (index.OwnerRef, bool) {
	for _, owner := range owners {
		if owner.ControllerKnown && owner.Controller {
			return owner, true
		}
	}
	for _, owner := range owners {
		if !owner.ControllerKnown {
			return owner, true
		}
	}
	return index.OwnerRef{}, false
}

func Rule(record index.Record) string {
	switch record.Kind {
	case "Pod":
		for _, container := range record.Containers {
			if container.WaitingReason == "CrashLoopBackOff" {
				return "pod.crash_loop"
			}
		}
		for _, container := range record.Containers {
			if container.WaitingReason == "ErrImagePull" || container.WaitingReason == "ImagePullBackOff" {
				return "pod.image_pull"
			}
		}
		if record.Phase == "Failed" {
			return "pod.failed"
		}
	case "Deployment", "StatefulSet", "DaemonSet":
		if record.ProgressingStatus == "False" && record.ProgressingReason == "ProgressDeadlineExceeded" {
			return "workload.rollout_stalled"
		}
		if record.Replicas > record.Available && availabilityKnown(record) {
			return "workload.unavailable"
		}
	case "Job":
		if count, _ := strconv.Atoi(record.Summary["failed"]); count > 0 {
			return "job.failed"
		}
	case "Node":
		if record.ReadyStatus != "True" {
			return "node.not_ready"
		}
	}
	return ""
}

func availabilityKnown(record index.Record) bool {
	for _, key := range []string{"available_replicas", "ready_replicas", "number_ready"} {
		if _, exists := record.Summary[key]; exists {
			return true
		}
	}
	return false
}

func Priority(rule string) int {
	for priority, candidate := range []string{"pod.crash_loop", "pod.image_pull", "pod.failed", "workload.rollout_stalled", "workload.unavailable", "job.failed", "node.not_ready"} {
		if rule == candidate {
			return priority
		}
	}
	return 99
}

func Classify(records []index.Record, observed time.Time) []Break {
	byUID := make(map[string]index.Record, len(records))
	for _, record := range records {
		byUID[record.UID] = record
	}
	groups := map[string]*Break{}
	pods := map[string]map[string]bool{}
	for _, record := range records {
		rule := Rule(record)
		if rule == "" {
			continue
		}
		root := Root(record, byUID)
		item := groups[root.UID]
		if item == nil {
			item = &Break{Root: root, Rule: rule, Severity: "warning", BrokeAt: observed, FirstSeen: observed, Source: "first_seen", Evidence: Evidence{From: observed, To: observed}}
			groups[root.UID] = item
			pods[root.UID] = map[string]bool{}
		}
		if Priority(rule) < Priority(item.Rule) {
			item.Rule = rule
		}
		rootRecord := byUID[root.UID]
		item.Of = int(rootRecord.Replicas)
		if availabilityKnown(rootRecord) && rootRecord.Replicas > 0 {
			unavailable := int(rootRecord.Replicas - rootRecord.Available)
			if unavailable > item.Count {
				item.Count = unavailable
			}
			if rootRecord.Available == 0 || unavailable*2 > int(rootRecord.Replicas) {
				item.Severity = "critical"
			}
		}
		if record.Kind == "Pod" {
			pods[root.UID][record.UID] = true
			if len(item.Examples) < 5 {
				item.Examples = append(item.Examples, Ref(record))
			}
		}
		if rule == "job.failed" || rule == "node.not_ready" || root.Kind == "Pod" {
			item.Severity = "critical"
		}
		at, source := failureTime(record, rule)
		if !at.IsZero() && !at.After(observed) && (item.Source == "first_seen" || at.Before(item.BrokeAt)) {
			item.BrokeAt, item.Source = at, source
		}
		addEvidence(&item.Evidence, record, observed)
	}
	result := []Break{}
	warnings := map[string]int{}
	for _, record := range records {
		if record.Kind != "Event" || record.EventType != "Warning" || record.Target == nil {
			continue
		}
		uid := record.Target.UID
		target := byUID[uid]
		if target, exists := byUID[uid]; exists {
			uid = Root(target, byUID).UID
		}
		if item := groups[uid]; item != nil && !record.EventLastAt.Before(item.BrokeAt.Add(-30*time.Minute)) && !record.EventLastAt.After(observed) {
			warnings[uid] += max(1, int(record.EventCount))
			if Rule(target) != "" && failureWarning(item.Rule, record.Summary["reason"]) {
				fence := target.ReadyTransitionAt
				if target.AvailableTransitionAt.After(fence) {
					fence = target.AvailableTransitionAt
				}
				at := record.EventFirstAt
				if !fence.IsZero() && !at.IsZero() && !at.Before(fence) && !at.After(observed) && !at.Before(observed.Add(-30*time.Minute)) && (item.Source == "first_seen" || at.Before(item.BrokeAt)) {
					item.BrokeAt, item.Source = at, "event"
				}
			}
		}
	}
	for uid, item := range groups {
		if len(pods[uid]) > item.Count {
			item.Count = len(pods[uid])
		}
		if item.Count == 0 {
			item.Count = 1
		}
		if item.Of > 0 && item.Count*2 > item.Of {
			item.Severity = "critical"
		}
		item.Evidence.Warnings = warnings[uid]
		if item.Evidence.From.After(item.BrokeAt) {
			item.Evidence.From = item.BrokeAt
		}
		sort.Slice(item.Examples, func(left, right int) bool { return item.Examples[left].Name < item.Examples[right].Name })
		result = append(result, *item)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Root.UID < result[right].Root.UID })
	return result
}

func failureTime(record index.Record, rule string) (time.Time, string) {
	var at time.Time
	var source string
	consider := func(next time.Time, kind string) {
		if !next.IsZero() && (at.IsZero() || next.Before(at)) {
			at, source = next, kind
		}
	}
	if record.Kind == "Pod" {
		if record.ReadyStatus == "False" {
			consider(record.ReadyTransitionAt, "pod_condition")
		}
		for _, container := range record.Containers {
			if container.TerminatedReason != "" && container.TerminatedReason != "Completed" && (container.ExitCode != 0 || container.TerminatedReason == "OOMKilled") && (record.ReadyTransitionAt.IsZero() || !container.TerminatedAt.Before(record.ReadyTransitionAt)) {
				consider(container.TerminatedAt, "container_state")
			}
		}
	}
	if rule == "workload.rollout_stalled" {
		consider(record.ProgressingTransitionAt, "workload_condition")
	}
	if rule == "workload.unavailable" && record.AvailableStatus == "False" {
		consider(record.AvailableTransitionAt, "workload_condition")
	}
	if rule == "node.not_ready" && record.ReadyStatus != "True" {
		consider(record.ReadyTransitionAt, "workload_condition")
	}
	return at, source
}

func failureWarning(rule, reason string) bool {
	switch rule {
	case "pod.crash_loop":
		return reason == "BackOff"
	case "pod.image_pull":
		return reason == "Failed" || reason == "ErrImagePull" || reason == "ImagePullBackOff"
	case "pod.failed", "workload.unavailable":
		return reason == "Failed" || reason == "Unhealthy" || reason == "FailedCreate"
	case "workload.rollout_stalled":
		return reason == "ProgressDeadlineExceeded"
	case "node.not_ready":
		return reason == "NodeNotReady" || reason == "NotReady"
	default:
		return false
	}
}

func addEvidence(evidence *Evidence, record index.Record, observed time.Time) {
	for _, container := range record.Containers {
		if reason := container.WaitingReason; reason != "" {
			found := false
			for position := range evidence.Waiting {
				if evidence.Waiting[position].Reason == reason {
					evidence.Waiting[position].Count++
					found = true
					break
				}
			}
			if !found && len(evidence.Waiting) < 16 {
				evidence.Waiting = append(evidence.Waiting, WaitingCount{Reason: reason, Count: 1})
			}
		}
		if container.TerminatedReason == "" || container.TerminatedReason == "Completed" || container.ExitCode == 0 && container.TerminatedReason != "OOMKilled" || container.TerminatedAt.IsZero() || container.TerminatedAt.After(observed) || container.TerminatedAt.Before(observed.Add(-24*time.Hour)) || !record.ReadyTransitionAt.IsZero() && container.TerminatedAt.Before(record.ReadyTransitionAt) {
			continue
		}
		found := false
		for position := range evidence.Terminations {
			item := &evidence.Terminations[position]
			if item.Reason == container.TerminatedReason && item.ExitCode == container.ExitCode {
				item.Count++
				if container.TerminatedAt.After(item.Last) {
					item.Last = container.TerminatedAt
				}
				found = true
				break
			}
		}
		if !found && len(evidence.Terminations) < 16 {
			evidence.Terminations = append(evidence.Terminations, Termination{Reason: container.TerminatedReason, ExitCode: container.ExitCode, Count: 1, Last: container.TerminatedAt})
		}
		if container.TerminatedAt.Before(evidence.From) {
			evidence.From = container.TerminatedAt
		}
	}
	sort.Slice(evidence.Waiting, func(left, right int) bool { return evidence.Waiting[left].Reason < evidence.Waiting[right].Reason })
	sort.Slice(evidence.Terminations, func(left, right int) bool {
		if evidence.Terminations[left].Reason == evidence.Terminations[right].Reason {
			return evidence.Terminations[left].ExitCode < evidence.Terminations[right].ExitCode
		}
		return evidence.Terminations[left].Reason < evidence.Terminations[right].Reason
	})
}
