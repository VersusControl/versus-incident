package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type authorityWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (ctx *authorityWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.observed) })
	return ctx.Context.Done()
}

func TestCurrentAuthorityProofSingleFlightCooldownAndScopeBinding(t *testing.T) {
	var tick atomic.Int64
	tick.Store(time.Now().UnixNano())
	state := inventoryAuthorityProofState{clock: func() time.Time { return time.Unix(0, tick.Load()) }}
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	owner := make(chan error, 1)
	probe := func() error { calls.Add(1); close(entered); <-release; return nil }
	go func() { owner <- state.verify(t.Context(), "scope-a", "episode-a", probe) }()
	<-entered
	var workers sync.WaitGroup
	results := make(chan error, 32)
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- state.verify(t.Context(), "scope-a", "episode-a", func() error { calls.Add(1); return nil })
		}()
	}
	cancelled, cancel := context.WithCancel(t.Context())
	waiter := &authorityWaitContext{Context: cancelled, observed: make(chan struct{})}
	cancelledResult := make(chan error, 1)
	go func() {
		cancelledResult <- state.verify(waiter, "scope-a", "episode-a", func() error { t.Error("cancelled waiter started network work"); return nil })
	}()
	<-waiter.observed
	cancel()
	if err := <-cancelledResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight waiter cancellation: %v", err)
	}

	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent proof count=%d", calls.Load())
	}
	if err := state.verify(t.Context(), "scope-a", "episode-b", func() error { calls.Add(1); return nil }); !errors.Is(err, ErrInventoryNotReady) {
		t.Fatalf("older positive admitted a newer denial during cooldown: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("cooldown issued another probe")
	}
	tick.Add(int64(2 * time.Second))
	if err := state.verify(t.Context(), "scope-a", "episode-b", func() error { calls.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := state.verify(t.Context(), "other-physical-scope", "episode-b", func() error { t.Error("cooldown issued cross-scope probe"); return nil }); !errors.Is(err, ErrInventoryNotReady) {
		t.Fatalf("proof crossed scope binding: %v", err)
	}
	tick.Add(int64(2 * time.Second))
	if err := state.verify(t.Context(), "scope-a", "episode-c", func() error { calls.Add(1); return ErrForbidden }); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if err := state.verify(t.Context(), "scope-a", "episode-d", func() error { t.Error("negative cooldown started network work"); return nil }); !errors.Is(err, ErrForbidden) {
		t.Fatalf("known negative was not retained conservatively: %v", err)
	}
	if err := state.verify(t.Context(), "scope-b", "episode-d", func() error { return nil }); !errors.Is(err, ErrInventoryNotReady) {
		t.Fatalf("negative proof crossed scope: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("unexpected proof count=%d", calls.Load())
	}
}

func TestCurrentAuthorityProofDistinguishesPartialScopedAndUnknownAuthority(t *testing.T) {
	for _, scenario := range []string{"partial", "namespace-scoped", "all-denied", "unauthorized", "network", "malformed", "escape", "ordinary-null", "metadata-null", "metadata-null-empty-continue", "metadata-null-zero-remaining", "metadata-missing-items", "metadata-missing-version", "metadata-wrong-kind", "metadata-wrong-meta", "metadata-empty-rv", "metadata-numeric-rv", "metadata-continue", "metadata-null-continue", "metadata-positive-remaining"} {
		t.Run(scenario, func(t *testing.T) {
			var reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					t.Error("live proof required a privileged identity read")
					http.Error(w, "forbidden", 403)
					return
				}
				if scenario == "network" {
					http.Error(w, "unavailable", 503)
					return
				}
				if scenario == "unauthorized" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if scenario == "ordinary-null" {
					writeJSON(w, map[string]any{"items": nil})
					return
				}
				if strings.HasPrefix(scenario, "metadata-") {
					if !strings.HasSuffix(r.URL.Path, "/secrets") {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					if !strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") {
						t.Error("sensitive fallback requested full values")
					}
					metadata := map[string]any{"resourceVersion": "opaque-fallback-version"}
					body := map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": metadata, "items": nil}
					switch scenario {
					case "metadata-null-empty-continue":
						metadata["continue"] = ""
					case "metadata-null-zero-remaining":
						metadata["remainingItemCount"] = 0
					case "metadata-missing-items":
						delete(body, "items")
					case "metadata-missing-version":
						delete(body, "apiVersion")
					case "metadata-wrong-kind":
						body["kind"] = "SecretList"
					case "metadata-wrong-meta":
						body["metadata"] = []any{}
					case "metadata-empty-rv":
						metadata["resourceVersion"] = ""
					case "metadata-numeric-rv":
						metadata["resourceVersion"] = 42
					case "metadata-continue":
						metadata["continue"] = "next"
					case "metadata-null-continue":
						metadata["continue"] = nil
					case "metadata-positive-remaining":
						metadata["remainingItemCount"] = 1
					}
					writeJSON(w, body)
					return
				}
				if scenario == "malformed" {
					writeJSON(w, map[string]any{})
					return
				}
				if scenario == "escape" {
					writeJSON(w, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"namespace": "other"}}}})
					return
				}
				permitted := scenario == "partial" && strings.HasSuffix(r.URL.Path, "/secrets") || scenario == "namespace-scoped" && r.URL.Path == "/api/v1/namespaces/shop/pods"
				if !permitted {
					http.Error(w, "forbidden", 403)
					return
				}
				writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{OrgID: "org", ClusterID: "cluster"})
			at := time.Now().UTC()
			old := at.Add(-time.Minute)
			generation := inventoryGeneration{EvidenceScope: storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}, View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{
				"Pod":    {State: "error", Error: "forbidden", ObservedAt: old, LastAttemptAt: at},
				"Secret": {State: "error", Error: "forbidden", ObservedAt: old, LastAttemptAt: at},
			}}}
			if scenario == "escape" {
				service.client.namespace = "shop"
			}
			if scenario == "namespace-scoped" {
				generation.Namespaces = map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", ObservedAt: old, LastAttemptAt: at}}}}
			}
			cache := &inventoryCache{generation: generation}
			service.changes.mu.Lock()
			service.changes.inventories[service.indexScope()] = cache
			service.changes.mu.Unlock()
			err := service.verifyCurrentInventoryAuthority(t.Context(), cache, generation, at)
			if scenario == "partial" || scenario == "namespace-scoped" || scenario == "metadata-null" || scenario == "metadata-null-empty-continue" || scenario == "metadata-null-zero-remaining" {
				if err != nil {
					t.Fatalf("legitimate partial authority was made a whole failure: %v", err)
				}
				if generation.View.Kinds["Pod"].Error != "forbidden" || generation.View.Kinds["Secret"].Error != "forbidden" {
					t.Fatal("a witness granted permissions to another kind")
				}
				return
			}
			if scenario == "unauthorized" {
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatalf("current authentication denial was treated as authority: %v", err)
				}
				return
			}
			if scenario == "all-denied" {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("all current material denials were treated as authority: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInventoryNotReady) || errors.Is(err, ErrForbidden) {
				t.Fatalf("indeterminate scope was made ready or universal denial: %v", err)
			}
			count := reads.Load()
			for i := 0; i < 20; i++ {
				workloads, workErr := service.Workloads(t.Context(), WorkloadListOptions{})
				if (!errors.Is(workErr, ErrInventoryNotReady) && !errors.Is(workErr, ErrForbidden)) || len(workloads.Items) != 0 {
					t.Fatalf("unknown authority exposed protected data: %v", workErr)
				}
				report, reportErr := service.SyncHealth(t.Context())
				if carried, ok := SyncHealthReport(reportErr); ok {
					report = carried
				}
				if report.State == "ready" {
					t.Fatal("probe error claimed ready")
				}
			}
			if reads.Load() > count+int32(len(workloadResources)) {
				t.Fatal("API polling repeated unverified scoped authority probes")
			}
		})
	}
}

