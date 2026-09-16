package servicehealth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	maxIncidentJoinRows      = 2000
	MaxEvidencePerService    = 64
	maxEvidenceIdentityBytes = 512
)

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

// AssessmentRequest contains only validated evidence and the current OSS
// snapshots. Assessors may enrich these snapshots but cannot replace them.
type AssessmentRequest struct {
	OrgID       string
	WindowStart time.Time
	WindowEnd   time.Time
	Evidence    []core.SignalEvidence
	Inputs      []core.HealthAssessmentInput
	Services    []ServiceSnapshot
}

// HealthAssessor computes optional source-neutral enrichment over validated evidence.
type HealthAssessor interface {
	Assess(context.Context, AssessmentRequest) ([]core.HealthAssessment, error)
}

// EnrichmentAccess revalidates whether premium enrichment may be persisted.
type EnrichmentAccess func(context.Context, string) error

// CollectorOptions supplies trusted, already-local readers. No option can pull a signal source.
type CollectorOptions struct {
	Manager           *Manager
	Store             storage.Provider
	Services          func() []ServiceMetadata
	Patterns          func() []PatternMetadata
	SourceIDs         []string
	Providers         []core.HealthProvider
	ProviderSourceIDs []string
	Assessor          HealthAssessor
	EnrichmentAccess  EnrichmentAccess
	Now               func() time.Time
}

// Collector builds and persists OSS snapshots without external reads.
type Collector struct {
	manager           *Manager
	store             storage.Provider
	services          func() []ServiceMetadata
	patterns          func() []PatternMetadata
	sourceIDs         []string
	providers         []core.HealthProvider
	providerSourceIDs []string
	assessor          HealthAssessor
	enrichmentAccess  EnrichmentAccess
	now               func() time.Time
}

// NewCollector creates a bounded snapshot collector.
func NewCollector(options CollectorOptions) *Collector {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{
		manager: options.Manager, store: options.Store, services: options.Services,
		patterns: options.Patterns, sourceIDs: append([]string(nil), options.SourceIDs...),
		providers:         append([]core.HealthProvider(nil), options.Providers...),
		providerSourceIDs: append([]string(nil), options.ProviderSourceIDs...),
		assessor:          options.Assessor, enrichmentAccess: options.EnrichmentAccess, now: now,
	}
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
	collector.enrich(ctx, orgID, settings, &snapshot)
	if collector.enrichmentAccess != nil && !safeEnrichmentAccess(ctx, collector.enrichmentAccess, orgID) {
		stripAccessControlledSnapshot(&snapshot)
	}
	if err := collector.manager.SaveSnapshot(orgID, snapshot); err != nil {
		return SnapshotEnvelope{}, err
	}
	return snapshot, nil
}

type evidenceCandidate struct {
	evidence      core.SignalEvidence
	input         *core.HealthAssessmentInput
	providerOrder int
}

