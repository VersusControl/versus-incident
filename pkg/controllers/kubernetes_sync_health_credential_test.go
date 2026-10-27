package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/gofiber/fiber/v2"
)

// flippedCredential is a connector identity that works and then dies: its
// authorization starts failing, which is what a retirement or an expiry does to a
// service that was reading the cluster a moment ago.
type flippedCredential struct{ dead atomic.Bool }

func (credential *flippedCredential) Authorization(context.Context) (string, error) {
	if credential.dead.Load() {
		return "", errors.New("identity canary-revoked-9c41 is no longer accepted")
	}
	return "test-authorization-placeholder", nil
}

func (credential *flippedCredential) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

// inventoryRecordProvider serves the cluster's stored inventory metadata for the
// scope key the worker derived, so a test can state what the durable record
// already holds.
type inventoryRecordProvider struct {
	storage.Provider
	record string
}

func (provider inventoryRecordProvider) ReadBlob(name string) ([]byte, error) {
	if strings.HasPrefix(name, "kubernetes/inventory/") {
		return []byte(provider.record), nil
	}
	return provider.Provider.ReadBlob(name)
}

// syncHealthCluster is one configured cluster behind an admin app, with a
// credential the test can kill.
type syncHealthCluster struct {
	app        *fiber.App
	credential *flippedCredential
}

func newSyncHealthCluster(t *testing.T, record string, controller func(*kubernetes.Service) *KubernetesAdminController) *syncHealthCluster {
	t.Helper()
	credential := &flippedCredential{}
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "https://cluster.example.invalid:6443", Credentials: credential})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(inventoryRecordProvider{Provider: storage.NewMemory(), record: record})

	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), ctx.Get("X-View") != "deny")
		ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), core.CallerAuthorization{
			Authenticated: true,
			Permissions:   map[core.Permission]bool{core.PermissionInfrastructureView: true},
			Clusters:      &core.ClusterScope{IDs: []string{"cluster"}},
		}))
		return ctx.Next()
	})
	controller(service).Register(app.Group("/api"))
	return &syncHealthCluster{app: app, credential: credential}
}

