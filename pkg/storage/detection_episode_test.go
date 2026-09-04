package storage_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func runDetectionEpisodeContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "elasticsearch", Service: "checkout",
		SignalKind: "logs", ConditionKey: "pattern-42",
	}
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	occurrence := storage.DetectionOccurrence{
		Identity: identity, Frequency: 500, Severity: "low", ReceivedAt: base,
		ClaimOwner: "worker-a", ClaimLease: time.Minute,
	}

	opened, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(open): %v", err)
	}
	if opened.Action != storage.DetectionEpisodeOpened || !opened.NotificationClaimed || opened.NotificationKind != storage.DetectionNotificationInitial {
		t.Fatalf("opened decision = %+v", opened)
	}
	if opened.OccurrenceCount != 500 || opened.OccurrenceDelta != 500 || opened.EpisodeID == "" || opened.IncidentID == "" {
		t.Fatalf("opened identity/count = %+v", opened)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: base,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(initial): %v", err)
	}
	if err := provider.SaveIncident(&storage.IncidentRecord{ID: opened.IncidentID, CreatedAt: opened.FirstSeen}); err != nil {
		t.Fatalf("SaveIncident(initial): %v", err)
	}

	occurrence.Frequency = 1
	occurrence.ReceivedAt = base.Add(time.Second)
	coalesced, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(coalesce): %v", err)
	}
	if coalesced.Action != storage.DetectionEpisodeCoalesced || coalesced.NotificationClaimed || coalesced.OccurrenceCount != 501 {
		t.Fatalf("coalesced decision = %+v", coalesced)
	}
	if coalesced.EpisodeID != opened.EpisodeID || coalesced.IncidentID != opened.IncidentID {
		t.Fatal("coalescing changed stable episode or incident ID")
	}

	occurrence.Severity = "high"
	occurrence.ReceivedAt = base.Add(2 * time.Second)
	escalated, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(escalate): %v", err)
	}
	if escalated.Action != storage.DetectionEpisodeEscalated || !escalated.NotificationClaimed || escalated.NotificationKind != storage.DetectionNotificationEscalation {
		t.Fatalf("escalated decision = %+v", escalated)
	}

	occurrence.ReceivedAt = base.Add(3 * time.Second)
	concurrent, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(concurrent escalation): %v", err)
	}
	if concurrent.NotificationClaimed || concurrent.OccurrenceCount != 503 {
		t.Fatalf("concurrent escalation = %+v", concurrent)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: escalated.EpisodeID, ClaimToken: escalated.ClaimToken, Outcome: "failed", CompletedAt: base.Add(3 * time.Second),
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(failed): %v", err)
	}

	occurrence.ReceivedAt = base.Add(4 * time.Second)
	retry, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(retry): %v", err)
	}
	if !retry.NotificationClaimed || retry.NotificationKind != storage.DetectionNotificationEscalation {
		t.Fatalf("retry decision = %+v", retry)
	}
	renewedAt := retry.ClaimExpiresAt.Add(-time.Second)
	if err := store.RenewDetectionNotificationClaim(storage.DetectionNotificationRenewal{
		EpisodeID: retry.EpisodeID, ClaimToken: retry.ClaimToken,
		ClaimLease: time.Minute, RenewedAt: renewedAt,
	}); err != nil {
		t.Fatalf("RenewDetectionNotificationClaim: %v", err)
	}
	occurrence.ReceivedAt = retry.ClaimExpiresAt
	occupied, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(renewed claim): %v", err)
	}
	if occupied.NotificationClaimed {
		t.Fatalf("renewed claim was taken over: %+v", occupied)
	}
	occurrence.ReceivedAt = renewedAt.Add(time.Minute)
	takeover, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(takeover): %v", err)
	}
	if !takeover.NotificationClaimed || takeover.ClaimToken == retry.ClaimToken {
		t.Fatalf("takeover decision = %+v", takeover)
	}
	if takeover.Action != storage.DetectionEpisodeEscalated || takeover.NotificationKind != storage.DetectionNotificationEscalation {
		t.Fatalf("escalation takeover lost action or kind: %+v", takeover)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: takeover.EpisodeID, ClaimToken: takeover.ClaimToken, Outcome: "sent", CompletedAt: takeover.LastSeen,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(escalation): %v", err)
	}

	occurrence.ReceivedAt = takeover.LastSeen.Add(storage.DefaultDetectionEpisodeQuietPeriod)
	quiet, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(quiet reopen): %v", err)
	}
	if quiet.Action != storage.DetectionEpisodeReopened || quiet.EpisodeID == opened.EpisodeID || quiet.IncidentID == opened.IncidentID {
		t.Fatalf("quiet decision = %+v", quiet)
	}
	if err := store.CloseDetectionEpisodeByIncident(quiet.IncidentID, quiet.LastSeen); err != nil {
		t.Fatalf("CloseDetectionEpisodeByIncident: %v", err)
	}
	occurrence.ReceivedAt = quiet.LastSeen.Add(time.Second)
	resolved, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(after resolve): %v", err)
	}
	if resolved.Action != storage.DetectionEpisodeReopened || resolved.EpisodeID == quiet.EpisodeID {
		t.Fatalf("resolved recurrence = %+v", resolved)
	}
}

func runDetectionRenewalSentinelContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC)
	occurrence := storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "renewal-source", Service: "checkout",
			SignalKind: "logs", ConditionKey: "renewal-sentinels",
		},
		Frequency: 1, Severity: "low", ReceivedAt: base, ClaimLease: time.Minute,
	}
	opened, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(open): %v", err)
	}
	if err := store.RenewDetectionNotificationClaim(storage.DetectionNotificationRenewal{
		EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken,
		ClaimLease: time.Hour, RenewedAt: opened.ClaimExpiresAt,
	}); !errors.Is(err, storage.ErrDetectionClaimLost) {
		t.Fatalf("expired renewal error = %v, want ErrDetectionClaimLost", err)
	}

	occurrence.ReceivedAt = opened.ClaimExpiresAt
	replaced, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(replace expired claim): %v", err)
	}
	if !replaced.NotificationClaimed || replaced.ClaimToken == opened.ClaimToken {
		t.Fatalf("expired renewal mutated lease: %+v", replaced)
	}
	if err := store.RenewDetectionNotificationClaim(storage.DetectionNotificationRenewal{
		EpisodeID: replaced.EpisodeID, ClaimToken: opened.ClaimToken,
		ClaimLease: time.Hour, RenewedAt: replaced.ClaimExpiresAt.Add(-time.Second),
	}); !errors.Is(err, storage.ErrDetectionClaimLost) {
		t.Fatalf("replaced-token renewal error = %v, want ErrDetectionClaimLost", err)
	}

	occurrence.ReceivedAt = replaced.ClaimExpiresAt
	takeover, err := store.RecordDetectionOccurrence(occurrence)
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(replace current claim): %v", err)
	}
	if !takeover.NotificationClaimed || takeover.ClaimToken == replaced.ClaimToken {
		t.Fatalf("replaced-token renewal mutated lease: %+v", takeover)
	}
}

func runDetectionEpisodeExistingOnlyContract(t *testing.T, provider storage.Provider) storage.DetectionEpisodeDecision {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	identity := func(condition string) storage.DetectionIdentity {
		return storage.DetectionIdentity{
			AgentKind: "detect", Source: "loki", Service: "checkout",
			SignalKind: "logs", ConditionKey: condition,
		}
	}
	continueOnly := func(condition string, frequency int64, receivedAt time.Time) (storage.DetectionEpisodeDecision, error) {
		return store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity(condition), Frequency: frequency, Severity: "medium",
			ReceivedAt: receivedAt, ExistingOnly: true,
		})
	}
	openWithIncident := func(condition string, receivedAt time.Time) storage.DetectionEpisodeDecision {
		t.Helper()
		opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity(condition), Frequency: 2, Severity: "medium", ReceivedAt: receivedAt,
		})
		if err != nil {
			t.Fatalf("RecordDetectionOccurrence(%s): %v", condition, err)
		}
		if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
			EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: receivedAt,
		}); err != nil {
			t.Fatalf("CompleteDetectionNotification(%s): %v", condition, err)
		}
		if err := provider.SaveIncident(&storage.IncidentRecord{
			ID: opened.IncidentID, CreatedAt: opened.FirstSeen, Content: map[string]interface{}{"Frequency": int64(2)},
		}); err != nil {
			t.Fatalf("SaveIncident(%s): %v", condition, err)
		}
		return opened
	}

	incidentsBefore, err := provider.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents(before existing-only): %v", err)
	}
	if _, err := continueOnly("historically-known", 4, base); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing existing-only episode error = %v, want ErrNotFound", err)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil || len(incidents) != len(incidentsBefore) {
		t.Fatalf("missing existing-only incidents = %d, want unchanged %d, %v", len(incidents), len(incidentsBefore), err)
	}

	opened := openWithIncident("active", base)
	continued, err := continueOnly("active", 4, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(existing-only): %v", err)
	}
	if continued.Action != storage.DetectionEpisodeCoalesced || continued.NotificationClaimed ||
		continued.EpisodeID != opened.EpisodeID || continued.IncidentID != opened.IncidentID ||
		continued.OccurrenceDelta != 4 || continued.OccurrenceCount != 6 || !continued.LastSeen.Equal(base.Add(time.Minute)) {
		t.Fatalf("existing-only decision = %+v", continued)
	}
	incident, err := provider.GetIncident(opened.IncidentID)
	if err != nil {
		t.Fatalf("GetIncident(existing-only): %v", err)
	}
	if incident.OccurrenceCount != 6 || incident.DetectionLastSeen == nil || !incident.DetectionLastSeen.Equal(base.Add(time.Minute)) ||
		fmt.Sprint(incident.Content["Frequency"]) != "6" {
		t.Fatalf("existing-only incident = %+v", incident)
	}
	if err := store.CloseDetectionEpisodeByIncident(opened.IncidentID, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("CloseDetectionEpisodeByIncident(existing-only): %v", err)
	}
	if _, err := continueOnly("active", 1, base.Add(3*time.Minute)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("resolved existing-only error = %v, want ErrNotFound", err)
	}

	quiet := openWithIncident("quiet", base)
	if _, err := continueOnly("quiet", 1, base.Add(storage.DefaultDetectionEpisodeQuietPeriod)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("quiet existing-only error = %v, want ErrNotFound", err)
	}
	recurred, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity("quiet"), Frequency: 1, Severity: "medium",
		ReceivedAt: base.Add(storage.DefaultDetectionEpisodeQuietPeriod + time.Second),
	})
	if err != nil || recurred.Action != storage.DetectionEpisodeReopened || recurred.EpisodeID == quiet.EpisodeID {
		t.Fatalf("quiet recurrence = %+v, %v", recurred, err)
	}

	orphan, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity("orphan"), Frequency: 1, Severity: "medium", ReceivedAt: base,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(orphan): %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: orphan.EpisodeID, ClaimToken: orphan.ClaimToken, Outcome: "sent", CompletedAt: base,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(orphan): %v", err)
	}
	if _, err := continueOnly("orphan", 1, base.Add(time.Minute)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("orphan existing-only error = %v, want ErrNotFound", err)
	}
	return continued
}

