package controllers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/gofiber/fiber/v2"
)

var auditClusterCharset = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func newSettingsAuditService(t *testing.T, cluster string, store storage.Provider) *kubernetes.Service {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: cluster}, 0)
	service.SetChangeStorage(store)
	t.Cleanup(service.StopCapture)
	return service
}

type settingsAuditRow struct {
	queryCluster string
	event        middleware.AdminAuditEvent
}

func settingsAuditApp(t *testing.T, controller *KubernetesAdminController, clusters []string) (*fiber.App, func() []settingsAuditRow) {
	t.Helper()
	gatewayAuth := middleware.GatewayAuthEnabled()
	middleware.SetGatewayAuthEnabled(false)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(gatewayAuth) })
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), true)
		if len(clusters) > 0 {
			ctx.SetUserContext(core.WithCallerAuthorization(ctx.UserContext(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true, core.PermissionInfrastructureSettingsWrite: true}, Clusters: &core.ClusterScope{IDs: clusters}}))
		}
		return ctx.Next()
	})
	controller.Register(app.Group("/api"))
	var mu sync.Mutex
	var rows []settingsAuditRow
	middleware.SetAdminAuditHook(func(ctx *fiber.Ctx, event middleware.AdminAuditEvent) {
		mu.Lock()
		defer mu.Unlock()
		rows = append(rows, settingsAuditRow{queryCluster: strings.Clone(ctx.Query("cluster")), event: event})
	})
	t.Cleanup(func() { middleware.SetAdminAuditHook(nil) })
	return app, func() []settingsAuditRow {
		mu.Lock()
		defer mu.Unlock()
		return append([]settingsAuditRow(nil), rows...)
	}
}

func putSettings(t *testing.T, app *fiber.App, query, body string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, "/api/admin/kubernetes/agent-settings"+query, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, 10000)
	if err != nil {
		t.Error(err)
		return 0
	}
	response.Body.Close()
	return response.StatusCode
}

// Two clusters saved at the same time each get audit rows naming their own
// cluster and only their own changes, in the order each cluster saved.
func TestKubernetesSettingsAuditConcurrentClustersNameOwnCluster(t *testing.T) {
	store := storage.NewMemory()
	flags := map[string]string{"east": "logs", "west": "metrics"}
	var entries []kubernetes.ClusterEntry
	for _, cluster := range []string{"east", "west"} {
		entries = append(entries, kubernetes.ClusterEntry{Info: kubernetes.ClusterInfo{ID: cluster, Provider: "generic"}, Service: newSettingsAuditService(t, cluster, store)})
	}
	app, rows := settingsAuditApp(t, NewKubernetesAdminControllerWithRegistry(kubernetes.NewClusterRegistry(true, entries)), []string{"east", "west"})
	const saves = 20
	var wg sync.WaitGroup
	for _, cluster := range []string{"east", "west"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range saves {
				body := fmt.Sprintf(`{"learning":{%q:%t}}`, flags[cluster], i%2 == 0)
				if status := putSettings(t, app, "?cluster="+cluster, body); status != http.StatusOK {
					t.Errorf("%s save %d: status %d", cluster, i, status)
				}
			}
		}()
	}
	wg.Wait()
	byCluster := map[string][]string{}
	for _, row := range rows() {
		if row.event.Action != "kubernetes.agent-settings.changed" || row.event.Result != middleware.AdminAuditSuccess {
			t.Fatalf("unexpected audit row %+v", row)
		}
		byCluster[row.queryCluster] = append(byCluster[row.queryCluster], row.event.Target)
	}
	for cluster, flag := range flags {
		targets := byCluster[cluster]
		if len(targets) != saves {
			t.Fatalf("%s: %d audit rows, want %d: %q", cluster, len(targets), saves, targets)
		}
		for i, target := range targets {
			want := "kubernetes:cluster:" + cluster + " learning." + flag + ":false->true"
			if i%2 == 1 {
				want = "kubernetes:cluster:" + cluster + " learning." + flag + ":true->false"
			}
			if target != want {
				t.Fatalf("%s save %d: target %q, want %q", cluster, i, target, want)
			}
		}
	}
}

