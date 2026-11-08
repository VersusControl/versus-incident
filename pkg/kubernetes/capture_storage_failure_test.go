package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"golang.org/x/time/rate"
)

func TestStorageStageTimeoutIsAStorageFailure(t *testing.T) {
	timeout := fmt.Errorf("journal statement: %w", context.DeadlineExceeded)
	for _, stage := range []string{"payload", "cas", "journal"} {
		err := storageStageFailure(stage, timeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s lost its cause", stage)
		}
		if reason := recoveryReason(err, stage); reason != "storage_unavailable" {
			t.Fatalf("%s timeout reported as %q", stage, reason)
		}
		if class := errorClass(err); class != "storage_unavailable" {
			t.Fatalf("%s timeout classified as %q", stage, class)
		}
	}
	if storageStageFailure("journal", nil) != nil {
		t.Fatal("success became a failure")
	}
}

func TestStorageStageKeepsLeaseCredentialAndCancellationMeaning(t *testing.T) {
	for _, original := range []error{errInventoryLeaseLost, kubechanges.ErrLeaseLost, ErrCredentialUnavailable, ErrUnauthorized, ErrForbidden, context.Canceled} {
		err := storageStageFailure("journal", original)
		var storage *captureStorageError
		if errors.As(err, &storage) || !errors.Is(err, original) {
			t.Fatalf("%v was relabelled as a storage failure", original)
		}
	}
	if reason := recoveryReason(errInventoryLeaseLost, "journal"); reason != "lease_lost" {
		t.Fatalf("lease loss reported as %q", reason)
	}
}

func TestCollectionTimeoutIsStillReportedAsATimeout(t *testing.T) {
	if reason := recoveryReason(context.DeadlineExceeded, "discovery"); reason != "timeout" {
		t.Fatalf("cluster read timeout reported as %q", reason)
	}
}

func TestJournalConflictKeepsItsLeaseMeaning(t *testing.T) {
	err := storageStageFailure("journal", fmt.Errorf("guard: %w", storage.ErrEvidenceConflict))
	var saveFailure *captureStorageError
	if errors.As(err, &saveFailure) || !errors.Is(err, storage.ErrEvidenceConflict) {
		t.Fatal("a journal conflict was relabelled as a storage failure")
	}
	if reason := recoveryReason(err, "journal"); reason != "lease_lost" {
		t.Fatalf("journal conflict reported as %q", reason)
	}
}

func TestRejectedHistorySaveIsNotAnOutage(t *testing.T) {
	for _, cause := range []error{storage.ErrEvidenceLimit, errInvalidCapture, storage.ErrUnsupported} {
		err := storageStageFailure("journal", fmt.Errorf("commit: %w", cause))
		var saveFailure *captureStorageError
		if !errors.As(err, &saveFailure) || !saveFailure.rejected || !errors.Is(err, cause) {
			t.Fatalf("%v is not a rejected save", cause)
		}
		if reason, class := recoveryReason(err, "journal"), errorClass(err); reason != "storage_rejected" || class != "storage_rejected" {
			t.Fatalf("%v reported as %q / %q", cause, reason, class)
		}
	}
	// The store also answers invalid evidence for inconsistent saved state,
	// which repair or retention can clear, so that answer is an outage.
	stored := storageStageFailure("journal", fmt.Errorf("occurrence models: %w", storage.ErrInvalidEvidence))
	var storedFailure *captureStorageError
	if !errors.As(stored, &storedFailure) || storedFailure.rejected || errorClass(stored) != "storage_unavailable" {
		t.Fatal("invalid saved state was treated as a rejected capture")
	}
	timeout := storageStageFailure("journal", context.DeadlineExceeded)
	var saveFailure *captureStorageError
	if !errors.As(timeout, &saveFailure) || saveFailure.rejected || errorClass(timeout) != "storage_unavailable" {
		t.Fatal("a save timeout is an outage, not a rejection")
	}
}

// failingSaveStore lets each cluster's first history save land and then fails
// every later capture of an armed cluster until the test restores it. Blob
// writes are untouched, so the cluster is read and published normally.
type failingSaveStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	failure   error
	readsDown atomic.Bool
	failures  atomic.Int64
	mu        sync.Mutex
	armed     map[string]bool
	first     map[string]string
}

