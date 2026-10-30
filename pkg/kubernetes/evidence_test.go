package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCaptureEvidenceLifecycleSurvivesRestartAndDeletedInventory(t *testing.T) {
	var forbidden atomic.Bool
	var replaced atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if forbidden.Load() {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			uid := "physical-cluster"
			if replaced.Load() {
				uid = "different-cluster"
			}
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": uid}})
			return
		}
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	defer server.Close()
	dir := t.TempDir()
	provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	origin := time.Now().UTC().Add(-10 * time.Minute)
	generation := func(at time.Time, broken, partial bool) inventoryGeneration {
		kinds := map[string]kubeindex.KindStatus{}
		for kind := range indexedResourceIDs {
			kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
		}
		records := []kubeindex.Record{{UID: "physical-cluster", Kind: "Namespace", Name: "kube-system"}}
		if broken {
			records = append(records, kubeindex.Record{UID: "pod-uid", Kind: "Pod", Namespace: "shop", Name: "api", Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}})
		}
		if partial {
			state := kinds["Pod"]
			state.State, state.Partial, state.Error = "error", true, "unavailable"
			kinds["Pod"] = state
		}
		return inventoryGeneration{Revision: at.Format(time.RFC3339Nano), View: kubeindex.Snapshot{Kinds: kinds, Records: records}}
	}
	previous := inventoryGeneration{}
	for i, sample := range []struct{ broken, partial bool }{{true, false}, {true, false}, {false, true}, {false, false}, {true, false}} {
		current := generation(origin.Add(time.Duration(i)*10*time.Second), sample.broken, sample.partial)
		if err = service.prepareEvidence(t.Context(), &current, previous); err != nil {
			t.Fatal(err)
		}
		if err = service.commitEvidence(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		if err = service.commitEvidence(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		previous = current
	}
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-cluster"}
	journal, err := provider.(storage.EvidenceStore).QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: origin.Add(-time.Minute), Until: time.Now().UTC(), Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	opened, resolved, knownGaps := 0, 0, 0
	occurrences := map[string]bool{}
	for _, record := range journal.Items {
		if record.Type == "coverage_gap" && record.Kind == "Pod" {
			knownGaps++
		}
		if record.Rule != "pod.crash_loop" {
			continue
		}
		switch record.Type {
		case "issue_opened":
			opened++
			var issue storage.EvidenceIssue
			if err = json.Unmarshal(record.Payload, &issue); err != nil {
				t.Fatal(err)
			}
			occurrences[issue.OccurrenceID] = true
		case "issue_resolved":
			resolved++
			if !record.At.Equal(origin.Add(30 * time.Second)) {
				t.Fatal("partial inventory falsely resolved an issue")
			}
		}
	}
	if knownGaps == 0 {
		t.Fatal("known short capture failure was silently marked complete on recovery")
	}
	if opened != 2 || resolved != 1 || len(occurrences) != 2 {
		t.Fatalf("lifecycle opened=%d resolved=%d occurrences=%d", opened, resolved, len(occurrences))
	}
	restartedProvider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	restarted := newTestService(t, server.URL, service.Scope())
	restarted.SetChangeStorage(restartedProvider)
	restarted.changes.evidenceEnabled = true
	// There is no live inventory at all in this successor. History is separately authorized.
	timeline, err := restarted.Timeline(t.Context(), TimelineQuery{Since: origin.Add(-30 * 24 * time.Hour), Until: time.Now().UTC(), Namespace: "shop", Limit: 500})
	if err != nil || len(timeline.Items) == 0 {
		t.Fatalf("history depends on live inventory: %v items=%d", err, len(timeline.Items))
	}
	issues, err := restarted.Issues(t.Context(), IssueOptions{Since: origin.Add(-time.Minute), Until: time.Now().UTC(), Namespace: "shop"})
	if err != nil || len(issues.Items) == 0 {
		t.Fatalf("issue observations unavailable after restart: %v", err)
	}
	forbidden.Store(true)
	if page, err := restarted.Timeline(t.Context(), TimelineQuery{Since: origin.Add(-time.Minute), Until: time.Now().UTC(), Namespace: "shop"}); err == nil || len(page.Items) != 0 {
		t.Fatal("reduced permission exposed history")
	}
	forbidden.Store(false)
	replaced.Store(true)
	if page, err := restarted.Timeline(t.Context(), TimelineQuery{Since: origin.Add(-time.Minute), Until: time.Now().UTC(), Namespace: "shop"}); err != nil || len(page.Items) != 0 {
		t.Fatal("reused configured cluster identity merged physical clusters")
	}
	// No legacy blob participates in the new read path.
	legacyChange := Change{ID: "legacy", Cluster: "cluster", Kind: "Pod", UID: "legacy-pod", Name: "old", Type: "created", At: origin}
	if err = kubechanges.NewStore(provider, service.indexScope()).Append([]Change{legacyChange}, origin); err != nil {
		t.Fatal(err)
	}
	replaced.Store(false)
	afterLegacy, err := restarted.Timeline(t.Context(), TimelineQuery{Since: origin.Add(-time.Minute), Until: time.Now().UTC(), Namespace: "shop", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range afterLegacy.Items {
		if item.ID == "change:legacy" {
			t.Fatal("legacy history was imported or dual-read")
		}
	}

}

func TestEvidenceFenceRejectsRetiredInventoryPublication(t *testing.T) {
	provider := storage.NewMemory()
	store := provider.(storage.EvidenceStore)
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	if err := provider.WriteBlob("kubernetes/inventory/test", []byte("successor")); err != nil {
		t.Fatal(err)
	}
	batch := storage.EvidenceBatch{ID: "stale", At: time.Now().UTC(), Epoch: "retired", GuardKey: "kubernetes/inventory/test", GuardExpected: []byte("retired")}
	if err := store.CommitEvidence(t.Context(), scope, batch); !errors.Is(err, storage.ErrEvidenceConflict) {
		t.Fatalf("stale publication committed: %v", err)
	}
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.ID != "" {
		t.Fatal("retired writer advanced checkpoint")
	}
}

func TestCandidateCloneCannotMutatePublishedRecordsOrObservationTime(t *testing.T) {
	origin := time.Now().UTC().Add(-time.Minute)
	original := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: origin}}, Records: []kubeindex.Record{{UID: "pod", Kind: "Pod", Summary: map[string]string{"phase": "Running"}}}}, NamespaceProof: map[string]time.Time{"shop": origin}}
	candidate, err := cloneInventoryGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	candidate.View.Records[0].Summary["phase"] = "Failed"
	candidate.NamespaceProof["other"] = time.Now()
	if original.View.Records[0].Summary["phase"] != "Running" || len(original.NamespaceProof) != 1 || !snapshotObservedAt(original).Equal(origin) {
		t.Fatal("candidate changed committed data")
	}
	service := newTestService(t, "http://127.0.0.1:12345", Scope{ClusterID: "cluster"})
	seedInventory(t, service, original.View.Records)
	service.changes.inventories[service.indexScope()].generation = original
	first, err := service.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.ObservedAt.Equal(origin) || !second.ObservedAt.Equal(first.ObservedAt) {
		t.Fatal("GET advanced snapshot time")
	}
}

