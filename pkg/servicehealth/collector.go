package servicehealth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const maxIncidentJoinRows = 2000

// ServiceMetadata is the source-neutral catalog view used by the collector.
type ServiceMetadata struct {
	OrgID  string
	Name   string
	Domain string
	Kind   string
}

// PatternMetadata contributes internal presence only; Count is intentionally absent.
type PatternMetadata struct {
	OrgID    string
	Service  string
	LastSeen time.Time
}

// CollectorOptions supplies trusted, already-local readers. No option can pull a signal source.
type CollectorOptions struct {
	Manager   *Manager
	Store     storage.Provider
	Services  func() []ServiceMetadata
	Patterns  func() []PatternMetadata
	SourceIDs []string
	Now       func() time.Time
}

// Collector builds and persists OSS snapshots without external reads.
type Collector struct {
	manager   *Manager
	store     storage.Provider
	services  func() []ServiceMetadata
	patterns  func() []PatternMetadata
	sourceIDs []string
	now       func() time.Time
}

// NewCollector creates a bounded snapshot collector.
func NewCollector(options CollectorOptions) *Collector {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{manager: options.Manager, store: options.Store, services: options.Services, patterns: options.Patterns, sourceIDs: append([]string(nil), options.SourceIDs...), now: now}
}

// Collect builds one snapshot under the captured settings revision and saves it atomically.
func (collector *Collector) Collect(ctx context.Context, orgID string) (SnapshotEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotEnvelope{}, err
	}
	settings, err := collector.manager.LoadSettings(orgID)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	now := collector.now().UTC()
	interval := time.Duration(settings.IntervalSeconds) * time.Second
	windowEnd := now.Truncate(interval)
	rows, statuses, err := collector.manager.QueryWindow(orgID, windowEnd, time.Duration(settings.WindowSeconds)*time.Second)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	snapshot := collector.build(orgID, settings, windowEnd, rows, statuses)
	if err := collector.manager.SaveSnapshot(orgID, snapshot); err != nil {
		return SnapshotEnvelope{}, err
	}
	return snapshot, nil
}

