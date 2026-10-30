package kubernetes

import (
	"context"
	"errors"
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

func TestLiveNamespaceAuthorityDoesNotRequirePrivilegedIdentityAccess(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "previously-bound"}[bound], func(t *testing.T) {
			var reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path != "/api/v1/namespaces/shop/pods" {
					t.Errorf("live proof requested unnecessary broader privilege: %s", r.URL.Path)
					http.Error(w, "forbidden", 403)
					return
				}
				writeJSON(w, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"uid": "pod", "name": "api", "namespace": "shop"}}}})
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{OrgID: "org", ClusterID: "cluster"})
			service.client.namespace = "shop"
			at := time.Now().UTC()
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", ObservedAt: at.Add(-time.Minute), LastAttemptAt: at}}}}
			if bound {
				generation.EvidenceScope = storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "prior-physical"}
			}
			err := service.verifyCurrentInventoryAuthority(t.Context(), &inventoryCache{}, generation, at)
			if err != nil {
				var unknown *inventoryAuthorityUnavailable
				if errors.As(err, &unknown) {
					t.Fatalf("authorized live list excluded: %v cause=%v", err, unknown.cause)
				}
				t.Fatal(err)
			}
			if reads.Load() != 1 || generation.View.Kinds["Pod"].Error != "forbidden" {
				t.Fatal("proof skipped current evidence or granted another permission")
			}
		})
	}
}

func TestLiveNamespaceAuthorityRejectsDeniedEscapedMalformedAndNetworkReads(t *testing.T) {
	for _, scenario := range []string{"denied", "escape", "malformed", "network"} {
		t.Run(scenario, func(t *testing.T) {
			var reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path != "/api/v1/namespaces/shop/pods" {
					t.Errorf("scope escaped: %s", r.URL.Path)
					http.Error(w, "forbidden", 403)
					return
				}
				switch scenario {
				case "denied":
					http.Error(w, "forbidden", 403)
				case "network":
					http.Error(w, "unavailable", 503)
				case "malformed":
					writeJSON(w, map[string]any{})
				case "escape":
					writeJSON(w, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"namespace": "other"}}}})
				}
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{ClusterID: "cluster"})
			service.client.namespace = "shop"
			at := time.Now().UTC()
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", ObservedAt: at.Add(-time.Minute), LastAttemptAt: at}}}}
			err := service.verifyCurrentInventoryAuthority(t.Context(), &inventoryCache{}, generation, at)
			expected := ErrInventoryNotReady
			if scenario == "denied" {
				expected = ErrForbidden
			}
			if !errors.Is(err, expected) || reads.Load() != 1 {
				t.Fatalf("%s became usable authority: reads=%d err=%v", scenario, reads.Load(), err)
			}
		})
	}
}

func TestLiveAuthorityRetainsMetadataOnlyProjectionPolicy(t *testing.T) {
	for kind, id := range map[string]string{"Secret": "core~v1~secrets", "ConfigMap": "core~v1~configmaps", "ServiceAccount": "core~v1~serviceaccounts"} {
		t.Run(kind, func(t *testing.T) {
			var reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if !strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") {
					t.Error("live probe requested full sensitive values")
				}
				writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{ClusterID: "cluster"})
			at := time.Now().UTC()
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{kind: {State: "error", Error: "forbidden", ObservedAt: at, LastAttemptAt: at}}}}
			if indexedResourceIDs[kind] != id {
				t.Fatal("test resource does not match catalog")
			}
			if err := service.verifyCurrentInventoryAuthority(t.Context(), &inventoryCache{}, generation, at); err != nil || reads.Load() != 1 {
				t.Fatalf("metadata proof failed: %v", err)
			}
		})
	}
}

func TestLiveAuthorityCannotAuthorizeHistoricalCaptureOrColdReplay(t *testing.T) {
	var identityReads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/shop/pods" {
			writeJSON(w, map[string]any{"items": []any{}})
			return
		}
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			identityReads.Add(1)
		}
		http.Error(w, "forbidden", 403)
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.client.namespace = "shop"
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	at := time.Now().UTC()
	generation := inventoryGeneration{liveAuthorityOnly: true, EvidenceScope: storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}, View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", ObservedAt: at, LastAttemptAt: at}}}}
	if err := service.verifyCurrentInventoryAuthority(t.Context(), &inventoryCache{}, generation, at); err != nil {
		t.Fatal(err)
	}
	if identityReads.Load() != 0 {
		t.Fatal("live list needed a privileged identity GET")
	}
	copy, err := cloneInventoryGeneration(generation)
	if err != nil || !copy.liveAuthorityOnly {
		t.Fatal("clone lost independent history admission")
	}
	if err = service.prepareEvidence(t.Context(), &copy, inventoryGeneration{}); !errors.Is(err, ErrInventoryNotReady) || copy.EvidenceBatch != nil {
		t.Fatalf("unverified physical identity advanced history: %v", err)
	}
	if err = service.authorizeInventoryRestore(context.Background(), generation); !errors.Is(err, ErrForbidden) || service.inventoryRestore.validated.Load() {
		t.Fatalf("live list authorized persisted replay: %v", err)
	}
	page, err := service.Issues(t.Context(), IssueOptions{Since: at.Add(-time.Hour), Until: at})
	if !errors.Is(err, ErrForbidden) || len(page.Items) != 0 || identityReads.Load() < 2 {
		t.Fatalf("history bypassed current physical proof: %v", err)
	}
}

