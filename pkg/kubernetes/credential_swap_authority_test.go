package kubernetes

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// swapCredential fences every read made before it, as a credential
// replacement does, so the next cycle runs under a new binding.
func (cluster *partialRoleCluster) swapCredential() {
	cluster.service.client.authGeneration.Add(1)
}

func (cluster *partialRoleCluster) recordsOf(kind string) int {
	cluster.t.Helper()
	view, _, err := cluster.service.CachedIndexSnapshot(cluster.t.Context(), kind)
	if err != nil {
		return 0
	}
	count := 0
	for _, record := range view.Records {
		if record.Kind == kind {
			count++
		}
	}
	return count
}

// A new credential that cannot read a kind the old one read must not inherit
// the old credential's records of that kind, even though its denial is a
// capability gap of the new binding and not a lost authority.
func TestSwappedCredentialDoesNotServeRecordsItCannotRead(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	if state := cluster.committed().View.Kinds["Secret"]; state.State != "ready" || state.Error != "" {
		t.Fatalf("Secret was not readable under the first credential: %+v", state)
	}
	if cluster.recordsOf("Secret") == 0 {
		t.Fatal("fixture holds no Secret records read by the first credential")
	}
	cluster.swapCredential()
	cluster.denySecrets.Store(true)
	cluster.cycle(nil, "Deployment")
	if state := cluster.committed().View.Kinds["Secret"]; state.Error != "forbidden" {
		t.Fatalf("the new credential's Secret denial was not published: %+v", state)
	}
	if served := cluster.recordsOf("Secret"); served != 0 {
		t.Fatalf("%d Secret records read by the replaced credential were served to the new one", served)
	}
	if err := cluster.readable(); err != nil {
		t.Fatalf("the new credential's readable kinds were hidden: %v", err)
	}
	if failure := cluster.committed().EvidenceFailure; failure != "" || strings.Contains(cluster.refusal(), "authority_unverified") {
		t.Fatalf("a capability gap of the new credential refused the capture: class=%s refusal=%s", failure, cluster.refusal())
	}
}

// A new credential that can read nothing has no baseline to call its denials
// gaps, so the proof runs, and when it fails the old data is hidden and no
// capture is saved.
func TestSwappedCredentialThatReadsNothingStillRunsTheProof(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	committed := cluster.committed()
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "partial-role", ClusterUID: "partial-role-physical"}
	store := cluster.provider.(storage.EvidenceStore)
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	cluster.cache.authorityProof.holdProbe.Store(&hold)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	checks := cluster.cache.authorityChecks.Load()
	cluster.swapCredential()
	cluster.denyAll.Store(true)
	cluster.force(nil)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("a new credential that read nothing did not start an authority proof")
	}
	ctx := t.Context()
	if workloads, _ := cluster.service.Workloads(ctx, WorkloadListOptions{}); len(workloads.Items) != 0 {
		t.Fatal("workloads read by the replaced credential were served while the proof was pending")
	}
	released = true
	close(release)
	waitUntil(t, 10*time.Second, "failed proof to fail the cycle", func() bool {
		cluster.cache.mu.RLock()
		defer cluster.cache.mu.RUnlock()
		return cluster.cache.failed
	})
	if cluster.cache.authorityChecks.Load() == checks {
		t.Fatal("a new credential that read nothing ran no authority check")
	}
	denied, err := cluster.service.Workloads(ctx, WorkloadListOptions{})
	if !errors.Is(err, ErrForbidden) || len(denied.Items) != 0 {
		t.Fatalf("data stayed readable after the proof failed: items=%d err=%v", len(denied.Items), err)
	}
	if served := cluster.recordsOf("Secret"); served != 0 {
		t.Fatalf("%d Secret records stayed readable after the proof failed", served)
	}
	if cluster.committed().Revision != committed.Revision {
		t.Fatal("a failed proof published a new generation")
	}
	current, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil || current.ID != checkpoint.ID {
		t.Fatalf("a failed proof saved a capture: %+v err=%v", current, err)
	}
}

// A new credential denied on every LIST has no baseline, so nothing read by
// the replaced credential may be served from the moment the cluster-wide LISTs
// are denied, not only once the proof begins after the namespace stage.
func TestSwappedCredentialThatReadsNothingWithholdsOldRecordsBeforeTheProof(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	if cluster.recordsOf("Pod") == 0 {
		t.Fatal("fixture holds no Pod records read by the first credential")
	}
	scoped := make(chan struct{}, 1)
	releaseScoped := make(chan struct{})
	holdScoped := func() {
		select {
		case scoped <- struct{}{}:
		default:
		}
		<-releaseScoped
	}
	proof := make(chan struct{}, 1)
	releaseProof := make(chan struct{})
	holdProof := func() {
		select {
		case proof <- struct{}{}:
		default:
		}
		<-releaseProof
	}
	cluster.holdScoped.Store(&holdScoped)
	cluster.cache.authorityProof.holdProbe.Store(&holdProof)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cluster.holdScoped.Store(nil)
			close(releaseScoped)
			close(releaseProof)
		})
	}
	defer release()
	cluster.swapCredential()
	cluster.denyAll.Store(true)
	cluster.force(nil)
	select {
	case <-scoped:
	case <-proof:
		t.Fatal("the proof started before the namespace stage the test holds")
	case <-time.After(10 * time.Second):
		t.Fatal("collection never reached its namespace stage")
	}
	for i := 0; i < 20; i++ {
		if served := cluster.recordsOf("Pod"); served != 0 {
			t.Fatalf("%d Pod records read by the replaced credential were served after every LIST of the new one was denied", served)
		}
		if workloads, _ := cluster.service.Workloads(t.Context(), WorkloadListOptions{}); len(workloads.Items) != 0 {
			t.Fatal("workloads read by the replaced credential were served before the proof")
		}
	}
	release()
}
