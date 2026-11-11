package fakeaws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	"github.com/aws/aws-sdk-go-v2/service/computeoptimizer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"
	"github.com/aws/smithy-go"
)

// costConfig signs as the given account (the fixture reads the account from
// the session token) and sends every request to the fixture.
func costConfig(server *Server, account string) aws.Config {
	return aws.Config{
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("ASIACOST"+account, "secret", "FwoGZXIvYXdzE"+account+"session"),
		HTTPClient:       server.HTTPClient(),
		RetryMaxAttempts: 1,
	}
}

func costClient(server *Server, account string) *costexplorer.Client {
	return costexplorer.NewFromConfig(costConfig(server, account))
}

func pinCostClock(server *Server) {
	server.SetClock(func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) })
}

func dailyByService(t *testing.T, client *costexplorer.Client, start, end string) *costexplorer.GetCostAndUsageOutput {
	t.Helper()
	out, err := client.GetCostAndUsage(context.Background(), &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String(start), End: aws.String(end)},
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{"NetAmortizedCost", "UsageQuantity"},
		GroupBy:     []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")}},
	})
	if err != nil {
		t.Fatalf("GetCostAndUsage: %v", err)
	}
	return out
}

func sumMicros(t *testing.T, out *costexplorer.GetCostAndUsageOutput, service string) int64 {
	t.Helper()
	var total int64
	for _, result := range out.ResultsByTime {
		for _, group := range result.Groups {
			if service != "" && group.Keys[0] != service {
				continue
			}
			value, err := parseCostMicros(aws.ToString(group.Metrics["NetAmortizedCost"].Amount))
			if err != nil {
				t.Fatal(err)
			}
			total += value
		}
	}
	return total
}

// TestCostNatSpikeServesTheStory reads the story through the real SDK: the
// NAT line steps from $20 to $85 a day on Oct 20, October is final and every
// day of the open month (November) is estimated.
func TestCostNatSpikeServesTheStory(t *testing.T) {
	server := New(t, ScenarioCostNatSpike)
	pinCostClock(server)
	out := dailyByService(t, costClient(server, "111111111111"), "2026-10-01", "2026-11-09")
	if got, want := sumMicros(t, out, "EC2 - Other"), int64(19*20+12*85+8*85)*1_000_000; got != want {
		t.Fatalf("NAT total = %d, want %d", got, want)
	}
	for _, result := range out.ResultsByTime {
		november := aws.ToString(result.TimePeriod.Start) >= "2026-11-01"
		if result.Estimated != november {
			t.Fatalf("%s estimated = %v", aws.ToString(result.TimePeriod.Start), result.Estimated)
		}
	}
	if server.PaidCostRequests() != 1 {
		t.Fatalf("paid requests = %d", server.PaidCostRequests())
	}
	// A request that is not a billing operation in a cost scenario still
	// answers from the default fixture, and other scenarios are untouched.
	plain := New(t, "new-account-connect")
	if _, err := costClient(plain, "111111111111").GetCostAndUsage(context.Background(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-02")}, Granularity: cetypes.GranularityDaily, Metrics: []string{"NetAmortizedCost"},
	}); err == nil {
		t.Fatal("a non-cost scenario answered Cost Explorer")
	}
	if plain.PaidCostRequests() != 0 {
		t.Fatal("non-cost scenario counted a paid request")
	}
}

// TestCostCallerSeesOnlyItsOwnLines checks a member account reads its own
// lines and the management account reads the whole Organization.
func TestCostCallerSeesOnlyItsOwnLines(t *testing.T) {
	server := New(t, ScenarioCostOrganization)
	pinCostClock(server)
	member := sumMicros(t, dailyByService(t, costClient(server, "222222222222"), "2026-10-01", "2026-10-02"), "")
	if member != 50*1_000_000 {
		t.Fatalf("member 222222222222 Oct 1 = %d, want 50000000", member)
	}
	management := sumMicros(t, dailyByService(t, costClient(server, "999999999999"), "2026-10-01", "2026-10-02"), "")
	if want := parseMust(t, "300") + parseMust(t, "100.333333") + parseMust(t, "20") + parseMust(t, "30") + parseMust(t, "50") + parseMust(t, "10") + parseMust(t, "12") + parseMust(t, "1200"); management != want {
		t.Fatalf("management Oct 1 = %d, want %d", management, want)
	}
}

func parseMust(t *testing.T, value string) int64 {
	t.Helper()
	micros, err := parseCostMicros(value)
	if err != nil {
		t.Fatal(err)
	}
	return micros
}

func forecastInput() *costexplorer.GetCostForecastInput {
	return &costexplorer.GetCostForecastInput{
		TimePeriod:  &cetypes.DateInterval{Start: aws.String("2026-11-09"), End: aws.String("2026-12-01")},
		Metric:      cetypes.MetricNetAmortizedCost,
		Granularity: cetypes.GranularityMonthly,
	}
}

func apiCode(err error) string {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode() + ": " + apiError.ErrorMessage()
	}
	return ""
}

