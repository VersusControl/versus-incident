package servicehealth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type fakeHealthProvider struct {
	capabilities []core.HealthCapability
	collection   core.HealthCollection
	err          error
	calls        int
	request      core.HealthCollectRequest
}

func (provider *fakeHealthProvider) Capabilities(context.Context, string) []core.HealthCapability {
	return provider.capabilities
}

func (provider *fakeHealthProvider) Collect(_ context.Context, request core.HealthCollectRequest) (core.HealthCollection, error) {
	provider.calls++
	provider.request = request
	return provider.collection, provider.err
}

type assessorFunc func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error)

func (assessor assessorFunc) Assess(ctx context.Context, request servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
	return assessor(ctx, request)
}

func TestNilExtensionsPreserveExactOSSShapeAndPerformNoReads(t *testing.T) {
	now := healthTestNow()
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: servicehealth.NewManager(storage.NewMemory()), Store: storage.NewMemory(),
		Now: func() time.Time { return now }, Services: oneHealthService,
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"evidence"`, `"assessment"`, `"base_severity"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("nil extension changed OSS JSON with %s: %s", forbidden, data)
		}
	}
}

func TestProviderFamilyCombinations(t *testing.T) {
	for _, test := range []struct {
		name             string
		families         []string
		wantMetrics      int
		wantTraces       int
		wantProviderCall int
	}{
		{name: "none"},
		{name: "metrics", families: []string{"metrics"}, wantMetrics: 1, wantProviderCall: 1},
		{name: "traces", families: []string{"traces"}, wantTraces: 1, wantProviderCall: 1},
		{name: "all", families: []string{"metrics", "traces"}, wantMetrics: 1, wantTraces: 1, wantProviderCall: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := healthTestNow()
			providers := make([]core.HealthProvider, 0, len(test.families))
			fakes := make([]*fakeHealthProvider, 0, len(test.families))
			for _, family := range test.families {
				measure := "latency"
				if family == "traces" {
					measure = "request_context"
				}
				provider := providerWithEvidence(healthEvidence(now, family, measure, "source-"+family, ""))
				providers = append(providers, provider)
				fakes = append(fakes, provider)
			}
			snapshot := collectHealthSnapshot(t, now, providers, nil)
			metrics, traces, calls := 0, 0, 0
			for _, evidence := range snapshot.Services[0].Evidence {
				switch evidence.Family {
				case "metrics":
					metrics++
				case "traces":
					traces++
				}
			}
			for _, provider := range fakes {
				calls += provider.calls
			}
			if metrics != test.wantMetrics || traces != test.wantTraces || calls != test.wantProviderCall {
				t.Fatalf("evidence=%#v calls=%d", snapshot.Services[0].Evidence, calls)
			}
		})
	}
}

func TestProviderFamilyCombinationsPreserveLogAndInternalEvidence(t *testing.T) {
	for _, families := range [][]string{nil, {"metrics"}, {"traces"}, {"metrics", "traces"}} {
		for _, withLogs := range []bool{false, true} {
			for _, withIncident := range []bool{false, true} {
				name := fmt.Sprintf("families=%v/logs=%t/incident=%t", families, withLogs, withIncident)
				t.Run(name, func(t *testing.T) {
					now := healthTestNow()
					store := storage.NewMemory()
					manager := servicehealth.NewManager(store)
					sourceIDs := []string(nil)
					if withLogs {
						sourceIDs = []string{"logs"}
						if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "logs", Service: "api", PatternID: "p", ObservedAt: now.Add(-time.Minute), Frequency: 3}); err != nil {
							t.Fatal(err)
						}
					}
					if withIncident {
						if err := store.SaveIncident(&storage.IncidentRecord{ID: "incident", OrgID: "org-a", Service: "api", CreatedAt: now.Add(-time.Minute)}); err != nil {
							t.Fatal(err)
						}
					}
					providers := make([]core.HealthProvider, 0, len(families))
					for _, family := range families {
						providers = append(providers, providerWithEvidence(healthEvidence(now, family, "latency", "source-"+family, "")))
					}
					collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
						Manager: manager, Store: store, SourceIDs: sourceIDs, Services: oneHealthService,
						Providers: providers, Now: func() time.Time { return now },
					})
					snapshot, err := collector.Collect(context.Background(), "org-a")
					if err != nil {
						t.Fatal(err)
					}
					service := snapshot.Services[0]
					wantLogs := int64(0)
					if withLogs {
						wantLogs = 3
					}
					wantIncidents := 0
					if withIncident {
						wantIncidents = 1
					}
					if service.Logs.MatchedLogs != wantLogs || service.ActiveIncidents == nil || *service.ActiveIncidents != wantIncidents || len(service.Evidence) != len(families) {
						t.Fatalf("service=%+v, want logs=%d incidents=%d evidence=%d", service, wantLogs, wantIncidents, len(families))
					}
				})
			}
		}
	}
}

