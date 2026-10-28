package baseline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type providerFunc func(context.Context, core.BaselineRequest) (core.BaselineResult, error)

func (provider providerFunc) DescribeBaselines(ctx context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
	return provider(ctx, request)
}

func TestManagerKeepsBaseAcrossExtensionStates(t *testing.T) {
	base := Surface{Family: "logs", SourceType: "catalog", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Records: []core.BaselineRecord{{Service: "api", Signal: "logs", PatternID: "log"}}, Coverage: []core.BaselineCoverage{{Family: "logs", SourceType: "catalog", Availability: core.HealthReady}}}, nil
	})}
	tests := []struct {
		name      string
		extension Surface
		wantState core.HealthState
	}{
		{name: "nil", extension: Surface{}, wantState: core.HealthReady},
		{name: "restricted", extension: Surface{Family: "metrics", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
			return core.BaselineResult{Coverage: []core.BaselineCoverage{{Family: "metrics", SourceType: "licensed", Availability: core.HealthRestricted}}}, nil
		})}, wantState: core.HealthPartial},
		{name: "error", extension: Surface{Family: "traces", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
			return core.BaselineResult{}, errors.New("secret backend detail")
		})}, wantState: core.HealthPartial},
		{name: "ready", extension: Surface{Family: "metrics", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
			return core.BaselineResult{Records: []core.BaselineRecord{{Service: "api", Signal: "metrics", PatternID: "metric"}}, Coverage: []core.BaselineCoverage{{Family: "metrics", SourceType: "licensed", Availability: core.HealthReady}}}, nil
		})}, wantState: core.HealthReady},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewManager(base, test.extension).DescribeBaselines(context.Background(), core.BaselineRequest{Limit: MaxRecords})
			if err != nil {
				t.Fatal(err)
			}
			if got.Availability != test.wantState || len(got.Records) == 0 || got.Records[0].PatternID != "log" {
				t.Fatalf("result = %#v, want state %q with retained log", got, test.wantState)
			}
		})
	}
}

func TestManagerIgnoresUnsupportedUnselectedSignalFamilyForAggregateAvailability(t *testing.T) {
	unsupportedLogs := Surface{Family: "logs", SourceType: "catalog", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Availability: core.HealthUnsupported, ReasonCode: "signal_unsupported"}, nil
	})}
	readyExtension := func(family string) Surface {
		return Surface{Family: family, SourceType: "intelligence", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
			return core.BaselineResult{Records: []core.BaselineRecord{{Service: "api", Signal: family}}}, nil
		})}
	}
	for _, family := range []string{"metrics", "traces"} {
		t.Run(family, func(t *testing.T) {
			got, err := NewManager(unsupportedLogs, readyExtension(family)).DescribeBaselines(context.Background(), core.BaselineRequest{Signal: family})
			if err != nil {
				t.Fatal(err)
			}
			if got.Availability != core.HealthReady || len(got.Coverage) != 2 || got.Coverage[0].Availability != core.HealthUnsupported {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

func TestManagerUnsupportedSelectedFamilyAndEmptyStatesRemainDistinct(t *testing.T) {
	unsupportedLogs := Surface{Family: "logs", SourceType: "catalog", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Availability: core.HealthUnsupported, ReasonCode: "signal_unsupported"}, nil
	})}
	readyMetrics := Surface{Family: "metrics", SourceType: "intelligence", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Availability: core.HealthReady}, nil
	})}
	tests := []struct {
		name    string
		manager *Manager
		signal  string
		want    core.HealthState
	}{
		{name: "requested unsupported logs", manager: NewManager(unsupportedLogs), signal: "logs", want: core.HealthUnsupported},
		{name: "ready provider without records", manager: NewManager(Surface{}, readyMetrics), signal: "metrics", want: core.HealthNoData},
		{name: "no providers", manager: NewManager(Surface{}), signal: "metrics", want: core.HealthNotConfigured},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.manager.DescribeBaselines(context.Background(), core.BaselineRequest{Signal: test.signal})
			if err != nil {
				t.Fatal(err)
			}
			if got.Availability != test.want || got.Found {
				t.Fatalf("result = %#v, want availability %q without records", got, test.want)
			}
		})
	}
}

