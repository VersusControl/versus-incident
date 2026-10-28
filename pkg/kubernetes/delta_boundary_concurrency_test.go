package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestTimelineRealCursorExpiresAndCanRestart(t *testing.T) {
	service, provider, query := timelineAdmissionFixture(t)
	first, err := service.Timeline(t.Context(), query)
	if err != nil || first.Next == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	var cursor timelineCursor
	raw, _ := base64.RawURLEncoding.DecodeString(first.Next)
	if err := json.Unmarshal(raw, &cursor); err != nil {
		t.Fatal(err)
	}
	service.changes.mu.Lock()
	snapshot := service.changes.timelines[cursor.ID]
	snapshot.Expires = time.Now().Add(-time.Second)
	service.changes.mu.Unlock()
	raw, _ = json.Marshal(snapshot)
	if err := provider.WriteBlob(service.timelineKey(cursor.Slot)+"/"+cursor.ID, raw); err != nil {
		t.Fatal(err)
	}
	raw, _ = provider.ReadBlob(service.timelineKey(0) + "/index")
	var manifest timelineManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Slots[cursor.Slot].Expires = snapshot.Expires
	raw, _ = json.Marshal(manifest)
	if err := provider.WriteBlob(service.timelineKey(0)+"/index", raw); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.Next
	if _, err := service.Timeline(t.Context(), query); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("expired cursor accepted: %v", err)
	}
	query.Cursor = ""
	restarted, err := service.Timeline(t.Context(), query)
	if err != nil || restarted.Next == "" || restarted.Next == first.Next {
		t.Fatalf("restart=%+v %v", restarted, err)
	}
}

type inventoryGCBarrier struct {
	storage.Provider
	key              string
	write            bool
	armed            atomic.Bool
	started, release chan struct{}
}

func (barrier *inventoryGCBarrier) wait(key string, write bool) {
	if key == barrier.key && write == barrier.write && barrier.armed.CompareAndSwap(true, false) {
		close(barrier.started)
		<-barrier.release
	}
}
func (barrier *inventoryGCBarrier) ReadBlob(key string) ([]byte, error) {
	barrier.wait(key, false)
	return barrier.Provider.ReadBlob(key)
}
func (barrier *inventoryGCBarrier) WriteBlob(key string, value []byte) error {
	barrier.wait(key, true)
	return barrier.Provider.WriteBlob(key, value)
}
func (barrier *inventoryGCBarrier) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	return barrier.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func expireInventoryLease(t *testing.T, provider storage.Provider, scope Scope) {
	t.Helper()
	raw, err := provider.ReadBlob(inventoryKey(scope))
	if err != nil {
		t.Fatal(err)
	}
	var record inventoryLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.Expires = time.Now().Add(-time.Second)
	raw, _ = json.Marshal(record)
	if err := provider.WriteBlob(inventoryKey(scope), raw); err != nil {
		t.Fatal(err)
	}
}
func boundaryGeneration(name string) inventoryGeneration {
	return inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now(), Records: 1}}, Records: []kubeindex.Record{{Kind: "Pod", Name: name, UID: name, Namespace: "shop"}}}}
}

func TestInventoryOwnerGCRetriesConcurrentReaderAndCleansLateWriter(t *testing.T) {
	for _, lateWrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(lateWrite), func(t *testing.T) {
			base := storage.NewMemory()
			barrier := &inventoryGCBarrier{Provider: base, write: lateWrite, started: make(chan struct{}), release: make(chan struct{})}
			service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
			first, _, err := service.claimInventory(t.Context(), barrier, "first")
			if err != nil {
				t.Fatal(err)
			}
			old := boundaryGeneration("old")
			if err := first.publish(t.Context(), service, &old); err != nil {
				t.Fatal(err)
			}
			record := first.record
			if lateWrite {
				record.Slot = 1 - record.Slot
			}
			barrier.key, _ = inventoryDataKey(service.ActiveScope(), record)
			barrier.armed.Store(true)
			result := make(chan error, 1)
			var read inventoryGeneration
			go func() {
				if lateWrite {
					stale := boundaryGeneration("stale")
					result <- first.publish(t.Context(), service, &stale)
				} else {
					var err error
					read, _, err = readInventoryGeneration(barrier, service.ActiveScope(), "")
					result <- err
				}
			}()
			select {
			case <-barrier.started:
			case <-time.After(time.Second):
				t.Fatal("barrier not reached")
			}
			expireInventoryLease(t, base, service.ActiveScope())
			replica := NewService(nil, service.ActiveScope(), 0)
			second, owned, err := replica.claimInventory(t.Context(), barrier, "second")
			if err != nil || !owned {
				t.Fatalf("claim=%v %v", owned, err)
			}
			fresh := boundaryGeneration("fresh")
			if err := second.publish(t.Context(), replica, &fresh); err != nil {
				t.Fatal(err)
			}
			close(barrier.release)
			select {
			case err := <-result:
				if lateWrite && err == nil {
					t.Fatal("late stale owner published")
				}
				if !lateWrite && (err != nil || len(read.View.Records) != 1 || read.View.Records[0].UID != "fresh") {
					t.Fatalf("reader lost last-good after GC: %+v %v", read, err)
				}
			case <-time.After(time.Second):
				t.Fatal("concurrent operation did not finish")
			}
			orphans, err := base.ListBlobs(inventoryKey(service.ActiveScope()) + "/data/first/")
			if err != nil || len(orphans) != 0 {
				t.Fatalf("retired writer resurrected slots: %d %v", len(orphans), err)
			}
		})
	}
}

