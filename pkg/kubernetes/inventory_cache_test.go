package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func seedInventory(t *testing.T, service *Service, records []kubeindex.Record) {
	t.Helper()
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC(), CompletedAt: time.Now().UTC()}
	}
	done := make(chan struct{})
	close(done)
	service.changes.inventories[service.indexScope()] = &inventoryCache{
		generation: inventoryGeneration{View: kubeindex.Snapshot{Records: records, Kinds: kinds}},
		done:       done, cancel: func() {},
	}
}

func TestCachedNamespaceSnapshotIgnoresRecordCounts(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, test := range []struct {
		name              string
		pods, deployments int
		empty             bool
	}{
		{name: "negative", pods: -1},
		{name: "minimum", pods: -maxInt - 1},
		{name: "maximum", pods: maxInt},
		{name: "overflow", pods: maxInt, deployments: 1},
		{name: "undercounted"},
		{name: "empty", pods: -1, empty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			input := []kubeindex.Record{
				{UID: "pod-a", Kind: "Pod", Namespace: "shop"},
				{UID: "node", Kind: "Node"},
				{UID: "deployment", Kind: "Deployment", Namespace: "shop"},
				{UID: "secret", Kind: "Secret", Namespace: "shop"},
				{UID: "pod-b", Kind: "Pod", Namespace: "shop"},
			}
			want := []kubeindex.Record{input[0], input[2], input[4]}
			if test.empty {
				input = input[1:2]
				want = []kubeindex.Record{}
			}
			seedInventory(t, service, input)
			cache := service.changes.inventories[service.indexScope()]
			now := time.Now().UTC()
			kinds := map[string]kubeindex.KindStatus{
				"Pod":        {State: "ready", Records: test.pods, ObservedAt: now},
				"Deployment": {State: "ready", Records: test.deployments, ObservedAt: now},
				"Secret":     {State: "partial", Error: "forbidden", Partial: true, ObservedAt: now},
			}
			pending := []string{indexedResourceIDs["ConfigMap"]}
			cache.generation.View.Kinds, cache.generation.Pending = kinds, pending
			beforeRecords := append([]kubeindex.Record(nil), input...)
			beforeKinds := make(map[string]kubeindex.KindStatus)
			for kind, state := range kinds {
				beforeKinds[kind] = state
			}

			snapshot, status, err := service.CachedNamespaceSnapshot(t.Context(), "", "Pod", "Deployment", "Secret", "ConfigMap")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Records == nil || !reflect.DeepEqual(snapshot.Records, want) {
				t.Fatalf("records = %+v, want %+v (non-nil)", snapshot.Records, want)
			}
			if status.State != "partial" || !status.Partial || !reflect.DeepEqual(status.Pending, pending) {
				t.Fatalf("status = %+v", status)
			}
			for kind, state := range beforeKinds {
				if snapshot.Kinds[kind] != state || status.Kinds[kind] != state {
					t.Fatalf("%s status was overwritten: snapshot=%+v status=%+v", kind, snapshot.Kinds[kind], status.Kinds[kind])
				}
			}
			if snapshot.Kinds["ConfigMap"] != (kubeindex.KindStatus{State: "pending"}) || status.Kinds["ConfigMap"] != snapshot.Kinds["ConfigMap"] {
				t.Fatalf("pending status = %+v", status.Kinds["ConfigMap"])
			}
			if !reflect.DeepEqual(kinds, beforeKinds) || !reflect.DeepEqual(input, beforeRecords) ||
				!reflect.DeepEqual(cache.generation.Pending, []string{indexedResourceIDs["ConfigMap"]}) {
				t.Fatal("snapshot read mutated published input")
			}
			snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "error"}
			status.Kinds["Deployment"] = kubeindex.KindStatus{State: "error"}
			if !reflect.DeepEqual(kinds, beforeKinds) {
				t.Fatal("returned kind maps alias published input")
			}
			if len(snapshot.Records) > 0 {
				snapshot.Records[0].UID = "changed"
				if !reflect.DeepEqual(input, beforeRecords) {
					t.Fatal("returned records alias published input")
				}
			}
		})
	}
}