func newFailingSaveStore(failure error, armed ...string) *failingSaveStore {
	base := storage.NewMemory()
	store := &failingSaveStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore), failure: failure, armed: map[string]bool{}, first: map[string]string{}}
	for _, cluster := range armed {
		store.armed[cluster] = true
	}
	return store
}

func (store *failingSaveStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	store.mu.Lock()
	first := store.first[scope.ClusterID]
	failing := store.armed[scope.ClusterID] && first != "" && first != batch.ID
	store.mu.Unlock()
	if failing {
		store.failures.Add(1)
		return store.failure
	}
	err := store.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err == nil {
		store.mu.Lock()
		if store.first[scope.ClusterID] == "" {
			store.first[scope.ClusterID] = batch.ID
		}
		store.mu.Unlock()
	}
	return err
}

func (store *failingSaveStore) QueryEvidence(ctx context.Context, scope storage.EvidenceScope, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	if store.readsDown.Load() {
		return storage.EvidencePage{}, errors.New("history read refused by 10.0.0.7")
	}
	return store.EvidenceStore.QueryEvidence(ctx, scope, query)
}

func (store *failingSaveStore) restore() {
	store.mu.Lock()
	store.armed = map[string]bool{}
	store.mu.Unlock()
}

func saveDelayCluster(t *testing.T) string {
	t.Helper()
	endpoint, _ := saveDelayFixture(t)
	return endpoint
}

