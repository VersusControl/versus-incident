package kubernetes

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestPodDetailsProjectOnlyValidAddressAndQoS(t *testing.T) {
	service := NewService(nil, Scope{}, 0)
	for _, test := range []struct{ ip, qos, wantIP, wantQoS string }{
		{"10.0.0.1", "Burstable", "10.0.0.1", "Burstable"},
		{"2001:db8::1", "Guaranteed", "2001:db8::1", "Guaranteed"},
		{"not-an-address-secret", "secret", "", ""},
		{"fe80::1%eth0", "BestEffort", "", "BestEffort"},
	} {
		summary := service.projectResource(ResourceDefinition{Kind: "Pod"}, map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api"}, "status": map[string]any{"podIP": test.ip, "qosClass": test.qos}}).Summary
		if summaryString(summary, "pod_ip") != test.wantIP || summaryString(summary, "qos_class") != test.wantQoS {
			t.Fatalf("unsafe Pod details: %+v", summary)
		}
	}
}

func TestRunningTimestampAndFullLogContainerCatalog(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	service := NewService(nil, Scope{}, 0)
	containers := []any{}
	statuses := []any{}
	for i := 0; i < 12; i++ {
		name := "app-" + strconv.Itoa(i)
		containers = append(containers, map[string]any{"name": name})
		statuses = append(statuses, map[string]any{"name": name, "state": map[string]any{"running": map[string]any{"startedAt": started.Format(time.RFC3339Nano)}}})
	}

	raw := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "pod", "annotations": map[string]any{"kubectl.kubernetes.io/default-container": "app-10"}}, "spec": map[string]any{"containers": containers}, "status": map[string]any{"containerStatuses": statuses}}
	projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, raw)
	record := indexRecord(projected)
	if len(record.Containers) != 8 || len(record.LogContainers) != 12 || record.MainContainer != "app-10" || !record.LogContainers[10].StartedAt.Equal(started) {
		t.Fatalf("log catalog or uptime lost: %+v", record)
	}
	encoded, _ := json.Marshal(projected.Summary["log_container_states"])
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil || len(indexedLogContainers(decoded)) != 12 || !indexedLogContainers(decoded)[10].StartedAt.Equal(started) {
		t.Fatal("timestamp lost in JSON round trip")
	}
}

func TestTerminatedIncarnationStartsSurviveProjectionAndPinning(t *testing.T) {
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	end := start.Add(time.Second)
	for _, current := range []bool{false, true} {
		terminated := map[string]any{"startedAt": start.Format(time.RFC3339Nano), "finishedAt": end.Format(time.RFC3339Nano), "reason": "Error", "exitCode": float64(1)}
		state := map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}
		if current {
			state = map[string]any{"terminated": terminated}
		}
		items := projectCrashContainers(map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(8), "state": state, "lastState": map[string]any{"terminated": terminated}}}})
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		containers := indexedContainers(decoded)
		pods := verifiedBreakPods(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}, Records: []kubeindex.Record{{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: "pod", CreatedAt: start.Add(-time.Hour), Containers: containers}}})
		if len(pods) != 1 {
			t.Fatalf("missing pinned pod: %+v", pods)
		}
		for _, at := range []time.Time{start.Add(time.Millisecond), end.Add(500 * time.Millisecond)} {
			if _, _, ok := breaklogs.ResolveLinkedLine(pods, breaklogs.LinkedLine{At: at, Pod: "api-0", Container: "app"}); !ok {
				t.Fatalf("verified terminated incarnation rejected (current=%v) at %v: %+v", current, at, pods)
			}
		}
		for _, at := range []time.Time{start.Add(-time.Nanosecond), end.Add(time.Second)} {
			if _, _, ok := breaklogs.ResolveLinkedLine(pods, breaklogs.LinkedLine{At: at, Pod: "api-0", Container: "app"}); ok {
				t.Fatalf("unproven incarnation accepted (current=%v) at %v", current, at)
			}
		}
	}
}