func runDetectionEpisodeFoldedSeverityClaimContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	identity := func(condition string) storage.DetectionIdentity {
		return storage.DetectionIdentity{
			AgentKind: "detect", Source: "loki", Service: "checkout",
			SignalKind: "logs", ConditionKey: condition,
		}
	}
	openAndComplete := func(condition string) storage.DetectionEpisodeDecision {
		t.Helper()
		opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity(condition), Frequency: 1, Severity: "low", ReceivedAt: base,
		})
		if err != nil {
			t.Fatalf("RecordDetectionOccurrence(%s open): %v", condition, err)
		}
		if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
			EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: base,
		}); err != nil {
			t.Fatalf("CompleteDetectionNotification(%s open): %v", condition, err)
		}
		if err := provider.SaveIncident(&storage.IncidentRecord{ID: opened.IncidentID, CreatedAt: base}); err != nil {
			t.Fatalf("SaveIncident(%s): %v", condition, err)
		}
		return opened
	}

	for _, testCase := range []struct {
		name        string
		activeLease bool
	}{
		{name: "existing_only"},
		{name: "active_lease", activeLease: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			opened := openAndComplete(testCase.name)
			var occupied storage.DetectionEpisodeDecision
			if testCase.activeLease {
				var err error
				occupied, err = store.RecordDetectionOccurrence(storage.DetectionOccurrence{
					Identity: identity(testCase.name), Frequency: 1, Severity: "medium",
					ReceivedAt: base.Add(time.Second), ClaimLease: time.Minute,
				})
				if err != nil || !occupied.NotificationClaimed {
					t.Fatalf("active lease decision = %+v, %v", occupied, err)
				}
			}
			folded, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity(testCase.name), Frequency: 1, Severity: "critical",
				ReceivedAt: base.Add(2 * time.Second), ExistingOnly: true,
			})
			if err != nil || folded.NotificationClaimed || folded.HighestObservedSeverity != "critical" {
				t.Fatalf("folded decision = %+v, %v", folded, err)
			}
			if testCase.activeLease {
				if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
					EpisodeID: opened.EpisodeID, ClaimToken: occupied.ClaimToken,
					Outcome: "sent", CompletedAt: base.Add(3 * time.Second),
				}); err != nil {
					t.Fatalf("CompleteDetectionNotification(active lease): %v", err)
				}
			}
			claimed, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity(testCase.name), Frequency: 1, Severity: "low",
				ReceivedAt: base.Add(4 * time.Second),
			})
			if err != nil || !claimed.NotificationClaimed ||
				claimed.NotificationKind != storage.DetectionNotificationEscalation ||
				claimed.HighestObservedSeverity != "critical" {
				t.Fatalf("folded severity claim = %+v, %v", claimed, err)
			}
		})
	}
}