// TestCostFailuresUseTheirWireShape covers the denied member, the
// Cost Explorer that is not enabled, and throttling.
func TestCostFailuresUseTheirWireShape(t *testing.T) {
	denied := New(t, ScenarioCostDeniedAccount)
	pinCostClock(denied)
	_, err := costClient(denied, CostDeniedAccount).GetCostAndUsage(context.Background(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-02")}, Granularity: cetypes.GranularityDaily, Metrics: []string{"NetAmortizedCost"}})
	if code := apiCode(err); code == "" || code[:len("AccessDeniedException")] != "AccessDeniedException" {
		t.Fatalf("denied member = %v", err)
	}
	dailyByService(t, costClient(denied, "111111111111"), "2026-10-01", "2026-10-02")

	unavailable := New(t, ScenarioCostUnavailable)
	_, err = costClient(unavailable, "111111111111").GetCostForecast(context.Background(), forecastInput())
	if code := apiCode(err); code != "AccessDeniedException: User not enabled for cost explorer access" {
		t.Fatalf("unavailable = %q", code)
	}

	throttled := New(t, ScenarioCostThrottled)
	_, err = costClient(throttled, "111111111111").GetCostForecast(context.Background(), forecastInput())
	if code := apiCode(err); code != "LimitExceededException: Rate exceeded" {
		t.Fatalf("throttled = %q", code)
	}
	throttled.SetCostThrottled(false)
	if _, err := costClient(throttled, "111111111111").GetCostForecast(context.Background(), forecastInput()); err != nil {
		t.Fatalf("throttle off: %v", err)
	}
}

// TestCostSavingsAndBudgetsDecode reads Budgets, Cost Optimization Hub and
// Compute Optimizer (rpc-v2-cbor) through their SDK clients.
func TestCostSavingsAndBudgetsDecode(t *testing.T) {
	server := New(t, ScenarioCostNatSpike)
	cfg := costConfig(server, "111111111111")
	hub, err := costoptimizationhub.NewFromConfig(cfg).ListRecommendations(context.Background(), &costoptimizationhub.ListRecommendationsInput{})
	if err != nil || len(hub.Items) != 4 {
		t.Fatalf("hub = %d items, %v", len(hub.Items), err)
	}
	optimizer, err := computeoptimizer.NewFromConfig(cfg).GetEC2InstanceRecommendations(context.Background(), &computeoptimizer.GetEC2InstanceRecommendationsInput{})
	if err != nil || len(optimizer.InstanceRecommendations) != 2 {
		t.Fatalf("compute optimizer = %+v, %v", optimizer, err)
	}
	saving := optimizer.InstanceRecommendations[0].RecommendationOptions[0].SavingsOpportunity.EstimatedMonthlySavings.Value
	if saving <= 0 {
		t.Fatalf("compute optimizer saving = %v", saving)
	}
	credits := New(t, ScenarioCostCredits)
	described, err := budgets.NewFromConfig(costConfig(credits, "111111111111")).DescribeBudgets(context.Background(), &budgets.DescribeBudgetsInput{AccountId: aws.String("111111111111")})
	if err != nil || len(described.Budgets) != 8 {
		t.Fatalf("budgets = %d, %v", len(described.Budgets), err)
	}
	if server.PaidCostRequests() != 0 || credits.PaidCostRequests() != 0 {
		t.Fatal("savings and budgets calls counted as paid Cost Explorer requests")
	}
}

// TestCostCapScenarioPages checks the large Organization pages its groups.
func TestCostCapScenarioPages(t *testing.T) {
	server := New(t, ScenarioCostCapReached)
	pinCostClock(server)
	out, err := costClient(server, "999999999999").GetCostAndUsage(context.Background(), &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-01"), End: aws.String("2026-10-03")}, Granularity: cetypes.GranularityDaily,
		Metrics: []string{"NetAmortizedCost"},
		GroupBy: []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("LINKED_ACCOUNT")}, {Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String("SERVICE")}},
	})
	if err != nil || out.NextPageToken == nil {
		t.Fatalf("large Organization did not page: %v", err)
	}
}

// TestCostTwoNATsRefusesResourceDetail keeps the daily lines and refuses only
// the resource-level call.
func TestCostTwoNATsRefusesResourceDetail(t *testing.T) {
	server := New(t, ScenarioCostNatSpikeTwoNATs)
	pinCostClock(server)
	client := costClient(server, "111111111111")
	dailyByService(t, client, "2026-10-01", "2026-10-03")
	_, err := client.GetCostAndUsageWithResources(context.Background(), &costexplorer.GetCostAndUsageWithResourcesInput{
		TimePeriod: &cetypes.DateInterval{Start: aws.String("2026-10-27"), End: aws.String("2026-11-09")}, Granularity: cetypes.GranularityDaily,
		Metrics: []string{"NetAmortizedCost"},
		Filter:  &cetypes.Expression{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionService, Values: []string{"EC2 - Other"}}},
	})
	if code := apiCode(err); code == "" || code[:len("AccessDeniedException")] != "AccessDeniedException" {
		t.Fatalf("resource detail = %v", err)
	}
}