func saveDelayFixture(t *testing.T) (string, *fakekube.Server) {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRecorder()
	fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"save-delay-physical"}}}`)))
	if seed.Code != http.StatusOK {
		t.Fatal("identity seed failed")
	}
	upstream := httptest.NewServer(fixture)
	t.Cleanup(upstream.Close)
	return upstream.URL, fixture
}

// useFixturePace raises the background read limiter to a fixture rate and
// shortens collection cadences, so a scenario that waits for the next
// autonomous capture sees it within seconds. What each capture reads and
// commits is unchanged. Call it before capture starts.
func useFixturePace(service *Service) {
	service.client.indexLimiter = rate.NewLimiter(20, 4)
	service.cadenceDivisor = 10
}

// replicaPace selects how fast a replica's background capture runs.
type replicaPace bool

const (
	fixturePace    replicaPace = true
	productionPace replicaPace = false
)

// saveDelayReplica starts one Versus replica capturing the cluster into the
// shared store. Replicas with the same scope compete for the same lease.
func saveDelayReplica(t *testing.T, endpoint string, store storage.Provider, clusterID string, pace replicaPace) *Service {
	t.Helper()
	service, _ := saveDelayReplicaRegistry(t, endpoint, store, clusterID, pace)
	return service
}

func saveDelayReplicaRegistry(t *testing.T, endpoint string, store storage.Provider, clusterID string, pace replicaPace) (*Service, *ServiceRegistry) {
	t.Helper()
	client, err := NewClient(Config{Endpoint: endpoint, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: clusterID, CredentialID: "unchanged"}, time.Hour)
	service.historySaveGrace = 500 * time.Millisecond
	if pace == fixturePace {
		useFixturePace(service)
	}
	service.SetChangeStorage(store)
	registry := NewServiceRegistry(service)
	t.Cleanup(registry.Close)
	registry.StartCapture(t.Context())
	return service, registry
}

func historySaveOf(t *testing.T, service *Service) *HistorySaveStatus {
	t.Helper()
	page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{})
	if err != nil {
		return nil
	}
	return page.Capture.HistorySave
}

// historySaveCleared reports a successful history read with nothing behind.
func historySaveCleared(t *testing.T, service *Service) bool {
	t.Helper()
	page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{})
	return err == nil && page.Capture.HistorySave == nil
}

func TestHistorySaveOutageIsReportedAlikeByEveryReplica(t *testing.T) {
	t.Parallel()
	// A rejected save is never retried, so that case waits for a second
	// capture and then compares both replicas while captures keep arriving.
	// At fixture pace a newly published capture reads as delayed until its own
	// save is rejected, so that case keeps the production interval.
	for _, test := range []struct {
		name, code string
		failure    error
		retryable  bool
		pace       replicaPace
	}{
		{"timeout", "history_save_delayed", fmt.Errorf("journal statement on 10.0.0.7: %w", context.DeadlineExceeded), true, fixturePace},
		{"rejected", "history_save_rejected", fmt.Errorf("batch from 10.0.0.7: %w", storage.ErrEvidenceLimit), false, productionPace},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			endpoint := saveDelayCluster(t)
			observed := newFailingSaveStore(test.failure, "save-delay")
			replicas := []*Service{saveDelayReplica(t, endpoint, observed, "save-delay", test.pace), saveDelayReplica(t, endpoint, observed, "save-delay", test.pace)}

			// The first failure is the capture's own save; the next is the owner's
			// retry of that pending save. Neither may count as a credential failure.
			waitUntil(t, 45*time.Second, "a failed history save seen by every replica", func() bool {
				return observed.failures.Load() > 1 && historySaveOf(t, replicas[0]) != nil && historySaveOf(t, replicas[1]) != nil
			})
			scope := storage.EvidenceScope{OrgID: "default", ClusterID: "save-delay", ClusterUID: "save-delay-physical"}
			checkpoint, err := observed.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || checkpoint.At.IsZero() {
				t.Fatalf("committed checkpoint: %+v %v", checkpoint, err)
			}

			// Only the lease owner saves history, so one replica answers from the
			// stored state alone.
			saving := 0
			for _, service := range replicas {
				service.recovery.mu.Lock()
				if _, ok := service.recovery.stages["journal"]; ok {
					saving++
				}
				service.recovery.mu.Unlock()
			}
			if saving != 1 {
				t.Fatalf("%d replicas saved history, want exactly the owner", saving)
			}

			now := time.Now().UTC()
			for i, service := range replicas {
				// Live pages keep serving the cluster that was read.
				if _, err := service.Overview(t.Context()); err != nil {
					t.Fatalf("replica %d overview during a save outage: %v", i, err)
				}
				if _, err := service.Workloads(t.Context(), WorkloadListOptions{}); err != nil {
					t.Fatalf("replica %d workloads during a save outage: %v", i, err)
				}
				live, err := service.Issues(t.Context(), IssueOptions{})
				if err != nil || live.Capture != nil {
					t.Fatalf("replica %d live issues during a save outage: capture=%v err=%v", i, live.Capture, err)
				}
				service.recovery.mu.Lock()
				credentialFailures := service.recovery.failures
				service.recovery.mu.Unlock()
				if credentialFailures != 0 || service.recovery.requestGate(service.scope.OrgID) {
					t.Fatalf("replica %d: a save outage reached the credential recovery gate (failures=%d)", i, credentialFailures)
				}

				crashes, crashErr := service.Crashes(t.Context(), kubechanges.CrashQuery{})
				changes, changeErr := service.Changes(t.Context(), ChangeQuery{})
				timeline, timelineErr := service.Timeline(t.Context(), TimelineQuery{})
				issues, issueErr := service.Issues(t.Context(), IssueOptions{Since: now.Add(-time.Hour), Until: now})
				for name, err := range map[string]error{"crashes": crashErr, "changes": changeErr, "timeline": timelineErr, "issues": issueErr} {
					if err != nil {
						t.Fatalf("replica %d %s history read failed during a save outage: %s", i, name, DiagnoseError(err).Code)
					}
				}
				for name, capture := range map[string]*CaptureStatus{"crashes": &crashes.Capture, "changes": changes.Capture, "timeline": timeline.Capture, "issues": issues.Capture} {
					if capture == nil || capture.HistorySave == nil {
						t.Fatalf("replica %d %s history page did not report the failed save", i, name)
					}
					save := capture.HistorySave
					if save.Code != test.code || save.Retryable != test.retryable || save.SavedThrough == nil || !save.SavedThrough.Equal(checkpoint.At) {
						t.Fatalf("replica %d %s save status = %+v, want %s retryable=%t saved through %s", i, name, save, test.code, test.retryable, checkpoint.At)
					}
				}
				encoded, err := json.Marshal([]any{crashes, changes, timeline, issues})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(encoded), `"history_save":{"code":"`+test.code+`"`) || !strings.Contains(string(encoded), `"saved_through":"`+checkpoint.At.UTC().Format(time.RFC3339Nano)+`"`) {
					t.Fatalf("replica %d history response shape: %s", i, encoded)
				}
				for _, leaked := range []string{"connector_credential_unresolved", "credential_unavailable", "deadline", "10.0.0.7", "journal statement", "evidence limit"} {
					if strings.Contains(string(encoded), leaked) {
						t.Fatalf("replica %d history response carried %q", i, leaked)
					}
				}
			}

			// A history read that itself fails is a store outage, never a credential one.
			observed.readsDown.Store(true)
			if _, err := replicas[1].Crashes(t.Context(), kubechanges.CrashQuery{}); DiagnoseError(err).Code != "history_store_unavailable" {
				t.Fatalf("failed history read reported as %q", DiagnoseError(err).Code)
			}
			observed.readsDown.Store(false)

			observed.restore()
			waitUntil(t, 45*time.Second, "the next successful history save on every replica", func() bool {
				return historySaveCleared(t, replicas[0]) && historySaveCleared(t, replicas[1])
			})
			latest, err := observed.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || !latest.At.After(checkpoint.At) {
				t.Fatalf("recovered checkpoint did not advance: %s -> %s %v", checkpoint.At, latest.At, err)
			}
		})
	}
}

func TestHistorySaveOutageStaysWithinItsCluster(t *testing.T) {
	t.Parallel()
	endpoint := saveDelayCluster(t)
	observed := newFailingSaveStore(fmt.Errorf("journal statement: %w", context.DeadlineExceeded), "save-delay-a")
	failing := saveDelayReplica(t, endpoint, observed, "save-delay-a", fixturePace)
	healthy := saveDelayReplica(t, endpoint, observed, "save-delay-b", fixturePace)
	waitUntil(t, 45*time.Second, "a failed history save on one cluster", func() bool {
		return observed.failures.Load() > 1 && historySaveOf(t, failing) != nil
	})
	page, err := healthy.Crashes(t.Context(), kubechanges.CrashQuery{})
	if err != nil {
		t.Fatalf("healthy cluster history read: %s", DiagnoseError(err).Code)
	}
	if page.Capture.HistorySave != nil {
		t.Fatalf("another cluster's save outage reached this cluster: %+v", page.Capture.HistorySave)
	}
}

func TestLeaseOutcomesNeverCountAgainstTheCredential(t *testing.T) {
	for _, err := range []error{errInventoryLeaseLost, kubechanges.ErrLeaseLost, fmt.Errorf("guard: %w", storage.ErrEvidenceConflict), storageStageFailure("lease", errors.New("lease read refused")), storageStageFailure("journal", context.DeadlineExceeded)} {
		if countsAgainstCredential(err) {
			t.Fatalf("%v counted against the credential", err)
		}
	}
	for _, err := range []error{ErrUnauthorized, ErrCredentialUnavailable, storageStageFailure("lease", ErrUnauthorized)} {
		if !countsAgainstCredential(err) {
			t.Fatalf("%v no longer counts against the credential", err)
		}
	}
}

// conflictingSaveStore answers every history save after the first with the
// store's lease fence, as when another owner moved the inventory.
type conflictingSaveStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	conflicts atomic.Int64
	saved     atomic.Bool
}

func (store *conflictingSaveStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	if store.saved.Load() {
		store.conflicts.Add(1)
		return storage.ErrEvidenceConflict
	}
	err := store.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err == nil {
		store.saved.Store(true)
	}
	return err
}

func TestLeaseLossNeverReportsAnUnresolvedCredential(t *testing.T) {
	t.Parallel()
	endpoint := saveDelayCluster(t)
	base := storage.NewMemory()
	observed := &conflictingSaveStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore)}
	service, registry := saveDelayReplicaRegistry(t, endpoint, observed, "lease-loss", fixturePace)
	waitUntil(t, 45*time.Second, "repeated lease conflicts on history saves", func() bool {
		return observed.conflicts.Load() > 2
	})
	service.recovery.mu.Lock()
	credentialFailures := service.recovery.failures
	service.recovery.mu.Unlock()
	if credentialFailures != 0 {
		t.Fatalf("a lease loss counted %d credential failures", credentialFailures)
	}
	// A closed credential gate is what readers are told as an unresolved
	// connector credential.
	if _, err := registry.ResolveCluster("default", "lease-loss"); errors.Is(err, ErrCredentialUnavailable) || DiagnoseError(err).Code == "connector_credential_unresolved" {
		t.Fatalf("a lease loss resolved as a credential fault: %v", err)
	}
}

// rejectingSaveStore lets the first history save land and then rejects every
// new capture for good until the test lets saves through again.
type rejectingSaveStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	mu        sync.Mutex
	first     string
	rejecting bool
	attempts  map[string]int
	rejected  []string
	saved     []storage.EvidenceBatch
}

func (store *rejectingSaveStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	store.mu.Lock()
	store.attempts[batch.ID]++
	if store.first != "" && batch.ID != store.first && store.rejecting {
		store.rejected = append(store.rejected, batch.ID)
		store.mu.Unlock()
		return fmt.Errorf("batch from 10.0.0.7: %w", storage.ErrEvidenceLimit)
	}
	store.mu.Unlock()
	err := store.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err == nil {
		store.mu.Lock()
		if store.first == "" {
			store.first = batch.ID
		}
		store.saved = append(store.saved, batch)
		store.mu.Unlock()
	}
	return err
}

func TestRejectedHistorySaveDoesNotStopCollection(t *testing.T) {
	t.Parallel()
	endpoint, fixture := saveDelayFixture(t)
	base := storage.NewMemory()
	observed := &rejectingSaveStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore), rejecting: true, attempts: map[string]int{}}
	service := saveDelayReplica(t, endpoint, observed, "save-rejected", fixturePace)
	waitUntil(t, 45*time.Second, "a rejected history save reported to readers", func() bool {
		save := historySaveOf(t, service)
		return save != nil && save.Code == "history_save_rejected" && !save.Retryable
	})
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "save-rejected", ClusterUID: "save-delay-physical"}
	checkpoint, err := observed.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}

	// Collection and live publishing go on while saves are rejected.
	pod := `{"operation":"upsert","resource":"pods","namespace":"batch","name":"after-rejection","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"after-rejection","namespace":"batch","uid":"after-rejection"},"status":{"phase":"Running"}}}`
	mutation := httptest.NewRecorder()
	fixture.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(pod)))
	if mutation.Code != http.StatusOK {
		t.Fatal("pod mutation failed")
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	waitUntil(t, 75*time.Second, "new cluster data published and its own save attempted while saves are rejected", func() bool {
		observed.mu.Lock()
		distinct := map[string]bool{}
		for _, id := range observed.rejected {
			distinct[id] = true
		}
		observed.mu.Unlock()
		cache.mu.RLock()
		published := false
		for _, record := range cache.generation.View.Records {
			published = published || record.UID == "after-rejection"
		}
		cache.mu.RUnlock()
		return published && len(distinct) >= 2
	})
	page, err := service.List(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Pod"], Namespace: "batch"})
	listed := false
	for _, item := range page.Items {
		listed = listed || item.Name == "after-rejection"
	}
	if err != nil || !listed {
		t.Fatalf("the live page did not show the new pod: %v", err)
	}
	if latest, err := observed.EvidenceCheckpoint(t.Context(), scope); err != nil || latest.ID != checkpoint.ID {
		t.Fatalf("the checkpoint moved past a rejected capture: %s -> %s %v", checkpoint.ID, latest.ID, err)
	}

	// A rejected capture is never retried.
	observed.mu.Lock()
	for _, id := range observed.rejected {
		if observed.attempts[id] != 1 {
			observed.mu.Unlock()
			t.Fatalf("a rejected capture was attempted %d times", observed.attempts[id])
		}
	}
	observed.rejecting = false
	observed.mu.Unlock()
	service.recovery.mu.Lock()
	credentialFailures := service.recovery.failures
	service.recovery.mu.Unlock()
	if credentialFailures != 0 {
		t.Fatalf("a rejected save counted %d credential failures", credentialFailures)
	}

	// The following good capture saves, records the uncovered interval as a
	// gap, and clears the rejection.
	waitUntil(t, 75*time.Second, "the next capture saved after the rejection", func() bool {
		observed.mu.Lock()
		saved := observed.saved[len(observed.saved)-1].ID != observed.first
		observed.mu.Unlock()
		return saved && historySaveCleared(t, service)
	})
	observed.mu.Lock()
	defer observed.mu.Unlock()
	recovered := observed.saved[len(observed.saved)-1]
	gap := false
	for _, record := range recovered.Records {
		gap = gap || record.Type == "coverage_gap" && !record.GapFrom.After(checkpoint.At)
	}
	if recovered.ID == observed.first || !gap {
		t.Fatalf("the capture after a rejection did not record the uncovered interval as a gap (records=%d)", len(recovered.Records))
	}
}

// casTimeoutStore times out every lease swap.
type casTimeoutStore struct {
	storage.Provider
}

func (casTimeoutStore) CompareAndSwapBlob(string, []byte, []byte) (bool, error) {
	return false, fmt.Errorf("lease statement: %w", context.DeadlineExceeded)
}

func TestLeaseClaimAndRenewalKeepCredentialFailures(t *testing.T) {
	invalid := errors.New("kubernetes: credential provider returned an invalid token")
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "lease-credential", CredentialID: "unchanged"})
	service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "", invalid }}
	base := storage.NewMemory()
	_, _, err := service.claimInventory(t.Context(), base, "owner")
	if !errors.Is(err, invalid) || !countsAgainstCredential(err) {
		t.Fatalf("a credential failure at claim did not reach the credential gate: %v (%s)", err, errorClass(err))
	}
	lease := &inventoryLease{provider: base, cas: base.(storage.BlobCAS), scope: service.scope, record: inventoryLeaseRecord{Owner: "owner", Expires: time.Now().Add(time.Minute)}}
	err = lease.renew(t.Context(), service)
	if !errors.Is(err, invalid) || !countsAgainstCredential(err) {
		t.Fatalf("a credential failure at renewal did not reach the credential gate: %v (%s)", err, errorClass(err))
	}
}

