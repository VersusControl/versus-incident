package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestCaptureCadenceDivisorDefaultsToProductionPace(t *testing.T) {
	scope := Scope{OrgID: "default", ClusterID: "cluster"}
	for name, service := range map[string]*Service{
		"no option":  NewService(nil, scope, 0),
		"divisor 1":  NewService(nil, scope, 0, WithCaptureCadenceDivisor(1)),
		"divisor 0":  NewService(nil, scope, 0, WithCaptureCadenceDivisor(0)),
		"nil option": NewService(nil, scope, 0, nil),
	} {
		for kind, cadence := range captureCadences {
			if got := service.collectionCadence(cadence); got != cadence {
				t.Fatalf("%s: %s cadence %s, want production %s", name, kind, got, cadence)
			}
		}
		if got := service.collectionCadence(inventoryCadence("Namespace")); got != time.Minute {
			t.Fatalf("%s: Namespace inventory cadence %s", name, got)
		}
	}
	newClient := func() *Client {
		client, err := NewClient(Config{Endpoint: "http://127.0.0.1:1", AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	for name, service := range map[string]*Service{
		"no option": NewService(newClient(), scope, 0),
		"divisor 1": NewService(newClient(), scope, 0, WithCaptureCadenceDivisor(1)),
	} {
		if limiter := service.client.indexLimiter; limiter.Limit() != 2 || limiter.Burst() != 4 {
			t.Fatalf("%s: list rate %v burst %d, want production 2/4", name, limiter.Limit(), limiter.Burst())
		}
	}
	fast := NewService(newClient(), scope, 0, WithCaptureCadenceDivisor(10))
	if got := fast.collectionCadence(captureCadences["ConfigMap"]); got != 30*time.Second {
		t.Fatalf("divided ConfigMap cadence %s", got)
	}
	if limiter := fast.client.indexLimiter; limiter.Limit() != 20 || limiter.Burst() != 4 {
		t.Fatalf("fixture list rate %v burst %d", limiter.Limit(), limiter.Burst())
	}
	if got := fast.Scoped(Scope{OrgID: "default", ClusterID: "other"}).collectionCadence(time.Minute); got != 6*time.Second {
		t.Fatalf("scoped service lost the fixture pace: %s", got)
	}
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
	return saveDelayReplicaWithGrace(t, endpoint, store, clusterID, pace, 500*time.Millisecond)
}

// saveDelayReplicaWithGrace starts a replica whose published capture reads as
// delayed once its own save has been pending for longer than grace.
func saveDelayReplicaWithGrace(t *testing.T, endpoint string, store storage.Provider, clusterID string, pace replicaPace, grace time.Duration) (*Service, *ServiceRegistry) {
	t.Helper()
	client, err := NewClient(Config{Endpoint: endpoint, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: clusterID, CredentialID: "unchanged"}, time.Hour)
	service.historySaveGrace = grace
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
			// A newer capture reads as delayed until its own save is tried, so the
			// wait holds until every replica reports this case's code.
			reports := func(service *Service) bool {
				save := historySaveOf(t, service)
				return save != nil && save.Code == test.code
			}
			eventually(t, "a failed history save seen by every replica", func() bool {
				return observed.failures.Load() > 1 && reports(replicas[0]) && reports(replicas[1])
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
			}

			// historyPages reads every history page of one replica and says how
			// it differs from the expected report, or "" when it matches. A
			// history read that fails, or a response that carries a cause,
			// fails the test at once.
			historyPages := func(i int, service *Service) string {
				now := time.Now().UTC()
				crashes, crashErr := service.Crashes(t.Context(), kubechanges.CrashQuery{})
				changes, changeErr := service.Changes(t.Context(), ChangeQuery{})
				timeline, timelineErr := service.Timeline(t.Context(), TimelineQuery{})
				issues, issueErr := service.Issues(t.Context(), IssueOptions{Since: now.Add(-time.Hour), Until: now})
				for name, err := range map[string]error{"crashes": crashErr, "changes": changeErr, "timeline": timelineErr, "issues": issueErr} {
					if err != nil {
						t.Fatalf("replica %d %s history read failed during a save outage: %s", i, name, DiagnoseError(err).Code)
					}
				}
				encoded, err := json.Marshal([]any{crashes, changes, timeline, issues})
				if err != nil {
					t.Fatal(err)
				}
				for _, leaked := range []string{"connector_credential_unresolved", "credential_unavailable", "deadline", "10.0.0.7", "journal statement", "evidence limit"} {
					if strings.Contains(string(encoded), leaked) {
						t.Fatalf("replica %d history response carried %q", i, leaked)
					}
				}
				for name, capture := range map[string]*CaptureStatus{"crashes": &crashes.Capture, "changes": changes.Capture, "timeline": timeline.Capture, "issues": issues.Capture} {
					if capture == nil || capture.HistorySave == nil {
						return fmt.Sprintf("replica %d %s history page did not report the failed save", i, name)
					}
					save := capture.HistorySave
					if save.Code != test.code || save.Retryable != test.retryable || save.SavedThrough == nil || !save.SavedThrough.Equal(checkpoint.At) {
						return fmt.Sprintf("replica %d %s save status = %+v, want %s retryable=%t saved through %s", i, name, save, test.code, test.retryable, checkpoint.At)
					}
				}
				if !strings.Contains(string(encoded), `"history_save":{"code":"`+test.code+`"`) || !strings.Contains(string(encoded), `"saved_through":"`+checkpoint.At.UTC().Format(time.RFC3339Nano)+`"`) {
					return fmt.Sprintf("replica %d history response shape: %s", i, encoded)
				}
				return ""
			}
			// Captures keep arriving. A capture published after the wait above
			// is not yet behind until its save grace passes, and reads as
			// delayed until its own save is tried, so a single read can land in
			// that window on a loaded run. Every page of both replicas must
			// agree in one pass; a lasting difference still fails at the hang
			// guard, with the last difference seen.
			for wait := polling(t, "every history page of every replica reporting the failed save"); ; wait.next() {
				mismatch := ""
				for i, service := range replicas {
					if mismatch = historyPages(i, service); mismatch != "" {
						break
					}
				}
				if mismatch == "" {
					break
				}
				wait.what = "every history page of every replica reporting the failed save; last difference: " + mismatch
			}

			// A history read that itself fails is a store outage, never a credential one.
			observed.readsDown.Store(true)
			if _, err := replicas[1].Crashes(t.Context(), kubechanges.CrashQuery{}); DiagnoseError(err).Code != "history_store_unavailable" {
				t.Fatalf("failed history read reported as %q", DiagnoseError(err).Code)
			}
			observed.readsDown.Store(false)

			observed.restore()
			// A newer capture inside its grace reports nothing before it is
			// saved, so a clear page alone does not prove a save: wait for the
			// checkpoint to advance, then for every replica to clear.
			eventually(t, "the next successful history save on every replica", func() bool {
				latest, err := observed.EvidenceCheckpoint(t.Context(), scope)
				return err == nil && latest.At.After(checkpoint.At) && historySaveCleared(t, replicas[0]) && historySaveCleared(t, replicas[1])
			})
		})
	}
}

func TestHistorySaveOutageStaysWithinItsCluster(t *testing.T) {
	t.Parallel()
	endpoint := saveDelayCluster(t)
	observed := newFailingSaveStore(fmt.Errorf("journal statement: %w", context.DeadlineExceeded), "save-delay-a")
	failing := saveDelayReplica(t, endpoint, observed, "save-delay-a", fixturePace)
	// The healthy cluster's own save can wait past a short grace on a loaded
	// race run and would then read as delayed by itself. Its grace is long so
	// only state from the other cluster could put a save status on its pages;
	// the check below proves its saves still land during the other's outage.
	healthy, _ := saveDelayReplicaWithGrace(t, endpoint, observed, "save-delay-b", fixturePace, scaledTimeout(10*time.Second))
	eventually(t, "a failed history save on one cluster", func() bool {
		return observed.failures.Load() > 1 && historySaveOf(t, failing) != nil
	})
	page, err := healthy.Crashes(t.Context(), kubechanges.CrashQuery{})
	if err != nil {
		t.Fatalf("healthy cluster history read: %s", DiagnoseError(err).Code)
	}
	if page.Capture.HistorySave != nil {
		t.Fatalf("another cluster's save outage reached this cluster: %+v", page.Capture.HistorySave)
	}
	healthyScope := storage.EvidenceScope{OrgID: "default", ClusterID: "save-delay-b", ClusterUID: "save-delay-physical"}
	eventually(t, "the healthy cluster's published capture saved during the other's outage", func() bool {
		healthy.changes.mu.Lock()
		cache := healthy.changes.inventories[healthy.indexScope()]
		healthy.changes.mu.Unlock()
		if cache == nil {
			return false
		}
		cache.mu.RLock()
		batch, failure := cache.generation.EvidenceBatch, cache.generation.EvidenceFailure
		cache.mu.RUnlock()
		checkpoint, err := observed.EvidenceCheckpoint(t.Context(), healthyScope)
		if err != nil || checkpoint.At.IsZero() || failure != "" {
			return false
		}
		return batch == nil || batch.ID == checkpoint.ID || !batch.At.After(checkpoint.At)
	})
	if observed.failures.Load() < 2 || historySaveOf(t, failing) == nil {
		t.Fatal("the failing cluster recovered before the healthy cluster's save was checked")
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
	eventually(t, "repeated lease conflicts on history saves", func() bool {
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
	eventually(t, "a rejected history save reported to readers", func() bool {
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
	eventuallyWithin(t, 75*time.Second, "new cluster data published and its own save attempted while saves are rejected", func() bool {
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
	// The first save after the release is the one that must carry the gap;
	// a slow test can see later saves land before it looks.
	released := len(observed.saved)
	observed.mu.Unlock()
	service.recovery.mu.Lock()
	credentialFailures := service.recovery.failures
	service.recovery.mu.Unlock()
	if credentialFailures != 0 {
		t.Fatalf("a rejected save counted %d credential failures", credentialFailures)
	}

	// The following good capture saves, records the uncovered interval as a
	// gap, and clears the rejection.
	eventuallyWithin(t, 75*time.Second, "the next capture saved after the rejection", func() bool {
		observed.mu.Lock()
		saved := len(observed.saved) > released
		observed.mu.Unlock()
		return saved && historySaveCleared(t, service)
	})
	observed.mu.Lock()
	defer observed.mu.Unlock()
	recovered := observed.saved[released]
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
	// Production pacing keeps captures apart, so the outage below starts
	// between them.
	replicas := []*Service{saveDelayReplica(t, endpoint, observed, "save-unprepared", productionPace), saveDelayReplica(t, endpoint, observed, "save-unprepared", productionPace)}
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "save-unprepared", ClusterUID: "save-delay-physical"}
	eventually(t, "a complete first capture saved to history", func() bool {
		health, err := replicas[0].SyncHealth(t.Context())
		checkpoint, checkpointErr := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), scope)
		return err == nil && health.State == "ready" && health.PendingKinds == 0 && checkpointErr == nil && checkpoint.ID != "" && historySaveCleared(t, replicas[0])
	})
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
	eventuallyWithin(t, 75*time.Second, "a capture published on every replica without a save", func() bool {
		return unprepared(replicas[0]) && unprepared(replicas[1])
	})
	// The holder publishes a capture without a save only when no save of
	// its own is running, so the checkpoint is settled now: read it here
	// rather than before the outage, when a second capture may still land.
	checkpoint, err := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
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

func (proofFailingSource) verifiedCredentialPrincipal(context.Context, string) (string, proofOutcome, error) {
	return "", proofUnavailable, errors.New("principal proof endpoint refused the token")
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
			err := service.commitStorage(t.Context(), "journal", func(ctx context.Context) error { return nil })
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
	err := service.commitStorage(ctx, "journal", func(ctx context.Context) error { saved = true; return nil })
	if saved || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a save past its deadline ran or failed differently: saved=%t %v", saved, err)
	}
	if countsAgainstCredential(err) || errors.Is(err, ErrCredentialUnavailable) || DiagnoseError(err).Code == "connector_credential_unresolved" {
		t.Fatalf("a contended credential lock was reported as a credential fault: %v", err)
	}
}

// While every history save times out, live inventory still publishes each
// new cluster read: live views stay current and the cluster stays ready, and
// only saved history lags, reported as not saving.
func TestLiveInventoryPublishesWhileEveryHistorySaveTimesOut(t *testing.T) {
	t.Parallel()
	endpoint, fixture := saveDelayFixture(t)
	observed := newFailingSaveStore(fmt.Errorf("journal statement on 10.0.0.7: %w", context.DeadlineExceeded), "save-timeout")
	service := saveDelayReplica(t, endpoint, observed, "save-timeout", fixturePace)
	eventually(t, "history reported as not saving", func() bool {
		save := historySaveOf(t, service)
		return observed.failures.Load() > 0 && save != nil && save.Code == "history_save_delayed"
	})
	for round := range 3 {
		name := fmt.Sprintf("while-not-saving-%d", round)
		failuresBefore := observed.failures.Load()
		pod := `{"operation":"upsert","resource":"pods","namespace":"batch","name":"` + name + `","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","namespace":"batch","uid":"` + name + `"},"status":{"phase":"Running"}}}`
		mutation := httptest.NewRecorder()
		fixture.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(pod)))
		if mutation.Code != http.StatusOK {
			t.Fatal("pod mutation failed")
		}
		eventuallyWithin(t, 75*time.Second, "the new pod live and its capture's save attempted", func() bool {
			page, err := service.List(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Pod"], Namespace: "batch"})
			if err != nil {
				return false
			}
			listed := false
			for _, item := range page.Items {
				listed = listed || item.Name == name
			}
			return listed && observed.failures.Load() > failuresBefore
		})
		health, err := service.SyncHealth(t.Context())
		if err != nil || health.State != "ready" || len(health.Failing) != 0 {
			t.Fatalf("round %d: live inventory is not ready while history is not saving: state=%q failing=%+v err=%v", round, health.State, health.Failing, err)
		}
		if save := historySaveOf(t, service); save == nil || save.Code != "history_save_delayed" || !save.Retryable {
			t.Fatalf("round %d: history save status = %+v, want delayed and retryable", round, save)
		}
	}
	service.recovery.mu.Lock()
	credentialFailures := service.recovery.failures
	service.recovery.mu.Unlock()
	if credentialFailures != 0 {
		t.Fatalf("history save timeouts counted %d credential failures", credentialFailures)
	}
}

// timingOutJournal times out every history save.
type timingOutJournal struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	mu       sync.Mutex
	attempts map[string]int
}

func (store *timingOutJournal) CommitEvidence(_ context.Context, _ storage.EvidenceScope, batch storage.EvidenceBatch) error {
	store.mu.Lock()
	store.attempts[batch.ID]++
	store.mu.Unlock()
	return fmt.Errorf("journal statement on 10.0.0.7: %w", context.DeadlineExceeded)
}

func (store *timingOutJournal) attemptsOf(id string) (int, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.attempts[id], len(store.attempts)
}

// Over a hundred cycles after the store failed a save, no capture is
// prepared or saved until the backoff has passed: the history save state stays
// one dropped capture, never a growing queue. The capture prepared once the
// backoff passes records the dropped stretch as a gap. A capture published over
// one still waiting to start replaces it and is counted, and a capture whose
// authority proof failed is never saved.
func TestHistorySaveQueueStaysBoundedOverManyFailingCycles(t *testing.T) {
	isolateProblems(t, 0, nil)
	base := storage.NewMemory()
	store := &timingOutJournal{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore), attempts: map[string]int{}}
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "save-queue", CredentialID: "unchanged"})
	service.SetChangeStorage(store)
	service.changes.evidenceEnabled = true
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	cache := &inventoryCache{}
	cache.historySave.clock = func() time.Time { return time.Unix(0, clock.Load()) }
	service.changes.mu.Lock()
	service.changes.inventories[service.indexScope()] = cache
	service.changes.mu.Unlock()
	t.Cleanup(service.startHistorySaver(t.Context(), cache))
	lease := &inventoryLease{scope: service.scope, record: inventoryLeaseRecord{Owner: "owner", Expires: time.Now().Add(time.Hour)}}
	settled := func() pendingHistorySave {
		var pending pendingHistorySave
		eventually(t, "the running save to end", func() bool {
			cache.mu.RLock()
			defer cache.mu.RUnlock()
			pending = cache.historySave
			return pending.inFlight == ""
		})
		return pending
	}
	// publish is one cycle: it prepares a capture only when one is due.
	prepared := 0
	publish := func(name string) (inventoryGeneration, bool) {
		if !service.historyCaptureDue(cache) {
			cache.mu.Lock()
			cache.generation = inventoryGeneration{Revision: name}
			cache.mu.Unlock()
			return inventoryGeneration{}, false
		}
		previous := previousCapture(cache)
		prepared++
		generation := inventoryGeneration{Revision: name, EvidenceBatch: &storage.EvidenceBatch{ID: "capture:" + name}, EvidenceFailure: previous.EvidenceFailure}
		cache.mu.Lock()
		cache.generation = generation
		cache.mu.Unlock()
		service.queueHistorySave(lease, cache, generation)
		settled()
		return generation, true
	}
	const cycles = 100
	for cycle := 1; cycle <= cycles; cycle++ {
		publish(fmt.Sprintf("%d", cycle))
	}
	pending := settled()
	if _, total := store.attemptsOf("capture:1"); prepared != 1 || total != 1 || pending.dropped != 1 || pending.queued != nil {
		t.Fatalf("over %d failing cycles %d captures were prepared and %d saved, %d dropped, queue %+v; want one of each and nothing waiting", cycles, prepared, total, pending.dropped, pending.queued)
	}

	clock.Add(int64(historySaveRetryCeiling))
	generation, ok := publish("after-backoff")
	if !ok || generation.EvidenceFailure != "storage_unavailable" {
		t.Fatalf("the capture after the backoff was not prepared to record the dropped stretch (prepared=%t failure=%q)", ok, generation.EvidenceFailure)
	}
	if attempts, total := store.attemptsOf("capture:after-backoff"); attempts != 1 || total != 2 {
		t.Fatalf("after the backoff %d captures were saved, the newest %d times", total, attempts)
	}

	// While the cycle's authority proof is pending a capture waits to start;
	// one published over it replaces it and is counted.
	cache.mu.Lock()
	cache.authorityQuestioned = inventoryQuestionedKinds{kinds: map[string]bool{"Pod": true}}
	cache.mu.Unlock()
	dropped := settled().dropped
	for _, name := range []string{"held-a", "held-b"} {
		service.queueHistorySave(lease, cache, inventoryGeneration{Revision: name, EvidenceBatch: &storage.EvidenceBatch{ID: "capture:" + name}})
	}
	pending = settled()
	if pending.queued == nil || pending.queued.EvidenceBatch.ID != "capture:held-b" || pending.dropped != dropped+1 {
		t.Fatalf("a replaced capture was not counted: queue %+v with %d dropped, want held-b and %d", pending.queued, pending.dropped, dropped+1)
	}
	// Once the proof fails the waiting capture is never saved.
	cache.mu.Lock()
	cache.authorityQuestioned = inventoryQuestionedKinds{}
	cache.authorityFailure = unverifiedInventoryAuthority(ErrForbidden)
	cache.mu.Unlock()
	service.startHistorySave(lease, cache)
	pending = settled()
	if attempts, _ := store.attemptsOf("capture:held-b"); attempts != 0 || pending.queued != nil || pending.dropped != dropped+2 {
		t.Fatalf("a capture was saved or kept after its proof failed: attempts=%d queue=%+v dropped=%d", attempts, pending.queued, pending.dropped)
	}
}

