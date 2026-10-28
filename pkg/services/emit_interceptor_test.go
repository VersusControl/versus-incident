package services

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fakeProvider is a capturing core.AlertProvider: it reports a fixed channel
// name and records every incident handed to it, so a test can assert which
// channels a fan-out actually reached.
type fakeProvider struct {
	name string
	sent []*m.Incident
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) SendAlert(incident *m.Incident) error {
	f.sent = append(f.sent, incident)
	return nil
}

// TestSetEmitInterceptor_NilDefaultInert proves the community default: with no
// interceptor registered the decision is always EmitProceed and no hook is ever
// consulted, so CreateIncident behaves exactly as before.
func TestSetEmitInterceptor_NilDefaultInert(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(nil) // ensure clean slate

	d := resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("nil-default decision = %v, want EmitProceed", d.Action)
	}

	// Registering then clearing must return to the no-op: the spy proves the
	// slot is truly empty (never consulted) after a nil clear.
	var calls int
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		calls++
		return EmitDecision{Action: EmitSuppress}
	})
	SetEmitInterceptor(nil)

	d = resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("after nil clear decision = %v, want EmitProceed", d.Action)
	}
	if calls != 0 {
		t.Fatalf("cleared interceptor was consulted %d times, want 0", calls)
	}
}

// TestSetEmitInterceptor_LastWins proves the atomic slot keeps the most recent
// registration and that nil clears it — mirroring the admin_audit seam.
func TestSetEmitInterceptor_LastWins(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitDivert, Reason: "first"}
	})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitSuppress, Reason: "second"}
	})

	d := resolveEmitDecision(nil, "")
	if d.Action != EmitSuppress || d.Reason != "second" {
		t.Fatalf("last-wins decision = %+v, want EmitSuppress/second", d)
	}

	SetEmitInterceptor(nil)
	if got := resolveEmitDecision(nil, ""); got.Action != EmitProceed {
		t.Fatalf("after nil clear = %v, want EmitProceed", got.Action)
	}
}

// TestResolveEmitDecision_FailOpenOnPanic proves a panicking interceptor never
// drops a page: the panic is recovered and the verdict is EmitProceed.
func TestResolveEmitDecision_FailOpenOnPanic(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		panic("boom")
	})

	d := resolveEmitDecision(map[string]interface{}{"title": "x"}, "team")
	if d.Action != EmitProceed {
		t.Fatalf("panicking interceptor decision = %v, want EmitProceed (fail-open)", d.Action)
	}
}

// TestApplyEmitRouting_Proceed proves EmitProceed fans out to the full default
// channel set — the built providers are returned untouched.
func TestApplyEmitRouting_Proceed(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	got := applyEmitRouting(EmitDecision{Action: EmitProceed}, providers)

	if names := providerChannels(got); !equalSet(names, []string{"slack", "telegram", "email"}) {
		t.Fatalf("EmitProceed channels = %v, want the full default set", names)
	}
}

// TestApplyEmitRouting_Divert proves EmitDivert fans out to ONLY the named
// channels — the primary on-call channels are excluded.
func TestApplyEmitRouting_Divert(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	got := applyEmitRouting(
		EmitDecision{Action: EmitDivert, DivertChannels: []string{"email"}},
		providers,
	)

	if names := providerChannels(got); !equalSet(names, []string{"email"}) {
		t.Fatalf("EmitDivert channels = %v, want only [email]", names)
	}
}

// TestFilterProvidersByChannel covers the divert channel restriction: only
// named channels survive, unknown names are ignored, and an empty set yields no
// providers.
func TestFilterProvidersByChannel(t *testing.T) {
	providers := fakeProviders("slack", "telegram", "email")

	t.Run("subset kept", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{"slack", "email"})
		if names := providerChannels(got); !equalSet(names, []string{"slack", "email"}) {
			t.Fatalf("got %v, want [slack email]", names)
		}
	})

	t.Run("unknown name ignored", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{"slack", "pager"})
		if names := providerChannels(got); !equalSet(names, []string{"slack"}) {
			t.Fatalf("got %v, want [slack]", names)
		}
	})

	t.Run("empty names yields none", func(t *testing.T) {
		if got := filterProvidersByChannel(providers, nil); len(got) != 0 {
			t.Fatalf("got %d providers, want 0", len(got))
		}
	})

	t.Run("case and whitespace tolerant", func(t *testing.T) {
		got := filterProvidersByChannel(providers, []string{" Slack "})
		if names := providerChannels(got); !equalSet(names, []string{"slack"}) {
			t.Fatalf("got %v, want [slack]", names)
		}
	})
}

