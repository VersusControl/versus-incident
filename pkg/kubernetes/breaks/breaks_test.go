package breaks

import (
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
)

func TestDisruptionTimePrecedesReadinessFailure(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	pod := index.Record{Kind: "Pod", UID: "pod", ReadyStatus: "False", ReadyTransitionAt: now.Add(-time.Minute), Summary: map[string]string{"disruption_status": "True", "disruption_reason": "EvictionByEvictionAPI"}}
	for _, at := range []time.Time{now.Add(-2 * time.Minute), {}, now.Add(time.Minute)} {
		pod.Summary["disruption_transition_at"] = at.Format(time.RFC3339Nano)
		item := Classify([]index.Record{pod}, now)[0]
		want := pod.ReadyTransitionAt
		if !at.IsZero() && !at.After(now) {
			want = at
		}
		if item.Rule != "pod.evicted" || !item.BrokeAt.Equal(want) {
			t.Fatalf("disruption %v: %+v", at, item)
		}
	}
}

func TestNodeCorrelationExcludesHealthySiblingPods(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	root := index.Record{Kind: "Deployment", UID: "root"}
	failing := index.Record{Kind: "Pod", UID: "failing", NodeName: "affected", Owners: []index.OwnerRef{{Kind: "Deployment", UID: root.UID}}, Containers: []index.ContainerState{{WaitingReason: "CrashLoopBackOff"}}}
	healthy := index.Record{Kind: "Pod", UID: "healthy", NodeName: "unrelated", Owners: failing.Owners, ReadyStatus: "True"}
	records := []index.Record{root, failing, healthy, {Kind: "Node", UID: "affected-node", Name: "affected"}, {Kind: "Node", UID: "unrelated-node", Name: "unrelated"}}
	history := changes.Page{Items: []changes.Change{{UID: "affected-node", Kind: "Node", At: now}, {UID: "unrelated-node", Kind: "Node", At: now}}}
	item := Break{Root: Ref(root), BrokeAt: now, Evidence: Evidence{To: now}}
	result := Correlate(item, records, graph.Graph{}, history, false)
	if len(result.Changes) != 1 || result.Changes[0].Change.UID != "affected-node" || result.Changes[0].Relation != "on_node" {
		t.Fatalf("node correlation: %+v", result)
	}
	history.Coverage = map[string]changes.Gap{}
	for _, kind := range []string{"Pod", "Deployment", "Event", "HorizontalPodAutoscaler"} {
		history.Coverage[kind] = changes.Gap{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	}
	result = Correlate(item, []index.Record{root, healthy}, graph.Graph{}, history, false)
	if result.History != "complete" {
		t.Fatalf("healthy sibling required node coverage: %+v", result)
	}
}

func TestAdmissionFailureRetainsActualReason(t *testing.T) {
	now := time.Now().UTC()
	for _, reason := range []string{"UnexpectedAdmissionError", "NodeAffinity", "OutOfResource", "Terminated"} {
		pod := index.Record{Kind: "Pod", UID: "pod", Phase: "Failed", Summary: map[string]string{"pod_reason": reason}}
		item := Classify([]index.Record{pod}, now)[0]
		if item.Rule != "pod.failed" || len(item.Evidence.PodReasons) != 1 || item.Evidence.PodReasons[0].Reason != reason {
			t.Fatalf("actual failure reason lost: %+v", item)
		}
	}
}

func TestCrashEvidenceRuleVocabulary(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct{ reason, rule string }{
		{"CrashLoopBackOff", "pod.crash_loop"}, {"ErrImagePull", "pod.image_pull"}, {"ImagePullBackOff", "pod.image_pull"}, {"ErrImageNeverPull", "pod.image_pull"}, {"InvalidImageName", "pod.image_pull"}, {"ImageInspectError", "pod.image_pull"}, {"RegistryUnavailable", "pod.image_pull"}, {"SignatureValidationFailed", "pod.image_pull"},
		{"CreateContainerConfigError", "pod.config_error"}, {"CreateContainerError", "pod.config_error"}, {"RunContainerError", "pod.config_error"}, {"PreCreateHookError", "pod.config_error"}, {"PreStartHookError", "pod.config_error"}, {"PostStartHookError", "pod.config_error"}, {"ContainerCreating", ""}, {"PodInitializing", ""},
	} {
		t.Run(test.reason, func(t *testing.T) {
			pod := index.Record{Kind: "Pod", UID: "pod", Containers: []index.ContainerState{{Name: "init", Init: true, WaitingReason: test.reason}}}
			if got := Rule(pod, now); got != test.rule {
				t.Fatalf("rule %q, want %q", got, test.rule)
			}
			if test.rule != "" {
				items := Classify([]index.Record{pod}, now)
				if len(items[0].Evidence.Waiting) != 1 || !items[0].Evidence.Waiting[0].Init {
					t.Fatal("init waiting lost")
				}
			}
		})
	}
}

func TestCrashEvidenceDistinctPodsAndLifetimeRestarts(t *testing.T) {
	now := time.Now().UTC()
	root := index.Record{Kind: "Deployment", UID: "root", Replicas: 3, Available: 2, Summary: map[string]string{"available_replicas": "2"}}
	container := index.ContainerState{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 6, TerminatedAt: now.Add(-time.Minute)}
	first := index.Record{Kind: "Pod", UID: "first", Owners: []index.OwnerRef{{Kind: "Deployment", UID: "root"}}, Containers: []index.ContainerState{container, container}}
	first.Containers[1].Name = "sidecar"
	second := index.Record{Kind: "Pod", UID: "second", Phase: "Running", Owners: first.Owners, Containers: []index.ContainerState{container}}
	second.Containers[0].WaitingReason = ""
	items := Classify([]index.Record{root, first, second}, now)
	if len(items) != 1 {
		t.Fatalf("items: %+v", items)
	}
	evidence := items[0].Evidence
	if items[0].Count != 1 || evidence.Terminations[0].Count != 2 || evidence.Terminations[0].Restarts != 18 || evidence.Restarts != 18 || evidence.Waiting[0].Count != 1 || evidence.Terminations[0].Signal != "" || evidence.Terminations[0].ExitHint != "SIGKILL" {
		t.Fatalf("distinct pod/lifetime totals: %+v", items[0])
	}
	first.Containers = first.Containers[:1]
	first.Containers[0].Init = true
	first.Containers[0].Signal = 9
	evidence = Classify([]index.Record{first}, now)[0].Evidence
	if !evidence.Terminations[0].Init || evidence.Terminations[0].Signal != "SIGKILL" || evidence.Terminations[0].ExitHint != "" {
		t.Fatalf("actual signal lost: %+v", evidence)
	}
}

func TestCrashEvidenceExitHintsAndFences(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		reason string
		code   int32
		hint   string
		keep   bool
	}{
		{"Error", 1, "", true}, {"Error", 137, "SIGKILL", true}, {"Error", 134, "SIGABRT", true}, {"Error", 139, "SIGSEGV", true}, {"Error", 143, "SIGTERM", true}, {"Error", 126, "not_executable", true}, {"Error", 127, "not_found", true}, {"ContainerCannotRun", 128, "", true}, {"StartError", 128, "", true}, {"ContainerStatusUnknown", 137, "SIGKILL", true}, {"Completed", 0, "", false}, {"OOMKilled", 0, "", true},
	} {
		t.Run(fmt.Sprintf("%s%d", test.reason, test.code), func(t *testing.T) {
			pod := index.Record{Kind: "Pod", UID: "pod", Containers: []index.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: test.reason, ExitCode: test.code, TerminatedAt: now.Add(-time.Minute)}}}
			items := Classify([]index.Record{pod}, now)
			terms := items[0].Evidence.Terminations
			if (len(terms) == 1) != test.keep {
				t.Fatalf("termination inclusion: %+v", terms)
			}
			if test.keep && (terms[0].Signal != "" || terms[0].ExitHint != test.hint) {
				t.Fatalf("invented signal: %+v", terms)
			}
			for _, at := range []time.Time{{}, now.Add(time.Minute), now.Add(-25 * time.Hour)} {
				pod.Containers[0].TerminatedAt = at
				item := Classify([]index.Record{pod}, now)[0]
				if len(item.Evidence.Terminations) != 0 || item.Source != "first_seen" {
					t.Fatalf("invalid time influenced break: %+v", item)
				}
			}
			pod.ReadyStatus, pod.ReadyTransitionAt = "False", now.Add(-time.Minute)
			pod.Containers[0].TerminatedAt = now.Add(-2 * time.Minute)
			if item := Classify([]index.Record{pod}, now)[0]; len(item.Evidence.Terminations) != 0 || !item.BrokeAt.Equal(pod.ReadyTransitionAt) {
				t.Fatal("prior episode leaked")
			}
		})
	}
}