func TestProviderFailurePreservesOSSEvidenceAndHidesRawError(t *testing.T) {
	now := healthTestNow()
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "logs", Service: "api", PatternID: "p", ObservedAt: now.Add(-time.Minute), Frequency: 3}); err != nil {
		t.Fatal(err)
	}
	provider := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	provider.err = errors.New("secret provider token and backend inventory")
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: manager, Store: store, SourceIDs: []string{"logs"}, Providers: []core.HealthProvider{provider},
		Now: func() time.Time { return now }, Services: oneHealthService,
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	if snapshot.Services[0].Logs.MatchedLogs != 3 || len(snapshot.Services[0].Evidence) != 0 || !snapshot.Coverage.Partial {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	capability := capabilityMeasure(t, snapshot, "metrics", "latency")
	if capability.State != core.HealthError || capability.ReasonCode != "provider_error" || capability.ActionID != "review_metric_source" || strings.Contains(string(data), "secret") || strings.Contains(string(data), "backend inventory") {
		t.Fatalf("capability=%#v json=%s", capability, data)
	}
}

func TestAcceptedEvidenceUpdatesCapabilityAndAssessmentBasis(t *testing.T) {
	now := healthTestNow()
	metric := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	metric.capabilities[0].Measures["latency"] = core.MeasureAvailability{State: core.HealthCollecting, ReasonCode: "insufficient_baseline"}
	traceEvidence := healthEvidence(now, "traces", "latency", "traces-a", "GET /cart")
	traceEvidence.Availability = core.MeasureAvailability{State: core.HealthStale, ReasonCode: "stale_baseline", ActionID: "review_trace_source"}
	trace := providerWithEvidence(traceEvidence)
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{trace, metric}, nil)
	if snapshot.Services[0].AssessmentBasis != "Internal data only + Metrics + Traces" {
		t.Fatalf("assessment basis=%q", snapshot.Services[0].AssessmentBasis)
	}
	if got := capabilityMeasure(t, snapshot, "metrics", "latency"); got.State != core.HealthReady {
		t.Fatalf("metric capability=%+v", got)
	}
	if got := capabilityMeasure(t, snapshot, "traces", "latency"); got.State != core.HealthStale || got.ActionID != "review_trace_source" {
		t.Fatalf("trace capability=%+v", got)
	}
}

func TestMalformedEvidenceIsRejected(t *testing.T) {
	now := healthTestNow()
	valid := healthEvidence(now, "metrics", "latency", "metrics-a", "")
	nan := math.NaN()
	zero := 0.0
	one := 1.0
	cases := map[string]core.SignalEvidence{
		"foreign org":         mutateEvidence(valid, func(item *core.SignalEvidence) { item.OrgID = "org-b" }),
		"foreign service":     mutateEvidence(valid, func(item *core.SignalEvidence) { item.Service = "worker" }),
		"stale":               mutateEvidence(valid, func(item *core.SignalEvidence) { item.ObservedAt = item.WindowStart.Add(-time.Second) }),
		"future":              mutateEvidence(valid, func(item *core.SignalEvidence) { item.ObservedAt = item.WindowEnd.Add(time.Second) }),
		"wrong window":        mutateEvidence(valid, func(item *core.SignalEvidence) { item.WindowStart = item.WindowStart.Add(time.Minute) }),
		"nonfinite":           mutateEvidence(valid, func(item *core.SignalEvidence) { item.Value = &nan }),
		"unsupported family":  mutateEvidence(valid, func(item *core.SignalEvidence) { item.Family = "profiles" }),
		"unsupported measure": mutateEvidence(valid, func(item *core.SignalEvidence) { item.Measure = "median" }),
		"zero denominator": mutateEvidence(valid, func(item *core.SignalEvidence) {
			item.Measure, item.Numerator, item.Denominator = "request_error_ratio", &one, &zero
		}),
	}
	for name, evidence := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{providerWithEvidence(evidence)}, nil)
			if len(snapshot.Services[0].Evidence) != 0 || !snapshot.Coverage.Partial {
				t.Fatalf("invalid evidence accepted: %#v", snapshot.Services[0].Evidence)
			}
		})
	}
}