func (collector *Collector) build(orgID string, settings Settings, now time.Time, rows []WindowAggregate, statuses []SourceStatus) SnapshotEnvelope {
	orgID = storage.NormalizeOrgID(orgID)
	services := map[string]*ServiceSnapshot{}
	ensure := func(name string) *ServiceSnapshot {
		if name == "" {
			name = "_unknown"
		}
		if existing := services[name]; existing != nil {
			return existing
		}
		item := &ServiceSnapshot{OrgID: orgID, Service: name, Domain: "Ungrouped", Kind: "unknown", Severity: "unknown", AssessmentBasis: "Internal data only", Logs: WindowAggregate{Service: name, SeverityCounts: map[string]int64{}}, Availability: defaultAvailability(), ActiveIncidents: nil}
		services[name] = item
		return item
	}
	if collector.services != nil {
		for _, metadata := range collector.services() {
			if storage.NormalizeOrgID(metadata.OrgID) != orgID {
				continue
			}
			item := ensure(metadata.Name)
			if metadata.Domain != "" {
				item.Domain = metadata.Domain
			}
			if metadata.Kind != "" {
				item.Kind = metadata.Kind
			}
		}
	}
	internalPatterns := map[string]int{}
	serviceSources := map[string]map[string]struct{}{}
	if collector.patterns != nil {
		for _, pattern := range collector.patterns() {
			if storage.NormalizeOrgID(pattern.OrgID) != orgID || pattern.Service == "" {
				continue
			}
			ensure(pattern.Service)
			internalPatterns[pattern.Service]++
		}
	}
	for _, row := range rows {
		service := normalizeService(row.Service)
		item := ensure(service)
		if serviceSources[service] == nil {
			serviceSources[service] = map[string]struct{}{}
		}
		serviceSources[service][row.SourceID] = struct{}{}
		item.Logs.MatchedLogs += row.MatchedLogs
		item.Logs.UniquePatterns += row.UniquePatterns
		item.Logs.NewPatterns += row.NewPatterns
		item.Logs.UnknownPatterns += row.UnknownPatterns
		item.Logs.SpikingPatterns += row.SpikingPatterns
		item.Logs.Estimated = item.Logs.Estimated || row.Estimated
		if row.Truncated {
			item.Logs.Truncated = true
			item.Logs.PartialReason = row.PartialReason
		}
		if row.LatestObservation.After(item.Logs.LatestObservation) {
			item.Logs.LatestObservation = row.LatestObservation
		}
		for severity, count := range row.SeverityCounts {
			item.Logs.SeverityCounts[severity] += count
		}
	}
	incidentReadOK, incidentPartial := collector.joinIncidents(orgID, ensure)
	statusBySource := make(map[string]SourceStatus, len(statuses))
	for _, status := range statuses {
		statusBySource[status.SourceID] = status
	}
	configured := append([]string(nil), collector.sourceIDs...)
	if len(configured) == 0 {
		for sourceID := range statusBySource {
			configured = append(configured, sourceID)
		}
	}
	sort.Strings(configured)
	for _, item := range services {
		if item.Logs.MatchedLogs > 0 {
			contributors := make([]string, 0, len(serviceSources[item.Service]))
			for sourceID := range serviceSources[item.Service] {
				contributors = append(contributors, sourceID)
			}
			availability := sourceAvailability(contributors, statusBySource, now, time.Duration(settings.WindowSeconds)*time.Second)
			switch {
			case item.Logs.Truncated:
				item.Availability["logs"] = core.MeasureAvailability{State: core.HealthPartial, ReasonCode: item.Logs.PartialReason}
			case availability.State == core.HealthError || availability.State == core.HealthPartial:
				item.Availability["logs"] = core.MeasureAvailability{State: core.HealthPartial, ReasonCode: "source_partial", ActionID: "review_log_source"}
			case availability.State == core.HealthStale:
				item.Availability["logs"] = availability
			default:
				item.Availability["logs"] = core.MeasureAvailability{State: core.HealthReady}
			}
			item.AssessmentBasis = "Logs + incidents"
			item.Severity = logSeverity(item.Logs)
		} else if len(configured) == 0 {
			item.Availability["logs"] = core.MeasureAvailability{State: core.HealthNotConfigured, ReasonCode: "source_not_configured", ActionID: "connect_log_source"}
		} else {
			item.Availability["logs"] = sourceAvailability(configured, statusBySource, now, time.Duration(settings.WindowSeconds)*time.Second)
		}
		hasPatternEvidence := internalPatterns[item.Service] > 0
		if hasPatternEvidence {
			if item.Logs.MatchedLogs > 0 {
				item.AssessmentBasis = "Logs + patterns + incidents"
			} else {
				item.AssessmentBasis = "Patterns + incidents"
			}
		}
		if !incidentReadOK {
			item.Availability["internal"] = core.MeasureAvailability{State: core.HealthError, ReasonCode: "internal_read_failed"}
		} else if incidentPartial {
			item.Availability["internal"] = core.MeasureAvailability{State: core.HealthPartial, ReasonCode: "join_truncated"}
		} else if hasPatternEvidence || (item.ActiveIncidents != nil && *item.ActiveIncidents > 0) {
			item.Availability["internal"] = core.MeasureAvailability{State: core.HealthReady}
		} else if incidentReadOK {
			item.Availability["internal"] = core.MeasureAvailability{State: core.HealthNoData, ReasonCode: "no_internal_evidence"}
		}
		if item.ActiveIncidents != nil && *item.ActiveIncidents > 0 {
			item.Severity = "degraded"
		}
	}
	list := make([]ServiceSnapshot, 0, len(services))
	for _, item := range services {
		list = append(list, *item)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Service < list[j].Service })
	partial := incidentPartial
	for _, item := range list {
		partial = partial || item.Logs.Truncated
	}
	if len(list) > MaxServicesPerSnapshot {
		list = list[:MaxServicesPerSnapshot]
		partial = true
	}
	globalAvailability := sourceAvailability(configured, statusBySource, now, time.Duration(settings.WindowSeconds)*time.Second)
	if globalAvailability.State == core.HealthPartial || globalAvailability.State == core.HealthError || globalAvailability.State == core.HealthStale {
		partial = true
	}
	domains := aggregateDomains(list)
	facets := map[string]int{}
	observed := 0
	for _, item := range list {
		facets[item.Severity]++
		if item.Availability["logs"].State == core.HealthReady || item.Availability["internal"].State == core.HealthReady {
			observed++
		}
	}
	attempt, success := latestSourceTimes(statuses)
	idSum := sha256.Sum256(fmt.Appendf(nil, "%s:%d:%d:%d", orgID, settings.Revision, now.Unix(), settings.WindowSeconds))
	return SnapshotEnvelope{
		SnapshotID: hex.EncodeToString(idSum[:16]), GeneratedAt: now, LatestAttempt: attempt, LatestSuccess: success,
		NextCollectionAt: now.Add(time.Duration(settings.IntervalSeconds) * time.Second), SettingsRevision: settings.Revision,
		WindowSeconds: settings.WindowSeconds, Services: list, Domains: domains, Facets: facets,
		Coverage:     CoverageSummary{TotalServices: len(services), ObservedServices: observed, Partial: partial},
		Capabilities: capabilities(len(configured) > 0),
	}
}

