package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestCaptureIndexStatusPreservesSafeVerification(t *testing.T) {
	status := CaptureStatus{Mode: "demand", Reason: "stable_principal_unverified", Verification: &projectedVerificationStatus{Stage: "jwks_response", HTTPStatus: http.StatusNotAcceptable}}
	encoded, err := json.Marshal(captureIndexStatus(status))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Verification projectedVerificationStatus `json:"verification"`
	}
	if json.Unmarshal(encoded, &decoded) != nil || decoded.Verification != *status.Verification {
		t.Fatal("stream projection dropped the safe verifier stage/status")
	}
	encoded, err = json.Marshal(captureIndexStatus(CaptureStatus{Mode: "continuous"}))
	if err != nil || bytes.Contains(encoded, []byte("verification")) {
		t.Fatal("verification diagnostic must remain optional")
	}
}

func TestUpgradeRegistryReadScopeAndTimeline(t *testing.T) {
	provider := storage.NewMemory()
	base := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"}, 0)
	base.SetChangeStorage(provider)
	now := time.Now().UTC()
	old := Change{ID: "old", Cluster: "cluster", Kind: "Pod", Name: "pod", Type: kubechanges.Created, At: now}
	if err := base.changeStore().Append([]Change{old}, now); err != nil {
		t.Fatal(err)
	}
	before, err := provider.ListBlobs("kubernetes/changes/")
	if err != nil {
		t.Fatal(err)
	}
	licensed := NewService(nil, Scope{OrgID: "licensed", ClusterID: "cluster", CredentialID: "credential"}, 0)
	licensed.SetChangeStorage(provider)
	registry := NewClusterRegistryWithScope(false, []ClusterEntry{{Info: ClusterInfo{ID: "cluster"}, Service: licensed}}, tenancy.NewOrgScope("licensed", "default"))
	service := registry.ResolveOrg("licensed")
	newer := old
	newer.ID = "new"
	if err := service.changeStore().Append([]Change{newer}, now); err != nil {
		t.Fatal(err)
	}
	ctx := tenancy.WithOrgScope(context.Background(), tenancy.NewOrgScope("licensed", "foreign"))
	page, err := service.Changes(ctx, ChangeQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("union=%+v %v", page, err)
	}
	page, err = registry.ResolveOrg("foreign").Changes(ctx, ChangeQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("foreign=%+v %v", page, err)
	}
	scoped := service.Scoped(service.Scope())
	scoped.readOrgs[1] = "changed"
	if service.readOrgs[1] != "default" {
		t.Fatal("scope copy aliases")
	}
	for _, blob := range before {
		after, err := provider.ReadBlob(blob.Name)
		if err != nil || !bytes.Equal(blob.Data, after) {
			t.Fatal("archive changed")
		}
	}
}