func TestScalarRatioAndExplicitStaleEvidenceAreAccepted(t *testing.T) {
	now := healthTestNow()
	ratio := healthEvidence(now, "metrics", "request_error_ratio", "metrics-a", "")
	value := .025
	ratio.Value = &value
	ratio.Numerator = nil
	ratio.Denominator = nil
	collecting := ratio
	collecting.Value = nil
	collecting.Availability = core.MeasureAvailability{State: core.HealthCollecting, ReasonCode: "insufficient_baseline"}
	stale := healthEvidence(now, "metrics", "latency", "metrics-a", "")
	stale.ObservedAt = stale.WindowStart.Add(-time.Second)
	stale.Availability = core.MeasureAvailability{State: core.HealthStale, ReasonCode: "stale_baseline"}

	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{providerWithEvidence(ratio, collecting, stale)}, nil)
	if len(snapshot.Services[0].Evidence) != 2 {
		t.Fatalf("evidence=%#v, want scalar ratio plus stale latency after identity dedup", snapshot.Services[0].Evidence)
	}
	if snapshot.Services[0].Evidence[0].Numerator != nil || snapshot.Services[0].Evidence[0].Denominator != nil {
		t.Fatalf("scalar ratio gained fabricated counts: %#v", snapshot.Services[0].Evidence[0])
	}
}

func TestEvidenceDeduplicationPriorityIsDeterministic(t *testing.T) {
	now := healthTestNow()
	partial := healthEvidence(now, "metrics", "latency", "source-z", "")
	partial.Availability.State = core.HealthPartial
	partial.FreshUntil = now.Add(10 * time.Minute)
	readyOld := healthEvidence(now, "metrics", "latency", "source-a", "")
	*readyOld.Value = 2
	readyOld.FreshUntil = now.Add(time.Minute)
	readyFresh := healthEvidence(now, "metrics", "latency", "source-b", "")
	*readyFresh.Value = 3
	readyFresh.FreshUntil = now.Add(2 * time.Minute)
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{
		providerWithEvidence(partial), providerWithEvidence(readyOld), providerWithEvidence(readyFresh),
	}, nil)
	if len(snapshot.Services[0].Evidence) != 1 || *snapshot.Services[0].Evidence[0].Value != 3 || snapshot.Services[0].Evidence[0].SourceRef != "source-b" {
		t.Fatalf("priority result=%#v", snapshot.Services[0].Evidence)
	}

	first := healthEvidence(now, "metrics", "throughput", "source-z", "")
	second := healthEvidence(now, "metrics", "throughput", "source-a", "")
	*first.Value, *second.Value = 4, 5
	provider := providerWithEvidence(first, second)
	provider.collection.Usage.Sources = 2
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{provider}, nil)
	if len(snapshot.Services[0].Evidence) != 1 || snapshot.Services[0].Evidence[0].SourceRef != "source-a" || *snapshot.Services[0].Evidence[0].Value != 5 {
		t.Fatalf("source order result=%#v", snapshot.Services[0].Evidence)
	}

	providerFirst := healthEvidence(now, "metrics", "latency", "source-z", "")
	providerSecond := healthEvidence(now, "metrics", "latency", "source-a", "")
	*providerFirst.Value, *providerSecond.Value = 6, 7
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{providerWithEvidence(providerFirst), providerWithEvidence(providerSecond)}, nil)
	if len(snapshot.Services[0].Evidence) != 1 || snapshot.Services[0].Evidence[0].SourceRef != "source-z" || *snapshot.Services[0].Evidence[0].Value != 6 {
		t.Fatalf("provider order result=%#v", snapshot.Services[0].Evidence)
	}
}