func TestInventoryViewsNeverReadCluster(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		http.Error(w, "unexpected cluster read", http.StatusInternalServerError)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	seedInventory(t, service, []kubeindex.Record{
		{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "pod", Phase: "Running", Summary: map[string]string{"phase": "Running"}},
		{UID: "ns", Kind: "Namespace", Name: "shop", Phase: "Active", Summary: map[string]string{"phase": "Active"}},
		{UID: "node", Kind: "Node", Name: "node", NodeReady: true},
	})
	ctx := t.Context()
	overview, err := service.Overview(ctx)
	if err != nil || overview.Pods != 1 || overview.Namespaces != 1 || overview.Nodes != 1 || overview.Sync.State != "ready" {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	if _, err := service.Workloads(ctx, WorkloadListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Issues(ctx, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Graph(ctx, GraphQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.OverviewGraph(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CachedResources(ctx, ListOptions{ResourceID: indexedResourceIDs["Node"]}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Usage(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Changes(ctx, ChangeQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Related(ctx, NeighborhoodQuery{ResourceID: indexedResourceIDs["Pod"], Namespace: "shop", Name: "pod"}); err != nil {
		t.Fatal(err)
	}
	namespaces, err := service.Namespaces(ctx)
	if err != nil || len(namespaces.Items) != 1 || namespaces.Items[0] != "shop" {
		t.Fatalf("namespaces=%+v %v", namespaces, err)
	}
	if reads.Load() != 0 {
		t.Fatalf("views made %d cluster reads", reads.Load())
	}
}

func TestInventoryIssuesHistoryRemainsReadableUnderCaptureLease(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1", Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	seedInventory(t, service, []kubeindex.Record{{
		UID: "pod", Kind: "Pod", Namespace: "shop", Name: "pod",
		Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}},
	}})
	store := service.changeStore()
	now := time.Now().UTC()
	if err := store.Append([]kubechanges.Change{{ID: "image", Cluster: "cluster", Kind: "Pod", Namespace: "shop", Name: "pod", UID: "pod", Type: kubechanges.ImageChanged, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLease("background", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	page, err := service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true})
	if err != nil || len(page.Items) != 1 || len(page.Partial) != 0 || page.Sync.Partial {
		t.Fatalf("readable leased history reported unavailable: %+v %v", page, err)
	}
	if len(page.Items[0].Changes) != 1 || page.Items[0].Changes[0].Change.ID != "image" {
		t.Fatal("leased history omitted its stored change")
	}
	if page.Items[0].History != "gap" || len(page.Items[0].Gaps) == 0 {
		t.Fatal("missing history coverage was promoted to complete")
	}
	if err := store.Append(nil, now); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("foreground history read weakened lease fencing: %v", err)
	}
}

func TestInventoryPagesSixtyThousandRecords(t *testing.T) {
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		offset, _ := strconv.Atoi(r.URL.Query().Get("continue"))
		items := make([]any, 0, 500)
		for i := offset; i < offset+500 && i < 60000; i++ {
			name := "pod-" + strconv.Itoa(i)
			items = append(items, map[string]any{"metadata": map[string]any{"uid": name, "name": name, "namespace": "shop"}})
		}
		next := ""
		if offset+500 < 60000 {
			next = strconv.Itoa(offset + 500)
		}
		writeJSON(w, map[string]any{"items": items, "metadata": map[string]string{"continue": next}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	ctx := context.WithValue(t.Context(), discoveryContextKey{}, Discovery{Resources: []ResourceDefinition{{ID: indexedResourceIDs["Pod"], Kind: "Pod", Version: "v1", Resource: "pods", Namespaced: true, Available: true}}})
	records, complete, err := service.loadIndexKind(ctx, "Pod")
	if err != nil || !complete || len(records) != 60000 || pages.Load() != 120 {
		t.Fatalf("records=%d complete=%v pages=%d err=%v", len(records), complete, pages.Load(), err)
	}
	index := kubeindex.NewUnbounded()
	if err := index.Ingest("Pod", records, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := len(index.Snapshot().Records); got != 60000 {
		t.Fatalf("indexed %d", got)
	}
	if err := index.Refresh(t.Context(), "Pod", 0, time.Now().UTC(), func(context.Context) ([]kubeindex.Record, bool, error) {
		return records[:10], false, errors.New("transient failure")
	}); err == nil {
		t.Fatal("failed refresh succeeded")
	}
	if view := index.Snapshot(); len(view.Records) != 60000 || view.Kinds["Pod"].State != "error" {
		t.Fatalf("failed refresh discarded last-good: records=%d status=%+v", len(view.Records), view.Kinds["Pod"])
	}
}

func TestInventoryMetricsPagesEveryPodAndKeepsLastGood(t *testing.T) {
	var fail atomic.Bool
	now := time.Now().UTC().Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		items := []any{}
		next := ""
		if r.URL.Path == "/apis/metrics.k8s.io/v1beta1/pods" {
			offset, _ := strconv.Atoi(r.URL.Query().Get("continue"))
			for i := offset; i < offset+500 && i < 60000; i++ {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "pod-" + strconv.Itoa(i), "namespace": "shop"}, "timestamp": now, "window": "30s", "containers": []any{map[string]any{"name": "app", "usage": map[string]any{"cpu": "1m", "memory": "1Mi"}}}})
			}
			if offset+500 < 60000 {
				next = strconv.Itoa(offset + 500)
			}
		}
		writeJSON(w, map[string]any{"items": items, "metadata": map[string]string{"continue": next}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	usage := service.collectInventoryUsage(t.Context(), Usage{})
	if len(usage.Pods) != 60000 || usage.PodMetrics.Total != 60000 || !usage.PodMetrics.Complete || usage.Truncated {
		t.Fatalf("metrics pods=%d status=%+v partial=%+v", len(usage.Pods), usage.PodMetrics, usage.Partial)
	}
	fail.Store(true)
	next := service.collectInventoryUsage(t.Context(), usage)
	if len(next.Pods) != 60000 || next.PodMetrics.Complete || next.Fresh || len(next.Partial) == 0 {
		t.Fatalf("failed sample lost/stated healthy last-good: pods=%d status=%+v", len(next.Pods), next.PodMetrics)
	}
}

func TestInventoryScopeUpgradeAndNamespaceDenial(t *testing.T) {
	provider := storage.NewMemory()
	service := NewService(nil, Scope{OrgID: "licensed", ClusterID: "cluster", CredentialID: "epoch"}, 0)
	service.SetChangeStorage(provider)
	service.readOrgs = []string{"licensed", "default"}
	write := func(scope Scope, name string, denied bool) {
		t.Helper()
		state := kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC()}
		if denied {
			state.State, state.Error, state.Partial = "error", "forbidden", true
		}
		raw, err := json.Marshal(inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Namespace": state}, Records: []kubeindex.Record{{UID: name, Kind: "Namespace", Name: name}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := provider.WriteBlob(inventoryKey(scope), raw); err != nil {
			t.Fatal(err)
		}
	}
	archive := service.Scope()
	archive.OrgID = "default"
	write(archive, "archive", false)
	page, err := service.Namespaces(t.Context())
	if err != nil || len(page.Items) != 1 || page.Items[0] != "archive" {
		t.Fatalf("archive fallback=%+v %v", page, err)
	}
	write(service.Scope(), "current", false)
	page, err = service.Namespaces(t.Context())
	if err != nil || len(page.Items) != 1 || page.Items[0] != "current" {
		t.Fatalf("precedence=%+v %v", page, err)
	}
	write(service.Scope(), "must-not-leak", true)
	page, err = service.Namespaces(t.Context())
	if err != nil || len(page.Items) != 0 || !page.Sync.Partial {
		t.Fatalf("denial=%+v %v", page, err)
	}
	other := service.Scoped(Scope{OrgID: "foreign", ClusterID: "cluster", CredentialID: "epoch"})
	page, err = other.Namespaces(t.Context())
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("cross org=%+v %v", page, err)
	}
	rotated := NewService(nil, Scope{OrgID: "licensed", ClusterID: "cluster", CredentialID: "new-epoch"}, 0)
	rotated.SetChangeStorage(provider)
	page, err = rotated.Namespaces(t.Context())
	if err != nil || page.Sync.State != "syncing" || page.Sync.Partial || len(page.Items) != 0 {
		t.Fatalf("credential invalidation=%+v %v", page, err)
	}
}

func TestInventoryOverviewReportsNodeAllocatableFromBackgroundCache(t *testing.T) {
	labels := map[string]any{
		"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux",
		"node.kubernetes.io/instance-type": "m6i.4xlarge",
		"topology.kubernetes.io/region":    "us-east-1", "topology.kubernetes.io/zone": "us-east-1a",
		"eks.amazonaws.com/nodegroup":                   "production-general-purpose",
		"eks.amazonaws.com/capacityType":                "ON_DEMAND",
		"eks.amazonaws.com/nodegroup-image":             "ami-0123456789abcdef0",
		"eks.amazonaws.com/sourceLaunchTemplateId":      "lt-0123456789abcdef0",
		"eks.amazonaws.com/sourceLaunchTemplateVersion": "12",
	}
	for index := 0; index < 40; index++ {
		labels["scheduling.example.com/production-workload-pool-"+strconv.Itoa(index)] = "general-purpose-on-demand-amd64"
	}
	nodes, pods, nodeMetrics := []any{}, []any{}, []any{}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for index, allocatable := range []map[string]any{
		{"cpu": "7840m", "memory": "28Gi"},
		{"cpu": "15840m", "memory": "60Gi"},
		{"cpu": "15840m", "memory": "60Gi"},
	} {
		name := "ip-10-0-0-" + strconv.Itoa(index+1) + ".ec2.internal"
		metadata := map[string]any{"name": name, "uid": name + "-uid"}
		if index > 0 {
			metadata["labels"] = labels
		}
		node := map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": metadata, "status": map[string]any{
			"allocatable": allocatable,
			"capacity":    map[string]any{"cpu": "16", "memory": "64Gi"},
			"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
		}}
		if index > 0 {
			projected := (&Service{}).projectResource(ResourceDefinition{ID: "core~v1~nodes", Kind: "Node"}, node)
			if len(projected.Labels) > kubeindex.MaxLabels || kubeindex.RecordWithinBudget(indexRecord(projected)) {
				t.Fatal("wide EKS node must exercise the byte budget, not the label count bound")
			}
		}
		nodes = append(nodes, node)
		pods = append(pods, map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "api", "namespace": "namespace-" + strconv.Itoa(index)},
			"spec": map[string]any{"nodeName": name, "containers": []any{map[string]any{
				"name": "api", "resources": map[string]any{
					"requests": map[string]any{"cpu": "6", "memory": "32Gi"},
					"limits":   map[string]any{"cpu": "8", "memory": "40Gi"},
				},
			}}},
			"status": map[string]any{"phase": "Running"},
		})
		nodeMetrics = append(nodeMetrics, map[string]any{
			"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
			"metadata": map[string]any{"name": name}, "timestamp": timestamp, "window": "30s",
			"usage": map[string]any{"cpu": []string{"181m", "4", "5"}[index], "memory": []string{"20Gi", "40Gi", "60Gi"}[index]},
		})
	}
	var foreground atomic.Bool
	var foregroundReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if foreground.Load() {
			foregroundReads.Add(1)
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "metrics.k8s.io", "preferredVersion": map[string]any{"version": "v1beta1"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "nodes", "kind": "Node", "verbs": []string{"get", "list"}},
			}})
		case "/apis/metrics.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "nodes", "kind": "NodeMetrics", "verbs": []string{"get", "list"}}}})
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			writeJSON(writer, map[string]any{"items": nodeMetrics})
		case "/api/v1/nodes":
			writeJSON(writer, map[string]any{"items": nodes})
		case "/api/v1/pods":
			writeJSON(writer, map[string]any{"items": pods})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	warmTestInventory(t, service)
	foreground.Store(true)
	overview, err := service.Overview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Nodes != 3 || overview.ReadyNodes != 3 || overview.AllocatableCPU != "988/25" || overview.AllocatableMemory != strconv.FormatInt(148<<30, 10) {
		t.Fatalf("cached overview lost node allocatable: nodes=%d cpu=%q memory=%q", overview.Nodes, overview.AllocatableCPU, overview.AllocatableMemory)
	}
	if overview.Pods != 3 || overview.RequestedCPU != "18" || overview.LimitedCPU != "24" || overview.RequestedMemory != strconv.FormatInt(96<<30, 10) || overview.LimitedMemory != strconv.FormatInt(120<<30, 10) {
		t.Fatalf("pod reservations must cover all nodes and namespaces: %+v", overview)
	}
	if overview.UsageSource != "node_metrics" || overview.MetricsStatus != "available" || !overview.MetricsFresh || overview.UsageCPU != "9181/1000" || overview.UsageMemory != strconv.FormatInt(120<<30, 10) {
		t.Fatalf("usage must cover all nodes: %+v", overview)
	}
	if foregroundReads.Load() != 0 {
		t.Fatalf("overview read the cluster %d times instead of serving the cache", foregroundReads.Load())
	}
}

