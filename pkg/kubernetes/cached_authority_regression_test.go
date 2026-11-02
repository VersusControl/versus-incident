package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

// Observe the actual durable head and journal writes, without changing their
// timing or guards. The LIST attempt provenance identifies which cycle wrote a
// checkpoint even if its journal commit completes after permission revocation.
type authorityObservedStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	t    *testing.T
	mode *atomic.Int32
}

func (p *authorityObservedStore) CompareAndSwapBlob(key string, old, next []byte) (bool, error) {
	won, err := p.BlobCAS.CompareAndSwapBlob(key, old, next)
	if won && err == nil {
		var a, b inventoryLeaseRecord
		if json.Unmarshal(old, &a) == nil && json.Unmarshal(next, &b) == nil && b.Revision != "" && b.Revision != a.Revision {
			raw, readErr := p.ReadBlob(key + "/data/" + b.DataOwner + "/" + strconv.Itoa(b.Slot))
			var generation inventoryGeneration
			if readErr == nil && json.Unmarshal(raw, &generation) == nil {
				p.t.Logf("durable head mode=%d revision=%s pod=%+v deployment=%+v", p.mode.Load(), b.Revision, generation.View.Kinds["Pod"], generation.View.Kinds["Deployment"])
			}
		}
	}
	return won, err
}

func (p *authorityObservedStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	err := p.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err == nil {
		p.t.Logf("durable journal mode=%d id=%s expected=%s at=%s", p.mode.Load(), batch.ID, batch.Expected, batch.At.Format(time.RFC3339Nano))
	}
	return err
}