// partedJournal saves a capture in three parts, asking the save's lease check
// before each one, as a Postgres save of a large capture does. betweenParts
// runs after the first part.
type partedJournal struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	betweenParts func()
	written      atomic.Int32
}

func (store *partedJournal) CommitEvidence(ctx context.Context, _ storage.EvidenceScope, batch storage.EvidenceBatch) error {
	for part := range 3 {
		if batch.KeepLease != nil {
			if _, err := batch.KeepLease(ctx); err != nil {
				return err
			}
		}
		store.written.Add(1)
		if part == 0 && store.betweenParts != nil {
			store.betweenParts()
		}
	}
	return nil
}

// An authority proof that fails between the transactions of one capture's
// save stops the transactions that remain.
func TestAuthorityFailureBetweenSavePartsStopsTheSave(t *testing.T) {
	base := storage.NewMemory()
	store := &partedJournal{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore)}
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "save-parts", CredentialID: "unchanged"})
	service.SetChangeStorage(store)
	service.changes.evidenceEnabled = true
	cache := &inventoryCache{}
	service.changes.mu.Lock()
	service.changes.inventories[service.indexScope()] = cache
	service.changes.mu.Unlock()
	store.betweenParts = func() {
		cache.mu.Lock()
		cache.authorityFailure = unverifiedInventoryAuthority(ErrForbidden)
		cache.mu.Unlock()
	}
	lease := &inventoryLease{scope: service.scope, record: inventoryLeaseRecord{Owner: "owner", Expires: time.Now().Add(time.Hour)}}
	generation := inventoryGeneration{Revision: "parts", EvidenceBatch: &storage.EvidenceBatch{ID: "capture:parts"}}
	err := lease.commitJournal(t.Context(), service, generation)
	if !errors.Is(err, errHistorySaveRefused) || store.written.Load() != 1 {
		t.Fatalf("a save went on after its authority proof failed: %d of 3 parts written, err=%v", store.written.Load(), err)
	}
}