func TestInventoryOverviewExcludesTerminatedPodReservations(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	records := []kubeindex.Record{}
	for index, phase := range []string{"Running", "Pending", "Unknown", "", "Succeeded", "Failed"} {
		records = append(records, kubeindex.Record{
			UID: strconv.Itoa(index), Kind: "Pod", Namespace: "namespace-" + strconv.Itoa(index), Name: "pod", Phase: phase,
			Summary: map[string]string{
				"phase": phase, "requested_cpu": "1/4", "requested_memory": "1073741824",
				"limited_cpu": "1/2", "limited_memory": "2147483648",
			},
		})
	}
	seedInventory(t, service, records)
	overview, err := service.Overview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Pods != 6 || overview.RunningPods != 1 {
		t.Fatalf("terminal pods must still be counted: %+v", overview)
	}
	if overview.RequestedCPU != "1" || overview.LimitedCPU != "2" || overview.RequestedMemory != strconv.FormatInt(4<<30, 10) || overview.LimitedMemory != strconv.FormatInt(8<<30, 10) {
		t.Fatalf("reservations include terminated pods: %+v", overview)
	}
	if overview.AllocatableCPU != "" || overview.AllocatableMemory != "" || overview.UsageCPU != "" || overview.UsageMemory != "" {
		t.Fatalf("unknown capacity and metrics must not be synthesized: %+v", overview)
	}
}