// TestCreateIncident_SuppressSkipsFanOut proves EmitSuppress short-circuits
// before any config resolution or provider building: CreateIncident returns nil
// without error even though no global config is loaded in this test (a proceed
// path would reach GetConfig and panic). This is the "no primary fan-out, no
// error" guarantee.
func TestCreateIncident_SuppressSkipsFanOut(t *testing.T) {
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitSuppress, Reason: "dedup"}
	})

	content := map[string]interface{}{"title": "disk full", "Status": "firing"}
	if err := CreateIncident("team", &content); err != nil {
		t.Fatalf("EmitSuppress CreateIncident returned error %v, want nil", err)
	}
}

func TestCreateIncident_DeliveryHandledIgnoredForWebhook(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitDivert, DeliveryHandled: true, DeliveryError: errors.New("external detection delivery failed")}
	})

	content := map[string]interface{}{"title": "disk full", "Status": "firing"}
	if err := CreateIncident("team", &content); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("webhook incidents=%+v err=%v", incidents, err)
	}
	if incidents[0].Origin != storage.OriginWebhook || incidents[0].NotifyStatus != "sent" {
		t.Fatalf("webhook lifecycle was silently dropped: %+v", incidents[0])
	}
}

func TestCreateIncident_HandledDetectionDivertSkipsIncidentLifecycle(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	onCall := &countingOnCallProvider{}
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, onCall))
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
		core.SetOnCallWorkflow(nil)
	})

	now := time.Now().UTC()
	detection := storage.DetectionEpisodeDecision{
		Fingerprint: "fingerprint", EpisodeID: "episode", IncidentID: "incident",
		OccurrenceCount: 500, FirstSeen: now, LastSeen: now,
		HighestObservedSeverity: "high",
	}
	content := map[string]interface{}{
		"AlertName": "checkout failures", "Service": "checkout", "Source": "agent:loki",
		"PatternID": "pattern", "Frequency": int64(500), "Severity": "high", "Status": "firing",
	}

	t.Run("success", func(t *testing.T) {
		SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
			return EmitDecision{Action: EmitDivert, DeliveryHandled: true}
		})
		result := createIncident("", &content, incidentCreateOptions{IncidentID: detection.IncidentID, Detection: &detection})
		if result.Err != nil || result.NotifyStatus != "diverted" || onCall.calls.Load() != 0 {
			t.Fatalf("handled divert result=%+v on-call=%d", result, onCall.calls.Load())
		}
		record, err := provider.GetIncident(detection.IncidentID)
		if err != nil || record.NotifyStatus != "diverted" || record.OnCallTriggered || len(record.ChannelsNotified) != 0 {
			t.Fatalf("handled divert record=%+v err=%v", record, err)
		}
		if _, ok := record.Content["AckURL"]; ok {
			t.Fatalf("handled divert injected AckURL: %+v", record.Content)
		}
	})

	t.Run("failure", func(t *testing.T) {
		deliveryErr := errors.New("external delivery failed")
		SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
			return EmitDecision{Action: EmitDivert, DeliveryHandled: true, DeliveryError: deliveryErr}
		})
		result := createIncident("", &content, incidentCreateOptions{IncidentID: detection.IncidentID, Detection: &detection})
		if !errors.Is(result.Err, deliveryErr) || result.NotifyStatus != "failed" || onCall.calls.Load() != 0 {
			t.Fatalf("failed handled divert result=%+v on-call=%d", result, onCall.calls.Load())
		}
		record, err := provider.GetIncident(detection.IncidentID)
		if err != nil || record.NotifyStatus != "failed" || record.OnCallTriggered {
			t.Fatalf("failed handled divert record=%+v err=%v", record, err)
		}
	})
}