func defaultAvailability() map[string]core.MeasureAvailability {
	return map[string]core.MeasureAvailability{
		"logs":     {State: core.HealthCollecting, ReasonCode: "awaiting_first_collection"},
		"internal": {State: core.HealthCollecting, ReasonCode: "awaiting_first_collection"},
	}
}

func normalizeService(service string) string {
	if service == "" {
		return "_unknown"
	}
	return service
}

func sourceAvailability(sourceIDs []string, statuses map[string]SourceStatus, now time.Time, window time.Duration) core.MeasureAvailability {
	haveAttempt, haveSuccess, haveError, stale := false, false, false, false
	for _, sourceID := range sourceIDs {
		status, ok := statuses[sourceID]
		if !ok {
			continue
		}
		haveAttempt = true
		if !status.LatestSuccess.IsZero() {
			haveSuccess = true
			stale = stale || now.Sub(status.LatestSuccess) > window
		}
		haveError = haveError || status.ErrorClass != ""
	}
	switch {
	case haveSuccess && stale:
		return core.MeasureAvailability{State: core.HealthStale, ReasonCode: "source_stale", ActionID: "review_log_source"}
	case haveError && haveSuccess:
		return core.MeasureAvailability{State: core.HealthPartial, ReasonCode: "source_partial", ActionID: "review_log_source"}
	case haveError:
		return core.MeasureAvailability{State: core.HealthError, ReasonCode: "source_error", ActionID: "review_log_source"}
	case haveAttempt:
		return core.MeasureAvailability{State: core.HealthNoData, ReasonCode: "no_observations_in_window"}
	default:
		return core.MeasureAvailability{State: core.HealthCollecting, ReasonCode: "awaiting_first_collection"}
	}
}

func (collector *Collector) joinIncidents(orgID string, ensure func(string) *ServiceSnapshot) (bool, bool) {
	if collector.store == nil {
		return false, true
	}
	const pageSize = 500
	records := make([]*storage.IncidentRecord, 0, maxIncidentJoinRows)
	for offset := 0; offset < maxIncidentJoinRows; offset += pageSize {
		var page []*storage.IncidentRecord
		var err error
		paged := true
		if pager, ok := collector.store.(storage.ScopedIncidentPager); ok {
			page, err = pager.ListIncidentsPageForScope(tenancy.NewOrgScope(orgID), "", offset, pageSize)
		} else if pager, ok := collector.store.(storage.IncidentPager); ok {
			page, err = pager.ListIncidentsPage("", offset, pageSize)
		} else {
			page, err = collector.store.ListIncidents(maxIncidentJoinRows)
			paged = false
		}
		if err != nil {
			return false, true
		}
		records = append(records, page...)
		if len(page) < pageSize || !paged {
			break
		}
	}
	partial := len(records) >= maxIncidentJoinRows
	if partial {
		return true, true
	}
	counts := map[string]int{}
	for _, record := range records {
		if storage.NormalizeOrgID(record.OrgID) != orgID || record.Service == "" || record.Resolved {
			continue
		}
		counts[record.Service]++
	}
	for service, count := range counts {
		value := count
		ensure(service).ActiveIncidents = &value
	}
	for _, item := range collector.serviceListForIncidents(orgID) {
		if item.Name == "" {
			continue
		}
		if ensure(item.Name).ActiveIncidents == nil {
			zero := 0
			ensure(item.Name).ActiveIncidents = &zero
		}
	}
	return true, false
}

