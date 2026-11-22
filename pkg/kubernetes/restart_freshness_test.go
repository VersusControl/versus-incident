package kubernetes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// restoredGeneration is a published generation whose every kind was last
// listed observedAgo ago, as one restored from storage after a restart.
func restoredGeneration(observedAgo time.Duration) inventoryGeneration {
	at := time.Now().UTC().Add(-observedAgo)
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, LastAttemptAt: at}
	}
	return inventoryGeneration{Revision: "restored", FirstReadyAt: at.Add(-time.Hour), View: kubeindex.Snapshot{Kinds: kinds}}
}

// restartFreshnessService serves generation from a scope cache this process
// started collecting startedAgo ago, with or without a generation published
// since that start.
func restartFreshnessService(t *testing.T, generation inventoryGeneration, startedAgo time.Duration, published bool) (*Service, *inventoryCache) {
	t.Helper()
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "cluster"})
	done := make(chan struct{})
	close(done)
	cache := &inventoryCache{generation: generation, done: done, cancel: func() {}, startedAt: time.Now().UTC().Add(-startedAgo), publishedSinceStart: published}
	service.changes.mu.Lock()
	service.changes.inventories[service.indexScope()] = cache
	service.changes.mu.Unlock()
	return service, cache
}

// syncAndViewState returns the cluster sync state and the state a view of
// Pods reports.
func syncAndViewState(t *testing.T, service *Service) (string, float64, string) {
	t.Helper()
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatalf("sync health: %v", err)
	}
	_, status, err := service.CachedIndexSnapshot(t.Context(), "Pod")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	age := 0.0
	if health.AgeSeconds != nil {
		age = *health.AgeSeconds
	}
	return health.State, age, status.State
}

// After a restart the cluster keeps its last state until a generation
// published after the restart is served, instead of turning stale on the
// observation times saved before the restart. Here the last listing was
// fresh at the restart (2m50s before it) and is past the stale limit now.
// The reported age stays the true age, and once a new generation is served
// the usual rule applies again.
func TestRestartedClusterKeepsItsStateUntilItsFirstCycleEnds(t *testing.T) {
	service, cache := restartFreshnessService(t, restoredGeneration(3*time.Minute+20*time.Second), 30*time.Second, false)
	state, age, view := syncAndViewState(t, service)
	if state != "ready" || view != "ready" {
		t.Fatalf("a restarted cluster before its first publication reads sync=%s view=%s, want ready", state, view)
	}
	if age < (3*time.Minute + 20*time.Second).Seconds() {
		t.Fatalf("the reported age %.0fs hides the stored observation's age", age)
	}
	cache.mu.Lock()
	cache.publishedSinceStart = true
	cache.mu.Unlock()
	if state, _, view := syncAndViewState(t, service); state != "stale" || view != "stale" {
		t.Fatalf("an old observation after a new publication reads sync=%s view=%s, want stale", state, view)
	}
}

// A first publication that never comes does not keep the cluster out of stale for
// ever: ages count from the process start, which ages like any observation.
func TestRestartGraceIsBoundedWhenTheFirstCycleNeverEnds(t *testing.T) {
	// Listed one minute before a start four minutes ago.
	service, _ := restartFreshnessService(t, restoredGeneration(5*time.Minute), 4*time.Minute, false)
	if state, _, view := syncAndViewState(t, service); state != "stale" || view != "stale" {
		t.Fatalf("a first cycle running for four minutes reads sync=%s view=%s, want stale", state, view)
	}
}

// A first cycle that fails is reported as a failure, whether or not a
// generation was published since the restart.
func TestFailedFirstCycleAfterRestartReportsTheFailure(t *testing.T) {
	for _, published := range []bool{false, true} {
		service, cache := restartFreshnessService(t, restoredGeneration(time.Minute), 30*time.Second, published)
		cache.mu.Lock()
		cache.failed, cache.failedClass = true, "unavailable"
		cache.mu.Unlock()
		health, err := service.SyncHealth(t.Context())
		if err != nil {
			t.Fatalf("sync health: %v", err)
		}
		if health.State != "error" {
			t.Fatalf("a failed first cycle (published since start=%t) reads %s, want error", published, health.State)
		}
	}
}

