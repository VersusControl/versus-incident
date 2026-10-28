package servicehealth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCollectorUsesBucketsNotLifetimePatternCounts(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
	if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "loki", Service: "api", PatternID: "p", ObservedAt: now.Add(-time.Minute), Frequency: 4}); err != nil {
		t.Fatal(err)
	}
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, SourceIDs: []string{"loki"}, Now: func() time.Time { return now }, Services: func() []servicehealth.ServiceMetadata {
		return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}, {OrgID: "org-b", Name: "api"}}
	}, Patterns: func() []servicehealth.PatternMetadata {
		return []servicehealth.PatternMetadata{{OrgID: "org-a", Service: "api", LastSeen: now}}
	}})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Services) != 1 || snapshot.Services[0].Logs.MatchedLogs != 4 {
		t.Fatalf("services = %#v", snapshot.Services)
	}
	if _, exists := snapshot.Facets["regressing"]; exists {
		t.Fatal("OSS exposed learned regression facet")
	}
}

func TestCollectorEmptyInstallAndSourceFailureAreHonest(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
	empty := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, Now: func() time.Time { return now }})
	snapshot, err := empty.Collect(context.Background(), "org-a")
	if err != nil || len(snapshot.Services) != 0 || snapshot.Capabilities[0].Measures["activity"].State != core.HealthNotConfigured {
		t.Fatalf("empty = %#v, err %v", snapshot, err)
	}
	if err := manager.RecordSourceHealth(context.Background(), core.SourceHealthObservation{OrgID: "org-a", SourceID: "bad", AttemptedAt: now, ErrorClass: "secret backend detail"}); err != nil {
		t.Fatal(err)
	}
	failed := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, SourceIDs: []string{"bad"}, Now: func() time.Time { return now }, Services: func() []servicehealth.ServiceMetadata {
		return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}}
	}})
	snapshot, err = failed.Collect(context.Background(), "org-a")
	if err != nil || snapshot.Services[0].Availability["logs"].State != core.HealthError {
		t.Fatalf("failed = %#v, err %v", snapshot, err)
	}
	if snapshot.Services[0].Availability["logs"].ReasonCode == "secret backend detail" {
		t.Fatal("raw backend error escaped")
	}
}

func TestSourceFailurePreservesOtherSourceEvidence(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
	if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "good", Service: "api", PatternID: "p", ObservedAt: now.Add(-time.Minute), Frequency: 3}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordSourceHealth(context.Background(), core.SourceHealthObservation{OrgID: "org-a", SourceID: "good", AttemptedAt: now, Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordSourceHealth(context.Background(), core.SourceHealthObservation{OrgID: "org-a", SourceID: "bad", AttemptedAt: now, ErrorClass: "connection"}); err != nil {
		t.Fatal(err)
	}
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, SourceIDs: []string{"good", "bad"}, Now: func() time.Time { return now }})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Services) != 1 || snapshot.Services[0].Logs.MatchedLogs != 3 || snapshot.Services[0].Availability["logs"].State != core.HealthReady || !snapshot.Coverage.Partial {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestOlderRevisionCannotOverwriteNewSettings(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	if _, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	err := manager.SaveSnapshot("org-a", servicehealth.SnapshotEnvelope{SnapshotID: "old", SettingsRevision: 0, GeneratedAt: time.Now().UTC()})
	if err != servicehealth.ErrConflict {
		t.Fatalf("error = %v", err)
	}
}

func TestCollectorBoundsFiveHundredServicesWithoutSourceQueries(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
	services := make([]servicehealth.ServiceMetadata, 0, servicehealth.MaxServicesPerSnapshot+1)
	for index := 0; index <= servicehealth.MaxServicesPerSnapshot; index++ {
		services = append(services, servicehealth.ServiceMetadata{OrgID: "org-a", Name: fmt.Sprintf("service-%03d", index)})
	}
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, Now: func() time.Time { return now }, Services: func() []servicehealth.ServiceMetadata { return services }})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Services) != servicehealth.MaxServicesPerSnapshot || !snapshot.Coverage.Partial || snapshot.Coverage.TotalServices != servicehealth.MaxServicesPerSnapshot+1 {
		t.Fatalf("coverage = %#v, services = %d", snapshot.Coverage, len(snapshot.Services))
	}
}

func TestSnapshotOrEmptyReturnsNoPreviewOrPremiumMeasurements(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	snapshot, err := manager.SnapshotOrEmpty("org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Services) != 0 || len(snapshot.Domains) != 0 || snapshot.SettingsRevision != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, ok := snapshot.Facets["silent_regressions"]; ok {
		t.Fatal("premium counter present")
	}
}