func TestCapabilitiesAndEvidenceUseOnlyAllowlistedActions(t *testing.T) {
	now := healthTestNow()
	evidence := healthEvidence(now, "metrics", "latency", "metrics-a", "")
	evidence.Availability.ActionID = "https://provider.invalid/secret"
	evidence.Availability.ReasonCode = "raw backend host and credential"
	provider := providerWithEvidence(evidence)
	provider.capabilities = []core.HealthCapability{
		{Family: "metrics", Measures: map[string]core.MeasureAvailability{"latency": {State: core.HealthReady, ReasonCode: "raw backend host and credential", ActionID: "arbitrary_action"}}},
		{Family: "profiles", Measures: map[string]core.MeasureAvailability{"cpu": {State: core.HealthReady}}},
	}
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, nil)
	if snapshot.Services[0].Evidence[0].Availability.ActionID != "" || snapshot.Services[0].Evidence[0].Availability.ReasonCode != "provider_status_unavailable" || capabilityMeasure(t, snapshot, "metrics", "latency").ActionID != "" || capabilityMeasure(t, snapshot, "metrics", "latency").ReasonCode != "provider_status_unavailable" || hasCapability(snapshot, "profiles") {
		t.Fatalf("unsafe capability output=%#v evidence=%#v", snapshot.Capabilities, snapshot.Services[0].Evidence)
	}
}

func TestOperationEvidenceIsNeverCombined(t *testing.T) {
	now := healthTestNow()
	checkout := healthEvidence(now, "traces", "latency", "traces-a", "checkout")
	search := healthEvidence(now, "traces", "latency", "traces-a", "search")
	*checkout.Value, *search.Value = 120, 450
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{providerWithEvidence(checkout, search)}, nil)
	if len(snapshot.Services[0].Evidence) != 2 || snapshot.Services[0].Evidence[0].Operation == snapshot.Services[0].Evidence[1].Operation {
		t.Fatalf("operations combined: %#v", snapshot.Services[0].Evidence)
	}
}

func TestAssessorReceivesValidatedEvidenceAndCannotEraseBaseHealth(t *testing.T) {
	now := healthTestNow()
	provider := providerWithEvidence(
		healthEvidence(now, "metrics", "latency", "metrics-a", ""),
		mutateEvidence(healthEvidence(now, "metrics", "throughput", "metrics-a", ""), func(item *core.SignalEvidence) { item.OrgID = "org-b" }),
	)
	assessor := assessorFunc(func(_ context.Context, request servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		if len(request.Evidence) != 1 || len(request.Services) != 1 || request.Services[0].Service != "api" {
			t.Fatalf("request=%#v", request)
		}
		return []core.HealthAssessment{{
			OrgID: "org-a", Service: "api", Confidence: 0.7, ReasonCode: "insufficient_baseline",
			AlgorithmVersion: "test-v1", AssessedAt: now,
			IncludedFamilies: []string{"logs", "metrics"},
			Drivers:          []core.AssessmentDriver{{Family: "logs", Measure: "anomalies"}, {Family: "metrics", Measure: "latency"}},
		}}, nil
	})
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, assessor)
	if snapshot.Services[0].Assessment == nil || snapshot.Services[0].Assessment.RegressionScore != nil || snapshot.Services[0].Severity != "unknown" {
		t.Fatalf("snapshot=%#v", snapshot.Services[0])
	}
}