func TestLeaseClaimStoreTimeoutIsAStorageFailure(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "lease-timeout", CredentialID: "unchanged"})
	_, _, err := service.claimInventory(t.Context(), casTimeoutStore{Provider: storage.NewMemory()}, "owner")
	if err == nil || countsAgainstCredential(err) || errorClass(err) != "storage_unavailable" {
		t.Fatalf("a lease swap timeout reached the credential gate: %v (%s)", err, errorClass(err))
	}
}

// checkpointOutageStore fails checkpoint reads while armed, so a capture is
// published without being prepared for saving.
type checkpointOutageStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	down atomic.Bool
}

func (store *checkpointOutageStore) EvidenceCheckpoint(ctx context.Context, scope storage.EvidenceScope) (storage.EvidenceCheckpoint, error) {
	if store.down.Load() {
		return storage.EvidenceCheckpoint{}, fmt.Errorf("checkpoint statement on 10.0.0.7: %w", context.DeadlineExceeded)
	}
	return store.EvidenceStore.EvidenceCheckpoint(ctx, scope)
}

func TestUnpreparedHistorySaveIsReportedAlikeByEveryReplica(t *testing.T) {
	t.Parallel()
	endpoint := saveDelayCluster(t)
	base := storage.NewMemory()
	observed := &checkpointOutageStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore)}
	// Production pacing: the expected saved-through time is read before the
	// checkpoint outage starts, and a faster capture could save again in between.
	replicas := []*Service{saveDelayReplica(t, endpoint, observed, "save-unprepared", productionPace), saveDelayReplica(t, endpoint, observed, "save-unprepared", productionPace)}
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "save-unprepared", ClusterUID: "save-delay-physical"}
	waitUntil(t, 45*time.Second, "a complete first capture saved to history", func() bool {
		health, err := replicas[0].SyncHealth(t.Context())
		checkpoint, checkpointErr := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), scope)
		return err == nil && health.State == "ready" && health.PendingKinds == 0 && checkpointErr == nil && checkpoint.ID != "" && historySaveCleared(t, replicas[0])
	})
	checkpoint, err := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	unprepared := func(service *Service) bool {
		service.changes.mu.Lock()
		cache := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		if cache == nil {
			return false
		}
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		return cache.generation.EvidenceBatch == nil && cache.generation.EvidenceFailure == "storage_unavailable"
	}
	observed.down.Store(true)
	waitUntil(t, 75*time.Second, "a capture published on every replica without a save", func() bool {
		return unprepared(replicas[0]) && unprepared(replicas[1])
	})
	observed.down.Store(false)
	for i, service := range replicas {
		page, err := service.Crashes(t.Context(), kubechanges.CrashQuery{})
		if err != nil {
			t.Fatalf("replica %d history read: %s", i, DiagnoseError(err).Code)
		}
		save := page.Capture.HistorySave
		if save == nil || save.Code != "history_save_delayed" || !save.Retryable || save.SavedThrough == nil || !save.SavedThrough.Equal(checkpoint.At) {
			t.Fatalf("replica %d save status = %+v, want delayed through %s", i, save, checkpoint.At)
		}
	}
}

