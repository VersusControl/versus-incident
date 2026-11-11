//go:build harness_fakeaws

package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/gofiber/fiber/v2"

	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestHarnessBuildRedirectsAWSToTheFixture: in the harness build, a real
// SDK call to the real STS host lands on the fake endpoint.
func TestHarnessBuildRedirectsAWSToTheFixture(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	t.Setenv(harnessFakeAWSEnv, fake.URL())
	client := awsHarnessHTTPClient()
	if client == nil {
		t.Fatal("harness build did not build a redirect")
	}
	api := sts.NewFromConfig(aws.Config{Region: "us-east-1", HTTPClient: client, RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDHARNESS", "secret", "session-111122223333")})
	if _, err := api.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{}); err != nil {
		t.Fatalf("redirected call = %v", err)
	}
	if fake.OperationCount("sts:GetCallerIdentity") != 1 {
		t.Fatal("call did not reach the fixture")
	}
	t.Setenv(harnessFakeAWSEnv, "not a url")
	if awsHarnessHTTPClient() != nil {
		t.Fatal("invalid fixture URL accepted")
	}
}

// TestHarnessBuildRedirectsCostCallsToTheFixture: in the harness build, the
// cost refresh boot wires up sends its Cost Explorer calls, addressed to the
// real billing host, to the fake endpoint like every other AWS call.
func TestHarnessBuildRedirectsCostCallsToTheFixture(t *testing.T) {
	fake := fakeaws.New(t, "cost-credits")
	t.Setenv(harnessFakeAWSEnv, fake.URL())
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 100
	connector := registerAWSSettingsController(fiber.New(), storage.NewMemory(), cfg)
	if connector.httpClient == nil {
		t.Fatal("harness build did not hand the connector a redirect")
	}
	conn := c.AWSConnectorConfig{Enable: true}
	conn.OptionalData.CostExplorer = true
	const payer = "111111111111"
	targets := []awscost.Target{{AccountID: payer,
		Credentials: credentials.NewStaticCredentialsProvider("ASIACOST"+payer, "secret", "FwoGZXIvYXdzE"+payer+"session")}}
	opts := awsCostOptions(conn, targets, nil, connector)
	adapter, err := awscost.New(storage.NewMemory(), tenancy.DefaultOrgScope(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = adapter.Refresh(context.Background(), awscost.KindScheduled)
	if fake.OperationCount("ce:GetCostAndUsage") == 0 {
		t.Fatal("cost refresh did not reach the fixture")
	}
}