func TestTerminalPodFenceAndStrictEviction(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet"} {
		t.Run(kind, func(t *testing.T) {
			controller := index.Record{Kind: kind, UID: "controller", Namespace: "shop", Replicas: 3, Available: 3, Summary: map[string]string{"available_replicas": "3"}, Owners: []index.OwnerRef{{Kind: "Deployment", UID: "missing", Namespace: "shop"}}}
			pod := index.Record{Kind: "Pod", UID: "pod", Namespace: "shop", Phase: "Failed", Summary: map[string]string{"pod_reason": "Evicted"}, Owners: []index.OwnerRef{{Kind: kind, UID: controller.UID}}}
			if items := Classify([]index.Record{controller, pod}, now); len(items) != 0 {
				t.Fatalf("healthy ancestor ignored: %+v", items)
			}
			controller.Available = 1
			if items := Classify([]index.Record{controller, pod}, now); len(items) == 0 || items[0].Evidence.PodReasons[0].Reason != "Evicted" {
				t.Fatal("terminal evidence lost under broken root")
			}
			controller.Available = 3
			controller.Summary = nil
			if len(Classify([]index.Record{controller, pod}, now)) == 0 {
				t.Fatal("unknown availability suppressed break")
			}
		})
	}
	for _, reason := range []string{"DeletionByPodGC", "TerminationByKubelet", "OrdinaryDeletion"} {
		pod := index.Record{Kind: "Pod", Summary: map[string]string{"disruption_status": "True", "disruption_reason": reason}}
		if Rule(pod, now) != "" {
			t.Fatalf("ordinary disruption became eviction: %s", reason)
		}
		pod.Phase = "Failed"
		if Rule(pod, now) != "pod.failed" {
			t.Fatal("terminal disruption must be generic failure")
		}
	}
	for _, reason := range []string{"Terminated", "UnexpectedAdmissionError", "NodeAffinity", "OutOfcpu", "OutOfResource"} {
		if Rule(index.Record{Kind: "Pod", Phase: "Failed", Summary: map[string]string{"pod_reason": reason}}, now) != "pod.failed" {
			t.Fatalf("admission/deletion became eviction: %s", reason)
		}
	}
	for _, reason := range []string{"PreemptionByScheduler", "DeletionByTaintManager", "EvictionByEvictionAPI"} {
		pod := index.Record{Kind: "Pod", UID: "pod", Summary: map[string]string{"disruption_status": "True", "disruption_reason": reason, "disruption_transition_at": now.Add(-time.Minute).Format(time.RFC3339Nano)}}
		item := Classify([]index.Record{pod}, now)[0]
		if item.Rule != "pod.evicted" || item.Source != "pod_condition" {
			t.Fatal("known eviction lost")
		}
	}
}