// A replica that did not restart, whose first cycle ended long ago, keeps the
// usual rule: a recent observation is ready and an old one is stale.
func TestReplicaThatDidNotRestartKeepsTheStaleRule(t *testing.T) {
	recent, _ := restartFreshnessService(t, restoredGeneration(2*time.Minute), 2*time.Hour, true)
	if state, _, view := syncAndViewState(t, recent); state != "ready" || view != "ready" {
		t.Fatalf("a running replica with a two-minute-old observation reads sync=%s view=%s, want ready", state, view)
	}
	old, _ := restartFreshnessService(t, restoredGeneration(10*time.Minute), 2*time.Hour, true)
	if state, _, view := syncAndViewState(t, old); state != "stale" || view != "stale" {
		t.Fatalf("a running replica with a ten-minute-old observation reads sync=%s view=%s, want stale", state, view)
	}
}

// An outage that began before the restart is not hidden by it. The replica
// that collects is alive but has not listed the cluster for an hour, and a
// replica restarted ten seconds ago, which does not hold the lease, reports
// that at once: stale while the last listing is served, and the failure
// once the owner publishes it.
func TestRestartDoesNotHideAnOutageThatBeganBeforeIt(t *testing.T) {
	stale, _ := restartFreshnessService(t, restoredGeneration(time.Hour), 10*time.Second, false)
	if state, _, view := syncAndViewState(t, stale); state != "stale" || view != "stale" {
		t.Fatalf("an hour-old listing on a just-restarted replica reads sync=%s view=%s, want stale", state, view)
	}
	failing := restoredGeneration(time.Hour)
	for kind, state := range failing.View.Kinds {
		state.State, state.Error, state.CycleFailure, state.FailingSince = "error", "unavailable", true, time.Now().UTC().Add(-time.Hour)
		failing.View.Kinds[kind] = state
	}
	failed, _ := restartFreshnessService(t, failing, 10*time.Second, false)
	health, err := failed.SyncHealth(t.Context())
	if err != nil {
		var failure *syncHealthFailure
		if !errors.As(err, &failure) {
			t.Fatalf("sync health: %v", err)
		}
		health = failure.Report
	}
	if health.State != "error" {
		t.Fatalf("an hour-long outage on a just-restarted replica reads %s, want error", health.State)
	}
}

// The real worker: a fresh restored generation stays ready while the first
// cycle's reads are held, and the failure of that cycle is what the cluster reports
// once it ends.
func TestInventoryWorkerEndsTheRestartGraceWhenItsFirstCycleEnds(t *testing.T) {
	release := make(chan struct{})
	// Every read waits for the release, then the cluster answers with a
	// server error.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	service.changes.mu.Lock()
	service.changes.inventories[service.indexScope()] = &inventoryCache{generation: restoredGeneration(time.Minute)}
	service.changes.mu.Unlock()
	service.startInventory(t.Context())
	defer service.StopCapture()

	for range 20 {
		health, err := service.SyncHealth(t.Context())
		if err != nil || health.State != "ready" {
			t.Fatalf("a restored cluster during its first cycle reads %s (%v), want ready", health.State, err)
		}
	}
	close(release)
	// The cycle's failure is reported as soon as it ends, grace or not.
	state := ""
	waitUntil(t, 30*time.Second, "the failed first cycle to be reported", func() bool {
		health, err := service.SyncHealth(t.Context())
		if err != nil {
			var failure *syncHealthFailure
			if !errors.As(err, &failure) {
				t.Fatalf("sync health after a failed first cycle: %v", err)
			}
			health = failure.Report
		}
		state = health.State
		return state != "ready" && state != "syncing"
	})
	if state != "error" && state != "partial" {
		t.Fatalf("a failed first cycle reads %s, want its failure", state)
	}
}