func runDetectionEpisodeResolvedIncidentRaceContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "loki", Service: "checkout", SignalKind: "logs", ConditionKey: "resolved-race",
	}
	base := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "medium", ReceivedAt: base,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(open): %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: base,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification: %v", err)
	}
	resolvedAt := base.Add(time.Second)
	if err := provider.SaveIncident(&storage.IncidentRecord{
		ID: opened.IncidentID, CreatedAt: base, Resolved: true, ResolvedAt: &resolvedAt,
		DetectionEpisodeID: opened.EpisodeID,
	}); err != nil {
		t.Fatalf("SaveIncident(resolved): %v", err)
	}
	if _, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "critical",
		ReceivedAt: base.Add(2 * time.Second), ExistingOnly: true,
	}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("resolved existing-only error=%v, want ErrNotFound", err)
	}
	recurred, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "high", ReceivedAt: base.Add(3 * time.Second),
	})
	if err != nil || recurred.EpisodeID == opened.EpisodeID || recurred.IncidentID == opened.IncidentID {
		t.Fatalf("resolved recurrence = %+v, %v", recurred, err)
	}
}

func runDetectionEpisodeSeverityPersistenceContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 4, 19, 0, 0, 0, time.UTC)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "severity-source", Service: "checkout",
		SignalKind: "logs", ConditionKey: "monotonic-severity",
	}
	opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "low", ReceivedAt: base,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(low): %v", err)
	}
	if err := provider.SaveIncident(&storage.IncidentRecord{
		ID: opened.IncidentID, CreatedAt: base, Content: map[string]interface{}{"Severity": "low"},
	}); err != nil {
		t.Fatalf("SaveIncident(low): %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: base,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(low): %v", err)
	}
	escalated, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "critical", ReceivedAt: base.Add(time.Second),
	})
	if err != nil || !escalated.NotificationClaimed {
		t.Fatalf("RecordDetectionOccurrence(critical)=%+v err=%v", escalated, err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: escalated.EpisodeID, ClaimToken: escalated.ClaimToken, Outcome: "sent", CompletedAt: base.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(critical): %v", err)
	}
	continued, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "low", ReceivedAt: base.Add(2 * time.Second), ExistingOnly: true,
	})
	if err != nil || continued.NotificationClaimed {
		t.Fatalf("RecordDetectionOccurrence(later low)=%+v err=%v", continued, err)
	}
	incident, err := provider.GetIncident(opened.IncidentID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if incident.HighestObservedSeverity != "critical" || incident.HighestNotifiedSeverity != "critical" ||
		incident.Content["Severity"] != "critical" {
		t.Fatalf("detail severity downgraded: %+v", incident)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	for _, listed := range incidents {
		if listed.ID == opened.IncidentID {
			if listed.HighestObservedSeverity != "critical" || listed.HighestNotifiedSeverity != "critical" {
				t.Fatalf("list severity downgraded: %+v", listed)
			}
			return
		}
	}
	t.Fatalf("incident %s missing from list", opened.IncidentID)
}

func TestMemoryDetectionEpisodeContract(t *testing.T) {
	provider := storage.NewMemory()
	runDetectionEpisodeContract(t, provider)
	runDetectionRenewalSentinelContract(t, provider)
	runDetectionEpisodeExistingOnlyContract(t, provider)
	runDetectionEpisodeFoldedSeverityClaimContract(t, provider)
	runDetectionEpisodeResolvedIncidentRaceContract(t, provider)
	runDetectionEpisodeSeverityPersistenceContract(t, provider)
}

func TestFileDetectionEpisodeContractAndRestart(t *testing.T) {
	dir := t.TempDir()
	provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	runDetectionEpisodeContract(t, provider)
	runDetectionRenewalSentinelContract(t, provider)
	continued := runDetectionEpisodeExistingOnlyContract(t, provider)
	runDetectionEpisodeFoldedSeverityClaimContract(t, provider)
	runDetectionEpisodeResolvedIncidentRaceContract(t, provider)
	runDetectionEpisodeSeverityPersistenceContract(t, provider)
	if err := provider.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile(restart): %v", err)
	}
	defer restarted.Close()
	incident, err := restarted.GetIncident(continued.IncidentID)
	if err != nil || incident.OccurrenceCount != 6 || incident.DetectionLastSeen == nil || !incident.DetectionLastSeen.Equal(continued.LastSeen) {
		t.Fatalf("existing-only incident after restart = %+v, %v", incident, err)
	}
	decision, err := restarted.(storage.DetectionEpisodeStore).RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "elasticsearch", Service: "checkout",
			SignalKind: "logs", ConditionKey: "pattern-42",
		},
		Frequency: 1, Severity: "high", ReceivedAt: time.Date(2026, 9, 3, 12, 10, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(after restart): %v", err)
	}
	if decision.Action != storage.DetectionEpisodeCoalesced || decision.OccurrenceCount != 2 {
		t.Fatalf("restart decision = %+v", decision)
	}
}

func TestPostgresDetectionEpisodeContract(t *testing.T) {
	provider := newTestPostgres(t)
	runDetectionEpisodeContract(t, provider)
	runDetectionRenewalSentinelContract(t, provider)
	runDetectionEpisodeExistingOnlyContract(t, provider)
	runDetectionEpisodeFoldedSeverityClaimContract(t, provider)
	runDetectionEpisodeResolvedIncidentRaceContract(t, provider)
	runDetectionEpisodeSeverityPersistenceContract(t, provider)
}