func TestSchedulingGraceAndJobFailureCondition(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		age           time.Duration
		reason        string
		known, broken bool
	}{
		{3 * time.Minute, "Unschedulable", true, true}, {2 * time.Minute, "Unschedulable", true, true}, {time.Minute, "Unschedulable", true, false}, {-time.Minute, "Unschedulable", true, false}, {3 * time.Minute, "SchedulingGated", true, false}, {3 * time.Minute, "Unschedulable", false, false},
	} {
		pod := index.Record{Kind: "Pod", UID: "pod", Phase: "Pending", ReadyStatus: "False", ReadyTransitionAt: now.Add(-time.Hour), Summary: map[string]string{"scheduled_status": "False", "scheduled_reason": test.reason}}
		if test.known {
			pod.Summary["scheduled_transition_at"] = now.Add(-test.age).Format(time.RFC3339Nano)
		}
		items := Classify([]index.Record{pod}, now)
		if (len(items) == 1) != test.broken {
			t.Fatalf("grace: %+v, %+v", test, items)
		}
		if test.broken && (items[0].Rule != "pod.unschedulable" || items[0].Source != "pod_condition") {
			t.Fatal("scheduling time lost")
		}
		if test.broken && !items[0].BrokeAt.Equal(now.Add(-test.age)) {
			t.Fatal("unrelated Ready transition displaced scheduling time")
		}
	}
	for _, reason := range []string{"BackoffLimitExceeded", "DeadlineExceeded", "PodFailurePolicy"} {
		job := index.Record{Kind: "Job", UID: "job", Summary: map[string]string{"job_failed_status": "True", "job_failed_reason": reason, "job_failed_transition_at": now.Add(-3 * time.Minute).Format(time.RFC3339Nano)}}
		item := Classify([]index.Record{job}, now)[0]
		if item.Rule != "job.failed" || item.Source != "workload_condition" || item.Evidence.Conditions[0].Reason != reason {
			t.Fatalf("job evidence: %+v", item)
		}
		delete(job.Summary, "job_failed_transition_at")
		if Classify([]index.Record{job}, now)[0].Source != "first_seen" {
			t.Fatal("invented job time")
		}
	}
}