func TestLinkedPinnedIncarnationFillsOnlyMatchingStatusBounds(t *testing.T) {
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	end := start.Add(time.Second)
	for _, later := range []bool{false, true} {
		pinned := []breaklogs.ManifestPod{{Namespace: "shop", Name: "api-0", UID: "pod", CreatedAt: start.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app", Restarts: 8, TerminationAt: end}}}}
		current := kubeindex.ContainerState{Name: "app", RestartCount: 8, PreviousStartedAt: start, TerminatedAt: end}
		if later {
			current.RestartCount++
			current.PreviousStartedAt = start.Add(time.Minute)
			current.TerminatedAt = end.Add(time.Minute)
		}
		pods := supplementLinkedPods(pinned, nil, []kubeindex.Record{{UID: "pod", Containers: []kubeindex.ContainerState{current}}}, end.Add(time.Minute))
		_, _, ok := breaklogs.ResolveLinkedLine(pods, breaklogs.LinkedLine{At: start.Add(time.Millisecond), Pod: "api-0", Container: "app"})
		if ok == later {
			t.Fatalf("pinned incarnation enriched from wrong restart or matching bound omitted: later=%v pods=%+v", later, pods)
		}
	}
}

func TestEventSourceProjectionSafeFallbacks(t *testing.T) {
	service := NewService(nil, Scope{}, 0)
	for _, test := range []struct {
		source                                   map[string]any
		component, host, wantComponent, wantHost string
	}{
		{map[string]any{"component": "kubelet", "host": "node-1"}, "fallback", "fallback", "kubelet", "node-1"},
		{nil, "deployment-controller", "controller-1", "deployment-controller", "controller-1"},
		{nil, "kubernetes.io/kubelet", "node-1", "kubernetes.io/kubelet", "node-1"},
		{map[string]any{"component": "bad\ncomponent", "host": "bad/host"}, "ignored", "ignored", "", ""},
	} {
		summary := service.projectResource(ResourceDefinition{Kind: "Event"}, map[string]any{"kind": "Event", "metadata": map[string]any{"name": "event"}, "source": test.source, "reportingController": test.component, "reportingInstance": test.host}).Summary
		if summaryString(summary, "source_component") != test.wantComponent || summaryString(summary, "source_host") != test.wantHost {
			t.Fatalf("unsafe event source: %+v", summary)
		}
	}
}

func TestProjectedCreatedAtSurvivesSummaryLoss(t *testing.T) {
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "pod", "creationTimestamp": created.Format(time.RFC3339Nano)}})
	projected.Summary = nil
	encoded, err := json.Marshal(projected)
	if err != nil || json.Unmarshal(encoded, &projected) != nil {
		t.Fatal("projection round trip failed", err)
	}
	record := indexRecord(projected)
	if !record.CreatedAt.Equal(created) || !recordProjection(record).CreatedAt.Equal(created) {
		t.Fatalf("typed creation provenance lost: %+v", record)
	}
}

func TestNativeDeploymentOmittedZeroAvailability(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	for _, test := range []struct {
		name   string
		status map[string]any
		broken bool
	}{
		{"native unavailable", map[string]any{"replicas": float64(1), "unavailableReplicas": float64(1), "conditions": []any{map[string]any{"type": "Available", "status": "False"}}}, true},
		{"absent status", nil, false},
		{"condition only", map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "False"}}}, false},
		{"counters only", map[string]any{"replicas": float64(1), "unavailableReplicas": float64(1)}, false},
		{"partial unavailable", map[string]any{"replicas": float64(2), "unavailableReplicas": float64(1), "conditions": []any{map[string]any{"type": "Available", "status": "False"}}}, false},
		{"inconsistent totals", map[string]any{"replicas": float64(1), "unavailableReplicas": float64(2), "conditions": []any{map[string]any{"type": "Available", "status": "False"}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "api", "uid": "root"}, "spec": map[string]any{"replicas": float64(1)}, "status": test.status}
			projected := service.projectResource(ResourceDefinition{Kind: "Deployment"}, raw)
			record := indexRecord(projected)
			for _, view := range []ProjectedResource{projected, recordProjection(record)} {
				findings := classifyIssue(view)
				if (len(findings) == 1) != test.broken || test.broken && findings[0].rule != "workload.unavailable" {
					t.Fatalf("ordinary findings: %+v summary=%+v", findings, view.Summary)
				}
			}
			items := kubebreaks.Classify([]kubeindex.Record{record}, time.Now().UTC())
			if (len(items) == 1) != test.broken {
				t.Fatalf("broken roots: %+v record=%+v", items, record)
			}
			if test.broken && (items[0].Rule != "workload.unavailable" || items[0].Count != 1 || items[0].Of != 1 || record.Summary["unavailable_replicas"] != "1") {
				t.Fatalf("native availability lost: %+v record=%+v", items, record)
			}
		})
	}
}