func (collector *Collector) enrich(ctx context.Context, orgID string, settings Settings, snapshot *SnapshotEnvelope) {
	if len(collector.providers) == 0 && collector.assessor == nil {
		return
	}
	windowEnd := snapshot.GeneratedAt
	windowStart := windowEnd.Add(-time.Duration(settings.WindowSeconds) * time.Second)
	serviceIndex := make(map[string]int, len(snapshot.Services))
	serviceIDs := make([]string, 0, len(snapshot.Services))
	for index := range snapshot.Services {
		serviceIndex[snapshot.Services[index].Service] = index
		serviceIDs = append(serviceIDs, snapshot.Services[index].Service)
	}
	if len(serviceIDs) == 0 {
		return
	}
	sourceIDs := append([]string(nil), collector.providerSourceIDs...)
	if len(sourceIDs) > MaxPremiumSources {
		sourceIDs = sourceIDs[:MaxPremiumSources]
		snapshot.Coverage.Partial = true
	}
	remaining := core.HealthBudget{
		Requests: MaxExternalRequests, RequestsPerSource: MaxRequestsPerSource,
		Rows: MaxRowsPerSource * MaxPremiumSources, ResponseBytes: MaxResponseBytes,
		CumulativeBytes: MaxCumulativeBytes, Concurrency: MaxConcurrency,
	}
	timeout := 25 * time.Second
	if intervalLimit := time.Duration(settings.IntervalSeconds)*time.Second - 5*time.Second; intervalLimit < timeout {
		timeout = intervalLimit
	}
	providerCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := providerCtx.Deadline()
	candidates := make(map[string]evidenceCandidate)
	for providerOrder, provider := range collector.providers {
		if provider == nil || remaining.Requests == 0 || remaining.Rows == 0 || remaining.CumulativeBytes == 0 {
			snapshot.Coverage.Partial = true
			continue
		}
		providerCapabilities := safeProviderCapabilities(providerCtx, provider, orgID)
		request := core.HealthCollectRequest{
			OrgID: orgID, SourceIDs: append([]string(nil), sourceIDs...), ServiceIDs: append([]string(nil), serviceIDs...),
			WindowStart: windowStart, WindowEnd: windowEnd, Deadline: deadline, Budget: remaining,
		}
		collection, err := safeProviderCollect(providerCtx, provider, request)
		if err != nil {
			markProviderCapabilities(snapshot, providerCapabilities, core.HealthError, "provider_error")
			snapshot.Coverage.Partial = true
			continue
		}
		if !usageWithinBudget(collection.Usage, remaining) || !usageCoversEvidence(collection.Usage, collection.Evidence) {
			markProviderCapabilities(snapshot, providerCapabilities, core.HealthPartial, "provider_budget_exceeded")
			snapshot.Coverage.Partial = true
			continue
		}
		if collection.Partial {
			snapshot.Coverage.Partial = true
		}
		inputs := validAssessmentInputs(collection.AssessmentInputs, collection.Evidence, orgID)
		remaining.Requests -= collection.Usage.Requests
		remaining.Rows -= collection.Usage.Rows
		remaining.CumulativeBytes -= collection.Usage.ResponseBytes
		if len(collection.Evidence) > remaining.Rows+collection.Usage.Rows || !evidenceWithinSourceBounds(collection.Evidence) {
			markProviderCapabilities(snapshot, providerCapabilities, core.HealthPartial, "provider_budget_exceeded")
			snapshot.Coverage.Partial = true
			continue
		}
		mergeCapabilities(snapshot, providerCapabilities)
		for _, evidence := range collection.Evidence {
			if !validEvidence(evidence, orgID, serviceIndex, windowStart, windowEnd) {
				snapshot.Coverage.Partial = true
				continue
			}
			evidence.Availability.ActionID = allowedActionID(evidence.Availability.ActionID)
			evidence.Availability.ReasonCode = safeProviderReason(evidence.Availability.ReasonCode)
			key := evidenceKey(evidence)
			candidate := evidenceCandidate{evidence: evidence, input: inputs[assessmentInputKey(evidence)], providerOrder: providerOrder}
			if current, ok := candidates[key]; !ok || preferEvidence(candidate, current) {
				candidates[key] = candidate
			}
		}
	}
	selected := make([]evidenceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		selected = append(selected, candidate)
	}
	sort.Slice(selected, func(i, j int) bool {
		left, right := selected[i].evidence, selected[j].evidence
		return evidenceKey(left)+"\x00"+left.SourceRef < evidenceKey(right)+"\x00"+right.SourceRef
	})
	validated := make([]core.SignalEvidence, 0, len(selected))
	assessmentInputs := make([]core.HealthAssessmentInput, 0, len(selected))
	perService := map[string]int{}
	evidenceCapabilities := map[string]map[string]core.MeasureAvailability{}
	for _, candidate := range selected {
		evidence := candidate.evidence
		if perService[evidence.Service] >= MaxEvidencePerService {
			snapshot.Coverage.Partial = true
			continue
		}
		perService[evidence.Service]++
		index := serviceIndex[evidence.Service]
		snapshot.Services[index].Evidence = append(snapshot.Services[index].Evidence, evidence)
		snapshot.Services[index].Availability[evidence.Family+"."+evidence.Measure] = evidence.Availability
		snapshot.Services[index].AssessmentBasis = appendAssessmentBasis(snapshot.Services[index].AssessmentBasis, evidence.Family)
		if evidenceCapabilities[evidence.Family] == nil {
			evidenceCapabilities[evidence.Family] = map[string]core.MeasureAvailability{}
		}
		evidenceCapabilities[evidence.Family][evidence.Measure] = evidence.Availability
		validated = append(validated, evidence)
		if candidate.input != nil {
			assessmentInputs = append(assessmentInputs, *candidate.input)
		}
	}
	capabilityAdditions := make([]core.HealthCapability, 0, len(evidenceCapabilities))
	for family, measures := range evidenceCapabilities {
		capabilityAdditions = append(capabilityAdditions, core.HealthCapability{Family: family, Measures: measures})
	}
	mergeCapabilities(snapshot, capabilityAdditions)
	collector.assess(providerCtx, orgID, windowStart, windowEnd, validated, assessmentInputs, snapshot, serviceIndex)
}

