package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

func TestPublishedInventoryReportsCurrentAuthorityLossWithoutDiscardingLastGood(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
			if err != nil {
				t.Fatal(err)
			}
			seed := httptest.NewRecorder()
			fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"current-physical"}}}`)))
			if seed.Code != http.StatusOK {
				t.Fatal("physical identity fixture failed")
			}
			var mode atomic.Int32
			discoveryGates := make(chan chan struct{}, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode.Load() == 1 {
					if r.URL.Path == "/api" {
						release := make(chan struct{})
						select {
						case discoveryGates <- release:
						case <-r.Context().Done():
							return
						}
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if mode.Load() == 2 && strings.HasSuffix(r.URL.Path, "/secrets") {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				fixture.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "unchanged-synthetic")})
			if err != nil {
				t.Fatal(err)
			}
			client.indexLimiter = nil
			service := NewService(client, Scope{OrgID: "default", ClusterID: "current-authority", CredentialID: "configured"}, time.Hour)
			var provider storage.Provider = storage.NewMemory()
			if backend == "file" {
				provider, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
			}
			service.SetChangeStorage(provider)
			registry := NewServiceRegistry(service)
			defer registry.Close()
			registry.StartCapture(t.Context())
			waitUntil(t, 8*time.Second, "complete initial publication", func() bool {
				report, err := service.SyncHealth(t.Context())
				return err == nil && report.State == "ready" && report.PendingKinds == 0 && !report.FirstReadyAt.IsZero()
			})
			initial, err := service.SyncHealth(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			workloads, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if err != nil || len(workloads.Items) == 0 {
				t.Fatalf("initial workloads missing: %+v %v", workloads, err)
			}
			issues, err := service.Issues(t.Context(), IssueOptions{Broken: true})
			if err != nil || len(issues.Items) == 0 {
				t.Fatalf("initial live issues missing: %+v %v", issues, err)
			}
			since := time.Now().UTC().Add(-time.Hour)
			history, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
			if err != nil || len(history.Items) == 0 {
				t.Fatalf("initial current-capture history missing: %+v %v", history, err)
			}
			originalScope := service.ActiveScope()
			service.changes.mu.Lock()
			cache := service.changes.inventories[service.indexScope()]
			service.changes.mu.Unlock()
			forceCycle := func(kinds map[string]bool) {
				service.InvalidateDiscovery()
				cache.mu.Lock()
				retained := cache.generation
				before, _ := json.Marshal(retained)
				candidate, err := cloneInventoryGeneration(retained)
				if err != nil {
					cache.mu.Unlock()
					t.Fatal(err)
				}
				now := time.Now().UTC()
				for kind, state := range candidate.View.Kinds {
					state.LastAttemptAt = now
					if kinds[kind] {
						state.LastAttemptAt = now.Add(-24 * time.Hour)
					}
					candidate.View.Kinds[kind] = state
				}
				candidate.Usage.ObservedAt = now
				cache.generation = candidate
				after, _ := json.Marshal(retained)
				cache.mu.Unlock()
				if !bytes.Equal(before, after) {
					t.Fatal("test admission changed a retained immutable generation")
				}
				select {
				case cache.wake <- struct{}{}:
				default:
				}
			}
			short := map[string]bool{"Pod": true, "Deployment": true, "ReplicaSet": true, "StatefulSet": true, "DaemonSet": true, "Event": true}
			receiveGate := func() chan struct{} {
				select {
				case release := <-discoveryGates:
					return release
				case <-time.After(8 * time.Second):
					t.Fatal("current discovery attempt was not admitted")
					return nil
				}
			}
			mode.Store(1)
			forceCycle(short)
			firstGate := receiveGate()
			// The producer is held before its first denied response, so these bytes are
			// the last authorized durable publication, not a concurrently changing one.
			before, record, err := readInventoryGeneration(provider, originalScope, "")
			if err != nil {
				t.Fatal(err)
			}
			beforeRaw, _ := json.Marshal(before)
			evidenceScope := storage.EvidenceScope{OrgID: originalScope.OrgID, ClusterID: originalScope.ClusterID, ClusterUID: "current-physical"}
			store := provider.(storage.EvidenceStore)
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
			if err != nil {
				t.Fatal(err)
			}
			close(firstGate)
			waitUntil(t, 8*time.Second, "failed attempt or its candidate publication", func() bool {
				cache.mu.RLock()
				defer cache.mu.RUnlock()
				return cache.failed || cache.generation.Revision != record.Revision
			})
			assertFailedHealth := func(phase string) {
				report, err := service.SyncHealth(t.Context())
				if err != nil || report.State != "error" || report.FailingKinds == 0 || report.ReadyKinds != 0 || report.UnreadableKinds != 0 {
					t.Errorf("%s hid whole-current authority failure: state=%s ready=%d failing=%d unreadable=%d err=%v", phase, report.State, report.ReadyKinds, report.FailingKinds, report.UnreadableKinds, err)
				}
				if !report.FirstReadyAt.Equal(initial.FirstReadyAt) {
					t.Error("authority failure changed first publication")
				}
			}
			assertFailedHealth("first denied cycle")
			service.inventoryRestore.mu.Lock()
			proofs := len(service.inventoryRestore.proved)
			service.inventoryRestore.mu.Unlock()
			if proofs != 0 {
				t.Error("earlier restore proof remained validated after current permission loss")
			}
			deniedWorkloads, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if !errors.Is(err, ErrForbidden) || len(deniedWorkloads.Items) != 0 {
				t.Errorf("terminal Workloads did not fail closed: items=%d err=%v", len(deniedWorkloads.Items), err)
			}
			deniedIssues, err := service.Issues(t.Context(), IssueOptions{Broken: true})
			if !errors.Is(err, ErrForbidden) || len(deniedIssues.Items) != 0 {
				t.Errorf("terminal live Issues did not fail closed: items=%d err=%v", len(deniedIssues.Items), err)
			}
			deniedHistory, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
			if !errors.Is(err, ErrForbidden) || len(deniedHistory.Items) != 0 {
				t.Errorf("historical Issues did not prove current authority: items=%d err=%v", len(deniedHistory.Items), err)
			}
			deniedTimeline, err := service.Timeline(t.Context(), TimelineQuery{Since: since, Until: time.Now().UTC()})
			if !errors.Is(err, ErrForbidden) || len(deniedTimeline.Items) != 0 {
				t.Errorf("historical Timeline did not prove current authority: items=%d err=%v", len(deniedTimeline.Items), err)
			}
			after, afterRecord, err := readInventoryGeneration(provider, originalScope, "")
			if err != nil {
				t.Fatal(err)
			}
			afterRaw, _ := json.Marshal(after)
			if record.Revision != afterRecord.Revision || !bytes.Equal(beforeRaw, afterRaw) || len(after.View.Records) == 0 {
				t.Error("failed collection replaced or mutated the last-good durable data")
			}
			currentCheckpoint, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
			if err != nil || currentCheckpoint.ID != checkpoint.ID {
				t.Errorf("denial advanced captured history: %+v err=%v", currentCheckpoint, err)
			}
			if service.ActiveScope() != originalScope {
				t.Fatal("test rotated or rebound the credential scope")
			}
			// A retry may rehydrate the old data, but may not reuse its old authority
			// proof to clear the failure before a current read succeeds.
			forceCycle(short)
			secondGate := receiveGate()
			assertFailedHealth("held retry")
			mode.Store(0)
			close(secondGate)
			forceCycle(short)
			waitUntil(t, 8*time.Second, "current authorized recovery publication", func() bool {
				report, err := service.SyncHealth(t.Context())
				if err != nil || report.State != "ready" || report.FailingKinds != 0 {
					return false
				}
				_, fresh, err := readInventoryGeneration(provider, originalScope, "")
				if err != nil || fresh.Revision == record.Revision {
					return false
				}
				page, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				return err == nil && len(page.Items) > 0
			})
			recovered, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if err != nil || len(recovered.Items) == 0 {
				t.Fatalf("current authorization did not recover data: %+v %v", recovered, err)
			}
			recoveredHistory, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
			if err != nil || len(recoveredHistory.Items) == 0 {
				_, proof, _ := service.persistedHistoryScope(t.Context())
				t.Fatalf("history did not recover under current authority: %+v %v code=%s pod=%+v deployment=%+v", recoveredHistory, err, DiagnoseError(err).Code, proof.Authority["Pod"], proof.Authority["Deployment"])
			}
			_, priorSecret, err := readInventoryGeneration(provider, originalScope, "")
			if err != nil {
				t.Fatal(err)
			}
			mode.Store(2)
			forceCycle(map[string]bool{"Secret": true})
			// Readers see the new denial while its proof runs, before the
			// collection that carries it is published, so wait for both.
			waitUntil(t, 8*time.Second, "published isolated Secret capability gap", func() bool {
				report, err := service.SyncHealth(t.Context())
				if err != nil || report.Kinds["Secret"].Error != "forbidden" {
					return false
				}
				_, published, err := readInventoryGeneration(provider, originalScope, "")
				return err == nil && published.Revision != priorSecret.Revision
			})
			limited, err := service.SyncHealth(t.Context())
			if err != nil || limited.State != "ready" || limited.FailingKinds != 0 || limited.UnreadableKinds != 1 || limited.Kinds["Secret"].CycleFailure {
				t.Fatalf("per-kind capability gap became cycle failure: %+v %v", limited, err)
			}
			_, secretRecord, err := readInventoryGeneration(provider, originalScope, "")
			if err != nil || secretRecord.Revision == priorSecret.Revision {
				t.Fatal("capability control was not a newly collected publication")
			}
			readable, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if err != nil || len(readable.Items) == 0 {
				t.Fatalf("per-kind gap blocked permitted workloads: %+v %v", readable, err)
			}
			live, err := service.Issues(t.Context(), IssueOptions{Broken: true})
			if err != nil || len(live.Items) == 0 {
				t.Fatalf("per-kind gap blocked permitted issues: %+v %v", live, err)
			}
			final, err := service.SyncHealth(t.Context())
			if err != nil || !final.FirstReadyAt.Equal(initial.FirstReadyAt) || service.ActiveScope() != originalScope {
				t.Fatal("recovery changed initial publication or credential/physical scope")
			}
			t.Logf("%s: current denial observed, protected APIs blocked, durable last-good retained, same-scope recovery and isolated unreadable-kind control pass", backend)
		})
	}
}

func TestQualifiedMetadataNullFallbackPublishesCurrentAuthorityToPublicViews(t *testing.T) {
	t.Parallel()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRecorder()
	fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"fallback-physical"}}}`)))
	if seed.Code != http.StatusOK {
		t.Fatal("physical identity fixture failed")
	}
	var denyMaterial atomic.Bool
	var proofs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denyMaterial.Load() {
			if strings.HasSuffix(r.URL.Path, "/secrets") && r.URL.Query().Get("limit") == "1" && strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") {
				proofs.Add(1)
				writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{"resourceVersion": "opaque-fallback-version"}, "items": nil})
				return
			}
			if r.URL.Query().Has("limit") || r.URL.Path == "/api/v1/namespaces/kube-system" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: "default", ClusterID: "fallback"}, time.Hour)
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	defer registry.Close()
	registry.StartCapture(t.Context())
	waitUntil(t, 10*time.Second, "initial publication before material denial", func() bool {
		report, err := service.SyncHealth(t.Context())
		return err == nil && report.State == "ready" && report.PendingKinds == 0 && !report.FirstReadyAt.IsZero()
	})
	initial, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	denyMaterial.Store(true)
	cache.mu.Lock()
	candidate, err := cloneInventoryGeneration(cache.generation)
	if err != nil {
		cache.mu.Unlock()
		t.Fatal(err)
	}
	for kind, state := range candidate.View.Kinds {
		state.LastAttemptAt = time.Now().Add(-24 * time.Hour)
		candidate.View.Kinds[kind] = state
	}
	cache.generation = candidate
	cache.mu.Unlock()
	select {
	case cache.wake <- struct{}{}:
	default:
	}
	waitUntil(t, 10*time.Second, "qualified empty metadata fallback publication", func() bool {
		report, err := service.SyncHealth(t.Context())
		return err == nil && proofs.Load() > 0 && report.Kinds["Pod"].Error == "forbidden" && report.Kinds["Secret"].Error == "forbidden"
	})
	report, err := service.SyncHealth(t.Context())
	if err != nil || !report.FirstReadyAt.Equal(initial.FirstReadyAt) || report.Class != "" {
		t.Fatalf("scoped empty fallback did not preserve public readiness history: class=%s err=%v", report.Class, err)
	}
	if report.Kinds["Secret"].State == "ready" || report.Kinds["Pod"].State == "ready" {
		t.Fatal("empty witness granted another material LIST permission")
	}
	if _, err := service.Overview(t.Context()); err != nil {
		t.Fatalf("validated fallback left public inventory authority unavailable: %v", err)
	}
	if _, err := service.Timeline(t.Context(), TimelineQuery{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("live-only fallback bypassed independent history authority: %v", err)
	}
}

