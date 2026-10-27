package controllers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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