func TestInventoryFirstSyncProgressIsMonotonicAndEndsAtPublication(t *testing.T) {
	release := make(chan struct{})
	var released atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/version":
			writeJSON(writer, map[string]any{"gitVersion": "v1.30.2"})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "nodes", "kind": "Node", "verbs": []string{"get", "list"}},
				map[string]any{"name": "namespaces", "kind": "Namespace", "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/nodes":
			writeJSON(writer, map[string]any{"items": []any{
				map[string]any{"metadata": map[string]any{"name": "node-a", "uid": "node-a"}},
				map[string]any{"metadata": map[string]any{"name": "node-b", "uid": "node-b"}},
			}})
		case "/api/v1/namespaces":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "shop", "uid": "shop"}}}})
		case "/api/v1/pods":
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
			writeJSON(writer, map[string]any{"items": []any{podFixture("api-1")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer func() {
		if !released.Load() {
			close(release)
		}
	}()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.client.indexLimiter = nil
	service.startInventory(t.Context())
	t.Cleanup(service.StopCapture)
	if version := service.clusterVersion(t.Context()); version != "v1.30.2" {
		t.Fatalf("cluster version=%q", version)
	}
	lastStep := 0
	deadline := time.Now().Add(20 * time.Second)
	for {
		overview, err := service.Overview(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		progress := overview.Sync.Progress
		if overview.Sync.State != "syncing" || progress == nil {
			t.Fatalf("cold overview reported %q with progress %+v before the first generation published", overview.Sync.State, progress)
		}
		if progress.StepIndex < lastStep || progress.StepTotal != 4 || len(progress.StepsDone) != progress.StepIndex-1 {
			t.Fatalf("progress moved backwards or is inconsistent: last=%d now=%+v", lastStep, progress)
		}
		lastStep = progress.StepIndex
		if progress.Counts["Node"] == 2 && progress.Counts["Namespace"] == 1 {
			if progress.Step != "listing" || progress.NodeCount == nil || *progress.NodeCount != 2 || progress.ClusterVersion != "v1.30.2" || progress.KindsDone >= progress.KindsTotal || progress.StartedAt.IsZero() {
				t.Fatalf("listing progress=%+v", progress)
			}
			if _, listed := progress.Counts["Pod"]; listed {
				t.Fatal("progress counted pods before their list completed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never reported node and namespace counts: %+v", progress)
		}
		time.Sleep(5 * time.Millisecond)
	}
	resources, err := service.Workloads(t.Context(), WorkloadListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resources.Sync.State == "syncing" && resources.Sync.Progress == nil {
		t.Fatal("workloads view lost first-sync progress")
	}
	released.Store(true)
	close(release)
	for {
		overview, err := service.Overview(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if overview.Sync.State != "syncing" {
			if overview.Sync.Progress != nil || overview.Pods != 1 || overview.Nodes != 2 {
				t.Fatalf("published overview=%+v", overview)
			}
			break
		}
		if progress := overview.Sync.Progress; progress == nil || progress.StepIndex < lastStep {
			t.Fatalf("syncing overview lost or rewound progress: %+v", progress)
		} else {
			lastStep = progress.StepIndex
		}
		if time.Now().After(deadline) {
			t.Fatal("first generation never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// coreFirstServer serves a small cluster whose non-core API group discovery
// blocks until release closes, standing in for a cluster whose many extra
// API groups take a long time to discover and list.
func coreFirstServer(t *testing.T, release <-chan struct{}, extraDiscovery *atomic.Int32, failExtras *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			if failExtras != nil && failExtras.Load() {
				http.Error(writer, "discovery down", http.StatusServiceUnavailable)
				return
			}
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "example.com", "preferredVersion": map[string]any{"version": "v1"}, "versions": []any{map[string]any{"version": "v1"}}}}})
		case "/apis/example.com/v1":
			extraDiscovery.Add(1)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "widgets", "kind": "Widget", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/example.com/v1/widgets":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": map[string]any{"name": "w", "namespace": "shop", "uid": "widget"}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("pods", "nodes", "namespaces", "persistentvolumeclaims")})
		case "/api/v1/pods":
			writeJSON(writer, map[string]any{"items": []any{podFixture("api-1")}})
		case "/api/v1/nodes":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "node-a", "uid": "node-a"}, "status": map[string]any{"allocatable": map[string]any{"cpu": "2", "memory": "4Gi"}}}}})
		case "/api/v1/namespaces":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "shop", "uid": "shop"}}}})
		case "/api/v1/persistentvolumeclaims":
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "settings", "namespace": "shop", "uid": "settings"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
}