func appendAssessmentBasis(basis, family string) string {
	label := ""
	switch family {
	case "metrics":
		label = "Metrics"
	case "traces":
		label = "Traces"
	}
	if label == "" {
		return basis
	}
	for _, part := range strings.Split(basis, " + ") {
		if part == label {
			return basis
		}
	}
	if basis == "" {
		return label
	}
	return basis + " + " + label
}

func (collector *Collector) assess(ctx context.Context, orgID string, windowStart, windowEnd time.Time, evidence []core.SignalEvidence, inputs []core.HealthAssessmentInput, snapshot *SnapshotEnvelope, serviceIndex map[string]int) {
	if collector.assessor == nil {
		return
	}
	assessments, err := safeAssess(ctx, collector.assessor, AssessmentRequest{
		OrgID: orgID, WindowStart: windowStart, WindowEnd: windowEnd,
		Evidence: append([]core.SignalEvidence(nil), evidence...), Inputs: append([]core.HealthAssessmentInput(nil), inputs...), Services: append([]ServiceSnapshot(nil), snapshot.Services...),
	})
	if err != nil {
		mergeCapabilities(snapshot, []core.HealthCapability{{Family: "assessment", Measures: map[string]core.MeasureAvailability{
			"regression_score": {State: core.HealthError, ReasonCode: "assessor_error"},
			"silent":           {State: core.HealthError, ReasonCode: "assessor_error"},
		}}})
		snapshot.Coverage.Partial = true
		return
	}
	for _, assessment := range assessments {
		index, ok := serviceIndex[assessment.Service]
		if !ok || !validAssessment(assessment, orgID, snapshot.Services[index], windowEnd) {
			snapshot.Coverage.Partial = true
			continue
		}
		copy := assessment
		snapshot.Services[index].Assessment = &copy
		if assessment.RaiseSeverity && severityRank(assessment.Severity) > severityRank(snapshot.Services[index].Severity) {
			snapshot.Services[index].BaseSeverity = snapshot.Services[index].Severity
			snapshot.Services[index].Severity = assessment.Severity
		}
	}
	mergeAssessmentCapabilities(snapshot)
	recomputeDerived(snapshot)
}

func safeProviderCapabilities(ctx context.Context, provider core.HealthProvider, orgID string) (capabilities []core.HealthCapability) {
	defer func() {
		if recover() != nil {
			capabilities = nil
		}
	}()
	return sanitizeCapabilities(provider.Capabilities(ctx, orgID))
}

func safeProviderCollect(ctx context.Context, provider core.HealthProvider, request core.HealthCollectRequest) (collection core.HealthCollection, err error) {
	defer func() {
		if recover() != nil {
			collection = core.HealthCollection{}
			err = fmt.Errorf("provider failed")
		}
	}()
	return provider.Collect(ctx, request)
}

