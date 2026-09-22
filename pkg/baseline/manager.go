// Package baseline combines an OSS baseline surface with optional extensions.
package baseline

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

const MaxRecords = 50

// Surface identifies one provider without exposing provider configuration.
type Surface struct {
	Family     string
	SourceType string
	Provider   core.BaselineProvider
}

// Manager appends authorized extension records without replacing the OSS surface.
type Manager struct {
	base       Surface
	extensions []Surface
}

// NewManager constructs a provider composite. Nil surfaces are valid.
func NewManager(base Surface, extensions ...Surface) *Manager {
	normalized := make([]Surface, len(extensions))
	for index, extension := range extensions {
		normalized[index] = normalizeSurface(extension)
	}
	return &Manager{base: normalizeSurface(base), extensions: normalized}
}

// DescribeBaselines returns a deterministic bounded merge. Extension failures
// are represented as coverage and never remove records returned by the base.
func (manager *Manager) DescribeBaselines(ctx context.Context, request core.BaselineRequest) (core.BaselineResult, error) {
	request.Limit = clampLimit(request.Limit)
	result := emptyResult()
	if manager == nil {
		return result, nil
	}
	base := manager.base
	if base.Provider != nil {
		provided, err := base.Provider.DescribeBaselines(ctx, request)
		if err != nil {
			return core.BaselineResult{}, err
		}
		mergeResult(&result, normalizeResult(base, provided))
	}
	sortRecords(result.Records)
	if len(result.Records) > request.Limit {
		result.Omitted += len(result.Records) - request.Limit
		result.Records = result.Records[:request.Limit]
		result.Truncated = true
	}
	extensionRecords := make([]core.BaselineRecord, 0)
	for _, candidate := range manager.extensions {
		surface := candidate
		if surface.Provider == nil {
			continue
		}
		provided, err := surface.Provider.DescribeBaselines(ctx, request)
		if err != nil {
			result.Coverage = append(result.Coverage, core.BaselineCoverage{Family: surface.Family, SourceType: surface.SourceType, Availability: core.HealthError, ReasonCode: "provider_error"})
			continue
		}
		provided = normalizeResult(surface, provided)
		extensionRecords = append(extensionRecords, provided.Records...)
		provided.Records = nil
		mergeResult(&result, provided)
	}
	sortRecords(extensionRecords)
	remaining := request.Limit - len(result.Records)
	if len(extensionRecords) > remaining {
		result.Omitted += len(extensionRecords) - remaining
		extensionRecords = extensionRecords[:remaining]
		result.Truncated = true
	}
	result.Records = append(result.Records, extensionRecords...)
	finalize(&result, request)
	return result, nil
}

func normalizeSurface(surface Surface) Surface {
	surface.Family = strings.TrimSpace(surface.Family)
	surface.SourceType = strings.TrimSpace(surface.SourceType)
	if isNil(surface.Provider) {
		surface.Provider = nil
	}
	return surface
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Interface || kind == reflect.Pointer || kind == reflect.Func || kind == reflect.Map || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > MaxRecords {
		return MaxRecords
	}
	return limit
}

func emptyResult() core.BaselineResult {
	return core.BaselineResult{Availability: core.HealthNotConfigured, Records: []core.BaselineRecord{}, Coverage: []core.BaselineCoverage{}}
}

func mergeResult(target *core.BaselineResult, addition core.BaselineResult) {
	target.Records = append(target.Records, addition.Records...)
	target.Coverage = append(target.Coverage, addition.Coverage...)
	target.Omitted += addition.Omitted
	target.Truncated = target.Truncated || addition.Truncated
}

func normalizeResult(surface Surface, result core.BaselineResult) core.BaselineResult {
	for index := range result.Records {
		result.Records[index].Family = surface.Family
		result.Records[index].SourceType = surface.SourceType
	}
	if len(result.Coverage) == 0 {
		result.Coverage = []core.BaselineCoverage{{
			Family:       surface.Family,
			SourceType:   surface.SourceType,
			Availability: resultAvailability(result),
			ReasonCode:   result.ReasonCode,
		}}
		return result
	}
	for index := range result.Coverage {
		result.Coverage[index].Family = surface.Family
		result.Coverage[index].SourceType = surface.SourceType
	}
	return result
}

func resultAvailability(result core.BaselineResult) core.HealthState {
	if result.Availability != "" {
		return result.Availability
	}
	if len(result.Records) == 0 {
		return core.HealthNoData
	}
	if result.Truncated || result.Omitted > 0 {
		return core.HealthPartial
	}
	for _, record := range result.Records {
		if record.Availability != "" && record.Availability != core.HealthReady {
			return core.HealthPartial
		}
	}
	return core.HealthReady
}

func sortRecords(records []core.BaselineRecord) {
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		return strings.Join([]string{left.Service, left.Signal, left.Family, left.Operation, left.PatternID, left.SourceType}, "\x00") <
			strings.Join([]string{right.Service, right.Signal, right.Family, right.Operation, right.PatternID, right.SourceType}, "\x00")
	})
}

func finalize(result *core.BaselineResult, request core.BaselineRequest) {
	sort.Slice(result.Coverage, func(i, j int) bool {
		left, right := result.Coverage[i], result.Coverage[j]
		return left.Family+"\x00"+left.SourceType < right.Family+"\x00"+right.SourceType
	})
	result.Found = len(result.Records) > 0
	result.Availability = coverageAvailability(result.Coverage, request.Signal, result.Found, result.Truncated)
	if !result.Found && result.Availability == core.HealthReady {
		result.Availability = core.HealthNoData
		result.ReasonCode = "no_matching_baseline"
	}
}

func coverageAvailability(coverage []core.BaselineCoverage, selectedFamily string, found, truncated bool) core.HealthState {
	ready := false
	partial := truncated
	fallback := core.HealthNotConfigured
	for _, item := range coverage {
		if !coverageApplies(item, selectedFamily) {
			continue
		}
		switch item.Availability {
		case core.HealthReady:
			ready = true
		case core.HealthPartial:
			ready, partial = true, true
		case core.HealthError, core.HealthRestricted, core.HealthUnsupported, core.HealthStale:
			partial = partial || ready || found
			fallback = item.Availability
		case core.HealthNoData, core.HealthCollecting:
			if fallback == core.HealthNotConfigured {
				fallback = item.Availability
			}
		}
	}
	if partial {
		return core.HealthPartial
	}
	if ready || found {
		return core.HealthReady
	}
	return fallback
}

func coverageApplies(item core.BaselineCoverage, selectedFamily string) bool {
	if item.Availability != core.HealthUnsupported || item.ReasonCode != "signal_unsupported" {
		return true
	}
	selectedFamily = strings.ToLower(strings.TrimSpace(selectedFamily))
	itemFamily := strings.ToLower(strings.TrimSpace(item.Family))
	if !isSignalFamily(selectedFamily) || !isSignalFamily(itemFamily) {
		return true
	}
	return selectedFamily == itemFamily
}

func isSignalFamily(value string) bool {
	switch value {
	case "logs", "metrics", "traces":
		return true
	default:
		return false
	}
}