// countingJournal records conflicts and coverage gaps among the captures a
// healthy store saves.
type countingJournal struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	mu        sync.Mutex
	saves     int
	conflicts int
	gaps      int
}

func (store *countingJournal) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	err := store.EvidenceStore.CommitEvidence(ctx, scope, batch)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saves++
	if errors.Is(err, storage.ErrEvidenceConflict) {
		store.conflicts++
	}
	for _, record := range batch.Records {
		if record.Type == "coverage_gap" {
			store.gaps++
			break
		}
	}
	return err
}

// On a healthy store, saving history beside collection never conflicts with
// this process's own lease moves and never records a gap.
func TestHealthyStoreSavesEveryCaptureWithoutConflictsOrGaps(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			var provider storage.Provider
			switch backend {
			case "memory":
				provider = storage.NewMemory()
			case "file":
				file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				provider = file
			default:
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated Postgres required")
				}
				postgres, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = postgres.Close() })
				provider = postgres
			}
			store := &countingJournal{Provider: provider, BlobCAS: provider.(storage.BlobCAS), EvidenceStore: provider.(storage.EvidenceStore)}
			endpoint, fixture := saveDelayFixture(t)
			cluster := fmt.Sprintf("healthy-%s-%d", backend, time.Now().UnixNano())
			service := saveDelayReplica(t, endpoint, store, cluster, fixturePace)
			published := func() string {
				cache := service.inventoryCache()
				if cache == nil {
					return ""
				}
				cache.mu.RLock()
				defer cache.mu.RUnlock()
				if cache.generation.EvidenceBatch == nil {
					return ""
				}
				return cache.generation.EvidenceBatch.ID
			}
			for round := range 15 {
				before := published()
				name := fmt.Sprintf("healthy-%d", round)
				pod := `{"operation":"upsert","resource":"pods","namespace":"batch","name":"` + name + `","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","namespace":"batch","uid":"` + name + `"},"status":{"phase":"Running"}}}`
				mutation := httptest.NewRecorder()
				fixture.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(pod)))
				if mutation.Code != http.StatusOK {
					t.Fatal("pod mutation failed")
				}
				eventuallyWithin(t, 75*time.Second, "a new capture published", func() bool {
					id := published()
					return id != "" && id != before
				})
			}
			historySavesSettled(t, service.inventoryCache())
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.saves < 15 || store.conflicts != 0 || store.gaps != 0 {
				t.Fatalf("a healthy %s store saved %d captures with %d conflicts and %d gap records, want at least 15 with none", backend, store.saves, store.conflicts, store.gaps)
			}
		})
	}
}

