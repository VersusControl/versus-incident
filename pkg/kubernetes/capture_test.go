package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"golang.org/x/time/rate"
)

type journalFaultProvider struct {
	storage.Provider
	fail atomic.Bool
}

func (provider *journalFaultProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if provider.fail.Load() {
		return false, errors.New("storage unavailable")
	}
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func TestCrashJournalRawCaptureRestartAndDeletion(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	var deleted atomic.Bool
	var restarts atomic.Int32
	restarts.Store(2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}, "versions": []any{map[string]any{"version": "v1"}}}}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "statefulsets", "kind": "StatefulSet", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/statefulsets":
			if deleted.Load() {
				writeJSON(writer, map[string]any{"items": []any{}})
				return
			}
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": "api", "namespace": "shop", "uid": "root"}}}})
		case "/api/v1/pods":
			if deleted.Load() {
				writeJSON(writer, map[string]any{"items": []any{}})
				return
			}
			var statuses []any
			for position := range 20 {
				statuses = append(statuses, map[string]any{"name": "app-" + strconv.Itoa(position), "restartCount": restarts.Load(), "state": map[string]any{"terminated": map[string]any{"reason": "Reason" + string(rune('A'+position)), "exitCode": 1, "finishedAt": now.Format(time.RFC3339Nano), "message": "CANARY_CURRENT"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137, "signal": 9, "finishedAt": now.Add(-time.Second).Format(time.RFC3339Nano), "message": "CANARY_PREVIOUS"}}})
			}
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "api-0", "namespace": "shop", "uid": "pod", "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "api", "uid": "root", "controller": true}}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "env": []any{map[string]any{"name": "TOKEN", "value": "CANARY_SECRET"}}}}}, "status": map[string]any{"containerStatuses": statuses, "initContainerStatuses": statuses}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider := &journalFaultProvider{Provider: storage.NewMemory()}
	construct := func() *Service {
		service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
		service.SetChangeStorage(provider)
		if _, _, err := service.IndexSnapshot(t.Context(), "Pod", "StatefulSet"); err != nil {
			t.Fatal(err)
		}
		return service
	}
	service := construct()
	assertRows := func(service *Service, want int) {
		t.Helper()
		query := kubechanges.CrashQuery{RootUID: "root", Namespace: "shop", Limit: 13}
		ids := map[string]bool{}
		for {
			page, err := service.changeStore().QueryCrashes(query, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(page)
			if strings.Contains(string(encoded), "CANARY") || len(page.Gaps) != 0 || len(encoded) > 64<<10 {
				t.Fatalf("unsafe/incomplete page: %s", encoded)
			}
			for _, event := range page.Items {
				if ids[event.ID] || event.AncestryPartial || event.RootKind != "StatefulSet" {
					t.Fatalf("duplicate or lost ancestry: %+v", event)
				}
				ids[event.ID] = true
			}
			if page.Next == "" {
				break
			}
			query.Cursor = page.Next
		}
		if len(ids) != want {
			t.Fatalf("journal rows=%d want=%d", len(ids), want)
		}
	}
	assertRows(service, 120)
	assertRows(construct(), 120)
	provider.fail.Store(true)
	restarts.Store(9)
	failed := construct()
	page, err := failed.Crashes(t.Context(), kubechanges.CrashQuery{RootUID: "root"})
	if err != nil || !page.Sync.Partial || !page.Capture.Behind || !containsString(page.Limitations, "journal_commit_failed") {
		t.Fatalf("failed commit hidden: %+v %v", page, err)
	}
	provider.fail.Store(false)
	if _, _, err := failed.IndexSnapshot(t.Context(), "Pod"); err != nil {
		t.Fatal(err)
	}
	retained, err := failed.changeStore().QueryCrashes(kubechanges.CrashQuery{RootUID: "root"}, time.Now().UTC())
	if err != nil || len(retained.Gaps) == 0 {
		t.Fatalf("recovered commit loss hidden: %+v %v", retained, err)
	}
	recovered := construct()
	retained, err = recovered.changeStore().QueryCrashes(kubechanges.CrashQuery{RootUID: "root", Limit: 100}, time.Now().UTC())
	if err != nil || retained.Next == "" {
		t.Fatalf("recovered retained history: %+v %v", retained, err)
	}
	deleted.Store(true)
	deletedService := construct()
	query := kubechanges.CrashQuery{RootUID: "root", Limit: 100}
	count := 0
	for {
		page, err := deletedService.changeStore().QueryCrashes(query, time.Now().UTC())
		if err != nil || len(page.Gaps) == 0 {
			t.Fatalf("deletion lost disclosure: %+v %v", page, err)
		}
		count += len(page.Items)
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if count != 160 {
		t.Fatalf("deletion lost rows: %d", count)
	}
}

func TestCrashJournalObservationGapsAndUnknownTimes(t *testing.T) {
	now := time.Now().UTC()
	pod := kubeindex.Record{UID: "pod", Kind: "Pod", Name: "api", Namespace: "shop", Owners: []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "missing", Name: "rs", ControllerKnown: true, Controller: true}}}
	batch := &crashCaptureBatch{}
	batch.add("cluster", pod, map[string]any{"initContainerStatuses": []any{map[string]any{"name": "setup", "restartCount": float64(7), "lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "finishedAt": "invalid"}}}}}, now)
	events, gaps, read := batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, now)
	if !read || len(gaps) != 0 || len(events) != 2 || events[0].Type != "termination" || !events[0].Init || !events[0].AncestryPartial || events[0].TimeSource != "observed_at" || !events[0].At.Equal(now) {
		t.Fatalf("unknown time/ancestry hidden: %+v %+v", events, gaps)
	}
	batch.bytes = 1 << 20
	batch.add("cluster", pod, map[string]any{"containerStatuses": []any{map[string]any{"name": "extra", "restartCount": float64(1)}}}, now)
	_, gaps, _ = batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, now)
	if len(gaps) != 1 || gaps[0].From.After(now.Add(-24*time.Hour)) {
		t.Fatal("capture budget omission hidden")
	}
	batch.omitted = false
	_, gaps, _ = batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "partial", Partial: true}}}, now)
	if len(gaps) == 0 {
		t.Fatal("partial Pod read hidden")
	}
	failed := &crashCaptureBatch{attempted: true}
	_, gaps, _ = failed.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Partial: true}}}, now)
	if len(gaps) == 0 {
		t.Fatal("failed Pod poll hidden")
	}
}

func TestCrashJournalHealthyPollsDoNotStarveLateCrashes(t *testing.T) {
	for _, healthy := range []int{2101, 5001} {
		t.Run(strconv.Itoa(healthy), func(t *testing.T) {
			now := time.Now().UTC()
			store := kubechanges.NewStore(storage.NewMemory(), kubeindex.Scope{ClusterID: "cluster"})
			pod := kubeindex.Record{UID: "crashed", Kind: "Pod", Name: "crashed", Namespace: "shop"}
			snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
			for poll := range 4 {
				observed := now.Add(time.Duration(poll) * time.Second)
				batch := &crashCaptureBatch{}
				for position := range healthy {
					name := "healthy-" + strconv.Itoa(position)
					batch.add("cluster", kubeindex.Record{UID: name, Kind: "Pod", Name: name, Namespace: "shop"}, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "state": map[string]any{"running": map[string]any{}}}}, "initContainerStatuses": []any{map[string]any{"name": "setup"}}}, observed)
				}
				if len(batch.events) != 0 || len(batch.pods) != 0 || batch.bytes != 0 || batch.omitted {
					t.Fatal("healthy containers consumed capture budget")
				}
				batch.add("cluster", pod, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(poll + 1), "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "finishedAt": observed.Format(time.RFC3339Nano)}}}}}, observed)
				events, gaps, read := batch.resolve(snapshot, observed)
				if len(events) != 2 || len(gaps) != 0 || !read || events[0].Type != "termination" {
					t.Fatalf("late crash lost: %+v %+v", events, gaps)
				}
				if err := store.CommitCapture(nil, nil, nil, "", observed, events, gaps, read); err != nil {
					t.Fatal(err)
				}
			}
			page, err := store.QueryCrashes(kubechanges.CrashQuery{}, now.Add(4*time.Second))
			if err != nil || len(page.Items) != 8 || len(page.Gaps) != 0 {
				t.Fatalf("healthy churn evicted crashes: rows=%d gaps=%+v err=%v", len(page.Items), page.Gaps, err)
			}
		})
	}
}

