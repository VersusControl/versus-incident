package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/stats"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type baselineSettingsCountingStore struct {
	storage.Provider
	reads int
}

func (store *baselineSettingsCountingStore) ReadBlob(name string) ([]byte, error) {
	store.reads++
	return store.Provider.ReadBlob(name)
}

func TestLogBaselineProviderSelectionBoundsAndOrgIsolation(t *testing.T) {
	catalog, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 52; index++ {
		id := string(rune('A' + index))
		catalog.patterns[id] = &Pattern{ID: id, OrgID: "acme", Service: "api", Count: 10, Verdict: "known", BaselineFrequency: float64(index), LastSeen: time.Unix(int64(index), 0)}
	}
	catalog.patterns["foreign"] = &Pattern{ID: "foreign", OrgID: "other", Service: "api", Count: 100, Verdict: "known"}
	catalog.patterns["worker"] = &Pattern{ID: "worker", OrgID: "acme", Service: "worker", Count: 100, Verdict: "known"}
	provider := newLogBaselineProvider(catalog, tenancy.NewOrgScope("acme"), 5, storage.NewMemory())
	got, err := provider.DescribeBaselines(context.Background(), core.BaselineRequest{OrgID: "ignored", Service: "api", Signal: "logs", Limit: 50, At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 50 || !got.Truncated || got.Omitted != 2 || got.Records[0].PatternID >= got.Records[49].PatternID {
		t.Fatalf("bounded records = %#v", got)
	}
	for _, record := range got.Records {
		if record.PatternID == "foreign" || record.CurrentValue != nil {
			t.Fatalf("unsafe record = %#v", record)
		}
	}
	exact, err := provider.DescribeBaselines(context.Background(), core.BaselineRequest{Service: "api", Signal: "logs", PatternID: "B", Limit: 50, At: time.Now()})
	if err != nil || len(exact.Records) != 1 || exact.Records[0].PatternID != "B" {
		t.Fatalf("exact = %#v, err = %v", exact, err)
	}
}

func TestLogBaselineProviderUsesStoredStatisticsAndReadiness(t *testing.T) {
	catalog, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	seasonal := make([]stats.EWMA, stats.HoursPerDay)
	at := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	seasonal[8] = stats.EWMA{Mean: 9, Variance: 4, Count: 5}
	catalog.patterns["ready"] = &Pattern{ID: "ready", OrgID: "acme", Service: "api", Count: 10, Verdict: "known", BaselineFrequency: 3, BaselineVariance: 16, Seasonal: seasonal, SpikeBaselineMode: baselineModeTimeOfDay, LastSeen: at}
	catalog.patterns["learning"] = &Pattern{ID: "learning", OrgID: "acme", Service: "api", Count: 2, BaselineFrequency: 2, BaselineVariance: -4, LastSeen: at}
	provider := newLogBaselineProvider(catalog, tenancy.NewOrgScope("acme"), 5, storage.NewMemory())
	got, err := provider.DescribeBaselines(context.Background(), core.BaselineRequest{Service: "api", Signal: "logs", Limit: 50, At: at})
	if err != nil {
		t.Fatal(err)
	}
	if got.Availability != core.HealthPartial || len(got.Records) != 2 {
		t.Fatalf("result = %#v", got)
	}
	byID := map[string]core.BaselineRecord{got.Records[0].PatternID: got.Records[0], got.Records[1].PatternID: got.Records[1]}
	if ready := byID["ready"]; !ready.Confident || ready.ExpectedMean != 9 || ready.ExpectedStd != 2 || ready.ObservationCount != 10 || ready.CurrentValue != nil {
		t.Fatalf("ready = %#v", ready)
	}
	if learning := byID["learning"]; learning.Confident || learning.Availability != core.HealthCollecting || learning.ExpectedStd != math.Sqrt(0) || learning.ReasonCode != "baseline_collecting_current_value_unavailable" {
		t.Fatalf("learning = %#v", learning)
	}
}

func TestLogBaselineProviderUsesOneSettingsSnapshotAndDetectorPrecedence(t *testing.T) {
	inner := storage.NewMemory()
	if err := SaveSpikeSettings(inner, SpikeSettings{BaselineMode: baselineModeTimeOfDay}); err != nil {
		t.Fatal(err)
	}
	store := &baselineSettingsCountingStore{Provider: inner}
	catalog, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	seasonal := make([]stats.EWMA, stats.HoursPerDay)
	seasonal[8] = stats.EWMA{Mean: 9, Variance: 4, Count: logMinBucketSamples}
	catalog.patterns["global"] = &Pattern{ID: "global", OrgID: "acme", Service: "api", Count: 10, BaselineFrequency: 3, BaselineVariance: 1, Seasonal: seasonal, LastSeen: at}
	catalog.patterns["whitespace"] = &Pattern{ID: "whitespace", OrgID: "acme", Service: "api", Count: 10, BaselineFrequency: 4, BaselineVariance: 1, Seasonal: seasonal, SpikeBaselineMode: "   ", LastSeen: at}
	provider := newLogBaselineProvider(catalog, tenancy.NewOrgScope("acme"), 5, store)

	got, err := provider.DescribeBaselines(context.Background(), core.BaselineRequest{Service: "api", Signal: "logs", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if store.reads != 1 {
		t.Fatalf("spike settings reads = %d, want 1", store.reads)
	}
	byID := map[string]core.BaselineRecord{got.Records[0].PatternID: got.Records[0], got.Records[1].PatternID: got.Records[1]}
	if byID["global"].ExpectedMean != 9 {
		t.Fatalf("global mode mean = %v, want time-of-day mean 9", byID["global"].ExpectedMean)
	}
	if byID["whitespace"].ExpectedMean != 4 {
		t.Fatalf("whitespace override mean = %v, want default mean 4", byID["whitespace"].ExpectedMean)
	}
}
