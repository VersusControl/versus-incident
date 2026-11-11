package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestAWSConnectionControllerAndToolSameResult: the settings route and the
// get_aws_connection tool are both answered by settings.Service.Connection.
// The tool's model view is exactly the compaction of the route's DTO at the
// same time, so the UI and the agent never disagree.
func TestAWSConnectionControllerAndToolSameResult(t *testing.T) {
	service := awsTestService(t, "new-account-connect", true,
		config.AWSAccountConfig{AccountID: "111122223333", DisplayName: "Production", Regions: []string{"us-east-1"}},
		config.AWSAccountConfig{AccountID: "444455556666", DisplayName: "Staging", Regions: []string{"eu-west-1"}})
	app, _ := awsTestApp(t, service, map[string]bool{string(core.PermissionInfrastructureView): true})
	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}
	var routeView awssettings.SettingsView
	if err := json.Unmarshal(body, &routeView); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	tool := awstools.New(func() *awssettings.Service { return service }, awstools.WithClock(func() time.Time { return now }))[0]
	result, err := tool.Invoke(core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true,
		Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	fromTool, _ := json.Marshal(result.Data)
	fromRoute, _ := json.Marshal(awstools.ConnectionView(routeView, "", 0, now))
	if !bytes.Equal(fromTool, fromRoute) {
		t.Fatalf("tool and route disagree\ntool:  %s\nroute: %s", fromTool, fromRoute)
	}
	if !result.Found || len(routeView.Accounts) != 2 {
		t.Fatalf("found = %v, accounts = %d", result.Found, len(routeView.Accounts))
	}
}
