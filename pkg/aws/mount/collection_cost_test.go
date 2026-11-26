package mount

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// costCardNow is the time the mounted cost card reads at in tests.
var costCardNow = time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)

// The mounted cost card reads the provider's call ledger for the accounts
// in settings, with the configured budgets and daily target; an account
// no longer connected is left out.
func TestMountedCollectionCostReadsTheLedger(t *testing.T) {
	const connected, removed = "123400000001", "123400000009"
	store := storage.NewMemory()
	ledger, _ := storage.AWSCallCountsOf(store)
	now := time.Date(2026, 11, 23, 12, 30, 0, 0, time.UTC)
	if err := ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{
		{AccountID: connected, Region: "eu-south-2", Source: "elbv2", Hour: now, Calls: 120, Runs: 6},
		{AccountID: removed, Region: "eu-south-2", Source: "elbv2", Hour: now, Calls: 999, Runs: 1},
	}); err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	config := &c.Config{}
	config.Connectors.AWS = c.AWSConnectorConfig{Enable: true, Accounts: []c.AWSAccountConfig{{AccountID: connected, Regions: []string{"eu-south-2"}}},
		Budgets: c.AWSBudgetsConfig{CallsPerDay: 5000, PerCycle: map[string]int{"health": 25}}}
	mountForTest(t, app, Deps{Store: store, Config: config, Clock: func() time.Time { return now }})
	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/aws/settings/cost", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var answer struct {
		Enabled  bool  `json:"enabled"`
		Calls24h int64 `json:"calls_24h"`
		Budgets  []struct {
			Source string `json:"source"`
			Limit  int64  `json:"limit"`
			Used   int64  `json:"used"`
		} `json:"budgets"`
	}
	if res.StatusCode != fiber.StatusOK || json.Unmarshal(body, &answer) != nil || !answer.Enabled || answer.Calls24h != 120 {
		t.Fatalf("cost = %d %s", res.StatusCode, body)
	}
	limits := map[string]int64{}
	for _, budget := range answer.Budgets {
		limits[budget.Source] = budget.Limit
	}
	if limits["calls_per_day"] != 5000 || limits["health"] != 25 || limits["inventory"] != 700 {
		t.Fatalf("budgets = %s", body)
	}
}

// The scheduler's cadences come from the collector block: the defaults,
// and an interval the user set.
func TestCollectIntervalsFromConfig(t *testing.T) {
	got := CollectIntervals(c.AWSCollectConfig{})
	want := awscollect.Intervals{Inventory: 6 * time.Hour, Health: 10 * time.Minute, Changes: 5 * time.Minute, HealthHot: 2 * time.Minute,
		Identity: time.Hour, Policies: 6 * time.Hour, Trails: 24 * time.Hour, RegionsOff: 24 * time.Hour, CredentialReport: 4 * time.Hour, Findings: time.Hour}
	if got != want {
		t.Fatalf("default intervals = %+v", got)
	}
	explicit := CollectIntervals(c.AWSCollectConfig{Intervals: c.AWSCollectIntervalsConfig{Health: "2m", Inventory: "15m"}})
	if explicit.Health != 2*time.Minute || explicit.Inventory != 15*time.Minute || explicit.HealthHot != 2*time.Minute {
		t.Fatalf("explicit intervals = %+v", explicit)
	}
}