func TestIndexStreamAdmissionSpansClusters(t *testing.T) {
	releases := []func(){}
	for range MaxOrgIndexStreams {
		release, err := acquireIndexStream("stream-scope")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := acquireIndexStream("stream-scope"); !errors.Is(err, ErrIndexStreamBusy) {
		t.Fatalf("aggregate admission: %v", err)
	}
	other, err := acquireIndexStream("other-scope")
	if err != nil {
		t.Fatal(err)
	}
	other()
	for _, release := range releases {
		release()
		release()
	}
	release, err := acquireIndexStream("stream-scope")
	if err != nil {
		t.Fatal("admission not released")
	}
	release()
}

func TestIndexRecordRetainsProjectedWorkloadValues(t *testing.T) {
	var metadata metadataIndexItem
	metadata.Metadata.Name = "checkout-cfg"
	metadata.Metadata.UID = "config-uid"
	metadata.Metadata.ResourceVersion = "42"
	if record := metadata.record("ConfigMap"); record.ResourceVersion != "42" {
		t.Fatalf("metadata resource version lost: %+v", record)
	}
	for _, numeric := range []struct {
		name  string
		value any
	}{{"integer", 3}, {"decoded", float64(3)}, {"string", "3"}} {
		for _, containers := range []struct {
			name  string
			value any
		}{{"projected", []map[string]any{{"name": "app", "image": "example.invalid/api:v1"}}}, {"decoded", []any{map[string]any{"name": "app", "image": "example.invalid/api:v1"}}}} {
			t.Run(numeric.name+"/"+containers.name, func(t *testing.T) {
				resource := ProjectedResource{UID: "deployment-api", Kind: "Deployment", Namespace: "shop", Name: "api", Summary: map[string]any{
					"desired_replicas": numeric.value, "ready_replicas": numeric.value, "available_replicas": numeric.value,
					"generation": numeric.value, "observed_generation": numeric.value, "containers": containers.value,
				}}
				record := indexRecord(resource)
				if record.Replicas != 3 || record.Ready != 3 || record.Available != 3 || record.Generation != 3 || record.ObservedGeneration != 3 || len(record.Images) != 1 || record.Images[0] != "example.invalid/api:v1" {
					t.Fatalf("projected workload values lost: %+v", record)
				}
			})
		}
	}
	for _, invalid := range []any{-1, 1.5, "invalid", "2147483648", nil} {
		record := indexRecord(ProjectedResource{Summary: map[string]any{"desired_replicas": invalid}})
		if record.Replicas != 0 {
			t.Fatalf("invalid replica count accepted: %v", invalid)
		}
	}
}

func TestControllerRevisionNamesImagesAndForbiddenFallback(t *testing.T) {
	var denied atomic.Bool
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "controllerrevisions", "kind": "ControllerRevision", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			if denied.Load() {
				http.Error(writer, "CANARY_RBAC", http.StatusForbidden)
				return
			}
			items := []any{}
			for revision := 1; revision <= 2; revision++ {
				items = append(items, map[string]any{"metadata": map[string]any{"uid": "revision-" + strconv.Itoa(revision), "name": "revision-" + strconv.Itoa(revision), "namespace": "shop", "creationTimestamp": now.Add(-time.Duration(3-revision) * time.Minute).Format(time.RFC3339), "ownerReferences": []any{map[string]any{"kind": "StatefulSet", "name": "db", "uid": "root"}}}, "revision": revision, "data": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "db:v" + strconv.Itoa(revision), "env": []any{map[string]any{"value": "CANARY_ENV"}}}}}}}}})
			}
			containers := []any{}
			for position := range 17 {
				containers = append(containers, map[string]any{"name": "app-" + strconv.Itoa(position), "image": "db:v1"})
			}
			largeImages := []any{}
			for position := range 16 {
				largeImages = append(largeImages, map[string]any{"name": "app-" + strconv.Itoa(position), "image": strings.Repeat("a", 512)})
			}
			for _, bad := range []struct {
				name  string
				owner string
				data  any
			}{{"empty", "StatefulSet", map[string]any{"spec": map[string]any{}}}, {"large", "DaemonSet", map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}}}, {"large-images", "StatefulSet", map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": largeImages}}}}}, {"unsupported", "CustomController", "CANARY_DATA"}} {
				items = append(items, map[string]any{"metadata": map[string]any{"uid": bad.name, "name": bad.name, "namespace": "shop", "ownerReferences": []any{map[string]any{"kind": bad.owner, "name": bad.name, "uid": bad.name + "-root"}}}, "revision": 1, "data": bad.data})
			}
			writeJSON(writer, map[string]any{"items": items})
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	items, complete, err := service.loadIndexKind(t.Context(), "ControllerRevision")
	if err != nil || !complete || len(items) != 5 || items[1].Images[0] != "app=db:v2" {
		t.Fatalf("revision loader: %+v %v", items, err)
	}
	for _, item := range items[2:] {
		if item.Summary["revision_history_partial"] != "true" || len(item.Images) != 0 || len(item.Owners) != 1 {
			t.Fatalf("revision omission not isolated to its owner: %+v", item)
		}
	}
	if err := kubeindex.New(10).Ingest("ControllerRevision", items, true, now); err != nil {
		t.Fatalf("oversized revision still poisoned kind ingestion: %v", err)
	}
	root := kubeindex.Record{Kind: "StatefulSet", Name: "db", UID: "root", Namespace: "shop", UpdateRevision: "db-new", CurrentRevision: "db-old"}
	rebuilt := kubechanges.Reconstruct("cluster", append(items, root), now)
	encoded, _ := json.Marshal(rebuilt)
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatal("ControllerRevision values escaped")
	}
	if len(rebuilt) != 2 {
		t.Fatalf("bad revision or fallback tainted valid reconstruction: %+v", rebuilt)
	}
	denied.Store(true)
	if _, _, err := service.loadIndexKind(t.Context(), "ControllerRevision"); !errors.Is(err, ErrForbidden) {
		t.Fatal("forbidden revision read not classified")
	}
	fallback := kubechanges.Reconstruct("cluster", []kubeindex.Record{root}, now)
	if len(fallback) != 0 {
		t.Fatalf("fallback invented image or historical time: %+v", fallback)
	}
	root.ProgressingTransitionAt = now.Add(-time.Minute)
	fallback = kubechanges.Reconstruct("cluster", []kubeindex.Record{root}, now)
	if len(fallback) != 1 || fallback[0].Type != kubechanges.RolloutStarted || !fallback[0].At.Equal(root.ProgressingTransitionAt) {
		t.Fatalf("fallback lost actual transition time: %+v", fallback)
	}
}