// restartFixture is one in-code cluster behind a proxy that can fail every
// read while discovery keeps answering, with a store that outlives processes.
type restartFixture struct {
	t        *testing.T
	fixture  http.Handler
	upstream *httptest.Server
	provider storage.Provider
	// fail answers one read during an outage; nil means the cluster answers.
	fail        atomic.Pointer[func(http.ResponseWriter, *http.Request)]
	proofs      atomic.Int32
	lists       atomic.Int32
	requests    atomic.Int32
	released    chan struct{}
	releaseOnce sync.Once
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	harness := &restartFixture{t: t, fixture: fixture, provider: storage.NewMemory(), released: make(chan struct{})}
	harness.mutate(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"restart-physical"}}}`)
	// Discovery and authentication still answer during an outage, as they do
	// when the API server serves them without its storage and only reads fail.
	answers := func(path string) bool {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		return path == "/api" || path == "/apis" || len(parts) == 2 && parts[0] == "api" || len(parts) == 3 && parts[0] == "apis" || strings.HasSuffix(path, "/selfsubjectreviews")
	}
	harness.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail := harness.fail.Load(); fail != nil && !answers(r.URL.Path) {
			harness.requests.Add(1)
			switch {
			case r.URL.Path == "/api/v1/namespaces/kube-system":
				harness.proofs.Add(1)
			case r.Method == http.MethodGet && r.URL.Query().Get("limit") != "1":
				harness.lists.Add(1)
			}
			(*fail)(w, r)
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	t.Cleanup(harness.upstream.Close)
	// Runs before the server closes, so no held request outlives the test.
	t.Cleanup(harness.release)
	return harness
}

// release ends an outage's held requests; it is safe to call more than once.
func (harness *restartFixture) release() {
	harness.releaseOnce.Do(func() { close(harness.released) })
}

func (harness *restartFixture) mutate(body string) {
	harness.t.Helper()
	recorder := httptest.NewRecorder()
	harness.fixture.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		harness.t.Fatalf("fixture mutation failed: %d", recorder.Code)
	}
}

// boot starts one process for org over the shared store. A divisor above one
// shortens collection so the restarted process relists without waiting for
// production intervals.
func (harness *restartFixture) boot(org string, read []string, divisor int) (*Service, *ServiceRegistry) {
	harness.t.Helper()
	client, err := NewClient(Config{Endpoint: harness.upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(harness.t, "unchanged-synthetic")})
	if err != nil {
		harness.t.Fatal(err)
	}
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: org, ClusterID: "restarted", CredentialID: "configured"}, time.Hour)
	service.cadenceDivisor = divisor
	service.SetChangeStorage(harness.provider)
	registry := NewClusterRegistryWithScope(false, []ClusterEntry{{Info: ClusterInfo{ID: "restarted", Provider: "generic"}, Service: service}}, tenancy.NewOrgScope(org, read...))
	registry.StartCapture(harness.t.Context())
	harness.t.Cleanup(registry.Close)
	return service, registry
}

// storeFirstGeneration runs a first process until it publishes a complete
// generation, stops it, and returns what it stored.
func (harness *restartFixture) storeFirstGeneration() inventoryGeneration {
	harness.t.Helper()
	first, registry := harness.boot("default", nil, 0)
	waitUntil(harness.t, 10*time.Second, "the first process to publish a complete generation", func() bool {
		report, err := first.SyncHealth(harness.t.Context())
		return err == nil && report.State == "ready" && report.PendingKinds == 0 && !report.FirstReadyAt.IsZero()
	})
	registry.Close()
	stored, published := publishedGeneration(harness.provider, first.ActiveScope())
	if !published || len(podNames(stored)) == 0 {
		harness.t.Fatal("the first process stored no pods")
	}
	return stored
}

func (harness *restartFixture) inventory() map[string][]byte {
	harness.t.Helper()
	blobs, err := harness.provider.ListBlobs("kubernetes/inventory/")
	if err != nil {
		harness.t.Fatal(err)
	}
	result := make(map[string][]byte, len(blobs))
	for _, blob := range blobs {
		result[blob.Name] = append([]byte(nil), blob.Data...)
	}
	return result
}

func restartedPods(t *testing.T, service *Service) ([]string, error) {
	t.Helper()
	page, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Pod"]})
	names := []string{}
	for _, item := range page.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names, err
}

// A process that restarts while the cluster's API server answers discovery but
// cannot answer reads (a server error, a throttled request, or no answer at
// all) keeps the inventory it stored before the restart: it lists nothing and
// publishes nothing over that generation, so it stays the last good inventory
// for its own org and for a write org that reads it as a fallback. Reads during
// the outage fail quickly and never return the unproven generation. Once the
// cluster answers again the restarted process restores the stored generation,
// lists the cluster and publishes into its own org, and the listing is compared
// with what was stored: a pod created during the outage is recorded as created
// and the stored pods are not.
func TestRestartDuringClusterOutageKeepsStoredInventoryAndRestoresIt(t *testing.T) {
	t.Parallel()
	status := func(code int) func(*restartFixture) func(http.ResponseWriter, *http.Request) {
		return func(*restartFixture) func(http.ResponseWriter, *http.Request) {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, http.StatusText(code), code)
			}
		}
	}
	hang := func(harness *restartFixture) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-harness.released:
				http.Error(w, "released", http.StatusServiceUnavailable)
			}
		}
	}
	for _, restart := range []struct {
		name    string
		org     string
		read    []string
		outage  func(*restartFixture) func(http.ResponseWriter, *http.Request)
		retries int32
	}{
		{name: "same org server error", org: "default", outage: status(http.StatusServiceUnavailable), retries: 3},
		{name: "write org reading the default org server error", org: "licensed", read: []string{"default"}, outage: status(http.StatusServiceUnavailable), retries: 3},
		{name: "same org throttled", org: "default", outage: status(http.StatusTooManyRequests), retries: 3},
		{name: "write org reading the default org no answer", org: "licensed", read: []string{"default"}, outage: hang, retries: 1},
	} {
		t.Run(restart.name, func(t *testing.T) {
			t.Parallel()
			harness := newRestartFixture(t)
			storedGeneration := harness.storeFirstGeneration()
			stored := harness.inventory()

			outage := restart.outage(harness)
			harness.fail.Store(&outage)
			restarted, _ := harness.boot(restart.org, restart.read, 30)
			waitUntil(t, 30*time.Second, "the restarted process to retry the unanswered restore proof", func() bool {
				restarted.inventoryRestore.mu.Lock()
				remembered := restarted.inventoryRestore.unanswered != nil
				restarted.inventoryRestore.mu.Unlock()
				return remembered && harness.proofs.Load() >= restart.retries || harness.lists.Load() > 0
			})
			if lists := harness.lists.Load(); lists != 0 {
				t.Fatalf("the restarted process listed the cluster %d times while it could not prove the stored generation", lists)
			}
			// Reads during the outage fail quickly, and many of them do not
			// each ask the cluster again.
			requests := harness.requests.Load()
			for read := range 50 {
				started := time.Now()
				names, err := restartedPods(t, restarted)
				if err == nil || len(names) != 0 {
					t.Fatalf("an unproven stored generation read as %v (err %v)", names, err)
				}
				if elapsed := time.Since(started); elapsed > 4*time.Second {
					t.Fatalf("read %d during the outage took %s", read+1, elapsed)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if sent := harness.requests.Load() - requests; sent > 12 {
				t.Fatalf("50 reads during the outage sent %d requests to the cluster", sent)
			}
			after := harness.inventory()
			if len(after) != len(stored) {
				t.Fatalf("the outage changed the stored inventory from %d to %d blobs", len(stored), len(after))
			}
			for name, want := range stored {
				if !bytes.Equal(after[name], want) {
					t.Fatalf("the outage rewrote stored inventory blob %q", name)
				}
			}

			harness.mutate(`{"operation":"upsert","resource":"pods","namespace":"batch","name":"created-during-outage","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"created-during-outage","namespace":"batch","uid":"created-during-outage"},"status":{"phase":"Running"}}}`)
			harness.fail.Store(nil)
			harness.release()
			waitUntil(t, 30*time.Second, "the restarted process to publish and read its own generation with the new pod", func() bool {
				generation, published := publishedGeneration(harness.provider, restarted.ActiveScope())
				names, err := restartedPods(t, restarted)
				return published && slices.Contains(podNames(generation), "created-during-outage") && err == nil && slices.Contains(names, "created-during-outage")
			})
			names, err := restartedPods(t, restarted)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range podNames(storedGeneration) {
				if !slices.Contains(names, name) {
					t.Fatalf("the recovered read lost stored pod %s: %v", name, names)
				}
			}
			var created []string
			waitUntil(t, 30*time.Second, "the new pod recorded as created", func() bool {
				history, err := restarted.Changes(t.Context(), ChangeQuery{Kind: "Pod", Type: kubechanges.Created, Limit: 500})
				if err != nil {
					return false
				}
				created = created[:0]
				for _, change := range history.Items {
					created = append(created, change.Name)
				}
				return slices.Contains(created, "created-during-outage")
			})
			for _, name := range podNames(storedGeneration) {
				if slices.Contains(created, name) {
					t.Fatalf("stored pod %s was recorded as created after the restart: %v", name, created)
				}
			}
		})
	}
}

