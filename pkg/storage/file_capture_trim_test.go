package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// captureOfSize is a capture of count change records, newest at index 0, each
// with a payload of about payloadBytes.
func captureOfSize(id string, at time.Time, count, payloadBytes int) EvidenceBatch {
	batch := EvidenceBatch{ID: id, At: at, CoveredThrough: at, Epoch: "file", Records: make([]EvidenceRecord, count)}
	for i := range batch.Records {
		namespace := fmt.Sprintf("team-%02d", i%20)
		name := fmt.Sprintf("api-%05d", i)
		payload, _ := json.Marshal(map[string]any{"kind": "Pod", "namespace": namespace, "name": name, "type": "created", "note": strings.Repeat("n", payloadBytes)})
		batch.Records[i] = EvidenceRecord{ID: fmt.Sprintf("change:%s-%05d", id, i), Type: "change", At: at.Add(-time.Duration(i) * time.Millisecond), ObservedAt: at, Namespace: namespace, Name: name, Kind: "Pod",
			AuthorizationKind: "Pod", UID: fmt.Sprintf("%s-%05d", id, i), RootUID: "rs-" + namespace, Rule: "created", Epoch: "file", Priority: 1, Payload: payload}
	}
	return batch
}

// A capture larger than the file store keeps is saved as its newest records,
// within the record limit or the byte limit, whichever binds first; every
// older record is gone, and the cut reads as pruned history, not as a gap.
// The second save onto a stored capture keeps the newest records again.
func TestFileStoreKeepsTheNewestRecordsOfAnOversizedCaptureWithoutAGap(t *testing.T) {
	for _, check := range []struct {
		name         string
		count, bytes int
	}{
		{"record limit", 5600, 40},
		{"byte limit", 1500, 3000},
	} {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			store := provider.(EvidenceStore)
			policy := store.EvidencePolicy()
			origin := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			scope := EvidenceScope{OrgID: "default", ClusterID: "file-trim", ClusterUID: "physical"}
			expected := ""
			for round := range 2 {
				batch := captureOfSize(fmt.Sprintf("capture-%d", round), origin.Add(time.Duration(round)*time.Minute), check.count, check.bytes)
				total := 0
				for _, record := range batch.Records {
					total += len(record.Payload)
				}
				if len(batch.Records) <= policy.MaxRecords && total <= policy.MaxBytes {
					t.Fatalf("the capture (%d records, %d bytes) fits the store; it must not", len(batch.Records), total)
				}
				batch.Expected = expected
				if err := store.CommitEvidence(context.Background(), scope, batch); err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
				expected = batch.ID
				captureKeepsItsNewestRecords(t, store, scope, batch, origin)
			}
		})
	}
}
