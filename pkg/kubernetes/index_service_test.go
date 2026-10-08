package kubernetes

import (
	"context"
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
)

func TestIndexRecordRetainsProjectedWorkloadValues(t *testing.T) {
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
				items = append(items, map[string]any{"metadata": map[string]any{
					"name": "secret-" + strconv.Itoa(position), "namespace": "shop", "uid": "uid-" + strconv.Itoa(position),
					"labels": map[string]string{"app": "checkout"},
				}})
			}
			continuation := ""
			if end < 10001 {
				continuation = strconv.Itoa(end)
			}
			writeJSON(writer, map[string]any{"items": items, "metadata": map[string]any{"continue": continuation}})
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
	if _, _, err := service.loadIndexKind(context.Background(), "Secret"); err != ErrForbidden || listRequests.Load() != 1 {
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
		call := service.changes.refreshes[kubeindex.Scope{OrgID: "org-a", ClusterID: "cluster-a"}]
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