// After a restart one indexed kind keeps failing with a server error, as a
// resource behind a broken conversion webhook does, while the cluster and every
// other kind answer. The cluster is reachable, so the restarted process collects
// it again rather than waiting on that kind: pods are readable within the
// normal recovery time and the failing kind is reported as an error of its own.
func TestRestartWithOneFailingKindServesTheRestOfTheCluster(t *testing.T) {
	t.Parallel()
	harness := newRestartFixture(t)
	harness.storeFirstGeneration()
	broken := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/controllerrevisions") {
			http.Error(w, "conversion webhook failed", http.StatusInternalServerError)
			return
		}
		harness.fixture.ServeHTTP(w, r)
	}
	harness.fail.Store(&broken)
	restarted, _ := harness.boot("default", nil, 0)
	waitUntil(t, 10*time.Second, "pods readable after the restart", func() bool {
		names, err := restartedPods(t, restarted)
		return err == nil && len(names) > 0
	})
	waitUntil(t, 10*time.Second, "the failing kind reported as an error", func() bool {
		generation, published := publishedGeneration(harness.provider, restarted.ActiveScope())
		state := generation.View.Kinds["ControllerRevision"]
		return published && state.State == "error" && state.Error == "unavailable" && generation.View.Kinds["Pod"].State == "ready"
	})
	waitUntil(t, 10*time.Second, "the failing kind read as partial", func() bool {
		page, err := restarted.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["ControllerRevision"]})
		reported := false
		for _, partial := range page.Partial {
			reported = reported || partial.ResourceID == indexedResourceIDs["ControllerRevision"] && partial.Class == "unavailable"
		}
		return err == nil && page.Sync.Partial && page.Sync.State == "error" && reported
	})
}

