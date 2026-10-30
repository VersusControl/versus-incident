package kubernetes

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

func TestPublishedInventoryReportsCurrentAuthorityLossWithoutDiscardingLastGood(t *testing.T) {
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
			if service.inventoryRestore.validated.Load() {
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
				return err == nil && fresh.Revision != record.Revision
			})
			recovered, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if err != nil || len(recovered.Items) == 0 {
				t.Fatalf("current authorization did not recover data: %+v %v", recovered, err)
			}
			recoveredHistory, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
			if err != nil || len(recoveredHistory.Items) == 0 {
				t.Fatalf("history did not recover under current authority: %+v %v", recoveredHistory, err)
			}
			_, priorSecret, err := readInventoryGeneration(provider, originalScope, "")
			if err != nil {
				t.Fatal(err)
			}
			mode.Store(2)
			forceCycle(map[string]bool{"Secret": true})
			waitUntil(t, 8*time.Second, "published isolated Secret capability gap", func() bool {
				report, err := service.SyncHealth(t.Context())
				return err == nil && report.Kinds["Secret"].Error == "forbidden"
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
