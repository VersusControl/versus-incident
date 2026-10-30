package kubernetes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Ownership proofs for a fleet with more than one replica. Two replicas share
// one store and both know every cluster: each cluster's generation has exactly
// one owner, ownership moves per cluster, and losing one cluster's ownership
// leaves the other cluster serving.

// principalCredential is one principal presenting a different token value on
// each replica, exactly as a rotated projected token would. The credential
// identity is the principal, so both replicas contend for the same scopes.
func principalCredential(principal, token string) CredentialSource {
	return &expiringBearerSource{principal: principal, now: time.Now, refresh: func(context.Context) (string, time.Time, error) {
		return token, time.Now().Add(time.Hour), nil
	}}
}

// leaseRecord reads the current inventory lease record for one scope.
func leaseRecord(t *testing.T, provider storage.Provider, scope Scope) inventoryLeaseRecord {
	t.Helper()
	raw, err := provider.ReadBlob(inventoryKey(scope))
	if err != nil || len(raw) == 0 {
		t.Fatalf("no inventory lease for cluster %s: %v", scope.ClusterID, err)
	}
	var record inventoryLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("inventory lease for cluster %s: %v", scope.ClusterID, err)
	}
	return record
}

// expireLease makes one cluster's lease claimable without publishing through
// the owner, so a proof can move exactly one cluster to the other replica.
func expireLease(t *testing.T, provider storage.Provider, scope Scope) {
	t.Helper()
	key := inventoryKey(scope)
	raw, err := provider.ReadBlob(key)
	if err != nil || len(raw) == 0 {
		t.Fatalf("no inventory lease for cluster %s: %v", scope.ClusterID, err)
	}
	var record inventoryLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record.Expires = time.Now().Add(-time.Minute)
	replacement, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob(key, replacement); err != nil {
		t.Fatal(err)
	}
}