// hangingJournal lets the first history save land, then holds every later
// save until its context ends, as a database that stopped answering does. It
// counts how many saves run at once.
type hangingJournal struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	mu       sync.Mutex
	saved    bool
	running  int
	most     int
	attempts int
}

func (store *hangingJournal) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	store.mu.Lock()
	if !store.saved {
		store.mu.Unlock()
		err := store.EvidenceStore.CommitEvidence(ctx, scope, batch)
		store.mu.Lock()
		store.saved = err == nil
		store.mu.Unlock()
		return err
	}
	store.running++
	store.attempts++
	store.most = max(store.most, store.running)
	store.mu.Unlock()
	<-ctx.Done()
	store.mu.Lock()
	store.running--
	store.mu.Unlock()
	return ctx.Err()
}

// While a history save hangs on a database that does not answer, collection
// keeps publishing each new cluster read, and no more than one save runs.
func TestCollectionKeepsPublishingWhileAHistorySaveHangs(t *testing.T) {
	t.Parallel()
	endpoint, fixture := saveDelayFixture(t)
	base := storage.NewMemory()
	store := &hangingJournal{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore)}
	service := saveDelayReplica(t, endpoint, store, "save-hangs", fixturePace)
	eventually(t, "a history save hanging", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.running > 0
	})
	for round := range 3 {
		name := fmt.Sprintf("while-saving-hangs-%d", round)
		pod := `{"operation":"upsert","resource":"pods","namespace":"batch","name":"` + name + `","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","namespace":"batch","uid":"` + name + `"},"status":{"phase":"Running"}}}`
		mutation := httptest.NewRecorder()
		fixture.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(pod)))
		if mutation.Code != http.StatusOK {
			t.Fatal("pod mutation failed")
		}
		eventuallyWithin(t, 75*time.Second, "the new pod published by collection while a save hangs", func() bool {
			service.changes.mu.Lock()
			cache := service.changes.inventories[service.indexScope()]
			service.changes.mu.Unlock()
			if cache == nil {
				return false
			}
			cache.mu.RLock()
			defer cache.mu.RUnlock()
			for _, record := range cache.generation.View.Records {
				if record.UID == name {
					return true
				}
			}
			return false
		})
	}
	store.mu.Lock()
	most, attempts := store.most, store.attempts
	store.mu.Unlock()
	if most != 1 || attempts < 1 {
		t.Fatalf("%d saves ran at once (%d started), want exactly one at a time", most, attempts)
	}
	if health, err := service.SyncHealth(t.Context()); err != nil || health.State != "ready" {
		t.Fatalf("live inventory while a save hangs: state=%q err=%v", health.State, err)
	}
}

