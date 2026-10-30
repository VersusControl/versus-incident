package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
)

func TestEnrichedIssuesHTTPTruncatedHistoryRemainsPartial(t *testing.T) {
	gateway := middleware.GatewayAuthEnabled()
	middleware.SetGatewayAuthEnabled(false)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(gateway) })
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(resource, namespace, name string, object map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"operation": "upsert", "resource": resource, "namespace": namespace, "name": name, "object": object})
		recorder := httptest.NewRecorder()
		fixture.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(string(body))))
		if recorder.Code != http.StatusOK {
			t.Fatalf("synthetic fixture upsert: %d", recorder.Code)
		}
	}
	mutate("namespaces", "", "kube-system", map[string]any{"kind": "Namespace", "metadata": map[string]any{"uid": "enrichment-physical"}})
	mutate("namespaces", "", "other", map[string]any{"kind": "Namespace"})
	for i := 0; i < 24; i++ {
		ns := "payments"
		if i%2 == 0 {
			ns = "other"
		}
		mutate("pods", ns, fmt.Sprintf("synthetic-%02d", i), map[string]any{
			"kind": "Pod", "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "app"}}},
			"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 3, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}},
		})
	}
	var denyProof atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") && strings.Contains(r.URL.Path, "/namespaces/payments/") && (strings.HasSuffix(r.URL.Path, "/statefulsets") || strings.HasSuffix(r.URL.Path, "/daemonsets")) {
			if denyProof.Load() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "opaque-version"}, "items": nil})
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, time.Hour)
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	registry := kubernetes.NewServiceRegistry(service)
	defer registry.Close()
	registry.StartCapture(t.Context())
	deadline := time.Now().Add(30 * time.Second)
	for {
		report, err := service.SyncHealth(t.Context())
		if err == nil && report.State == "ready" && report.PendingKinds == 0 && !report.FirstReadyAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic autonomous capture did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var frameMu sync.Mutex
	frames := []string{}
	app := fiber.New(fiber.Config{Immutable: true, DisableStartupMessage: true})
	app.Use(fiberrecover.New(fiberrecover.Config{EnableStackTrace: true, StackTraceHandler: func(_ *fiber.Ctx, _ interface{}) {
		pcs := make([]uintptr, 48)
		n := runtime.Callers(2, pcs)
		iterator := runtime.CallersFrames(pcs[:n])
		frameMu.Lock()
		defer frameMu.Unlock()
		for {
			frame, more := iterator.Next()
			if strings.Contains(frame.Function, "/pkg/kubernetes.") || strings.Contains(frame.Function, "/pkg/controllers.") {
				frames = append(frames, fmt.Sprintf("%s %s:%d", frame.Function, filepath.Base(frame.File), frame.Line))
			}
			if !more {
				break
			}
		}
	}}))
	app.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		middleware.SetRequestPermission(c, string(core.PermissionInfrastructureView), true)
		return c.Next()
	})
	NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
	request := func(path string) (int, kubernetes.IssuePage) {
		t.Helper()
		response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), 15000)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var page kubernetes.IssuePage
		if response.StatusCode == http.StatusOK && json.NewDecoder(response.Body).Decode(&page) != nil {
			t.Fatal("successful Issues response is not a JSON page")
		}
		return response.StatusCode, page
	}
	base := "/api/admin/kubernetes/issues?cluster=cluster&broken=true"
	status, plain := request(base + "&limit=50")
	if status != http.StatusOK || len(plain.Items) < 20 {
		t.Fatalf("large cross-namespace current Issues fixture missing: status=%d items=%d", status, len(plain.Items))
	}
	now := time.Now().UTC()
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "cluster", ClusterUID: "enrichment-physical"}
	store := provider.(storage.EvidenceStore)
	batch := storage.EvidenceBatch{ID: "capture:synthetic-enrichment-history", At: now, Epoch: "synthetic"}
	for i := 0; i < 501; i++ {
		namespace := "other"
		if i == 500 {
			namespace = "payments"
		}
		change := kubechanges.Change{ID: fmt.Sprintf("synthetic-change-%03d", i), Cluster: "cluster", Kind: "Pod", Namespace: namespace, Name: "synthetic", UID: fmt.Sprintf("synthetic-history-%03d", i), Type: kubechanges.ImageChanged, At: now.Add(-time.Second)}
		raw, _ := json.Marshal(change)
		batch.Records = append(batch.Records, storage.EvidenceRecord{ID: change.ID, Type: "change", Kind: change.Kind, Namespace: change.Namespace, UID: change.UID, At: change.At, ObservedAt: now, Epoch: "synthetic", Payload: raw})
	}
	for attempt := 0; attempt < 8; attempt++ {
		checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		batch.Expected, batch.Generation = checkpoint.ID, checkpoint.Generation
		if batch.At.Before(checkpoint.At) {
			batch.At = checkpoint.At
		}
		err = store.CommitEvidence(t.Context(), scope, batch)
		if err == nil {
			break
		}
		if err != storage.ErrEvidenceConflict || attempt == 7 {
			t.Fatal(err)
		}
	}
	history, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now.Add(time.Minute), Limit: 500})
	if err != nil || history.Next == "" {
		t.Fatalf("successful bounded history must have continuation: next=%t err=%v", history.Next != "", err)
	}
	for _, limit := range []string{"5", "50", ""} {
		t.Run("all-namespace-limit-"+limit, func(t *testing.T) {
			path := base + "&include=changes"
			if limit != "" {
				path += "&limit=" + limit
			}
			status, page := request(path)
			if status != http.StatusOK {
				frameMu.Lock()
				safeFrames := append([]string(nil), frames...)
				frameMu.Unlock()
				t.Fatalf("enriched Issues status=%d; frame-only panic evidence=%v", status, safeFrames)
			}
			if !page.Sync.Partial || len(page.Items) == 0 || page.Broken.Total != plain.Broken.Total {
				t.Fatalf("bounded history changed current issues/totals or claimed complete: items=%d total=%d partial=%t", len(page.Items), page.Broken.Total, page.Sync.Partial)
			}
			found := false
			for _, partial := range page.Partial {
				found = found || partial.ResourceID == "kubernetes.history" && partial.Class == "budget_exhausted"
			}
			if !found {
				t.Fatal("history continuation was not reported as bounded incomplete enrichment")
			}
			for _, issue := range page.Items[:min(5, len(page.Items))] {
				if !issue.Partial || issue.History == "complete" {
					t.Fatal("bounded enrichment falsely claimed complete per-root history")
				}
			}
			if limit == "5" && (len(page.Items) != 5 || page.Next == "") {
				t.Fatal("enrichment changed current Issues page limit/continuation")
			}
		})
	}
	status, selected := request(base + "&namespace=payments&include=changes&limit=50")
	if status != http.StatusOK || len(selected.Items) == 0 {
		t.Fatalf("selected namespace enrichment: status=%d items=%d", status, len(selected.Items))
	}
	for _, partial := range selected.Partial {
		if partial.ResourceID == "kubernetes.history" {
			t.Fatalf("complete small selected history unavailable: class=%s", partial.Class)
		}
	}
	rootUID := ""
	for _, issue := range plain.Items {
		if issue.Root.Namespace == "payments" {
			rootUID = issue.Root.UID
			break
		}
	}
	if rootUID == "" {
		t.Fatal("selected namespace root fixture missing")
	}
	windows := []struct {
		name, query string
		empty       bool
	}{
		{name: "default"},
		{name: "recent", query: "&since=" + now.Add(-time.Hour).Format(time.RFC3339Nano) + "&until=" + now.Add(time.Minute).Format(time.RFC3339Nano)},
		{name: "empty-authorized", query: "&since=" + now.Add(-48*time.Hour).Format(time.RFC3339Nano) + "&until=" + now.Add(-47*time.Hour).Format(time.RFC3339Nano), empty: true},
	}
	for _, namespace := range []string{"", "payments"} {
		for _, endpoint := range []string{"issues", "timeline", "changes", "crashes"} {
			for _, window := range windows {
				for _, limit := range []string{"", "1", "5", "20", "50", "100"} {
					t.Run(fmt.Sprintf("root-%s/namespace-%s/%s/limit-%s", endpoint, namespace, window.name, limit), func(t *testing.T) {
						path := "/api/admin/kubernetes/" + endpoint + "?cluster=cluster&root_uid=" + rootUID + window.query
						if endpoint == "issues" {
							path += "&include=changes"
						}
						if namespace != "" {
							path += "&namespace=" + namespace
						}
						if limit != "" {
							path += "&limit=" + limit
						}
						response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), 15000)
						if err != nil {
							t.Fatal(err)
						}
						defer response.Body.Close()
						var page struct {
							Items []json.RawMessage `json:"items"`
						}
						if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&page) != nil {
							t.Fatalf("authorized root history must be a successful JSON page: status=%d", response.StatusCode)
						}
						if window.empty && len(page.Items) != 0 {
							t.Fatalf("empty authorized time window returned %d records", len(page.Items))
						}
					})
				}
			}
		}
	}
	denyProof.Store(true)
	status, denied := request(base + "&namespace=payments&include=changes&limit=50")
	foundDenial := false
	for _, partial := range denied.Partial {
		foundDenial = foundDenial || partial.ResourceID == "kubernetes.history" && partial.Class == "forbidden"
	}
	if status != http.StatusOK || !foundDenial {
		t.Fatalf("current Issues must retain the current history authority denial: status=%d denial=%t", status, foundDenial)
	}
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/timeline?cluster=cluster&namespace=payments", nil), 15000)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("dedicated historical data bypassed current permission denial: status=%d", response.StatusCode)
	}
}