func cachedGeneration(service *Service) inventoryGeneration {
	cache := service.inventoryCacheEntry()
	if cache == nil {
		return inventoryGeneration{}
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.generation
}

func TestInventoryPublishesCoreKindsBeforeSlowExtrasAndFillsThemInLater(t *testing.T) {
	release := make(chan struct{})
	var extraDiscovery atomic.Int32
	server := coreFirstServer(t, release, &extraDiscovery, nil)
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.client.indexLimiter = nil
	service.SetChangeStorage(provider)
	service.startInventory(t.Context())
	t.Cleanup(service.StopCapture)
	deadline := time.Now().Add(10 * time.Second)
	for generation := cachedGeneration(service); len(generation.View.Kinds) == 0 || generation.Usage.ObservedAt.IsZero(); generation = cachedGeneration(service) {
		if time.Now().After(deadline) {
			t.Fatal("core generation never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	core := cachedGeneration(service)
	for kind := range core.View.Kinds {
		if !inventoryCoreKinds[kind] {
			t.Fatalf("first generation listed non-core kind %s", kind)
		}
	}
	if !core.PendingDiscovery || !containsString(core.Pending, indexedResourceIDs["PersistentVolumeClaim"]) || len(core.ResourceStatus) != 0 {
		t.Fatalf("core generation does not mark the rest as pending: pending=%v discovery=%v resources=%v", core.Pending, core.PendingDiscovery, core.ResourceStatus)
	}
	stored, _, err := readInventoryGeneration(provider, service.scope, "")
	if err != nil || stored.Revision != core.Revision || len(stored.Pending) != len(core.Pending) || !stored.PendingDiscovery {
		t.Fatalf("core generation was not published through the fenced lease: revision=%q/%q pending=%v err=%v", stored.Revision, core.Revision, stored.Pending, err)
	}
	overview, err := service.Overview(t.Context())
	if err != nil || overview.Sync.State != "ready" || overview.Sync.Progress != nil || overview.Pods != 1 || overview.Nodes != 1 || overview.AllocatableCPU != "2" {
		t.Fatalf("core overview not ready: %+v err=%v", overview, err)
	}
	if overview.Sync.PendingKinds != len(core.Pending) || len(overview.Sync.Pending) != 0 {
		t.Fatalf("overview pending=%v kinds=%d", overview.Sync.Pending, overview.Sync.PendingKinds)
	}
	health, err := service.SyncHealth(t.Context())
	if err != nil || health.State != "syncing" || !health.FirstReadyAt.IsZero() || health.PendingKinds != len(core.Pending) || health.Progress == nil {
		t.Fatalf("core-only health lost live progress: %+v err=%v", health, err)
	}
	progress := health.Progress
	if progress.KindsTotal != len(indexedResourceIDs) || len(progress.RemainingKinds) != len(core.Pending) || !containsString(progress.StepsDone, "connected") || containsString(progress.StepsDone, "listing") {
		t.Fatalf("core-only progress invented completion: %+v", progress)
	}
	if progress.KindStatus["PersistentVolumeClaim"] != "pending" || len(progress.PhaseSeconds) != len(kubeindex.SyncSteps) {
		t.Fatalf("missing remaining kind status or bounded phase timing: %+v", progress)
	}
	claims, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["PersistentVolumeClaim"]})
	if err != nil || len(claims.Items) != 0 || claims.Sync.State != "partial" || !claims.Sync.Partial || !containsString(claims.Sync.Pending, indexedResourceIDs["PersistentVolumeClaim"]) {
		t.Fatalf("a not-yet-listed kind was presented as empty or complete: %+v err=%v", claims.Sync, err)
	}
	widgets, err := service.CachedResources(t.Context(), ListOptions{ResourceID: "example.com~v1~widgets"})
	if err != nil || widgets.Sync.State != "partial" || !containsString(widgets.Sync.Pending, "example.com~v1~widgets") {
		t.Fatalf("an undiscovered type was not reported as pending: %+v err=%v", widgets.Sync, err)
	}
	graph, err := service.OverviewGraph(t.Context())
	if err != nil || !graph.Sync.Partial || !containsString(graph.Sync.Pending, indexedResourceIDs["PersistentVolumeClaim"]) {
		t.Fatalf("graph hid pending kinds: %+v err=%v", graph.Sync, err)
	}
	complete, err := service.Graph(t.Context(), GraphQuery{Namespace: "shop", Complete: true})
	if err != nil || len(complete.Nodes) != 0 || complete.Sync.State != "partial" || len(complete.Sync.Pending) == 0 {
		t.Fatalf("complete graph claimed completeness while kinds load: %+v err=%v", complete.Sync, err)
	}
	for extraDiscovery.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the extras cycle never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if latest := cachedGeneration(service); latest.Revision != core.Revision {
		t.Fatal("a generation published while extras were still loading")
	}
	close(release)
	for {
		full := cachedGeneration(service)
		if !full.PendingDiscovery && len(full.Pending) == 0 && len(full.Resources["example.com~v1~widgets"]) == 1 {
			break
		}
		if time.Now().After(deadline) {
			keys := []string{}
			for id := range full.Resources {
				keys = append(keys, id)
			}
			t.Fatalf("extras never filled in: pending=%v discovery=%v resources=%v partial=%+v", full.Pending, full.PendingDiscovery, keys, full.DiscoveryPartial)
		}
		time.Sleep(5 * time.Millisecond)
	}
	claims, err = service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["PersistentVolumeClaim"]})
	if err != nil || len(claims.Items) != 1 || claims.Sync.State != "ready" || len(claims.Sync.Pending) != 0 || claims.Sync.PendingKinds != 0 {
		t.Fatalf("filled-in kind=%+v items=%d partial=%+v err=%v", claims.Sync, len(claims.Items), claims.Partial, err)
	}
	overview, err = service.Overview(t.Context())
	if err != nil || overview.Sync.PendingKinds != 0 {
		t.Fatalf("full overview still pending: %+v err=%v", overview.Sync, err)
	}
}