func TestWindowChangeInvalidatesOlderSnapshot(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	old := servicehealth.SnapshotEnvelope{SnapshotID: "old", GeneratedAt: time.Now().UTC(), SettingsRevision: 0, WindowSeconds: 300, Services: []servicehealth.ServiceSnapshot{{OrgID: "org-a", Service: "api"}}, Capabilities: []servicehealth.Capability{{Family: "logs", Measures: map[string]core.MeasureAvailability{"activity": {State: core.HealthReady}}}}}
	if err := manager.SaveSnapshot("org-a", old); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60, Revision: 0}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.SnapshotOrEmpty("org-a")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SettingsRevision != 0 || snapshot.WindowSeconds != 300 || snapshot.PendingSettings == nil || snapshot.PendingSettings.Revision != 1 || snapshot.PendingSettings.WindowSeconds != 60 || len(snapshot.Services) != 1 || snapshot.Services[0].Availability["logs"].State != core.HealthStale || snapshot.Capabilities[0].Measures["activity"].ReasonCode != "settings_changed" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCollectorNormalizesEmptyServiceBeforeSourceCoverage(t *testing.T) {
	for _, test := range []struct {
		name       string
		source     core.SourceHealthObservation
		wantState  core.HealthState
		wantReason string
	}{
		{name: "stale", source: core.SourceHealthObservation{Succeeded: true}, wantState: core.HealthStale, wantReason: "source_stale"},
		{name: "error", source: core.SourceHealthObservation{ErrorClass: "connection"}, wantState: core.HealthPartial, wantReason: "source_partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := storage.NewMemory()
			manager := servicehealth.NewManager(store)
			now := time.Date(2026, 9, 9, 12, 5, 0, 0, time.UTC)
			if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "loki", PatternID: "p", ObservedAt: now.Add(-time.Minute), Frequency: 1}); err != nil {
				t.Fatal(err)
			}
			test.source.OrgID = "org-a"
			test.source.SourceID = "loki"
			test.source.AttemptedAt = now.Add(-10 * time.Minute)
			if err := manager.RecordSourceHealth(context.Background(), test.source); err != nil {
				t.Fatal(err)
			}
			collector := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: manager, Store: store, SourceIDs: []string{"loki"}, Now: func() time.Time { return now }})
			snapshot, err := collector.Collect(context.Background(), "org-a")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Services) != 1 {
				t.Fatalf("snapshot = %#v", snapshot)
			}
			availability := snapshot.Services[0].Availability["logs"]
			if snapshot.Services[0].Service != "_unknown" || availability.State != test.wantState || availability.ReasonCode != test.wantReason {
				t.Fatalf("snapshot = %#v", snapshot)
			}
		})
	}
}

func TestSnapshotOrEmptyMarksExpiredSnapshotStale(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	old := servicehealth.SnapshotEnvelope{
		SnapshotID: "old", GeneratedAt: time.Now().UTC().Add(-time.Hour), SettingsRevision: 0,
		Services:     []servicehealth.ServiceSnapshot{{OrgID: "org-a", Service: "api", Availability: map[string]core.MeasureAvailability{"logs": {State: core.HealthReady}, "internal": {State: core.HealthReady}}}},
		Capabilities: []servicehealth.Capability{{Family: "logs", Measures: map[string]core.MeasureAvailability{"activity": {State: core.HealthReady}}}},
	}
	if err := manager.SaveSnapshot("org-a", old); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.SnapshotOrEmpty("org-a")
	if err != nil || snapshot.Services[0].Availability["logs"].State != core.HealthStale || snapshot.Services[0].Availability["internal"].ReasonCode != "snapshot_stale" {
		t.Fatalf("snapshot = %#v, err %v", snapshot, err)
	}
}

func TestPeerCollectorsUseDeterministicSnapshotID(t *testing.T) {
	store := storage.NewMemory()
	now := time.Date(2026, 9, 9, 12, 5, 45, 0, time.UTC)
	first := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: servicehealth.NewManager(store), Store: store, Now: func() time.Time { return now }})
	second := servicehealth.NewCollector(servicehealth.CollectorOptions{Manager: servicehealth.NewManager(store), Store: store, Now: func() time.Time { return now.Add(10 * time.Second) }})
	one, err := first.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if one.SnapshotID != two.SnapshotID || !one.GeneratedAt.Equal(two.GeneratedAt) {
		t.Fatalf("peer snapshots differ: %#v %#v", one, two)
	}
}

func TestIncidentJoinWithholdsCountsWhenBoundIsReached(t *testing.T) {
	store := storage.NewMemory()
	now := time.Now().UTC()
	for index := 0; index <= 2000; index++ {
		if err := store.SaveIncident(&storage.IncidentRecord{ID: fmt.Sprintf("incident-%04d", index), OrgID: "org-a", Service: "api", CreatedAt: now.Add(-time.Duration(index) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: servicehealth.NewManager(store), Store: store, Now: func() time.Time { return now },
		Services: func() []servicehealth.ServiceMetadata {
			return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}}
		},
		Patterns: func() []servicehealth.PatternMetadata {
			return []servicehealth.PatternMetadata{{OrgID: "org-a", Service: "api", LastSeen: now}}
		},
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Coverage.Partial || snapshot.Services[0].ActiveIncidents != nil || snapshot.Services[0].Availability["internal"].State != core.HealthPartial || snapshot.Services[0].Availability["internal"].ReasonCode != "join_truncated" || snapshot.Services[0].AssessmentBasis != "Patterns + incidents" {
		t.Fatalf("truncated incident coverage claimed exact counts: %#v", snapshot)
	}
}

type incidentReadFailureStore struct {
	storage.Provider
	storage.BlobCAS
}

func (store incidentReadFailureStore) ListIncidents(int) ([]*storage.IncidentRecord, error) {
	return nil, errors.New("incident read failed")
}

func TestIncidentReadFailureMarksCoveragePartial(t *testing.T) {
	memory := storage.NewMemory()
	store := incidentReadFailureStore{Provider: memory, BlobCAS: memory.(storage.BlobCAS)}
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: servicehealth.NewManager(store), Store: store,
		Services: func() []servicehealth.ServiceMetadata {
			return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}}
		},
		Patterns: func() []servicehealth.PatternMetadata {
			return []servicehealth.PatternMetadata{{OrgID: "org-a", Service: "api", LastSeen: time.Now().UTC()}}
		},
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Coverage.Partial || snapshot.Services[0].Availability["internal"].State != core.HealthError || snapshot.Services[0].Availability["internal"].ReasonCode != "internal_read_failed" || snapshot.Services[0].AssessmentBasis != "Patterns + incidents" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
