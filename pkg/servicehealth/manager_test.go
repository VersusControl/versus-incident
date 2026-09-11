package servicehealth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type failingReadStore struct{ storage.Provider }

func (store failingReadStore) ReadBlob(string) ([]byte, error) { return nil, errors.New("read failed") }

type countingStore struct {
	storage.Provider
	mu     sync.Mutex
	reads  int
	writes int
}

type failOnceCASStore struct {
	storage.Provider
	mu   sync.Mutex
	fail bool
}

func (store *failOnceCASStore) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail {
		store.fail = false
		return false, errors.New("injected CAS failure")
	}
	return store.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func (store *countingStore) ReadBlob(key string) ([]byte, error) {
	store.mu.Lock()
	store.reads++
	store.mu.Unlock()
	return store.Provider.ReadBlob(key)
}

func (store *countingStore) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	store.mu.Lock()
	store.writes++
	store.mu.Unlock()
	return store.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func TestSettingsPersistenceBoundsAndRevision(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	got, err := manager.LoadSettings("org-a")
	if err != nil || got.IntervalSeconds != 60 || got.WindowSeconds != 300 || got.Revision != 0 {
		t.Fatalf("defaults = %#v, err %v", got, err)
	}
	if _, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 29, WindowSeconds: 300}); err == nil {
		t.Fatal("invalid interval accepted")
	}
	saved, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60})
	if err != nil || saved.Revision != 1 {
		t.Fatalf("saved = %#v, err %v", saved, err)
	}
	reloaded, err := servicehealth.NewManager(store).LoadSettings("org-a")
	if err != nil || reloaded != saved {
		t.Fatalf("reloaded = %#v, err %v", reloaded, err)
	}
	if _, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 60, WindowSeconds: 300, Revision: 0}); !errors.Is(err, servicehealth.ErrConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestSettingsReadFailureDoesNotReturnDefaults(t *testing.T) {
	manager := servicehealth.NewManager(failingReadStore{Provider: storage.NewMemory()})
	if settings, err := manager.LoadSettings("org-a"); err == nil || settings == servicehealth.DefaultSettings() {
		t.Fatalf("settings = %#v, err %v", settings, err)
	}
}

func TestInvalidStoredSettingsReturnDiagnosedDefaultsAndCanBeRepaired(t *testing.T) {
	store := storage.NewMemory()
	sum := sha256.Sum256([]byte("org-a"))
	key := "models/settings/service-health/" + hex.EncodeToString(sum[:16])
	if err := store.WriteBlob(key, []byte(`{"interval_seconds":0}`)); err != nil {
		t.Fatal(err)
	}
	manager := servicehealth.NewManager(store)
	settings, err := manager.LoadSettings("org-a")
	if err != nil || settings.IntervalSeconds != servicehealth.DefaultIntervalSeconds || settings.Diagnostic != "stored_settings_invalid" {
		t.Fatalf("settings = %#v, err %v", settings, err)
	}
	repaired, err := manager.UpdateSettings("org-a", servicehealth.Settings{IntervalSeconds: 30, WindowSeconds: 60})
	if err != nil || repaired.Revision != 1 || repaired.Diagnostic != "" {
		t.Fatalf("repaired = %#v, err %v", repaired, err)
	}
}

func TestSnapshotHistoryTrimsToMarshaledByteLimit(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	services := make([]servicehealth.ServiceSnapshot, servicehealth.MaxServicesPerSnapshot)
	for index := range services {
		services[index] = servicehealth.ServiceSnapshot{OrgID: "org-a", Service: fmt.Sprintf("service-%03d-with-realistic-name", index), Availability: map[string]core.MeasureAvailability{"logs": {State: core.HealthReady}}}
	}
	start := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < servicehealth.MaxSnapshots; index++ {
		snapshot := servicehealth.SnapshotEnvelope{SnapshotID: fmt.Sprintf("snapshot-%02d", index), GeneratedAt: start.Add(time.Duration(index) * time.Minute), Services: services}
		if err := manager.SaveSnapshot("org-a", snapshot); err != nil {
			t.Fatalf("save %d: %v", index, err)
		}
	}
	sum := sha256.Sum256([]byte("org-a"))
	data, err := store.ReadBlob("models/service-health/snapshots/" + hex.EncodeToString(sum[:16]))
	if err != nil || len(data) > servicehealth.MaxSnapshotBytes {
		t.Fatalf("history bytes = %d, err %v", len(data), err)
	}
	var history struct {
		Snapshots []servicehealth.SnapshotEnvelope `json:"snapshots"`
	}
	if err := json.Unmarshal(data, &history); err != nil || len(history.Snapshots) >= servicehealth.MaxSnapshots || history.Snapshots[len(history.Snapshots)-1].SnapshotID != "snapshot-59" {
		t.Fatalf("history count = %d, err %v", len(history.Snapshots), err)
	}
}

func TestSingleOversizedSnapshotReturnsBoundedError(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	err := manager.SaveSnapshot("org-a", servicehealth.SnapshotEnvelope{
		SnapshotID: "oversized", GeneratedAt: time.Now().UTC(),
		Services: []servicehealth.ServiceSnapshot{{OrgID: "org-a", Service: strings.Repeat("x", servicehealth.MaxSnapshotBytes)}},
	})
	if err == nil || !strings.Contains(err.Error(), "newest snapshot") || len(err.Error()) > 128 {
		t.Fatalf("error = %v", err)
	}
}

func TestLogBucketRetentionIsMaximumWindowPlusSlack(t *testing.T) {
	manager := servicehealth.NewManager(storage.NewMemory())
	end := time.Now().UTC().Truncate(time.Minute)
	for minute := 0; minute < 180; minute++ {
		observedAt := end.Add(-time.Duration(minute) * time.Minute)
		if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "loki", Service: "api", PatternID: fmt.Sprintf("p-%d", minute), ObservedAt: observedAt, Frequency: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err := manager.QueryWindow("org-a", end.Add(time.Minute), time.Duration(servicehealth.MaxWindowSeconds)*time.Second)
	if err != nil || len(rows) != 1 || rows[0].MatchedLogs > int64(servicehealth.MaxWindowSeconds/60+2) {
		t.Fatalf("rows = %#v, err %v", rows, err)
	}
}

func TestWindowBucketsAreEstimatedScopedAndCountOnly(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 9, 12, 0, 30, 0, time.UTC)
	observation := core.LogHealthObservation{OrgID: "org-a", SourceID: "source", Service: "api", PatternID: "p1", ObservedAt: now, Frequency: 7, NewPattern: true, LifecycleClassified: true, LifecycleClass: "spike"}
	if err := manager.RecordLogHealth(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecordLogHealth(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	rows, _, err := manager.QueryWindow("org-a", now.Add(time.Minute), 5*time.Minute)
	if err != nil || len(rows) != 1 || rows[0].MatchedLogs != 14 || rows[0].SpikingPatterns != 1 || !rows[0].Estimated {
		t.Fatalf("rows = %#v, err %v", rows, err)
	}
	other, _, err := manager.QueryWindow("org-b", now.Add(time.Minute), 5*time.Minute)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-org rows = %#v, err %v", other, err)
	}
	data, _ := store.ReadBlob("models/service-health/state/org-a")
	if len(data) != 0 {
		t.Fatal("org id used directly in persistence key")
	}
}

func TestPatternCardinalityIsBoundedAndSurvivesRestart(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for index := 0; index < servicehealth.MaxPatternIDsPerWindow+500; index++ {
		observedAt := now.Add(time.Duration(index/servicehealth.MaxPatternIDsPerBucket) * time.Minute)
		err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{
			OrgID: "org-a", SourceID: "source", Service: "api", PatternID: fmt.Sprintf("pattern-%04d", index),
			ObservedAt: observedAt, Frequency: 1, NewPattern: true, LifecycleClassified: true, LifecycleClass: "spike",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	end := now.Add(10 * time.Minute)
	rows, _, err := manager.QueryWindow("org-a", end, time.Hour)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	row := rows[0]
	if row.UniquePatterns != servicehealth.MaxPatternIDsPerWindow || row.NewPatterns != servicehealth.MaxPatternIDsPerWindow || row.SpikingPatterns != servicehealth.MaxPatternIDsPerWindow || !row.Truncated || row.PartialReason != "pattern_cardinality_limited" {
		t.Fatalf("row=%#v", row)
	}
	sum := sha256.Sum256([]byte("org-a"))
	data, err := store.ReadBlob("models/service-health/state/" + hex.EncodeToString(sum[:16]))
	if err != nil || len(data) > servicehealth.MaxStateBytes {
		t.Fatalf("state bytes=%d err=%v", len(data), err)
	}
	if strings.Contains(string(data), "pattern-0000") {
		t.Fatal("persisted state retained the caller-supplied pattern identifier")
	}
	restartedRows, _, err := servicehealth.NewManager(store).QueryWindow("org-a", end, time.Hour)
	if err != nil || len(restartedRows) != 1 || !reflect.DeepEqual(restartedRows[0], row) {
		t.Fatalf("restarted rows=%#v err=%v", restartedRows, err)
	}
}

func TestConcurrentHighCardinalityRemainsBoundedAndPartial(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
	const workers = 16
	const observationsPerWorker = 200
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for index := 0; index < observationsPerWorker; index++ {
				if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "source", Service: "api", PatternID: fmt.Sprintf("%d-%d", worker, index), ObservedAt: now, Frequency: 1}); err != nil {
					t.Errorf("record: %v", err)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: manager, Store: store, SourceIDs: []string{"source"}, Now: func() time.Time { return now.Add(time.Minute) },
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil || len(snapshot.Services) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	service := snapshot.Services[0]
	if service.Logs.MatchedLogs != workers*observationsPerWorker || service.Logs.UniquePatterns != servicehealth.MaxPatternIDsPerBucket || !service.Logs.Truncated || service.Logs.PartialReason != "pattern_cardinality_limited" {
		t.Fatalf("logs=%#v", service.Logs)
	}
	if service.Availability["logs"].State != core.HealthPartial || service.Availability["logs"].ReasonCode != "pattern_cardinality_limited" || !snapshot.Coverage.Partial {
		t.Fatalf("snapshot did not expose partial provenance: %#v", snapshot)
	}
	response, err := json.Marshal(snapshot)
	if err != nil || len(response) > servicehealth.MaxSnapshotBytes {
		t.Fatalf("snapshot bytes=%d err=%v", len(response), err)
	}
}

func TestOversizedPersistedStateRejectedBeforeDecode(t *testing.T) {
	store := storage.NewMemory()
	sum := sha256.Sum256([]byte("org-a"))
	if err := store.WriteBlob("models/service-health/state/"+hex.EncodeToString(sum[:16]), make([]byte, servicehealth.MaxStateBytes+1)); err != nil {
		t.Fatal(err)
	}
	_, _, err := servicehealth.NewManager(store).QueryWindow("org-a", time.Now().UTC(), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error=%v", err)
	}
}

func TestPersistedStateRejectsOversizedPatternIdentity(t *testing.T) {
	store := storage.NewMemory()
	sum := sha256.Sum256([]byte("org-a"))
	data := fmt.Appendf(nil, `{"buckets":[{"service":"api","source_id":"source","start":"2026-09-10T12:00:00Z","pattern_ids":[%q]}]}`, strings.Repeat("a", 1024))
	if err := store.WriteBlob("models/service-health/state/"+hex.EncodeToString(sum[:16]), data); err != nil {
		t.Fatal(err)
	}
	_, _, err := servicehealth.NewManager(store).QueryWindow("org-a", time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC), time.Hour)
	if err == nil || !strings.Contains(err.Error(), "invalid pattern identity") {
		t.Fatalf("error=%v", err)
	}
}

func TestFailedStatePersistRestoresBoundedPendingData(t *testing.T) {
	store := &failOnceCASStore{Provider: storage.NewMemory(), fail: true}
	manager := servicehealth.NewManager(store)
	now := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	for index := 0; index < servicehealth.MaxPatternIDsPerBucket+50; index++ {
		if err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: "source", Service: "api", PatternID: fmt.Sprintf("pattern-%d", index), ObservedAt: now, Frequency: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := manager.QueryWindow("org-a", now.Add(time.Minute), time.Hour); err == nil {
		t.Fatal("injected persistence failure was not returned")
	}
	rows, _, err := manager.QueryWindow("org-a", now.Add(time.Minute), time.Hour)
	if err != nil || len(rows) != 1 || rows[0].MatchedLogs != servicehealth.MaxPatternIDsPerBucket+50 || rows[0].UniquePatterns != servicehealth.MaxPatternIDsPerBucket || !rows[0].Truncated {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

func TestRecordLogHealthBuffersWithoutStorageIOAndConcurrentLoss(t *testing.T) {
	store := &countingStore{Provider: storage.NewMemory()}
	manager := servicehealth.NewManager(store)
	now := time.Now().UTC().Truncate(time.Minute)
	const sources = 8
	const observationsPerSource = 250
	var wait sync.WaitGroup
	for source := 0; source < sources; source++ {
		wait.Add(1)
		go func(source int) {
			defer wait.Done()
			for index := 0; index < observationsPerSource; index++ {
				err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "org-a", SourceID: fmt.Sprintf("source-%d", source), Service: "api", PatternID: "pattern", ObservedAt: now, Frequency: 1})
				if err != nil {
					t.Errorf("record source %d: %v", source, err)
					return
				}
			}
		}(source)
	}
	wait.Wait()
	if store.reads != 0 || store.writes != 0 {
		t.Fatalf("hot path performed storage IO: reads=%d writes=%d", store.reads, store.writes)
	}
	rows, _, err := manager.QueryWindow("org-a", now.Add(time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, row := range rows {
		total += row.MatchedLogs
	}
	if total != sources*observationsPerSource || store.writes != 1 {
		t.Fatalf("total=%d writes=%d", total, store.writes)
	}
}

func TestTrainingProjectionDoesNotInventVerdict(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	now := time.Now().UTC()
	err := manager.RecordLogHealth(context.Background(), core.LogHealthObservation{OrgID: "default", SourceID: "file", Service: "api", PatternID: "p", ObservedAt: now, Frequency: 2, LifecycleClass: "spike", LifecycleClassified: false})
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := manager.QueryWindow("default", now.Add(time.Minute), 5*time.Minute)
	if err != nil || len(rows) != 1 || rows[0].SpikingPatterns != 0 {
		t.Fatalf("rows = %#v, err %v", rows, err)
	}
}