func TestEvidenceBoundsWarningsAndNodeCounts(t *testing.T) {
	now := time.Now().UTC()
	root := index.Record{Kind: "Deployment", UID: "root", Replicas: 4, Summary: map[string]string{"available_replicas": "0"}}
	node := index.Record{Kind: "Node", UID: "node", Name: "node", ReadyStatus: "False", Summary: map[string]string{"node_pressure": "MemoryPressure,MemoryPressure,DiskPressure"}}
	records := []index.Record{root, node}
	for position := range 24 {
		pod := index.Record{Kind: "Pod", UID: fmt.Sprint(position), NodeName: "node", Owners: []index.OwnerRef{{Kind: "Deployment", UID: "root"}}, Summary: map[string]string{"pod_reason": fmt.Sprintf("Reason%c", 'A'+position)}, Containers: []index.ContainerState{{Name: "app", RestartCount: maxEvidenceCount, WaitingReason: fmt.Sprintf("Waiting%c", 'A'+position), TerminatedReason: fmt.Sprintf("Reason%c", 'A'+position), ExitCode: 1, TerminatedAt: now.Add(-time.Minute)}}}
		records = append(records, pod)
	}
	for position, reason := range []string{"BackOff", "Unhealthy", "ProbeWarning", "Failed", "FailedScheduling", "FailedMount", "FailedAttachVolume", "FailedCreate", "FailedCreatePodSandBox", "FailedKillPod", "FailedPostStartHook", "FailedPreStopHook", "Evicted", "Preempting", "OOMKilling", "SystemOOM", "MyOperatorSaidNo"} {
		records = append(records, index.Record{Kind: "Event", UID: fmt.Sprintf("event%d", position), EventType: "Warning", EventCount: maxEvidenceCount, EventLastAt: now, Target: &index.OwnerRef{UID: "root"}, Summary: map[string]string{"reason": reason}})
	}
	for _, item := range Classify(records, now) {
		if item.Root.UID != "root" {
			continue
		}
		evidence := item.Evidence
		if !evidence.Partial || len(evidence.Terminations) != 16 || len(evidence.Waiting) != 16 || len(evidence.PodReasons) != 16 || len(evidence.WarningReasons) > 16 || evidence.Restarts != maxEvidenceCount {
			t.Fatalf("unbounded evidence: %+v", evidence)
		}
		total := 0
		for _, bucket := range evidence.WarningReasons {
			total += bucket.Count
		}
		if total != evidence.Warnings || total != maxEvidenceCount {
			t.Fatal("warning sum mismatch")
		}
		for _, bucket := range evidence.Nodes {
			if bucket.Count != 24 {
				t.Fatalf("node count: %+v", bucket)
			}
		}
	}
	acc := &accumulator{}
	for _, reason := range []string{"Unhealthy", "FailedMount", "MyOperatorSaidNo"} {
		acc.warning(reason, 4)
	}
	if acc.warningTotal != 12 || acc.warnings["Other"] != 4 {
		t.Fatal("allowlist buckets wrong")
	}
	for _, reason := range []string{"BackOff", "ProbeWarning", "Failed", "FailedScheduling", "FailedAttachVolume", "FailedCreate", "FailedCreatePodSandBox", "FailedKillPod", "FailedPostStartHook", "FailedPreStopHook", "Evicted", "Preempting", "OOMKilling", "SystemOOM", "ErrImagePull", "ImagePullBackOff", "ErrImageNeverPull", "InspectFailed", "NodeNotReady", "NetworkNotReady"} {
		acc.warning(reason, 3)
	}
	total := 0
	for _, count := range acc.warnings {
		total += count
	}
	if len(acc.warnings) > 16 || !acc.partial || total != 72 || acc.warningTotal != total || acc.warnings["Other"] <= 4 {
		t.Fatal("warning overflow failed to fold into Other")
	}
	transient := index.Record{Containers: []index.ContainerState{{WaitingReason: "ContainerCreating"}, {WaitingReason: "PodInitializing"}}, Summary: map[string]string{"break_evidence_partial": "true"}}
	acc.addPod(transient, nil, now)
	if len(acc.waits) != 0 || !acc.partial {
		t.Fatal("transient noise or partial lost")
	}
}

func TestEvidenceReasonCountsRemainSeparateFromLifetimeRestarts(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	pod := index.Record{Kind: "Pod", UID: "pod", Containers: []index.ContainerState{
		{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 5, TerminatedAt: now},
		{Name: "sidecar", TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 7, TerminatedAt: now},
		{Name: "worker", TerminatedReason: "Error", ExitCode: 1, RestartCount: 11, TerminatedAt: now},
		{Name: "init", Init: true, TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 13, TerminatedAt: now},
		{Name: "clean", TerminatedReason: "Completed", RestartCount: 17, TerminatedAt: now},
	}}
	items := Classify([]index.Record{pod}, now)
	if len(items) != 1 || items[0].Count != 1 || items[0].Evidence.Restarts != 53 || len(items[0].Evidence.Terminations) != 3 {
		t.Fatalf("reason and lifetime totals: %+v", items)
	}
	for _, term := range items[0].Evidence.Terminations {
		want := 12
		if term.Init {
			want = 13
		} else if term.Reason == "Error" {
			want = 11
		}
		if term.Count != 1 || term.Restarts != want {
			t.Fatalf("reason bucket: %+v, want restarts %d", term, want)
		}
	}
}

func TestEvidenceActualSignalsOverrideExitInterpretations(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, signal, hint string
		code, actual       int32
	}{
		{"exit only", "", "SIGKILL", 137, 0},
		{"conflicting actual", "SIGTERM", "", 137, 15},
		{"numeric actual", "12", "", 139, 12},
		{"zero exit signalled", "SIGABRT", "", 0, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, init := range []bool{false, true} {
				pod := index.Record{Kind: "Pod", UID: "pod", Containers: []index.ContainerState{{Name: "app", Init: init, WaitingReason: "CrashLoopBackOff", TerminatedReason: "Error", ExitCode: test.code, Signal: test.actual, TerminatedAt: now}}}
				items := Classify([]index.Record{pod}, now)
				if len(items) != 1 || len(items[0].Evidence.Terminations) != 1 {
					t.Fatalf("missing termination: %+v", items)
				}
				term := items[0].Evidence.Terminations[0]
				if term.Signal != test.signal || term.ExitHint != test.hint || term.Init != init {
					t.Fatalf("signal interpretation: %+v", term)
				}
			}
		})
	}
}