// storeNamespaceSecret runs a first process whose credential may list Secrets
// only in the shop namespace, until its stored generation holds the shop
// Secret, and stops it.
func (harness *restartFixture) storeNamespaceSecret() {
	harness.t.Helper()
	harness.mutate(`{"operation":"upsert","resource":"secrets","namespace":"shop","name":"shop-secret","object":{"apiVersion":"v1","kind":"Secret","metadata":{"name":"shop-secret","namespace":"shop","uid":"shop-secret"},"type":"Opaque"}}`)
	clusterWideDenied := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/secrets" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		harness.fixture.ServeHTTP(w, r)
	}
	harness.fail.Store(&clusterWideDenied)
	first, registry := harness.boot("default", nil, 0)
	waitUntil(harness.t, 20*time.Second, "the shop Secret stored for its namespace", func() bool {
		names, _ := shopSecrets(harness.t, first)
		generation, published := publishedGeneration(harness.provider, first.ActiveScope())
		stored := false
		for _, record := range generation.Namespaces["shop"].Records {
			stored = stored || record.Kind == "Secret" && record.Name == "shop-secret"
		}
		return published && stored && slices.Contains(names, "shop-secret")
	})
	registry.Close()
}

// denySecrets answers every Secret list with a denial: the credential lost its
// namespace grant as well.
func (harness *restartFixture) denySecrets() {
	denied := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		harness.fixture.ServeHTTP(w, r)
	}
	harness.fail.Store(&denied)
}