func safeAssess(ctx context.Context, assessor HealthAssessor, request AssessmentRequest) (assessments []core.HealthAssessment, err error) {
	defer func() {
		if recover() != nil {
			assessments = nil
			err = fmt.Errorf("assessor failed")
		}
	}()
	return assessor.Assess(ctx, request)
}

func safeEnrichmentAccess(ctx context.Context, access EnrichmentAccess, orgID string) (allowed bool) {
	defer func() {
		if recover() != nil {
			allowed = false
		}
	}()
	return access(ctx, orgID) == nil
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

var supportedEvidenceMeasures = map[string]map[string]struct{}{
	"metrics": {
		"latency": {}, "request_error_ratio": {}, "throughput": {}, "apdex": {},
	},
	"traces": {
		"latency": {}, "request_error_ratio": {}, "throughput": {}, "request_context": {},
	},
	"assessment": {
		"regression_score": {}, "silent": {},
	},
}

var allowedHealthActions = map[string]struct{}{
	"connect_metric_source":        {},
	"review_metric_source":         {},
	"connect_trace_source":         {},
	"review_trace_source":          {},
	"review_enterprise_capability": {},
}

var allowedProviderReasons = map[string]struct{}{
	"authentication":              {},
	"entitlement":                 {},
	"insufficient_baseline":       {},
	"missing_distribution":        {},
	"missing_service_attribution": {},
	"missing_target":              {},
	"no_observations_in_window":   {},
	"permission":                  {},
	"sampled_data":                {},
	"source_not_configured":       {},
	"stale_baseline":              {},
	"timeout":                     {},
	"unsupported_measure":         {},
	"zero_traffic":                {},
}

var allowedAssessmentReasons = map[string]struct{}{
	"alert_state_unknown":    {},
	"baseline_regression":    {},
	"incident_state_unknown": {},
	"incompatible_baseline":  {},
	"insufficient_baseline":  {},
	"low_confidence":         {},
	"no_regression":          {},
	"score_withheld":         {},
	"stale_baseline":         {},
}

func usageWithinBudget(usage core.HealthUsage, budget core.HealthBudget) bool {
	if usage.Sources < 0 || usage.Requests < 0 || usage.Rows < 0 || usage.ResponseBytes < 0 || usage.LargestResponseBytes < 0 {
		return false
	}
	if usage.Sources > MaxPremiumSources || usage.Requests > budget.Requests || usage.Rows > budget.Rows || usage.ResponseBytes > budget.CumulativeBytes || usage.LargestResponseBytes > budget.ResponseBytes || usage.LargestResponseBytes > usage.ResponseBytes {
		return false
	}
	sources := usage.Sources
	if sources == 0 {
		sources = 1
	}
	return usage.Requests <= sources*budget.RequestsPerSource && usage.Rows <= sources*MaxRowsPerSource
}

func usageCoversEvidence(usage core.HealthUsage, evidence []core.SignalEvidence) bool {
	if len(evidence) == 0 {
		return true
	}
	sources := map[string]struct{}{}
	for _, item := range evidence {
		sources[item.SourceRef] = struct{}{}
	}
	return usage.Requests > 0 && usage.Rows >= len(evidence) && usage.Sources >= len(sources)
}

func evidenceWithinSourceBounds(evidence []core.SignalEvidence) bool {
	counts := map[string]int{}
	for _, item := range evidence {
		counts[item.SourceRef]++
		if len(counts) > MaxPremiumSources || counts[item.SourceRef] > MaxRowsPerSource {
			return false
		}
	}
	return true
}

func validEvidence(evidence core.SignalEvidence, orgID string, services map[string]int, windowStart, windowEnd time.Time) bool {
	if storage.NormalizeOrgID(evidence.OrgID) != orgID || evidence.Service == "" || len(evidence.Service) > maxEvidenceIdentityBytes {
		return false
	}
	if _, ok := services[evidence.Service]; !ok || len(evidence.Operation) > maxEvidenceIdentityBytes || evidence.SourceRef == "" || len(evidence.SourceRef) > maxEvidenceIdentityBytes || len(evidence.SignalRef) > maxEvidenceIdentityBytes {
		return false
	}
	if (evidence.Family != "metrics" && evidence.Family != "traces") || !supportedEvidenceMeasure(evidence.Family, evidence.Measure) || !validHealthState(evidence.Availability.State) {
		return false
	}
	if !evidence.WindowStart.Equal(windowStart) || !evidence.WindowEnd.Equal(windowEnd) || evidence.ObservedAt.After(windowEnd) {
		return false
	}
	if evidence.Availability.State != core.HealthStale && evidence.ObservedAt.Before(windowStart) {
		return false
	}
	if nonFinite(evidence.Value) || nonFinite(evidence.Numerator) || nonFinite(evidence.Denominator) {
		return false
	}
	if evidence.Availability.State == core.HealthReady && evidence.Value == nil && (evidence.Numerator == nil || evidence.Denominator == nil) {
		return false
	}
	if evidence.Measure == "request_error_ratio" && evidence.Numerator == nil && evidence.Denominator == nil {
		if evidence.Value != nil && (*evidence.Value < 0 || *evidence.Value > 1) {
			return false
		}
	} else if evidence.Numerator != nil || evidence.Denominator != nil {
		if evidence.Numerator == nil || evidence.Denominator == nil || *evidence.Denominator <= 0 || *evidence.Numerator < 0 || *evidence.Numerator > *evidence.Denominator {
			return false
		}
	}
	return true
}

func nonFinite(value *float64) bool {
	return value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0))
}