func TestCrashJournalCompletedInitPollsDoNotStarveLateCrash(t *testing.T) {
	for _, reason := range []string{"Completed", ""} {
		t.Run("reason="+reason, func(t *testing.T) {
			now := time.Now().UTC()
			store := kubechanges.NewStore(storage.NewMemory(), kubeindex.Scope{ClusterID: "cluster"})
			snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
			var crashID string
			for poll := range 4 {
				observed := now.Add(time.Duration(poll) * time.Second)
				batch := &crashCaptureBatch{}
				for position := range 5001 {
					name := "healthy-" + strconv.Itoa(position)
					batch.add("cluster", kubeindex.Record{UID: name, Kind: "Pod", Name: name, Namespace: "shop"}, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "state": map[string]any{"running": map[string]any{}}}}, "initContainerStatuses": []any{map[string]any{"name": "setup", "state": map[string]any{"terminated": map[string]any{"reason": reason, "exitCode": float64(0), "signal": float64(0), "finishedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}}}}}, observed)
				}
				if len(batch.events) != 0 || len(batch.pods) != 0 || batch.bytes != 0 || batch.omitted {
					t.Fatal("successful init terminations consumed capture budget")
				}
				batch.add("cluster", kubeindex.Record{UID: "late", Kind: "Pod", Name: "late", Namespace: "shop"}, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "state": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "finishedAt": now.Format(time.RFC3339Nano)}}}}}, observed)
				events, gaps, read := batch.resolve(snapshot, observed)
				if len(events) != 1 || len(gaps) != 0 || !read || events[0].Type != "termination" || events[0].PodUID != "late" {
					t.Fatalf("late crash starved: %+v %+v", events, gaps)
				}
				if poll == 0 {
					crashID = events[0].ID
				}
				if err := store.CommitCapture(nil, nil, nil, "", observed, events, gaps, read); err != nil {
					t.Fatal(err)
				}
				page, err := store.QueryCrashes(kubechanges.CrashQuery{}, observed)
				if err != nil || len(page.Items) != 1 || page.Items[0].ID != crashID || page.Items[0].Sequence != 1 || !page.Items[0].ObservedAt.Equal(now) || len(page.Gaps) != 0 || page.Next != "" {
					t.Fatalf("repeat poll churned journal: %+v %v", page, err)
				}
			}
		})
	}
}

func TestCrashJournalSuccessfulTerminationKeepsOnlyLifetimeStatus(t *testing.T) {
	now := time.Now().UTC()
	for _, field := range []string{"containerStatuses", "initContainerStatuses"} {
		for _, source := range []string{"state", "lastState"} {
			for _, reason := range []string{"Completed", ""} {
				for _, restarts := range []int{0, 3} {
					t.Run(field+"/"+source+"/reason="+reason+"/restarts="+strconv.Itoa(restarts), func(t *testing.T) {
						batch := &crashCaptureBatch{}
						batch.add("cluster", kubeindex.Record{UID: "pod", Kind: "Pod", Name: "pod"}, map[string]any{field: []any{map[string]any{"name": "app", "restartCount": float64(restarts), source: map[string]any{"terminated": map[string]any{"reason": reason, "exitCode": float64(0), "signal": float64(0)}}}}}, now)
						events, gaps, read := batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, now)
						want := 0
						if restarts > 0 {
							want = 1
						}
						if !read || len(gaps) != 0 || len(events) != want || want == 1 && (events[0].Type != "status" || events[0].RestartCount != int32(restarts)) {
							t.Fatalf("successful termination duplicated or lifetime count lost: %+v %+v", events, gaps)
						}
					})
				}
			}
		}
	}
}

func TestCrashJournalAbnormalTerminationIsNotSuccessful(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name     string
		reason   string
		exitCode int
		signal   int
	}{
		{name: "nonzero exit", exitCode: 1},
		{name: "signal only", signal: 9},
		{name: "abnormal reason", reason: "Error"},
		{name: "completed with nonzero exit", reason: "Completed", exitCode: 1},
		{name: "completed with signal", reason: "Completed", signal: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch := &crashCaptureBatch{}
			batch.add("cluster", kubeindex.Record{UID: "pod", Kind: "Pod", Name: "pod"}, map[string]any{"initContainerStatuses": []any{map[string]any{"name": "setup", "state": map[string]any{"terminated": map[string]any{"reason": test.reason, "exitCode": float64(test.exitCode), "signal": float64(test.signal)}}}}}, now)
			events, gaps, read := batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, now)
			if !read || len(gaps) != 0 || len(events) != 1 || events[0].Type != "termination" || events[0].Reason != test.reason || events[0].ExitCode != int32(test.exitCode) || events[0].Signal != int32(test.signal) {
				t.Fatalf("abnormal termination suppressed: %+v %+v", events, gaps)
			}
		})
	}
}

func TestCrashJournalTerminationPriorityAndBounds(t *testing.T) {
	now := time.Now().UTC()
	batch := &crashCaptureBatch{}
	for position := range 6000 {
		name := "waiting-" + strconv.Itoa(position)
		batch.add("cluster", kubeindex.Record{UID: name, Kind: "Pod", Name: name}, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "state": map[string]any{"waiting": map[string]any{"reason": "ContainerCreating"}}}}}, now)
	}
	pod := kubeindex.Record{UID: "late", Kind: "Pod", Name: "late"}
	batch.add("cluster", pod, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(1), "state": map[string]any{"terminated": map[string]any{"reason": "Error"}}, "lastState": map[string]any{"terminated": map[string]any{"reason": "OOMKilled"}}}}}, now)
	events, gaps, _ := batch.resolve(kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, now)
	terminations := 0
	for _, event := range events {
		if event.PodUID == "late" && event.Type == "termination" {
			terminations++
		}
	}
	if terminations != 2 || len(gaps) != 1 || len(events) > 5000 || batch.bytes > 1<<20 || len(batch.pods) > len(events) {
		t.Fatalf("priority/bounds: terminations=%d events=%d pods=%d bytes=%d gaps=%+v", terminations, len(events), len(batch.pods), batch.bytes, gaps)
	}
}

func TestCrashJournalInvalidResolvedEventDoesNotAbortChanges(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	store := kubechanges.NewStore(storage.NewMemory(), kubeindex.Scope{ClusterID: "cluster"})
	batch := &crashCaptureBatch{}
	for _, name := range []string{"invalid", "valid"} {
		pod := kubeindex.Record{UID: name, Kind: "Pod", Name: name, Namespace: "shop"}
		if name == "invalid" {
			pod.Owners = []kubeindex.OwnerRef{{UID: "root", Kind: "StatefulSet", ControllerKnown: true, Controller: true}}
		}
		batch.add("cluster", pod, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(1)}}}, now)
	}
	deltas := make(chan kubeindex.Delta, 1)
	close(deltas)
	snapshot := kubeindex.Snapshot{Records: []kubeindex.Record{{UID: "root", Kind: "StatefulSet", Name: "api", Namespace: "shop", ProgressingTransitionAt: now, CurrentRevision: "old", UpdateRevision: "new"}}, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
	if err := service.captureCycleWithCrashes(store, deltas, snapshot, []string{"Pod"}, now, batch); err != nil {
		t.Fatalf("invalid crash aborted shared commit: %v", err)
	}
	page, err := store.QueryCrashes(kubechanges.CrashQuery{}, time.Now().UTC())
	if err != nil || len(page.Items) != 1 || page.Items[0].PodUID != "valid" || len(page.Gaps) != 1 || page.Gaps[0].From.After(page.StartedAt.Add(-24*time.Hour)) {
		t.Fatalf("invalid crash not skipped/disclosed: %+v %v", page, err)
	}
	changes, err := store.Query(kubechanges.Query{}, time.Now().UTC())
	if err != nil || len(changes.Items) == 0 {
		t.Fatalf("shared resource change lost: %+v %v", changes, err)
	}
}