func TestAssessorFailureAndUnknownSilentStatePreserveBaseHealth(t *testing.T) {
	now := healthTestNow()
	provider := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	failing := assessorFunc(func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		return nil, errors.New("raw model endpoint and token")
	})
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, failing)
	if snapshot.Services[0].Severity != "unknown" || snapshot.Services[0].Assessment != nil || capabilityMeasure(t, snapshot, "assessment", "regression_score").ReasonCode != "assessor_error" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	data, _ := json.Marshal(snapshot)
	if strings.Contains(string(data), "raw model") || strings.Contains(string(data), "token") {
		t.Fatalf("raw assessor error leaked: %s", data)
	}

	silent := true
	unknownSilent := assessorFunc(func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		return []core.HealthAssessment{{OrgID: "org-a", Service: "api", Silent: &silent, Confidence: 1, AlgorithmVersion: "test-v1", AssessedAt: now}}, nil
	})
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{provider}, unknownSilent)
	if snapshot.Services[0].Assessment != nil || !snapshot.Coverage.Partial {
		t.Fatalf("silent claim accepted without authoritative alert state: %#v", snapshot.Services[0])
	}

	knownSilent := assessorFunc(func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		return []core.HealthAssessment{{
			OrgID: "org-a", Service: "api", Silent: &silent, AlertStateKnown: true,
			Confidence: 1, ReasonCode: "score_withheld", AlgorithmVersion: "test-v1", AssessedAt: now,
		}}, nil
	})
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{provider}, knownSilent)
	if snapshot.Services[0].Assessment == nil || snapshot.Services[0].Assessment.Silent == nil || !*snapshot.Services[0].Assessment.Silent {
		t.Fatalf("authoritative silent assessment withheld: %#v", snapshot.Services[0])
	}
}

func TestAssessmentSeverityRequiresExplicitRaiseContract(t *testing.T) {
	now := healthTestNow()
	score := 80.0
	makeAssessor := func(raise bool) assessorFunc {
		return func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
			return []core.HealthAssessment{{
				OrgID: "org-a", Service: "api", RegressionScore: &score, Confidence: 0.9,
				ReasonCode: "baseline_regression", AlgorithmVersion: "test-v1", AssessedAt: now,
				Severity: "creeping", RaiseSeverity: raise,
			}}, nil
		}
	}
	provider := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	preserved := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, makeAssessor(false))
	raised := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, makeAssessor(true))
	if preserved.Services[0].Severity != "unknown" || raised.Services[0].Severity != "creeping" || raised.Services[0].BaseSeverity != "unknown" {
		t.Fatalf("preserved=%#v raised=%#v", preserved.Services[0], raised.Services[0])
	}
}

func TestEnrichmentAccessDenialStripsBeforePersistence(t *testing.T) {
	now := healthTestNow()
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	score := 80.0
	assessor := assessorFunc(func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		return []core.HealthAssessment{{
			OrgID: "org-a", Service: "api", RegressionScore: &score, Confidence: 1,
			ReasonCode: "baseline_regression", AlgorithmVersion: "test-v1", AssessedAt: now,
			Severity: "critical", RaiseSeverity: true,
		}}, nil
	})
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: manager, Store: store, Services: oneHealthService,
		Providers: []core.HealthProvider{providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))},
		Assessor:  assessor, EnrichmentAccess: func(context.Context, string) error { return errors.New("access lost") },
		Now: func() time.Time { return now },
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok, err := manager.LoadSnapshot("org-a")
	if err != nil || !ok {
		t.Fatalf("persisted=%#v ok=%t err=%v", persisted, ok, err)
	}
	for _, candidate := range []servicehealth.SnapshotEnvelope{snapshot, persisted} {
		if len(candidate.Services[0].Evidence) != 0 || candidate.Services[0].Assessment != nil || candidate.Services[0].Severity != "unknown" || candidate.Services[0].BaseSeverity != "" || candidate.Services[0].AssessmentBasis != "Internal data only" || candidate.Coverage.Partial || !hasCapability(candidate, "metrics") || !hasCapability(candidate, "traces") || hasCapability(candidate, "assessment") {
			t.Fatalf("premium enrichment persisted after access denial: %#v", candidate)
		}
		if capabilityMeasure(t, candidate, "metrics", "latency").State != core.HealthRestricted || capabilityMeasure(t, candidate, "traces", "request_context").State != core.HealthRestricted {
			t.Fatalf("community capability shape not restored: %#v", candidate.Capabilities)
		}
	}
}