func TestInventoryKeepsTheCoreGenerationWhenExtrasFail(t *testing.T) {
	release := make(chan struct{})
	close(release)
	var extraDiscovery atomic.Int32
	var failExtras atomic.Bool
	server := coreFirstServer(t, release, &extraDiscovery, &failExtras)
	defer server.Close()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.client.indexLimiter = nil
	service.SetChangeStorage(provider)
	// Full discovery fails from the start; core discovery reads the core API
	// version directly and does not depend on it.
	failExtras.Store(true)
	service.startInventory(t.Context())
	t.Cleanup(service.StopCapture)
	deadline := time.Now().Add(10 * time.Second)
	for generation := cachedGeneration(service); len(generation.View.Kinds) == 0 || generation.Usage.ObservedAt.IsZero(); generation = cachedGeneration(service) {
		if time.Now().After(deadline) {
			t.Fatal("core generation never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	core := cachedGeneration(service)
	for {
		generation := cachedGeneration(service)
		failed := false
		for _, failure := range generation.DiscoveryPartial {
			failed = failed || failure.ResourceID == "kubernetes.discovery" && failure.Class == "unavailable"
		}
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("optional discovery failure was not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stored, _, err := readInventoryGeneration(provider, service.scope, "")
	if err != nil || !stored.View.Kinds["Pod"].ObservedAt.Equal(core.View.Kinds["Pod"].ObservedAt) {
		t.Fatal("optional discovery changed the committed Pod observation")
	}
	pods, status, err := service.CachedIndexSnapshot(t.Context(), "Pod")
	if err != nil || len(pods.Records) != 1 || status.Kinds["Pod"].State != "ready" || status.Kinds["Pod"].Error != "" {
		t.Fatalf("optional discovery blocked usable core records: records=%d status=%+v err=%v", len(pods.Records), status, err)
	}

	if len(status.Pending) != 0 {
		t.Fatalf("listed kind reported pending: %v", status.Pending)
	}
}

func TestCaptureIgnoresKindsTheGenerationHasNotListed(t *testing.T) {
	core := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now()}}, Records: []kubeindex.Record{{UID: "pod", Kind: "Pod", Name: "pod", Namespace: "shop"}}}
	full := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": core.Kinds["Pod"], "ConfigMap": {State: "ready", ObservedAt: time.Now()}}, Records: append(append([]kubeindex.Record{}, core.Records...), kubeindex.Record{UID: "settings", Kind: "ConfigMap", Name: "settings", Namespace: "shop"})}
	for delta := range inventoryDeltas(core, full, []string{"Pod", "ConfigMap"}) {
		t.Fatalf("a kind's first listing produced a change: %+v", delta)
	}
	for delta := range inventoryDeltas(full, core, []string{"Pod"}) {
		t.Fatalf("an unrequested pending kind produced a change: %+v", delta)
	}
	generation := inventoryGeneration{View: core, Pending: []string{indexedResourceIDs["ConfigMap"]}, PendingDiscovery: true}
	if pending := generation.pendingResources([]string{"Pod", "ConfigMap"}); len(pending) != 1 || pending[0] != indexedResourceIDs["ConfigMap"] {
		t.Fatalf("pending resources=%v", pending)
	}
	if pending := generation.pendingResources([]string{"Pod"}); len(pending) != 0 {
		t.Fatalf("listed kind reported pending: %v", pending)
	}
}

// seedCoreInventory installs a published core generation whose other kinds
// are still loading, as the first cycle on a cold cluster leaves it.
func seedCoreInventory(t *testing.T, service *Service, records []kubeindex.Record) []string {
	t.Helper()
	return seedCoreInventoryAt(t, service, records, time.Now().UTC())
}

func seedCoreInventoryAt(t *testing.T, service *Service, records []kubeindex.Record, now time.Time) []string {
	t.Helper()
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range inventoryCoreKinds {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: now}
	}
	view := kubeindex.Snapshot{Records: records, Kinds: kinds}
	done := make(chan struct{})
	close(done)
	generation := inventoryGeneration{View: view, Pending: pendingInventoryKinds(view), PendingDiscovery: true}
	service.changes.inventories[service.indexScope()] = &inventoryCache{generation: generation, done: done, cancel: func() {}}
	return generation.Pending
}

func TestPendingKindsNeverAppearAsFailuresInAnyView(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	owner := func(kind, name, uid string) []kubeindex.OwnerRef {
		return []kubeindex.OwnerRef{{Kind: kind, Name: name, UID: uid, Namespace: "shop", Controller: true, ControllerKnown: true}}
	}
	pending := seedCoreInventory(t, service, []kubeindex.Record{
		{UID: "ns", Kind: "Namespace", Name: "shop", Phase: "Active", Summary: map[string]string{"phase": "Active"}},
		{UID: "node", Kind: "Node", Name: "node-a", NodeReady: true},
		{UID: "deploy", Kind: "Deployment", Namespace: "shop", Name: "api", Replicas: 1, Summary: map[string]string{"desired_replicas": "1", "available_replicas": "0"}},
		{UID: "rs", Kind: "ReplicaSet", Namespace: "shop", Name: "api-1", Owners: owner("Deployment", "api", "deploy")},
		{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "api-1-x", NodeName: "node-a", Owners: owner("ReplicaSet", "api-1", "rs"),
			References: map[string][]string{"config_maps": {"settings"}, "secrets": {"token"}},
			Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 4}}},
	})
	if len(pending) == 0 {
		t.Fatal("fixture has no pending kinds")
	}
	pendingIDs := map[string]bool{}
	for _, id := range pending {
		pendingIDs[id] = true
	}
	ctx := t.Context()
	now := time.Now().UTC()
	views := map[string]func() (any, error){
		"overview":      func() (any, error) { return service.Overview(ctx) },
		"issues":        func() (any, error) { return service.Issues(ctx, IssueOptions{}) },
		"broken issues": func() (any, error) { return service.Issues(ctx, IssueOptions{Broken: true, IncludeChanges: true}) },
		"issue root": func() (any, error) {
			return service.Issues(ctx, IssueOptions{RootUID: "deploy", Namespace: "shop", IncludeChanges: true})
		},
		"workloads": func() (any, error) { return service.Workloads(ctx, WorkloadListOptions{}) },
		"pending list": func() (any, error) {
			return service.CachedResources(ctx, ListOptions{ResourceID: indexedResourceIDs["ConfigMap"]})
		},
		"core list": func() (any, error) {
			return service.CachedResources(ctx, ListOptions{ResourceID: indexedResourceIDs["Pod"]})
		},
		"graph":          func() (any, error) { return service.Graph(ctx, GraphQuery{Namespace: "shop"}) },
		"complete graph": func() (any, error) { return service.Graph(ctx, GraphQuery{Namespace: "shop", Complete: true}) },
		"overview graph": func() (any, error) { return service.OverviewGraph(ctx) },
		"related": func() (any, error) {
			return service.Related(ctx, NeighborhoodQuery{ResourceID: indexedResourceIDs["Pod"], Namespace: "shop", Name: "api-1-x"})
		},
		"timeline": func() (any, error) {
			return service.Timeline(ctx, TimelineQuery{Since: now.Add(-time.Hour), Until: now})
		},
		"timeline breaks": func() (any, error) {
			return service.Timeline(ctx, TimelineQuery{Since: now.Add(-time.Hour), Until: now, BreaksOnly: true})
		},
		"namespaces": func() (any, error) { return service.Namespaces(ctx) },
		"changes":    func() (any, error) { return service.Changes(ctx, ChangeQuery{}) },
		"crashes": func() (any, error) {
			return service.Crashes(ctx, kubechanges.CrashQuery{Since: now.Add(-time.Hour), Until: now})
		},
	}
	for name, read := range views {
		value, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := json.Marshal(value)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		failures, _ := body["partial_failures"].([]any)
		for _, failure := range failures {
			if id, _ := failure.(map[string]any)["resource_id"].(string); pendingIDs[id] {
				t.Errorf("%s reported loading kind %s as a failure: %v", name, id, failure)
			}
		}
		omitted, _ := body["omitted_categories"].([]any)
		for _, id := range omitted {
			if pendingIDs[id.(string)] {
				t.Errorf("%s reported loading kind %s as omitted", name, id)
			}
		}
		sync, _ := body["sync"].(map[string]any)
		if sync["state"] != "unavailable" && sync["pending_kinds"] == nil {
			t.Errorf("%s does not say other kinds are loading: %v", name, sync)
		}
	}
	root, err := service.Issues(ctx, IssueOptions{RootUID: "deploy", Namespace: "shop", IncludeChanges: true})
	if err != nil || len(root.Items) != 1 {
		t.Fatalf("root issue=%+v err=%v", root, err)
	}
	// Evidence that depends on loading kinds is marked as loading and the page
	// stays partial, so nothing is resolved or presented as complete.
	if len(root.Sync.Pending) == 0 || !root.Sync.Partial || len(root.Items[0].Pending) == 0 || root.Items[0].History == "complete" {
		t.Fatalf("root issue hid loading evidence: sync=%+v pending=%v history=%q", root.Sync, root.Items[0].Pending, root.Items[0].History)
	}
	if root.Items[0].Partial {
		t.Fatal("loading evidence was reported as a coverage failure")
	}
}