func TestIndexSnapshotSharesSameScopeAndIsolatesOrganizations(t *testing.T) {
	var podLists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			podLists.Add(1)
			writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	base := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "credential-a"})
	registry := NewServiceRegistry(base)
	first := registry.ResolveOrg("org-a")
	second := registry.ResolveOrg("org-a")
	third := registry.ResolveOrg("org-b")
	if first != second || first == third {
		t.Fatal("service registry did not preserve exact scope identity")
	}
	for _, service := range []*Service{first, second} {
		snapshot, status, err := service.IndexSnapshot(context.Background(), "Pod")
		if err != nil || len(snapshot.Records) != 1 || status.State != "ready" || status.Kinds["Pod"].Records != 1 || status.AgeSeconds < 0 {
			t.Fatalf("index snapshot=%#v status=%#v err=%v", snapshot, status, err)
		}
	}
	if podLists.Load() != 1 {
		t.Fatalf("same-scope service copies made %d pod LISTs", podLists.Load())
	}
	if _, _, err := third.IndexSnapshot(context.Background(), "Pod"); err != nil {
		t.Fatal(err)
	}
	if podLists.Load() != 2 {
		t.Fatalf("different org did not receive an isolated index: LISTs=%d", podLists.Load())
	}
	if _, _, err := first.IndexSnapshot(context.Background(), "Pod", "UnknownKind"); err != ErrInvalidArguments {
		t.Fatalf("unknown kind error=%v", err)
	}
	if _, _, err := first.IndexSnapshot(context.Background()); err != ErrInvalidArguments {
		t.Fatalf("empty kind set error=%v", err)
	}
	if _, _, _, err := first.SubscribeIndex(context.Background(), 8, "Secret"); err != ErrInvalidArguments {
		t.Fatalf("non-stream kind error=%v", err)
	}
	if _, _, err := (*Service)(nil).IndexSnapshot(context.Background(), "Pod"); err != ErrInvalidArguments {
		t.Fatalf("nil service error=%v", err)
	}
}