func supportedEvidenceMeasure(family, measure string) bool {
	measures, ok := supportedEvidenceMeasures[family]
	if !ok {
		return false
	}
	_, ok = measures[measure]
	return ok
}

func validHealthState(state core.HealthState) bool {
	switch state {
	case core.HealthReady, core.HealthPartial, core.HealthNotConfigured, core.HealthCollecting, core.HealthNoData, core.HealthUnsupported, core.HealthError, core.HealthStale, core.HealthRestricted:
		return true
	default:
		return false
	}
}

func allowedActionID(actionID string) string {
	if _, ok := allowedHealthActions[actionID]; ok {
		return actionID
	}
	return ""
}

func safeProviderReason(reason string) string {
	if reason == "" {
		return ""
	}
	if _, ok := allowedProviderReasons[reason]; ok {
		return reason
	}
	return "provider_status_unavailable"
}

func evidenceKey(evidence core.SignalEvidence) string {
	return strings.Join([]string{
		storage.NormalizeOrgID(evidence.OrgID), evidence.Service, evidence.Operation,
		evidence.Family, evidence.Measure, evidence.WindowStart.UTC().Format(time.RFC3339Nano),
		evidence.WindowEnd.UTC().Format(time.RFC3339Nano),
	}, "\x00")
}

func assessmentInputKey(evidence core.SignalEvidence) string {
	return strings.Join([]string{
		storage.NormalizeOrgID(evidence.OrgID), evidence.Service, evidence.Operation,
		evidence.Family, evidence.Measure, evidence.SourceRef, evidence.SignalRef,
	}, "\x00")
}

func validAssessmentInputs(inputs []core.HealthAssessmentInput, evidence []core.SignalEvidence, orgID string) map[string]*core.HealthAssessmentInput {
	evidenceKeys := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		evidenceKeys[assessmentInputKey(item)] = struct{}{}
	}
	validated := make(map[string]*core.HealthAssessmentInput, len(inputs))
	for index := range inputs {
		item := &inputs[index]
		key := assessmentInputKey(core.SignalEvidence{
			OrgID: item.OrgID, Service: item.Service, Operation: item.Operation,
			Family: item.Family, Measure: item.Measure, SourceRef: item.SourceRef, SignalRef: item.SignalRef,
		})
		if storage.NormalizeOrgID(item.OrgID) != orgID || math.IsNaN(item.ExpectedMean) || math.IsInf(item.ExpectedMean, 0) || math.IsNaN(item.ExpectedStd) || math.IsInf(item.ExpectedStd, 0) || item.ExpectedStd < 0 || item.ObservationCount < 0 || item.MaturityThreshold < 0 {
			continue
		}
		if _, ok := evidenceKeys[key]; !ok {
			continue
		}
		if _, duplicate := validated[key]; !duplicate {
			copy := *item
			validated[key] = &copy
		}
	}
	return validated
}

