//go:build harness_awsendpoint || harness_fakeaws

package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/gofiber/fiber/v2"

	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestHarnessBuildRedirectsCostCallsToTheFixture: in a test build, the cost
// refresh boot wires up sends its Cost Explorer calls, addressed to the real
// billing host, to the fake endpoint like every other AWS call.
func TestHarnessBuildRedirectsCostCallsToTheFixture(t *testing.T) {
	fake := fakeaws.New(t, "cost-credits")
	t.Setenv("VERSUS_HARNESS_FAKEAWS_URL", "")
	t.Setenv("VERSUS_HARNESS_AWS_ENDPOINT_URL", fake.URL())
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 100
	connector := mountAWS(fiber.New(), storage.NewMemory(), cfg)
	if connector.HTTPClient == nil {
		t.Fatal("test build did not hand the connector a redirect")
	}
	conn := c.AWSConnectorConfig{Enable: true}
	conn.OptionalData.CostExplorer = true
	const payer = "111111111111"
	targets := []awscost.Target{{AccountID: payer,
		Credentials: credentials.NewStaticCredentialsProvider("ASIACOST"+payer, "secret", "FwoGZXIvYXdzE"+payer+"session")}}
	opts := connector.CostOptions(conn, targets, nil)
	adapter, err := awscost.New(storage.NewMemory(), tenancy.DefaultOrgScope(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = adapter.Refresh(context.Background(), awscost.KindScheduled)
	if fake.OperationCount("ce:GetCostAndUsage") == 0 {
		t.Fatal("cost refresh did not reach the fixture")
	}
}
