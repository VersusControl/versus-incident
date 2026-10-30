package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

func TestActualMaterialLoaderValidatesEveryListEnvelopeBeforeWitnessOrDeletion(t *testing.T) {
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"explicit-empty", `{"items":[]}`, true},
		{"optional-metadata", `{"items":[],"metadata":{}}`, true},
		{"missing-items", `{}`, false},
		{"invalid-json", `{"items":[}`, false},
		{"oversized-continue", `{"items":[],"metadata":{"continue":"` + strings.Repeat("x", 4097) + `"}}`, false},
		{"null-items", `{"items":null}`, false},
		{"versioned-metadata-null-empty", `{"items":null,"metadata":{"resourceVersion":"42"}}`, false},
		{"metadata-null-has-more-pages", `{"items":null,"metadata":{"resourceVersion":"42","continue":"next"}}`, false},
		{"wrong-items", `{"items":{}}`, false},
		{"null-metadata", `{"items":[],"metadata":null}`, false},
		{"wrong-metadata", `{"items":[],"metadata":[]}`, false},
		{"null-continue", `{"items":[],"metadata":{"continue":null}}`, false},
		{"wrong-continue", `{"items":[],"metadata":{"continue":2}}`, false},
		{"null-item", ` {"items":[null]} `, false},
		{"missing-item-metadata", `{"items":[{}]}`, false},
		{"wrong-item-name", `{"items":[{"metadata":{"name":7,"namespace":"shop"}}]}`, false},
		{"item-metadata", `{"items":[{"metadata":null}]}`, false},
		{"page-escape", `{"items":[{"metadata":{"uid":"escape","name":"escape","namespace":"other"}}]}`, false},
		{"invalid-second-page", `{"items":null}`, false},
		{"repeat-page", `{"items":[],"metadata":{"continue":"repeat"}}`, false},
	}
	for _, kind := range []string{"Pod", "Secret", "ControllerRevision"} {
		for _, test := range cases {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				var reads atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/deployments") {
						http.Error(w, "forbidden", 403)
						return
					}
					reads.Add(1)
					body := test.body
					if test.name == "invalid-second-page" && r.URL.Query().Get("continue") == "" {
						body = `{"items":[{"metadata":{"uid":"new","name":"new","namespace":"shop"}}],"metadata":{"continue":"next"}}`
					}
					if kind == "Secret" && test.name != "invalid-json" {
						var raw map[string]json.RawMessage
						_ = json.Unmarshal([]byte(body), &raw)
						raw["apiVersion"] = json.RawMessage(`"meta.k8s.io/v1"`)
						raw["kind"] = json.RawMessage(`"PartialObjectMetadataList"`)
						encoded, _ := json.Marshal(raw)
						body = string(encoded)
						if !strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") {
							t.Error("sensitive loader requested full values")
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				defer upstream.Close()
				service := newTestService(t, upstream.URL, Scope{ClusterID: "list-envelope"})
				seedInventoryDiscovery(service)
				ctx := context.WithValue(t.Context(), inventoryCollectionAuthorityKey{}, &inventoryCollectionAuthority{})
				if _, _, err := service.loadIndexKind(ctx, "Deployment", "shop"); !errors.Is(err, ErrForbidden) {
					t.Fatal("did not establish actual material denial")
				}
				index := kubeindex.NewUnbounded()
				old := kubeindex.Record{Kind: kind, UID: "old", Name: "old", Namespace: "shop"}
				if err := index.Ingest(kind, []kubeindex.Record{old}, true, time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				err := index.RefreshGuarded(ctx, kind, 0, time.Now().UTC(), func(ctx context.Context) ([]kubeindex.Record, bool, error) {
					return service.loadIndexKind(ctx, kind, "shop")
				}, nil)
				snapshot := index.Snapshot()
				if test.valid || kind == "Secret" && test.name == "versioned-metadata-null-empty" {
					if err != nil || len(snapshot.Records) != 0 || !hasCurrentInventoryMaterialWitness(ctx) {
						t.Fatalf("explicit empty not authoritative: err=%v records=%d witness=%t", err, len(snapshot.Records), hasCurrentInventoryMaterialWitness(ctx))
					}
				} else {
					if err == nil || len(snapshot.Records) != 1 || snapshot.Records[0].UID != "old" || hasCurrentInventoryMaterialWitness(ctx) {
						t.Fatalf("invalid actual LIST supplied completeness/deletion/witness: err=%v records=%v witness=%t", err, snapshot.Records, hasCurrentInventoryMaterialWitness(ctx))
					}
				}
				if reads.Load() == 0 {
					t.Fatal("test never executed the actual loader")
				}
				expectedReads := int32(1)
				if test.name == "invalid-second-page" || test.name == "repeat-page" {
					expectedReads = 2
				}
				if reads.Load() != expectedReads {
					t.Fatalf("invalid envelope retried/reused continuation: reads=%d expected=%d", reads.Load(), expectedReads)
				}
			})
		}
	}
}