func TestPostgresDetectionEpisodeConcurrentOwnerAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres episode concurrency test")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	truncateAllTables(t, dsn)
	store := provider.(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "loki", Service: "checkout", SignalKind: "logs", ConditionKey: "stable-condition",
	}
	const occurrences = 500
	decisions := make(chan storage.DetectionEpisodeDecision, occurrences)
	errorsCh := make(chan error, occurrences)
	var wait sync.WaitGroup
	for index := 0; index < occurrences; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity, Frequency: 1, Severity: "medium",
				ReceivedAt: time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC), ClaimOwner: "replica",
			})
			if err != nil {
				errorsCh <- err
				return
			}
			decisions <- decision
		}()
	}
	wait.Wait()
	close(decisions)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("RecordDetectionOccurrence: %v", err)
	}
	var episodeID, incidentID string
	claimed := 0
	maxCount := int64(0)
	var initialClaim storage.DetectionEpisodeDecision
	for decision := range decisions {
		if episodeID == "" {
			episodeID, incidentID = decision.EpisodeID, decision.IncidentID
		}
		if decision.EpisodeID != episodeID || decision.IncidentID != incidentID {
			t.Fatalf("concurrent occurrence changed stable IDs: %+v", decision)
		}
		if decision.NotificationClaimed {
			claimed++
			initialClaim = decision
		}
		if decision.OccurrenceCount > maxCount {
			maxCount = decision.OccurrenceCount
		}
	}
	if claimed != 1 || maxCount != occurrences {
		t.Fatalf("claimed=%d max_count=%d, want 1/%d", claimed, maxCount, occurrences)
	}
	if err := provider.SaveIncident(&storage.IncidentRecord{ID: incidentID, CreatedAt: initialClaim.FirstSeen}); err != nil {
		t.Fatalf("SaveIncident(initial): %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: episodeID, ClaimToken: initialClaim.ClaimToken, Outcome: "sent", CompletedAt: initialClaim.LastSeen,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(initial): %v", err)
	}

	const escalations = 100
	escalatedAt := time.Date(2026, 9, 3, 15, 0, 1, 0, time.UTC)
	escalationDecisions := make(chan storage.DetectionEpisodeDecision, escalations)
	escalationErrors := make(chan error, escalations)
	for index := 0; index < escalations; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity, Frequency: 1, Severity: "high", ReceivedAt: escalatedAt, ClaimOwner: "replica",
			})
			if err != nil {
				escalationErrors <- err
				return
			}
			escalationDecisions <- decision
		}()
	}
	wait.Wait()
	close(escalationDecisions)
	close(escalationErrors)
	for err := range escalationErrors {
		t.Errorf("RecordDetectionOccurrence(escalation): %v", err)
	}
	escalationClaims := 0
	var escalationClaim storage.DetectionEpisodeDecision
	for decision := range escalationDecisions {
		if decision.EpisodeID != episodeID || decision.IncidentID != incidentID {
			t.Fatalf("concurrent escalation changed stable IDs: %+v", decision)
		}
		if decision.NotificationClaimed {
			escalationClaims++
			escalationClaim = decision
		}
	}
	if escalationClaims != 1 || escalationClaim.NotificationKind != storage.DetectionNotificationEscalation {
		t.Fatalf("escalation claims=%d decision=%+v, want one escalation owner", escalationClaims, escalationClaim)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: episodeID, ClaimToken: escalationClaim.ClaimToken, Outcome: "sent", CompletedAt: escalatedAt,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification(escalation): %v", err)
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres(restart): %v", err)
	}
	defer restarted.Close()
	decision, err := restarted.(storage.DetectionEpisodeStore).RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "medium",
		ReceivedAt: escalatedAt.Add(time.Second), ClaimOwner: "restarted",
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(restart): %v", err)
	}
	if decision.EpisodeID != episodeID || decision.IncidentID != incidentID ||
		decision.OccurrenceCount != occurrences+escalations+1 || !decision.LastSeen.Equal(escalatedAt.Add(time.Second)) ||
		decision.HighestNotifiedSeverity != "high" {
		t.Fatalf("restart decision = %+v", decision)
	}
}

