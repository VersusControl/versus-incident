package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

// The observer delegates every operation unchanged, including evidence guards.
// Its event order distinguishes a new data-head CAS from a lease-only renewal.
type completionObservedStore struct {
	storage.Provider
	storage.BlobCAS
	storage.EvidenceStore
	mu     sync.Mutex
	events []string
}

func (p *completionObservedStore) event(s string) {
	p.mu.Lock()
	p.events = append(p.events, s)
	p.mu.Unlock()
}

func (p *completionObservedStore) CompareAndSwapBlob(key string, old, next []byte) (bool, error) {
	won, err := p.BlobCAS.CompareAndSwapBlob(key, old, next)
	if won && err == nil {
		var a, b inventoryLeaseRecord
		if json.Unmarshal(old, &a) == nil && json.Unmarshal(next, &b) == nil && b.Revision != "" && b.Revision != a.Revision {
			p.event("data-head-CAS:" + b.Revision)
		}
	}
	return won, err
}

func (p *completionObservedStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	err := p.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err == nil {
		p.event("journal:" + batch.ID)
	}
	return err
}

func TestAutonomousMixedAuthorityFiniteCompletion(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"memory", "file"} {
		for _, incomplete := range []bool{false, true} {
			name := "complete"
			if incomplete {
				name = "incomplete-final-list"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				t.Parallel()
				fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
				if err != nil {
					t.Fatal(err)
				}
				seed := httptest.NewRecorder()
				fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"finite-physical"}}}`)))
				if seed.Code != http.StatusOK {
					t.Fatal("identity seed failed")
				}
				var base storage.Provider = storage.NewMemory()
				if backend == "file" {
					base, err = storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
					if err != nil {
						t.Fatal(err)
					}
				}
				observed := &completionObservedStore{Provider: base, BlobCAS: base.(storage.BlobCAS), EvidenceStore: base.(storage.EvidenceStore)}
				resourceKinds := map[string]string{}
				for kind, id := range indexedResourceIDs {
					parts := strings.Split(id, "~")
					resourceKinds[parts[len(parts)-1]] = kind
				}
				// Phases: authorized baseline, whole denial, scoped-only grant,
				// full grant during namespace collection, explicit repair of the negative.
				var phase atomic.Int32
				var mu sync.Mutex
				deniedKinds, scopeCalls := map[string]bool{}, map[string]int{}
				var lastNamespace, lastKind string
				// Scoped LISTs in arrival order. The producer walks namespaces in
				// sorted order and kinds in core-first order, once per cycle. One
				// restart is tolerated: a cycle that fails part-way may walk again
				// from the first namespace and first kind. A retry inside a walk,
				// a walk out of order or a second restart fails the test.
				var scopedOrder, heldOrder []string
				var heldFreshGlobal int32
				var firstOnce, tailOnce atomic.Bool
				firstSeen, firstRelease := make(chan struct{}), make(chan struct{})
				tailSeen, tailRelease := make(chan struct{}), make(chan struct{})
				var releaseFirstOnce, releaseTailOnce sync.Once
				releaseFirst := func() { releaseFirstOnce.Do(func() { close(firstRelease) }) }
				releaseTail := func() { releaseTailOnce.Do(func() { close(tailRelease) }) }
				var freshGlobal, invalidFinal atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					parts := strings.Split(r.URL.Path, "/")
					kind := resourceKinds[parts[len(parts)-1]]
					ns := ""
					for i := 0; i+2 < len(parts); i++ {
						if parts[i] == "namespaces" {
							ns = parts[i+1]
						}
					}
					bulk := r.URL.Query().Get("limit") != "" && r.URL.Query().Get("limit") != "1"
					current := phase.Load()
					// The producer reads namespaces for every kind whose latest
					// cluster-wide LIST was denied. Under load a paced kind may not
					// be listed again after the whole denial, so it stays denied
					// from that phase; count denials from both denying phases.
					if (current == 1 || current == 2) && ns == "" && bulk && kind != "" && kind != "Node" && kind != "Namespace" {
						mu.Lock()
						deniedKinds[kind] = true
						mu.Unlock()
					}
					if current == 1 {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					if (current == 2 || current == 3 && incomplete) && ns == "" && kind != "" && r.URL.Query().Get("limit") != "" {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					if current >= 2 && current < 4 && ns != "" && bulk && kind != "" {
						if firstOnce.CompareAndSwap(false, true) {
							close(firstSeen)
							select {
							case <-firstRelease:
							case <-r.Context().Done():
								return
							}
						}
						mu.Lock()
						if freshGlobal.Load() == 0 {
							key := ns + "/" + kind
							scopeCalls[key]++
							scopedOrder = append(scopedOrder, key)
						}
						last := ns == lastNamespace && kind == lastKind
						holdTail := last && tailOnce.CompareAndSwap(false, true)
						if holdTail {
							heldOrder = append([]string(nil), scopedOrder...)
							heldFreshGlobal = freshGlobal.Load()
						}
						mu.Unlock()
						if holdTail {
							close(tailSeen)
							select {
							case <-tailRelease:
							case <-r.Context().Done():
								return
							}
							observed.event("final-scoped-response-released")
						}
						if last && incomplete && phase.Load() == 3 {
							invalidFinal.Add(1)
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{}`))
							return
						}
					}
					if phase.Load() >= 3 && ns == "" && bulk && kind != "" {
						freshGlobal.Add(1)
					}
					fixture.ServeHTTP(w, r)
				}))
				defer upstream.Close()
				// The test holds the first and the last scoped LIST open while it
				// inspects state. Under load that inspection can outlast the
				// default 10s request timeout; the held LIST then times out, the
				// producer lists the pair again and the run sees a second walk and
				// no released tail. Use the longest request timeout the client
				// accepts so only the test decides when a held LIST ends.
				client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Timeout: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				service := NewService(client, Scope{OrgID: "default", ClusterID: "finite-authority", CredentialID: "unchanged"}, time.Hour)
				// The incomplete-final-list case keeps production pacing: it reads
				// Workloads as soon as SyncHealth reports the fresh completion, and
				// at fixture pace the workload authority proof can trail that report
				// by a fraction of a second.
				if !incomplete {
					useFixturePace(service)
				}
				service.SetChangeStorage(observed)
				registry := NewServiceRegistry(service)
				defer registry.Close()
				defer releaseFirst()
				defer releaseTail()
				registry.StartCapture(t.Context())
				health := func() SyncHealth {
					r, err := service.SyncHealth(t.Context())
					if carried, ok := SyncHealthReport(err); ok {
						r = carried
					}
					return r
				}
				eventually(t, "initial complete capture", func() bool {
					r := health()
					return r.State == "ready" && r.PendingKinds == 0 && !r.FirstReadyAt.IsZero()
				})
				initial := health()
				since := time.Now().UTC().Add(-time.Hour)
				history, err := service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				if err != nil || len(history.Items) == 0 {
					t.Fatalf("baseline captured history: %d %v", len(history.Items), err)
				}
				phase.Store(1)
				eventually(t, "natural current whole-authority failure", func() bool {
					r := health()
					return r.State == "error" && r.Kinds["Pod"].CycleFailure && r.Kinds["Pod"].Error == "forbidden"
				})
				scope := service.ActiveScope()
				before, head, err := readInventoryGeneration(observed, scope, "")
				if err != nil {
					t.Fatal(err)
				}
				beforeRaw, _ := json.Marshal(before)
				evidenceScope := storage.EvidenceScope{OrgID: scope.OrgID, ClusterID: scope.ClusterID, ClusterUID: "finite-physical"}
				checkpoint, err := observed.EvidenceCheckpoint(t.Context(), evidenceScope)
				if err != nil || checkpoint.ID == "" {
					t.Fatalf("baseline checkpoint: %+v %v", checkpoint, err)
				}
				phase.Store(2)
				await := func(ch <-chan struct{}, label string) {
					t.Helper()
					select {
					case <-ch:
					case <-time.After(55 * time.Second):
						t.Fatal("phase not admitted: " + label)
					}
				}
				await(firstSeen, "first scoped material LIST after global denials")
				// The producer traverses the union of active subscriptions and
				// observed proofs, not only namespaces present in the payload.
				wanted := map[string]bool{}
				for ns := range before.NamespaceProof {
					wanted[ns] = true
				}
				cache := service.namespaceCache()
				cache.mu.RLock()
				for ns := range cache.namespaces {
					wanted[ns] = true
				}
				cache.mu.RUnlock()
				namespaces := []string{}
				for ns := range wanted {
					namespaces = append(namespaces, ns)
				}
				sort.Strings(namespaces)
				mu.Lock()
				kinds := []string{}
				for kind := range deniedKinds {
					kinds = append(kinds, kind)
				}
				sortCoreKindsFirst(kinds)
				if len(namespaces) == 0 || len(kinds) == 0 {
					mu.Unlock()
					t.Fatal("fixture did not admit a finite namespace/kind fallback")
				}
				lastNamespace, lastKind = namespaces[len(namespaces)-1], kinds[len(kinds)-1]
				mu.Unlock()
				phase.Store(3)
				releaseFirst()
				await(tailSeen, "last scoped namespace/kind")
				assertRetained := func() {
					t.Helper()
					after, currentHead, err := readInventoryGeneration(observed, scope, "")
					afterRaw, _ := json.Marshal(after)
					cp, cpErr := observed.EvidenceCheckpoint(t.Context(), evidenceScope)
					for kind, proof := range cp.Authority {
						if proof != checkpoint.Authority[kind] && proof.State != "denied" {
							t.Fatalf("unfinished candidate promoted source authority: %s %+v", kind, proof)
						}
					}
					// Revocation is persisted immediately, not held until this
					// incomplete collection can publish its protected payload.
					cp.Authority = checkpoint.Authority
					if cp.CommitRevision < checkpoint.CommitRevision {
						t.Fatal("authority publication moved revision backwards")
					}
					cp.CommitRevision = checkpoint.CommitRevision
					cpRaw, _ := json.Marshal(cp)
					checkpointRaw, _ := json.Marshal(checkpoint)
					if err != nil || cpErr != nil || currentHead.Revision != head.Revision || !bytes.Equal(beforeRaw, afterRaw) || !bytes.Equal(cpRaw, checkpointRaw) {
						t.Fatalf("unfinished candidate changed immutable snapshot/checkpoint: %v %v", err, cpErr)
					}
				}
				assertRetained()
				held := health()
				if held.State != "error" || !held.Kinds["Pod"].CycleFailure {
					t.Fatalf("held tail hid known failed cycle: %+v", held)
				}
				workloads, err := service.Workloads(t.Context(), WorkloadListOptions{})
				if (!errors.Is(err, ErrInventoryNotReady) && !errors.Is(err, ErrForbidden)) || len(workloads.Items) != 0 {
					t.Fatalf("held tail allowed protected access: %d %v", len(workloads.Items), err)
				}
				// The walk that reached the held tail lists each subscribed
				// namespace and denied kind exactly once, in producer order, and
				// nothing else. At most one earlier walk, cut short by a failed
				// cycle, may precede it; every walk starts at the first namespace
				// and first kind and only moves forward. Nothing is listed while
				// the tail is held.
				mu.Lock()
				rank := map[string]int{}
				for i, ns := range namespaces {
					for j, kind := range kinds {
						rank[ns+"/"+kind] = i*len(kinds) + j
					}
				}
				var walkStarts []int
				contract := heldFreshGlobal == 0 && len(scopedOrder) == len(heldOrder)
				for i, key := range heldOrder {
					r, known := rank[key]
					contract = contract && known
					if i == 0 || r <= rank[heldOrder[i-1]] {
						walkStarts = append(walkStarts, i)
						contract = contract && r == 0
					}
				}
				var heldWalk []string
				if len(walkStarts) > 0 {
					heldWalk = heldOrder[walkStarts[len(walkStarts)-1]:]
				}
				contract = contract && len(walkStarts) >= 1 && len(walkStarts) <= 2 && len(heldWalk) == len(rank)
				for i, key := range heldWalk {
					contract = contract && rank[key] == i
				}
				calls, total := len(heldWalk), len(scopedOrder)
				if !contract {
					heldCount := map[string]int{}
					keys := map[string]bool{}
					for _, key := range heldWalk {
						heldCount[key]++
					}
					for key := range rank {
						keys[key] = true
					}
					for key := range scopeCalls {
						keys[key] = true
					}
					sorted := make([]string, 0, len(keys))
					for key := range keys {
						sorted = append(sorted, key)
					}
					sort.Strings(sorted)
					for _, key := range sorted {
						_, known := rank[key]
						t.Logf("scoped LISTs %s: heldWalk=%d allWalks=%d expected=%t", key, heldCount[key], scopeCalls[key], known)
					}
					t.Logf("walk start offsets in %d LISTs up to the held tail: %v", len(heldOrder), walkStarts)
				}
				mu.Unlock()
				if !contract {
					t.Fatalf("fallback operation contract: heldWalk=%d expected=%d*%d walks=%d (max 2) LISTsAtTail=%d allLISTs=%d freshGlobalAtTail=%d", calls, len(namespaces), len(kinds), len(walkStarts), len(heldOrder), total, heldFreshGlobal)
				}
				t.Logf("held finite traversal: namespaces=%d deniedKinds=%d LISTs=%d walks=%d; no fresh complete global LIST exists yet", len(namespaces), len(kinds), calls, len(walkStarts))
				releasedAt := time.Now().UTC()
				releaseTail()
				if incomplete {
					eventually(t, "invalid final LIST uncertainty admission", func() bool {
						_, err := service.Workloads(t.Context(), WorkloadListOptions{})
						return invalidFinal.Load() > 0 && (errors.Is(err, ErrInventoryNotReady) || errors.Is(err, ErrForbidden))
					})
					assertRetained()
					if health().State == "ready" {
						t.Fatal("incomplete final response claimed readiness")
					}
					t.Log("negative: malformed final LIST -> current authority uncertain; no head/checkpoint advancement")
					phase.Store(4)
				}
				// Only autonomous collection may satisfy this. No due-time edit,
				// cache invalidation, wake send or protected recovery read occurs here.
				eventuallyWithin(t, 80*time.Second, "finite tail and natural fresh global completion", func() bool {
					r := health()
					if r.State != "ready" || r.PendingKinds != 0 || r.FailingKinds != 0 || freshGlobal.Load() == 0 {
						return false
					}
					for _, kind := range []string{"Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "Event", "Node"} {
						state := r.Kinds[kind]
						if !state.Absent && (state.State != "ready" || state.Error != "") {
							return false
						}
					}
					return true
				})
				eventually(t, "new current capture journal", func() bool {
					cp, err := observed.EvidenceCheckpoint(t.Context(), evidenceScope)
					return err == nil && cp.ID != checkpoint.ID && cp.At.After(checkpoint.At)
				})
				after, currentHead, err := readInventoryGeneration(observed, scope, "")
				if err != nil || currentHead.Revision == head.Revision || !after.View.Kinds["Pod"].CompletedAt.After(releasedAt) {
					t.Fatalf("missing fresh committed global completion: %s %v", currentHead.Revision, err)
				}
				workloads, err = service.Workloads(t.Context(), WorkloadListOptions{})
				if err != nil || len(workloads.Items) == 0 {
					t.Fatalf("authorized Workloads did not recover: %d %v", len(workloads.Items), err)
				}
				issues, err := service.Issues(t.Context(), IssueOptions{Broken: true})
				if err != nil || len(issues.Items) == 0 {
					t.Fatalf("authorized live Issues did not recover: %d %v", len(issues.Items), err)
				}
				history, err = service.Issues(t.Context(), IssueOptions{Since: since, Until: time.Now().UTC()})
				if err != nil || len(history.Items) == 0 {
					t.Fatalf("current authorized history did not recover: %d %v", len(history.Items), err)
				}
				if !health().FirstReadyAt.Equal(initial.FirstReadyAt) || service.ActiveScope() != scope {
					t.Fatal("recovery changed immutable first-ready time or identity scope")
				}
				observed.mu.Lock()
				events := append([]string(nil), observed.events...)
				observed.mu.Unlock()
				tail, cas, journal := -1, -1, -1
				for i, event := range events {
					if event == "final-scoped-response-released" {
						tail = i
					}
					if tail >= 0 && i > tail && strings.HasPrefix(event, "data-head-CAS:") && cas < 0 {
						cas = i
					}
					if cas >= 0 && i > cas && strings.HasPrefix(event, "journal:") {
						journal = i
						break
					}
				}
				if tail < 0 || cas <= tail || journal <= cas {
					t.Fatalf("completion order lacks tail -> admitted data CAS -> journal: %v", events)
				}
				t.Logf("completion: tailEvent=%d headCAS=%d journal=%d freshGlobalLISTs=%d Workloads=%d Issues=%d history=%d elapsedAfterRelease=%s; final scoped proof is a publication precondition, not an added test hook", tail, cas, journal, freshGlobal.Load(), len(workloads.Items), len(issues.Items), len(history.Items), time.Since(releasedAt))

				// A separate synchronous boundary probe uses the actual newly
				// committed complete global states. It cannot wake or alter the
				// autonomous producer, and any redundant scoped I/O is observable.
				var redundant atomic.Int32
				probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redundant.Add(1)
					http.Error(w, "unexpected scoped request", http.StatusInternalServerError)
				}))
				defer probe.Close()
				probeClient, err := NewClient(Config{Endpoint: probe.URL, AllowLoopbackHTTP: true})
				if err != nil {
					t.Fatal(err)
				}
				probeService := NewService(probeClient, scope, time.Hour)
				fresh, err := cloneInventoryGeneration(after)
				if err != nil {
					t.Fatal(err)
				}
				freshKinds := []string{}
				for kind, state := range fresh.View.Kinds {
					if state.State == "ready" && !state.Partial && state.Error == "" && state.CompletedAt.After(releasedAt) {
						freshKinds = append(freshKinds, kind)
					}
				}
				if len(freshKinds) == 0 {
					t.Fatal("no real fresh complete global state for the skip discriminator")
				}
				probeCache := &inventoryCache{namespaces: wanted}
				if err := probeService.collectScopedInventory(t.Context(), probeCache, &fresh, freshKinds, time.Now().UTC()); err != nil || redundant.Load() != 0 {
					t.Fatalf("fresh global completion redundantly required namespace reads: count=%d err=%v", redundant.Load(), err)
				}
				t.Logf("separate complete-global boundary: freshKinds=%d subscribedNamespaces=%d redundantScopedLISTs=%d; no producer/fixture parameter changed", len(freshKinds), len(wanted), redundant.Load())
			})
		}
	}
}