func preferEvidence(candidate, current evidenceCandidate) bool {
	candidateReady := candidate.evidence.Availability.State == core.HealthReady
	currentReady := current.evidence.Availability.State == core.HealthReady
	if candidateReady != currentReady {
		return candidateReady
	}
	if !candidate.evidence.FreshUntil.Equal(current.evidence.FreshUntil) {
		return candidate.evidence.FreshUntil.After(current.evidence.FreshUntil)
	}
	if !candidate.evidence.ObservedAt.Equal(current.evidence.ObservedAt) {
		return candidate.evidence.ObservedAt.After(current.evidence.ObservedAt)
	}
	if candidate.providerOrder != current.providerOrder {
		return candidate.providerOrder < current.providerOrder
	}
	return candidate.evidence.SourceRef < current.evidence.SourceRef
}

func sanitizeCapabilities(capabilities []core.HealthCapability) []core.HealthCapability {
	out := make([]core.HealthCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability.Family != "metrics" && capability.Family != "traces" {
			continue
		}
		measures := map[string]core.MeasureAvailability{}
		for measure, availability := range capability.Measures {
			if !supportedEvidenceMeasure(capability.Family, measure) || !validHealthState(availability.State) {
				continue
			}
			availability.ActionID = allowedActionID(availability.ActionID)
			availability.ReasonCode = safeProviderReason(availability.ReasonCode)
			measures[measure] = availability
		}
		if len(measures) > 0 {
			out = append(out, core.HealthCapability{Family: capability.Family, Measures: measures})
		}
	}
	return out
}

func mergeCapabilities(snapshot *SnapshotEnvelope, additions []core.HealthCapability) {
	for _, addition := range additions {
		index := -1
		for existingIndex := range snapshot.Capabilities {
			if snapshot.Capabilities[existingIndex].Family == addition.Family {
				index = existingIndex
				break
			}
		}
		if index < 0 {
			snapshot.Capabilities = append(snapshot.Capabilities, Capability{Family: addition.Family, Measures: map[string]core.MeasureAvailability{}})
			index = len(snapshot.Capabilities) - 1
		}
		for measure, availability := range addition.Measures {
			current, exists := snapshot.Capabilities[index].Measures[measure]
			if !exists || current.State == core.HealthRestricted {
				snapshot.Capabilities[index].Measures[measure] = availability
				continue
			}
			snapshot.Capabilities[index].Measures[measure] = combineCapabilityAvailability(current, availability)
		}
	}
	sort.SliceStable(snapshot.Capabilities, func(i, j int) bool { return snapshot.Capabilities[i].Family < snapshot.Capabilities[j].Family })
}

func combineCapabilityAvailability(current, next core.MeasureAvailability) core.MeasureAvailability {
	if current.State == next.State {
		if current.ReasonCode == "" {
			return current
		}
		return next
	}
	if (current.State == core.HealthReady && (next.State == core.HealthError || next.State == core.HealthPartial)) ||
		(next.State == core.HealthReady && (current.State == core.HealthError || current.State == core.HealthPartial)) {
		return core.MeasureAvailability{State: core.HealthPartial, ReasonCode: "provider_partial"}
	}
	return next
}

func markProviderCapabilities(snapshot *SnapshotEnvelope, capabilities []core.HealthCapability, state core.HealthState, reason string) {
	marked := make([]core.HealthCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		measures := make(map[string]core.MeasureAvailability, len(capability.Measures))
		actionID := "review_metric_source"
		if capability.Family == "traces" {
			actionID = "review_trace_source"
		}
		for measure := range capability.Measures {
			measures[measure] = core.MeasureAvailability{State: state, ReasonCode: reason, ActionID: actionID}
		}
		marked = append(marked, core.HealthCapability{Family: capability.Family, Measures: measures})
	}
	mergeCapabilities(snapshot, marked)
}