func TestCachedEnrichmentIsStrippedAfterAccessLossAndCrossOrgProjection(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Now().UTC()
	score := 70.0
	snapshot := servicehealth.SnapshotEnvelope{
		SnapshotID: "premium", GeneratedAt: now, SettingsRevision: 0, WindowSeconds: 300,
		Services: []servicehealth.ServiceSnapshot{{
			OrgID: "org-a", Service: "api", Domain: "Ungrouped", Severity: "creeping", BaseSeverity: "unknown",
			Availability: map[string]core.MeasureAvailability{"internal": {State: core.HealthNoData}, "metrics.latency": {State: core.HealthReady}},
			Evidence:     []core.SignalEvidence{{OrgID: "org-a", Service: "api", Family: "metrics", Measure: "latency", SourceRef: "metrics-a"}},
			Assessment:   &core.HealthAssessment{OrgID: "org-a", Service: "api", RegressionScore: &score},
		}},
		Facets:       map[string]int{"creeping": 1, "regressing": 1},
		Capabilities: []servicehealth.Capability{{Family: "internal", Measures: map[string]core.MeasureAvailability{"incidents": {State: core.HealthReady}}}, {Family: "metrics", Measures: map[string]core.MeasureAvailability{"latency": {State: core.HealthReady}}}},
	}
	if err := manager.SaveSnapshot("org-a", snapshot); err != nil {
		t.Fatal(err)
	}
	allowed := true
	manager.SetSnapshotProjection(func(_ context.Context, orgID string, snapshot servicehealth.SnapshotEnvelope) (servicehealth.SnapshotEnvelope, error) {
		if orgID != "org-a" {
			t.Fatalf("projection org=%q", orgID)
		}
		if !allowed {
			return servicehealth.SnapshotEnvelope{}, errors.New("license lookup failed with secret")
		}
		return snapshot, nil
	})
	visible, err := manager.SnapshotFor(context.Background(), "org-a")
	if err != nil || len(visible.Services[0].Evidence) != 1 {
		t.Fatalf("visible=%#v err=%v", visible, err)
	}
	allowed = false
	stripped, err := manager.SnapshotFor(context.Background(), "org-a")
	if err != nil || len(stripped.Services[0].Evidence) != 0 || stripped.Services[0].Assessment != nil || stripped.Services[0].Severity != "unknown" || !hasCapability(stripped, "metrics") || !hasCapability(stripped, "traces") || hasCapability(stripped, "assessment") || stripped.Facets["regressing"] != 0 || stripped.Facets["creeping"] != 0 || stripped.Facets["unknown"] != 1 {
		t.Fatalf("stripped=%#v err=%v", stripped, err)
	}
	if got := capabilityMeasure(t, stripped, "metrics", "latency"); got.State != core.HealthRestricted || got.ReasonCode != "enterprise_required" || got.ActionID != "review_enterprise_capability" {
		t.Fatalf("restored metric capability=%#v", got)
	}
	if got := capabilityMeasure(t, stripped, "traces", "request_context"); got.State != core.HealthRestricted || got.ReasonCode != "enterprise_required" || got.ActionID != "review_enterprise_capability" {
		t.Fatalf("restored trace capability=%#v", got)
	}

	manager.SetSnapshotProjection(func(_ context.Context, _ string, snapshot servicehealth.SnapshotEnvelope) (servicehealth.SnapshotEnvelope, error) {
		snapshot.Services[0].OrgID = "org-b"
		return snapshot, nil
	})
	stripped, err = manager.SnapshotFor(context.Background(), "org-a")
	if err != nil || len(stripped.Services[0].Evidence) != 0 || stripped.Services[0].Assessment != nil {
		t.Fatalf("cross-org projection did not fail closed: %#v err=%v", stripped, err)
	}
}