func TestCrashJournalCapturedSourceAncestryRemainsPartial(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name         string
		podPartial   bool
		ownerMissing bool
		ownerPartial bool
		wantPartial  bool
	}{
		{name: "complete chain"},
		{name: "captured Pod partial", podPartial: true, wantPartial: true},
		{name: "missing owner", ownerMissing: true, wantPartial: true},
		{name: "partial owner", ownerPartial: true, wantPartial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := kubeindex.Record{UID: "pod", Kind: "Pod", Name: "api-0", Namespace: "shop", Summary: map[string]string{}, Owners: []kubeindex.OwnerRef{{UID: "root", Kind: "StatefulSet", Name: "api", ControllerKnown: true, Controller: true}}}
			if test.podPartial {
				pod.Summary["references_partial"] = "true"
			}
			batch := &crashCaptureBatch{}
			batch.add("cluster", pod, map[string]any{"containerStatuses": []any{map[string]any{"name": "app", "restartCount": float64(1)}}}, now)
			pod.Summary = nil
			snapshot := kubeindex.Snapshot{Records: []kubeindex.Record{pod}, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
			if !test.ownerMissing {
				owner := kubeindex.Record{UID: "root", Kind: "StatefulSet", Name: "api", Namespace: "shop", Summary: map[string]string{}}
				if test.ownerPartial {
					owner.Summary["references_partial"] = "true"
				}
				snapshot.Records = append(snapshot.Records, owner)
			}
			events, gaps, _ := batch.resolve(snapshot, now)
			if len(events) != 1 || events[0].RootUID != "root" || events[0].AncestryPartial != test.wantPartial || len(gaps) != 0 {
				t.Fatalf("capture-time ancestry misrepresented: %+v %+v", events, gaps)
			}
		})
	}
}