func shopSecrets(t *testing.T, service *Service) ([]string, error) {
	t.Helper()
	page, err := service.CachedResources(t.Context(), ListOptions{ResourceID: indexedResourceIDs["Secret"], Namespace: "shop"})
	names := []string{}
	for _, item := range page.Items {
		names = append(names, item.Name)
	}
	return names, err
}

// Records stored for one namespace were listed with a namespace grant. After a
// restart whose credential lost that grant, no read serves them: the proof of
// the stored generation covers each kind in each namespace it was stored for,
// not only the kinds the credential may list cluster-wide.
func TestRestartWithoutNamespaceGrantNeverServesStoredNamespaceRecords(t *testing.T) {
	t.Parallel()
	harness := newRestartFixture(t)
	harness.storeNamespaceSecret()
	harness.denySecrets()
	restarted, _ := harness.boot("default", nil, 0)
	reads := 0
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); reads++ {
		if names, err := shopSecrets(t, restarted); slices.Contains(names, "shop-secret") {
			t.Fatalf("the stored shop Secret was served on read %d after the credential lost the namespace (err %v)", reads+1, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A restart during an outage keeps the stored generation, and the credential
// loses its namespace grant before the cluster answers again. Readers that
// prove the stored generation themselves while the worker publishes its first
// live generation are never served the stored namespace records: a proof
// covers only the generation it proved, and a live publication vouches only
// for itself.
func TestOutageThenLostNamespaceGrantNeverServesStoredNamespaceRecords(t *testing.T) {
	t.Parallel()
	harness := newRestartFixture(t)
	harness.storeNamespaceSecret()
	outage := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
	}
	harness.fail.Store(&outage)
	restarted, _ := harness.boot("default", nil, 30)
	waitUntil(t, 10*time.Second, "the restarted process to keep the stored generation", func() bool {
		restarted.inventoryRestore.mu.Lock()
		defer restarted.inventoryRestore.mu.Unlock()
		return restarted.inventoryRestore.unanswered != nil
	})
	stop := make(chan struct{})
	var served, reads atomic.Int64
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				names, _ := shopSecrets(t, restarted)
				reads.Add(1)
				if slices.Contains(names, "shop-secret") {
					served.Add(1)
				}
			}
		}()
	}
	harness.denySecrets()
	time.Sleep(4 * time.Second)
	close(stop)
	readers.Wait()
	if served.Load() > 0 {
		t.Fatalf("the stored shop Secret was served %d of %d reads after the outage ended", served.Load(), reads.Load())
	}
}