func validAssessment(assessment core.HealthAssessment, orgID string, service ServiceSnapshot, windowEnd time.Time) bool {
	if storage.NormalizeOrgID(assessment.OrgID) != orgID || assessment.Service != service.Service || assessment.AlgorithmVersion == "" {
		return false
	}
	if math.IsNaN(assessment.Confidence) || math.IsInf(assessment.Confidence, 0) || assessment.Confidence < 0 || assessment.Confidence > 1 || nonFinite(assessment.RegressionScore) {
		return false
	}
	if assessment.RegressionScore != nil && (*assessment.RegressionScore < 0 || *assessment.RegressionScore > 100) {
		return false
	}
	if (assessment.RegressionScore == nil || assessment.Silent == nil) && assessment.ReasonCode == "" {
		return false
	}
	if assessment.ReasonCode != "" {
		if _, ok := allowedAssessmentReasons[assessment.ReasonCode]; !ok {
			return false
		}
	}
	if assessment.AssessedAt.IsZero() || assessment.AssessedAt.After(windowEnd) || (!assessment.FreshUntil.IsZero() && assessment.FreshUntil.Before(assessment.AssessedAt)) {
		return false
	}
	if assessment.Silent != nil {
		if service.ActiveIncidents == nil {
			return false
		}
		if *assessment.Silent && (*service.ActiveIncidents != 0 || !assessment.AlertStateKnown) {
			return false
		}
		if !*assessment.Silent && *service.ActiveIncidents == 0 && !assessment.AlertStateKnown {
			return false
		}
	}
	if assessment.Severity != "" && severityRank(assessment.Severity) == 0 {
		return false
	}
	for _, family := range assessment.IncludedFamilies {
		if !supportedAssessmentFamily(family) {
			return false
		}
	}
	for _, driver := range assessment.Drivers {
		if !supportedAssessmentDriver(driver.Family, driver.Measure) || math.IsNaN(driver.Weight) || math.IsInf(driver.Weight, 0) {
			return false
		}
	}
	return true
}

func supportedAssessmentFamily(family string) bool {
	return family == "logs" || family == "internal" || family == "metrics" || family == "traces"
}

func supportedAssessmentDriver(family, measure string) bool {
	switch family {
	case "logs":
		return measure == "activity" || measure == "patterns" || measure == "anomalies"
	case "internal":
		return measure == "incidents" || measure == "patterns"
	default:
		return supportedEvidenceMeasure(family, measure) && family != "assessment"
	}
}

func mergeAssessmentCapabilities(snapshot *SnapshotEnvelope) {
	scoreState := core.MeasureAvailability{State: core.HealthNoData, ReasonCode: "assessment_withheld"}
	silentState := core.MeasureAvailability{State: core.HealthNoData, ReasonCode: "assessment_withheld"}
	for _, service := range snapshot.Services {
		if service.Assessment == nil {
			continue
		}
		if service.Assessment.RegressionScore != nil {
			scoreState = core.MeasureAvailability{State: core.HealthReady}
		}
		if service.Assessment.Silent != nil {
			silentState = core.MeasureAvailability{State: core.HealthReady}
		}
	}
	mergeCapabilities(snapshot, []core.HealthCapability{{Family: "assessment", Measures: map[string]core.MeasureAvailability{
		"regression_score": scoreState, "silent": silentState,
	}}})
}

func recomputeDerived(snapshot *SnapshotEnvelope) {
	snapshot.Domains = aggregateDomains(snapshot.Services)
	snapshot.Facets = map[string]int{}
	for _, service := range snapshot.Services {
		snapshot.Facets[service.Severity]++
		if service.Assessment != nil && service.Assessment.Regressing {
			snapshot.Facets["regressing"]++
		}
		if service.Assessment != nil && service.Assessment.Silent != nil && *service.Assessment.Silent {
			snapshot.Facets["silent_regressions"]++
		}
	}
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
