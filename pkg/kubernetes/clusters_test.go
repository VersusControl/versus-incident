package kubernetes

import (
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
)

func TestClusterResolveOrgScopeAndTimelineIsolation(t *testing.T) {
	provider := storage.NewMemory()
	entries := []ClusterEntry{}
	for _, id := range []string{"one", "two"} {
		base := NewService(nil, Scope{OrgID: "default", ClusterID: id, CredentialID: "credential-" + id}, 0)
		base.SetChangeStorage(provider)
		entries = append(entries, ClusterEntry{Info: ClusterInfo{ID: id}, Service: base})
	}
	registry := NewClusterRegistry(true, entries)
	if _, err := registry.ResolveCluster("licensed", ""); !errors.Is(err, ErrClusterRequired) {
		t.Fatal(err)
	}
	if _, err := registry.ResolveCluster("licensed", "missing"); !errors.Is(err, ErrClusterNotFound) {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	defaultStore := kubechanges.NewStore(provider, kubeindex.Scope{OrgID: "default", ClusterID: "one", CredentialID: "credential-one"})
	if err := defaultStore.Append([]kubechanges.Change{{ID: "saved", Cluster: "one", Kind: "Pod", Name: "saved", Type: kubechanges.Created, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	for _, orgID := range []string{"default", "licensed"} {
		for _, id := range []string{"one", "two"} {
			service, err := registry.ResolveCluster(orgID, id)
			if err != nil || service.Scope() != (Scope{OrgID: orgID, ClusterID: id, CredentialID: "credential-" + id}) {
				t.Fatalf("scope: %+v %v", service, err)
			}
			page, err := service.Changes(context.Background(), ChangeQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute)})
			want := 0
			if orgID == "default" && id == "one" {
				want = 1
			}
			if err != nil || len(page.Items) != want {
				t.Fatalf("timeline %s/%s: %d %v", orgID, id, len(page.Items), err)
			}
			if service != registry.Resolve(Scope{OrgID: orgID, ClusterID: id, CredentialID: "untrusted"}) {
				t.Fatal("resolver accepted credential override")
			}
		}
	}
	single := NewServiceRegistry(entries[0].Service)
	if single.ResolveOrg("licensed") != registry.ResolveOrg("licensed") && single.ResolveOrg("licensed").Scope() != registry.ResolveOrg("licensed").Scope() {
		t.Fatal("legacy org resolver drift")
	}
}

func TestClusterListFiltersBeforeReadsAndCachesByOrg(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	base := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "one", CredentialID: "private-credential"})
	registry := NewClusterRegistry(true, []ClusterEntry{{Info: ClusterInfo{ID: "one", Provider: "generic"}, Service: base}, {Info: ClusterInfo{ID: "broken", Provider: "eks"}, Err: errors.New("private-endpoint upstream-body")}})
	if result := registry.Summaries(context.Background(), "default", func(string) bool { return false }); len(result) != 0 || reads.Load() != 0 {
		t.Fatal("denied cluster fetched")
	}
	result := registry.Summaries(context.Background(), "default", nil)
	before := reads.Load()
	registry.Summaries(context.Background(), "default", nil)
	if before == 0 || reads.Load() != before || len(result) != 2 || result[1].Health != "unreachable" {
		t.Fatal("summary cache or reachability")
	}
	encoded, _ := json.Marshal(result)
	for _, denied := range []string{"private-credential", "private-endpoint", "upstream-body", "org_id", server.URL} {
		if strings.Contains(string(encoded), denied) {
			t.Fatalf("summary leaked %s", denied)
		}
	}
	registry.Summaries(context.Background(), "licensed", func(id string) bool { return id == "one" })
	if reads.Load() == before {
		t.Fatal("cache crossed org")
	}
}

func TestClusterListBoundedFanOutAcrossConcurrentRequests(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	entries := []ClusterEntry{}
	for _, id := range []string{"one", "two", "three", "four", "five"} {
		entries = append(entries, ClusterEntry{Info: ClusterInfo{ID: id}, Service: newTestService(t, server.URL, Scope{ClusterID: id})})
	}
	registry := NewClusterRegistry(true, entries)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	var requests sync.WaitGroup
	for range 3 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			for _, value := range registry.Summaries(ctx, "default", nil) {
				if value.Health != "unreachable" {
					t.Error("hung cluster not unreachable")
				}
			}
		}()
	}
	requests.Wait()
	if maximum.Load() > 4 || maximum.Load() == 0 || time.Since(started) > time.Second {
		t.Fatalf("fanout=%d elapsed=%s", maximum.Load(), time.Since(started))
	}
}

func TestClusterHealth(t *testing.T) {
	for _, test := range []struct {
		overview Overview
		want     string
	}{{Overview{}, "unknown"}, {Overview{Nodes: 1, ReadyNodes: 1}, "healthy"}, {Overview{Nodes: 1}, "attention"}, {Overview{Warnings: 1}, "attention"}, {Overview{Nodes: 1, Truncated: true}, "partial"}} {
		if got := ClusterHealth(test.overview); got != test.want {
			t.Fatalf("health %s != %s", got, test.want)
		}
	}
}