func TestAutonomousCachedCatalogClassifiesCurrentMaterialAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: observe permission revocation and recovery on the natural inventory cadence")
	}
	for _, backend := range []string{"memory", "file"} {
		for _, scenario := range []string{"all", "multiple-kinds", "secret-only"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
				if err != nil {
					t.Fatal(err)
				}
				seed := httptest.NewRecorder()
				fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"cached-physical"}}}`)))
				if seed.Code != 200 {
					t.Fatal("identity fixture failed")
				}
				var mode atomic.Int32
				if scenario == "secret-only" {
					mode.Store(2)
				}
				var catalogReads, materialDenials, authorityProofs atomic.Int32
				var firstMaterial, heldOnce atomic.Bool
				materialSeen := make(chan struct{})
				proofSeen := make(chan chan struct{}, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api" {
						catalogReads.Add(1)
					}
					current := mode.Load()
					denied := current == 1 || current == 2 && strings.HasSuffix(r.URL.Path, "/secrets") || current == 3 && (strings.HasSuffix(r.URL.Path, "/pods") || strings.HasSuffix(r.URL.Path, "/deployments"))
					if denied {
						if limit := r.URL.Query().Get("limit"); limit != "" && limit != "1" {
							materialDenials.Add(1)
							if firstMaterial.CompareAndSwap(false, true) {
								close(materialSeen)
							}
						}
						if current == 1 && r.URL.Query().Get("limit") == "1" && firstMaterial.Load() {
							authorityProofs.Add(1)
							if heldOnce.CompareAndSwap(false, true) {
								release := make(chan struct{})
								select {
								case proofSeen <- release:
								case <-r.Context().Done():
									return
								}
								select {
								case <-release:
								case <-r.Context().Done():
									return
								}
							}
						}
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					fixture.ServeHTTP(w, r)
				}))
				defer upstream.Close()
				client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
				if err != nil {
					t.Fatal(err)
				}
				service := NewService(client, Scope{OrgID: "default", ClusterID: "cached-authority", CredentialID: "unchanged"}, time.Hour)
				var provider storage.Provider = storage.NewMemory()
				if backend == "file" {
					provider, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
					if err != nil {
						t.Fatal(err)
					}
				}
				provider = &authorityObservedStore{Provider: provider, BlobCAS: provider.(storage.BlobCAS), EvidenceStore: provider.(storage.EvidenceStore), t: t, mode: &mode}
				service.SetChangeStorage(provider)
				registry := NewServiceRegistry(service)
				defer registry.Close()
				registry.StartCapture(t.Context())
				health := func() (SyncHealth, error) {
					report, err := service.SyncHealth(t.Context())
					if carried, ok := SyncHealthReport(err); ok {
						report = carried
					}
					return report, err
				}
				waitUntil(t, 20*time.Second, "complete initial natural publication", func() bool {
					report, err := health()
					return err == nil && report.State == "ready" && report.PendingKinds == 0 && !report.FirstReadyAt.IsZero()
				})
				initial, _ := health()
				scope := service.ActiveScope()
				first, err := service.Workloads(t.Context(), WorkloadListOptions{})
				if err != nil || len(first.Items) == 0 {
					t.Fatalf("initial data missing: count=%d err=%v", len(first.Items), err)
				}
				if scenario == "secret-only" {
					if initial.FailingKinds != 0 || initial.UnreadableKinds != 1 || initial.Kinds["Secret"].Error != "forbidden" || initial.Kinds["Secret"].CycleFailure {
						t.Fatalf("isolated permission gap was made a cycle failure: %+v", initial)
					}
					return
				}
				since := time.Now().UTC().Add(-time.Hour)
				history, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				if err != nil || len(history.Items) == 0 {
					t.Fatalf("initial history missing: %d %v", len(history.Items), err)
				}
				store := provider.(storage.EvidenceStore)
				evidenceScope := storage.EvidenceScope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, ClusterUID: "cached-physical"}
				before, record, err := readInventoryGeneration(provider, scope, "")
				if err != nil {
					t.Fatal(err)
				}
				beforeRaw, _ := json.Marshal(before)
				// Ready data is visible before its journal commit finishes. Pin the
				// checkpoint belonging to this exact head before revoking permission;
				// an authorized baseline commit is not a denied-cycle publication.
				var checkpoint storage.EvidenceCheckpoint
				waitUntil(t, 8*time.Second, "baseline data-head journal committed", func() bool {
					checkpoint, err = store.EvidenceCheckpoint(t.Context(), evidenceScope)
					return err == nil && before.EvidenceBatch != nil && checkpoint.ID == before.EvidenceBatch.ID
				})
				catalogs := catalogReads.Load()
				t.Logf("baseline checkpoint=%s at=%s revision=%s revoke_at=%s", checkpoint.ID, checkpoint.At.Format(time.RFC3339Nano), record.Revision, time.Now().UTC().Format(time.RFC3339Nano))
				if scenario == "all" {
					mode.Store(1)
				} else {
					mode.Store(3)
				}
				select {
				case <-materialSeen:
					t.Logf("first material denial at=%s", time.Now().UTC().Format(time.RFC3339Nano))
				case <-time.After(40 * time.Second):
					t.Fatal("normal material cadence never observed the changed permissions")
				}
				if scenario == "multiple-kinds" {
					waitUntil(t, 8*time.Second, "partial-permission candidate publication", func() bool {
						report, err := health()
						return err == nil && report.Kinds["Pod"].Error == "forbidden" && report.Kinds["Deployment"].Error == "forbidden"
					})
					report, err := health()
					if err != nil || report.State != "ready" || report.FailingKinds != 0 || report.UnreadableKinds != 2 {
						t.Fatalf("multiple legitimate gaps became a whole-authority fault: state=%s failing=%d unreadable=%d err=%v", report.State, report.FailingKinds, report.UnreadableKinds, err)
					}
					allowed, err := service.Workloads(t.Context(), WorkloadListOptions{})
					if err != nil || len(allowed.Items) == 0 {
						t.Fatalf("partial permission loss blocked allowed workload kinds: %v", err)
					}
					for _, item := range allowed.Items {
						if item.Kind == "Pod" || item.Kind == "Deployment" {
							t.Errorf("one successful witness broadened permissions for denied kind %s", item.Kind)
						}
					}
					if catalogReads.Load() != catalogs || service.ActiveScope() != scope {
						t.Fatal("control did not retain the warm catalog and original credential scope")
					}
					return
				}
				// Admit a current producer proof or the old incorrect publication, not an
				// arbitrary delay. No cache invalidation or cadence override is used.
				var release chan struct{}
				waitUntil(t, 8*time.Second, "current proof or a forbidden candidate checkpoint", func() bool {
					select {
					case release = <-proofSeen:
						return true
					default:
					}
					current, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
					return err == nil && current.ID != checkpoint.ID
				})
				if release == nil {
					report, _ := health()
					workloads, workErr := service.Workloads(t.Context(), WorkloadListOptions{})
					issues, issueErr := service.Issues(t.Context(), IssueOptions{})
					current, _ := store.EvidenceCheckpoint(t.Context(), evidenceScope)
					latest, head, _ := readInventoryGeneration(provider, scope, "")
					t.Fatalf("cached catalog bypassed current proof: denied=%d sync=%s failing=%d unreadable=%d Workloads=%d/%v Issues=%d/%v checkpoint=%s/%s revision=%s pod=%+v", materialDenials.Load(), report.State, report.FailingKinds, report.UnreadableKinds, len(workloads.Items), workErr, len(issues.Items), issueErr, current.ID, current.At.Format(time.RFC3339Nano), head.Revision, latest.View.Kinds["Pod"])
				}
				pending, _ := health()
				if pending.State == "ready" {
					t.Error("an unverified current authority probe claimed ready")
				}
				close(release)
				waitUntil(t, 8*time.Second, "confirmed whole-authority failure", func() bool {
					report, _ := health()
					return report.State == "error" && report.FailingKinds > 0 && report.Kinds["Pod"].Error == "forbidden" && report.Kinds["Pod"].CycleFailure
				})
				deniedWorkloads, err := service.Workloads(t.Context(), WorkloadListOptions{})
				if !errors.Is(err, ErrForbidden) || len(deniedWorkloads.Items) != 0 {
					t.Errorf("terminal Workloads: count=%d err=%v", len(deniedWorkloads.Items), err)
				}
				deniedIssues, err := service.Issues(t.Context(), IssueOptions{})
				if !errors.Is(err, ErrForbidden) || len(deniedIssues.Items) != 0 {
					t.Errorf("terminal live Issues: count=%d err=%v", len(deniedIssues.Items), err)
				}
				deniedHistory, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				if !errors.Is(err, ErrForbidden) || len(deniedHistory.Items) != 0 {
					t.Errorf("terminal history: count=%d err=%v", len(deniedHistory.Items), err)
				}
				after, afterRecord, err := readInventoryGeneration(provider, scope, "")
				if err != nil {
					t.Fatal(err)
				}
				afterRaw, _ := json.Marshal(after)
				current, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
				if err != nil || current.ID != checkpoint.ID || record.Revision != afterRecord.Revision || !bytes.Equal(beforeRaw, afterRaw) {
					t.Fatal("failed current proof published or altered last-good data/history")
				}
				if catalogReads.Load() != catalogs || authorityProofs.Load() == 0 || service.ActiveScope() != scope {
					t.Fatal("proof depended on catalog refresh, credential rotation or scope replacement")
				}
				mode.Store(0)
				waitUntil(t, 15*time.Second, "same-scope authorized recovery", func() bool {
					report, err := health()
					if err != nil || report.State != "ready" || report.FailingKinds != 0 {
						return false
					}
					fresh, err := store.EvidenceCheckpoint(t.Context(), evidenceScope)
					return err == nil && fresh.ID != checkpoint.ID
				})
				recovered, err := service.Workloads(t.Context(), WorkloadListOptions{})
				if err != nil || len(recovered.Items) == 0 {
					t.Fatalf("recovery did not restore permitted data: %v", err)
				}
				recoveredHistory, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				if err != nil || len(recoveredHistory.Items) == 0 {
					t.Fatalf("current-authority history did not recover: %v", err)
				}
				final, _ := health()
				if service.ActiveScope() != scope || !final.FirstReadyAt.Equal(initial.FirstReadyAt) {
					t.Fatal("recovery changed credential scope or first publication")
				}
			})
		}
	}
}