func TestRegistryFileCaptureCommitsIssuesWithNoBrowserAndRestoresHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metadata := func(uid, name, namespace string) map[string]any {
			return map[string]any{"uid": uid, "name": name, "namespace": namespace}
		}
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"metadata": metadata("physical-cluster", "kube-system", "")})
			return
		}
		items := []map[string]any{}
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			items = append(items, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata("pod-uid", "api", "default"), "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "restartCount": 2, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}})
		case strings.HasSuffix(r.URL.Path, "/namespaces"):
			items = append(items, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata("physical-cluster", "kube-system", "")}, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": metadata("default-uid", "default", "")})
		}
		kind, version := "List", "v1"
		if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadataList") {
			kind, version = "PartialObjectMetadataList", "meta.k8s.io/v1"
			for i, item := range items {
				items[i] = map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata", "metadata": item["metadata"]}
			}
		}
		writeJSON(w, map[string]any{"apiVersion": version, "kind": kind, "items": items})
	}))
	defer server.Close()
	dir := t.TempDir()
	provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	service.client.indexLimiter = nil
	service.SetChangeStorage(provider)
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	defer registry.Close()
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-cluster"}
	since := time.Now().UTC().Add(-time.Hour)
	waitUntil(t, 8*time.Second, "background-only issue journal commit", func() bool {
		page, err := provider.(storage.EvidenceStore).QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: since, Until: time.Now().UTC(), Type: "issue_opened", Rule: "pod.crash_loop"})
		return err == nil && len(page.Items) == 1
	})
	if err = registry.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	reader := newTestService(t, server.URL, service.Scope())
	reader.SetChangeStorage(reopened)
	reader.changes.evidenceEnabled = true
	restored, _, restoreErr := reader.CachedNamespaceSnapshot(t.Context(), "default", "Pod")
	if restoreErr != nil || len(restored.Records) != 1 || restored.Records[0].UID != "pod-uid" {
		t.Fatalf("authorized committed inventory did not restore: %v", restoreErr)
	}
	page, err := reader.Timeline(t.Context(), TimelineQuery{Since: since, Until: time.Now().UTC(), Namespace: "default", BreaksOnly: true})
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("restarted journal=%+v err=%v", page, err)
	}
}