func TestIndexLoaderPaginatesMetadataOnlyBeyondForegroundItemCap(t *testing.T) {
	var listRequests atomic.Int32
	var fullObjectRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/secrets":
			listRequests.Add(1)
			if !strings.Contains(request.Header.Get("Accept"), "as=PartialObjectMetadataList") {
				fullObjectRequests.Add(1)
				http.Error(writer, "full secret list forbidden", http.StatusForbidden)
				return
			}
			start, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			end := min(start+500, 10001)
			items := make([]map[string]any, 0, end-start)
			for position := start; position < end; position++ {
				items = append(items, map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{
					"name": "secret-" + strconv.Itoa(position), "namespace": "shop", "uid": "uid-" + strconv.Itoa(position),
					"labels": map[string]string{"app": "checkout"},
				}})
			}
			continuation := ""
			if end < 10001 {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": items, "metadata": map[string]any{"continue": continuation}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	records, complete, err := service.loadIndexKind(context.Background(), "Secret")
	if err != nil || !complete || len(records) != 10001 {
		t.Fatalf("loaded records=%d complete=%v err=%v", len(records), complete, err)
	}
	if listRequests.Load() != 21 || fullObjectRequests.Load() != 0 {
		t.Fatalf("list requests=%d full-object requests=%d", listRequests.Load(), fullObjectRequests.Load())
	}
	if records[10000].Name != "secret-10000" || records[10000].Namespace != "shop" {
		t.Fatalf("last metadata record=%+v", records[10000])
	}
}