func TestSuccessfulExitLoopRetainsWaitingWithoutInventingCrash(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"Completed", "Error", ""} {
		t.Run(reason, func(t *testing.T) {
			pod := index.Record{Kind: "Pod", UID: "pod", Phase: "Running", Containers: []index.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: reason, RestartCount: 9, TerminatedAt: now.Add(-time.Minute)}}}
			items := Classify([]index.Record{pod}, now)
			if len(items) != 1 || items[0].Rule != "pod.crash_loop" || len(items[0].Evidence.Waiting) != 1 || items[0].Evidence.Waiting[0].Count != 1 || items[0].Evidence.Restarts != 9 || len(items[0].Evidence.Terminations) != 0 {
				t.Fatalf("successful exit loop: %+v", items)
			}
			pod.Containers[0].WaitingReason = ""
			if items := Classify([]index.Record{pod}, now); len(items) != 0 {
				t.Fatalf("recovered loop retained break: %+v", items)
			}
		})
	}
}

func TestSchedulingGraceNanosecondAndInvalidTimeBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 123456789, time.UTC)
	for _, test := range []struct {
		name, at string
		broken   bool
	}{
		{"before grace", now.Add(-2*time.Minute + time.Nanosecond).Format(time.RFC3339Nano), false},
		{"at grace", now.Add(-2 * time.Minute).Format(time.RFC3339Nano), true},
		{"after grace", now.Add(-2*time.Minute - time.Nanosecond).Format(time.RFC3339Nano), true},
		{"empty", "", false}, {"malformed", "yesterday", false},
		{"zero", time.Time{}.Format(time.RFC3339Nano), false},
		{"future", now.Add(time.Nanosecond).Format(time.RFC3339Nano), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := index.Record{Kind: "Pod", UID: "pod", Phase: "Pending", ReadyStatus: "False", ReadyTransitionAt: now.Add(-time.Hour), Summary: map[string]string{"scheduled_status": "False", "scheduled_reason": "Unschedulable", "scheduled_transition_at": test.at}}
			items := Classify([]index.Record{pod}, now)
			if (len(items) == 1) != test.broken {
				t.Fatalf("grace classification: %+v", items)
			}
			if test.broken && (items[0].BrokeAt.Format(time.RFC3339Nano) != test.at || items[0].Source != "pod_condition") {
				t.Fatalf("scheduling timestamp: %+v", items[0])
			}
			pod.Phase = "Running"
			if Rule(pod, now) != "" {
				t.Fatal("non-Pending Pod became unschedulable")
			}
			pod.Phase, pod.Summary["scheduled_status"] = "Pending", "Unknown"
			if Rule(pod, now) != "" {
				t.Fatal("unknown scheduling status became unschedulable")
			}
		})
	}
}

func TestEvidenceExactBoundsAndOverflowPartial(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{8, 9} {
		acc := &accumulator{}
		pod := index.Record{UID: "pod", Containers: make([]index.ContainerState, count)}
		for position := range pod.Containers {
			pod.Containers[position] = index.ContainerState{Name: fmt.Sprint(position), RestartCount: 1}
		}
		acc.addPod(pod, nil, now)
		if acc.partial != (count > 8) || acc.restarts != min(count, 8) {
			t.Fatalf("container bound %d: %+v", count, acc)
		}
	}
	for _, count := range []int{16, 17} {
		acc := &accumulator{}
		for position := range count {
			acc.addPod(index.Record{UID: fmt.Sprint(position), Containers: []index.ContainerState{{WaitingReason: fmt.Sprintf("Reason%c", 'A'+position), TerminatedReason: fmt.Sprintf("Reason%c", 'A'+position), ExitCode: 1, TerminatedAt: now}}}, nil, now)
		}
		evidence := acc.evidence(now, now)
		if evidence.Partial != (count > 16) || len(evidence.Waiting) != min(count, 16) || len(evidence.Terminations) != min(count, 16) {
			t.Fatalf("bucket bound %d: %+v", count, evidence)
		}
	}
	for _, added := range []int32{0, 1, -1} {
		acc := &accumulator{}
		acc.addPod(index.Record{Containers: []index.ContainerState{{RestartCount: maxEvidenceCount}, {RestartCount: added}}}, nil, now)
		if acc.restarts != maxEvidenceCount || acc.partial != (added != 0) {
			t.Fatalf("restart saturation %d: %+v", added, acc)
		}
	}
	acc := &accumulator{}
	acc.warning("Failed", maxEvidenceCount-1)
	acc.warning("Unhealthy", 1)
	if acc.partial {
		t.Fatal("exact warning limit marked partial")
	}
	acc.warning("UnknownControllerReason", 1)
	total := 0
	for _, bucket := range acc.evidence(now, now).WarningReasons {
		total += bucket.Count
	}
	if !acc.partial || total != maxEvidenceCount || acc.warningTotal != total {
		t.Fatalf("warning saturation: %+v", acc)
	}
}

