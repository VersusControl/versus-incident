package mount

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// countingAWS is the AWS transport of a mounted surface: it counts every
// request and answers none.
type countingAWS struct{ requests atomic.Int64 }

func (client *countingAWS) Do(request *http.Request) (*http.Response, error) {
	client.requests.Add(1)
	return nil, context.Canceled
}

// The cost card answers from the stores alone: with a connected account,
// a ledger and stored reads, any number of requests to the route send no
// request to AWS.
func TestCollectionCostRouteNeverCallsAWS(t *testing.T) {
	const account = "123400000001"
	store := storage.NewMemory()
	ledger, _ := storage.AWSCallCountsOf(store)
	if err := ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), []storage.AWSCallCount{
		{AccountID: account, Region: "eu-south-2", Source: "elbv2", Hour: costCardNow, Calls: 120, Runs: 6},
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
	config.Connectors.AWS = c.AWSConnectorConfig{Enable: true, Accounts: []c.AWSAccountConfig{{AccountID: account, Regions: []string{"eu-south-2"}}}}
	config.Connectors.AWS.Collect.Enable = true
	transport := &countingAWS{}
	mounted := mountForTest(t, app, Deps{Store: store, Config: config, HTTPClient: transport, Clock: func() time.Time { return costCardNow }})
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: account, Region: "eu-south-2", Service: awscollect.TrailsService}, snapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	for request := 0; request < 3; request++ {
		res, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/aws/settings/cost", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != fiber.StatusOK {
			t.Fatalf("cost = %d", res.StatusCode)
		}
	}
	if sent := transport.requests.Load(); sent != 0 {
		t.Fatalf("the cost route sent %d requests to AWS", sent)
	}
}

// An interval the user set in the collector block wins over its default
// on the mounted scheduler, for every kind of unit.
func TestConfiguredIntervalsWinOnTheMountedScheduler(t *testing.T) {
	const account = "123400000001"
	config := &c.Config{}
	config.Connectors.AWS = c.AWSConnectorConfig{Enable: true}
	config.Connectors.AWS.Collect.Intervals = c.AWSCollectIntervalsConfig{
		Health: "3m", HealthHot: "1m", Inventory: "2h", Changes: "7m", Identity: "30m",
		Policies: "8h", Trails: "12h", RegionsOff: "36h", CredentialReport: "90m", Findings: "45m",
	}
	mounted := mountForTest(t, fiber.New(), Deps{Config: config})
	unit := func(region, service string) model.Unit {
		return model.Unit{Account: account, Region: region, Service: service}
	}
	for name, tc := range map[string]struct {
		unit model.Unit
		want time.Duration
	}{
		"health":    {unit("eu-south-2", "elbv2"), 3 * time.Minute},
		"inventory": {unit("eu-south-2", "lambda"), 2 * time.Hour},
		"changes":   {unit("eu-south-2", snapshot.ChangeService), 7 * time.Minute},
		"identity":  {unit(model.GlobalRegion, "iam"), 30 * time.Minute},
		"policies":  {unit(model.GlobalRegion, "s3-policy"), 8 * time.Hour},
		"trails":    {unit("eu-south-2", awscollect.TrailsService), 12 * time.Hour},
	} {
		if got := mounted.Collectors.Cadence(tc.unit); got.Interval != tc.want {
			t.Errorf("%s: %s, want %s", name, got.Interval, tc.want)
		}
	}
	effective := CollectIntervals(config.Connectors.AWS.Collect).Effective()
	if effective.HealthHot != time.Minute || effective.RegionsOff != 36*time.Hour || effective.CredentialReport != 90*time.Minute || effective.Findings != 45*time.Minute {
		t.Errorf("effective intervals = %+v", effective)
	}
}