func TestInventoryFailedSuccessorPublishRetainsLastGood(t *testing.T) {
	provider := storage.NewMemory()
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	first, _, _ := service.claimInventory(t.Context(), provider, "first")
	generation := boundaryGeneration("last-good")
	if err := first.publish(t.Context(), service, &generation); err != nil {
		t.Fatal(err)
	}
	expireInventoryLease(t, provider, service.ActiveScope())
	second, _, _ := service.claimInventory(t.Context(), provider, "second")
	second.expected = []byte("not-current")
	replacement := boundaryGeneration("replacement")
	if err := second.publish(t.Context(), service, &replacement); err == nil {
		t.Fatal("invalid fence published")
	}
	got, _, err := readInventoryGeneration(provider, service.ActiveScope(), "")
	if err != nil || len(got.View.Records) != 1 || got.View.Records[0].UID != "last-good" {
		t.Fatalf("failed publication GC discarded last-good: %+v %v", got, err)
	}
}

func TestNamespaceCandidateFloodHasOneSharedProbeBudgetAndValidScopesSurvive(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/shop/pods") {
			writeJSON(w, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "pod", "namespace": "shop"}}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	provider := storage.NewMemory()
	first := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	second := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	first.SetChangeStorage(provider)
	second.SetChangeStorage(provider)
	for i := 0; i < 256; i++ {
		first.queueInventoryNamespace("missing-" + strconv.Itoa(i))
		second.queueInventoryNamespace("other-" + strconv.Itoa(i))
	}
	key := inventoryKey(first.ActiveScope()) + "/namespace-candidates"
	raw, _ := provider.ReadBlob(key)
	var manifest namespaceCandidateManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) > 2 {
		t.Fatalf("viewers multiplied admission: %d", len(manifest.Entries))
	}
	now := time.Now().UTC()
	gen := inventoryGeneration{}
	if _, err := first.discoverNamespaceCandidates(t.Context(), first.namespaceCache(), &gen, now); err != nil {
		t.Fatal(err)
	}
	if _, err := second.discoverNamespaceCandidates(t.Context(), second.namespaceCache(), &gen, now); err != nil {
		t.Fatal(err)
	}
	if reads.Load() > 4 {
		t.Fatalf("viewers multiplied probes: %d", reads.Load())
	}
	raw, _ = provider.ReadBlob(key)
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	rejected := ""
	for name, candidate := range manifest.Entries {
		if candidate.State == "rejected" {
			rejected = name
		}
		candidate.Expires = now.Add(-time.Second)
		manifest.Entries[name] = candidate
	}
	if rejected == "" {
		t.Fatal("missing namespace was not rejected")
	}
	page, err := first.Namespaces(t.Context(), rejected)
	if err != nil || page.Sync.State == "syncing" {
		t.Fatalf("rejected hint remained loading: %+v %v", page, err)
	}
	manifest.AdmissionWindow, manifest.ProbeWindow = now.Add(-time.Minute), now.Add(-time.Minute)
	raw, _ = json.Marshal(manifest)
	if err := provider.WriteBlob(key, raw); err != nil {
		t.Fatal(err)
	}
	if status := first.queueInventoryNamespace("shop"); status != "namespace_hint_pending" {
		t.Fatalf("valid scope could not enter candidate queue: %s", status)
	}
	if _, err := first.discoverNamespaceCandidates(context.Background(), first.namespaceCache(), &gen, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !namespaceObserved(gen, "shop", time.Now()) {
		t.Fatal("legitimate namespace-only scope was not verified")
	}
	before := reads.Load()
	if _, err := first.discoverNamespaceCandidates(t.Context(), first.namespaceCache(), &gen, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != before {
		t.Fatal("expired missing names were probed perpetually")
	}
}
