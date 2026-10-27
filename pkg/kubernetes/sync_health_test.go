package kubernetes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// publishInventoryKinds installs one published generation for the service's
// scope, the way the inventory worker publishes it.
func publishInventoryKinds(t *testing.T, service *Service, kinds map[string]kubeindex.KindStatus, pending []string) {
	t.Helper()
	done := make(chan struct{})
	close(done)
	service.changes.inventories[service.indexScope()] = &inventoryCache{
		generation: inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds}, Pending: pending},
		done:       done, cancel: func() {},
	}
}

// publishFailedInventory installs the state the inventory worker leaves behind
// after a cycle that failed as a whole: one last-good generation plus the
// cycle's failure class.
func publishFailedInventory(t *testing.T, service *Service, kinds map[string]kubeindex.KindStatus, class string) {
	t.Helper()
	done := make(chan struct{})
	close(done)
	service.changes.inventories[service.indexScope()] = &inventoryCache{
		generation:  inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds}},
		failed:      true,
		failedClass: class,
		done:        done, cancel: func() {},
	}
}

func readyKinds(now time.Time, kinds ...string) map[string]kubeindex.KindStatus {
	result := map[string]kubeindex.KindStatus{}
	for _, kind := range kinds {
		result[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: now}
	}
	return result
}

// fullySyncedKinds is a cluster whose every known kind has a ready listing, so a
// test can vary exactly one field and mean it.
func fullySyncedKinds(now time.Time) map[string]kubeindex.KindStatus {
	result := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		result[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: now, Records: 1}
	}
	return result
}