func TestNativeFailedJobPodCurrentTerminationBeforeReady(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 3, 0, 0, time.UTC)
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	for _, current := range []bool{false, true} {
		for _, phase := range []string{"Failed", "Running"} {
			for _, finished := range []time.Time{now.Add(-43 * time.Second), {}, now.Add(time.Second), now.Add(-25 * time.Hour)} {
				state := map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "finishedAt": finished.Format(time.RFC3339Nano)}}
				container := map[string]any{"name": "app", "state": map[string]any{}, "lastState": state}
				if current {
					container["state"], container["lastState"] = state, map[string]any{}
				}
				raw := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "job-pod", "uid": "pod", "ownerReferences": []any{map[string]any{"kind": "Job", "name": "job", "uid": "job", "controller": true}}}, "status": map[string]any{"phase": phase, "conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "PodFailed", "lastTransitionTime": now.Add(-42 * time.Second).Format(time.RFC3339Nano)}}, "containerStatuses": []any{container}}}
				projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, raw)
				encoded, _ := json.Marshal(projected)
				if err := json.Unmarshal(encoded, &projected); err != nil {
					t.Fatal(err)
				}
				record := indexRecord(projected)
				job := kubeindex.Record{Kind: "Job", UID: "job", Summary: map[string]string{"job_failed_status": "True", "job_failed_reason": "BackoffLimitExceeded"}}
				items := kubebreaks.Classify([]kubeindex.Record{job, record}, now)
				want := current && phase == "Failed" && finished.Equal(now.Add(-43*time.Second))
				if len(items) != 1 || (len(items[0].Evidence.Terminations) == 1) != want {
					t.Fatalf("current=%v phase=%s finished=%v: %+v", current, phase, finished, items)
				}
				if want && (items[0].Rule != "pod.failed" || items[0].Evidence.Terminations[0].ExitCode != 1 || !items[0].BrokeAt.Equal(finished)) {
					t.Fatalf("terminal exit lost: %+v", items[0])
				}
			}
		}
	}
}

func TestProjectPodBreakFieldsNoMessages(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	raw := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "pod"}, "status": map[string]any{
		"conditions":        []any{map[string]any{"type": "Ready", "status": "False", "lastTransitionTime": at.Format(time.RFC3339), "message": "CANARY_CONDITION"}},
		"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(2), "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "CANARY_WAIT"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": float64(137), "finishedAt": at.Format(time.RFC3339), "message": "CANARY_EXIT"}}}},
	}}
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, raw)
	record := indexRecord(projected)
	if len(record.Containers) != 1 || record.Containers[0].WaitingReason != "CrashLoopBackOff" || record.Containers[0].ExitCode != 137 || !record.ReadyTransitionAt.Equal(at) {
		t.Fatalf("evidence lost: %+v", record)
	}
	encoded, _ := json.Marshal([]any{projected, record})
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatal("status message escaped projection")
	}
	if safeBreakReason("Error output") != "" || safeBreakReason("OOMKilled") != "OOMKilled" {
		t.Fatal("reason allowlist")
	}
}

func TestProjectCrashEvidenceRoundTrip(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	summary := map[string]any{}
	conditions := []any{}
	for _, kind := range []string{"PodScheduled", "DisruptionTarget", "Failed", "Ready"} {
		conditions = append(conditions, map[string]any{"type": kind, "status": "True", "reason": "Evicted", "lastTransitionTime": at.Format(time.RFC3339), "message": "CANARY_CONDITION"})
	}
	projectBreakSummary("Pod", nil, map[string]any{"reason": "OutOfnvidia.com/gpu", "message": "CANARY_STATUS", "conditions": conditions,
		"initContainerStatuses": []any{map[string]any{"name": "init", "restartCount": float64(9), "state": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(137), "signal": float64(9), "finishedAt": at.Format(time.RFC3339), "message": "CANARY_EXIT"}}}},
	}, summary)
	encoded, _ := json.Marshal(summary)
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	record := indexRecord(ProjectedResource{Kind: "Pod", Name: "pod", UID: "pod", Summary: decoded})
	if record.Summary["pod_reason"] != "OutOfResource" || record.Summary["scheduled_reason"] != "Evicted" || record.Summary["disruption_transition_at"] != at.Format(time.RFC3339) || record.Summary["ready_reason"] != "Evicted" || record.Summary["job_failed_reason"] != "" || len(record.Containers) != 1 || !record.Containers[0].Init || record.Containers[0].Signal != 9 || record.Containers[0].RestartCount != 9 {
		t.Fatalf("round trip lost evidence: %+v", record)
	}
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatal("message leaked")
	}
	job := map[string]any{}
	projectBreakSummary("Job", nil, map[string]any{"conditions": conditions}, job)
	if indexed, _ := safeIndexedSummary(job); indexed["job_failed_reason"] != "Evicted" {
		t.Fatal("job condition lost")
	}
	if podStatusReason("OutOf secret value") != "" {
		t.Fatal("unsafe admission reason")
	}
}