func TestCreateIncident_HandledEscalationPreservesInitialDeliveryAndOperatorState(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	now := time.Now().UTC()
	ackedAt := now.Add(time.Minute)
	initial := &storage.IncidentRecord{
		ID: "incident", CreatedAt: now, AckedAt: &ackedAt,
		AssignedTeamID: "sre", AssignedMemberIDs: []string{"operator"},
		ChannelsEnabled: []string{"slack", "email"}, ChannelsNotified: []string{"slack"},
		OnCallTriggered: true, OnCallError: "secondary provider unavailable",
		NotifyStatus: "partial", NotifyError: "email failed",
		DetectionFingerprint: "fingerprint", DetectionEpisodeID: "episode",
		OccurrenceCount: 1, Content: map[string]interface{}{"Frequency": int64(1)},
	}
	if err := provider.(storage.DetectionIncidentStore).SaveDetectionIncident(initial); err != nil {
		t.Fatalf("SaveDetectionIncident(initial): %v", err)
	}
	detection := storage.DetectionEpisodeDecision{
		Fingerprint: "fingerprint", EpisodeID: "episode", IncidentID: "incident",
		OccurrenceCount: 2, FirstSeen: now, LastSeen: now.Add(time.Minute),
		HighestObservedSeverity: "high", NotificationKind: storage.DetectionNotificationEscalation,
	}
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitDivert, DeliveryHandled: true}
	})
	content := map[string]interface{}{
		"AlertName": "checkout failures", "Service": "checkout", "Source": "agent:loki",
		"PatternID": "pattern", "Frequency": int64(2), "Severity": "high", "Status": "firing",
	}
	result := createIncident("", &content, incidentCreateOptions{IncidentID: detection.IncidentID, Detection: &detection})
	if result.Err != nil || result.NotifyStatus != "diverted" {
		t.Fatalf("handled escalation result=%+v", result)
	}
	record, err := provider.GetIncident("incident")
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if record.NotifyStatus != "diverted" || record.NotifyError != "" ||
		!equalSet(record.ChannelsEnabled, initial.ChannelsEnabled) || !equalSet(record.ChannelsNotified, initial.ChannelsNotified) ||
		!record.OnCallTriggered || record.OnCallError != initial.OnCallError || record.AckedAt == nil ||
		record.AssignedTeamID != "sre" || !equalSet(record.AssignedMemberIDs, []string{"operator"}) {
		t.Fatalf("handled escalation lost prior state: %+v", record)
	}
}

func TestCreateIncidentFromFinding_SuppressedInitialVisibleOnPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	previousStore := Storage()
	SetStorage(provider)
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitSuppress}
	})
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	emission := &core.DetectionEmissionResult{}
	result := core.AgentResult{
		PatternID: "postgres-suppressed-" + time.Now().UTC().Format("150405.000000000"),
		Frequency: 4, AgentKind: "detect", SignalKind: "logs", DetectionEmission: emission,
	}
	finding := &core.AIFinding{Title: "Suppressed Postgres condition", Severity: "medium"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("CreateIncidentFromFinding: %v", err)
	}
	record, err := provider.GetIncident(emission.IncidentID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if record.NotifyStatus != "suppressed" || record.OccurrenceCount != 4 ||
		record.DetectionEpisodeID == "" || record.Content["Frequency"] != float64(4) ||
		len(record.ChannelsNotified) != 0 || record.OnCallTriggered {
		t.Fatalf("suppressed Postgres incident=%+v", record)
	}
}