func (cluster *syncHealthCluster) read(t *testing.T, query, view string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/sync?"+query, nil)
	if view != "" {
		request.Header.Set("X-View", view)
	}
	response, err := cluster.app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// syncHealthReportBody is the part of the payload a surface reads to decide
// whether a cluster ever worked.
type syncHealthReportBody struct {
	State      string `json:"state"`
	Partial    bool   `json:"partial"`
	Class      string `json:"class"`
	FirstReady string `json:"first_ready_at"`
	TotalKinds int    `json:"total_kinds"`
	Failing    int    `json:"failing_kinds"`
}

func decodeSyncHealthReport(t *testing.T, body []byte) syncHealthReportBody {
	t.Helper()
	var report syncHealthReportBody
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return report
}

// A retired credential must not lose the cluster's durable first ready moment:
// the sync route answers with the report, and the report says error in a closed
// vocabulary a surface already knows. Without this a reload has no report at all
// and a cached one keeps the cluster looking healthy while every read fails.
func TestKubernetesSyncHealthKeepsFirstReadyWhenTheCredentialDies(t *testing.T) {
	stamped := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	record := `{"FirstReadyAt":"` + stamped.Format(time.RFC3339Nano) + `"}`
	for _, test := range []struct {
		name       string
		configured func(*kubernetes.Service) *KubernetesAdminController
	}{
		{name: "one configured service", configured: NewKubernetesAdminController},
		{name: "cluster registry", configured: func(service *kubernetes.Service) *KubernetesAdminController {
			return NewKubernetesAdminControllerWithRegistry(kubernetes.NewServiceRegistry(service))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster := newSyncHealthCluster(t, record, test.configured)

			// Before the fault the route answers with the ordinary report.
			status, body := cluster.read(t, "cluster=cluster", "")
			if status != fiber.StatusOK {
				t.Fatalf("live credential status=%d body=%s", status, body)
			}
			if report := decodeSyncHealthReport(t, body); report.Class != "" || report.State == "error" {
				t.Fatalf("live report = %+v, want an ordinary report with no failure class", report)
			}

			cluster.credential.dead.Store(true)
			status, body = cluster.read(t, "cluster=cluster", "")
			if status != fiber.StatusOK {
				t.Fatalf("dead credential status=%d body=%s, want the report itself", status, body)
			}
			report := decodeSyncHealthReport(t, body)
			if report.State != "error" || !report.Partial || report.Class != "unauthorized" {
				t.Fatalf("dead report = state %q partial %t class %q, want an unauthorized error", report.State, report.Partial, report.Class)
			}
			if report.FirstReady != stamped.Format(time.RFC3339Nano) {
				t.Fatalf("dead report first_ready_at = %q, want the recorded %s", report.FirstReady, stamped.Format(time.RFC3339Nano))
			}
			if report.TotalKinds == 0 || report.Failing != report.TotalKinds {
				t.Fatalf("dead report covers %d kinds of %d failing", report.Failing, report.TotalKinds)
			}
			// The report is a report, not a dump: no credential, identity, or
			// endpoint reaches it.
			for _, canary := range []string{"canary-revoked-9c41", "test-authorization-placeholder", "cluster.example.invalid"} {
				if strings.Contains(string(body), canary) {
					t.Fatalf("report carries %q: %s", canary, body)
				}
			}
			// Reading it again answers the same way: the marker is local state, so
			// it neither disappears nor moves while the credential is dead.
			if _, again := cluster.read(t, "cluster=cluster", ""); decodeSyncHealthReport(t, again).FirstReady != stamped.Format(time.RFC3339Nano) {
				t.Fatalf("repeated dead report = %s", again)
			}
		})
	}
}

// A failure must not fabricate a marker for a cluster that was never ready, and
// the route keeps its authorization and cluster allow-list while answering.
func TestKubernetesSyncHealthDeadCredentialNeverFabricatesFirstReady(t *testing.T) {
	cluster := newSyncHealthCluster(t, "", NewKubernetesAdminController)
	cluster.credential.dead.Store(true)

	status, body := cluster.read(t, "cluster=cluster", "")
	if status != fiber.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	report := decodeSyncHealthReport(t, body)
	if report.State != "error" || report.Class != "unauthorized" || report.FirstReady != "" {
		t.Fatalf("report = %+v, want an unauthorized error with no first ready time", report)
	}
	if strings.Contains(string(body), "first_ready_at") {
		t.Fatalf("a never ready cluster rendered a first ready time: %s", body)
	}
	if status, body := cluster.read(t, "cluster=other", ""); status != fiber.StatusNotFound {
		t.Fatalf("cluster outside the allow-list status=%d body=%s", status, body)
	}
	if status, body := cluster.read(t, "cluster=cluster", "deny"); status != fiber.StatusForbidden {
		t.Fatalf("missing permission status=%d body=%s", status, body)
	}
}

// kubernetesPermissionApp is the admin surface behind an authenticated caller
// whose infrastructure:view permission the test decides with the X-View header.
func kubernetesPermissionApp(t *testing.T, controller *KubernetesAdminController) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), ctx.Get("X-View") != "deny")
		ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), core.CallerAuthorization{
			Authenticated: true,
			Permissions:   map[core.Permission]bool{core.PermissionInfrastructureView: true},
			Clusters:      &core.ClusterScope{IDs: []string{"cluster"}},
		}))
		return ctx.Next()
	})
	controller.Register(app.Group("/api"))
	return app
}

// kubernetesRouteProbes lists every route the admin surface serves, with its
// parameters filled in, so a permission answer can be checked across the whole
// group instead of on a sample of it.
func kubernetesRouteProbes(t *testing.T, app *fiber.App) []kubernetesRouteProbe {
	t.Helper()
	var probes []kubernetesRouteProbe
	for _, routes := range app.Stack() {
		for _, route := range routes {
			if route.Method == "USE" || !strings.HasPrefix(route.Path, "/api/admin/kubernetes/") {
				continue
			}
			segments := strings.Split(route.Path, "/")
			for index, segment := range segments {
				if strings.HasPrefix(segment, ":") {
					segments[index] = "probe"
				}
			}
			probes = append(probes, kubernetesRouteProbe{method: route.Method, path: strings.Join(segments, "/")})
		}
	}
	return probes
}