func TestProjectCrashJournalAllStatuses(t *testing.T) {
	statuses := []any{}
	for position := range 25 {
		statuses = append(statuses, map[string]any{"name": "app-" + strconv.Itoa(position), "restartCount": float64(position), "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "CANARY_WAIT"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "message": "CANARY_EXIT"}}})
	}
	items := projectCrashContainers(map[string]any{"containerStatuses": statuses, "initContainerStatuses": statuses})
	if len(items) != 50 || items[24].Init || !items[25].Init || items[49].RestartCount != 24 {
		t.Fatalf("statuses lost: %+v", items)
	}
	encoded, err := json.Marshal(items)
	if err != nil || strings.Contains(string(encoded), "CANARY") {
		t.Fatalf("unsafe projection: %s %v", encoded, err)
	}
}

func TestProjectCrashEvidenceContainerBudget(t *testing.T) {
	statuses := []any{}
	for position := range 9 {
		statuses = append(statuses, map[string]any{"name": "app-" + strconv.Itoa(position), "state": map[string]any{"waiting": map[string]any{"reason": "ContainerCreating", "message": "CANARY_WAIT"}}})
	}
	summary := map[string]any{}
	projectBreakSummary("Pod", nil, map[string]any{"containerStatuses": statuses, "initContainerStatuses": []any{map[string]any{"name": "setup", "state": map[string]any{"waiting": map[string]any{"reason": "CreateContainerConfigError", "message": "CANARY_CONFIG"}}}}}, summary)
	record := indexRecord(ProjectedResource{Kind: "Pod", Name: "pod", UID: "pod", Summary: summary})
	if record.Summary["break_evidence_partial"] != "true" || len(record.Containers) != 8 || !record.Containers[0].Init || record.Containers[0].WaitingReason != "CreateContainerConfigError" {
		t.Fatalf("failure lost to cap: %+v", record)
	}
	for _, reason := range []string{"OutOfnvidia.com/gpu", "OutOfCustom"} {
		if podStatusReason(reason) != "OutOfResource" {
			t.Fatal("unbounded admission vocabulary")
		}
	}
}

func TestReferenceOmissionPreventsCompleteCorrelation(t *testing.T) {
	for _, count := range []int{33, 129} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			var sources []any
			for position := range count {
				sources = append(sources, map[string]any{"configMapRef": map[string]any{"name": "cfg-" + strconv.Itoa(1000+position)}})
			}
			service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
			projected := service.projectResource(ResourceDefinition{Kind: "Pod"}, map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "root", "namespace": "shop"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "envFrom": sources}}}})
			record := indexRecord(projected)
			if record.Summary["references_partial"] != "true" || len(record.References["config_maps"]) != 32 {
				t.Fatal("reference overflow silently disappeared")
			}
			now := time.Now().UTC()
			records := []kubeindex.Record{record}
			for _, name := range record.References["config_maps"] {
				records = append(records, kubeindex.Record{Kind: "ConfigMap", UID: name, Name: name, Namespace: "shop"})
			}
			history := kubechanges.Page{Coverage: map[string]kubechanges.Gap{}, Items: []kubechanges.Change{{UID: "omitted", Kind: "ConfigMap", Name: "cfg-" + strconv.Itoa(1000+count-1), Type: kubechanges.ConfigChanged, At: now.Add(-time.Minute)}}}
			for _, kind := range []string{"Pod", "Event", "ConfigMap", "HorizontalPodAutoscaler"} {
				history.Coverage[kind] = kubechanges.Gap{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
			}
			graph := kubegraph.Build(kubeindex.Snapshot{Records: records}, 500)
			result := kubebreaks.Correlate(kubebreaks.Break{Root: kubebreaks.Ref(record), BrokeAt: now}, records, graph, history, false)
			if result.History == "complete" || !result.Partial || graph.Omitted["references"] == 0 {
				t.Fatal("omitted reference certified complete no-change history")
			}
		})
	}
	projected := enforceProjectionSize(ProjectedResource{Kind: "Pod", Name: "api", Summary: map[string]any{"config_maps": []string{"cfg"}, "z-large": strings.Repeat("x", maxProjectionBytes)}})
	if indexRecord(projected).Summary["references_partial"] != "true" {
		t.Fatal("byte-limit summary omission lost linkage uncertainty")
	}
}

func TestReferenceChangesCompareBeyondDisplayPrefix(t *testing.T) {
	old := kubeindex.Record{Kind: "Pod", UID: "pod", Name: "api", References: map[string][]string{"config_maps": {}}}
	for position := range 10 {
		old.References["config_maps"] = append(old.References["config_maps"], "a"+strconv.Itoa(position)+strings.Repeat("x", 120))
	}
	updated := old
	updated.References = map[string][]string{"config_maps": append(append([]string(nil), old.References["config_maps"]...), "z-new")}
	items := kubechanges.DetectAll("cluster", kubeindex.Delta{Old: &old, New: &updated}, time.Now().UTC())
	for _, item := range items {
		if item.Type == kubechanges.ConfigRefChanged && item.FieldsTruncated && len(item.Fields[0].From) <= 1024 && len(item.Fields[0].To) <= 1024 {
			return
		}
	}
	t.Fatal("different bounded reference sets with identical display prefix compared equal")
}