func TestManagerOrdersAndCapsRecords(t *testing.T) {
	records := make([]core.BaselineRecord, 0, 52)
	for index := 51; index >= 0; index-- {
		records = append(records, core.BaselineRecord{Service: "api", Signal: "logs", PatternID: string(rune('A' + index))})
	}
	manager := NewManager(Surface{Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Records: records, Coverage: []core.BaselineCoverage{{Family: "logs", Availability: core.HealthReady}}}, nil
	})})
	got, err := manager.DescribeBaselines(context.Background(), core.BaselineRequest{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 50 || !got.Truncated || got.Omitted != 2 || got.Availability != core.HealthPartial {
		t.Fatalf("bounded result = %#v", got)
	}
	ordered := append([]core.BaselineRecord(nil), got.Records...)
	if !reflect.DeepEqual(got.Records, ordered) || got.Records[0].PatternID >= got.Records[49].PatternID {
		t.Fatal("records are not deterministically ordered")
	}
}

func TestManagerReservesRecordBudgetForBase(t *testing.T) {
	baseRecords := make([]core.BaselineRecord, 0, MaxRecords)
	extensionRecords := make([]core.BaselineRecord, 0, MaxRecords)
	for index := MaxRecords - 1; index >= 0; index-- {
		baseRecords = append(baseRecords, core.BaselineRecord{Service: "z-base", PatternID: fmt.Sprintf("base-%02d", index)})
		extensionRecords = append(extensionRecords, core.BaselineRecord{Service: "a-extension", PatternID: fmt.Sprintf("extension-%02d", index)})
	}
	base := Surface{Family: "logs", SourceType: "catalog", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Records: baseRecords}, nil
	})}
	extension := Surface{Family: "metrics", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Records: extensionRecords}, nil
	})}

	got, err := NewManager(base, extension).DescribeBaselines(context.Background(), core.BaselineRequest{Limit: MaxRecords})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != MaxRecords || got.Omitted != MaxRecords || !got.Truncated {
		t.Fatalf("bounded result = %#v", got)
	}
	for _, record := range got.Records {
		if record.Service != "z-base" {
			t.Fatalf("extension evicted base record: %#v", record)
		}
	}
	if got.Records[0].PatternID != "base-00" || got.Records[MaxRecords-1].PatternID != "base-49" {
		t.Fatalf("base records are not sorted: first=%q last=%q", got.Records[0].PatternID, got.Records[MaxRecords-1].PatternID)
	}
}

func TestManagerNormalizesSurfaceCoverage(t *testing.T) {
	tests := []struct {
		name             string
		result           core.BaselineResult
		wantAvailability core.HealthState
		wantReason       string
	}{
		{name: "spoofed coverage", result: core.BaselineResult{Availability: core.HealthRestricted, ReasonCode: "license_required", Coverage: []core.BaselineCoverage{{Family: "spoofed", SourceType: "spoofed", Availability: core.HealthRestricted, ReasonCode: "license_required"}}}, wantAvailability: core.HealthRestricted, wantReason: "license_required"},
		{name: "no coverage ready records", result: core.BaselineResult{Records: []core.BaselineRecord{{PatternID: "ready"}}}, wantAvailability: core.HealthReady},
		{name: "no coverage partial records", result: core.BaselineResult{Records: []core.BaselineRecord{{PatternID: "learning", Availability: core.HealthCollecting}}}, wantAvailability: core.HealthPartial},
		{name: "no coverage no data", result: core.BaselineResult{Availability: core.HealthNoData, ReasonCode: "empty"}, wantAvailability: core.HealthNoData, wantReason: "empty"},
		{name: "no coverage stale", result: core.BaselineResult{Availability: core.HealthStale, ReasonCode: "old"}, wantAvailability: core.HealthStale, wantReason: "old"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			extension := Surface{Family: "metrics", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
				return test.result, nil
			})}
			got, err := NewManager(Surface{}, extension).DescribeBaselines(context.Background(), core.BaselineRequest{Limit: MaxRecords})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Coverage) != 1 || got.Coverage[0].Family != "metrics" || got.Coverage[0].SourceType != "licensed" || got.Coverage[0].Availability != test.wantAvailability || got.Coverage[0].ReasonCode != test.wantReason {
				t.Fatalf("coverage = %#v", got.Coverage)
			}
		})
	}
}

func TestManagerStampsSurfaceIdentityOnRecords(t *testing.T) {
	extension := Surface{Family: "metrics", SourceType: "intelligence", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{Records: []core.BaselineRecord{{Service: "api", Signal: "request_rate", Family: "spoofed", SourceType: "secret-endpoint"}}}, nil
	})}
	got, err := NewManager(Surface{}, extension).DescribeBaselines(context.Background(), core.BaselineRequest{Limit: MaxRecords})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 || got.Records[0].Family != "metrics" || got.Records[0].SourceType != "intelligence" {
		t.Fatalf("records = %#v", got.Records)
	}
}

func TestManagerDoesNotExposeExtensionError(t *testing.T) {
	extension := Surface{Family: "traces", SourceType: "licensed", Provider: providerFunc(func(context.Context, core.BaselineRequest) (core.BaselineResult, error) {
		return core.BaselineResult{}, errors.New("secret backend detail")
	})}
	got, err := NewManager(Surface{}, extension).DescribeBaselines(context.Background(), core.BaselineRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.BaselineCoverage{{Family: "traces", SourceType: "licensed", Availability: core.HealthError, ReasonCode: "provider_error"}}
	if !reflect.DeepEqual(got.Coverage, want) {
		t.Fatalf("coverage = %#v, want %#v", got.Coverage, want)
	}
}