func TestClusterSummaryRetriesUnreachableWithoutCachingFailure(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		if unavailable.Load() {
			http.Error(writer, "private upstream failure", http.StatusServiceUnavailable)
			return
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	registry := NewServiceRegistry(newTestService(t, server.URL, Scope{ClusterID: "one"}))
	first := registry.Summaries(context.Background(), "default", nil)
	if len(first) != 1 || first[0].Health != "unreachable" || len(registry.summaries) != 0 {
		t.Fatalf("failure summary=%+v cache=%v", first, registry.summaries)
	}
	before := reads.Load()
	unavailable.Store(false)
	second := registry.Summaries(context.Background(), "default", nil)
	if len(second) != 1 || second[0].Health == "unreachable" || reads.Load() <= before {
		t.Fatalf("immediate retry summary=%+v reads=%d", second, reads.Load())
	}
}

func TestClusterSummaryIncludesActualVersionAndIssueTotals(t *testing.T) {
	var version atomic.Value
	version.Store("v1.33.2-eks.123")
	var denyIssues atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/version":
			writeJSON(writer, map[string]any{"gitVersion": version.Load(), "platform": "private-platform", "gitCommit": "private-commit"})
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{
				map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}},
				map[string]any{"name": "batch", "versions": []any{map[string]any{"groupVersion": "batch/v1", "version": "v1"}}},
			}})
		case "/api/v1", "/apis/apps/v1", "/apis/batch/v1":
			resources := []any{}
			for _, resource := range []struct{ path, name, kind string }{
				{"/api/v1", "nodes", "Node"}, {"/api/v1", "pods", "Pod"}, {"/api/v1", "events", "Event"}, {"/api/v1", "namespaces", "Namespace"},
				{"/apis/apps/v1", "deployments", "Deployment"}, {"/apis/apps/v1", "statefulsets", "StatefulSet"}, {"/apis/apps/v1", "daemonsets", "DaemonSet"},
				{"/apis/batch/v1", "jobs", "Job"}, {"/apis/batch/v1", "cronjobs", "CronJob"},
			} {
				if resource.path == request.URL.Path {
					resources = append(resources, map[string]any{"name": resource.name, "kind": resource.kind, "namespaced": resource.kind != "Node" && resource.kind != "Namespace", "verbs": []string{"get", "list"}})
				}
			}
			writeJSON(writer, map[string]any{"resources": resources})
		case "/api/v1/pods":
			pods := []any{}
			for index := range 12 {
				name := "failed-" + strconv.Itoa(index)
				pods = append(pods, map[string]any{"kind": "Pod", "metadata": map[string]any{"name": name, "namespace": "shop", "uid": name}, "status": map[string]any{"phase": "Failed"}})
			}
			writeJSON(writer, map[string]any{"items": pods})
		default:
			if denyIssues.Load() && request.URL.Path == "/api/v1/events" {
				http.Error(writer, "private failure", http.StatusForbidden)
				return
			}
			writeJSON(writer, map[string]any{"items": []any{}})
		}
	}))
	defer server.Close()
	base := newTestService(t, server.URL, Scope{ClusterID: "one"})
	registry := NewServiceRegistry(base)
	result := registry.Summaries(context.Background(), "default", nil)
	if len(result) != 1 || result[0].Version != "v1.33.2-eks.123" || result[0].Issues == nil || *result[0].Issues != 12 {
		t.Fatalf("actual summary=%+v", result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("summary leaked non-version fields: %s", encoded)
	}
	denyIssues.Store(true)
	version.Store("v1.33.2\nprivate-token")
	result = NewServiceRegistry(base).Summaries(context.Background(), "default", nil)
	if result[0].Version != "" || result[0].Issues != nil {
		t.Fatalf("unsafe version or incomplete issues exposed: %+v", result)
	}
	version.Store(strings.Repeat("x", 4096))
	if got := base.clusterVersion(context.Background()); got != "" {
		t.Fatalf("oversized version exposed: %q", got)
	}
}

func TestClusterSummaryReadBudgetStartsAfterAdmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	registry := NewServiceRegistry(newTestService(t, server.URL, Scope{ClusterID: "one"}))
	registry.summaryTimeout = 100 * time.Millisecond
	for range 4 {
		registry.summarySlots <- struct{}{}
	}
	result := make(chan []ClusterSummary, 1)
	go func() { result <- registry.Summaries(context.Background(), "default", nil) }()
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	for range 4 {
		<-registry.summarySlots
	}
	if summaries := <-result; len(summaries) != 1 || summaries[0].Health == "unreachable" {
		t.Fatal("admission queue consumed the cluster read budget")
	}
}