func TestPostgresDetectionEpisodeConcurrentExistingOnly(t *testing.T) {
	provider := newTestPostgres(t)
	store := provider.(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "loki", Service: "checkout",
		SignalKind: "logs", ConditionKey: "known-continuation",
	}
	base := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity, Frequency: 1, Severity: "medium", ReceivedAt: base,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(open): %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent", CompletedAt: base,
	}); err != nil {
		t.Fatalf("CompleteDetectionNotification: %v", err)
	}
	if err := provider.SaveIncident(&storage.IncidentRecord{
		ID: opened.IncidentID, CreatedAt: opened.FirstSeen, Content: map[string]interface{}{"Frequency": int64(1)},
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	const continuations = 400
	decisions := make(chan storage.DetectionEpisodeDecision, continuations)
	errorsCh := make(chan error, continuations)
	var wait sync.WaitGroup
	for index := 0; index < continuations; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
				Identity: identity, Frequency: 1, Severity: "critical",
				ReceivedAt: base.Add(time.Minute), ExistingOnly: true,
			})
			if err != nil {
				errorsCh <- err
				return
			}
			decisions <- decision
		}()
	}
	wait.Wait()
	close(decisions)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("RecordDetectionOccurrence(existing-only): %v", err)
	}
	maxCount := int64(0)
	for decision := range decisions {
		if decision.EpisodeID != opened.EpisodeID || decision.IncidentID != opened.IncidentID || decision.NotificationClaimed {
			t.Fatalf("concurrent existing-only decision = %+v", decision)
		}
		if decision.OccurrenceCount > maxCount {
			maxCount = decision.OccurrenceCount
		}
	}
	incident, err := provider.GetIncident(opened.IncidentID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if maxCount != continuations+1 || incident.OccurrenceCount != continuations+1 ||
		incident.DetectionLastSeen == nil || !incident.DetectionLastSeen.Equal(base.Add(time.Minute)) ||
		fmt.Sprint(incident.Content["Frequency"]) != "401" {
		t.Fatalf("concurrent continuation max=%d incident=%+v", maxCount, incident)
	}
}

func TestDetectionIdentityFingerprintRejectsPayloadShapedInput(t *testing.T) {
	_, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs",
	})
	if !errors.Is(err, storage.ErrInvalidDetectionOccurrence) {
		t.Fatalf("missing condition key error = %v", err)
	}

	first, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: "stable-key",
	})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	second, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		Version: storage.DetectionIdentityVersion, AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: "stable-key",
	})
	if err != nil || first != second {
		t.Fatalf("default version fingerprint mismatch: %q %q %v", first, second, err)
	}
}

func TestDetectionIdentityFingerprintCanonicalizesLabelsButPreservesConditionCase(t *testing.T) {
	upper, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		AgentKind: " Detect ", Source: "LOKI-PROD", Service: "Checkout",
		SignalKind: "LOGS", ConditionKey: "Pattern-Key",
	})
	if err != nil {
		t.Fatalf("upper fingerprint: %v", err)
	}
	lower, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		AgentKind: "detect", Source: "loki-prod", Service: "checkout",
		SignalKind: "logs", ConditionKey: "Pattern-Key",
	})
	if err != nil || upper != lower {
		t.Fatalf("case-folded fingerprints = %q/%q, %v", upper, lower, err)
	}
	differentCondition, err := storage.DetectionIdentityFingerprint(storage.DetectionIdentity{
		AgentKind: "detect", Source: "loki-prod", Service: "checkout",
		SignalKind: "logs", ConditionKey: "pattern-key",
	})
	if err != nil || differentCondition == lower {
		t.Fatalf("condition-key fingerprints = %q/%q, %v", lower, differentCondition, err)
	}
}

func TestMemoryDetectionEpisodeContinuousTrafficNeverReopens(t *testing.T) {
	provider := storage.NewMemory()
	store := provider.(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{
		AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: "condition",
	}
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	var episodeID, incidentID string
	for index := 0; index < 5; index++ {
		decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity, Frequency: 1, Severity: "medium", ReceivedAt: base.Add(time.Duration(index) * 9 * time.Minute),
		})
		if err != nil {
			t.Fatalf("RecordDetectionOccurrence(%d): %v", index, err)
		}
		if index == 0 {
			episodeID, incidentID = decision.EpisodeID, decision.IncidentID
			if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
				EpisodeID: decision.EpisodeID, ClaimToken: decision.ClaimToken, Outcome: "sent", CompletedAt: decision.LastSeen,
			}); err != nil {
				t.Fatalf("CompleteDetectionNotification: %v", err)
			}
			if err := provider.SaveIncident(&storage.IncidentRecord{ID: decision.IncidentID, CreatedAt: decision.FirstSeen}); err != nil {
				t.Fatalf("SaveIncident: %v", err)
			}
		}
		if decision.EpisodeID != episodeID || decision.IncidentID != incidentID {
			t.Fatalf("continuous traffic reopened at index %d: %+v", index, decision)
		}
	}
}

func TestMemoryDetectionEpisodeMissingDeliveredIncidentReopens(t *testing.T) {
	store := storage.NewMemory().(storage.DetectionEpisodeStore)
	identity := storage.DetectionIdentity{AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: "condition"}
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	opened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{Identity: identity, Frequency: 1, Severity: "medium", ReceivedAt: base})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence: %v", err)
	}
	if err := store.CompleteDetectionNotification(storage.DetectionNotificationCompletion{EpisodeID: opened.EpisodeID, ClaimToken: opened.ClaimToken, Outcome: "sent"}); err != nil {
		t.Fatalf("CompleteDetectionNotification: %v", err)
	}
	reopened, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{Identity: identity, Frequency: 1, Severity: "medium", ReceivedAt: base.Add(time.Second)})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence(reopen): %v", err)
	}
	if reopened.Action != storage.DetectionEpisodeReopened || reopened.IncidentID == opened.IncidentID {
		t.Fatalf("orphan recurrence = %+v", reopened)
	}
}