// W6: two replicas over two clusters. Exactly one owner per cluster, ownership
// moves per cluster, and losing one cluster's ownership does not disturb the
// other cluster or its last-good generation.
func TestClusterOwnershipIsPerClusterAcrossReplicas(t *testing.T) {
	base := storage.NewMemory()
	broker := &inventoryBroker{}
	leader := newBootFleetWithBroker(t, "acme", base, "replica-1", broker)
	follower := newBootFleetWithBroker(t, "acme", base, "replica-2", broker)
	east := newFakeCluster(t, "east-token", "east-api")
	west := newFakeCluster(t, "west-token", "west-api")
	east.acceptToken("east-token-replica-2")
	west.acceptToken("west-token-replica-2")
	leaderEast := leader.add("east", east, principalCredential("fleet-reader", "east-token"), 0)
	leaderWest := leader.add("west", west, principalCredential("fleet-reader", "west-token"), 0)
	followerEast := follower.add("east", east, principalCredential("fleet-reader", "east-token-replica-2"), 0)
	followerWest := follower.add("west", west, principalCredential("fleet-reader", "west-token-replica-2"), 0)
	// Both replicas resolve the same cluster to the same scope, so they
	// contend for one lease per cluster rather than collecting twice.
	if leaderEast.key != followerEast.key || leaderWest.key != followerWest.key {
		t.Fatal("replicas derived different scopes for the same cluster")
	}
	if leaderEast.key == leaderWest.key {
		t.Fatal("two clusters share one inventory scope key")
	}

	origin := time.Now()
	leader.startWithin(2 * time.Second)
	leader.waitPublished(leaderEast, origin, 10*time.Second)
	leader.waitPublished(leaderWest, origin, 10*time.Second)
	follower.startWithin(2 * time.Second)
	// The follower hydrates both clusters from the shared store and publishes
	// neither of them.
	waitUntil(t, 10*time.Second, "the follower to serve both clusters from the store", func() bool {
		for _, cluster := range []*bootedCluster{followerEast, followerWest} {
			overview, err := follower.resolve(cluster).Overview(t.Context())
			if err != nil || overview.Pods != 1 {
				return false
			}
		}
		return true
	})
	if published := broker.replicas(leaderEast.key); len(published) != 1 || published[0] != "replica-1" {
		t.Fatalf("cluster east was published by %v, want exactly one owner", published)
	}
	if published := broker.replicas(leaderWest.key); len(published) != 1 || published[0] != "replica-1" {
		t.Fatalf("cluster west was published by %v, want exactly one owner", published)
	}
	leader.settle(leaderWest, 1200*time.Millisecond, 20*time.Second)
	eastLease := leaseRecord(t, base, leaderEast.service.ActiveScope())
	westLease := leaseRecord(t, base, leaderWest.service.ActiveScope())
	westBefore, ok := publishedGeneration(base, leaderWest.service.ActiveScope())
	if !ok {
		t.Fatal("cluster west published no generation")
	}
	westPublishes := broker.count(leaderWest.key)
	westLists := west.listCount("pods")
	westHealthBefore := buildSyncHealth(syncHealthKinds(westBefore), westBefore.FirstReadyAt)
	t.Logf("west baseline: revision=%s state=%s pending=%d failing=%d publications=%d podLists=%d", westBefore.Revision, westHealthBefore.State, westHealthBefore.PendingKinds, westHealthBefore.FailingKinds, westPublishes, westLists)

	// Only the east lease expires, and its owner can no longer claim it. The
	// follower must take east over without touching west.
	leader.view.refuse(leaderEast.key)
	expireLease(t, base, leaderEast.service.ActiveScope())
	waitUntil(t, 15*time.Second, "the follower to take over the expired cluster", func() bool {
		at, ok := broker.first("replica-2", leaderEast.key)
		return ok && !at.IsZero()
	})
	// Exactly one owner, and it moved for one cluster only.
	if published := broker.replicas(leaderEast.key); len(published) != 2 || published[0] != "replica-1" || published[1] != "replica-2" {
		t.Fatalf("cluster east ownership trail is %v", published)
	}
	if published := broker.replicas(leaderWest.key); len(published) != 1 || published[0] != "replica-1" {
		t.Fatalf("a takeover of one cluster disturbed another cluster's owner: %v", published)
	}
	if after := leaseRecord(t, base, leaderEast.service.ActiveScope()); after.Owner == eastLease.Owner {
		t.Fatal("the expired cluster kept its owner after takeover")
	}
	if after := leaseRecord(t, base, leaderWest.service.ActiveScope()); after.Owner != westLease.Owner {
		t.Fatal("another cluster's lease changed owner during the takeover")
	}
	// The cluster that stayed put is undisturbed: no republication, no relist.
	if publishes := broker.count(leaderWest.key); publishes != westPublishes {
		after, present := publishedGeneration(base, leaderWest.service.ActiveScope())
		afterHealth := buildSyncHealth(syncHealthKinds(after), after.FirstReadyAt)
		t.Fatalf("cluster west republished %d times during another cluster's takeover: present=%t beforeRev=%s afterRev=%s before=%s/pending%d/failing%d after=%s/pending%d/failing%d podListsBefore=%d after=%d ownerBefore=%s after=%s", publishes-westPublishes, present, westBefore.Revision, after.Revision, westHealthBefore.State, westHealthBefore.PendingKinds, westHealthBefore.FailingKinds, afterHealth.State, afterHealth.PendingKinds, afterHealth.FailingKinds, westLists, west.listCount("pods"), westLease.Owner, leaseRecord(t, base, leaderWest.service.ActiveScope()).Owner)
	}
	westAfter, ok := publishedGeneration(base, leaderWest.service.ActiveScope())
	if !ok || westAfter.Revision != westBefore.Revision {
		t.Fatal("another cluster's takeover rewrote cluster west's generation")
	}
	if lists := west.listCount("pods"); lists != westLists {
		t.Fatalf("another cluster's takeover re-listed cluster west %d times", lists-westLists)
	}
	// The replica that lost ownership keeps serving its last good generation
	// for that cluster, and keeps owning nothing it should not.
	overview, err := leader.resolve(leaderEast).Overview(t.Context())
	if err != nil || overview.Pods != 1 || overview.Sync.State == "syncing" {
		t.Fatalf("the replica that lost one cluster stopped serving its last good view: pods=%d state=%q err=%v", overview.Pods, overview.Sync.State, err)
	}
	overview, err = leader.resolve(leaderWest).Overview(t.Context())
	if err != nil || overview.Pods != 1 {
		t.Fatalf("the replica that lost one cluster stopped serving the other: pods=%d err=%v", overview.Pods, err)
	}
	// The new owner serves the same records it inherited, from its own read.
	overview, err = follower.resolve(followerEast).Overview(t.Context())
	if err != nil || overview.Pods != 1 {
		t.Fatalf("the new owner of one cluster cannot serve it: pods=%d err=%v", overview.Pods, err)
	}
	if names := podNames(westBefore); len(names) != 1 || names[0] != "west-api" {
		t.Fatalf("cluster west's generation is not its own: %v", names)
	}
}