func TestRecoveryAndHealthyTerminalFenceOwnership(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	root := index.Record{Kind: "Deployment", UID: "root", Namespace: "shop", Replicas: 1, Summary: map[string]string{"available_replicas": "0"}}
	pod := index.Record{Kind: "Pod", UID: "pod", Namespace: "shop", Phase: "Running", Owners: []index.OwnerRef{{Kind: "Deployment", UID: "root", ControllerKnown: true, Controller: true}}, Containers: []index.ContainerState{{Name: "app", TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 6, TerminatedAt: now.Add(-time.Minute)}}}
	items := Classify([]index.Record{root, pod}, now)
	if len(items) != 1 || len(items[0].Evidence.Terminations) != 1 {
		t.Fatalf("broken root lost Running Pod evidence: %+v", items)
	}
	root.Available, root.Summary["available_replicas"] = 1, "1"
	if items := Classify([]index.Record{root, pod}, now); len(items) != 0 {
		t.Fatalf("healthy root retained historical break: %+v", items)
	}
	for _, reason := range []string{"Evicted", "Error"} {
		t.Run(reason, func(t *testing.T) {
			terminal := pod
			terminal.Phase, terminal.Summary = "Failed", map[string]string{"pod_reason": reason}
			if items := Classify([]index.Record{root, terminal}, now); len(items) != 0 {
				t.Fatalf("terminal leftover opened break: %+v", items)
			}
			current := pod
			current.UID = "current"
			current.Containers = []index.ContainerState{{Name: "app", WaitingReason: "CreateContainerConfigError"}}
			items := Classify([]index.Record{root, terminal, current}, now)
			if len(items) != 1 || items[0].Rule != "pod.config_error" || items[0].Count != 1 || len(items[0].Evidence.PodReasons) != 1 || items[0].Evidence.PodReasons[0].Reason != reason || len(items[0].Evidence.Terminations) != 1 {
				t.Fatalf("current failure lost terminal evidence: %+v", items)
			}
			for _, owner := range []index.OwnerRef{
				{Kind: "Deployment", UID: "root", Namespace: "other", ControllerKnown: true, Controller: true},
				{Kind: "StatefulSet", UID: "root", ControllerKnown: true, Controller: true},
				{Kind: "Deployment", UID: "root", ControllerKnown: true, Controller: false},
			} {
				terminal.Owners = []index.OwnerRef{owner}
				if items := Classify([]index.Record{root, terminal}, now); len(items) != 1 {
					t.Fatalf("invalid owner borrowed healthy fence: owner=%+v breaks=%+v", owner, items)
				}
			}
		})
	}
}

func TestBrokenOneItemPerRoot(t *testing.T) {
	now := time.Now().UTC()
	controller := index.Record{Kind: "Deployment", UID: "deployment", Name: "checkout", Namespace: "shop", Replicas: 7, Available: 0, Summary: map[string]string{"available_replicas": "0"}}
	replicaSet := index.Record{Kind: "ReplicaSet", UID: "rs", Name: "checkout-rs", Namespace: "shop", Owners: []index.OwnerRef{{Kind: "Deployment", UID: "deployment", Name: "checkout", Namespace: "shop"}}}
	records := []index.Record{controller, replicaSet}
	for position := range 7 {
		name := string(rune('a' + position))
		records = append(records, index.Record{Kind: "Pod", UID: name, Name: name, Namespace: "shop", Phase: "Running", ReadyStatus: "False", ReadyTransitionAt: now.Add(-4 * time.Minute), Owners: []index.OwnerRef{{Kind: "ReplicaSet", UID: "rs", Name: "checkout-rs", Namespace: "shop"}}, Containers: []index.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: "OOMKilled", ExitCode: 137, RestartCount: 6, TerminatedAt: now.Add(-3 * time.Minute)}}})
	}
	items := Classify(records, now)
	if len(items) != 1 || items[0].Root.Kind != "Deployment" || items[0].Rule != "pod.crash_loop" || items[0].Count != 7 || items[0].Of != 7 || len(items[0].Examples) != 5 || items[0].Severity != "critical" || items[0].Source != "pod_condition" || !items[0].BrokeAt.Equal(now.Add(-4*time.Minute)) || items[0].Evidence.Terminations[0].Count != 7 {
		t.Fatalf("break aggregation: %+v", items)
	}
	if items[0].Evidence.Terminations[0].Restarts != 42 || items[0].Evidence.Restarts != 42 {
		t.Fatal("lifetime restart totals lost")
	}
}