func TestIndexLoaderRetriesOversizedPageAndKeepsReducedLimit(t *testing.T) {
	var requestedLimits []string
	var returnedItems []any
	for index := 0; index < 25; index++ {
		returnedItems = append(returnedItems, map[string]any{"metadata": map[string]any{
			"name": "pod-" + strconv.Itoa(index), "namespace": "shop", "uid": "uid-" + strconv.Itoa(index),
			"annotations": map[string]string{"payload": strings.Repeat("x", 240)},
		}})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			requestedLimits = append(requestedLimits, request.URL.Query().Get("limit"))
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			start, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			end := min(start+limit, len(returnedItems))
			continuation := ""
			if end < len(returnedItems) {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": continuation}, "items": returnedItems[start:end]})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	service.client.maxBodyBytes = 4096
	records, complete, err := service.loadIndexKind(context.Background(), "Pod")
	if err != nil || !complete || len(records) != len(returnedItems) {
		t.Fatalf("records=%d complete=%v err=%v limits=%v", len(records), complete, err, requestedLimits)
	}
	if len(requestedLimits) < 4 || requestedLimits[0] != "500" || requestedLimits[len(requestedLimits)-1] != "10" {
		t.Fatalf("index LIST limits did not fall back and persist: %v", requestedLimits)
	}
	for _, limit := range requestedLimits[len(requestedLimits)-3:] {
		if limit != "10" {
			t.Fatalf("later index pages did not retain the reduced limit: %v", requestedLimits)
		}
	}
}

func TestIndexLoaderDoesNotRetryForbiddenMetadataListAsFullObjects(t *testing.T) {
	var listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/secrets":
			listRequests.Add(1)
			http.Error(writer, "forbidden", http.StatusForbidden)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	if _, _, err := service.loadIndexKind(context.Background(), "Secret"); !errors.Is(err, ErrForbidden) || listRequests.Load() != 1 {
		t.Fatalf("error=%v secret LIST requests=%d", err, listRequests.Load())
	}
}

func TestIndexLoaderSupportsFiftyThousandRecords(t *testing.T) {
	var listRequests atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			listRequests.Add(1)
			start, _ := strconv.Atoi(request.URL.Query().Get("continue"))
			end := min(start+500, 50000)
			items := make([]map[string]any, 0, end-start)
			for position := start; position < end; position++ {
				items = append(items, map[string]any{"metadata": map[string]any{
					"name": "pod-" + strconv.Itoa(position), "namespace": "shop", "uid": "uid-" + strconv.Itoa(position),
				}})
			}
			continuation := ""
			if end < 50000 {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"items": items, "metadata": map[string]any{"continue": continuation}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer httpServer.Close()
	service := newTestService(t, httpServer.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	started := time.Now()
	records, complete, err := service.loadIndexKind(context.Background(), "Pod")
	if err != nil || !complete || len(records) != 50000 || listRequests.Load() != 100 {
		t.Fatalf("loaded records=%d complete=%v elapsed=%s err=%v", len(records), complete, time.Since(started), err)
	}
	if records[0].Kind != "Pod" || records[len(records)-1].UID == "" {
		t.Fatalf("first or last indexed record invalid: first=%+v last=%+v", records[0], records[len(records)-1])
	}
}

func TestScopedIndexReadinessRequiresEveryRequestedKind(t *testing.T) {
	readyAt := time.Now().UTC()
	status := kubeindex.Status{State: "ready", Kinds: map[string]kubeindex.KindStatus{
		"Pod": {State: "ready", ObservedAt: readyAt},
	}}
	got := scopedIndexStatus(status, []string{"Pod", "Secret"})
	if got.State != "warming" || got.Partial || len(got.Kinds) != 1 || got.Kinds["Pod"].State != "ready" {
		t.Fatalf("scoped readiness=%+v", got)
	}
}

func TestIndexSnapshotReadinessWaitDoesNotWaitForSlowCollection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			once.Do(func() { close(started) })
			<-release
			writeJSON(writer, map[string]any{"items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer close(release)
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org-a", ClusterID: "cluster-a"}, time.Minute)
	startedAt := time.Now()
	_, status, err := service.IndexSnapshot(context.Background(), "Pod")
	elapsed := time.Since(startedAt)
	if err != nil || elapsed < kubernetesIndexReadinessWait || elapsed > kubernetesIndexReadinessWait+500*time.Millisecond || status.State != "warming" {
		t.Fatalf("snapshot readiness=%+v elapsed=%s err=%v", status, elapsed, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("background list did not start before the readiness response")
	}
}

func TestIndexSnapshotCoalescesConcurrentKindsPerScope(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var podOnce sync.Once
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	var podLists atomic.Int32
	var nodeLists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "nodes", "kind": "Node", "namespaced": false, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/pods":
			podLists.Add(1)
			podOnce.Do(func() { close(started) })
			<-release
			writeJSON(writer, map[string]any{"items": []any{}})
		case "/api/v1/nodes":
			nodeLists.Add(1)
			writeJSON(writer, map[string]any{"items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer releaseRefresh()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org-a", ClusterID: "cluster-a"}, time.Minute)
	first := make(chan error, 1)
	go func() {
		_, _, indexErr := service.IndexSnapshot(context.Background(), "Pod")
		first <- indexErr
	}()
	<-started

	secondStarted := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, _, indexErr := service.IndexSnapshot(context.Background(), "Node")
		second <- indexErr
	}()
	<-secondStarted
	deadline := time.After(time.Second)
	for {
		service.changes.mu.Lock()
		call := service.changes.refreshes[indexRefreshKey{scope: service.indexScope()}]
		queuedNode := call != nil
		if queuedNode {
			_, queuedNode = call.requested["Node"]
		}
		refreshCount := len(service.changes.refreshes)
		service.changes.mu.Unlock()
		if queuedNode {
			if refreshCount != 1 {
				t.Fatalf("in-flight scope refreshes=%d, want one", refreshCount)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("second kind did not join the in-flight scope refresh")
		default:
		}
	}
	releaseRefresh()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if podLists.Load() != 1 || nodeLists.Load() != 1 {
		t.Fatalf("kind refreshes: Pod=%d Node=%d, want one each", podLists.Load(), nodeLists.Load())
	}
}

func TestServiceChangesPersistAndIsolateOrgScopes(t *testing.T) {
	provider := storage.NewMemory()
	base := NewService(nil, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}, 0)
	base.SetChangeStorage(provider)
	orgA := base.Scoped(Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"})
	orgB := base.Scoped(Scope{OrgID: "org-b", ClusterID: "cluster-a", CredentialID: "cred-a"})
	now := time.Now().UTC()
	change := Change{ID: "change-a", Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-a", Type: kubechanges.Created, At: now}
	if err := orgA.changeStore().Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	page, err := orgA.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != change.ID {
		t.Fatalf("org A changes=%#v err=%v", page, err)
	}
	page, err = orgB.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("org B read org A changes: %#v err=%v", page, err)
	}
	reopened := NewService(nil, Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}, 0)
	reopened.SetChangeStorage(provider)
	page, err = reopened.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("reopened persistent changes=%#v err=%v", page, err)
	}
	if _, err := (*Service)(nil).Changes(context.Background(), ChangeQuery{}); err != ErrInvalidArguments {
		t.Fatalf("nil service error=%v", err)
	}
}

func TestPersistIndexDeltasAndResyncGap(t *testing.T) {
	provider := storage.NewMemory()
	service := NewService(nil, Scope{OrgID: "org-a", ClusterID: "cluster-a"}, 0)
	service.SetChangeStorage(provider)
	now := time.Now().UTC()
	oldRecord := kubeindex.Record{UID: "uid-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", Images: []string{"checkout:v1"}}
	newRecord := oldRecord
	newRecord.Images = []string{"checkout:v2"}
	deltas := make(chan kubeindex.Delta, 1)
	deltas <- kubeindex.Delta{Kind: "Deployment", Op: "upsert", Old: &oldRecord, New: &newRecord}
	close(deltas)
	if err := persistIndexDeltas(service.changeStore(), "cluster-a", deltas, now); err != nil {
		t.Fatal(err)
	}
	page, err := service.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 1 || page.Items[0].Type != kubechanges.ImageChanged {
		t.Fatalf("persisted deltas=%#v err=%v", page, err)
	}
	resync := make(chan kubeindex.Delta, 1)
	resync <- kubeindex.Delta{Kind: "Pod", Resync: true}
	close(resync)
	if err := persistIndexDeltas(service.changeStore(), "cluster-a", resync, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err = service.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
	if err != nil || len(page.Gaps) != 1 {
		t.Fatalf("resync gaps=%#v err=%v", page.Gaps, err)
	}
}

// A kind the cluster serves but that holds nothing yet is a listed kind: the
// first object created after that empty generation is exactly one created
// change, not a suppressed one.
func TestServedEmptyGenerationThenCreationReportsExactlyOneCreatedChange(t *testing.T) {
	index := kubeindex.New(10)
	now := time.Now().UTC()
	if err := index.Refresh(t.Context(), "ConfigMap", 0, now, func(context.Context) ([]kubeindex.Record, bool, error) {
		return nil, false, kubeindex.ErrKindNotServed
	}); err != nil {
		t.Fatal(err)
	}
	unserved := index.Snapshot()
	if err := index.Ingest("ConfigMap", nil, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	servedEmpty := index.Snapshot()
	created := kubeindex.Record{Kind: "ConfigMap", UID: "inventory", Namespace: "shop", Name: "inventory"}
	if err := index.Ingest("ConfigMap", []kubeindex.Record{created}, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	afterCreation := index.Snapshot()
	empty := indexDeltas(inventoryDeltas(unserved, servedEmpty, []string{"ConfigMap"}))
	if len(empty) != 0 {
		t.Fatalf("an empty generation of a served kind reported %d changes", len(empty))
	}
	deltas := indexDeltas(inventoryDeltas(servedEmpty, afterCreation, []string{"ConfigMap"}))
	if len(deltas) != 1 || deltas[0].Op != "upsert" || deltas[0].Old != nil || deltas[0].New == nil || deltas[0].New.UID != created.UID {
		t.Fatalf("the first object created after a served empty generation reported %d changes: %#v", len(deltas), deltas)
	}
}

// An API group that answers on another version is still discovered, but a kind
// pinned to a version that could not be read is a failure: the group's answer
// elsewhere says nothing about that version's contents.
func TestKindPinnedToAnUnreadableDiscoveryVersionFailsInsteadOfReadingEmpty(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writeJSON(writer, map[string]any{"items": []any{}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "acme", ClusterID: "cluster"})
	failed := Discovery{
		Resources: []ResourceDefinition{{ID: "networking.k8s.io~v1beta1~ingresses", Group: "networking.k8s.io", Version: "v1beta1", Resource: "ingresses", Kind: "Ingress", Namespaced: true, Available: true}},
		unlisted:  []PartialFailure{{Scope: "discovery", GroupVersion: "networking.k8s.io~v1", Class: "unavailable"}},
	}
	ctx := context.WithValue(t.Context(), discoveryContextKey{}, failed)
	ctx = context.WithValue(ctx, inventoryDiscoveryContextKey{}, failed)
	ctx = context.WithValue(ctx, indexDefinitionContextKey{}, map[string]ResourceDefinition{})
	if _, _, err := service.loadIndexKind(ctx, "Ingress"); err == nil || errors.Is(err, kubeindex.ErrKindNotServed) {
		t.Fatalf("a kind pinned to an unreadable API version read as an absent kind: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("the loader issued %d requests for a version discovery could not read", requests.Load())
	}
	served := Discovery{ObservedAt: failed.ObservedAt, Resources: failed.Resources}
	ctx = context.WithValue(t.Context(), discoveryContextKey{}, served)
	ctx = context.WithValue(ctx, inventoryDiscoveryContextKey{}, served)
	ctx = context.WithValue(ctx, indexDefinitionContextKey{}, map[string]ResourceDefinition{})
	if _, _, err := service.loadIndexKind(ctx, "Ingress"); !errors.Is(err, kubeindex.ErrKindNotServed) {
		t.Fatalf("a discovered group that serves another version did not read as absent: %v", err)
	}
}

// Discovery keeps the failure of a version that could not be read even after
// another version of the same group answered, so a resource pinned to it is not
// silently published as empty.
func TestDiscoveryKeepsAnUnreadableVersionFailureForPinnedResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{
				"name":             "networking.k8s.io",
				"versions":         []any{map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"}, map[string]any{"groupVersion": "networking.k8s.io/v1beta1", "version": "v1beta1"}},
				"preferredVersion": map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"},
			}}})
		case "/apis/networking.k8s.io/v1":
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","reason":"InternalError","code":500}`))
		case "/apis/networking.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "ingresses", "kind": "Ingress", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "acme", ClusterID: "cluster"})
	discovery, err := service.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Partial) != 0 {
		t.Fatalf("a group that answered on another version reported partial discovery: %#v", discovery.Partial)
	}
	if !discoveryVersionFailed(discovery, indexedResourceIDs["Ingress"]) {
		t.Fatalf("the failure of the version Ingress is pinned to was dropped: %#v", discovery)
	}
	if discoveryVersionFailed(discovery, "networking.k8s.io~v1beta1~ingresses") {
		t.Fatal("a version that answered was reported as unreadable")
	}
}

// A live refresh resolves the kind through a discovery read of its own. The
// failure of the version the kind is pinned to must still read as a failure
// there: that path holds no pre-fetched discovery in its context.
func TestOnDemandRefreshKeepsAPinnedVersionFailureAsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{
				"name":             "networking.k8s.io",
				"versions":         []any{map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"}, map[string]any{"groupVersion": "networking.k8s.io/v1beta1", "version": "v1beta1"}},
				"preferredVersion": map[string]any{"groupVersion": "networking.k8s.io/v1", "version": "v1"},
			}}})
		case "/apis/networking.k8s.io/v1":
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","reason":"InternalError","code":500}`))
		case "/apis/networking.k8s.io/v1beta1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "ingresses", "kind": "Ingress", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "acme", ClusterID: "cluster"})
	if _, status, err := service.IndexSnapshot(t.Context(), "Ingress"); err != nil {
		t.Fatalf("ingress refresh: %v", err)
	} else if state := status.Kinds["Ingress"]; state.State != "error" || state.Absent {
		t.Fatalf("a kind pinned to an unreadable API version read as %+v", state)
	}
}

func indexDeltas(deltas <-chan kubeindex.Delta) []kubeindex.Delta {
	collected := []kubeindex.Delta{}
	for delta := range deltas {
		collected = append(collected, delta)
	}
	return collected
}

// A denial is memoized only for a bounded time and only for its own tenant, so a
// granted permission becomes visible on the next probe instead of an hour later,
// and one organization's denial cannot silence another's read.
func TestMetadataDenialExpiresAndIsScopedPerTenant(t *testing.T) {
	// The window is part of the contract, not an implementation detail: a grant
	// has to become visible within a bounded time, so the memo may never grow
	// past a minute.
	if metadataDenyTTL <= 0 || metadataDenyTTL > time.Minute {
		t.Fatalf("denial memo window %s is not bounded to a minute", metadataDenyTTL)
	}
	var listRequests atomic.Int32
	var denied atomic.Bool
	denied.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{
				map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
		case "/api/v1/secrets":
			listRequests.Add(1)
			if denied.Load() {
				http.Error(writer, "forbidden", http.StatusForbidden)
				return
			}
			writeJSON(writer, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	service.client.now = func() time.Time { return now }

	if _, _, err := service.loadIndexKind(context.Background(), "Secret"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("first read error=%v", err)
	}
	if listRequests.Load() != 1 {
		t.Fatalf("first read requests=%d", listRequests.Load())
	}
	// Inside the memo window the retry must not reach the cluster at all: a
	// persistent denial costs one probe per window, not one per retry cycle.
	for attempt := 0; attempt < 3; attempt++ {
		if _, _, err := service.loadIndexKind(context.Background(), "Secret"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("memoized read error=%v", err)
		}
	}
	if listRequests.Load() != 1 {
		t.Fatalf("memoized retries reached the cluster: requests=%d", listRequests.Load())
	}
	// The denial is scoped to org-a: another tenant on the same cluster is not
	// blocked by it.
	other := service.Scoped(Scope{OrgID: "org-b", ClusterID: "cluster-a"})
	other.client.now = func() time.Time { return now }
	if _, _, err := other.loadIndexKind(context.Background(), "Secret"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other tenant error=%v", err)
	}
	if listRequests.Load() != 2 {
		t.Fatalf("another tenant was silenced by the first tenant's denial: requests=%d", listRequests.Load())
	}
	// A grant is invisible only until the bounded window ends.
	now = now.Add(metadataDenyTTL + time.Second)
	denied.Store(false)
	records, complete, err := service.loadIndexKind(context.Background(), "Secret")
	if err != nil || !complete || len(records) != 0 {
		t.Fatalf("recovery: records=%d complete=%v err=%v", len(records), complete, err)
	}
	if listRequests.Load() != 3 {
		t.Fatalf("recovery did not probe the cluster: requests=%d", listRequests.Load())
	}
	// And a success clears the memo, so the next read is a real read again.
	if _, _, err := service.loadIndexKind(context.Background(), "Secret"); err != nil {
		t.Fatal(err)
	}
	if listRequests.Load() != 4 {
		t.Fatalf("cleared memo did not probe: requests=%d", listRequests.Load())
	}
}

// A remembered credential failure is still a credential failure. Replaying the
// memo as forbidden sends an operator to change RBAC for a token that is simply
// invalid, and hides a revoked credential behind a missing permission.
func TestMetadataDenialMemoKeepsItsClass(t *testing.T) {
	var listRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "configmaps", "kind": "ConfigMap", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/configmaps":
			listRequests.Add(1)
			http.Error(writer, "denied", http.StatusUnauthorized)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org-a", ClusterID: "cluster-a"})
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	service.client.now = func() time.Time { return now }

	if _, _, err := service.loadIndexKind(context.Background(), "ConfigMap"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("first read error=%v", err)
	}
	// The memo answers the next reads without touching the cluster, and it has to
	// answer with the class it remembered.
	for attempt := 0; attempt < 3; attempt++ {
		_, _, err := service.loadIndexKind(context.Background(), "ConfigMap")
		if errors.Is(err, ErrForbidden) || !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("memoized credential failure reported as a missing permission: %v", err)
		}
		if class := errorClass(err); class != "unauthorized" {
			t.Fatalf("memoized credential failure class = %q", class)
		}
	}
	if listRequests.Load() != 1 {
		t.Fatalf("memoized retries reached the cluster: requests=%d", listRequests.Load())
	}
}