func TestMemoryDetectionEpisodePrunesOldestClosedHistory(t *testing.T) {
	provider := storage.NewMemory()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	for index := 0; index <= storage.MaxClosedDetectionEpisodesDefault; index++ {
		decision, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: storage.DetectionIdentity{
				AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs",
				ConditionKey: fmt.Sprintf("condition-%04d", index),
			},
			Frequency: 1, Severity: "low", ReceivedAt: base.Add(time.Duration(index) * time.Second),
		})
		if err != nil {
			t.Fatalf("RecordDetectionOccurrence(%d): %v", index, err)
		}
		if err := store.CloseDetectionEpisodeByIncident(decision.IncidentID, decision.LastSeen); err != nil {
			t.Fatalf("CloseDetectionEpisodeByIncident(%d): %v", index, err)
		}
	}
	oldest, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: "condition-0000",
		},
		Frequency: 1, Severity: "low", ReceivedAt: base.Add(2 * time.Hour),
	})
	if err != nil || oldest.Action != storage.DetectionEpisodeOpened {
		t.Fatalf("oldest retained decision = %+v, %v", oldest, err)
	}
}

func TestFileDetectionEpisodeRetentionKeepsActiveEpisodes(t *testing.T) {
	provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir(), MaxIncidents: 2})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer provider.Close()
	store := provider.(storage.DetectionEpisodeStore)
	base := time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	identity := func(condition string) storage.DetectionIdentity {
		return storage.DetectionIdentity{
			AgentKind: "detect", Source: "source", Service: "service", SignalKind: "logs", ConditionKey: condition,
		}
	}
	for index := 0; index < 3; index++ {
		condition := fmt.Sprintf("closed-%d", index)
		decision, recordErr := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity(condition), Frequency: 1, Severity: "low", ReceivedAt: base.Add(time.Duration(index) * time.Second),
		})
		if recordErr != nil {
			t.Fatalf("RecordDetectionOccurrence(%s): %v", condition, recordErr)
		}
		if closeErr := store.CloseDetectionEpisodeByIncident(decision.IncidentID, decision.LastSeen); closeErr != nil {
			t.Fatalf("CloseDetectionEpisodeByIncident(%s): %v", condition, closeErr)
		}
	}
	forgotten, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity("closed-0"), Frequency: 1, Severity: "low", ReceivedAt: base.Add(time.Hour),
	})
	if err != nil || forgotten.Action != storage.DetectionEpisodeOpened {
		t.Fatalf("forgotten closed episode = %+v, %v", forgotten, err)
	}

	for _, condition := range []string{"active-1", "active-2", "active-3"} {
		if _, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
			Identity: identity(condition), Frequency: 1, Severity: "low", ReceivedAt: base.Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("open %s: %v", condition, err)
		}
	}
	continued, err := store.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: identity("active-1"), Frequency: 1, Severity: "low",
		ReceivedAt: base.Add(2*time.Hour + time.Second), ExistingOnly: true,
	})
	if err != nil || continued.OccurrenceCount != 2 {
		t.Fatalf("active episode after pruning = %+v, %v", continued, err)
	}
}

