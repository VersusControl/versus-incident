package kubernetes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestFailedHistorySavesHalveTheNextSavesChangeLimit(t *testing.T) {
	service := alertTestService(t)
	cache := &inventoryCache{}
	cache.historySave.ctx = t.Context()
	if limit := historySaveChangeLimit(cache); limit != storage.DefaultEvidenceChangeLimit {
		t.Fatalf("limit %d before any failure, want the store default %d", limit, storage.DefaultEvidenceChangeLimit)
	}
	slow := errors.New("history store timed out")
	generation := inventoryGeneration{EvidenceBatch: &storage.EvidenceBatch{ID: "capture:a"}}
	want := storage.DefaultEvidenceChangeLimit
	for range 3 {
		service.finishHistorySave(cache, generation, slow)
		want /= 2
		if limit := historySaveChangeLimit(cache); limit != want {
			t.Fatalf("limit %d after %d failed saves, want %d", limit, cache.historySave.failures, want)
		}
	}
	for range 10 {
		service.finishHistorySave(cache, generation, slow)
	}
	if limit := historySaveChangeLimit(cache); limit != minHistorySaveChanges {
		t.Fatalf("limit %d after many failed saves, want the floor %d", limit, minHistorySaveChanges)
	}
	service.finishHistorySave(cache, generation, nil)
	if limit := historySaveChangeLimit(cache); limit != storage.DefaultEvidenceChangeLimit {
		t.Fatalf("limit %d after a saved capture, want the store default again", limit)
	}
}

// A root whose evidence is partial keeps only its own issues open: the
// capture still records its coverage, so every other issue can resolve.
func TestPartialRootHoldsOnlyItsOwnIssuesOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	started := time.Now().UTC().Add(-time.Minute)
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: started, CompletedAt: started.Add(time.Second)}
	}
	crashing := func(name, root string, summary map[string]string) []kubeindex.Record {
		return []kubeindex.Record{
			{Kind: "Deployment", Namespace: "shop", Name: name, UID: root, CreatedAt: started.Add(-time.Hour)},
			{Kind: "ReplicaSet", Namespace: "shop", Name: name + "-revision", UID: root + "-revision", Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: root, Controller: true}}},
			{Kind: "Pod", Namespace: "shop", Name: name + "-0", UID: root + "-pod", CreatedAt: started.Add(-time.Hour), Summary: summary, Owners: []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: root + "-revision", Controller: true}}, Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2, TerminatedAt: started.Add(-time.Second)}}},
		}
	}
	records := []kubeindex.Record{{Kind: "Namespace", Name: "kube-system", UID: "physical"}}
	records = append(records, crashing("api", "api-root", nil)...)
	records = append(records, crashing("web", "web-root", map[string]string{"references_partial": "true"})...)
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: records}}
	if err := service.prepareEvidence(t.Context(), &generation, inventoryGeneration{}); err != nil {
		t.Fatal(err)
	}
	batch := generation.EvidenceBatch
	if batch.CoveredThrough.IsZero() {
		t.Fatal("one partial root withheld the coverage of the whole cluster")
	}
	if !slices.Contains(batch.HoldOpen, "web-root") || slices.Contains(batch.HoldOpen, "api-root") {
		t.Fatalf("roots held open %v, want only the partial web root", batch.HoldOpen)
	}
	if batch.CaptureInterval != historyCaptureInterval() || batch.CaptureInterval <= 0 {
		t.Fatalf("capture interval %s, want the capture cadence %s", batch.CaptureInterval, historyCaptureInterval())
	}
}

// Issue alerts come from the live break list. Resolving issues in saved
// history, catch-up resolutions included, sends none.
func TestHistoryResolutionsSendNoIssueAlert(t *testing.T) {
	service, alerts := issueAlertCaptureService(t)
	service.changes.evidenceEnabled = true
	store := service.evidenceStore()
	if store == nil {
		t.Fatal("the test service has no evidence store")
	}
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	start := time.Now().UTC().Add(-time.Hour)
	issue := storage.EvidenceIssue{Key: "api", Kind: "Deployment", UID: "api-root", Rule: "pod.crashloop", Namespace: "shop", AuthorizationKind: "Pod", Broken: true, LastSeen: start, Payload: []byte(`{"severity":"critical"}`)}
	if err := store.CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "c0", At: start, Epoch: "e", Issues: []storage.EvidenceIssue{issue}}); err != nil {
		t.Fatal(err)
	}
	at := start.Add(30 * time.Minute)
	if err := store.CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "c1", Expected: "c0", At: at, Epoch: "e", CoveredThrough: at, CaptureInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	service.notifyEvidenceCommit()
	runIssueAlertCaptureCycle(t, service, kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{}})
	select {
	case alert := <-alerts:
		t.Fatalf("a resolution in saved history sent an alert: %+v", alert)
	default:
	}
}

// The capture a cycle publishes after failed saves carries the halved limit
// into the batch the store is given.
func TestPublishedCaptureCarriesTheHalvedChangeLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	lease, _, err := service.claimInventory(t.Context(), provider, "owner")
	if err != nil {
		t.Fatal(err)
	}
	cache := &inventoryCache{}
	cache.historySave.failures = 2
	service.changes.mu.Lock()
	service.changes.inventories[service.indexScope()] = cache
	service.changes.mu.Unlock()
	started := time.Now().UTC().Add(-time.Minute)
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: started, CompletedAt: started.Add(time.Second)}
	}
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{
		{Kind: "Namespace", Name: "kube-system", UID: "physical"},
		{Kind: "Deployment", Namespace: "shop", Name: "api", UID: "api-root", CreatedAt: started.Add(-time.Hour)},
	}}}
	if err = lease.publish(t.Context(), service, &generation); err != nil {
		t.Fatal(err)
	}
	if generation.EvidenceBatch == nil {
		t.Fatal("the published cycle prepared no capture")
	}
	if want := storage.DefaultEvidenceChangeLimit / 4; generation.EvidenceBatch.ChangeLimit != want {
		t.Fatalf("the capture after 2 failed saves carries limit %d, want %d", generation.EvidenceBatch.ChangeLimit, want)
	}
}