// hangingBlobStore answers blob reads only when their context ends once it is
// armed, as a database that stopped answering does.
type hangingBlobStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	armed   atomic.Bool
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func (store *hangingBlobStore) hang(ctx context.Context) error {
	store.once.Do(func() { close(store.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.release:
		return errors.New("released")
	}
}

func (store *hangingBlobStore) ReadBlob(name string) ([]byte, error) {
	if store.armed.Load() && strings.HasPrefix(name, "kubernetes/inventory/") {
		return nil, store.hang(context.Background())
	}
	return store.Provider.ReadBlob(name)
}

func (store *hangingBlobStore) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	if store.armed.Load() && strings.HasPrefix(name, "kubernetes/inventory/") {
		return nil, store.hang(ctx)
	}
	return store.Provider.ReadBlob(name)
}

func (store *hangingBlobStore) WriteBlobContext(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.Provider.WriteBlob(name, data)
}

// A blob read on a database that stopped answering ends with its context, so
// a credential rebuild that stops the collection waiting on it completes
// instead of being held behind the hung read.
func TestRebuildStopsACollectionWaitingOnAHungBlobRead(t *testing.T) {
	isolateProblems(t, 0, nil)
	fixture := newFakeCluster(t, "synthetic-a", "api")
	client, err := NewClient(fixture.trustedConfig(&credentialSource{authorization: func(context.Context) (string, error) {
		return "Bearer synthetic-a", nil
	}}))
	if err != nil {
		t.Fatal(err)
	}
	base := storage.NewMemory()
	store := &hangingBlobStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore), entered: make(chan struct{}), release: make(chan struct{})}
	service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.recovery = &clusterRecovery{cluster: "cluster"}
	service.SetChangeStorage(store)
	registry := NewServiceRegistry(service)
	t.Cleanup(registry.Close)
	// Runs before the registry closes, so a read the code under test failed
	// to cancel cannot hold the test's own cleanup.
	t.Cleanup(func() { close(store.release) })
	store.armed.Store(true)
	registry.StartCapture(t.Context())
	waitFor(t, store.entered, "the collection to wait on the hung read")

	service.identity.mu.Lock()
	service.identity.retired, service.identity.renewal = true, true
	service.identity.mu.Unlock()
	type outcome struct {
		service *Service
		err     error
	}
	result := make(chan outcome, 1)
	go func() {
		rebuilt, err := registry.resolveCluster(t.Context(), "org", "cluster", nil)
		result <- outcome{rebuilt, err}
	}()
	done := waitFor(t, result, "the rebuild to stop the waiting collection and finish")
	if done.err != nil || done.service == nil || done.service.identity == service.identity {
		t.Fatalf("rebuild behind a hung blob read ended with %v", done.err)
	}
}