func TestKubernetesSettingsAuditClusterIDIsSanitisedAndCapped(t *testing.T) {
	cases := map[string]string{
		"two-byte unicode":   strings.Repeat("é", 2000),
		"three-byte unicode": strings.Repeat("名", 2000),
		"slashes":            strings.Repeat("a/b\\", 500),
		"layout breakers":    strings.Repeat(" \n\t:,+", 400),
		"split rune at cap":  strings.Repeat("x", 127) + strings.Repeat("名", 600),
		"invalid utf8":       strings.Repeat("\xff\xfe", 1000),
		"plain 2000":         strings.Repeat("c", 2000),
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: "default", ClusterID: id}, 0)
			for _, saved := range []bool{false, true} {
				target := kubernetesSettingsAuditTarget(service, []string{"learning.logs:false->true"}, saved)
				if !strings.HasPrefix(target, "kubernetes:cluster:") {
					t.Fatalf("target %q", target)
				}
				cluster, rest, _ := strings.Cut(strings.TrimPrefix(target, "kubernetes:cluster:"), " ")
				if len(cluster) == 0 || len(cluster) > maxSettingsAuditCluster || !utf8.ValidString(cluster) || !auditClusterCharset.MatchString(cluster) {
					t.Fatalf("cluster %q (%d bytes) not sanitised and capped", cluster, len(cluster))
				}
				if saved && rest != "learning.logs:false->true" || !saved && rest != "" {
					t.Fatalf("saved=%t target %q lost its layout", saved, target)
				}
			}
		})
	}
}

// A long unicode cluster ID goes through the real save path and the row still
// keeps its layout. The save is written; the read-back that builds the response
// may refuse such an ID, so the response status is not asserted.
func TestKubernetesSettingsAuditLongUnicodeClusterThroughSave(t *testing.T) {
	id := strings.Repeat("集群/", 700)
	service := newSettingsAuditService(t, id, storage.NewMemory())
	app, rows := settingsAuditApp(t, NewKubernetesAdminController(service), nil)
	putSettings(t, app, "", `{"learning":{"traces":true}}`)
	got := rows()
	if len(got) != 1 {
		t.Fatalf("audit rows %+v", got)
	}
	cluster, rest, ok := strings.Cut(strings.TrimPrefix(got[0].event.Target, "kubernetes:cluster:"), " ")
	if !ok || rest != "learning.traces:false->true" || len(cluster) > maxSettingsAuditCluster || !auditClusterCharset.MatchString(cluster) {
		t.Fatalf("target %q", got[0].event.Target)
	}
}

func TestKubernetesSettingsAuditChangeListCap(t *testing.T) {
	service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: "default", ClusterID: "cluster-a"}, 0)
	names := func(count int) []string {
		changes := make([]string, count)
		for i := range changes {
			changes[i] = "setting." + strconv.Itoa(i) + ":changed"
		}
		return changes
	}
	for _, tc := range []struct {
		count int
		more  string
	}{
		{1, ""},
		{maxSettingsAuditChanges, ""},
		{maxSettingsAuditChanges + 1, ",+1 more"},
		{40, ",+8 more"},
	} {
		t.Run(strconv.Itoa(tc.count), func(t *testing.T) {
			changes := names(tc.count)
			input := append([]string(nil), changes...)
			target := kubernetesSettingsAuditTarget(service, changes, true)
			kept := min(tc.count, maxSettingsAuditChanges)
			want := "kubernetes:cluster:cluster-a " + strings.Join(input[:kept], ",") + tc.more
			if target != want {
				t.Fatalf("target\n got %q\nwant %q", target, want)
			}
			if strings.Join(changes, ",") != strings.Join(input, ",") {
				t.Fatal("caller's change list was modified")
			}
		})
	}
	long := make([]string, 40)
	for i := range long {
		long[i] = strings.Repeat("n", 100)
	}
	if target := kubernetesSettingsAuditTarget(service, long, true); len(target) > maxSettingsAuditTarget {
		t.Fatalf("target not capped: %d bytes", len(target))
	}
}
