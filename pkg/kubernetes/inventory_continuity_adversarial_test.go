package kubernetes

import (
	"errors"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestCommittedReplacementNeverRebindsSameNameUID(t *testing.T) {
	for _, newerScoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "global", true: "namespace"}[newerScoped], func(t *testing.T) {
			service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			old := kubeindex.Record{UID: "retired-root", Kind: "StatefulSet", Namespace: "shop", Name: "api"}
			replacement := old
			replacement.UID = "replacement-root"
			child := kubeindex.Record{UID: "replacement-pod", Kind: "Pod", Namespace: "shop", Name: "api-0",
				Owners: []kubeindex.OwnerRef{{UID: replacement.UID, Kind: replacement.Kind, Name: replacement.Name, Namespace: "shop", ControllerKnown: true, Controller: true}}}
			seedInventory(t, service, []kubeindex.Record{replacement, child})
			cache := service.changes.inventories[service.indexScope()]
			now := time.Now().UTC()
			states := map[string]kubeindex.KindStatus{
				"StatefulSet": {State: "ready", ObservedAt: now.Add(-time.Minute)},
				"Pod":         {State: "ready", ObservedAt: now.Add(-time.Minute)},
			}
			cache.generation.Namespaces = map[string]kubeindex.Snapshot{"shop": {Records: []kubeindex.Record{old}, Kinds: states}}
			if newerScoped {
				cache.generation.View.Records = []kubeindex.Record{old}
				cache.generation.Namespaces["shop"] = kubeindex.Snapshot{Records: []kubeindex.Record{replacement, child},
					Kinds: map[string]kubeindex.KindStatus{"StatefulSet": {State: "ready", ObservedAt: now.Add(time.Second)}, "Pod": {State: "ready", ObservedAt: now.Add(time.Second)}}}
			}
			snapshot, _, err := service.CachedNamespaceSnapshot(t.Context(), "shop", "StatefulSet", "Pod")
			if err != nil {
				t.Fatal(err)
			}
			options := WorkloadLogStreamOptions{Kind: "StatefulSet", Namespace: "shop", Name: "api", UID: old.UID}
			if pods, err := workloadLogPods(snapshot, options, true); !errors.Is(err, ErrNotFound) || len(pods) != 0 {
				t.Fatalf("retired UID was rebound to same-name replacement: pods=%+v err=%v", pods, err)
			}
			options.UID = replacement.UID
			pods, err := workloadLogPods(snapshot, options, true)
			if err != nil || len(pods) != 1 || pods[0].UID != child.UID {
				t.Fatalf("replacement membership=%+v err=%v", pods, err)
			}
			options.Namespace = "other"
			if pods, err := workloadLogPods(snapshot, options, true); !errors.Is(err, ErrNotFound) || len(pods) != 0 {
				t.Fatalf("membership leaked namespace: pods=%+v err=%v", pods, err)
			}
		})
	}
}

func TestWorkloadsOptionalKindFailurePreservesScopedCountsAndEmptyReplacement(t *testing.T) {
	for _, failure := range []string{"pending", "timeout", "forbidden"} {
		t.Run(failure, func(t *testing.T) {
			service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			seedInventory(t, service, []kubeindex.Record{
				{UID: "shop-deploy", Kind: "Deployment", Namespace: "shop", Name: "api"},
				{UID: "other-deploy", Kind: "Deployment", Namespace: "other", Name: "api"},
				{UID: "shop-cron", Kind: "CronJob", Namespace: "shop", Name: "cleanup"},
			})
			cache := service.changes.inventories[service.indexScope()]
			state := cache.generation.View.Kinds["CronJob"]
			if failure == "pending" {
				state = kubeindex.KindStatus{State: "pending"}
				cache.generation.Pending = []string{indexedResourceIDs["CronJob"]}
				// An uncommitted optional kind cannot supply rows.
				cache.generation.View.Records = cache.generation.View.Records[:2]
			} else {
				state.State, state.Partial, state.Error = "error", true, failure
			}
			cache.generation.View.Kinds["CronJob"] = state
			page, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop"})
			wantCron := 0
			if failure == "timeout" {
				wantCron = 1
			}
			if err != nil || len(page.Items) != 1+wantCron || page.Counts["Deployment"] != 1 || page.Counts["CronJob"] != wantCron {
				t.Fatalf("optional %s erased or leaked committed projection: page=%+v err=%v", failure, page, err)
			}
			if _, known := page.Counts["CronJob"]; known != (failure == "timeout") {
				t.Fatalf("optional %s count provenance: counts=%v", failure, page.Counts)
			}
			if failure != "pending" && (len(page.Partial) != 1 || page.Partial[0].Class != failure) {
				t.Fatalf("optional failure concealed: %+v", page.Partial)
			}
			// A complete empty publication must erase the old scoped inventory,
			// including after a retained partial publication.
			seedInventory(t, service, nil)
			for _, namespace := range []string{"shop", "other", ""} {
				empty, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: namespace})
				if err != nil || len(empty.Items) != 0 || empty.Counts["Deployment"] != 0 || empty.Counts["CronJob"] != 0 {
					t.Fatalf("empty replacement resurrected rows: namespace=%q page=%+v err=%v", namespace, empty, err)
				}
				for _, candidate := range workloadResources {
					if count, known := empty.Counts[candidate.Kind]; !known || count != 0 {
						t.Fatalf("successfully committed empty %s count=%d known=%t", candidate.Kind, count, known)
					}
				}
			}
		})
	}
}

func TestWorkloadsCountRequiresSuccessfulAuthorizedObservation(t *testing.T) {
	for _, failure := range []string{"timeout", "unavailable", "throttled", "forbidden", "unauthorized", "namespace_scope", "credential_unavailable"} {
		for _, observed := range []bool{false, true} {
			t.Run(failure+"/"+map[bool]string{false: "never-observed", true: "last-good"}[observed], func(t *testing.T) {
				service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
				records := []kubeindex.Record{{UID: "shop-deploy", Kind: "Deployment", Namespace: "shop", Name: "api"}}
				if observed {
					records = append(records, kubeindex.Record{UID: "shop-cron", Kind: "CronJob", Namespace: "shop", Name: "cleanup"})
				}
				seedInventory(t, service, records)
				cache := service.changes.inventories[service.indexScope()]
				state := cache.generation.View.Kinds["CronJob"]
				state.State, state.Error, state.Partial = "error", failure, true
				state.LastAttemptAt = time.Now().UTC()
				if !observed {
					state.ObservedAt, state.CompletedAt = time.Time{}, time.Time{}
				}
				cache.generation.View.Kinds["CronJob"] = state
				page, err := service.Workloads(t.Context(), WorkloadListOptions{Namespace: "shop"})
				if err != nil {
					t.Fatal(err)
				}
				wantKnown := observed && (failure == "timeout" || failure == "unavailable" || failure == "throttled")
				count, known := page.Counts["CronJob"]
				if known != wantKnown || wantKnown && count != 1 {
					t.Fatalf("count=%d known=%t wantKnown=%t page=%+v", count, known, wantKnown, page)
				}
				if len(page.Partial) != 1 || page.Partial[0].Class != failure {
					t.Fatalf("failure lost: %+v", page)
				}
				if containsString(page.Sync.Pending, indexedResourceIDs["CronJob"]) || page.Sync.State == "ready" {
					t.Fatalf("failed LIST changed into pending or ready: %+v", page.Sync)
				}
			})
		}
	}
}