func (collector *Collector) serviceListForIncidents(orgID string) []ServiceMetadata {
	if collector.services == nil {
		return nil
	}
	out := []ServiceMetadata{}
	for _, item := range collector.services() {
		if storage.NormalizeOrgID(item.OrgID) == orgID {
			out = append(out, item)
		}
	}
	return out
}

func logSeverity(logs WindowAggregate) string {
	if logs.SeverityCounts["critical"] > 0 {
		return "critical"
	}
	if logs.SpikingPatterns > 0 || logs.UnknownPatterns > 0 || logs.SeverityCounts["error"] > 0 || logs.SeverityCounts["warning"] > 0 {
		return "pressure"
	}
	return "nominal"
}

func aggregateDomains(services []ServiceSnapshot) []DomainSnapshot {
	byName := map[string]*DomainSnapshot{}
	for _, service := range services {
		domain := byName[service.Domain]
		if domain == nil {
			domain = &DomainSnapshot{Name: service.Domain, Severity: "unknown"}
			byName[service.Domain] = domain
		}
		domain.ServiceCount++
		if service.Availability["logs"].State == core.HealthReady || service.Availability["internal"].State == core.HealthReady {
			domain.ObservedCount++
		}
		if service.Severity != "unknown" && service.Severity != "nominal" {
			domain.AffectedCount++
		}
		if severityRank(service.Severity) > severityRank(domain.Severity) {
			domain.Severity = service.Severity
		}
	}
	out := make([]DomainSnapshot, 0, len(byName))
	for _, domain := range byName {
		out = append(out, *domain)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func severityRank(value string) int {
	switch strings.ToLower(value) {
	case "degraded":
		return 5
	case "critical":
		return 4
	case "creeping":
		return 3
	case "pressure":
		return 2
	case "nominal":
		return 1
	default:
		return 0
	}
}

func latestSourceTimes(statuses []SourceStatus) (time.Time, time.Time) {
	var attempt, success time.Time
	for _, status := range statuses {
		if status.LatestAttempt.After(attempt) {
			attempt = status.LatestAttempt
		}
		if status.LatestSuccess.After(success) {
			success = status.LatestSuccess
		}
	}
	return attempt, success
}

func capabilities(logsConfigured bool) []Capability {
	logState := core.MeasureAvailability{State: core.HealthNotConfigured, ReasonCode: "source_not_configured", ActionID: "connect_log_source"}
	if logsConfigured {
		logState = core.MeasureAvailability{State: core.HealthCollecting, ReasonCode: "availability_is_service_scoped"}
	}
	return []Capability{
		{Family: "logs", Measures: map[string]core.MeasureAvailability{"activity": logState, "patterns": logState, "anomalies": logState}},
		{Family: "internal", Measures: map[string]core.MeasureAvailability{"incidents": {State: core.HealthReady}, "patterns": {State: core.HealthReady}}},
		{Family: "metrics", Measures: map[string]core.MeasureAvailability{"latency": {State: core.HealthRestricted, ReasonCode: "enterprise_required", ActionID: "review_enterprise_capability"}, "request_error_ratio": {State: core.HealthRestricted, ReasonCode: "enterprise_required", ActionID: "review_enterprise_capability"}, "throughput": {State: core.HealthRestricted, ReasonCode: "enterprise_required", ActionID: "review_enterprise_capability"}}},
		{Family: "traces", Measures: map[string]core.MeasureAvailability{"request_context": {State: core.HealthRestricted, ReasonCode: "enterprise_required", ActionID: "review_enterprise_capability"}}},
	}
}