type kubernetesRouteProbe struct {
	method string
	path   string
}

func kubernetesAnswer(t *testing.T, app *fiber.App, method, path, view string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if view != "" {
		request.Header.Set("X-View", view)
	}
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return response.StatusCode, body
}

func kubernetesAnswerCode(t *testing.T, body []byte) string {
	t.Helper()
	var answer struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return answer.Code
}

// Presence and credential state belong to the deployment, so a caller that lacks
// the permission must get the same refusal whatever they are: one answer for a
// missing connector, a live one, and one whose identity died, on every route of
// the surface including the health report.
func TestKubernetesPermissionIsAnsweredBeforeConnectorState(t *testing.T) {
	noConnector := kubernetesPermissionApp(t, NewKubernetesAdminController(nil))
	live := newSyncHealthCluster(t, "", NewKubernetesAdminController)
	dead := newSyncHealthCluster(t, "", NewKubernetesAdminController)
	dead.credential.dead.Store(true)

	for _, deployment := range []struct {
		name string
		app  *fiber.App
	}{
		{name: "no connector configured", app: noConnector},
		{name: "configured with a live credential", app: live.app},
		{name: "configured with a dead credential", app: dead.app},
	} {
		t.Run(deployment.name, func(t *testing.T) {
			probes := kubernetesRouteProbes(t, deployment.app)
			covered := map[string]bool{}
			for _, probe := range probes {
				covered[probe.path] = true
			}
			for _, required := range []string{"/api/admin/kubernetes/sync", "/api/admin/kubernetes/clusters", "/api/admin/kubernetes/overview", "/api/admin/kubernetes/resources/probe/probe", "/api/admin/kubernetes/workloads/probe/probe/logs"} {
				if !covered[required] {
					t.Fatalf("%s is not in the enumerated surface, so the sweep does not prove the group", required)
				}
			}
			for _, probe := range probes {
				for _, cluster := range []string{"", "?cluster=cluster", "?cluster=other"} {
					path := probe.path + cluster
					status, body := kubernetesAnswer(t, deployment.app, probe.method, path, "deny")
					if status != fiber.StatusForbidden {
						t.Errorf("%s %s without the permission status=%d body=%s, want %d", probe.method, path, status, body, fiber.StatusForbidden)
						continue
					}
					for _, leak := range []string{"connector_not_configured", "connector_credential_unresolved", "cluster_not_found"} {
						if strings.Contains(string(body), leak) {
							t.Errorf("%s %s without the permission answered %q: %s", probe.method, path, leak, body)
						}
					}
				}
			}
		})
	}
}

