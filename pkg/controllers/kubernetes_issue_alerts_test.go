package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestIssueAlertsCommunityGatewayWithoutPermissionInjector(t *testing.T) {
	// A previous test may consume the singleton load with another gateway
	// secret. Use the existing restoring session fixture rather than assuming
	// LoadConfig replaces that earlier configuration.
	previousGatewayAuth := middleware.GatewayAuthEnabled()
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(previousGatewayAuth) })
	app := gatewaySessionTestApp(t)
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
