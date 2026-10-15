package breaks

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/graph"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
)

func TestBrokenOneItemPerRoot(t *testing.T) {
	now := time.Now().UTC()
	controller := index.Record{Kind: "Deployment", UID: "deployment", Name: "checkout", Namespace: "shop", Replicas: 7, Available: 0, Summary: map[string]string{"available_replicas": "0"}}
	replicaSet := index.Record{Kind: "ReplicaSet", UID: "rs", Name: "checkout-rs", Namespace: "shop", Owners: []index.OwnerRef{{Kind: "Deployment", UID: "deployment", Name: "checkout", Namespace: "shop"}}}
	records := []index.Record{controller, replicaSet}
	for position := range 7 {
		name := string(rune('a' + position))
		records = append(records, index.Record{Kind: "Pod", UID: name, Name: name, Namespace: "shop", Phase: "Running", ReadyStatus: "False", ReadyTransitionAt: now.Add(-4 * time.Minute), Owners: []index.OwnerRef{{Kind: "ReplicaSet", UID: "rs", Name: "checkout-rs", Namespace: "shop"}}, Containers: []index.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", TerminatedReason: "OOMKilled", ExitCode: 137, TerminatedAt: now.Add(-3 * time.Minute)}}})
	}
	items := Classify(records, now)
	if len(items) != 1 || items[0].Root.Kind != "Deployment" || items[0].Rule != "pod.crash_loop" || items[0].Count != 7 || items[0].Of != 7 || len(items[0].Examples) != 5 || items[0].Severity != "critical" || items[0].Source != "pod_condition" || !items[0].BrokeAt.Equal(now.Add(-4*time.Minute)) || items[0].Evidence.Terminations[0].Count != 7 {
		t.Fatalf("break aggregation: %+v", items)
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
