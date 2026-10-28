package agent

import (
	"context"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/stats"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type logBaselineProvider struct {
	catalog          *Catalog
	scope            tenancy.OrgScope
	autoPromoteAfter int
	store            storage.Provider
}

func newLogBaselineProvider(catalog *Catalog, scope tenancy.OrgScope, autoPromoteAfter int, store storage.Provider) core.BaselineProvider {
	if catalog == nil {
		return nil
	}
	return &logBaselineProvider{catalog: catalog, scope: scope.Normalized(), autoPromoteAfter: effectiveAutoPromote(autoPromoteAfter), store: store}
}

func (provider *logBaselineProvider) DescribeBaselines(_ context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
	result := core.BaselineResult{Availability: core.HealthNoData, ReasonCode: "no_matching_baseline", Records: []core.BaselineRecord{}, Coverage: []core.BaselineCoverage{{Family: "logs", SourceType: "catalog", Availability: core.HealthNoData, ReasonCode: "no_matching_baseline"}}}
	if provider == nil || provider.catalog == nil {
		result.Availability = core.HealthNotConfigured
		result.ReasonCode = "catalog_not_configured"
		result.Coverage[0].Availability = core.HealthNotConfigured
		result.Coverage[0].ReasonCode = result.ReasonCode
		return result, nil
	}
	if request.Signal != "logs" {
		result.Availability = core.HealthUnsupported
		result.ReasonCode = "signal_unsupported"
		result.Coverage[0].Availability = core.HealthUnsupported
		result.Coverage[0].ReasonCode = result.ReasonCode
		return result, nil
	}
	settings := LoadSpikeSettings(provider.store)
	patterns := provider.catalog.All()
	selected := make([]*Pattern, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == nil || !provider.scope.Contains(pattern.OrgID) || pattern.Service != request.Service {
			continue
		}
		if request.PatternID != "" && pattern.ID != request.PatternID {
			continue
		}
		selected = append(selected, pattern)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	limit := request.Limit
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	if len(selected) > limit {
		result.Truncated = true
		result.Omitted = len(selected) - limit
		selected = selected[:limit]
	}
	collecting := false
	for _, pattern := range selected {
		record := provider.project(pattern, request.At, settings)
		collecting = collecting || !record.Confident
		result.Records = append(result.Records, record)
	}
	result.Found = len(result.Records) > 0
	if result.Found {
		result.Availability = core.HealthReady
		result.ReasonCode = ""
		result.Coverage[0] = core.BaselineCoverage{Family: "logs", SourceType: "catalog", Availability: core.HealthReady}
		if collecting || result.Truncated {
			result.Availability = core.HealthPartial
			result.Coverage[0].Availability = core.HealthPartial
			if collecting {
				result.Coverage[0].ReasonCode = "baseline_collecting"
			}
		}
	}
	return result, nil
}

func (provider *logBaselineProvider) project(pattern *Pattern, at time.Time, settings SpikeSettings) core.BaselineRecord {
	global := stats.EWMA{Mean: pattern.BaselineFrequency, Variance: pattern.BaselineVariance, Count: pattern.Count}
	mean, std := global.Mean, global.Std()
	mode := normalizeBaselineMode(settings.BaselineMode)
	if pattern.SpikeBaselineMode != "" {
		mode = normalizeBaselineMode(pattern.SpikeBaselineMode)
	}
	switch mode {
	case baselineModeAverage:
		mean = pattern.BaselineAvg
	case baselineModeTimeOfDay:
		mean, std, _ = stats.Expected(global, pattern.Seasonal, at, stats.HoursPerDay, logMinBucketSamples, 0)
	}
	confident := LogReadiness(pattern, provider.autoPromoteAfter, time.Second).Ready
	availability := core.HealthReady
	reason := "current_value_unavailable"
	if !confident {
		availability = core.HealthCollecting
		reason = "baseline_collecting_current_value_unavailable"
	}
	return core.BaselineRecord{
		Service: pattern.Service, Signal: "logs", Family: "log_pattern", SourceType: "catalog", PatternID: pattern.ID,
		ExpectedMean: mean, ExpectedStd: std, CurrentValue: nil, Unit: "events_per_second",
		ObservationCount: pattern.Count, Confident: confident, LastTrainedAt: pattern.LastSeen.UTC(),
		Availability: availability, ReasonCode: reason, Provenance: []string{"learned_log_pattern"},
	}
}
