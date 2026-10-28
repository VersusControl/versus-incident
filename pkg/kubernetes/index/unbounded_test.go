package index

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestRestoreKeepsFailureAndOwnerEvidenceWithoutNewObservation(t *testing.T) {
	observed := time.Now().UTC().Add(-time.Minute)
	ownerObserved := observed.Add(time.Second)
	snapshot := Snapshot{Kinds: map[string]KindStatus{"Pod": {State: "error", ObservedAt: observed, LastAttemptAt: observed.Add(30 * time.Second), Partial: true, Error: "forbidden", Records: 1}}, Records: []Record{{Kind: "Pod", UID: "pod", Name: "pod", OwnerObservedAt: ownerObserved, Owners: []OwnerRef{{Kind: "Deployment", UID: "owner", Name: "owner"}}}}}
	index, err := Restore(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	got := index.Snapshot()
	if got.Kinds["Pod"].State != "error" || got.Kinds["Pod"].Error != "forbidden" || !got.Kinds["Pod"].ObservedAt.Equal(observed) || !got.Records[0].OwnerObservedAt.Equal(ownerObserved) {
		t.Fatalf("restore invented successful/fresh evidence: %+v", got)
	}
	snapshot.Records[0].Name = ""
	if _, err := Restore(snapshot); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid stored record accepted: %v", err)
	}
}

func TestUnboundedInventoryRegistryKeepsScopeAndRecordValidation(t *testing.T) {
	registry := NewUnboundedRegistry(2)
	index, err := registry.ForScope(Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "epoch"})
	if err != nil {
		t.Fatal(err)
	}
	records := make([]Record, 60001)
	for i := range records {
		records[i] = Record{UID: "pod-" + strconv.Itoa(i), Kind: "Pod", Name: "pod-" + strconv.Itoa(i)}
	}
	if err := index.Ingest("Pod", records, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	records[0].Name = ""
	// A refresh that could not read one record keeps the rest of the inventory
	// and reports the skip; the known copy of the rejected record stays.
	if err := index.Ingest("Pod", records, true, time.Now()); err != nil {
		t.Fatalf("one unreadable record failed the kind: %v", err)
	}
	if status, _ := index.KindStatus("Pod"); status.Skipped != 1 || status.Error != "record_rejected" {
		t.Fatalf("skipped record went unreported: %+v", status)
	}
	if len(index.Snapshot().Records) != 60001 {
		t.Fatal("invalid refresh discarded inventory")
	}
	if _, err := registry.ForScope(Scope{OrgID: "other", ClusterID: "cluster"}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ForScope(Scope{OrgID: "third", ClusterID: "cluster"}); !errors.Is(err, ErrScopeLimit) {
		t.Fatalf("scope admission lost: %v", err)
	}
}

// A published generation keeps a kind the cluster does not serve marked as
// never listed, so a replica that restores it does not report that kind's first
// listing as a wave of creations.
func TestRestoreKeepsTheAbsentBaselineAndToleratesASnapshotWithoutIt(t *testing.T) {
	now := time.Now().UTC()
	index := New(10)
	if err := index.Refresh(context.Background(), "ConfigMap", 0, now, func(context.Context) ([]Record, bool, error) {
		return nil, false, ErrKindNotServed
	}); err != nil {
		t.Fatal(err)
	}
	published, err := json.Marshal(index.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var stored Snapshot
	if err := json.Unmarshal(published, &stored); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(stored)
	if err != nil {
		t.Fatal(err)
	}
	if status := restored.Snapshot().Kinds["ConfigMap"]; !status.Absent || status.State != "ready" || status.Records != 0 {
		t.Fatalf("a restored generation lost the kind's unlisted state: %+v", status)
	}
	deltas, unsubscribe := restored.Subscribe(8)
	defer unsubscribe()
	if err := restored.Ingest("ConfigMap", []Record{{Kind: "ConfigMap", UID: "late", Namespace: "shop", Name: "late-config"}}, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 0 {
		t.Fatal("a restored replica reported the first listing of an unserved kind as a change")
	}

	// A generation written before the mark existed carries no baseline. It must
	// restore, report no absent kind, and keep reporting changes: an old payload
	// cannot know more than it stored.
	var legacy map[string]any
	if err := json.Unmarshal(published, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, kind := range legacy["Kinds"].(map[string]any) {
		delete(kind.(map[string]any), "absent")
	}
	stripped, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var upgraded Snapshot
	if err := json.Unmarshal(stripped, &upgraded); err != nil {
		t.Fatal(err)
	}
	restored, err = Restore(upgraded)
	if err != nil {
		t.Fatalf("an upgraded snapshot without the mark was rejected: %v", err)
	}
	if status := restored.Snapshot().Kinds["ConfigMap"]; status.Absent || status.State != "ready" {
		t.Fatalf("an upgraded snapshot invented an unlisted kind: %+v", status)
	}
	deltas, unsubscribe = restored.Subscribe(8)
	defer unsubscribe()
	if err := restored.Ingest("ConfigMap", []Record{{Kind: "ConfigMap", UID: "late", Namespace: "shop", Name: "late-config"}}, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if delta := receiveDelta(t, deltas); delta.Op != "upsert" || delta.Old != nil || delta.New == nil || delta.New.UID != "late" {
		t.Fatalf("an upgraded replica stopped reporting changes: %#v", delta)
	}
}