func TestInventoryPublishesBeforeOptionalMetricsUntilFirstReady(t *testing.T) {
	release := make(chan struct{})
	close(release)
	var extraDiscovery atomic.Int32
	server := coreFirstServer(t, release, &extraDiscovery, nil)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.client.indexLimiter = nil
	service.SetChangeStorage(storage.NewMemory())
	index, err := hydrateInventory(kubeindex.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	cache := &inventoryCache{namespaces: map[string]bool{"default": true}, wake: make(chan struct{}, 1)}
	ctx := context.WithValue(captureReadContext(t.Context(), "owner"), credentialEpochContextKey{}, service)
	early := 0
	publish := func(current *inventoryGeneration) error { early++; current.markFirstReady(time.Now()); return nil }
	generation := inventoryGeneration{}
	if dirty, err := service.collectInventoryCycle(ctx, cache, index, &generation, publish); err != nil || !dirty {
		t.Fatalf("cold cycle dirty=%v err=%v", dirty, err)
	}
	if early != 1 {
		t.Fatalf("cold first sync published early %d times, want once", early)
	}
	// The first complete inventory publishes before optional usage on the warm
	// cycle too, without falsely stamping the earlier core-only generation.
	if !generation.FirstReadyAt.IsZero() {
		t.Fatal("core-only generation claimed ready")
	}
	early = 0
	generation.Usage.ObservedAt = time.Time{}
	if dirty, err := service.collectInventoryCycle(ctx, cache, index, &generation, publish); err != nil || !dirty {
		t.Fatalf("warm cycle dirty=%v err=%v", dirty, err)
	}
	if early != 1 || generation.FirstReadyAt.IsZero() {
		t.Fatalf("first ready did not publish before optional usage: early=%d ready=%s", early, generation.FirstReadyAt)
	}
	early = 0
	generation.Usage.ObservedAt = time.Time{}
	if _, err := service.collectInventoryCycle(ctx, cache, index, &generation, publish); err != nil || early != 0 {
		t.Fatalf("later cycle published early=%d err=%v", early, err)
	}
	if generation.PendingDiscovery || len(generation.Pending) != 0 || generation.Usage.ObservedAt.IsZero() {
		t.Fatalf("warm cycle did not complete the inventory: pending=%v discovery=%v", generation.Pending, generation.PendingDiscovery)
	}
}

func coreBreakRecords() []kubeindex.Record {
	owner := func(kind, name, uid string) []kubeindex.OwnerRef {
		return []kubeindex.OwnerRef{{Kind: kind, Name: name, UID: uid, Namespace: "shop", Controller: true, ControllerKnown: true}}
	}
	return []kubeindex.Record{
		{UID: "ns", Kind: "Namespace", Name: "shop", Phase: "Active", Summary: map[string]string{"phase": "Active"}},
		{UID: "node", Kind: "Node", Name: "node-a", NodeReady: true},
		{UID: "deploy", Kind: "Deployment", Namespace: "shop", Name: "api", Replicas: 1, Summary: map[string]string{"desired_replicas": "1", "available_replicas": "0"}},
		{UID: "rs", Kind: "ReplicaSet", Namespace: "shop", Name: "api-1", Owners: owner("Deployment", "api", "deploy")},
		{UID: "pod", Kind: "Pod", Namespace: "shop", Name: "api-1-x", NodeName: "node-a", Owners: owner("ReplicaSet", "api-1", "rs"),
			References: map[string][]string{"config_maps": {"settings"}},
			Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 4}}},
	}
}

func TestAgedCoreDataIsStaleEvenWhileOtherKindsLoad(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	seedCoreInventoryAt(t, service, coreBreakRecords(), time.Now().UTC().Add(-30*time.Minute))
	_, status, err := service.CachedIndexSnapshot(t.Context(), "Pod", "ConfigMap")
	if err != nil || status.State != "stale" || !status.Partial || len(status.Pending) != 1 || status.PendingKinds == 0 {
		t.Fatalf("aged core data with a loading kind was not stale: %+v err=%v", status, err)
	}
	issues, err := service.Issues(t.Context(), IssueOptions{Broken: true, IncludeChanges: true})
	if err != nil || issues.Sync.State != "stale" || len(issues.Sync.Pending) == 0 {
		t.Fatalf("issues with loading evidence hid aged data: %+v err=%v", issues.Sync, err)
	}
	overview, err := service.Overview(t.Context())
	if err != nil || overview.Sync.State != "stale" || overview.Sync.PendingKinds == 0 {
		t.Fatalf("overview=%+v err=%v", overview.Sync, err)
	}
	// Fresh core data with loading kinds stays partial, not stale.
	seedCoreInventory(t, service, coreBreakRecords())
	if _, status, err := service.CachedIndexSnapshot(t.Context(), "Pod", "ConfigMap"); err != nil || status.State != "partial" {
		t.Fatalf("fresh core data reported %+v err=%v", status, err)
	}
}