func runDetectionIncidentOwnershipContract(t *testing.T, provider storage.Provider) {
	t.Helper()
	detectionStore := provider.(storage.DetectionIncidentStore)
	created := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	firstSeen := created
	lastSeen := created.Add(time.Minute)
	initial := &storage.IncidentRecord{
		ID: "stable-detection-incident", CreatedAt: created, Title: "initial",
		DetectionFingerprint: "fingerprint", DetectionEpisodeID: "episode",
		OccurrenceCount: 1, DetectionFirstSeen: &firstSeen, DetectionLastSeen: &lastSeen,
		HighestObservedSeverity: "critical", Content: map[string]interface{}{"Frequency": int64(1), "Severity": "critical"},
	}
	if err := detectionStore.SaveDetectionIncident(initial); err != nil {
		t.Fatalf("SaveDetectionIncident(initial): %v", err)
	}
	stale, err := provider.GetIncident(initial.ID)
	if err != nil {
		t.Fatalf("GetIncident(stale): %v", err)
	}
	operator, err := provider.GetIncident(initial.ID)
	if err != nil {
		t.Fatalf("GetIncident(operator): %v", err)
	}
	acked := created.Add(2 * time.Minute)
	resolved := created.Add(3 * time.Minute)
	operator.AckedAt = &acked
	operator.Resolved = true
	operator.ResolvedAt = &resolved
	operator.TeamID = "operator-team"
	operator.AssignedTeamID = "sre"
	operator.AssignedMemberIDs = []string{"alice", "bob"}
	if err := provider.SaveIncident(operator); err != nil {
		t.Fatalf("SaveIncident(operator): %v", err)
	}

	stale.Title = "latest detection"
	stale.NotifyStatus = "sent"
	stale.ChannelsNotified = []string{"slack"}
	stale.OccurrenceCount = 2
	stale.DetectionLastSeen = &resolved
	stale.HighestObservedSeverity = "high"
	stale.Content["Frequency"] = int64(2)
	stale.Content["Severity"] = "high"
	if err := detectionStore.SaveDetectionIncident(stale); err != nil {
		t.Fatalf("SaveDetectionIncident(stale): %v", err)
	}
	got, err := provider.GetIncident(initial.ID)
	if err != nil {
		t.Fatalf("GetIncident(final): %v", err)
	}
	if !got.Resolved || got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolved) ||
		got.AckedAt == nil || !got.AckedAt.Equal(acked) || got.TeamID != "operator-team" ||
		got.AssignedTeamID != "sre" || fmt.Sprint(got.AssignedMemberIDs) != "[alice bob]" {
		t.Fatalf("operator fields reverted by stale detection save: %+v", got)
	}
	if got.NotifyStatus != "sent" || got.OccurrenceCount != 2 || got.HighestObservedSeverity != "critical" ||
		got.Content["Severity"] != "critical" {
		t.Fatalf("detection fields not advanced: %+v", got)
	}
}

func TestMemoryDetectionIncidentPreservesOperatorState(t *testing.T) {
	runDetectionIncidentOwnershipContract(t, storage.NewMemory())
}

func TestFileDetectionIncidentPreservesOperatorState(t *testing.T) {
	provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer provider.Close()
	runDetectionIncidentOwnershipContract(t, provider)
}

func TestPostgresDetectionIncidentPreservesOperatorState(t *testing.T) {
	runDetectionIncidentOwnershipContract(t, newTestPostgres(t))
}

func TestPostgresDetectionIncidentConcurrentOperatorRace(t *testing.T) {
	provider := newTestPostgres(t)
	detectionStore := provider.(storage.DetectionIncidentStore)
	created := time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC)
	initial := &storage.IncidentRecord{
		ID: "detection-operator-race", CreatedAt: created,
		DetectionFingerprint: "fingerprint", DetectionEpisodeID: "episode",
		OccurrenceCount: 1, Content: map[string]interface{}{"Frequency": int64(1)},
	}
	if err := detectionStore.SaveDetectionIncident(initial); err != nil {
		t.Fatalf("SaveDetectionIncident(initial): %v", err)
	}
	stale, err := provider.GetIncident(initial.ID)
	if err != nil {
		t.Fatalf("GetIncident(stale): %v", err)
	}
	resolved := created.Add(time.Minute)
	operator := storage.CloneIncidentRecord(stale)
	operator.Resolved = true
	operator.ResolvedAt = &resolved
	operator.AckedAt = &resolved
	operator.AssignedTeamID = "sre"
	operator.AssignedMemberIDs = []string{"alice"}
	stale.OccurrenceCount = 2
	stale.NotifyStatus = "sent"
	stale.Content["Frequency"] = int64(2)

	const writers = 50
	start := make(chan struct{})
	errorsCh := make(chan error, writers*2)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			errorsCh <- provider.SaveIncident(storage.CloneIncidentRecord(operator))
		}()
		go func() {
			defer wait.Done()
			<-start
			errorsCh <- detectionStore.SaveDetectionIncident(storage.CloneIncidentRecord(stale))
		}()
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("concurrent save: %v", err)
		}
	}
	got, err := provider.GetIncident(initial.ID)
	if err != nil {
		t.Fatalf("GetIncident(final): %v", err)
	}
	if !got.Resolved || got.ResolvedAt == nil || got.AckedAt == nil ||
		got.AssignedTeamID != "sre" || fmt.Sprint(got.AssignedMemberIDs) != "[alice]" ||
		got.OccurrenceCount != 2 || got.NotifyStatus != "sent" {
		t.Fatalf("concurrent final incident = %+v", got)
	}
}

func TestPostgresConnectionPoolBounds(t *testing.T) {
	provider := newTestPostgres(t)
	db := provider.(storage.SQLAccessor).DB()
	if got := db.Stats().MaxOpenConnections; got != 20 {
		t.Fatalf("max open connections=%d, want 20", got)
	}
	connections := make([]interface{ Close() error }, 0, 10)
	for index := 0; index < 10; index++ {
		connection, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("Conn(%d): %v", index, err)
		}
		connections = append(connections, connection)
	}
	for _, connection := range connections {
		if err := connection.Close(); err != nil {
			t.Fatalf("Close connection: %v", err)
		}
	}
	if got := db.Stats().Idle; got > 5 {
		t.Fatalf("idle connections=%d, want at most 5", got)
	}
}