func TestCurrentAuthorityProbeBudgetDoesNotInferUniversalDenial(t *testing.T) {
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "physical"}})
			return
		}
		http.Error(w, "forbidden", 403)
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	at := time.Now().UTC()
	generation := inventoryGeneration{EvidenceScope: storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}, ResourceStatus: map[string]kubeindex.KindStatus{}, Discovery: &Discovery{}}
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("custom.example~v1~objects-%02d", i)
		generation.ResourceStatus[id] = kubeindex.KindStatus{State: "error", Error: "forbidden", LastAttemptAt: at}
		generation.Discovery.Resources = append(generation.Discovery.Resources, ResourceDefinition{ID: id, Group: "custom.example", Version: "v1", Resource: fmt.Sprintf("objects-%02d", i), Available: true, Namespaced: true})
	}
	cache := &inventoryCache{}
	service.client.indexLimiter.SetLimit(0)
	service.client.indexLimiter.SetBurst(0)
	err := service.verifyCurrentInventoryAuthority(captureReadContext(t.Context(), "collector"), cache, generation, at)
	if !errors.Is(err, ErrInventoryNotReady) || errors.Is(err, ErrForbidden) {
		t.Fatalf("incomplete proof set became universal denial: %v", err)
	}
	if reads.Load() != inventoryAuthorityProbeRequests {
		t.Fatalf("probe requests=%d limit=%d", reads.Load(), inventoryAuthorityProbeRequests)
	}
}

