package kubernetes

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubegraph "github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

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