func TestLoadingEvidenceKeepsRealCorrelationPartials(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	seedCoreInventory(t, service, coreBreakRecords())
	read := func() Issue {
		t.Helper()
		page, err := service.Issues(t.Context(), IssueOptions{RootUID: "deploy", Namespace: "shop", IncludeChanges: true})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("root issue=%+v err=%v", page, err)
		}
		return page.Items[0]
	}
	if item := read(); item.Partial || len(item.Pending) == 0 || item.History == "complete" {
		t.Fatalf("loading evidence alone: partial=%v pending=%v history=%q", item.Partial, item.Pending, item.History)
	}
	now := time.Now().UTC()
	truncated := Change{ID: "truncated", Cluster: "cluster", Kind: "Deployment", Namespace: "shop", Name: "api", UID: "deploy", RootUID: "deploy", Type: kubechanges.ImageChanged, At: now.Add(-time.Minute), FieldsTruncated: true}
	if err := service.changeStore().Append([]Change{truncated}, now); err != nil {
		t.Fatal(err)
	}
	item := read()
	found := false
	for _, linked := range item.Changes {
		found = found || linked.Change.ID == "truncated"
	}
	if !found || !item.Partial || len(item.Pending) == 0 {
		t.Fatalf("a truncated change was hidden behind loading evidence: found=%v partial=%v pending=%v", found, item.Partial, item.Pending)
	}
}

func TestInventoryProgressRetainsFactsWithoutInventingCompletedPhases(t *testing.T) {
	var progress inventoryProgress
	now := time.Now().UTC().Add(-time.Second)
	progress.advance("connected", now)
	progress.due(len(indexedResourceIDs), now)
	progress.complete("connected", now)
	progress.advance("listing", now)
	progress.kindObserved("Pod", kubeindex.KindStatus{State: "ready", Records: 2}, now)
	progress.kindListed("Pod", 2, now)
	progress.advance("building", now.Add(time.Millisecond))
	progress.complete("building", now)
	progress.advance("metrics", now.Add(2*time.Millisecond))
	progress.phaseResult("metrics", "unavailable (optional)")
	progress.advance("listing", now.Add(3*time.Millisecond))
	result := progress.snapshot()
	if !containsString(result.StepsDone, "connected") || !containsString(result.StepsDone, "building") || containsString(result.StepsDone, "listing") || containsString(result.StepsDone, "metrics") {
		t.Fatalf("invented or lost completed phases: %+v", result)
	}
	if result.KindsDone != 1 || result.Counts["Pod"] != 2 || len(result.RemainingKinds) != len(indexedResourceIDs)-1 || result.PhaseSeconds["listing"] <= 0 || result.PhaseStatus["metrics"] != "unavailable (optional)" {
		t.Fatalf("inconsistent progress: %+v", result)
	}
	progress.kindObserved("Deployment", kubeindex.KindStatus{State: "ready", Absent: true}, now)
	progress.kindObserved("Namespace", kubeindex.KindStatus{State: "error", Error: "forbidden"}, now)
	checked := progress.snapshot()
	if checked.KindsDone != 3 || checked.KindsDone+len(checked.RemainingKinds) != checked.KindsTotal || len(checked.Counts) != 1 {
		t.Fatalf("resolved API checks fabricated successful record counts: %+v", checked)
	}
	result.Counts["Pod"] = 99
	result.KindStatus["Pod"] = "pending"
	if next := progress.snapshot(); next.Counts["Pod"] != 2 || next.KindStatus["Pod"] != "ready" {
		t.Fatalf("snapshot mutated worker facts: %+v", next)
	}
}

func TestFirstReadyPublicationDoesNotWaitForOptionalUsage(t *testing.T) {
	release := make(chan struct{})
	var blockUsage atomic.Bool
	metricsStarted := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "metrics.k8s.io", "preferredVersion": map[string]any{"version": "v1beta1"}, "versions": []any{map[string]any{"version": "v1beta1"}}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("pods", "nodes", "namespaces")})
		case "/api/v1/pods", "/api/v1/nodes", "/api/v1/namespaces":
			writeJSON(writer, map[string]any{"items": []any{}})
		case "/apis/metrics.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": graphTestResources("pods", "nodes")})
		case "/apis/metrics.k8s.io/v1beta1/pods", "/apis/metrics.k8s.io/v1beta1/nodes":
			if blockUsage.Load() {
				metricsStarted <- struct{}{}
				select {
				case <-release:
				case <-request.Context().Done():
					return
				}
			}
			writeJSON(writer, map[string]any{"items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.client.indexLimiter = nil
	service.SetChangeStorage(storage.NewMemory())
	index, err := hydrateInventory(kubeindex.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	cache := &inventoryCache{namespaces: map[string]bool{}, wake: make(chan struct{}, 1)}
	ctx := context.WithValue(captureReadContext(t.Context(), "owner"), credentialEpochContextKey{}, service)
	generation := inventoryGeneration{}
	if _, err := service.collectInventoryCycle(ctx, cache, index, &generation); err != nil {
		t.Fatal(err)
	}
	// Make optional usage due again on the cycle that fills the pending kinds.
	generation.Usage.ObservedAt = time.Time{}
	blockUsage.Store(true)
	ready := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := service.collectInventoryCycle(ctx, cache, index, &generation, func(current *inventoryGeneration) error {
			current.markFirstReady(time.Now())
			if current.FirstReadyAt.IsZero() {
				return errors.New("complete inventory did not qualify as ready")
			}
			close(ready)
			return nil
		})
		finished <- err
	}()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("usage finished without a first ready publication: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first ready waited for blocked optional usage")
	}
	select {
	case err := <-finished:
		t.Fatalf("optional usage was not blocked: %v", err)
	default:
	}
	select {
	case <-metricsStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("optional usage read did not start")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