func TestAutonomousMalformedListAfterMaterialDenialRetainsDataAndHistory(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
			if err != nil {
				t.Fatal(err)
			}
			seed := httptest.NewRecorder()
			fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"list-physical"}}}`)))
			if seed.Code != 200 {
				t.Fatal("identity fixture failed")
			}
			var malformed atomic.Bool
			var catalogReads, denials, invalidReads atomic.Int32
			var deniedMu sync.Mutex
			deniedPaths := map[string]bool{}
			allOtherShortDenied := make(chan struct{})
			var deniedOnce sync.Once
			invalidSeen := make(chan struct{})
			var invalidOnce sync.Once
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api" {
					catalogReads.Add(1)
				}
				if !malformed.Load() {
					fixture.ServeHTTP(w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pods") {
					if r.URL.Query().Get("limit") != "1" {
						select {
						case <-allOtherShortDenied:
						case <-r.Context().Done():
							return
						}
					}
					invalidReads.Add(1)
					invalidOnce.Do(func() { close(invalidSeen) })
					_, _ = w.Write([]byte(`{}`))
					return
				}
				for _, suffix := range []string{"/deployments", "/replicasets", "/daemonsets", "/statefulsets", "/events"} {
					if strings.HasSuffix(r.URL.Path, suffix) && r.URL.Query().Get("limit") != "1" {
						denials.Add(1)
						deniedMu.Lock()
						deniedPaths[suffix] = true
						complete := len(deniedPaths) == 5
						deniedMu.Unlock()
						if complete {
							deniedOnce.Do(func() { close(allOtherShortDenied) })
						}
					}
				}
				http.Error(w, "forbidden", 403)
			}))
			defer upstream.Close()
			client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{OrgID: "default", ClusterID: "malformed-current", CredentialID: "unchanged"}, time.Hour)
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
			health := func() SyncHealth {
				report, err := service.SyncHealth(t.Context())
				if carried, ok := SyncHealthReport(err); ok {
					return carried
				}
				return report
			}
			waitUntil(t, 20*time.Second, "initial complete current data", func() bool { report := health(); return report.State == "ready" && report.PendingKinds == 0 })
			since := time.Now().UTC().Add(-time.Hour)
			initial, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
			if err != nil || len(initial.Items) == 0 {
				t.Fatalf("initial history missing: %v", err)
			}
			scope := service.ActiveScope()
			stored, record, err := readInventoryGeneration(provider, scope, "")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(stored)
			evidenceScope := storage.EvidenceScope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, ClusterUID: "list-physical"}
			store := provider.(storage.EvidenceStore)
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
			if err != nil {
				t.Fatal(err)
			}
			catalogs := catalogReads.Load()
			malformed.Store(true)
			select {
			case <-invalidSeen:
			case <-time.After(40 * time.Second):
				t.Fatal("normal cached producer never executed malformed material LIST")
			}
			waitUntil(t, 8*time.Second, "failed current episode or incorrect checkpoint publication", func() bool {
				current, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
				return err == nil && current.ID != checkpoint.ID || health().State == "error"
			})
			report := health()
			current, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
			if err != nil || report.State == "ready" || current.ID != checkpoint.ID {
				t.Errorf("malformed witness admitted ready/checkpoint: state=%s checkpointChanged=%t err=%v", report.State, current.ID != checkpoint.ID, err)
			}
			after, afterRecord, err := readInventoryGeneration(provider, scope, "")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(after)
			if record.Revision != afterRecord.Revision || !bytes.Equal(before, encoded) {
				t.Error("malformed collection replaced last-good data")
			}
			page, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if !errors.Is(err, ErrInventoryNotReady) || len(page.Items) != 0 {
				t.Errorf("malformed current authority returned successful data/zero: items=%d err=%v", len(page.Items), err)
			}
			journal, err := store.QueryEvidence(t.Context(), evidenceScope, storage.EvidenceQuery{Since: since, Until: time.Now().UTC(), Type: "issue_resolved"})
			if err != nil || journal.Total != 0 {
				t.Errorf("malformed response resolved an ongoing issue: %d %v", journal.Total, err)
			}
			if denials.Load() == 0 || invalidReads.Load() == 0 || catalogReads.Load() != catalogs || service.ActiveScope() != scope {
				t.Fatal("test bypassed actual cached material denial or changed authority scope")
			}
			malformed.Store(false)
			waitUntil(t, 15*time.Second, "valid current LIST recovery", func() bool {
				current, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
				return err == nil && current.ID != checkpoint.ID && health().State == "ready"
			})
			recovered, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if err != nil || len(recovered.Items) == 0 {
				t.Fatalf("valid current LIST did not recover: %v", err)
			}
		})
	}
}
