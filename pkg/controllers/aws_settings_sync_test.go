package controllers

import (
	"net/http"
	"testing"
	"time"

	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestAWSSettingsSyncFieldKeepsContract adds collection progress to the
// settings answer: every existing account field is still present and typed
// as before, and the new sync object carries freshness and per-service
// first-sync progress.
func TestAWSSettingsSyncFieldKeepsContract(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	provider := storage.NewMemory()
	snapshots := snapshot.New(provider, tenancy.DefaultOrgScope())
	service := awssettings.NewService(awssettings.NewStore(provider, tenancy.DefaultOrgScope()),
		awssettings.WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{
				Enable: true, HealthCheckInterval: "5m",
				Budgets:  config.AWSBudgetsConfig{MaxRequestsPerSecond: 100},
				Collect:  config.AWSCollectConfig{Enable: true},
				Accounts: []config.AWSAccountConfig{{AccountID: "111122223333", DisplayName: "Production", Regions: []string{"us-east-1"}}},
			}
		}),
		awssettings.WithHTTPClient(fake.HTTPClient()),
		awssettings.WithBaseProvider(awsTestBase),
		awssettings.WithLimiter(awslimit.NewManager(100)),
		awssettings.WithClock(func() time.Time { return time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC) }),
		awssettings.WithSnapshots(snapshots),
	)
	units := []model.Unit{{Account: "111122223333", Region: "us-east-1", Service: "ec2"}, {Account: "111122223333", Region: "us-east-1", Service: "lambda"}}
	snapshots.RecordPending(units)
	snapshots.Commit(units[0], snapshot.Candidate{})

	app, _ := awsTestApp(t, service, map[string]bool{string(core.PermissionInfrastructureView): true})
	response, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/settings", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d %s", response.StatusCode, body)
	}
	root := decodeAWSJSON(t, body)
	accounts, ok := root["accounts"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts = %v", root["accounts"])
	}
	account := accounts[0].(map[string]any)
	for _, field := range []string{"account_id", "display_name", "source", "status", "disconnected_since", "error_code", "services", "regions", "api_use"} {
		if _, present := account[field]; !present {
			t.Fatalf("account lost %q: %s", field, body)
		}
	}
	awsObject(t, account["services"], "services")
	awsObject(t, account["api_use"], "api_use")
	sync := awsObject(t, account["sync"], "sync")
	if state := awsString(t, sync["state"], "state"); state != awssettings.SyncStateSyncing {
		t.Fatalf("sync state = %q", state)
	}
	awsString(t, sync["synced_at"], "synced_at")
	awsNumber(t, sync["units_done"], "units_done")
	awsNumber(t, sync["units_total"], "units_total")
	if first, ok := sync["first_sync"].(bool); !ok || !first {
		t.Fatalf("first_sync = %v", sync["first_sync"])
	}
	services, ok := sync["services"].([]any)
	if !ok || len(services) != 2 {
		t.Fatalf("sync services = %v", sync["services"])
	}
	lambda := services[1].(map[string]any)
	if lambda["service"] != "lambda" || lambda["state"] != awssettings.SyncStatePending || lambda["last_good_at"] != nil {
		t.Fatalf("lambda progress = %v", lambda)
	}
	awsArray(t, lambda["gaps"], "gaps")
}