func TestEvidenceCheckpointContentionAndDeletionResumeWithoutStaleReplay(t *testing.T) {
	provider := storage.NewMemory()
	store := provider.(storage.EvidenceStore)
	service := newTestService(t, "http://127.0.0.1:12345", Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC()
	initial := storage.EvidenceBatch{ID: "initial", At: now, Epoch: service.scope.CredentialID}
	if err := store.CommitEvidence(t.Context(), scope, initial); err != nil {
		t.Fatal(err)
	}
	newer := storage.EvidenceBatch{ID: "capture:newer", Expected: "", At: now.Add(time.Second), Epoch: service.scope.CredentialID, CoveredThrough: now.Add(time.Second)}
	generation := inventoryGeneration{EvidenceScope: scope, EvidenceBatch: &newer}
	if err := service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatal("newer capture could not rebase after checkpoint contention")
	}
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.ID != "capture:newer" || !checkpoint.CoveredThrough.Equal(newer.At) {
		t.Fatal("contention lost newer complete coverage")
	}
	late := storage.EvidenceBatch{ID: "capture:late", Expected: "initial", At: now, Epoch: service.scope.CredentialID, Issues: []storage.EvidenceIssue{{Key: "key", Kind: "Pod", UID: "pod", Rule: "pod.crash_loop", LastSeen: now, Payload: json.RawMessage(`{}`)}}}
	generation.EvidenceBatch = &late
	if err = service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatal("safe older evidence was not recovered")
	}
	records, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: now.Add(-time.Minute), Until: now.Add(time.Minute), Type: "issue"})
	if err != nil || len(records.Items) != 1 {
		t.Fatal("older observation was dropped")
	}
	var issue storage.EvidenceIssue
	if err = json.Unmarshal(records.Items[0].Payload, &issue); err != nil || !issue.ObservationOnly {
		t.Fatal("older observation changed the active lifecycle")
	}
	if err = store.DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if err = service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.ID != "" {
		t.Fatal("queued old evidence repopulated an explicitly deleted journal")
	}
	fresh := storage.EvidenceBatch{ID: "capture:fresh", Generation: checkpoint.Generation, At: now.Add(2 * time.Second), Epoch: service.scope.CredentialID}
	generation.EvidenceBatch = &fresh
	if err = service.commitEvidence(t.Context(), generation); err != nil {
		t.Fatal("new captures could not resume after explicit deletion")
	}
	checkpoint, err = store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || checkpoint.ID != fresh.ID {
		t.Fatal("fresh capture did not publish after deletion")
	}
}

func TestColdRestoreCannotReplayInventoryThroughRejectedOrReducedStaticCredentials(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var denied atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if denied.Load() {
					http.Error(w, "synthetic denied", code)
					return
				}
				if r.URL.Path == "/api/v1/namespaces/kube-system" {
					writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "physical"}})
					return
				}
				writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": []any{}})
			}))
			defer server.Close()
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			construct := func() *Service {
				client, err := NewClient(Config{Endpoint: server.URL, Credentials: staticCredential(t, "synthetic-static"), AllowLoopbackHTTP: true})
				if err != nil {
					t.Fatal(err)
				}
				service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
				service.SetChangeStorage(provider)
				seedInventoryDiscovery(service)
				client.indexLimiter = nil
				return service
			}
			original := construct()
			now := time.Now().UTC()
			kinds := map[string]kubeindex.KindStatus{}
			for kind := range indexedResourceIDs {
				kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: now}
			}
			generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{{UID: "physical", Kind: "Namespace", Name: "kube-system"}, {UID: "old-pod", Kind: "Pod", Namespace: "private", Name: "old-authorized-data"}}}}
			lease, owned, err := original.claimInventory(t.Context(), provider, "seed-owner")
			if err != nil || !owned {
				t.Fatal("could not seed committed inventory")
			}
			if err = lease.publish(t.Context(), original, &generation); err != nil {
				t.Fatal(err)
			}
			if err = lease.release(); err != nil {
				t.Fatal(err)
			}
			denied.Store(true)
			restarted := construct()
			registry := NewServiceRegistry(restarted)
			registry.StartCapture(t.Context())
			defer registry.Close()
			for i := 0; i < 10; i++ {
				overview, err := restarted.Overview(t.Context())
				if err == nil {
					t.Fatal("rejected static credential returned a successful snapshot or false zero")
				}
				raw, _ := json.Marshal(overview)
				if strings.Contains(string(raw), "old-authorized-data") {
					t.Fatal("cold restore replayed protected data before authority was proved")
				}
			}
			if restarted.recordedFirstReadyAt().IsZero() {
				t.Fatal("failed authority proof erased first publication metadata")
			}
		})
	}
}