func TestCreateIncident_HandledEscalationPreservesInitialStateOnPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	now := time.Now().UTC()
	incidentID := "handled-escalation-" + now.Format("150405.000000000")
	ackedAt := now.Add(time.Minute)
	initial := &storage.IncidentRecord{
		ID: incidentID, CreatedAt: now, AckedAt: &ackedAt,
		AssignedTeamID: "sre", AssignedMemberIDs: []string{"operator"},
		ChannelsEnabled: []string{"slack", "email"}, ChannelsNotified: []string{"slack"},
		OnCallTriggered: true, OnCallError: "secondary provider unavailable",
		NotifyStatus: "partial", NotifyError: "email failed",
		DetectionFingerprint: "fingerprint", DetectionEpisodeID: "episode-" + incidentID,
		OccurrenceCount: 1, Content: map[string]interface{}{"Frequency": int64(1)},
	}
	if err := provider.(storage.DetectionIncidentStore).SaveDetectionIncident(initial); err != nil {
		t.Fatalf("SaveDetectionIncident(initial): %v", err)
	}
	detection := storage.DetectionEpisodeDecision{
		Fingerprint: initial.DetectionFingerprint, EpisodeID: initial.DetectionEpisodeID, IncidentID: incidentID,
		OccurrenceCount: 2, FirstSeen: now, LastSeen: now.Add(time.Minute),
		HighestObservedSeverity: "high", NotificationKind: storage.DetectionNotificationEscalation,
	}
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitDivert, DeliveryHandled: true}
	})
	content := map[string]interface{}{
		"AlertName": "checkout failures", "Service": "checkout", "Source": "agent:loki",
		"PatternID": "pattern", "Frequency": int64(2), "Severity": "high", "Status": "firing",
	}
	result := createIncident("", &content, incidentCreateOptions{IncidentID: incidentID, Detection: &detection})
	if result.Err != nil || result.NotifyStatus != "diverted" {
		t.Fatalf("handled escalation result=%+v", result)
	}
	record, err := provider.GetIncident(incidentID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if record.NotifyStatus != "diverted" || record.NotifyError != "" ||
		!equalSet(record.ChannelsEnabled, initial.ChannelsEnabled) || !equalSet(record.ChannelsNotified, initial.ChannelsNotified) ||
		!record.OnCallTriggered || record.OnCallError != initial.OnCallError || record.AckedAt == nil ||
		record.AssignedTeamID != "sre" || !equalSet(record.AssignedMemberIDs, []string{"operator"}) {
		t.Fatalf("handled Postgres escalation lost prior state: %+v", record)
	}
}