// The gate order only changes the answer for a caller that is refused: every
// answer a permitted caller gets stays what it was, including the report across
// a dead credential.
func TestKubernetesPermittedCallerStatusesAreUnchanged(t *testing.T) {
	noConnector := kubernetesPermissionApp(t, NewKubernetesAdminController(nil))
	for _, path := range []string{"/api/admin/kubernetes/clusters", "/api/admin/kubernetes/overview", "/api/admin/kubernetes/sync"} {
		status, body := kubernetesAnswer(t, noConnector, http.MethodGet, path, "")
		if status != fiber.StatusServiceUnavailable || kubernetesAnswerCode(t, body) != "connector_not_configured" {
			t.Errorf("%s with no connector status=%d code=%q body=%s", path, status, kubernetesAnswerCode(t, body), body)
		}
	}

	stamped := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	live := newSyncHealthCluster(t, `{"FirstReadyAt":"`+stamped.Format(time.RFC3339Nano)+`"}`, NewKubernetesAdminController)
	status, body := kubernetesAnswer(t, live.app, http.MethodGet, "/api/admin/kubernetes/sync?cluster=cluster", "")
	if status != fiber.StatusOK {
		t.Errorf("sync with a live credential status=%d body=%s", status, body)
	}
	for _, path := range []string{"/api/admin/kubernetes/overview?cluster=other", "/api/admin/kubernetes/sync?cluster=other"} {
		status, body := kubernetesAnswer(t, live.app, http.MethodGet, path, "")
		if status != fiber.StatusNotFound {
			t.Errorf("%s outside the allow-list status=%d body=%s", path, status, body)
		}
	}

	// The registry constructor validates the credential during resolution, so the
	// report has to come from the configured-service fallback rather than from a
	// resolution that never looks at the identity.
	dead := newSyncHealthCluster(t, "", func(service *kubernetes.Service) *KubernetesAdminController {
		return NewKubernetesAdminControllerWithRegistry(kubernetes.NewServiceRegistry(service))
	})
	dead.credential.dead.Store(true)
	status, body = kubernetesAnswer(t, dead.app, http.MethodGet, "/api/admin/kubernetes/overview?cluster=cluster", "")
	if status != fiber.StatusServiceUnavailable || kubernetesAnswerCode(t, body) != "connector_credential_unresolved" {
		t.Errorf("overview with a dead credential status=%d code=%q body=%s", status, kubernetesAnswerCode(t, body), body)
	}
	status, body = kubernetesAnswer(t, dead.app, http.MethodGet, "/api/admin/kubernetes/sync?cluster=cluster", "")
	if status != fiber.StatusOK {
		t.Fatalf("sync with a dead credential status=%d body=%s, want the report", status, body)
	}
	if report := decodeSyncHealthReport(t, body); report.State != "error" || report.Class != "unauthorized" {
		t.Errorf("sync with a dead credential report = %+v, want an unauthorized error", report)
	}
	if status, body := kubernetesAnswer(t, dead.app, http.MethodGet, "/api/admin/kubernetes/sync?cluster=other", ""); status != fiber.StatusNotFound {
		t.Errorf("sync outside the allow-list status=%d body=%s", status, body)
	}

	// A cluster the caller's scope excludes stays not found even though the
	// deployment has it configured and its credential is fine: the allow-list is
	// answered before the report is.
	scopedClient, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "https://cluster.example.invalid:6443", Credentials: &flippedCredential{}})
	if err != nil {
		t.Fatal(err)
	}
	visible := kubernetes.NewService(scopedClient, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	visible.SetChangeStorage(inventoryRecordProvider{Provider: storage.NewMemory(), record: `{"FirstReadyAt":"` + stamped.Format(time.RFC3339Nano) + `"}`})
	hiddenService := kubernetes.NewService(scopedClient, kubernetes.Scope{OrgID: "default", ClusterID: "hidden"}, 0)
	hiddenService.SetChangeStorage(storage.NewMemory())
	scoped := kubernetesPermissionApp(t, NewKubernetesAdminControllerWithRegistry(kubernetes.NewClusterRegistry(true, []kubernetes.ClusterEntry{
		{Info: kubernetes.ClusterInfo{ID: "cluster"}, Service: visible},
		{Info: kubernetes.ClusterInfo{ID: "hidden"}, Service: hiddenService},
	})))
	status, body = kubernetesAnswer(t, scoped, http.MethodGet, "/api/admin/kubernetes/sync?cluster=hidden", "")
	if status != fiber.StatusNotFound || kubernetesAnswerCode(t, body) != "cluster_not_found" {
		t.Errorf("configured cluster outside the scope status=%d code=%q body=%s", status, kubernetesAnswerCode(t, body), body)
	}
	status, body = kubernetesAnswer(t, scoped, http.MethodGet, "/api/admin/kubernetes/sync?cluster=cluster", "")
	if status != fiber.StatusOK {
		t.Errorf("in-scope cluster status=%d body=%s", status, body)
	}
}