func TestAutonomousNamespaceOnlyLiveFindingsDoNotNeedIdentityPrivilege(t *testing.T) {
	var pathsMu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		switch r.URL.Path {
		case "/api":
			writeJSON(w, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(w, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(w, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			http.Error(w, "forbidden", 403)
		case "/api/v1/namespaces/shop/pods":
			writeJSON(w, map[string]any{"items": []any{map[string]any{"kind": "Pod", "metadata": map[string]any{"uid": "pod", "name": "api", "namespace": "shop"}, "status": map[string]any{"phase": "Failed"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{ClusterID: "cluster"})
	warmTestInventory(t, service, "shop")
	defer service.StopCapture()
	page, err := service.Issues(t.Context(), IssueOptions{Namespace: "shop", Broken: true})
	if err != nil || len(page.Items) != 1 {
		var unknown *inventoryAuthorityUnavailable
		var cause error
		if errors.As(err, &unknown) {
			cause = unknown.cause
		}
		pathsMu.Lock()
		defer pathsMu.Unlock()
		t.Fatalf("namespace live findings=%d err=%v cause=%v paths=%v", len(page.Items), err, cause, paths)
	}
}

func TestAutonomousConfiguredNamespaceLiveDataUsesItsSuccessfulScope(t *testing.T) {
	var pathsMu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if !strings.Contains(r.URL.Path, "/namespaces/shop/") {
			http.Error(w, "forbidden", 403)
			return
		}
		items := []any{}
		if strings.HasSuffix(r.URL.Path, "/pods") && !strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
			items = append(items, map[string]any{"metadata": map[string]any{"uid": "pod", "name": "api", "namespace": "shop"}, "status": map[string]any{"phase": "Failed"}})
		}
		writeJSON(w, map[string]any{"items": items})
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{ClusterID: "configured"})
	seedInventoryDiscovery(service)
	service.client.namespace = "shop"
	service.startInventory(t.Context())
	defer service.StopCapture()
	waitUntil(t, 8*time.Second, "authorized configured namespace", func() bool {
		view, status, err := service.CachedNamespaceSnapshot(t.Context(), "shop", "Pod")
		if err != nil {
			var unknown *inventoryAuthorityUnavailable
			var cause error
			if errors.As(err, &unknown) {
				cause = unknown.cause
			}
			pathsMu.Lock()
			defer pathsMu.Unlock()
			t.Fatalf("configured namespace err=%v cause=%v paths=%v", err, cause, paths)
		}
		return status.State == "ready" && len(view.Records) == 1
	})
}

func TestCurrentCollectionWitnessIsNotAStoredPermissionGrant(t *testing.T) {
	ctx := context.WithValue(t.Context(), inventoryCollectionAuthorityKey{}, &inventoryCollectionAuthority{})
	if hasCurrentInventoryMaterialWitness(ctx) {
		t.Fatal("empty episode inherited authority")
	}
	recordInventoryMaterialResult(ctx, nil)
	recordInventoryMaterialResult(ctx, ErrForbidden)
	if hasCurrentInventoryMaterialWitness(ctx) {
		t.Fatal("an earlier success overrode a later current denial")
	}
	recordInventoryMaterialResult(ctx, nil)
	if !hasCurrentInventoryMaterialWitness(ctx) {
		t.Fatal("validated current scoped LIST was not usable live evidence")
	}
	next := context.WithValue(t.Context(), inventoryCollectionAuthorityKey{}, &inventoryCollectionAuthority{})
	if hasCurrentInventoryMaterialWitness(next) {
		t.Fatal("a prior collection authorized a newer episode")
	}
}

func TestLiveListDoesNotWeakenPersistedPhysicalIdentityChecks(t *testing.T) {
	for _, scenario := range []string{"forbidden", "missing", "empty", "mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					switch scenario {
					case "forbidden":
						http.Error(w, "forbidden", 403)
					case "missing":
						http.NotFound(w, r)
					case "empty":
						writeJSON(w, map[string]any{"metadata": map[string]any{"uid": ""}})
					case "mismatch":
						writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "other-physical"}})
					}
					return
				}
				writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{OrgID: "org", ClusterID: "cluster"})
			service.SetChangeStorage(storage.NewMemory())
			service.changes.evidenceEnabled = true
			at := time.Now().UTC()
			generation := inventoryGeneration{EvidenceScope: storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}, View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", ObservedAt: at.Add(-time.Minute), LastAttemptAt: at}}}}
			if err := service.verifyCurrentInventoryAuthority(t.Context(), &inventoryCache{}, generation, at); err != nil {
				t.Fatalf("current authorized list was excluded: %v", err)
			}
			expected := ErrForbidden
			switch scenario {
			case "missing":
				expected = ErrNotFound
			case "empty":
				expected = ErrIncompleteInventory
			case "mismatch":
				expected = ErrClusterUnavailable
			}
			if err := service.authorizeInventoryRestore(t.Context(), generation); !errors.Is(err, expected) || service.inventoryRestore.validated.Load() {
				t.Fatalf("persisted %s proof weakened: %v", scenario, err)
			}
		})
	}
}