// proofFailingSource resolves a token but cannot prove whose it is.
type proofFailingSource struct{ *credentialSource }

func (proofFailingSource) verifiedCredentialPrincipal(context.Context, string) (string, error) {
	return "", errors.New("principal proof endpoint refused the token")
}

func TestEveryCredentialFailureClosesTheGate(t *testing.T) {
	resolved := func(context.Context) (string, error) { return "Bearer synthetic", nil }
	bearer := func(refresh func(context.Context) (string, time.Time, error)) CredentialSource {
		return &expiringBearerSource{refresh: refresh, now: time.Now}
	}
	missing := filepath.Join(t.TempDir(), "missing-token")
	for _, test := range []struct {
		name        string
		credentials CredentialSource
		review      bool
	}{
		{"resolve", &credentialSource{authorization: func(context.Context) (string, error) { return "", errors.New("secret store refused the read") }}, false},
		{"refresh", bearer(func(context.Context) (string, time.Time, error) {
			return "", time.Time{}, errors.New("token exchange refused")
		}), false},
		{"refresh timeout", bearer(func(context.Context) (string, time.Time, error) {
			return "", time.Time{}, fmt.Errorf("token exchange: %w", context.DeadlineExceeded)
		}), false},
		{"invalid token", bearer(func(context.Context) (string, time.Time, error) {
			return "synthetic", time.Now().Add(-time.Minute), nil
		}), false},
		{"unreadable token file", &tokenFileSource{path: missing}, false},
		{"principal proof", proofFailingSource{&credentialSource{authorization: resolved}}, false},
		{"identity review", &credentialSource{authorization: resolved}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "credential-" + strings.ReplaceAll(test.name, " ", "-"), CredentialID: "unchanged"})
			service.client.credentials = test.credentials
			if test.review {
				// A proved principal cannot be replaced by an unreviewed one.
				service.identity.reviewPrincipal = "self-subject-review:previous"
			}
			err := service.commitStorage(t.Context(), "journal", func() error { return nil })
			if err == nil || !countsAgainstCredential(err) {
				t.Fatalf("%s failure did not close the credential gate: %v", test.name, err)
			}
		})
	}
	for _, err := range []error{ErrCredentialUnavailable, ErrUnauthorized, fmt.Errorf("list pods: %w", ErrForbidden), commitConflictError{cause: ErrCredentialUnavailable}} {
		if !countsAgainstCredential(err) {
			t.Fatalf("credential answer %v did not close the gate", err)
		}
	}
}

