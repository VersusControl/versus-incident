package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type baselineProviderFunc func(context.Context, core.BaselineRequest) (core.BaselineResult, error)

func (provider baselineProviderFunc) DescribeBaselines(ctx context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
	return provider(ctx, request)
}

func baselineAuthorizedContext() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

func TestDescribeBaselineValidatesBoundsAndTrustedScope(t *testing.T) {
	var captured core.BaselineRequest
	tool := DescribeBaseline{OrgID: "trusted-org", Now: func() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) }, Provider: baselineProviderFunc(func(_ context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
		captured = request
		return core.BaselineResult{Availability: core.HealthNoData, Records: []core.BaselineRecord{}, Coverage: []core.BaselineCoverage{}}, nil
	})}
	result, err := tool.Invoke(baselineAuthorizedContext(), json.RawMessage(`{"service":"api","signal":"pattern-1","window":"5m"}`))
	if err != nil || result == nil {
		t.Fatalf("Invoke error = %v, result = %#v", err, result)
	}
	if captured.OrgID != "trusted-org" || captured.Service != "api" || captured.Signal != "logs" || captured.PatternID != "pattern-1" || captured.Window != 5*time.Minute || captured.Limit != 50 {
		t.Fatalf("request = %#v", captured)
	}
	for _, raw := range []string{`{"service":"api","signal":"logs","window":"4m59s"}`, `{"service":"api","signal":"logs","window":"24h1m"}`, `{"service":"bad value","signal":"logs","window":"5m"}`, `{"service":"api","signal":"logs","window":"1d"}`} {
		if _, err := tool.Invoke(baselineAuthorizedContext(), json.RawMessage(raw)); err == nil {
			t.Errorf("Invoke(%s) accepted invalid arguments", raw)
		}
	}
}

func TestDescribeBaselineRequiresPermissionAndUsesSafeErrors(t *testing.T) {
	tool := DescribeBaseline{Provider: baselineProviderFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{}, errors.New("postgres password secret")
	})}
	denied, err := tool.Invoke(context.Background(), json.RawMessage(`{"service":"api","signal":"logs","window":"5m"}`))
	if err != nil || denied.IsAvailable() || denied.Reason != "infrastructure:view permission is required" {
		t.Fatalf("denied = %#v, err = %v", denied, err)
	}
	_, err = tool.Invoke(baselineAuthorizedContext(), json.RawMessage(`{"service":"api","signal":"logs","window":"5m"}`))
	code, message := core.ClassifyToolError(err)
	if code != core.ToolErrorBackend || message != "baseline provider failed" {
		t.Fatalf("classified error = %q %q", code, message)
	}
	if got := FilterBaselineAuthorized(context.Background(), []core.Tool{tool}); len(got) != 0 {
		t.Fatalf("unauthorized catalog retained %d tools", len(got))
	}
}