func TestBrokeAtOnlyCurrentFailingConditions(t *testing.T) {
	now := time.Now().UTC()
	pod := index.Record{Kind: "Pod", UID: "pod", Name: "pod", ReadyStatus: "True", ReadyTransitionAt: now.Add(-time.Hour), Containers: []index.ContainerState{{Name: "app", WaitingReason: "ImagePullBackOff"}}}
	items := Classify([]index.Record{pod}, now)
	if len(items) != 1 || items[0].Source != "first_seen" || !items[0].BrokeAt.Equal(now) {
		t.Fatalf("healthy timestamp or invented waiting start: %+v", items)
	}
	pod.ReadyStatus = "False"
	pod.ReadyTransitionAt = now.Add(-time.Minute)
	pod.Containers[0].TerminatedReason = "Error"
	pod.Containers[0].ExitCode = 1
	pod.Containers[0].TerminatedAt = now.Add(-time.Hour)
	items = Classify([]index.Record{pod}, now)
	if !items[0].BrokeAt.Equal(pod.ReadyTransitionAt) {
		t.Fatal("stale termination moved current episode")
	}
	for _, record := range []index.Record{{Kind: "Pod", UID: "pending", Name: "pending", Phase: "Pending"}, {Kind: "Event", UID: "event", Name: "event", EventType: "Warning"}, {Kind: "Node", UID: "node", Name: "node", ReadyStatus: "True", Summary: map[string]string{"node_pressure": "MemoryPressure"}}} {
		if len(Classify([]index.Record{record}, now)) != 0 {
			t.Fatalf("ordinary finding became a break: %+v", record)
		}
	}
}

func TestBrokenHalfUnavailableIsWarning(t *testing.T) {
	now := time.Now().UTC()
	items := Classify([]index.Record{{Kind: "Deployment", UID: "root", Name: "root", Replicas: 4, Available: 2, Summary: map[string]string{"available_replicas": "2"}}}, now)
	if len(items) != 1 || items[0].Severity != "warning" || items[0].Count != 2 {
		t.Fatalf("half unavailable: %+v", items)
	}
}

func TestRootPrefersControllingOwnerAndPreservesAPIVersion(t *testing.T) {
	controller := index.Record{UID: "deployment", Kind: "Deployment", APIVersion: "apps/v1", Namespace: "shop", Name: "api"}
	pod := index.Record{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "api-pod", Owners: []index.OwnerRef{{Kind: "ConfigMap", UID: "config", Name: "config", ControllerKnown: true}, {Kind: "Deployment", UID: "deployment", Name: "api", ControllerKnown: true, Controller: true}}}
	root := Root(pod, map[string]index.Record{"deployment": controller})
	if root.UID != "deployment" || root.APIVersion != "apps/v1" {
		t.Fatalf("wrong top controller: %+v", root)
	}
	pod.Owners = pod.Owners[:1]
	if Root(pod, nil).UID != "pod" {
		t.Fatal("non-controller owner became issue root")
	}
}

func TestWarningBreakTimeRequiresCurrentFailureEpisode(t *testing.T) {
	now := time.Now().UTC()
	pod := index.Record{UID: "pod", Kind: "Pod", Name: "api", ReadyStatus: "True", ReadyTransitionAt: now.Add(-10 * time.Minute), Containers: []index.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}}}
	event := index.Record{UID: "event", Kind: "Event", Name: "backoff", EventType: "Warning", EventFirstAt: now.Add(-5 * time.Minute), EventLastAt: now, EventCount: 1, Summary: map[string]string{"reason": "BackOff"}, Target: &index.OwnerRef{UID: "pod", Kind: "Pod", Name: "api"}}
	items := Classify([]index.Record{pod, event}, now)
	if len(items) != 1 || items[0].Source != "event" || !items[0].BrokeAt.Equal(event.EventFirstAt) {
		t.Fatalf("current failure event not used: %+v", items)
	}
	event.EventFirstAt = now.Add(-20 * time.Minute)
	items = Classify([]index.Record{pod, event}, now)
	if items[0].Source != "first_seen" {
		t.Fatal("prior readiness episode warning moved break time")
	}
}

func TestCorrelateRankingAndCoverage(t *testing.T) {
	now := time.Now().UTC()
	item := Break{Root: kref.Ref{Kind: "Deployment", UID: "root", Name: "api"}, BrokeAt: now.Add(-5 * time.Minute)}
	history := changes.Page{}
	if result := Correlate(item, nil, graph.Graph{}, history, false); result.History != "gap" || len(result.Gaps) == 0 {
		t.Fatal("absence of gaps was treated as coverage")
	}
	history.Coverage = map[string]changes.Gap{}
	for _, kind := range []string{"Pod", "Deployment", "Event", "HorizontalPodAutoscaler"} {
		history.Coverage[kind] = changes.Gap{From: now.Add(-time.Hour), To: now}
	}
	if result := Correlate(item, nil, graph.Graph{}, history, false); result.History != "complete" {
		t.Fatalf("affirmative coverage: %+v", result)
	}
	for position := range 5 {
		history.Items = append(history.Items, changes.Change{ID: string(rune('a' + position)), UID: "root", Type: changes.ImageChanged, At: item.BrokeAt.Add(-time.Duration(position+1) * time.Minute), Source: "reconstructed"})
	}
	history.Gaps = []changes.Gap{{From: now.Add(-10 * time.Minute), To: now}}
	result := Correlate(item, nil, graph.Graph{}, history, false)
	if result.History != "reconstructed" || len(result.Rebuilt) != 1 || len(result.Changes) != 3 || result.MoreChanges != 2 || result.Changes[0].LeadSec != 60 || result.Changes[2].LeadSec != 180 {
		t.Fatalf("rank/window: %+v", result)
	}
	if result := Correlate(item, nil, graph.Graph{}, changes.Page{Coverage: history.Coverage}, true); result.History == "complete" {
		t.Fatal("partial snapshot certified history")
	}
}