// demandIndex fills only the on-demand scope index, the way a capture, log, or
// alert read does, with no published generation at all.
func demandIndex(t *testing.T, service *Service, kinds ...string) {
	t.Helper()
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range kinds {
		if err := index.Ingest(kind, []kubeindex.Record{{Kind: kind, UID: "uid-" + kind, Name: "one-" + kind, Namespace: "shop"}}, true, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncHealthReportsKindsOutsideTheViewSet(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name  string
		kind  string
		state kubeindex.KindStatus
	}{
		{name: "statefulset budget", kind: "StatefulSet", state: kubeindex.KindStatus{State: "error", Partial: true, Error: "budget_exhausted", LastAttemptAt: now.Add(-2 * time.Minute)}},
		{name: "configmap rejected records", kind: "ConfigMap", state: kubeindex.KindStatus{State: "partial", Partial: true, Error: "record_rejected", Skipped: 3, Records: 7, ObservedAt: now, LastAttemptAt: now}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			kinds := fullySyncedKinds(now)
			kinds[test.kind] = test.state
			publishInventoryKinds(t, service, kinds, nil)

			health, err := service.SyncHealth(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			entry, known := health.Kinds[test.kind]
			if !known || entry.Error != test.state.Error || entry.State != test.state.State {
				t.Fatalf("failing kind missing from the complete status: %+v", entry)
			}
			problem, found := SyncKindProblem{}, false
			for _, candidate := range health.Failing {
				if candidate.Kind == test.kind {
					problem, found = candidate, true
				}
			}
			if !found || problem.Class != test.state.Error || problem.Since.IsZero() {
				t.Fatalf("failing kind not reported as a problem: %+v", health.Failing)
			}
			if test.state.Skipped > 0 && problem.Skipped != test.state.Skipped {
				t.Fatalf("skipped count lost: %+v", problem)
			}
			if health.State == "ready" || !health.Partial || health.FailingKinds != 1 || health.UnreadableKinds != 0 {
				t.Fatalf("aggregate stayed healthy: %+v", health)
			}
			// The narrow, per-view status the explorer used to gate on cannot see
			// it: this is the hole the complete status closes.
			_, narrowed, err := service.CachedIndexSnapshot(t.Context(), "Node", "Pod", "Deployment")
			if err != nil || narrowed.State != "ready" {
				t.Fatalf("narrow status = %+v err=%v", narrowed, err)
			}
		})
	}
}

// A kind the connector identity may not read is a capability gap, not a sync
// fault: it is reported on its own, never blocks a view, and the cluster still
// reaches ready with everything the identity is allowed to see.
func TestSyncHealthPermissionGapDoesNotBlock(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(now)
	kinds["Secret"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: now.Add(-90 * time.Second), FailingSince: now.Add(-90 * time.Second)}
	publishInventoryKinds(t, service, kinds, nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "ready" || health.Partial || health.FailingKinds != 0 || len(health.Failing) != 0 {
		t.Fatalf("a permission gap blocked the cluster: %+v", health)
	}
	if health.UnreadableKinds != 1 || len(health.Unreadable) != 1 || health.Unreadable[0].Kind != "Secret" || health.Unreadable[0].Class != "forbidden" {
		t.Fatalf("permission gap not reported: %+v", health.Unreadable)
	}
	if health.Unreadable[0].Since.IsZero() {
		t.Fatal("permission gap has no start time")
	}
	// The kind itself still says exactly what happened.
	if entry := health.Kinds["Secret"]; entry.State != "error" || entry.Error != "forbidden" {
		t.Fatalf("kind status = %+v", entry)
	}
	if health.ReadyKinds != len(indexedResourceIDs)-1 || health.TotalKinds != len(indexedResourceIDs) {
		t.Fatalf("counts = ready %d total %d", health.ReadyKinds, health.TotalKinds)
	}
	if encoded, err := json.Marshal(health); err == nil {
		t.Logf("permission gap payload: %s", encoded)
	}
}

// An invalid or revoked credential is not a missing permission. Reporting it as
// a capability gap hides a dead connector behind a cluster that looks ready.
func TestSyncHealthInvalidCredentialBlocks(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(now)
	kinds["ServiceAccount"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "unauthorized", LastAttemptAt: now, FailingSince: now}
	publishInventoryKinds(t, service, kinds, nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "error" || !health.Partial || health.FailingKinds != 1 || health.UnreadableKinds != 0 {
		t.Fatalf("a dead credential looked like a permission gap: %+v", health)
	}
	if len(health.Failing) != 1 || health.Failing[0].Kind != "ServiceAccount" || health.Failing[0].Class != "unauthorized" {
		t.Fatalf("credential failure not reported as a fault: %+v", health.Failing)
	}
}

// A cycle that fails as a whole says the connector could not read anything at
// all. That is a fault with the cycle's own class, never a per-kind capability
// gap marking every kind unreadable, which would leave the cluster preparing
// forever with nothing failing and nothing ready.
func TestSyncHealthCycleFailureIsAFault(t *testing.T) {
	for _, test := range []struct {
		name  string
		class string
	}{
		{name: "unauthorized", class: "unauthorized"},
		{name: "forbidden", class: "forbidden"},
		{name: "timeout", class: "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			publishFailedInventory(t, service, fullySyncedKinds(now), test.class)

			health, err := service.SyncHealth(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if health.State != "error" || !health.Partial {
				t.Fatalf("a cycle %s looked ready or preparing: %+v", test.class, health)
			}
			if health.FailingKinds != len(indexedResourceIDs) || health.UnreadableKinds != 0 {
				t.Fatalf("cycle failure counted as failing=%d unreadable=%d, want failing=%d unreadable=0", health.FailingKinds, health.UnreadableKinds, len(indexedResourceIDs))
			}
			if health.ReadyKinds != 0 {
				t.Fatalf("ready kinds during a failed cycle = %d", health.ReadyKinds)
			}
			for kind, entry := range health.Kinds {
				if entry.Error != test.class || entry.State != "error" {
					t.Fatalf("%s kept %q/%q instead of the cycle class", kind, entry.State, entry.Error)
				}
			}
		})
	}
}

// A cluster where nothing is readable at all is broken, not preparing: the
// aggregate must say so instead of polling for a generation that cannot come.
func TestSyncHealthNothingReadableIsAFailure(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: now, FailingSince: now}
	}
	publishInventoryKinds(t, service, kinds, nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "error" || !health.Partial || health.ReadyKinds != 0 || health.UnreadableKinds != len(kinds) {
		t.Fatalf("an unreadable cluster polled forever: state=%q ready=%d unreadable=%d", health.State, health.ReadyKinds, health.UnreadableKinds)
	}
	// Control: one kind still loading means the cluster is preparing, and a
	// capability gap must not be turned into a failure while it waits.
	kinds["Pod"] = kubeindex.KindStatus{State: "pending"}
	publishInventoryKinds(t, service, kinds, nil)
	health, err = service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "syncing" || health.Partial || health.PendingKinds != 1 || health.FailingKinds != 0 {
		t.Fatalf("a loading kind was reported as a failure: %+v", health)
	}
}

func TestSyncHealthIsBoundedWithManyKinds(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := readyKinds(now, "Node", "Pod")
	// Fill past the payload bound, and put the only failure at the very end of the
	// kind order so a naive truncation would drop it.
	for index := 0; index < 200; index++ {
		kinds["Synthetic"+strconv.Itoa(index)] = kubeindex.KindStatus{State: "ready", ObservedAt: now}
	}
	kinds["ZzRejected"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "record_rejected", LastAttemptAt: now, FailingSince: now}
	publishInventoryKinds(t, service, kinds, nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Kinds) != maxSyncHealthKinds || !health.Truncated || len(health.OmittedKinds) == 0 {
		t.Fatalf("payload not bounded: kinds=%d truncated=%v omitted=%d", len(health.Kinds), health.Truncated, len(health.OmittedKinds))
	}
	// Every known kind is accounted for, whether kept or omitted: the seeded set
	// plus every indexed kind it did not mention.
	wantTotal := len(kinds)
	for kind := range indexedResourceIDs {
		if _, seeded := kinds[kind]; !seeded {
			wantTotal++
		}
	}
	if health.TotalKinds != wantTotal {
		t.Fatalf("kind accounting: total=%d want=%d kept=%d omitted=%d", health.TotalKinds, wantTotal, len(health.Kinds), len(health.OmittedKinds))
	}
	if len(health.OmittedKinds) > maxSyncHealthOmitted || len(health.Failing) > maxSyncHealthProblems {
		t.Fatalf("bound lists unbounded: omitted=%d failing=%d", len(health.OmittedKinds), len(health.Failing))
	}
	if _, kept := health.Kinds["ZzRejected"]; !kept || health.FailingKinds != 1 || health.Failing[0].Kind != "ZzRejected" {
		t.Fatalf("the bound dropped a failing kind: kept=%v failing=%+v", kept, health.Failing)
	}
	encoded, err := json.Marshal(health)
	if err != nil || len(encoded) > 64<<10 {
		t.Fatalf("health payload bytes=%d err=%v", len(encoded), err)
	}
}

func TestSyncHealthMarksUnlistedKindsAsPending(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	publishInventoryKinds(t, service, readyKinds(now, "Node", "Pod", "Deployment"), nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Secret", "ConfigMap"} {
		if entry := health.Kinds[kind]; entry.State != "pending" {
			t.Fatalf("%s not reported as loading: %+v", kind, entry)
		}
		if isFailingSyncKind(health.Kinds[kind]) {
			t.Fatalf("%s pending kind counted as a failure", kind)
		}
	}
	wantPending := len(indexedResourceIDs) - 3
	if health.State != "syncing" || health.Partial || health.PendingKinds != wantPending || health.FailingKinds != 0 || health.TotalKinds != len(indexedResourceIDs) {
		t.Fatalf("loading aggregate = %+v", health)
	}
}

// A kind the cluster does not serve is reported, and cannot hold the gate back:
// the surface must be able to tell "not served" from "not loaded yet".
func TestSyncHealthSeparatesAbsentFromFailing(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(now)
	kinds["HTTPRoute"] = kubeindex.KindStatus{State: "ready", Absent: true}
	publishInventoryKinds(t, service, kinds, nil)

	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if entry := health.Kinds["HTTPRoute"]; !entry.Absent {
		t.Fatalf("absent kind lost its mark: %+v", entry)
	}
	if health.State != "ready" || health.Partial || health.FailingKinds != 0 || health.PendingKinds != 0 {
		t.Fatalf("an unserved kind held the gate back: %+v", health)
	}
	if health.Failing == nil {
		t.Fatal("the problem list must be present and empty, not null")
	}
}

func TestSyncHealthReadsOnlyThePublishedGeneration(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		http.Error(w, "unexpected cluster read", http.StatusInternalServerError)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	now := time.Now().UTC()
	kinds := fullySyncedKinds(now)
	kinds["Secret"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: now}
	publishInventoryKinds(t, service, kinds, nil)

	for attempt := 0; attempt < 5; attempt++ {
		health, err := service.SyncHealth(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if health.State != "ready" || health.UnreadableKinds != 1 {
			t.Fatalf("health = %+v", health)
		}
	}
	if reads.Load() != 0 {
		t.Fatalf("complete status caused %d cluster reads", reads.Load())
	}
}

// A cluster that has not published a generation yet has nothing ready. Reporting
// it as ready would open every data view on an empty cluster, so an empty
// published set is still loading until it says otherwise.
func TestSyncHealthWithoutAPublishedGenerationIsNotReady(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatalf("empty generation: %v", err)
	}
	if health.State != "syncing" || health.Partial {
		t.Fatalf("empty status must be loading, not ready: %+v", health.State)
	}
	if health.Kinds == nil || health.Failing == nil {
		t.Fatalf("empty status must still carry both collections")
	}
	if health.TotalKinds != len(indexedResourceIDs) || health.ReadyKinds != 0 || health.FailingKinds != 0 || health.PendingKinds != len(indexedResourceIDs) {
		t.Fatalf("empty status counts: total=%d ready=%d failing=%d pending=%d", health.TotalKinds, health.ReadyKinds, health.FailingKinds, health.PendingKinds)
	}
}

// The failure class is what a surface turns into plain words, so the complete
// status has to carry it for kinds no view asked about.
func TestSyncHealthKeepsTheFailureClassAndAge(t *testing.T) {
	attempt := time.Now().UTC().Add(-4 * time.Minute)
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(time.Now().UTC())
	kinds["StatefulSet"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "budget_exhausted", LastAttemptAt: attempt}
	publishInventoryKinds(t, service, kinds, nil)
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	problem := health.Failing[0]
	if problem.Kind != "StatefulSet" || problem.Class != "budget_exhausted" || !problem.Since.Equal(attempt) {
		t.Fatalf("problem = %+v", problem)
	}
	if age := time.Since(problem.Since); age < 3*time.Minute {
		t.Fatalf("problem since is not the failing attempt: %s", age)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":"error"`, `"failing_kinds":1`, `"class":"budget_exhausted"`, `"since":"`, `"kinds":{`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("payload %s lacks %s", encoded, want)
		}
	}
}

// A capture, log, or alert read fills the on-demand scope index without ever
// publishing a generation. One demand-loaded kind must not report the cluster
// ready, or the gate would open on views that have no inventory behind them.
func TestSyncHealthIgnoresTheDemandIndexBeforeAnyGeneration(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	for _, kind := range []string{"Pod", "Node"} {
		demandIndex(t, service, kind)
	}
	demanded, _, err := service.CachedIndexSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, onDemand := demanded.Kinds["Pod"]; !onDemand {
		t.Fatal("fixture did not fill the on-demand index")
	}
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State == "ready" {
		t.Fatalf("a demand-loaded kind reported the cluster ready: %+v", health)
	}
	if health.State != "syncing" || health.PendingKinds == 0 || health.TotalKinds != len(indexedResourceIDs) {
		t.Fatalf("demand-only status = %+v", health)
	}
	if entry := health.Kinds["Secret"]; entry.State != "pending" {
		t.Fatalf("an unlisted kind was not reported as loading: %+v", entry)
	}
}

// A kind that lost records but reported no class is still failing: the gate must
// not open on data we know is incomplete. This is the one rule the UI mirrors.
func TestSyncHealthCountsClasslessPartialAsFailing(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(now)
	kinds["Pod"] = kubeindex.KindStatus{State: "ready", Partial: true, Records: 50, ObservedAt: now, LastAttemptAt: now}
	publishInventoryKinds(t, service, kinds, nil)
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "partial" || !health.Partial || health.FailingKinds != 1 || len(health.Failing) != 1 || health.Failing[0].Kind != "Pod" {
		t.Fatalf("a class-less partial kind did not keep the gate closed: %+v", health)
	}
	if !isFailingSyncKind(health.Kinds["Pod"]) {
		t.Fatal("the per-kind predicate disagrees with the aggregate")
	}
	// A kind that is partial only because it is still loading is not a failure.
	if isFailingSyncKind(kubeindex.KindStatus{State: "pending", Partial: true}) || isFailingSyncKind(kubeindex.KindStatus{State: "ready", Absent: true}) {
		t.Fatal("loading or unserved kinds counted as failures")
	}
}

// The failing-kind loop must respect the payload bound, not only the fill loop.
func TestBoundedSyncKindsCapsFailingKinds(t *testing.T) {
	all := map[string]kubeindex.KindStatus{}
	names := make([]string, 0, 200)
	for index := 0; index < 200; index++ {
		kind := "Kind" + strconv.Itoa(index)
		all[kind] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden"}
		names = append(names, kind)
	}
	kept, omitted := boundedSyncKinds(names, all)
	if len(kept) != maxSyncHealthKinds || len(omitted) == 0 {
		t.Fatalf("bounded kinds=%d omitted=%d", len(kept), len(omitted))
	}
}

// A first sync reports every kind as pending, and no pending kind carries the
// partial flag: the wire shape alone has to say "loading", because a consumer
// that checks partial before state would otherwise show a fresh cluster as
// degraded with every kind failed.
func TestSyncHealthFirstSyncPayloadCarriesNoPartialKind(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "syncing" || health.Partial || health.FailingKinds != 0 || len(health.Failing) != 0 {
		t.Fatalf("first sync aggregate = %+v", health)
	}
	for kind, entry := range health.Kinds {
		if entry.State != "pending" {
			t.Fatalf("%s is %q on a first sync", kind, entry.State)
		}
		if entry.Partial {
			t.Fatalf("%s carries partial while loading: %+v", kind, entry)
		}
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"partial":true`) {
		t.Fatalf("first sync payload contains a partial kind: %s", encoded)
	}
	t.Logf("first sync payload: %s", encoded)
	// A genuinely partial served kind still says so, and is still named.
	kinds := fullySyncedKinds(time.Now().UTC())
	kinds["Pod"] = kubeindex.KindStatus{State: "partial", Partial: true, Error: "record_rejected", Skipped: 2, Records: 8, ObservedAt: time.Now().UTC()}
	publishInventoryKinds(t, service, kinds, nil)
	health, err = service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !health.Partial || health.State != "partial" || health.FailingKinds != 1 || !health.Kinds["Pod"].Partial {
		t.Fatalf("a real partial lost its report: %+v", health.Kinds["Pod"])
	}
}

// A failing kind's since is the start of the failure, and a timestamp that was
// never observed is omitted rather than rendered as a zero date.
func TestSyncHealthFailureRowsCarryStableTimesOrNone(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	kinds := fullySyncedKinds(now)
	kinds["StatefulSet"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "budget_exhausted", LastAttemptAt: now.Add(3 * time.Minute), FailingSince: now}
	kinds["Secret"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: now.Add(3 * time.Minute), FailingSince: now.Add(-time.Hour)}
	publishInventoryKinds(t, service, kinds, nil)
	health, err := service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !health.Failing[0].Since.Equal(now) {
		t.Fatalf("failing since = %s, want the first failure %s", health.Failing[0].Since, now)
	}
	if !health.Unreadable[0].Since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("unreadable since = %s", health.Unreadable[0].Since)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "0001-01-01") {
		t.Fatalf("a zero timestamp reached the wire: %s", encoded)
	}
	// A row with nothing to say about time omits both fields entirely.
	never := syncProblemRow("Pod", kubeindex.KindStatus{State: "error", Error: "timeout"})
	row, err := json.Marshal(never)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row), "since") || strings.Contains(string(row), "observed_at") {
		t.Fatalf("a row without times rendered them: %s", row)
	}
}

// durableSyncCluster is one cluster on a shared store, with a publishing service
// and its lease owner.
type durableSyncCluster struct {
	provider storage.Provider
	scope    Scope
	service  *Service
	owner    string
}

func newDurableSyncCluster(t *testing.T) *durableSyncCluster {
	t.Helper()
	scope := Scope{OrgID: "default", ClusterID: "cluster"}
	service := NewService(nil, scope, 0)
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	return &durableSyncCluster{provider: provider, scope: scope, service: service, owner: "publishing-owner"}
}

// publish writes one generation through the lease publish, the only path that
// makes a generation durable for a later report or a restarted process.
func (cluster *durableSyncCluster) publish(t *testing.T, generation inventoryGeneration) inventoryGeneration {
	t.Helper()
	return publishDurableGeneration(t, cluster.provider, cluster.service, cluster.owner, generation)
}

// report reads the health report the way a process that has not synced anything
// does: no in-memory generation, only what the store already holds.
func (cluster *durableSyncCluster) report(t *testing.T) SyncHealth {
	t.Helper()
	reader := NewService(nil, cluster.scope, 0)
	reader.SetChangeStorage(cluster.provider)
	health, err := reader.SyncHealth(t.Context())
	if err != nil {
		t.Fatalf("report from store: %v", err)
	}
	return health
}

func publishDurableGeneration(t *testing.T, provider storage.Provider, service *Service, owner string, generation inventoryGeneration) inventoryGeneration {
	t.Helper()
	lease, owned, err := service.claimInventory(t.Context(), provider, owner)
	if err != nil || !owned {
		t.Fatalf("claim by %s: owned=%v err=%v", owner, owned, err)
	}
	if err := lease.publish(t.Context(), service, &generation); err != nil {
		t.Fatalf("publish by %s: %v", owner, err)
	}
	return generation
}

// The first ready time is recorded once, by the first generation the report calls
// ready, and no later ready report restamps it. Until then the field is absent
// from the payload, so a surface can still tell a first boot from a re-sync.
func TestSyncHealthFirstReadyIsStampedOnceByTheFirstReadyGeneration(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)

	neverReady := cluster.report(t)
	if !neverReady.FirstReadyAt.IsZero() {
		t.Fatalf("a cluster that was never ready carries %s", neverReady.FirstReadyAt)
	}
	encoded, err := json.Marshal(neverReady)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "first_ready_at") {
		t.Fatalf("a cluster that was never ready rendered a first ready time: %s", encoded)
	}

	// A generation that is still filling in is not ready, so it must not end the
	// preparing state before the cluster was ever ready.
	filling := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: readyKinds(now, "Pod")}})
	if !filling.FirstReadyAt.IsZero() {
		t.Fatalf("a syncing generation recorded first ready %s", filling.FirstReadyAt)
	}
	if health := cluster.report(t); health.State != "syncing" || !health.FirstReadyAt.IsZero() {
		t.Fatalf("syncing report = %q first ready %s", health.State, health.FirstReadyAt)
	}

	first := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}})
	if first.FirstReadyAt.IsZero() {
		t.Fatal("the first ready generation recorded no first ready time")
	}
	health := cluster.report(t)
	if health.State != "ready" || !health.FirstReadyAt.Equal(first.FirstReadyAt) {
		t.Fatalf("report = state %q first ready %s, want ready at %s", health.State, health.FirstReadyAt, first.FirstReadyAt)
	}
	for later := 1; later <= 3; later++ {
		repeated := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now.Add(time.Duration(later) * time.Minute))}})
		if !repeated.FirstReadyAt.Equal(first.FirstReadyAt) {
			t.Fatalf("ready report %d restamped first ready to %s, want %s", later, repeated.FirstReadyAt, first.FirstReadyAt)
		}
	}
	if health := cluster.report(t); !health.FirstReadyAt.Equal(first.FirstReadyAt) {
		t.Fatalf("first ready moved to %s after repeated ready reports, want %s", health.FirstReadyAt, first.FirstReadyAt)
	}
	encoded, err = json.Marshal(cluster.report(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"first_ready_at":`) {
		t.Fatalf("a recorded first ready time never reached the payload: %s", encoded)
	}
}

// Once a cluster has been ready, a later degraded report still carries that
// moment: a syncing, partial, stale, or dead-credential cluster has been ready
// before, and it must not look like a first boot again.
func TestSyncHealthFirstReadySurvivesDegradedReports(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt

	for _, test := range []struct {
		name string
		want string
		with func(map[string]kubeindex.KindStatus)
	}{
		{name: "partial", want: "partial", with: func(kinds map[string]kubeindex.KindStatus) {
			kinds["Pod"] = kubeindex.KindStatus{State: "partial", Partial: true, Error: "record_rejected", Records: 8, Skipped: 2, ObservedAt: now, LastAttemptAt: now}
		}},
		{name: "stale", want: "stale", with: func(kinds map[string]kubeindex.KindStatus) {
			for kind := range kinds {
				kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-2 * time.Hour)}
			}
		}},
		{name: "error", want: "error", with: func(kinds map[string]kubeindex.KindStatus) {
			kinds["StatefulSet"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "timeout", LastAttemptAt: now, FailingSince: now}
		}},
		{name: "unreadable credential", want: "error", with: func(kinds map[string]kubeindex.KindStatus) {
			kinds["ServiceAccount"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "unauthorized", LastAttemptAt: now, FailingSince: now}
		}},
		{name: "nothing readable", want: "error", with: func(kinds map[string]kubeindex.KindStatus) {
			for kind := range kinds {
				kinds[kind] = kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: now, FailingSince: now}
			}
		}},
		{name: "re-indexed", want: "syncing", with: func(kinds map[string]kubeindex.KindStatus) {
			// A re-index publishes only the kinds it has listed so far; every
			// other known kind is loading again, not failed.
			for kind := range kinds {
				if kind != "Pod" {
					delete(kinds, kind)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			kinds := fullySyncedKinds(now)
			test.with(kinds)
			cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: kinds}})

			health := cluster.report(t)
			if health.State != test.want {
				t.Fatalf("state = %q, want %q: %+v", health.State, test.want, health)
			}
			if !health.FirstReadyAt.Equal(stamped) {
				t.Fatalf("%s report carries first ready %s, want the recorded %s", test.name, health.FirstReadyAt, stamped)
			}
			for _, row := range append(append([]SyncKindProblem{}, health.Failing...), health.Unreadable...) {
				encoded, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "first_ready_at") {
					t.Fatalf("the cluster's first ready time leaked into a problem row: %s", encoded)
				}
			}
		})
	}
}

// The first ready time is a property of the cluster, not of the process: a versus
// restart on the same store still reports it, even when the cluster is failing by
// then, and the process that reads it has synced nothing itself.
func TestSyncHealthFirstReadySurvivesRestart(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt

	failing := fullySyncedKinds(now)
	failing["Pod"] = kubeindex.KindStatus{State: "error", Partial: true, Error: "throttled", LastAttemptAt: now, FailingSince: now}
	cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: failing}})

	persisted, ok := publishedGeneration(cluster.provider, cluster.scope)
	if !ok || !persisted.FirstReadyAt.Equal(stamped) {
		t.Fatalf("the stored generation carries first ready %s, want %s", persisted.FirstReadyAt, stamped)
	}
	health := cluster.report(t)
	if health.State != "error" || !health.FirstReadyAt.Equal(stamped) {
		t.Fatalf("after restart state = %q first ready = %s, want error at %s", health.State, health.FirstReadyAt, stamped)
	}
}

// The recorded moment is never replaced by the moment of a later publish, and a
// second replica that takes the lease over and republishes what it read back
// keeps it, so the value can only ever be the cluster's first ready moment.
func TestSyncHealthFirstReadyNeverMovesBackwards(t *testing.T) {
	now := time.Now().UTC()

	t.Run("an already recorded moment is kept", func(t *testing.T) {
		cluster := newDurableSyncCluster(t)
		earlier := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		carried := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}, FirstReadyAt: earlier})
		if !carried.FirstReadyAt.Equal(earlier) {
			t.Fatalf("first ready = %s, want the recorded %s", carried.FirstReadyAt, earlier)
		}
		if health := cluster.report(t); !health.FirstReadyAt.Equal(earlier) {
			t.Fatalf("report first ready = %s, want %s", health.FirstReadyAt, earlier)
		}
	})

	t.Run("a rebuilt generation keeps the stored moment", func(t *testing.T) {
		cluster := newDurableSyncCluster(t)
		stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt
		expireInventoryLease(t, cluster.provider, cluster.scope)
		// A re-index hands the lease a generation built from scratch rather than
		// the one it read back: the cluster's own record still decides.
		rebuilt := publishDurableGeneration(t, cluster.provider, cluster.service, "reindexing-replica", inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}})
		if !rebuilt.FirstReadyAt.Equal(stamped) {
			t.Fatalf("rebuild restamped first ready to %s, want the stored %s", rebuilt.FirstReadyAt, stamped)
		}
		if health := cluster.report(t); !health.FirstReadyAt.Equal(stamped) {
			t.Fatalf("report after rebuild first ready = %s, want %s", health.FirstReadyAt, stamped)
		}
	})

	t.Run("a takeover keeps the stored moment", func(t *testing.T) {
		cluster := newDurableSyncCluster(t)
		stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt
		expireInventoryLease(t, cluster.provider, cluster.scope)
		persisted, ok := publishedGeneration(cluster.provider, cluster.scope)
		if !ok || !persisted.FirstReadyAt.Equal(stamped) {
			t.Fatalf("stored generation carries first ready %s, want %s", persisted.FirstReadyAt, stamped)
		}
		took := publishDurableGeneration(t, cluster.provider, cluster.service, "second-replica", persisted)
		if !took.FirstReadyAt.Equal(stamped) || took.FirstReadyAt.After(time.Now()) {
			t.Fatalf("takeover moved first ready to %s, want %s", took.FirstReadyAt, stamped)
		}
		if health := cluster.report(t); !health.FirstReadyAt.Equal(stamped) {
			t.Fatalf("report after takeover first ready = %s, want %s", health.FirstReadyAt, stamped)
		}
	})
}

// stripStoredFirstReady rewrites the published generation without its own copy of
// the first ready moment, the way a generation written before the cluster's
// metadata carried it looks.
func stripStoredFirstReady(t *testing.T, provider storage.Provider, scope Scope) {
	t.Helper()
	record := leaseRecord(t, provider, scope)
	key, err := inventoryDataKey(scope, record)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := provider.ReadBlob(key)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	delete(payload, "FirstReadyAt")
	replacement, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob(key, replacement); err != nil {
		t.Fatal(err)
	}
}

// A stored generation can be older than the cluster's own memory of being ready:
// the metadata record is the authority, so the report still carries the moment
// when the generation payload does not.
func TestSyncHealthFirstReadyComesFromTheClusterMetadata(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt

	stripStoredFirstReady(t, cluster.provider, cluster.scope)

	health := cluster.report(t)
	if health.State != "ready" || !health.FirstReadyAt.Equal(stamped) {
		t.Fatalf("report = state %q first ready %s, want ready at %s", health.State, health.FirstReadyAt, stamped)
	}
}

// A restarted process seeds its worker from the cluster's stored generation
// before it lists anything, so its report already carries the first ready moment
// even though this process has synced nothing and cannot reach the cluster at all.
// That is what keeps the preparing screen to a boot.
func TestSyncHealthFirstReadySurvivesRestartWithTheWorkerRunning(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt

	// The stored generation is older than the cluster's memory of being ready, so
	// only the cluster's own metadata can fill the report in.
	stripStoredFirstReady(t, cluster.provider, cluster.scope)
	restarted := NewService(nil, cluster.scope, 0)
	restarted.SetChangeStorage(cluster.provider)
	cache := restarted.namespaceCache()
	cache.mu.Lock()
	cache.started = true
	cache.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go restarted.inventoryWorker(ctx, cache)

	deadline := time.Now().Add(5 * time.Second)
	for {
		health, err := restarted.SyncHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !health.FirstReadyAt.IsZero() {
			if health.State != "ready" || !health.FirstReadyAt.Equal(stamped) {
				t.Fatalf("restarted report = state %q first ready %s, want ready at %s", health.State, health.FirstReadyAt, stamped)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted process never read the cluster's first ready moment: %+v", health)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// revokedCredentialSource is a connector identity that dies after it has worked:
// every authorization attempt fails, which is what a retired or expired
// credential does to a service that was reading the cluster a moment ago.
type revokedCredentialSource struct{ cause error }

func (source *revokedCredentialSource) Authorization(context.Context) (string, error) {
	return "", source.cause
}

func (source *revokedCredentialSource) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return nil, source.cause
}

// revokedCredentialService is a service whose credential is already dead, so the
// health read fails before any inventory is looked at.
func revokedCredentialService(t *testing.T, scope Scope) *Service {
	t.Helper()
	client, err := NewClient(Config{
		Endpoint:    "https://cluster.example.invalid:6443",
		Credentials: &revokedCredentialSource{cause: errors.New(`token "canary-revoked-2f81" was retired`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(client, scope, 0)
}

// assertBoundedNonSensitiveSyncReport holds a failure report to the same promise
// as any other Kubernetes payload: it is bounded, and it carries no identity,
// credential, endpoint, or object name.
func assertBoundedNonSensitiveSyncReport(t *testing.T, report SyncHealth) {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 64<<10 {
		t.Fatalf("failure report is %d bytes, past any surface's bound", len(encoded))
	}
	for _, canary := range []string{"canary-object-name", "canary-namespace", "canary-revoked-2f81", "cluster.example.invalid", "uid-canary"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("failure report carries %q: %s", canary, encoded)
		}
	}
}

// The durable marker exists for exactly this fault. A cluster that was ready and
// whose credential then died still has to say it was ready before, so the read
// that fails answers with a report instead of dropping the one fact that
// separates a broken connector from a cluster that never worked.
func TestSyncHealthKeepsFirstReadyWhenTheCredentialDies(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{
		Kinds:   fullySyncedKinds(now),
		Records: []kubeindex.Record{{Kind: "Pod", UID: "uid-canary", Name: "canary-object-name", Namespace: "canary-namespace"}},
	}}).FirstReadyAt

	revoked := revokedCredentialService(t, cluster.scope)
	revoked.SetChangeStorage(cluster.provider)
	_, err := revoked.SyncHealth(t.Context())
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("dead credential error = %v, want the credential fault", err)
	}
	report, ok := SyncHealthReport(err)
	if !ok {
		t.Fatal("a dead credential lost the report instead of answering with it")
	}
	if report.State != "error" || !report.Partial || report.Class != "unauthorized" {
		t.Fatalf("report = state %q partial %t class %q, want an unauthorized error", report.State, report.Partial, report.Class)
	}
	if !report.FirstReadyAt.Equal(stamped) {
		t.Fatalf("first ready = %s, want the recorded %s", report.FirstReadyAt, stamped)
	}
	// A connector that read nothing proves nothing about what it may read, so
	// every kind is a fault of the whole cycle rather than a capability gap.
	if report.FailingKinds != len(indexedResourceIDs) || report.UnreadableKinds != 0 {
		t.Fatalf("failing = %d, unreadable = %d, want every known kind failing", report.FailingKinds, report.UnreadableKinds)
	}
	if pod := report.Kinds["Pod"]; pod.State != "error" || pod.Error != "unauthorized" || !pod.CycleFailure {
		t.Fatalf("Pod = %+v, want a cycle failure in the unauthorized class", pod)
	}
	assertBoundedNonSensitiveSyncReport(t, report)
}

// A cluster that was never ready must not acquire a marker from the failure: the
// report omits it, and the read writes nothing to the cluster's record.
func TestSyncHealthDeadCredentialNeverFabricatesFirstReady(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: readyKinds(now, "Pod")}})

	revoked := revokedCredentialService(t, cluster.scope)
	revoked.SetChangeStorage(cluster.provider)
	_, err := revoked.SyncHealth(t.Context())
	report, ok := SyncHealthReport(err)
	if !ok {
		t.Fatalf("dead credential error = %v, want a report", err)
	}
	if report.State != "error" || report.Class != "unauthorized" {
		t.Fatalf("report = %+v, want an unauthorized error", report)
	}
	if !report.FirstReadyAt.IsZero() {
		t.Fatalf("a cluster that was never ready reported first ready %s", report.FirstReadyAt)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "first_ready_at") {
		t.Fatalf("a never ready cluster rendered a first ready time: %s", encoded)
	}
	raw, err := cluster.provider.ReadBlob(inventoryKey(cluster.scope))
	if err != nil {
		t.Fatal(err)
	}
	var record inventoryLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if !record.FirstReadyAt.IsZero() {
		t.Fatalf("the failed read stamped the cluster record at %s", record.FirstReadyAt)
	}
}

// A cluster with no store keeps no record, so the published generation in memory
// is the only local copy of the marker. A credential that dies mid-session still
// gets it back.
func TestSyncHealthMidSessionRevocationKeepsFirstReadyFromMemory(t *testing.T) {
	now := time.Now().UTC()
	stamped := now.Add(-time.Hour)
	service := revokedCredentialService(t, Scope{OrgID: "default", ClusterID: "cluster"})
	done := make(chan struct{})
	close(done)
	service.changes.inventories[service.indexScope()] = &inventoryCache{
		generation: inventoryGeneration{FirstReadyAt: stamped, View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}},
		done:       done, cancel: func() {},
	}

	_, err := service.SyncHealth(t.Context())
	report, ok := SyncHealthReport(err)
	if !ok {
		t.Fatalf("dead credential error = %v, want a report", err)
	}
	if report.State != "error" || report.Class != "unauthorized" || !report.FirstReadyAt.Equal(stamped) {
		t.Fatalf("report = %+v, want an unauthorized error at the recorded %s", report, stamped)
	}
}

// A cluster that can be read is reported exactly as before: the failure class is
// absent from the payload, not present and empty.
func TestSyncHealthReadyReportCarriesNoFailureClass(t *testing.T) {
	now := time.Now().UTC()
	cluster := newDurableSyncCluster(t)
	stamped := cluster.publish(t, inventoryGeneration{View: kubeindex.Snapshot{Kinds: fullySyncedKinds(now)}}).FirstReadyAt

	health := cluster.report(t)
	if health.State != "ready" || health.Partial || health.Class != "" || !health.FirstReadyAt.Equal(stamped) {
		t.Fatalf("ready report = %+v, want ready at %s with no failure", health, stamped)
	}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"class"`) {
		t.Fatalf("a healthy report carried a failure class: %s", encoded)
	}
}
