package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// inProcessSaveBudget is the most heap one save of a capture may need on top
// of what the process holds before it.
const inProcessSaveBudget = 128 << 20

// captureHeapInUse is the heap held by live and not yet collected objects.
func captureHeapInUse() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// captureTransientPeak runs step and answers the most heap it held above the
// heap in use when it started, garbage not yet collected included, at the
// default collection pace. The heap is sampled continuously without stopping
// the world.
func captureTransientPeak(step func()) uint64 {
	runtime.GC()
	start := captureHeapInUse()
	var peak atomic.Uint64
	peak.Store(start)
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			if current := captureHeapInUse(); current > peak.Load() {
				peak.Store(current)
			}
			select {
			case <-stop:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	step()
	if current := captureHeapInUse(); current > peak.Load() {
		peak.Store(current)
	}
	close(stop)
	sampler.Wait()
	return peak.Load() - start
}

// captureAtTheLimit is a capture of exactly the record limit, with change
// payloads shaped like a replaced Pod's (about 600 bytes each), so it also
// sits just under the payload byte limit.
func captureAtTheLimit(id string, at time.Time) EvidenceBatch {
	batch := EvidenceBatch{ID: id, At: at, CoveredThrough: at, Epoch: "memory", Records: make([]EvidenceRecord, MaxEvidenceBatchRecords)}
	for i := range batch.Records {
		namespace := fmt.Sprintf("team-%02d", i%60)
		name := fmt.Sprintf("checkout-api-7d9f8b6c5-%05d", i)
		payload, _ := json.Marshal(map[string]any{
			"cluster": "memory-cluster", "kind": "Pod", "namespace": namespace, "name": name, "type": "created", "at": at,
			"uid": fmt.Sprintf("%s-%05d", id, i), "node": fmt.Sprintf("ip-10-0-%d-%d.ec2.internal", i%250, i%7),
			"image": "registry.example.com/shop/checkout-api:2026.11.06-1a2b3c4d", "phase": "Running",
			"labels": map[string]string{"app.kubernetes.io/name": "checkout-api", "app.kubernetes.io/instance": "checkout", "pod-template-hash": "7d9f8b6c5", "team": namespace},
			"note":   strings.Repeat("n", 150),
		})
		batch.Records[i] = EvidenceRecord{ID: fmt.Sprintf("change:%s-%05d", id, i), Type: "change", At: at.Add(-time.Duration(i) * time.Millisecond), ObservedAt: at, Namespace: namespace, Name: name, Kind: "Pod",
			AuthorizationKind: "Pod", UID: fmt.Sprintf("%s-%05d", id, i), RootUID: "rs-" + namespace, Rule: "created", Epoch: "memory", Priority: 1, Payload: payload}
	}
	return batch
}

// The file store saves a whole capture as one document. This measures the
// heap one save at the record and byte limits needs, first onto an empty store
// and then onto a store that already holds a capture, and keeps it within the
// save budget. The memory store keeps every record it is given and is known to
// exceed this budget at the capture limit (about 190 MiB onto an empty store
// and 440 MiB onto a stored capture when measured); clusters that large run on
// Postgres, so it is not measured here.
func TestFileSaveAtTheCaptureLimitFitsTheMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("saves captures at the record limit")
	}
	if raceDetectorEnabled {
		t.Skip("heap figures are not representative under the race detector")
	}
	origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "default", ClusterID: "capture-memory-file", ClusterUID: "physical"}
	expected := ""
	for round, name := range []string{"onto an empty store", "onto a stored capture"} {
		batch := captureAtTheLimit(fmt.Sprintf("capture-%d", round), origin.Add(time.Duration(round)*time.Minute))
		batch.Expected = expected
		payload := 0
		for _, record := range batch.Records {
			payload += len(record.Payload)
		}
		if payload > MaxEvidenceBatchBytes {
			t.Fatalf("the capture holds %d payload bytes, over the limit", payload)
		}
		var err error
		peak := captureTransientPeak(func() { err = store.CommitEvidence(context.Background(), scope, batch) })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		expected = batch.ID
		t.Logf("file save %s: %d records, %.1f MiB payload, peak %.1f MiB above the heap before it", name, len(batch.Records), float64(payload)/(1<<20), float64(peak)/(1<<20))
		if peak > inProcessSaveBudget {
			t.Errorf("file save %s needed %.1f MiB of transient heap, over the %d MiB budget", name, float64(peak)/(1<<20), inProcessSaveBudget>>20)
		}
		captureKeepsItsNewestRecords(t, store, scope, batch, origin)
	}
}