func TestKubernetesSyncHealthRevokedCredentialKeepsRequestOrg(t *testing.T) {
	for _, query := range []string{"", "cluster=cluster"} {
		for _, publications := range []struct{ base, own bool }{{true, false}, {true, true}, {false, false}} {
			t.Run(query+"/publications="+fmt.Sprint(publications), func(t *testing.T) {
				credential := &flippedCredential{}
				client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: "https://cluster.example.invalid:6443", Credentials: credential})
				if err != nil {
					t.Fatal(err)
				}
				base := kubernetes.NewService(client, kubernetes.Scope{OrgID: "org-a", ClusterID: "cluster"}, 0)
				provider := storage.NewMemory()
				base.SetChangeStorage(provider)
				stamped := time.Now().UTC().Add(-2 * time.Hour)
				own := stamped.Add(time.Hour)
				seed := func(org string, at time.Time) {
					scope := base.ActiveScope()
					hash := sha256.Sum256([]byte(org + "\x00" + scope.ClusterID + "\x00" + scope.CredentialID))
					// An existing snapshot supplies both the publication and age.
					raw, err := json.Marshal(map[string]any{"FirstReadyAt": at, "View": map[string]any{"Kinds": map[string]any{"Pod": map[string]any{"observed_at": at}}}})
					if err != nil {
						t.Fatal(err)
					}
					if err := provider.WriteBlob("kubernetes/inventory/"+base64.RawURLEncoding.EncodeToString(hash[:]), raw); err != nil {
						t.Fatal(err)
					}
				}
				if publications.base {
					seed("org-a", stamped)
				}
				seed("default", stamped.Add(-time.Hour))
				if publications.own {
					seed("org-b", own)
				}
				registry := kubernetes.NewClusterRegistryWithScope(false, []kubernetes.ClusterEntry{{Info: kubernetes.ClusterInfo{ID: "cluster"}, Service: base}}, tenancy.NewOrgScope("org-a", "default"))
				app := fiber.New(fiber.Config{Immutable: true})
				app.Use(func(ctx *fiber.Ctx) error {
					middleware.MarkAuthorized(ctx)
					ctx.Locals(middleware.OrgContextKey, strings.Clone(ctx.Get("X-Test-Org")))
					middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), ctx.Get("X-View") != "deny")
					// A request context cannot grant itself the deployment's upgrade fallback.
					ctx.SetUserContext(tenancy.WithOrgScope(ctx.UserContext(), tenancy.NewOrgScope("org-b", "org-a", "default")))
					ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}, Clusters: &core.ClusterScope{IDs: []string{"cluster"}}}))
					return ctx.Next()
				})
				NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
				credential.dead.Store(true)
				read := func(org, path, view string) (int, []byte) {
					request := httptest.NewRequest(http.MethodGet, path, nil)
					request.Header.Set("X-Test-Org", org)
					request.Header.Set("X-View", view)
					response, err := app.Test(request, -1)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					return response.StatusCode, body
				}
				for _, org := range []string{"org-a", "org-b"} {
					status, body := read(org, "/api/admin/kubernetes/sync?"+query, "")
					if status != fiber.StatusOK {
						t.Fatalf("%s status=%d body=%s", org, status, body)
					}
					var report kubernetes.SyncHealth
					if err := json.Unmarshal(body, &report); err != nil {
						t.Fatal(err)
					}
					if report.Class != "unauthorized" || report.State != "error" {
						t.Fatalf("unclosed fault: %s", body)
					}
					want := stamped
					if !publications.base {
						want = stamped.Add(-time.Hour)
					}
					if org == "org-b" {
						want = own
					}
					if org == "org-b" && !publications.own {
						if !report.FirstReadyAt.IsZero() || report.AgeSeconds != nil {
							t.Fatalf("foreign publication/age leaked: %s", body)
						}
					} else if !report.FirstReadyAt.Equal(want) || report.AgeSeconds == nil || *report.AgeSeconds < time.Since(want).Seconds()-2 || *report.AgeSeconds > time.Since(want).Seconds()+2 {
						t.Fatalf("%s publication/age is not its own: %s", org, body)
					}
				}
				for _, path := range []string{"/api/admin/kubernetes/overview?" + query, "/api/admin/kubernetes/workloads?" + query} {
					status, body := read("org-b", path, "")
					if status != fiber.StatusServiceUnavailable || kubernetesAnswerCode(t, body) != "connector_credential_unresolved" {
						t.Fatalf("inventory widened: %d %s", status, body)
					}
				}
				status, body := read("org-b", "/api/admin/kubernetes/sync?"+query, "deny")
				if status != fiber.StatusForbidden {
					t.Fatalf("permission bypass: %d %s", status, body)
				}
				status, body = read("org-b", "/api/admin/kubernetes/sync?cluster=other", "")
				if status != fiber.StatusNotFound {
					t.Fatalf("allow-list bypass: %d %s", status, body)
				}
			})
		}
	}
}
