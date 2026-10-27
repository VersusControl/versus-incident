package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type issueAlertChannelResolver bool

func (enabled issueAlertChannelResolver) ResolveAlert(_ context.Context, base *config.AlertConfig) bool {
	*base = config.AlertConfig{}
	base.Slack.Enable = bool(enabled)
	return true
}

func TestIssueAlertsRequiresEnabledRuntimeChannel(t *testing.T) {
	previousGatewayAuth := middleware.GatewayAuthEnabled()
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(previousGatewayAuth); config.SetAlertConfigResolver(nil) })
	app := gatewaySessionTestApp(t)
	service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	for _, test := range []struct {
		channels, enabled bool
		status            int
	}{{false, true, 400}, {false, false, 200}, {true, true, 200}} {
		config.SetAlertConfigResolver(issueAlertChannelResolver(test.channels))
		body := `{"enabled":false,"level":"warning"}`
		if test.enabled {
			body = `{"enabled":true,"level":"warning"}`
		}
		request := httptest.NewRequest(http.MethodPut, "/api/admin/kubernetes/issue-alerts?cluster=cluster", strings.NewReader(body))
		request.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("channels=%v enabled=%v: status=%d want=%d", test.channels, test.enabled, response.StatusCode, test.status)
		}
	}
}

func TestIssueAlertsCommunityGatewayWithoutPermissionInjector(t *testing.T) {
	// A previous test may consume the singleton load with another gateway
	// secret. Use the existing restoring session fixture rather than assuming
	// LoadConfig replaces that earlier configuration.
	previousGatewayAuth := middleware.GatewayAuthEnabled()
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(previousGatewayAuth) })
	app := gatewaySessionTestApp(t)
	config.SetAlertConfigResolver(issueAlertChannelResolver(true))
	t.Cleanup(func() { config.SetAlertConfigResolver(nil) })
	service := kubernetes.NewService(nil, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		request := httptest.NewRequest(method, "/api/admin/kubernetes/issue-alerts?cluster=cluster", strings.NewReader(`{"enabled":true,"level":"warning"}`))
		request.Header.Set("X-Gateway-Secret", gatewaySessionTestSecret)
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}

		var value map[string]interface{}
		err = json.NewDecoder(response.Body).Decode(&value)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || value["can_write"] != true || value["available"] != true {
			t.Fatalf("%s: %d %+v %v", method, response.StatusCode, value, err)
		}
		if method == http.MethodPut && (value["enabled"] != true || value["level"] != "warning" || value["enabled_at"] == "") {
			t.Fatal(value)
		}
		for _, key := range []string{"enabled", "level", "enabled_at", "can_write", "available", "reason", "links_ready", "channels_enabled"} {
			if _, ok := value[key]; !ok {
				t.Errorf("missing %s", key)
			}
		}
	}
}