// captureKeepsItsNewestRecords checks that the store holds the newest records
// of the capture it just saved, within its limits, that every older record is
// gone, and that the cut is accounted as pruned history rather than a gap.
func captureKeepsItsNewestRecords(t *testing.T, store EvidenceStore, scope EvidenceScope, batch EvidenceBatch, origin time.Time) {
	t.Helper()
	policy := store.EvidencePolicy()
	query := EvidenceQuery{Type: "change", Since: origin.Add(-time.Hour), Until: time.Now().UTC(), Limit: 500, WithGaps: true}
	kept := map[string]bool{}
	for {
		page, err := store.QueryEvidence(context.Background(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Type == "coverage_gap" {
				t.Fatalf("the trimmed capture reads as a coverage gap: %+v", item)
			}
			kept[item.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if len(kept) == 0 || len(kept) > policy.MaxRecords {
		t.Fatalf("the store holds %d records, want some and at most %d", len(kept), policy.MaxRecords)
	}
	// The capture's records are newest at index 0.
	for i, record := range batch.Records {
		if kept[record.ID] != (i < len(kept)) {
			t.Fatalf("record %d (%s) kept=%t, but the store holds the newest %d", i, record.ID, kept[record.ID], len(kept))
		}
	}
	checkpoint, err := store.EvidenceCheckpoint(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the store holds the newest %d records, pruned through %s", len(kept), checkpoint.PrunedThrough.Format(time.RFC3339Nano))
	if want := batch.Records[len(kept)].At; checkpoint.PrunedThrough.Before(want) {
		t.Fatalf("history is pruned through %s, want at least the newest record cut, %s", checkpoint.PrunedThrough, want)
	}
}

// A cluster whose captures are larger than the file store keeps is
// warned about once, naming the cluster and Postgres; the same cluster is not
// warned about again within the throttle window, and is after it.
func TestTrimmedCaptureWarnsOncePerClusterAndWindow(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	throttle := logging.NewThrottle(10*time.Minute, func() time.Time { return now })
	policy := EvidencePolicy{Mode: "bounded_file", MaxRecords: 5000, MaxBytes: 4 << 20}
	prod := EvidenceScope{OrgID: "default", ClusterID: "prod", ClusterUID: "physical"}
	if !warnCaptureTrimmed(throttle, prod, policy, 25000, 20000) {
		t.Fatal("the first trimmed capture was not reported")
	}
	now = now.Add(5 * time.Minute)
	if warnCaptureTrimmed(throttle, prod, policy, 25000, 20000) {
		t.Fatal("a second trimmed capture within the window was reported again")
	}
	if !warnCaptureTrimmed(throttle, EvidenceScope{OrgID: "default", ClusterID: "staging", ClusterUID: "physical"}, policy, 9000, 4000) {
		t.Fatal("another cluster's trimmed capture was not reported")
	}
	now = now.Add(6 * time.Minute)
	if !warnCaptureTrimmed(throttle, prod, policy, 25000, 20000) {
		t.Fatal("a trimmed capture after the window was not reported")
	}
}

// A capture within the store's limits is saved as offered; one over them
// keeps its newest records within both the record and the byte limit, and
// never trims issues.
func TestCaptureTrimKeepsTheNewestRecordsWithinTheLimits(t *testing.T) {
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	batch := EvidenceBatch{ID: "c", At: at, Issues: []EvidenceIssue{{Key: "issue"}}}
	for i := 0; i < 10; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("r%d", i), Type: "change", At: at.Add(-time.Duration(i) * time.Second), Payload: json.RawMessage(`{"v":"` + strings.Repeat("x", 90) + `"}`)})
	}
	within := batch
	if dropped, _ := trimCaptureToPolicy(&within, EvidencePolicy{MaxRecords: 10, MaxBytes: 10 * 100}); dropped != 0 || len(within.Records) != 10 {
		t.Fatalf("a capture within the limits lost %d records", dropped)
	}
	for _, check := range []struct {
		name   string
		policy EvidencePolicy
		keep   int
	}{
		{"records", EvidencePolicy{MaxRecords: 4}, 4},
		{"bytes", EvidencePolicy{MaxRecords: 100, MaxBytes: 350}, 3},
	} {
		trimmed := batch
		trimmed.Records = append([]EvidenceRecord(nil), batch.Records...)
		dropped, newest := trimCaptureToPolicy(&trimmed, check.policy)
		if dropped != 10-check.keep || len(trimmed.Records) != check.keep || len(trimmed.Issues) != 1 {
			t.Fatalf("%s: kept %d records and %d issues, dropped %d; want the newest %d and the issue", check.name, len(trimmed.Records), len(trimmed.Issues), dropped, check.keep)
		}
		for i, record := range trimmed.Records {
			if record.ID != fmt.Sprintf("r%d", i) {
				t.Fatalf("%s: kept %s at %d, want the newest records in capture order", check.name, record.ID, i)
			}
		}
		if newest.ID != fmt.Sprintf("r%d", check.keep) {
			t.Fatalf("%s: newest record cut is %s, want r%d", check.name, newest.ID, check.keep)
		}
	}
}