func TestCorrelateUntrackedReferencesAndRootLocalOmissions(t *testing.T) {
	now := time.Now().UTC()
	root := index.Record{UID: "root", Kind: "StatefulSet", Namespace: "shop", Name: "api", References: map[string][]string{"persistent_volume_claims": {"data"}, "service_accounts": {"runner"}}}
	item := Break{Root: Ref(root), BrokeAt: now.Add(-5 * time.Minute)}
	pod := index.Record{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "api-0", Owners: []index.OwnerRef{{UID: "root", Kind: "StatefulSet", Name: "api"}}, References: map[string][]string{"service_accounts": {"default"}}}
	records := []index.Record{root, pod, {UID: "pvc", Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data"}, {UID: "sa", Kind: "ServiceAccount", Namespace: "shop", Name: "runner"}}
	history := changes.Page{Coverage: map[string]changes.Gap{}}
	for _, kind := range []string{"Pod", "StatefulSet", "Event", "HorizontalPodAutoscaler", "ControllerRevision"} {
		history.Coverage[kind] = changes.Gap{From: now.Add(-time.Hour), To: now}
	}
	result := Correlate(item, records, graph.Graph{}, history, false)
	if result.History != "complete" || result.Partial || len(result.Untracked) != 2 || result.Untracked[0] != "PersistentVolumeClaim" || result.Untracked[1] != "ServiceAccount" {
		t.Fatalf("demand-only references invalidated tracked coverage: %+v", result)
	}
	records[0].References["config_maps"] = []string{"unavailable"}
	history.Coverage["ConfigMap"] = changes.Gap{From: now.Add(-time.Hour), To: now}
	result = Correlate(item, records, graph.Graph{}, history, false)
	if result.History == "complete" || !result.Partial || len(result.Gaps) == 0 {
		t.Fatalf("unavailable tracked config certified history: %+v", result)
	}
	delete(records[0].References, "config_maps")
	missingCoverage := history.Coverage["Pod"]
	delete(history.Coverage, "Pod")
	if result := Correlate(item, records, graph.Graph{}, history, false); result.History == "complete" || len(result.Gaps) == 0 {
		t.Fatalf("missing tracked coverage certified history: %+v", result)
	}
	history.Coverage["Pod"] = missingCoverage
	records[0].References = nil
	records = append(records, index.Record{UID: "bad", Kind: "ControllerRevision", Name: "bad", Namespace: "shop", Owners: []index.OwnerRef{{UID: "other", Kind: "StatefulSet", Name: "other"}}, Summary: map[string]string{"revision_history_partial": "true"}})
	result = Correlate(item, records, graph.Graph{}, history, false)
	if result.Partial || result.History != "complete" {
		t.Fatalf("unrelated omission tainted root: %+v", result)
	}
	records[len(records)-1].Owners[0].UID = "root"
	result = Correlate(item, records, graph.Graph{}, history, false)
	if !result.Partial || result.History == "complete" {
		t.Fatalf("affected root hid revision omission: %+v", result)
	}
}

func TestCorrelateLifecycleCannotDisplaceConfigAndScaling(t *testing.T) {
	now := time.Now().UTC()
	root := index.Record{UID: "root", Kind: "Deployment", Name: "api", References: map[string][]string{"config_maps": {"config"}}}
	pod := index.Record{UID: "pod", Kind: "Pod", Name: "pod", Owners: []index.OwnerRef{{UID: "root", Kind: "Deployment", Name: "api"}}}
	config := index.Record{UID: "cfg", Kind: "ConfigMap", Name: "config"}
	hpa := index.Record{UID: "hpa", Kind: "HorizontalPodAutoscaler", Name: "hpa"}
	item := Break{Root: Ref(root), BrokeAt: now}
	history := changes.Page{Items: []changes.Change{{ID: "cfg", UID: "cfg", Kind: "ConfigMap", Type: changes.ConfigChanged, At: now.Add(-5 * time.Minute)}, {ID: "hpa", UID: "hpa", Kind: "HorizontalPodAutoscaler", Type: changes.SpecChanged, At: now.Add(-6 * time.Minute)}}}
	for position := range 20 {
		history.Items = append(history.Items, changes.Change{ID: string(rune('a' + position)), UID: "pod", Kind: "Pod", Type: changes.Created, At: now.Add(-time.Second)})
	}
	edges := graph.Graph{Edges: []graph.Edge{{From: "hpa", To: "root", Type: graph.Scales}}}
	result := Correlate(item, []index.Record{root, pod, config, hpa}, edges, history, false)
	if len(result.Changes) != 2 || result.MoreChanges != 0 {
		t.Fatalf("lifecycle displaced meaningful changes: %+v", result)
	}
}