// A cluster recovers from an outage slowly, answering every request after a
// delay, while readers ask for its inventory without pause. The readers never
// start proofs of their own against the worker: the worker's proof restores
// the stored generation about as soon as it does with no readers, and readers
// add no proof requests.
func TestSlowRecoveryUnderContinuousReadsRestoresAsFastAsWithoutReads(t *testing.T) {
	t.Parallel()
	type outcome struct {
		took   time.Duration
		proofs int32
	}
	restoreAfterOutage := func(t *testing.T, readers int) outcome {
		harness := newRestartFixture(t)
		harness.storeFirstGeneration()
		var proofs atomic.Int32
		slow := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("limit") == "1" || r.URL.Path == "/api/v1/namespaces/kube-system" {
				proofs.Add(1)
			}
			time.Sleep(200 * time.Millisecond)
			harness.fixture.ServeHTTP(w, r)
		}
		down := func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		}
		harness.fail.Store(&down)
		restarted, _ := harness.boot("default", nil, 0)
		stop := make(chan struct{})
		var reading sync.WaitGroup
		for range readers {
			reading.Add(1)
			go func() {
				defer reading.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = restartedPods(t, restarted)
				}
			}()
		}
		defer func() {
			close(stop)
			reading.Wait()
		}()
		// The outage lasts long enough for the worker to back off between
		// attempts, as it does in a real outage.
		time.Sleep(8 * time.Second)
		harness.fail.Store(&slow)
		started := time.Now()
		waitUntil(t, 60*time.Second, "the worker to restore the stored generation", func() bool {
			restarted.changes.mu.Lock()
			cache := restarted.changes.inventories[restarted.indexScope()]
			restarted.changes.mu.Unlock()
			cache.mu.RLock()
			defer cache.mu.RUnlock()
			return len(cache.generation.View.Kinds) > 0 && !cache.failed
		})
		return outcome{took: time.Since(started), proofs: proofs.Load()}
	}
	var quiet, busy outcome
	var runs sync.WaitGroup
	runs.Add(2)
	t.Run("no readers", func(t *testing.T) {
		t.Parallel()
		defer runs.Done()
		quiet = restoreAfterOutage(t, 0)
	})
	t.Run("six readers", func(t *testing.T) {
		t.Parallel()
		defer runs.Done()
		busy = restoreAfterOutage(t, 6)
	})
	t.Cleanup(func() {
		runs.Wait()
		t.Logf("restore took %s without readers (%d proof requests) and %s with six readers (%d proof requests)", quiet.took, quiet.proofs, busy.took, busy.proofs)
		if busy.took > quiet.took*3/2+2*time.Second {
			t.Errorf("continuous reads delayed the restore from %s to %s", quiet.took, busy.took)
		}
		if busy.proofs > quiet.proofs*2+10 {
			t.Errorf("continuous reads raised proof requests from %d to %d", quiet.proofs, busy.proofs)
		}
	})
}