func TestEvidenceAndBudgetBoundsProduceHonestPartialResults(t *testing.T) {
	now := healthTestNow()
	evidence := make([]core.SignalEvidence, 0, servicehealth.MaxEvidencePerService+1)
	for index := 0; index <= servicehealth.MaxEvidencePerService; index++ {
		evidence = append(evidence, healthEvidence(now, "traces", "request_context", "traces-a", fmt.Sprintf("operation-%03d", index)))
	}
	provider := providerWithEvidence(evidence...)
	snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, nil)
	if len(snapshot.Services[0].Evidence) != servicehealth.MaxEvidencePerService || !snapshot.Coverage.Partial {
		t.Fatalf("evidence=%d coverage=%#v", len(snapshot.Services[0].Evidence), snapshot.Coverage)
	}
	if provider.request.Budget.Requests != servicehealth.MaxExternalRequests || provider.request.Budget.Rows != servicehealth.MaxRowsPerSource*servicehealth.MaxPremiumSources || provider.request.Deadline.IsZero() {
		t.Fatalf("request budget=%#v deadline=%v", provider.request.Budget, provider.request.Deadline)
	}

	overBudget := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	overBudget.collection.Usage.Requests = servicehealth.MaxExternalRequests + 1
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{overBudget}, nil)
	if len(snapshot.Services[0].Evidence) != 0 || capabilityMeasure(t, snapshot, "metrics", "latency").ReasonCode != "provider_budget_exceeded" || !snapshot.Coverage.Partial {
		t.Fatalf("over-budget snapshot=%#v", snapshot)
	}

	underReported := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	underReported.collection.Usage.Rows = 0
	snapshot = collectHealthSnapshot(t, now, []core.HealthProvider{underReported}, nil)
	if len(snapshot.Services[0].Evidence) != 0 || capabilityMeasure(t, snapshot, "metrics", "latency").ReasonCode != "provider_budget_exceeded" {
		t.Fatalf("under-reported usage accepted: %#v", snapshot)
	}
}

func TestAssessmentInputsAndRegressionFacetsStayTickScoped(t *testing.T) {
	now := healthTestNow()
	for _, test := range []struct {
		name     string
		score    float64
		severity string
	}{
		{name: "pressure", score: 50, severity: "pressure"},
		{name: "critical", score: 80, severity: "critical"},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := healthEvidence(now, "metrics", "latency", "metrics-a", "")
			input := core.HealthAssessmentInput{
				OrgID: "org-a", Service: "api", Family: "metrics", Measure: "latency",
				SourceRef: "metrics-a", SignalRef: "metrics.latency", ExpectedMean: 100, ExpectedStd: 5,
			}
			provider := providerWithEvidence(evidence)
			provider.collection.AssessmentInputs = []core.HealthAssessmentInput{input}
			provider.collection.Partial = true
			assessor := assessorFunc(func(_ context.Context, request servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
				if len(request.Inputs) != 1 || request.Inputs[0] != input {
					t.Fatalf("assessment inputs=%+v", request.Inputs)
				}
				return []core.HealthAssessment{{
					OrgID: "org-a", Service: "api", RegressionScore: &test.score, Regressing: true,
					Confidence: 1, ReasonCode: "baseline_regression", AlgorithmVersion: "test-v1",
					AssessedAt: now, FreshUntil: now.Add(time.Minute), Severity: test.severity, RaiseSeverity: true,
				}}, nil
			})
			snapshot := collectHealthSnapshot(t, now, []core.HealthProvider{provider}, assessor)
			if snapshot.Facets["regressing"] != 1 || snapshot.Facets["silent_regressions"] != 0 || !snapshot.Coverage.Partial {
				t.Fatalf("snapshot facets=%v coverage=%+v", snapshot.Facets, snapshot.Coverage)
			}
		})
	}
}