func TestWarmingGenerationCannotReplacePublishedValuesWithSuccessfulZeroResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("warming read started collection")
		http.Error(w, "unexpected", 500)
	}))
	defer server.Close()
	provider := storage.NewMemory()
	original := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "old"})
	original.SetChangeStorage(provider)
	published := time.Now().UTC().Add(-time.Hour)
	if _, err := rememberFirstPublication(provider, original.scope, published); err != nil {
		t.Fatal(err)
	}
	successor := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "new"})
	successor.SetChangeStorage(provider)
	successor.changes.evidenceEnabled = true
	// An empty newly scoped store is warming, not an authoritative empty LIST.
	generation, err := successor.readInventory(t.Context())
	if err != nil || len(generation.View.Kinds) != 0 {
		t.Fatal("did not reproduce the warming generation")
	}
	if _, err = successor.Overview(t.Context()); !errors.Is(err, ErrInventoryNotReady) {
		t.Fatalf("warming Overview returned false zeros: %v", err)
	}
	if _, err = successor.Issues(t.Context(), IssueOptions{Broken: true}); !errors.Is(err, ErrInventoryNotReady) {
		t.Fatalf("warming Issues returned a false empty conclusion: %v", err)
	}
	if !successor.recordedFirstReadyAt().Equal(published) {
		t.Fatal("warming replacement reopened first publication")
	}
	// A committed, complete empty observation remains a real current result.
	seedInventory(t, successor, nil)
	page, err := successor.Issues(t.Context(), IssueOptions{Broken: true})
	if err != nil || len(page.Items) != 0 || page.Broken.Total != 0 || page.Sync.Partial {
		t.Fatal("authoritative empty observation was not accepted")
	}
}

func TestColdRestoreProofCannotMixTwoUnverifiedCredentialEpochs(t *testing.T) {
	var rotated atomic.Bool
	source := &credentialSource{authorization: func(context.Context) (string, error) {
		if rotated.Load() {
			return "Bearer synthetic-b", nil
		}
		return "Bearer synthetic-a", nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "physical"}})
			return
		}
		if r.URL.Path == "/api/v1/nodes" {
			rotated.Store(true)
		}
		writeJSON(w, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": []any{}})
	}))
	defer server.Close()
	provider := storage.NewMemory()
	construct := func() *Service {
		client, err := NewClient(Config{Endpoint: server.URL, Credentials: source, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
		service.SetChangeStorage(provider)
		return service
	}
	original := construct()
	at := time.Now().UTC()
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
	}
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{{Kind: "Namespace", Name: "kube-system", UID: "physical"}, {Kind: "Pod", Name: "protected", UID: "pod", Namespace: "private"}}}}
	lease, owned, err := original.claimInventory(t.Context(), provider, "seed-owner")
	if err != nil || !owned {
		t.Fatal("seed lease unavailable")
	}
	if err = lease.publish(t.Context(), original, &generation); err != nil {
		t.Fatal(err)
	}
	if err = lease.release(); err != nil {
		t.Fatal(err)
	}
	reader := construct()
	reader.changes.evidenceEnabled = true
	if snapshot, err := reader.readInventory(t.Context()); err == nil || len(snapshot.View.Records) != 0 {
		t.Fatal("mixed-epoch proof exposed old protected inventory")
	}
	if reader.identity.ctx.Err() == nil {
		t.Fatal("unverified rotation did not fence the restore")
	}
}