func TestWorkloadReadAuthorityCacheExpiresAndBindsScope(t *testing.T) {
	at := time.Now()
	state := inventoryAuthorityProofState{clock: func() time.Time { return at }}
	calls := 0
	denied := false
	probe := func() error {
		calls++
		if denied {
			return ErrForbidden
		}
		return nil
	}
	for i := 0; i < 20; i++ {
		if err := state.verifyCurrentRead(t.Context(), "cluster-shop", probe); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("polling multiplied authority probes: %d", calls)
	}
	denied = true
	at = at.Add(inventoryAuthorityProbeInterval)
	if err := state.verifyCurrentRead(t.Context(), "cluster-shop", probe); !errors.Is(err, ErrForbidden) {
		t.Fatalf("old grant survived expiry: %v", err)
	}
	if err := state.verifyCurrentRead(t.Context(), "cluster-other", probe); !errors.Is(err, ErrForbidden) || calls != 3 {
		t.Fatalf("authority crossed namespace scope: calls=%d err=%v", calls, err)
	}
}

func TestWorkloadAuthorityDoesNotCacheCanceledCaller(t *testing.T) {
	var state inventoryAuthorityProofState
	ctx, cancel := context.WithCancel(t.Context())
	if err := state.verifyCurrentRead(ctx, "scope", func() error { cancel(); return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	calls := 0
	if err := state.verifyCurrentRead(t.Context(), "scope", func() error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("canceled caller poisoned next read: calls=%d err=%v", calls, err)
	}
}

func TestWorkloadAuthorityWaiterRetriesCanceledLeader(t *testing.T) {
	var state inventoryAuthorityProofState
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	defer cancelLeader()
	entered, release := make(chan struct{}), make(chan struct{})
	leader := make(chan error, 1)
	go func() {
		leader <- state.verifyCurrentRead(leaderCtx, "scope", func() error { close(entered); <-release; return context.Canceled })
	}()
	<-entered
	waiterCtx := &authorityWaitContext{Context: t.Context(), observed: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- state.verifyCurrentRead(waiterCtx, "scope", func() error { return nil }) }()
	<-waiterCtx.observed
	cancelLeader()
	close(release)
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("live waiter inherited canceled leader: %v", err)
	}
}