// TestEmitFingerprint covers the composite key: stable across calls, distinct
// for distinct (source, service, pattern, severity), a sane webhook fallback
// when PatternID is absent, and no dependence on any raw payload field.
func TestEmitFingerprint(t *testing.T) {
	agent := map[string]interface{}{
		"Source":      "agent:elasticsearch:prod-app",
		"ServiceName": "checkout",
		"PatternID":   "p-123",
		"Severity":    "high",
	}

	t.Run("stable across calls", func(t *testing.T) {
		fp1 := EmitFingerprint(agent)
		fp2 := EmitFingerprint(agent)
		if fp1 != fp2 {
			t.Fatal("fingerprint not stable across calls")
		}
	})

	t.Run("distinct on severity", func(t *testing.T) {
		other := cloneContent(agent)
		other["Severity"] = "low"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("severity change did not change fingerprint")
		}
	})

	t.Run("distinct on service", func(t *testing.T) {
		other := cloneContent(agent)
		other["ServiceName"] = "billing"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("service change did not change fingerprint")
		}
	})

	t.Run("distinct on pattern", func(t *testing.T) {
		other := cloneContent(agent)
		other["PatternID"] = "p-999"
		if EmitFingerprint(agent) == EmitFingerprint(other) {
			t.Fatal("pattern change did not change fingerprint")
		}
	})

	t.Run("webhook title fallback distinguishes distinct titles", func(t *testing.T) {
		a := map[string]interface{}{"Source": "webhook", "service": "api", "title": "CPU high", "Severity": "warning"}
		b := map[string]interface{}{"Source": "webhook", "service": "api", "title": "Disk full", "Severity": "warning"}
		if EmitFingerprint(a) == EmitFingerprint(b) {
			t.Fatal("distinct webhook titles produced the same fingerprint")
		}
		// Cosmetic-only differences (case, spacing) must fold to one key.
		c := map[string]interface{}{"Source": "webhook", "service": "api", "title": "  cpu   HIGH  ", "Severity": "warning"}
		if EmitFingerprint(a) != EmitFingerprint(c) {
			t.Fatal("cosmetically identical titles produced different fingerprints")
		}
	})

	t.Run("distinct cloudwatch alarms on one service do not collide", func(t *testing.T) {
		// Both alarms share source, service and severity; only the alarm name
		// differs. Before AlarmName joined the title key set both hashed to the
		// same degenerate key and deduped each other.
		alarm := func(name string) map[string]interface{} {
			return map[string]interface{}{
				"Source":        "sns",
				"AlarmName":     name,
				"NewStateValue": "ALARM",
				"severity":      "warning",
				"Trigger": map[string]interface{}{"Dimensions": []interface{}{
					map[string]interface{}{"name": "ServiceName", "value": "checkout"},
				}},
			}
		}
		five := alarm("checkout-5xx")
		latency := alarm("checkout-latency")
		if EmitFingerprint(five) == EmitFingerprint(latency) {
			t.Fatalf("distinct CloudWatch alarms shared fingerprint %q", EmitFingerprint(five))
		}
		// The same alarm firing again must still fold onto one key.
		if EmitFingerprint(five) != EmitFingerprint(alarm("checkout-5xx")) {
			t.Fatal("the same CloudWatch alarm produced different fingerprints")
		}
	})

	t.Run("duplicate spellings of a title key keep one dedup bucket", func(t *testing.T) {
		// A payload carrying two spellings of the same title key used to be
		// titled by whichever spelling map iteration yielded, so the same alert
		// landed in two dedup buckets and lost its suppression state.
		seen := map[string]int{}
		for i := 0; i < 2000; i++ {
			seen[EmitFingerprint(map[string]interface{}{
				"Source":    "sns",
				"service":   "checkout",
				"severity":  "warning",
				"AlarmName": "alpha",
				"alarmname": "beta",
			})]++
		}
		if len(seen) != 1 {
			t.Fatalf("duplicate title spellings produced %d dedup buckets: %v", len(seen), seen)
		}
	})

	t.Run("ignores raw payload fields", func(t *testing.T) {
		withRaw := cloneContent(agent)
		withRaw["Raw"] = "SECRET-TOKEN-abc123"
		withRaw["raw"] = "another secret"
		if EmitFingerprint(agent) != EmitFingerprint(withRaw) {
			t.Fatal("fingerprint changed with an unrelated raw field; it must read only the composite keys")
		}
	})

	t.Run("a pattern verdict does not move the severity slot", func(t *testing.T) {
		// An agent finding carries no real severity; the pattern later acquires
		// an operator-set verdict. That must not re-key the finding into a new
		// dedup bucket, and it must not put operator free text in the slot the
		// priority floor reads.
		finding := map[string]interface{}{
			"Source":      "agent:elasticsearch:prod-app",
			"ServiceName": "checkout",
			"PatternID":   "p-123",
		}
		labelled := cloneContent(finding)
		labelled["Verdict"] = "known"
		if EmitFingerprint(finding) != EmitFingerprint(labelled) {
			t.Fatalf("verdict changed the fingerprint: %q vs %q", EmitFingerprint(finding), EmitFingerprint(labelled))
		}
		if got := ExtractSeverity(labelled); got != "" {
			t.Fatalf("ExtractSeverity of a verdict-only finding = %q, want empty", got)
		}
		critical := cloneContent(finding)
		critical["verdict"] = "critical"
		if got := ExtractSeverity(critical); got != "" {
			t.Fatalf("operator-typed verdict surfaced as severity %q; the priority floor must not be steerable this way", got)
		}
	})
}

func fakeProviders(names ...string) []core.AlertProvider {
	out := make([]core.AlertProvider, 0, len(names))
	for _, n := range names {
		out = append(out, &fakeProvider{name: n})
	}
	return out
}

func providerChannels(providers []core.AlertProvider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func cloneContent(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
