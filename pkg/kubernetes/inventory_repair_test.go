package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// warmTestInventory runs the producer explicitly. Consumer tests must never
// depend on a request performing inventory LISTs as a hidden setup step.
func warmTestInventory(t *testing.T, service *Service, namespaces ...string) {
	t.Helper()
	if service.client == nil {
		return
	}
	service.client.indexLimiter = nil
	for _, ns := range namespaces {
		service.queueInventoryNamespace(ns)
	}
	service.startInventory(t.Context())
	t.Cleanup(service.StopCapture)
	deadline := time.Now().Add(30 * time.Second)
	for {
		service.changes.mu.Lock()
		cache := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		cache.mu.RLock()
		// Warm means the full set: the core generation publishes first and the
		// remaining kinds fill in on the following cycle.
		full := len(cache.generation.Pending) == 0 && !cache.generation.PendingDiscovery
		loaded, failed := len(cache.generation.View.Kinds) > 0 && !cache.generation.Usage.ObservedAt.IsZero() && full, cache.failed
		cache.mu.RUnlock()
		if loaded || failed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background inventory did not complete its first attempt")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInventoryContinuationFailureStopsWithoutPublishingPartial(t *testing.T) {
	for _, token := range []string{"repeat", strings.Repeat("x", 4097)} {
		t.Run(token[:6], func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				writeJSON(w, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "pod", "uid": "pod", "namespace": "shop"}}}, "metadata": map[string]string{"continue": token}})
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
			ctx := context.WithValue(t.Context(), discoveryContextKey{}, Discovery{Resources: []ResourceDefinition{{ID: indexedResourceIDs["Pod"], Kind: "Pod", Resource: "pods", Version: "v1", Namespaced: true, Available: true}}})
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			records, complete, err := service.loadIndexKind(ctx, "Pod")
			if !errors.Is(err, ErrIncompleteInventory) || complete || len(records) != 0 || requests.Load() > 2 {
				t.Fatalf("records=%d complete=%v err=%v requests=%d", len(records), complete, err, requests.Load())
			}
		})
	}
}