func TestRuntimeThreadsProviderAssessorAndReadProjection(t *testing.T) {
	now := healthTestNow()
	servicehealth.SetOrganizationLister(func(context.Context) ([]string, error) { return []string{"org-a"}, nil })
	t.Cleanup(func() { servicehealth.SetOrganizationLister(nil) })
	provider := providerWithEvidence(healthEvidence(now, "metrics", "latency", "metrics-a", ""))
	assessorCalls := 0
	assessor := assessorFunc(func(context.Context, servicehealth.AssessmentRequest) ([]core.HealthAssessment, error) {
		assessorCalls++
		return []core.HealthAssessment{{
			OrgID: "org-a", Service: "api", Confidence: 0.5, ReasonCode: "insufficient_baseline",
			AlgorithmVersion: "test-v1", AssessedAt: now,
		}}, nil
	})
	projectionCalls := 0
	runtime := servicehealth.NewRuntime(servicehealth.RuntimeOptions{
		Store: storage.NewMemory(), Services: oneHealthService, Providers: []core.HealthProvider{provider},
		ProviderSourceIDs: []string{"metrics-a"}, Assessor: assessor, Now: func() time.Time { return now },
		Projection: func(_ context.Context, orgID string, snapshot servicehealth.SnapshotEnvelope) (servicehealth.SnapshotEnvelope, error) {
			projectionCalls++
			if orgID != "org-a" {
				t.Fatalf("projection org=%q", orgID)
			}
			for index := range snapshot.Services {
				snapshot.Services[index].Evidence = nil
				snapshot.Services[index].Assessment = nil
			}
			return snapshot, nil
		},
	})
	if err := runtime.Job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := runtime.Manager.LoadSnapshot("org-a")
	if err != nil || !ok || len(stored.Services[0].Evidence) != 1 || stored.Services[0].Assessment == nil {
		t.Fatalf("stored=%#v ok=%v err=%v", stored, ok, err)
	}
	projected, err := runtime.Manager.SnapshotFor(context.Background(), "org-a")
	if err != nil || len(projected.Services[0].Evidence) != 0 || projected.Services[0].Assessment != nil || provider.calls != 1 || assessorCalls != 1 || projectionCalls != 1 || len(provider.request.SourceIDs) != 1 || provider.request.SourceIDs[0] != "metrics-a" {
		t.Fatalf("projected=%#v provider=%#v assessorCalls=%d projectionCalls=%d err=%v", projected, provider, assessorCalls, projectionCalls, err)
	}
}

func healthTestNow() time.Time {
	return time.Date(2026, 9, 15, 12, 5, 0, 0, time.UTC)
}

func oneHealthService() []servicehealth.ServiceMetadata {
	return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}}
}

func healthEvidence(now time.Time, family, measure, source, operation string) core.SignalEvidence {
	value := 1.0
	return core.SignalEvidence{
		OrgID: "org-a", Service: "api", Operation: operation, Family: family, Measure: measure,
		Value: &value, Unit: "count", Availability: core.MeasureAvailability{State: core.HealthReady},
		SourceRef: source, SignalRef: family + "." + measure, ObservedAt: now.Add(-time.Minute), WindowStart: now.Add(-5 * time.Minute),
		WindowEnd: now, FreshUntil: now.Add(time.Minute), Provenance: "authoritative",
	}
}

func mutateEvidence(evidence core.SignalEvidence, mutate func(*core.SignalEvidence)) core.SignalEvidence {
	mutate(&evidence)
	return evidence
}

func providerWithEvidence(evidence ...core.SignalEvidence) *fakeHealthProvider {
	family, measure := "metrics", "latency"
	if len(evidence) > 0 {
		family, measure = evidence[0].Family, evidence[0].Measure
	}
	return &fakeHealthProvider{
		capabilities: []core.HealthCapability{{Family: family, Measures: map[string]core.MeasureAvailability{measure: {State: core.HealthReady}}}},
		collection:   core.HealthCollection{Evidence: evidence, Usage: core.HealthUsage{Sources: 1, Requests: 1, Rows: len(evidence), ResponseBytes: 100, LargestResponseBytes: 100}},
	}
}

func collectHealthSnapshot(t *testing.T, now time.Time, providers []core.HealthProvider, assessor servicehealth.HealthAssessor) servicehealth.SnapshotEnvelope {
	t.Helper()
	store := storage.NewMemory()
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: servicehealth.NewManager(store), Store: store, Services: oneHealthService,
		Providers: providers, Assessor: assessor, Now: func() time.Time { return now },
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func capabilityMeasure(t *testing.T, snapshot servicehealth.SnapshotEnvelope, family, measure string) core.MeasureAvailability {
	t.Helper()
	for _, capability := range snapshot.Capabilities {
		if capability.Family == family {
			return capability.Measures[measure]
		}
	}
	t.Fatalf("capability %s.%s absent", family, measure)
	return core.MeasureAvailability{}
}

func hasCapability(snapshot servicehealth.SnapshotEnvelope, family string) bool {
	for _, capability := range snapshot.Capabilities {
		if capability.Family == family {
			return true
		}
	}
	return false
}
