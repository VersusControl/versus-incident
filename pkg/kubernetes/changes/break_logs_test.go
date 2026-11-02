package changes

import (
	"errors"
	"fmt"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"testing"
	"time"
)

func TestSettleAdmissionSharedAcrossClusterAndUpgradeScopes(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	stores := []*Store{}
	for i, org := range []string{"default", "licensed", "licensed"} {
		stores = append(stores, NewStore(provider, index.Scope{OrgID: org, ClusterID: fmt.Sprint(i), CredentialID: "credential"}))
	}
	for i := 0; i < 2; i++ {
		if err := stores[i].AcquireBreakSettleSlot(fmt.Sprint(i), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := stores[2].AcquireBreakSettleSlot("third", now); err == nil {
		t.Fatal("third deployment-wide query admitted")
	}
	if err := stores[0].ReleaseBreakSettleSlot("unknown"); err != nil {
		t.Fatal(err)
	}
	if err := stores[2].AcquireBreakSettleSlot("third", now); err == nil {
		t.Fatal("unrelated release freed slot")
	}
	if err := stores[0].ReleaseBreakSettleSlot("0"); err != nil {
		t.Fatal(err)
	}
	if err := stores[2].AcquireBreakSettleSlot("third", now); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].AcquireBreakSettleSlot("restart", now.Add(time.Minute)); err != nil {
		t.Fatal("expired slot blocks restart", err)
	}
}

func TestBreakManifestUpgradeReadOrderAndPersistedSettleProgress(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	oldScope := index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"}
	old := NewStore(provider, oldScope)
	pod := breaklogs.ManifestPod{Namespace: "shop", RootUID: "root", Name: "api-0", UID: "gone", CreatedAt: now.Add(-time.Hour), LastSeen: now, Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
	if err := old.ObserveBreakPods([]breaklogs.ManifestPod{pod}, "", now); err != nil {
		t.Fatal(err)
	}
	manifest := breaklogs.Manifest{ID: "selected", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now, SettleAt: now, Pods: []breaklogs.ManifestPod{pod}}
	if err := old.CommitBreakLogs(nil, nil, "captured", "", now, manifest); err != nil {
		t.Fatal(err)
	}
	scope := oldScope
	scope.OrgID = "licensed"
	view, err := NewView(provider, scope, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := view.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := view.ObserveBreakPods(nil, "owner", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	state, err := view.BreakLogs(time.Now().UTC())
	if err != nil || len(state.Manifests) != 1 || len(state.PodHistory) != 1 || state.PodHistory[0].DeletedAt.IsZero() || state.Jobs["captured"].IsZero() {
		t.Fatal("upgrade lost default proof", state, err)
	}
	claimed, err := view.ClaimBreakSettle(manifest.ID, "owner", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := view.CommitBreakSettle(claimed, nil, false, "owner", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewView(provider, scope, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	state, err = restarted.BreakLogs(time.Now().UTC())
	if err != nil || len(state.Manifests) != 1 || state.Manifests[0].Attempts != 1 || !state.Manifests[0].Done {
		t.Fatal("archive rewound primary settle progress", state, err)
	}
	if _, err := restarted.ClaimBreakSettle(manifest.ID, "owner", now.Add(11*time.Minute)); err == nil {
		t.Fatal("default read requeued completed occurrence")
	}
}

func TestBreakLogsLeaseFencingPersistenceAndScopes(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	scope := index.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "credential"}
	store := NewStore(provider, scope)
	item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: "api-1", PodUID: "pod", Container: "app", Slot: "previous", From: now.Add(-time.Minute), To: now, CapturedAt: now, Lines: []breaklogs.Line{{At: now, Text: "safe"}}}
	item.ID = breaklogs.Identity(item)
	if err := store.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	for _, owner := range []string{"", "stale"} {
		if err := store.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", owner, now); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("stale write", err)
		}
	}
	if err := store.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", "owner", now); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(provider, scope)
	state, err := restarted.BreakLogs(now)
	if err != nil || len(state.Snapshots) != 1 || state.Snapshots[0].Lines[0].Text != "safe" {
		t.Fatal("persist", err)
	}
	for _, other := range []index.Scope{{OrgID: "other", ClusterID: "cluster", CredentialID: "credential"}, {OrgID: "org", ClusterID: "other", CredentialID: "credential"}, {OrgID: "org", ClusterID: "cluster", CredentialID: "other"}} {
		state, err := NewStore(provider, other).BreakLogs(now)
		if err != nil || len(state.Snapshots) != 0 {
			t.Fatal("scope leak", err)
		}
	}
}

func TestManifestSettleAttemptFenceSurvivesTakeover(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	store := NewStore(provider, index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"})
	manifest := breaklogs.Manifest{ID: "occurrence", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now, SettleAt: now}
	if err := store.ClaimLease("first", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := store.CommitBreakLogs(nil, nil, "", "first", now, manifest); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimBreakSettle("occurrence", "first", now)
	if err != nil || claimed.Attempts != 1 {
		t.Fatal(claimed, err)
	}
	if _, err := store.ClaimBreakSettle("occurrence", "first", now); err == nil {
		t.Fatal("claimed same attempt twice")
	}
	if err := store.ReleaseLease("first"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLease("second", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBreakSettle(claimed, nil, true, "first", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale query committed", err)
	}
	state, err := store.BreakLogs(now)
	if err != nil || state.Manifests[0].Attempts != 1 {
		t.Fatal(state, err)
	}
	retry, err := store.ClaimBreakSettle("occurrence", "second", now.Add(11*time.Minute))
	if err != nil || retry.Attempts != 2 {
		t.Fatal(retry, err)
	}
	if err := store.CommitBreakSettle(retry, nil, true, "second", now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimBreakSettle("occurrence", "second", now.Add(12*time.Minute)); err == nil {
		t.Fatal("third attempt admitted")
	}
}

func TestSettleCompletionRequiresCaptureEpochEvenWithReusedOwner(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"})
	manifest := breaklogs.Manifest{ID: "occurrence", Namespace: "shop", RootUID: "root", BreakAt: now, From: now.Add(-time.Minute), To: now, CapturedAt: now, SettleAt: now}
	if err := store.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBreakLogs(nil, nil, "", "owner", now, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimBreakSettle(manifest.ID, "owner", now)
	if err != nil {
		t.Fatal(err)
	}
	// Renewal retains the epoch; releasing and reclaiming the same token does not.
	if err := store.RenewLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseLease("owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBreakSettle(claim, nil, false, "owner", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale epoch completed", err)
	}
	state, err := store.BreakLogs(now)
	if err != nil || state.Manifests[0].Done || !state.Manifests[0].SettlingUntil.Equal(claim.SettlingUntil) {
		t.Fatal("rejected completion changed progress", state, err)
	}
}