func TestOnlyCredentialFailuresCloseTheGate(t *testing.T) {
	for name, err := range map[string]error{
		"storage":        storageStageFailure("journal", errors.New("connection reset")),
		"rejected save":  storageStageFailure("journal", storage.ErrEvidenceLimit),
		"inventory read": inventoryReadFailure(errors.New("blob read failed")),
		"lease lost":     errInventoryLeaseLost,
		"lease fence":    fmt.Errorf("guard: %w", storage.ErrEvidenceConflict),
		"journal lease":  kubechanges.ErrLeaseLost,
		"bare deadline":  context.DeadlineExceeded,
		"cancellation":   context.Canceled,
		"unknown":        errors.New("something else failed"),
		"cluster down":   ErrClusterUnavailable,
	} {
		if countsAgainstCredential(err) {
			t.Fatalf("%s closed the credential gate", name)
		}
	}
}

func TestContendedCredentialLockPastTheSaveDeadlineKeepsTheGateOpen(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "contended", CredentialID: "unchanged"})
	service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer synthetic", nil }}
	service.identity.mu.Lock()
	release := time.AfterFunc(300*time.Millisecond, service.identity.mu.Unlock)
	defer release.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	saved := false
	err := service.commitStorage(ctx, "journal", func() error { saved = true; return nil })
	if saved || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a save past its deadline ran or failed differently: saved=%t %v", saved, err)
	}
	if countsAgainstCredential(err) || errors.Is(err, ErrCredentialUnavailable) || DiagnoseError(err).Code == "connector_credential_unresolved" {
		t.Fatalf("a contended credential lock was reported as a credential fault: %v", err)
	}
}