// A reset forgets every proof; a proof that was already running when the
// reset happened cannot record its result afterwards.
func TestRestoreProofStartedBeforeResetIsDiscarded(t *testing.T) {
	authority := &inventoryRestoreAuthority{}
	started := authority.epoch()
	authority.prove("published")
	authority.reset()
	authority.proveSince(started, "stored")
	if authority.covers("stored") || authority.covers("published") {
		t.Fatal("a proof that started before the reset was recorded after it")
	}
	authority.proveSince(authority.epoch(), "stored")
	if !authority.covers("stored") {
		t.Fatal("a proof that started after the reset was not recorded")
	}
}

// A reset while a restore proof is running discards that proof, so the caller
// waiting on it is not served the stored generation either: a reader and the
// worker both fail closed exactly as if the proof had not been made, and only
// a proof that runs entirely after the reset authorizes the restore.
func TestRestoreCallerIsNotServedWhenResetDiscardsItsProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		caller restoreCaller
	}{{"reader", restoreByReader}, {"worker", restoreByWorker}} {
		t.Run(test.name, func(t *testing.T) {
			harness := newRestartFixture(t)
			stored := harness.storeFirstGeneration()
			client, err := NewClient(Config{Endpoint: harness.upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "unchanged-synthetic")})
			if err != nil {
				t.Fatal(err)
			}
			client.indexLimiter = nil
			service := NewService(client, Scope{OrgID: "default", ClusterID: "restarted", CredentialID: "configured"}, time.Hour)
			service.SetChangeStorage(harness.provider)
			// As capture does, without starting a worker that would prove the
			// generation on its own.
			service.changes.mu.Lock()
			service.changes.evidenceEnabled = true
			service.changes.mu.Unlock()
			if service.evidenceStore() == nil {
				t.Fatal("the evidence store is not enabled")
			}

			// The identity read is the proof's first request: hold the first
			// one until the authority has been reset, then let it succeed.
			identityHeld, resumeIdentity := make(chan struct{}), make(chan struct{})
			var holdOnce sync.Once
			hold := func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					holdOnce.Do(func() {
						close(identityHeld)
						select {
						case <-resumeIdentity:
						case <-harness.released:
						}
					})
				}
				harness.fixture.ServeHTTP(w, r)
			}
			harness.fail.Store(&hold)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- service.authorizeRestore(ctx, stored, test.caller) }()
			select {
			case <-identityHeld:
			case <-ctx.Done():
				t.Fatal("the restore proof never read the cluster identity")
			}
			service.inventoryRestore.reset()
			close(resumeIdentity)
			err = <-result
			if err == nil {
				t.Fatal("the caller was served the stored generation although a reset discarded its proof")
			}
			// The discarded proof fails closed as an unanswered proof of an
			// unready inventory, not as some unrelated failure such as a timeout.
			var unanswered restoreProofUnanswered
			if !errors.As(err, &unanswered) || !errors.Is(err, ErrInventoryNotReady) {
				t.Fatalf("a proof discarded by a reset failed with %v, want an unanswered proof of an unready inventory", err)
			}
			if service.inventoryRestore.covers(stored.Revision) {
				t.Fatal("a proof discarded by a reset was recorded")
			}

			if err := service.authorizeRestore(ctx, stored, test.caller); err != nil {
				t.Fatalf("a proof made after the reset did not authorize the restore: %v", err)
			}
			if !service.inventoryRestore.covers(stored.Revision) {
				t.Fatal("a proof made after the reset was not recorded")
			}
		})
	}
}