func TestIndependentCaptureServicesShareIdentityAndLease(t *testing.T) {
	provider := storage.NewMemory()
	scope := Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}
	services := make([]*Service, 2)
	for position := range services {
		credentials, err := staticTokenCredential("shared-credential-canary")
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewClient(Config{Endpoint: "http://127.0.0.1:12345", Credentials: credentials, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		services[position] = NewService(client, scope, 0)
		services[position].SetChangeStorage(provider)
	}
	if services[0].indexScope() != services[1].indexScope() {
		t.Fatal("independent replicas use different persistent capture identities")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for position, service := range services {
		go func() {
			<-start
			results <- service.changeStore().ClaimLease("owner-"+strconv.Itoa(position), time.Now().UTC(), time.Minute)
		}()
	}
	close(start)
	owners := 0
	for range services {
		if err := <-results; err == nil {
			owners++
		} else if !errors.Is(err, kubechanges.ErrLeaseLost) {
			t.Fatal(err)
		}
	}
	if owners != 1 {
		t.Fatalf("shared capture lease has %d owners", owners)
	}
}

func TestIndependentCaptureFailoverAndForegroundReads(t *testing.T) {
	var podReads atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseScan := func() { releaseOnce.Do(func() { close(release) }) }
	created := time.Now().UTC().Add(-time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer shared-capture-canary" {
			http.Error(writer, "denied", http.StatusForbidden)
			return
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/pods":
			podReads.Add(1)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
			writeJSON(writer, map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "api", "namespace": "default", "uid": "pod", "resourceVersion": "1", "creationTimestamp": created.Format(time.RFC3339Nano)}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	defer releaseScan()
	provider := storage.NewMemory()
	construct := func() *Service {
		t.Helper()
		credentials, _ := staticTokenCredential("shared-capture-canary")
		client, err := NewClient(Config{Endpoint: server.URL, Credentials: credentials, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
		service.SetChangeStorage(provider)
		if _, err := service.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		index, err := service.indexes.ForScope(service.indexScope())
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Ingest("Pod", nil, true, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		return service
	}
	start := func(service *Service) *captureLoop {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		loop := &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: "owner-" + strconv.FormatInt(time.Now().UnixNano(), 10), leaseTTL: 300 * time.Millisecond, renewInterval: 50 * time.Millisecond, next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous", Behind: true}}
		for kind := range captureCadences {
			if kind != "Pod" {
				loop.next[kind] = time.Now().Add(time.Hour)
			}
		}
		service.changes.mu.Lock()
		service.changes.lifecycle = t.Context()
		service.changes.captures[service.indexScope()] = loop
		service.changes.mu.Unlock()
		go service.capture(loop)
		t.Cleanup(service.StopCapture)
		return loop
	}
	wait := func(check func() bool) {
		t.Helper()
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for !check() {
			select {
			case <-timer.C:
				t.Fatal("capture ownership or commit did not settle")
			case <-ticker.C:
			}
		}
	}
	first, second := construct(), construct()
	loops := []*captureLoop{start(first), start(second)}
	services := []*Service{first, second}
	wait(func() bool { return podReads.Load() > 0 })
	results := make(chan error, 2)
	for _, service := range services {
		go func() {
			snapshot, status, err := service.IndexSnapshot(t.Context(), "Pod")
			if err == nil && (status.State != "ready" || len(snapshot.Records) != 1) {
				err = errors.New("foreground did not read its independent index")
			}
			results <- err
		}()
	}
	wait(func() bool { return podReads.Load() >= 2 })
	releaseScan()
	for range services {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	owner := -1
	wait(func() bool {
		count := 0
		for position, loop := range loops {
			loop.mu.Lock()
			if loop.owned {
				count++
				owner = position
			}
			loop.mu.Unlock()
		}
		if count > 1 {
			t.Fatal("independent capture loops simultaneously own one scope")
		}
		page, err := first.changeStore().Query(ChangeQuery{}, time.Now().UTC())
		if err != nil || len(page.Items) > 1 {
			t.Fatalf("capture history before failover: items=%d err=%v", len(page.Items), err)
		}
		return count == 1 && len(page.Items) == 1
	})
	if reads := podReads.Load(); reads < 2 || reads > 3 {
		t.Fatalf("foreground/capture multiplied upstream scans: %d", reads)
	}
	blobs, err := provider.ListBlobs("kubernetes/changes/")
	if err != nil || len(blobs) != 1 {
		t.Fatal("independent services persisted separate capture blobs")
	}
	leader, follower := services[owner], services[1-owner]
	loops[owner].mu.Lock()
	staleOwner := loops[owner].owner
	loops[owner].mu.Unlock()
	leader.StopCapture()
	wait(func() bool {
		loop := loops[1-owner]
		loop.mu.Lock()
		defer loop.mu.Unlock()
		return loop.owned && !loop.status.Behind
	})
	coverage := map[string]kubechanges.Gap{"Pod": {From: created, To: time.Now().UTC()}}
	stale := Change{ID: "stale", Kind: "Pod", Cluster: "cluster", Name: "stale", Type: kubechanges.Created, At: time.Now().UTC()}
	if err := leader.changeStore().Commit([]Change{stale}, nil, coverage, staleOwner, time.Now().UTC()); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal("released owner committed stale changes or coverage")
	}
	follower.StopCapture()
	restarted := construct()
	if restarted.indexScope() != first.indexScope() || restarted.identity == first.identity || restarted.client == first.client {
		t.Fatal("restart did not separate persistent identity from process lifetime")
	}
	restartedLoop := start(restarted)
	wait(func() bool {
		restartedLoop.mu.Lock()
		defer restartedLoop.mu.Unlock()
		return restartedLoop.owned && !restartedLoop.status.Behind
	})
	page, err := restarted.changeStore().Query(ChangeQuery{}, time.Now().UTC())
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("failover/restart duplicated persistent history: items=%d err=%v", len(page.Items), err)
	}
}

func TestCaptureScopeIsolationAndProviderRenewal(t *testing.T) {
	construct := func(endpoint, token string, scope Scope) *Service {
		t.Helper()
		credentials, _ := staticTokenCredential(token)
		client, err := NewClient(Config{Endpoint: endpoint, Credentials: credentials, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		return NewService(client, scope, 0)
	}
	scope := Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}
	base := construct("http://127.0.0.1:12345", "credential-a-canary", scope)
	provider := storage.NewMemory()
	base.SetChangeStorage(provider)
	now := time.Now().UTC()
	if err := base.changeStore().Append([]Change{{ID: "old", Kind: "Pod", Cluster: "cluster", Name: "old", Type: kubechanges.Created, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []struct {
		endpoint string
		token    string
		scope    Scope
	}{
		{"http://127.0.0.1:12346", "credential-a-canary", scope},
		{"http://127.0.0.1:12345", "restricted-b-canary", scope},
		{"http://127.0.0.1:12345", "credential-a-canary", Scope{OrgID: "other", ClusterID: "cluster", CredentialID: "configured"}},
		{"http://127.0.0.1:12345", "credential-a-canary", Scope{OrgID: "org", ClusterID: "other", CredentialID: "configured"}},
		{"http://127.0.0.1:12345", "credential-a-canary", Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "other"}},
	} {
		isolated := construct(variant.endpoint, variant.token, variant.scope)
		isolated.SetChangeStorage(provider)
		page, err := isolated.changeStore().Query(ChangeQuery{}, now)
		if isolated.indexScope() == base.indexScope() || err != nil || len(page.Items) != 0 {
			t.Fatal("endpoint, credential, org or cluster reused privileged scope")
		}
	}
	alias := base.Scoped(Scope{OrgID: "alias", ClusterID: "cluster", CredentialID: "configured"})
	independentAlias := construct("http://127.0.0.1:12345", "credential-a-canary", alias.Scope())
	if alias.indexScope() != independentAlias.indexScope() || alias.indexScope() == base.indexScope() {
		t.Fatal("org alias does not match an independent authorized scope")
	}
	changedAlias := base.Scoped(Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "other"})
	if changedAlias.indexScope() != construct("http://127.0.0.1:12345", "credential-a-canary", changedAlias.Scope()).indexScope() {
		t.Fatal("configured credential alias lost credential binding")
	}
	known := func(principal, token string) *Service {
		source := &expiringBearerSource{principal: principal, now: time.Now, refresh: func(context.Context) (string, time.Time, error) { return token, now.Add(time.Hour), nil }}
		client, err := NewClient(Config{Endpoint: "http://127.0.0.1:12345", Credentials: source, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		return NewService(client, scope, 0)
	}
	first := known("provider-principal-a", "renewal-one-canary")
	renewed := known("provider-principal-a", "renewal-two-canary")
	other := known("provider-principal-b", "renewal-two-canary")
	if first.indexScope() != renewed.indexScope() || first.indexScope() == other.indexScope() {
		t.Fatal("provider principal renewal or isolation failed across processes")
	}
	for _, service := range []*Service{base, alias, first, renewed, other} {
		encoded, _ := json.Marshal(service.ActiveScope())
		if strings.Contains(string(encoded), "canary") || strings.Contains(service.indexScope().CredentialID, "canary") {
			t.Fatal("credential escaped opaque scope")
		}
	}
}

func TestUnavailableCaptureCredentialCannotAcquireOrPublish(t *testing.T) {
	source := &credentialSource{authorization: func(context.Context) (string, error) {
		return "", errors.New("credential-failure-canary")
	}}
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1:12345", Credentials: source, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	service.StartCapture(t.Context())
	defer service.StopCapture()
	if service.captureState() != nil {
		t.Fatal("uninitialized credential started capture")
	}
	if err := service.commitCredential(t.Context(), func() error { t.Fatal("unavailable credential published"); return nil }); !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "canary") {
		t.Fatal("unavailable credential did not fail closed")
	}
	blobs, err := provider.ListBlobs("kubernetes/changes/")
	if err != nil || len(blobs) != 0 {
		t.Fatal("unavailable credential persisted a lease or history")
	}
}

func TestUnknownPrincipalRenewalRetiresCaptureScope(t *testing.T) {
	now := time.Now().UTC()
	refreshes := 0
	source := &expiringBearerSource{now: func() time.Time { return now }, refresh: func(context.Context) (string, time.Time, error) {
		refreshes++
		return "unknown-renewal-canary-" + strconv.Itoa(refreshes), now.Add(time.Minute), nil
	}}
	client, err := NewClient(Config{Endpoint: "http://127.0.0.1:12345", Credentials: source, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}
	old := NewService(client, scope, 0)
	provider := storage.NewMemory()
	old.SetChangeStorage(provider)
	old.StartCapture(t.Context())
	defer old.StopCapture()
	if old.CaptureStatus().Mode != "demand" {
		t.Fatal("unknown rotating provider started competing continuous capture")
	}
	blobs, err := provider.ListBlobs("kubernetes/changes/")
	if err != nil || len(blobs) != 0 {
		t.Fatal("unknown rotating provider acquired a capture lease")
	}
	if err := old.changeStore().Append([]Change{{ID: "old", Kind: "Pod", Cluster: "cluster", Name: "old", Type: kubechanges.Created, At: now}}, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := old.validateCredential(t.Context()); !errors.Is(err, ErrCredentialUnavailable) || old.identity.ctx.Err() == nil {
		t.Fatal("unknown-principal renewal retained the old authorization context")
	}
	fresh := NewService(client, scope, 0)
	fresh.SetChangeStorage(provider)
	if fresh.indexScope() == old.indexScope() || fresh.validateCredential(t.Context()) != nil {
		t.Fatal("unknown-principal renewal reused the old history scope")
	}
	page, err := fresh.changeStore().Query(ChangeQuery{}, now)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("unknown-principal renewal exposed old history")
	}
	if err := old.commitCredential(t.Context(), func() error { t.Fatal("retired renewal published"); return nil }); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("unknown-principal renewal admitted a stale commit")
	}
}

func TestCaptureHonoursRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{{"12", 12 * time.Second}, {"600", 5 * time.Minute}, {now.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second}, {"invalid", 30 * time.Second}, {"-1", time.Second}} {
		if got := parseRetryAfter(test.value, now); got != test.want {
			t.Fatalf("Retry-After %q: %s != %s", test.value, got, test.want)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "12")
		http.Error(writer, "CANARY_STATUS_BODY", http.StatusTooManyRequests)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	var output any
	err := service.client.GetJSON(t.Context(), "/api", &output)
	var throttled *ErrThrottled
	if !errors.As(err, &throttled) || throttled.RetryAfter != 12*time.Second || strings.Contains(err.Error(), "CANARY") || errorClass(err) != "throttled" {
		t.Fatalf("throttle error: %v", err)
	}
}

func TestStopCaptureDrainsForegroundRefresh(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	if err := service.SetCaptureMode("demand"); err != nil {
		t.Fatal(err)
	}
	service.StartCapture(t.Context())
	go service.IndexSnapshot(t.Context(), "Pod")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("foreground refresh did not start")
	}
	service.changes.mu.Lock()
	call := service.changes.refreshes[indexRefreshKey{scope: service.indexScope()}]
	service.changes.mu.Unlock()
	stopped := make(chan struct{})
	go func() { service.StopCapture(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not drain foreground refresh")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream read survived stop")
	}
	select {
	case <-call.done:
	default:
		t.Fatal("stop returned before foreground refresh exited")
	}
}

type namespaceFallbackTransport struct {
	started  chan struct{}
	canceled chan struct{}
	drain    chan struct{}
}

func (transport namespaceFallbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	close(transport.started)
	<-request.Context().Done()
	close(transport.canceled)
	<-transport.drain
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[{"apiVersion":"v1","kind":"Pod","metadata":{"uid":"stale","name":"stale","namespace":"default"}}]}`)), Request: request}, nil
}

func TestNamespaceFallbackScopeCancellationDrainsBeforeReturn(t *testing.T) {
	for _, stop := range []string{"capture", "deregister"} {
		t.Run(stop, func(t *testing.T) {
			service := newTestService(t, "http://127.0.0.1:12345", Scope{ClusterID: "cluster"})
			_ = service.SetCaptureMode("demand")
			registry := NewServiceRegistry(service)
			registry.StartCapture(t.Context())
			transport := namespaceFallbackTransport{started: make(chan struct{}), canceled: make(chan struct{}), drain: make(chan struct{})}
			var drainOnce sync.Once
			drain := func() { drainOnce.Do(func() { close(transport.drain) }) }
			defer registry.Close()
			defer drain()
			service.client.http.Transport = transport
			service.cache[service.cacheKey()] = discoveryCacheEntry{expires: time.Now().Add(time.Minute), discovery: Discovery{Resources: []ResourceDefinition{{ID: "core~v1~pods", Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}}}}}
			loop := service.captureState()
			owned, cancelOwned := context.WithCancel(loop.ctx)
			loop.mu.Lock()
			loop.ownedContext = owned
			loop.mu.Unlock()
			result := make(chan kubeindex.Snapshot, 1)
			go func() {
				snapshot, _ := service.namespaceIssueSnapshot(t.Context(), "default", []string{"Pod"})
				result <- snapshot
			}()
			select {
			case <-transport.started:
			case <-time.After(time.Second):
				t.Fatal("namespace fallback did not start")
			}
			cancelOwned()
			select {
			case <-transport.canceled:
				t.Fatal("foreground fallback canceled by lease loss")
			case <-time.After(20 * time.Millisecond):
			}
			stopped := make(chan struct{})
			go func() {
				if stop == "capture" {
					service.StopCapture()
				} else {
					registry.DeregisterCluster("cluster")
				}
				close(stopped)
			}()
			select {
			case <-transport.canceled:
			case <-time.After(time.Second):
				t.Fatal("scope teardown did not cancel fallback")
			}
			select {
			case <-stopped:
				t.Fatal("scope teardown returned before fallback drained")
			case <-time.After(20 * time.Millisecond):
			}
			drain()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("scope teardown did not finish after fallback drained")
			}
			select {
			case snapshot := <-result:
				if len(snapshot.Records) != 0 {
					t.Fatal("canceled namespace fallback accepted a stale response")
				}
			case <-time.After(time.Second):
				t.Fatal("fallback did not return after drain")
			}
		})
	}
}

func TestCredentialReplacementRetiresAliasesAndHistory(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("principal-a-canary"), 0o600); err != nil {
		t.Fatal("could not write token fixture")
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer principal-a-canary" {
			http.Error(writer, "CANARY_DENIED", http.StatusForbidden)
			return
		}
		if request.URL.Path == "/api/v1/pods" {
			close(started)
			<-request.Context().Done()
			close(canceled)
			return
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		default:
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, TokenFile: tokenFile, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster", CredentialID: "configured"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	_ = service.SetCaptureMode("demand")
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	defer registry.Close()
	first, err := registry.ResolveCluster("first", "cluster")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := registry.ResolveCluster("second", "cluster")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, scoped := range []*Service{first, alias} {
		if err := scoped.changeStore().Append([]Change{{ID: "old", Kind: "Pod", UID: "old", Name: "old-principal-data", Cluster: "cluster", Type: kubechanges.Created, At: now}}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := scoped.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		index, err := scoped.indexes.ForScope(scoped.indexScope())
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Ingest("Pod", []kubeindex.Record{{Kind: "Pod", UID: "old", Name: "old-principal-data"}}, true, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	go first.IndexSnapshot(t.Context(), "Pod")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old credential read did not start")
	}
	if err := os.WriteFile(tokenFile, []byte("principal-b-canary"), 0o600); err != nil {
		t.Fatal("could not replace token fixture")
	}
	replacement, err := registry.ResolveCluster("first", "cluster")
	if err != nil || replacement == first || replacement.indexScope() == first.indexScope() {
		t.Fatal("configured credential ID reused old identity epoch")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("old credential upstream read survived replacement")
	}
	for _, stale := range []*Service{first, alias} {
		if page, err := stale.Changes(t.Context(), ChangeQuery{}); err == nil || len(page.Items) != 0 {
			t.Fatal("stale alias served old history")
		}
		committed := false
		if err := stale.commitCredential(t.Context(), func() error { committed = true; return nil }); err == nil || committed {
			t.Fatal("old credential committed after replacement")
		}
		if page, err := stale.Issues(t.Context(), IssueOptions{IncludeChanges: true}); err == nil || len(page.Items) != 0 {
			t.Fatal("stale alias served Investigate evidence")
		}
	}
	page, err := replacement.Changes(t.Context(), ChangeQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("replacement exposed old history")
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "canary") || strings.Contains(string(encoded), "old-principal-data") {
		t.Fatal("credential or old data escaped")
	}
}

func TestDeregisterDrainsDemandAndContinuousAliases(t *testing.T) {
	for _, mode := range []string{"demand", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{}, 2)
			canceled := make(chan struct{}, 2)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api":
					writeJSON(writer, map[string]any{"versions": []string{"v1"}})
				case "/apis":
					writeJSON(writer, map[string]any{"groups": []any{}})
				case "/api/v1":
					writeJSON(writer, map[string]any{"resources": []any{
						map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
						map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
					}})
				case "/api/v1/services":
					started <- struct{}{}
					<-request.Context().Done()
					canceled <- struct{}{}
				default:
					writeJSON(writer, map[string]any{"items": []any{}})
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "first", ClusterID: "cluster"})
			service.SetChangeStorage(storage.NewMemory())
			_ = service.SetCaptureMode(mode)
			registry := NewServiceRegistry(service)
			defer registry.Close()
			aliases := []*Service{}
			for _, org := range []string{"first", "second"} {
				alias, err := registry.ResolveCluster(org, "cluster")
				if err != nil {
					t.Fatal(err)
				}
				aliases = append(aliases, alias)
				if _, err := alias.Discover(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			registry.StartCapture(t.Context())
			for _, alias := range aliases {
				go alias.IndexSnapshot(t.Context(), "Service")
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("alias foreground list did not start")
				}
			}
			registry.DeregisterCluster("cluster")
			for _, alias := range aliases {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("deregister left alias upstream read active")
				}
				if _, _, err := alias.IndexSnapshot(t.Context(), "Service"); err == nil {
					t.Fatal("retired scope admitted another refresh")
				}
				index, err := alias.indexes.ForScope(alias.indexScope())
				if err != nil || len(index.Snapshot().Records) != 0 {
					t.Fatal("canceled foreground list mutated index")
				}
				if err := alias.changeStore().ClaimLease("after-stop", time.Now().UTC(), time.Minute); err != nil {
					t.Fatal("deregister did not release capture lease")
				}
			}
		})
	}
}

func TestRegistrySlowCredentialRefreshDoesNotBlockOtherCluster(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	source := &credentialSource{authorization: func(ctx context.Context) (string, error) {
		if blocked.Load() {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "Bearer stable", nil
	}}
	slow := NewService(&Client{credentials: source}, Scope{ClusterID: "slow"}, 0)
	fast := NewService(nil, Scope{ClusterID: "fast"}, 0)
	registry := NewClusterRegistry(true, []ClusterEntry{{Info: ClusterInfo{ID: "slow"}, Service: slow}, {Info: ClusterInfo{ID: "fast"}, Service: fast}})
	t.Cleanup(registry.Close)
	blocked.Store(true)
	done := make(chan error, 1)
	go func() { _, err := registry.ResolveCluster("org", "slow"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := registry.resolveCluster(ctx, "org", "fast"); err != nil {
		t.Fatalf("slow refresh blocked other cluster: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRebuildDrainIsolationAndCloseRace(t *testing.T) {
	for _, closeRegistry := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeRegistry), func(t *testing.T) {
			var rotated atomic.Bool
			source := &credentialSource{authorization: func(context.Context) (string, error) {
				if rotated.Load() {
					return "Bearer replacement", nil
				}
				return "Bearer original", nil
			}}
			slow := NewService(&Client{credentials: source}, Scope{ClusterID: "slow"}, 0)
			fast := NewService(nil, Scope{ClusterID: "fast"}, 0)
			registry := NewClusterRegistry(true, []ClusterEntry{{Info: ClusterInfo{ID: "slow"}, Service: slow}, {Info: ClusterInfo{ID: "fast"}, Service: fast}})
			t.Cleanup(registry.Close)
			alias, err := registry.ResolveCluster("org", "slow")
			if err != nil {
				t.Fatal(err)
			}
			gate := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			call := &indexRefreshCall{ctx: ctx, cancel: cancel, done: make(chan struct{})}
			alias.changes.mu.Lock()
			alias.changes.refreshes[indexRefreshKey{scope: alias.indexScope()}] = call
			alias.changes.mu.Unlock()
			go func() { <-ctx.Done(); <-gate; close(call.done) }()
			rotated.Store(true)
			done := make(chan error, 1)
			go func() { _, err := registry.ResolveCluster("org", "slow"); done <- err }()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("rebuild drain did not start")
			}
			lookup, cancelLookup := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancelLookup()
			if _, err := registry.resolveCluster(lookup, "org", "fast"); err != nil {
				t.Fatalf("drain blocked other cluster: %v", err)
			}
			if closeRegistry {
				closeCtx, cancelClose := context.WithCancel(t.Context())
				cancelClose()
				if err := registry.CloseContext(closeCtx); !errors.Is(err, context.Canceled) {
					t.Fatal("close did not wait for drain")
				}
			}
			close(gate)
			err = <-done
			if closeRegistry {
				if !errors.Is(err, ErrClusterUnavailable) {
					t.Fatal("closed registry installed rebuilt service")
				}
				registry.mu.Lock()
				current := registry.entries[0].Service
				registry.mu.Unlock()
				if current != slow {
					t.Fatal("close/rebuild resurrected generation")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKnownPrincipalRenewalPreservesEpoch(t *testing.T) {
	now := time.Now().UTC()
	var refreshes int
	source := &expiringBearerSource{principal: "provider-bound-principal", now: func() time.Time { return now }, refresh: func(context.Context) (string, time.Time, error) {
		refreshes++
		return "token-" + strconv.Itoa(refreshes), now.Add(time.Minute), nil
	}}
	service := NewService(&Client{credentials: source}, Scope{ClusterID: "cluster", CredentialID: "configured"}, 0)
	registry := NewServiceRegistry(service)
	first, err := registry.ResolveCluster("org", "cluster")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	second, err := registry.ResolveCluster("org", "cluster")
	if err != nil || second != first || refreshes != 2 {
		t.Fatal("same provider principal renewal discarded its epoch")
	}
}

func TestRegistryCloseSharesAliasDrain(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	registry := NewServiceRegistry(service)
	first, _ := registry.ResolveCluster("first", "cluster")
	second, _ := registry.ResolveCluster("second", "cluster")
	gate := make(chan struct{})
	for _, scoped := range []*Service{first, second} {
		ctx, cancel := context.WithCancel(t.Context())
		call := &indexRefreshCall{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		scoped.changes.refreshes[indexRefreshKey{scope: scoped.indexScope()}] = call
		go func(call *indexRefreshCall) {
			<-call.ctx.Done()
			<-gate
			close(call.done)
		}(call)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := registry.CloseContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("expired waiter reported completed drain")
	}
	done := make(chan struct{})
	go func() { registry.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("concurrent close returned before shared alias drain")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("alias drain did not join")
	}
	registry.Close()
}

func TestCaptureLeaseRenewalIndependentOfLongScan(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	loop := &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: "owner", leaseTTL: 300 * time.Millisecond, renewInterval: 50 * time.Millisecond, next: map[string]time.Time{}, success: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous", Behind: true}}
	service.changes.captures[service.indexScope()] = loop
	go service.capture(loop)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("capture never started")
	}
	timer := time.NewTimer(700 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	second := kubechanges.NewStore(provider, service.indexScope())
	if err := second.ClaimLease("other", time.Now().UTC(), 90*time.Second); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatalf("lease expired during long scan: %v", err)
	}
	service.StopCapture()
	if err := second.ClaimLease("other", time.Now().UTC(), 90*time.Second); err != nil {
		t.Fatalf("shutdown did not release lease: %v", err)
	}
}

func TestCaptureMarksGapAndFencesLostOwner(t *testing.T) {
	now := time.Now().UTC()
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	store := service.changeStore()
	if err := store.ClaimLease("owner", now, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	loop := &captureLoop{ctx: ctx, cancel: cancel, owner: "owner", owned: true, status: CaptureStatus{Mode: "continuous"}, success: map[string]time.Time{"Pod": now.Add(-90 * time.Second)}, next: map[string]time.Time{}, gapFrom: map[string]time.Time{}}
	service.changes.captures[service.indexScope()] = loop
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", Records: 1}}}
	if err := service.captureCycle(store, nil, snapshot, []string{"Pod"}, now); err != nil {
		t.Fatal(err)
	}
	snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "ready", Records: 1, ObservedAt: loop.success["Pod"]}
	previousNext := loop.next["Pod"]
	previousSuccess := loop.success["Pod"]
	if err := service.captureCycle(store, nil, snapshot, []string{"Pod"}, now); err != nil || !loop.next["Pod"].Equal(previousNext) || !loop.success["Pod"].Equal(previousSuccess) {
		t.Fatal("cached read advanced coverage or postponed capture")
	}
	page, err := store.ReadWindow(now.Add(-time.Hour), now.Add(time.Minute), now)
	if err != nil || len(page.Gaps) != 1 || page.Coverage["Pod"].From.IsZero() {
		t.Fatalf("missed-cycle gap: %+v %v", page, err)
	}
	if err := store.ClaimLease("other", now.Add(3*time.Minute), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := service.captureCycle(store, nil, snapshot, []string{"Pod"}, now); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal("stale capture committed after takeover")
	}
	loop.owner = "other"
	if err := service.captureCycle(store, nil, snapshot, []string{"Pod"}, now, "owner"); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal("old refresh committed under reclaimed ownership")
	}
}

func TestMetadataNegotiationFailsClosedAndSecretReprobeCoalesces(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			reads.Add(1)
			http.Error(writer, "CANARY_SECRET", http.StatusForbidden)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	for _, scoped := range []*Service{service, service.Scoped(Scope{ClusterID: "cluster", OrgID: "other"})} {
		if _, _, err := scoped.loadIndexKind(t.Context(), "Secret"); !errors.Is(err, ErrForbidden) {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("Secret denial not coalesced: %d", reads.Load())
	}
	full := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, map[string]any{"kind": "SecretList", "items": []any{map[string]any{"data": map[string]any{"token": "CANARY_SECRET"}}}})
	}))
	defer full.Close()
	fullService := newTestService(t, full.URL, Scope{ClusterID: "cluster"})
	var output metadataIndexPage
	if err := fullService.client.GetMetadataJSON(t.Context(), "/api/v1/secrets", &output); err == nil || len(output.Items) != 0 || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("full object response accepted: %+v %v", output, err)
	}
}

func TestCaptureDemandAndNoCASShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("demand capture made an upstream request")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	if err := service.SetCaptureMode("demand"); err != nil {
		t.Fatal(err)
	}
	registry := NewServiceRegistry(service)
	registry.StartCapture(t.Context())
	registry.Close()
	registry.Close()
	if service.CaptureStatus().Mode != "demand" {
		t.Fatal("demand opt-out ignored")
	}
	if _, err := registry.ResolveCluster("default", "cluster"); !errors.Is(err, ErrClusterUnavailable) {
		t.Fatal("closed registry resolved credentials")
	}
	if err := service.SetCaptureMode("invalid"); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("invalid capture mode accepted")
	}
	second := newTestService(t, server.URL, Scope{ClusterID: "second"})
	second.StartCapture(t.Context())
	second.StopCapture()
	if second.CaptureStatus().Mode != "demand" {
		t.Fatal("non-CAS runtime did not fall back")
	}
}

func TestCaptureRateSharedAcrossScopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeJSON(writer, map[string]any{}) }))
	defer server.Close()
	first := newTestService(t, server.URL, Scope{ClusterID: "cluster", OrgID: "first"})
	second := first.Scoped(Scope{ClusterID: "cluster", OrgID: "second"})
	if first.client != second.client || captureCadences["Pod"] != 30*time.Second || captureCadences["Job"] != time.Minute || captureCadences["ConfigMap"] != 5*time.Minute {
		t.Fatal("capture cadence or shared limiter contract")
	}
	started := time.Now()
	for position := range 8 {
		service := first
		if position%2 == 0 {
			service = second
		}
		var output any
		if err := service.client.GetJSON(captureReadContext(t.Context(), "owner"), "/api", &output); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed < 1800*time.Millisecond {
		t.Fatalf("scopes multiplied rate budget: %s", elapsed)
	}
}

func TestCaptureLimiterPreservesForegroundDeadline(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		writeJSON(writer, map[string]any{})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	if !service.client.indexLimiter.AllowN(time.Now(), 4) {
		t.Fatal("could not exhaust capture burst")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var output any
	if err := service.client.GetJSON(ctx, "/api", &output); err != nil || reads.Load() != 1 {
		t.Fatalf("foreground deadline consumed capture budget: %v reads=%d", err, reads.Load())
	}
	if err := service.client.GetJSON(captureReadContext(ctx, "owner"), "/api", &output); err == nil || reads.Load() != 1 {
		t.Fatalf("background retry bypassed capture budget: %v reads=%d", err, reads.Load())
	}
}

func TestCaptureIndexRefreshThrottlesActualUpstreamReads(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.client.indexLimiter = rate.NewLimiter(0, 4)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	snapshot, status, err := service.IndexSnapshot(captureReadContext(ctx, "owner"), "Pod")
	if err != nil || status.State != "ready" || len(snapshot.Records) != 1 || reads.Load() != 4 || service.client.indexLimiter.Allow() {
		t.Fatalf("capture discovery/list did not consume actual read budget: %+v %v reads=%d", status, err, reads.Load())
	}
	var output any
	if err := service.client.GetJSON(captureReadContext(ctx, "owner"), "/api", &output); err == nil || reads.Load() != 4 {
		t.Fatalf("capture retry bypassed exhausted budget: %v reads=%d", err, reads.Load())
	}
	if err := service.client.GetJSON(ctx, "/api", &output); err != nil || reads.Load() != 5 {
		t.Fatalf("foreground inherited capture exhaustion: %v reads=%d", err, reads.Load())
	}
}

func TestIndexRefreshRetainsInitiatingReadOwnership(t *testing.T) {
	for _, background := range []bool{false, true} {
		name := "foreground"
		if background {
			name = "background"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var releaseOnce sync.Once
			releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api":
					once.Do(func() { close(started) })
					select {
					case <-release:
					case <-request.Context().Done():
						return
					}
					writeJSON(writer, map[string]any{"versions": []string{"v1"}})
				case "/apis":
					writeJSON(writer, map[string]any{"groups": []any{}})
				case "/api/v1":
					writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
				default:
					writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
				}
			}))
			defer server.Close()
			defer releaseRefresh()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
			ownedContext, cancelOwned := context.WithCancel(t.Context())
			defer cancelOwned()
			service.changes.captures[service.indexScope()] = &captureLoop{ctx: t.Context(), owned: true, owner: "owner", ownedContext: ownedContext, status: CaptureStatus{Mode: "continuous"}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if background {
				ctx = captureReadContext(ctx, "owner")
			} else if !service.client.indexLimiter.AllowN(time.Now(), 4) {
				t.Fatal("could not exhaust capture burst")
			}
			result := make(chan error, 1)
			go func() {
				_, _, err := service.IndexSnapshot(ctx, "Pod")
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("initiating discovery did not start")
			}
			service.changes.mu.Lock()
			call := service.changes.refreshes[indexRefreshKey{scope: service.indexScope(), background: background}]
			service.changes.mu.Unlock()
			joinedContext, cancelJoined := context.WithCancel(t.Context())
			cancelJoined()
			if background {
				joinedContext = captureReadContext(joinedContext, "other-owner")
			}
			if _, _, err := service.IndexSnapshot(joinedContext, "Pod"); !errors.Is(err, context.Canceled) {
				t.Fatalf("coalesced caller cancellation: %v", err)
			}
			service.changes.mu.Lock()
			coalesced := service.changes.refreshes[indexRefreshKey{scope: service.indexScope(), background: background}] == call && len(service.changes.refreshes) == 1
			service.changes.mu.Unlock()
			if !coalesced {
				t.Fatal("same ownership group did not coalesce")
			}
			cancelOwned()
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation: %v", err)
			}
			select {
			case <-call.done:
				if !background {
					t.Fatal("foreground refresh inherited ownership cancellation")
				}
			case <-time.After(50 * time.Millisecond):
				if background {
					t.Fatal("background refresh ignored ownership cancellation")
				}
			}
			releaseRefresh()
			select {
			case <-call.done:
			case <-time.After(time.Second):
				t.Fatal("refresh did not drain")
			}
			if !background {
				snapshot, status, err := service.IndexSnapshot(t.Context(), "Pod")
				if err != nil || status.State != "ready" || len(snapshot.Records) != 1 {
					t.Fatalf("foreground discovery lost result: %+v %v", status, err)
				}
			}
		})
	}
}

func TestIndexSnapshotSeparatesForegroundAndCaptureKinds(t *testing.T) {
	for _, backgroundFirst := range []bool{true, false} {
		for _, depleted := range []bool{true, false} {
			name := "foreground-first"
			if backgroundFirst {
				name = "background-first"
			}
			if depleted {
				name += "/depleted"
			} else {
				name += "/blocked-list"
			}
			t.Run(name, func(t *testing.T) {
				foregroundStarted := make(chan struct{})
				captureStarted := make(chan struct{})
				releaseForeground := make(chan struct{})
				var foregroundOnce sync.Once
				var captureOnce sync.Once
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseForeground) }) }
				var captureReads atomic.Int32
				var foregroundCanceled atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					switch request.URL.Path {
					case "/api":
						writeJSON(writer, map[string]any{"versions": []string{"v1"}})
					case "/apis":
						writeJSON(writer, map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}}}})
					case "/api/v1":
						writeJSON(writer, map[string]any{"resources": []any{
							map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}},
							map[string]any{"name": "services", "kind": "Service", "namespaced": true, "verbs": []string{"get", "list"}},
						}})
					case "/apis/apps/v1":
						writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}}})
					case "/api/v1/pods", "/apis/apps/v1/deployments":
						captureReads.Add(1)
						captureOnce.Do(func() { close(captureStarted) })
						<-request.Context().Done()
					case "/api/v1/services":
						foregroundOnce.Do(func() { close(foregroundStarted) })
						select {
						case <-releaseForeground:
							writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "Service", "metadata": map[string]any{"name": "api", "namespace": "default", "uid": "service-api"}}}})
						case <-request.Context().Done():
							foregroundCanceled.Store(true)
						}
					default:
						http.NotFound(writer, request)
					}
				}))
				defer server.Close()
				service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
				if _, err := service.Discover(t.Context()); err != nil {
					t.Fatal(err)
				}
				service.SetChangeStorage(storage.NewMemory())
				service.client.indexLimiter = rate.NewLimiter(rate.Every(10*time.Second), 1)
				if depleted && !service.client.indexLimiter.Allow() {
					t.Fatal("could not exhaust capture burst")
				}
				captureContext, cancelCapture := context.WithCancel(t.Context())
				defer cancelCapture()
				defer release()
				service.changes.lifecycle = t.Context()
				service.changes.captures[service.indexScope()] = &captureLoop{
					ctx: captureContext, ownedContext: captureContext, owned: true, owner: "owner",
					status: CaptureStatus{Mode: "continuous"}, completed: map[string]time.Time{},
				}
				if err := service.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
					t.Fatal(err)
				}
				foregroundResult := make(chan error, 1)
				startForeground := func() {
					go func() {
						snapshot, status, err := service.IndexSnapshot(t.Context(), "Service")
						if err == nil && (status.Kinds["Service"].State != "ready" || len(snapshot.Records) != 1 || snapshot.Records[0].Kind != "Service") {
							err = errors.New("foreground Service did not become fresh")
						}
						foregroundResult <- err
					}()
				}
				startCapture := func() {
					go func() {
						_, _, _ = service.IndexSnapshot(captureReadContext(captureContext, "owner"), "Pod", "Deployment")
					}()
				}
				waitStarted := func(started <-chan struct{}, label string) {
					t.Helper()
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatalf("%s did not start independently", label)
					}
				}
				waitCapture := func() *indexRefreshCall {
					t.Helper()
					timer := time.NewTimer(time.Second)
					defer timer.Stop()
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						service.changes.mu.Lock()
						call := service.changes.refreshes[indexRefreshKey{scope: service.indexScope(), background: true}]
						started := call != nil && len(call.requested) == 0
						service.changes.mu.Unlock()
						if started {
							return call
						}
						select {
						case <-timer.C:
							t.Fatal("capture did not get its own refresh group")
						case <-ticker.C:
						}
					}
				}
				if backgroundFirst {
					startCapture()
					waitCapture()
					if !depleted {
						waitStarted(captureStarted, "capture LIST")
					}
					startForeground()
				} else {
					startForeground()
					waitStarted(foregroundStarted, "foreground LIST")
					startCapture()
				}
				call := waitCapture()
				waitStarted(foregroundStarted, "foreground LIST")
				if !depleted {
					waitStarted(captureStarted, "capture LIST")
				}
				select {
				case <-call.done:
					t.Fatal("capture bypassed pending limiter or upstream read")
				case <-time.After(50 * time.Millisecond):
				}
				wantReads := int32(1)
				if depleted {
					wantReads = 0
				}
				if captureReads.Load() != wantReads {
					t.Fatalf("capture bypassed limiter: reads=%d want=%d", captureReads.Load(), wantReads)
				}
				cancelCapture()
				waitStarted(call.done, "canceled capture drain")
				release()
				select {
				case err := <-foregroundResult:
					if err != nil || foregroundCanceled.Load() {
						t.Fatalf("foreground inherited capture ownership: %v canceled=%v", err, foregroundCanceled.Load())
					}
				case <-time.After(time.Second):
					t.Fatal("foreground did not finish independently")
				}
			})
		}
	}
}

func TestForegroundPodRetriesCanceledCaptureLoad(t *testing.T) {
	started := make(chan struct{})
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		default:
			if reads.Add(1) == 1 {
				close(started)
				<-request.Context().Done()
				return
			}
			writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	if _, err := service.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	owned, cancelOwned := context.WithCancel(t.Context())
	defer cancelOwned()
	go service.IndexSnapshot(captureReadContext(owned, "owner"), "Pod")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("capture Pod list did not start")
	}
	result := make(chan error, 1)
	go func() {
		snapshot, status, err := service.IndexSnapshot(t.Context(), "Pod")
		if err == nil && (status.State != "ready" || len(snapshot.Records) != 1) {
			err = errors.New("foreground inherited canceled capture result")
		}
		result <- err
	}()
	cancelOwned()
	select {
	case err := <-result:
		if err != nil || reads.Load() != 2 {
			t.Fatalf("foreground retry: %v reads=%d", err, reads.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground Pod retry did not finish")
	}
}

func TestCaptureRetryAfterSharedAcrossScopes(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		writer.Header().Set("Retry-After", "2")
		http.Error(writer, "CANARY_BODY", http.StatusTooManyRequests)
	}))
	defer server.Close()
	first := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	var output any
	if err := first.client.GetJSON(t.Context(), "/api", &output); err == nil {
		t.Fatal("throttle accepted")
	}
	second := first.Scoped(Scope{ClusterID: "cluster", OrgID: "second"})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := second.client.GetJSON(ctx, "/api", &output); err == nil || reads.Load() != 1 {
		t.Fatalf("another scope bypassed Retry-After: %v reads=%d", err, reads.Load())
	}
}

func TestCaptureReadLimiterCancellationDoesNotPoisonOtherReads(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		writeJSON(writer, map[string]any{})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	if !service.client.indexLimiter.AllowN(time.Now(), 4) {
		t.Fatal("could not exhaust initial read burst")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output any
	if err := service.client.GetJSON(captureReadContext(ctx, "owner"), "/api", &output); !errors.Is(err, context.Canceled) || reads.Load() != 0 {
		t.Fatalf("canceled read reached transport: %v", err)
	}
	if err := service.Scoped(Scope{ClusterID: "cluster", OrgID: "other"}).client.GetJSON(captureReadContext(t.Context(), "owner"), "/api", &output); err != nil || reads.Load() != 1 {
		t.Fatalf("canceled queue poisoned subsequent read: %v", err)
	}
}

func TestCaptureDeltaDrainPreservesDemandLifecycle(t *testing.T) {
	now := time.Now().UTC()
	for _, continuous := range []bool{false, true} {
		deltas := make(chan kubeindex.Delta, 2)
		for position := range 2 {
			pod := kubeindex.Record{Kind: "Pod", Name: "api-" + strconv.Itoa(position), UID: "pod-" + strconv.Itoa(position), Owners: []kubeindex.OwnerRef{{Kind: "ReplicaSet", Name: "api", UID: "rs"}}}
			deltas <- kubeindex.Delta{New: &pod}
		}
		items, gaps := drainIndexChanges("cluster", deltas, now, now, continuous)
		if len(items) != 2 || len(gaps) != 0 || items[0].Coalesced != continuous || (items[0].ID == items[1].ID) != continuous {
			t.Fatalf("demand/continuous routing: continuous=%v items=%+v gaps=%v", continuous, items, gaps)
		}
	}
}

type renewalFaultProvider struct {
	storage.Provider
	failRead atomic.Bool
}

func (provider *renewalFaultProvider) ReadBlob(name string) ([]byte, error) {
	if provider.failRead.Swap(false) {
		return nil, errors.New("transient storage failure")
	}
	return provider.Provider.ReadBlob(name)
}

func (provider *renewalFaultProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func TestCaptureRenewFailureAllowsInteractiveRefreshAndReclaim(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "transient"
		if takeover {
			name = "takeover"
		}
		t.Run(name, func(t *testing.T) {
			provider := &renewalFaultProvider{Provider: storage.NewMemory()}
			canceled := make(chan struct{})
			var first atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api" && first.CompareAndSwap(false, true) {
					provider.failRead.Store(true)
					<-request.Context().Done()
					close(canceled)
					return
				}
				switch request.URL.Path {
				case "/api":
					writeJSON(writer, map[string]any{"versions": []string{"v1"}})
				case "/apis":
					writeJSON(writer, map[string]any{"groups": []any{}})
				case "/api/v1":
					writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
				default:
					writeJSON(writer, map[string]any{"items": []any{podFixture("api")}})
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
			service.client.indexLimiter = rate.NewLimiter(rate.Inf, 4)
			service.SetChangeStorage(provider)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			loop := &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: "initial", leaseTTL: 300 * time.Millisecond, renewInterval: 50 * time.Millisecond, next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous"}}
			service.changes.captures[service.indexScope()] = loop
			service.changes.lifecycle = ctx
			go service.capture(loop)
			defer service.StopCapture()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("renewal failure did not cancel owned read")
			}
			if ctx.Err() != nil {
				t.Fatal("renewal failure canceled capture lifetime")
			}
			other := kubechanges.NewStore(provider, service.indexScope())
			if takeover {
				if err := other.ClaimLease("other", time.Now().Add(time.Second), time.Minute); err != nil {
					t.Fatal(err)
				}
				if err := service.changeStore().Commit(nil, nil, nil, "initial", time.Now()); !errors.Is(err, kubechanges.ErrLeaseLost) {
					t.Fatal("takeover did not fence old owner")
				}
			}
			service.changes.mu.Lock()
			refresh := service.changes.refreshes[indexRefreshKey{scope: service.indexScope(), background: true}]
			service.changes.mu.Unlock()
			if refresh != nil {
				select {
				case <-refresh.done:
				case <-time.After(3 * time.Second):
					t.Fatal("lost-owner refresh did not drain")
				}
			}
			snapshot, status, err := service.IndexSnapshot(t.Context(), "Pod")
			if err != nil || status.Kinds["Pod"].State != "ready" || len(snapshot.Records) == 0 {
				t.Fatalf("interactive refresh inherited canceled ownership: %+v %v", status, err)
			}
			if takeover {
				if err := other.ReleaseLease("other"); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				loop.mu.Lock()
				reclaimed := loop.owned && loop.owner != "initial"
				loop.mu.Unlock()
				if reclaimed {
					break
				}
				select {
				case <-deadline.C:
					t.Fatal("capture did not reclaim after ownership loss")
				case <-ticker.C:
				}
			}
		})
	}
}

func TestMetadataListAllowsOmittedItemTypeMetaButRejectsValues(t *testing.T) {
	for _, test := range []struct {
		item    map[string]any
		allowed bool
	}{
		{map[string]any{"metadata": map[string]any{"name": "cfg", "uid": "cfg", "resourceVersion": "42"}}, true},
		{map[string]any{"metadata": map[string]any{"name": "cfg"}, "stringData": map[string]any{"token": "CANARY_SECRET"}}, false},
		{map[string]any{"kind": "Secret", "metadata": map[string]any{"name": "cfg"}}, false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeJSON(writer, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": []any{test.item}})
		}))
		service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
		var page metadataIndexPage
		err := service.client.GetMetadataJSON(t.Context(), "/api/v1/configmaps", &page)
		server.Close()
		if (err == nil) != test.allowed || err != nil && strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("negotiated metadata: allowed=%v err=%v", test.allowed, err)
		}
	}
}

func TestSecretDenialDoesNotHideAuthorizedNamespaceMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/secrets":
			http.Error(writer, "forbidden", http.StatusForbidden)
		case "/api/v1/namespaces/shop/secrets":
			writeJSON(writer, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": []any{map[string]any{"metadata": map[string]any{"name": "secret", "namespace": "shop", "uid": "secret", "resourceVersion": "42"}}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	if _, _, err := service.loadIndexKind(t.Context(), "Secret"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	records, complete, err := service.loadIndexKind(t.Context(), "Secret", "shop")
	if err != nil || !complete || len(records) != 1 || records[0].ResourceVersion != "42" {
		t.Fatalf("authorized metadata suppressed: %+v %v", records, err)
	}
}
