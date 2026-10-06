package fakekube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

func TestTriageFixtureSafeProjectionAndLogs(t *testing.T) {
	for _, scenario := range []string{"triage", "fault-forbidden-secret", "fault-transient-pods", "fault-partial-pods"} {
		t.Run(scenario, func(t *testing.T) {
			server, err := NewServer(Config{Scenario: scenario, Seed: 1})
			if err != nil {
				t.Fatal(err)
			}
			for resource, count := range map[string]int{"pods": 1, "nodes": 1, "secrets": 1, "configmaps": 1} {
				if got := server.store.Count(resource); got != count {
					t.Fatalf("%s count=%d, want %d", resource, got, count)
				}
			}
			for _, fixture := range []struct {
				resource string
				name     string
				canary   string
			}{
				{"pods", "checkout-api-0", "secret-token"},
				{"secrets", "api-secret", "c2VjcmV0LXRva2Vu"},
				{"configmaps", "api-config", "must-not-cross"},
			} {
				raw, exists := server.store.Get(fixture.resource, "payments", fixture.name)
				if !exists || !strings.Contains(string(raw), fixture.canary) {
					t.Fatalf("%s/%s missing raw canary", fixture.resource, fixture.name)
				}
			}
			httpServer := httptest.NewServer(server)
			defer httpServer.Close()
			client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "fakekube"}, time.Minute)
			redactor, errs := agent.NewRedactor(false, nil)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			service.SetScrubber(redactor)
			if scenario == "fault-transient-pods" {
				transient := httptest.NewRecorder()
				server.ServeHTTP(transient, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0", nil))
				if transient.Code != http.StatusServiceUnavailable {
					t.Fatalf("transient pod status=%d", transient.Code)
				}
			}
			for _, fixture := range []struct {
				resource string
				name     string
				key      string
			}{
				{"pods", "checkout-api-0", "api"},
				{"secrets", "api-secret", "token"},
				{"configmaps", "api-config", "config.yaml"},
			} {
				projected, err := service.Get(context.Background(), "core~v1~"+fixture.resource, "payments", fixture.name)
				if scenario == "fault-forbidden-secret" && fixture.resource == "secrets" {
					if err != kubernetes.ErrForbidden {
						t.Fatalf("secret denial=%v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(projected)
				if err != nil || len(encoded) > 4096 || !strings.Contains(string(encoded), fixture.key) {
					t.Fatalf("%s projection bytes=%d missing key=%q err=%v", fixture.resource, len(encoded), fixture.key, err)
				}
				for _, canary := range []string{"secret-token", "c2VjcmV0LXRva2Vu", "must-not-cross"} {
					if strings.Contains(string(encoded), canary) {
						t.Fatalf("%s projection leaked %q", fixture.resource, canary)
					}
				}
			}
			for _, container := range []string{"", "api"} {
				raw := httptest.NewRecorder()
				server.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0/log?container="+container+"&tailLines=20", nil))
				if raw.Code != http.StatusOK || raw.Body.String() != "2026-01-01T00:00:00Z fakekube checkout api token=super-secret-value\n" {
					t.Fatalf("raw log status=%d text=%q", raw.Code, raw.Body.String())
				}
				logs, err := service.PodLogs(context.Background(), "payments", "checkout-api-0", container, false, 60, 20)
				if err != nil || logs.Truncated || logs.TailLines != 20 || len(logs.Text) > 4096 || !strings.Contains(logs.Text, "<REDACTED:password>") || strings.Contains(logs.Text, "super-secret-value") {
					t.Fatalf("projected logs=%#v err=%v", logs, err)
				}
			}
		})
	}
}

func TestServerResourcePathRouting(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		path      string
		resource  string
		namespace string
		item      bool
		kind      string
	}{
		{"/api/v1/namespaces", "namespaces", "", false, "List"},
		{"/api/v1/namespaces/payments", "namespaces", "", true, "Namespace"},
		{"/api/v1/nodes/node-a", "nodes", "", true, "Node"},
		{"/api/v1/namespaces/payments/pods", "pods", "payments", false, "List"},
		{"/api/v1/namespaces/payments/pods/checkout-api-0", "pods", "payments", true, "Pod"},
		{"/apis/apps/v1/namespaces/payments/deployments", "deployments", "payments", false, "List"},
		{"/apis/apps/v1/namespaces/payments/deployments/checkout-api", "deployments", "payments", true, "Deployment"},
	} {
		t.Run(fixture.path, func(t *testing.T) {
			if resourceForPath(fixture.path) != fixture.resource || namespaceForPath(fixture.path) != fixture.namespace || isItemPath(fixture.path) != fixture.item {
				t.Errorf("routing resource=%q namespace=%q item=%t", resourceForPath(fixture.path), namespaceForPath(fixture.path), isItemPath(fixture.path))
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixture.path, nil))
			var body struct {
				Kind string `json:"kind"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Kind != fixture.kind {
				t.Fatalf("status=%d body=%s, want kind=%s", response.Code, response.Body.String(), fixture.kind)
			}
		})
	}
}

func TestServerDiscoveryMetadataAndFaultCounters(t *testing.T) {
	server, err := NewServer(Config{Scenario: "helm", Faults: map[string]Fault{"pods": {StatusCode: http.StatusTooManyRequests, Remaining: 1, RetryAfter: 2 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := httpServer.Client()

	response, err := client.Get(httpServer.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("discovery status=%d", response.StatusCode)
	}

	response, err = client.Get(httpServer.URL + "/api/v1/namespaces/shop/pods")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("injected response status=%d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/namespaces/shop/secrets?labelSelector=owner%3Dhelm", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), "secret-canary") || strings.Contains(string(body), "release\"") {
		t.Fatalf("metadata response status=%d body=%s", response.StatusCode, body)
	}
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Kind != "PartialObjectMetadataList" || len(list.Items) != 2 {
		t.Fatalf("metadata list=%#v err=%v", list, err)
	}

	response, err = client.Get(httpServer.URL + "/_fake/counters")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var counters []Counter
	if err := json.NewDecoder(response.Body).Decode(&counters); err != nil {
		t.Fatal(err)
	}
	foundMetadata := false
	for _, counter := range counters {
		if counter.Path == "/api/v1/namespaces/shop/secrets" && strings.Contains(counter.Accept, "PartialObjectMetadataList") {
			foundMetadata = counter.Requests == 1 && counter.Bytes > 0
		}
	}
	if !foundMetadata {
		t.Fatalf("metadata request missing from counters: %#v", counters)
	}
}

func TestGenerateFiftyThousandPodsWithinBounds(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	store := NewStore()
	if err := Generate(store, 50000, 2000, 23); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if count := store.Count("pods"); count != 50000 {
		t.Fatalf("generated pod count=%d", count)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("50k pod generation took %s, want less than 5s", elapsed)
	}
	if after.HeapAlloc < before.HeapAlloc || after.HeapAlloc-before.HeapAlloc >= 500<<20 {
		t.Fatalf("50k pod fixture retained %d bytes, want less than 500 MiB", after.HeapAlloc-before.HeapAlloc)
	}
}

func TestDiscoveryOnlyAdvertisesSeededGitOpsResources(t *testing.T) {
	for _, test := range []struct {
		scenario string
		wantCRD  bool
	}{{"triage", false}, {"gitops", true}} {
		server, err := NewServer(Config{Scenario: test.scenario})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apis", nil))
		containsCRD := strings.Contains(response.Body.String(), "argoproj.io")
		if containsCRD != test.wantCRD {
			t.Errorf("scenario %q CRD discovery=%v, want %v", test.scenario, containsCRD, test.wantCRD)
		}
		if test.wantCRD {
			response = httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apis/argoproj.io/v1alpha1", nil))
			if !strings.Contains(response.Body.String(), `"name":"applications"`) || !strings.Contains(response.Body.String(), `"name":"rollouts"`) {
				t.Fatalf("GitOps discovery response=%s", response.Body.String())
			}
		}
	}
}

func TestStaleContinuationReturnsGone(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.Upsert("pods", "payments", "checkout-api-1", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	server.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods?limit=1", nil))
	var page struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || page.Metadata.Continue == "" {
		t.Fatalf("first list=%s err=%v", first.Body.String(), err)
	}
	if err := server.store.Upsert("pods", "payments", "checkout-api-2", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	continued := httptest.NewRecorder()
	server.ServeHTTP(continued, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods?limit=1&continue="+page.Metadata.Continue, nil))
	if continued.Code != http.StatusGone {
		t.Fatalf("stale continuation status=%d body=%s", continued.Code, continued.Body.String())
	}
}

func TestWatchBookmarkExpiredVersionAndLimitedRBAC(t *testing.T) {
	server, err := NewServer(Config{Scenario: "limited-rbac"})
	if err != nil {
		t.Fatal(err)
	}
	bookmark := httptest.NewRecorder()
	server.ServeHTTP(bookmark, httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=1&allowWatchBookmarks=true", nil))
	var event struct {
		Type string `json:"type"`
	}
	if bookmark.Code != http.StatusOK || json.Unmarshal(bookmark.Body.Bytes(), &event) != nil || event.Type != "BOOKMARK" {
		t.Fatalf("bookmark status=%d body=%s", bookmark.Code, bookmark.Body.String())
	}
	expired := httptest.NewRecorder()
	server.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=1&resourceVersion=expired", nil))
	if expired.Code != http.StatusGone {
		t.Fatalf("expired watch status=%d", expired.Code)
	}
	for _, path := range []string{"/api/v1/nodes", "/api/v1/namespaces/shop/secrets"} {
		denied := httptest.NewRecorder()
		server.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, path, nil))
		if denied.Code != http.StatusForbidden {
			t.Errorf("limited-RBAC path %s status=%d", path, denied.Code)
		}
	}
}

func TestScaleAndFaultScenariosAreDeterministic(t *testing.T) {
	scale, err := NewServer(Config{Scenario: "scale", Pods: 501, Namespaces: 2, Seed: 23})
	if err != nil || scale.store.Count("pods") != 501 || scale.store.Count("namespaces") != 2 {
		t.Fatalf("scale fixture pods=%d namespaces=%d err=%v", scale.store.Count("pods"), scale.store.Count("namespaces"), err)
	}

	partial, err := NewServer(Config{Scenario: "fault-partial-pods", Pods: 501, Namespaces: 1, Seed: 23})
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	partial.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/load-0000/pods?limit=500", nil))
	var page struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil || page.Metadata.Continue == "" {
		t.Fatalf("first scale page status=%d body=%s", first.Code, first.Body.String())
	}
	continued := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/load-0000/pods?limit=500&continue="+page.Metadata.Continue, nil)
	partial.ServeHTTP(continued, request)
	if continued.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial-page fault status=%d body=%s", continued.Code, continued.Body.String())
	}

	forbidden, err := NewServer(Config{Scenario: "fault-forbidden-secret"})
	if err != nil {
		t.Fatal(err)
	}
	secretRequest := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/secrets", nil)
	secretRequest.Header.Set("Accept", "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	secretResponse := httptest.NewRecorder()
	forbidden.ServeHTTP(secretResponse, secretRequest)
	if secretResponse.Code != http.StatusForbidden {
		t.Fatalf("forbidden metadata scenario status=%d", secretResponse.Code)
	}
}

func TestTableAcceptReturnsKubernetesTable(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods", nil)
	request.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"kind":"Table"`) {
		t.Fatalf("table response status=%d body=%s", response.Code, response.Body.String())
	}
}
