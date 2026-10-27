//go:build integration

package fakekube

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/gofiber/fiber/v2"
)

// The real fixture serves discovery, indexed objects and streaming log reads.
// No transport or response is replaced by a test handler.
func workloadBackend(t *testing.T, kind string, pods int, decorate ...func(*Server) http.Handler) (*Server, *kubernetes.Service, context.Context) {
	t.Helper()
	api, err := NewServer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	workloadObject(t, api, kind, "orders", "root", "", "", "")
	if kind == "Deployment" {
		for _, revision := range []string{"old", "new"} {
			workloadObject(t, api, "ReplicaSet", revision, revision, kind, "orders", "root")
		}
	}
	for i := 0; i < pods; i++ {
		ownerKind, ownerName, ownerUID := kind, "orders", "root"
		if kind == "Deployment" {
			ownerKind, ownerName, ownerUID = "ReplicaSet", "new", "new"
			if i == 0 {
				ownerName, ownerUID = "old", "old"
			}
		}
		workloadObject(t, api, "Pod", fmt.Sprintf("orders-%d", i), fmt.Sprintf("pod-%d", i), ownerKind, ownerName, ownerUID)
	}
	workloadObject(t, api, "Pod", "same-label-stranger", "stranger", kind, "orders", "other-root")
	var handler http.Handler = api
	if len(decorate) != 0 {
		handler = decorate[0](api)
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}

	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "fixture", CredentialID: "fixture"}, time.Minute)
	redactor, errs := agent.NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	service.SetScrubber(redactor)
	registry := kubernetes.NewServiceRegistry(service)
	t.Cleanup(registry.Close)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{
		Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
	})
	return api, service, ctx
}

// Enable the native exact-line fixture for followed reads as well.
func exactWorkloadBackend(api *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") && api.serveLogFixture(w, r) {
			return
		}
		api.ServeHTTP(w, r)
	})
}

func workloadObject(t *testing.T, api *Server, kind, name, uid, ownerKind, ownerName, ownerUID string) {
	t.Helper()
	resources := map[string]string{"Deployment": "deployments", "StatefulSet": "statefulsets", "ReplicaSet": "replicasets", "Pod": "pods"}
	meta := map[string]any{"uid": uid, "labels": map[string]any{"app": "orders"}}
	if ownerUID != "" {
		meta["ownerReferences"] = []any{map[string]any{"apiVersion": "apps/v1", "kind": ownerKind, "name": ownerName, "uid": ownerUID, "controller": true}}
	}
	object := map[string]any{"apiVersion": "apps/v1", "kind": kind, "metadata": meta}
	if kind == "Pod" {
		object["apiVersion"] = "v1"
		object["spec"] = map[string]any{
			"containers":     []any{map[string]any{"name": "app", "image": "fixture/app"}, map[string]any{"name": "sidecar", "image": "fixture/sidecar"}},
			"initContainers": []any{map[string]any{"name": "setup", "image": "fixture/setup"}},
		}
		statuses := []any{}
		for _, container := range []string{"app", "sidecar"} {
			statuses = append(statuses, map[string]any{
				"name": container, "ready": true, "restartCount": 1,
				"state":     map[string]any{"running": map[string]any{"startedAt": "2026-10-01T00:00:00Z"}},
				"lastState": map[string]any{"terminated": map[string]any{"exitCode": 1, "finishedAt": "2026-09-30T23:59:00Z"}},
			})
		}
		object["status"] = map[string]any{
			"phase": "Running", "containerStatuses": statuses,
			"initContainerStatuses": []any{map[string]any{"name": "setup", "restartCount": 1,
				"state":     map[string]any{"terminated": map[string]any{"exitCode": 0, "finishedAt": "2026-10-01T00:00:00Z"}},
				"lastState": map[string]any{"terminated": map[string]any{"exitCode": 1, "finishedAt": "2026-09-30T23:58:00Z"}}}},
		}
	}
	if err := api.store.upsertObject(resources[kind], "shop", name, object); err != nil {
		t.Fatal(err)
	}
}

func awaitNoWorkloadReads(t *testing.T, api *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		active := int64(0)
		for _, counter := range api.logStreamCounters() {
			active += counter.Active
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream reads leaked: %+v", api.logStreamCounters())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkloadBackendContinuousMembershipAndUIDReplacement(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			api, service, authorized := workloadBackend(t, kind, 3)
			ctx, cancel := context.WithTimeout(authorized, 48*time.Second)
			defer cancel()
			initial := map[string]bool{}
			changed, replacementSeen := false, false
			var removedSources map[string]bool
			var retiredRequests int64
			ownerKind, ownerName, ownerUID := kind, "orders", "root"
			if kind == "Deployment" {
				ownerKind, ownerName, ownerUID = "ReplicaSet", "new", "new"
			}
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: kind, Name: "orders", Namespace: "shop", UID: "root", Timestamps: true,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.PodUID == "stranger" {
					t.Error("same-label stranger crossed the UID ownership boundary")
				}
				if event.Event == "sources" && changed {
					for _, pod := range event.Pods {
						if pod.UID == "pod-2" || pod.UID == "pod-1" {
							return nil // Still the shared index's valid pre-refresh snapshot.
						}
					}
					removedSources = map[string]bool{"pod-1": true, "pod-2": true}
				}
				if event.Event != "line" {
					return nil
				}
				if event.Container != "app" {
					t.Errorf("default container was %q", event.Container)
				}
				if removedSources[event.PodUID] {
					t.Errorf("retired UID emitted after refreshed membership: %s", event.PodUID)
				}
				if !changed {
					initial[event.PodUID] = true
					if len(initial) == 3 {
						changed = true
						if !api.store.Delete("pods", "shop", "orders-2") {
							t.Fatal("fixture Pod to delete was missing")
						}
						workloadObject(t, api, "Pod", "orders-1", "replacement", ownerKind, ownerName, ownerUID)
						workloadObject(t, api, "Pod", "orders-3", "joined", ownerKind, ownerName, ownerUID)
						for _, counter := range api.logStreamCounters() {
							if counter.Path == "/api/v1/namespaces/shop/pods/orders-2/log" {
								retiredRequests += counter.Requests
							}
						}
					}
				}
				if event.PodUID == "replacement" {
					replacementSeen = true
				}
				if event.PodUID == "joined" && replacementSeen && removedSources != nil {
					cancel()
				}
				return nil
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("stream: %v", err)
			}
			if ctx.Err() == context.DeadlineExceeded || !replacementSeen || removedSources == nil {
				t.Fatalf("continuous membership not observed: initial=%v replacement=%v retired=%v", initial, replacementSeen, removedSources)
			}
			awaitNoWorkloadReads(t, api)
			for _, counter := range api.logStreamCounters() {
				if counter.Path == "/api/v1/namespaces/shop/pods/orders-2/log" && counter.Requests != retiredRequests {
					t.Errorf("deleted Pod was reopened: %+v", counter)
				}
				if counter.Path == "/api/v1/namespaces/shop/pods/same-label-stranger/log" {
					t.Error("stranger was read")
				}
			}
			t.Logf("three initial Pods, join/delete/same-name UID replacement on one stream; cleanup=%+v", api.logStreamCounters())
		})
	}
}

func TestWorkloadBackendFiniteContainersPartialFailureAndCaps(t *testing.T) {
	for _, selection := range []string{"", "*", "setup"} {
		t.Run("container="+selection, func(t *testing.T) {
			api, service, authorized := workloadBackend(t, "Deployment", 23)
			unavailable := false
			api.logControls[logStreamKey{Path: "/api/v1/namespaces/shop/pods/orders-0/log", Container: "app"}] =
				logControl{PreviousAvailable: &unavailable}
			ctx, cancel := context.WithTimeout(authorized, 12*time.Second)
			defer cancel()
			lines, failures, admittedPods, admittedSources := 0, 0, 0, 0
			ended := false
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "Deployment", Name: "orders", Namespace: "shop", UID: "root", Container: selection, Previous: true, Timestamps: true,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				switch event.Event {
				case "sources":
					if len(event.Pods) != 23 {
						t.Errorf("pod catalog=%d, want 23 (not the stranger)", len(event.Pods))
					}
					admittedPods, admittedSources = 0, 0
					for _, pod := range event.Pods {
						if pod.Admitted {
							admittedPods++
						}
					}
					for _, source := range event.Sources {
						if source.Admitted {
							admittedSources++
						}
					}
				case "line":
					lines++
					if selection == "" && event.Container != "app" || selection == "setup" && event.Container != "setup" {
						t.Errorf("unexpected container: %s", event.Container)
					}
				case "source_status":
					failures++
				case "end":
					ended = true
				}
				return nil
			})
			if err != nil || !ended || ctx.Err() != nil || lines == 0 {
				t.Fatalf("finite Previous: err=%v ended=%v context=%v lines=%d", err, ended, ctx.Err(), lines)
			}
			if admittedPods > 20 || admittedPods == 0 || admittedSources > 40 || admittedSources == 0 {
				t.Fatalf("admission pods=%d sources=%d", admittedPods, admittedSources)
			}
			if selection == "*" && admittedSources != 40 {
				t.Errorf("all-container source cap=%d, want 40", admittedSources)
			}
			if selection != "setup" && failures == 0 {
				t.Error("unavailable child was not surfaced while healthy children completed")
			}
			awaitNoWorkloadReads(t, api)
			for _, counter := range api.logStreamCounters() {
				if counter.Follow {
					t.Errorf("Previous unexpectedly followed: %+v", counter)
				}
			}
			t.Logf("finite Previous container=%q pods=%d sources=%d lines=%d statuses=%d", selection, admittedPods, admittedSources, lines, failures)
		})
	}
}

func TestWorkloadBackendHistoryFallbackAndAdmissionDeny(t *testing.T) {
	api, service, authorized := workloadBackend(t, "StatefulSet", 3)
	options := kubernetes.WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root"}
	if err := service.ValidateWorkloadLogStream(context.Background(), options); !errors.Is(err, kubernetes.ErrForbidden) {
		t.Fatalf("default deny: %v", err)
	}
	options.PodUIDs = []string{"stranger"}
	if err := service.ValidateWorkloadLogStream(authorized, options); err == nil {
		t.Fatal("same-label foreign Pod filter admitted")
	}
	history, err := service.UsageHistory(authorized, "shop", "orders-0")
	if err != nil || history.Availability != "unavailable" || len(history.Samples) != 0 || history.LiveSince.IsZero() || history.RetentionSeconds != 21600 {
		t.Fatalf("live fallback=%+v err=%v", history, err)
	}
	if len(api.logStreamCounters()) != 0 {
		t.Fatal("denied admissions or history fallback opened upstream log reads")
	}
	t.Logf("default deny, foreign UID rejection before reads, explicit 6h empty-history live fallback: %+v", history)
}

// A viewer that reconnects carries a timestamp, not resume state. Every source
// reopens from that timestamp, the backend filters on it, and the stream itself
// never carries resume state back.
func TestWorkloadBackendTimestampReconnect(t *testing.T) {
	api, service, authorized := workloadBackend(t, "Deployment", 3)
	options := kubernetes.WorkloadLogStreamOptions{
		Kind: "Deployment", Name: "orders", Namespace: "shop", UID: "root", Timestamps: true,
	}
	stopped := errors.New("watermark collected")
	ctx, cancel := context.WithTimeout(authorized, 18*time.Second)
	defer cancel()
	lines := 0
	err := service.StreamWorkloadLogs(ctx, options, func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.Cursor != "" {
			t.Errorf("workload event carried resume state: %+v", event)
		}
		if event.Event != "line" {
			return nil
		}
		if _, parseErr := time.Parse(time.RFC3339Nano, event.Timestamp); parseErr != nil {
			t.Fatalf("line timestamp: %v", parseErr)
		}
		lines++
		if lines >= 128 {
			return stopped
		}
		return nil
	})
	if !errors.Is(err, stopped) {
		t.Fatalf("first read: err=%v lines=%d", err, lines)
	}
	awaitNoWorkloadReads(t, api)
	// The viewer resumes from the last line it rendered, backed off by the replay
	// overlap. The fixture serves its own synthetic clock, so the reconnect is
	// asserted on what reached the backend rather than on new lines.
	watermark := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	options.SinceTime = watermark
	resumeContext, stopResume := context.WithTimeout(authorized, 3*time.Second)
	defer stopResume()
	err = service.StreamWorkloadLogs(resumeContext, options, func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.Cursor != "" {
			t.Errorf("reconnect emitted resume state: %+v", event)
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconnect: err=%v", err)
	}
	awaitNoWorkloadReads(t, api)
	want := watermark.Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	counters := api.logStreamCounters()
	if len(counters) != 3 {
		t.Fatalf("sources=%d, want one read per Pod", len(counters))
	}
	for _, counter := range counters {
		if counter.Requests != 2 || counter.SinceTime != want {
			t.Errorf("source did not resume from the viewer timestamp: %+v want %s", counter, want)
		}
	}
	t.Logf("viewer watermark %s; three sources reopened at sinceTime=%s; no resume state in the stream", watermark.Format(time.RFC3339Nano), want)
}

func TestWorkloadBackendSlowConsumerPreservesTailAndCancels(t *testing.T) {
	api, service, authorized := workloadBackend(t, "StatefulSet", 20)
	ctx, cancel := context.WithTimeout(authorized, 10*time.Second)
	defer cancel()
	slowed := false
	counts := map[string]int{}
	statuses := 0
	err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
		Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root", Timestamps: true,
	}, func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.Event == "line" && !slowed {
			slowed = true
			// Twenty real streams produce enough initial records to fill the
			// bounded shared queue while the consumer is temporarily stalled.
			time.Sleep(1500 * time.Millisecond)
		}
		if event.Event == "source_status" {
			statuses++
		}
		if event.Event == "line" {
			counts[event.PodUID]++
			complete := len(counts) == 20
			for _, count := range counts {
				complete = complete && count >= 5
			}
			if complete {
				cancel()
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("slow consumer: %v", err)
	}
	if len(counts) != 20 || statuses != 0 || ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("lossless slow consumer counts=%v statuses=%d", counts, statuses)
	}
	awaitNoWorkloadReads(t, api)
	t.Log("real20-source slow consumer retains initial records from every Pod without error/gap notices; all upstream reads canceled")
}

func TestWorkloadBackendCurrentAllAndInitContainers(t *testing.T) {
	for _, selection := range []string{"*", "setup"} {
		t.Run("container="+selection, func(t *testing.T) {
			api, service, authorized := workloadBackend(t, "StatefulSet", 3)
			ctx, cancel := context.WithTimeout(authorized, 8*time.Second)
			defer cancel()
			want := 9
			if selection == "setup" {
				want = 3
				direct, err := service.PodLogs(authorized, "shop", "orders-0", "setup", false, 0, 10)
				if err != nil || direct.Text == "" {
					t.Fatalf("native backend must expose completed init Current logs: %v", err)
				}
				t.Log("native finite Current logs for completed init setup are readable")
			}

			seen := map[string]bool{}
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root", Container: selection, Timestamps: true,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.Event != "line" {
					return nil
				}
				if selection == "setup" && event.Container != "setup" {
					t.Errorf("init selection read %q", event.Container)
				}
				seen[event.PodUID+"/"+event.Container] = true
				if len(seen) == want {
					cancel()
				}
				return nil
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("current container stream: %v; distinct sources=%v; upstream=%+v", err, seen, api.logStreamCounters())
			}
			if len(seen) != want || ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("current container=%q got=%v want=%d", selection, seen, want)
			}
			awaitNoWorkloadReads(t, api)
			t.Logf("current container=%q produced %d distinct Pod/container sources; no active reads retained", selection, len(seen))
		})
	}
}

func TestNativeWorkloadStartupTailCompleteness(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous=%v", previous), func(t *testing.T) {
			api, service, authorized := workloadBackend(t, "StatefulSet", 20, exactWorkloadBackend)
			for pod := 0; pod < 20; pod++ {
				path := fmt.Sprintf("/api/v1/namespaces/shop/pods/orders-%d/log", pod)
				fixture := logFixture{Path: path, Container: "app", Previous: previous}
				for line := 0; line < 500; line++ {
					fixture.Lines = append(fixture.Lines, logFixtureLine{
						At:   time.Now().UTC().Add(-time.Minute).Truncate(time.Second),
						Text: fmt.Sprintf("record-%03d", line),
					})
				}
				api.logFixtures[logStreamKey{Path: path, Container: "app", Previous: previous}] = fixture
			}
			ctx, cancel := context.WithTimeout(authorized, 15*time.Second)
			defer cancel()
			stop := errors.New("complete startup received")
			seen := make(map[string]bool)
			counts := make(map[string]int)
			statuses, ended := 0, false
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root",
				TailLines: 500, Previous: previous, Timestamps: true,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.Event == "source_status" {
					statuses++
				}
				if event.Event == "end" {
					ended = true
				}
				if event.Event != "line" {
					return nil
				}
				key := event.PodUID + "/" + event.Text
				if seen[key] {
					t.Errorf("duplicate %s", key)
				}
				seen[key] = true
				counts[event.PodUID]++
				if len(seen) == 1 {
					time.Sleep(40 * time.Millisecond)
				}
				if len(seen)%64 == 0 {
					time.Sleep(100 * time.Microsecond)
				}
				if len(seen) == 10000 && !previous {
					return stop
				}
				return nil
			})
			if previous && (err != nil || !ended) || !previous && !errors.Is(err, stop) || len(seen) != 10000 || statuses != 0 {
				t.Fatalf("startup previous=%v unique=%d statuses=%d ended=%v err=%v", previous, len(seen), statuses, ended, err)
			}
			for pod := 0; pod < 20; pod++ {
				if counts[fmt.Sprintf("pod-%d", pod)] != 500 {
					t.Errorf("pod-%d retained %d/500", pod, counts[fmt.Sprintf("pod-%d", pod)])
				}
			}
			awaitNoWorkloadReads(t, api)
			t.Logf("previous=%v: all 10000 unique records, exactly500 per Pod, statuses0, active0", previous)
		})
	}
}

func TestNativeWorkloadStaleIndexCannotReopenReplacement(t *testing.T) {
	api, service, authorized := workloadBackend(t, "StatefulSet", 1, exactWorkloadBackend)
	path := "/api/v1/namespaces/shop/pods/orders-0/log"
	api.logFixtures[logStreamKey{Path: path, Container: "app"}] = logFixture{
		Path: path, Container: "app", Lines: []logFixtureLine{{At: time.Now().UTC().Add(-time.Minute), Text: "original-only"}},
	}
	ctx, cancel := context.WithTimeout(authorized, 9*time.Second)
	defer cancel()
	lines := 0
	timer := time.AfterFunc(6500*time.Millisecond, cancel)
	defer timer.Stop()
	err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
		Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root",
	}, func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.Event == "line" {
			lines++
			if lines == 1 {
				workloadObject(t, api, "Pod", "orders-0", "replacement", "StatefulSet", "orders", "root")
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	gets := int64(0)
	for _, counter := range api.requestCounters() {
		if counter.Path == strings.TrimSuffix(path, "/log") && counter.Verb == "GET" {
			gets += counter.Requests
		}
	}
	if gets < 2 || lines != 1 || ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("stale retry lines=%d validation GETs=%d", lines, gets)
	}
	for _, counter := range api.logStreamCounters() {
		if counter.Requests != 1 {
			t.Errorf("stale identity reopened replacement log path: %+v", counter)
		}
	}
	awaitNoWorkloadReads(t, api)
	t.Log("replacement rejected by native Pod GET before stale-index retry opens /log; only original line read")
}

func TestNativeScenarioWorkloadOwnership(t *testing.T) {
	for _, scenario := range []string{"triage", "populated"} {
		t.Run(scenario, func(t *testing.T) {
			api, err := NewServer(Config{Scenario: scenario, Seed: 1})
			if err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(api)
			defer upstream.Close()
			client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "fixture"}, time.Minute)
			redactor, _ := agent.NewRedactor(false, nil)
			service.SetScrubber(redactor)
			defer kubernetes.NewServiceRegistry(service).Close()
			authorized := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{
				Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
			})
			ctx, cancel := context.WithTimeout(authorized, 5*time.Second)
			defer cancel()
			stop := errors.New("owned fixture Pod emitted")
			err = service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "Deployment", Name: "checkout-api", Namespace: "payments", UID: "deployment-checkout",
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.Event == "line" {
					if event.PodUID != "pod-checkout-0" {
						t.Errorf("unexpected fixture owner: %+v", event)
					}
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatalf("stock %s workload could not reach its Pod: %v", scenario, err)
			}
			awaitNoWorkloadReads(t, api)
			t.Log("stock fixture Deployment -> ReplicaSet -> Pod strict controller UID chain emits through real native stream")
		})
	}
}

func TestNativeForbiddenSourceIsIdentifiedAndQuietAcrossReconciles(t *testing.T) {
	api, service, authorized := workloadBackend(t, "StatefulSet", 3, exactWorkloadBackend)
	path := "/api/v1/namespaces/shop/pods/orders-0/log"
	api.logFixtures[logStreamKey{Path: path, Container: "app"}] = logFixture{Path: path, Container: "app", Status: 403}
	ctx, cancel := context.WithTimeout(authorized, 10600*time.Millisecond)
	defer cancel()
	notices, uncertain, healthy := 0, 0, 0
	err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
		Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root",
	}, func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.ReplayUncertain {
			uncertain++
		}
		if event.Event == "source_status" {
			notices++
			if event.PodUID != "pod-0" || event.Pod != "orders-0" || event.SourceID == "" {
				t.Errorf("unidentified denial: %+v", event)
			}
		}
		if event.Event == "line" {
			if event.PodUID == "pod-0" {
				t.Error("forbidden source emitted a line")
			}
			healthy++
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || notices != 1 || uncertain != 0 || healthy == 0 {
		t.Fatalf("denial notices=%d uncertainty=%d healthy=%d err=%v", notices, uncertain, healthy, err)
	}
	for _, counter := range api.logStreamCounters() {
		if counter.Path == path && counter.Requests != 1 {
			t.Errorf("forbidden source retried: %+v", counter)
		}
	}
	awaitNoWorkloadReads(t, api)
	t.Logf("one identified denial over two5s reconciles; uncertainty0; healthy lines%d; forbidden reads1; active0", healthy)
}

func TestNativeInitialSourceRejectsLiveReplacementBeforeLogRead(t *testing.T) {
	api, service, authorized := workloadBackend(t, "StatefulSet", 1)
	ctx, cancel := context.WithTimeout(authorized, 250*time.Millisecond)
	defer cancel()
	session, err := service.OpenWorkloadLogStream(ctx, kubernetes.WorkloadLogStreamOptions{
		Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	workloadObject(t, api, "Pod", "orders-0", "replacement", "StatefulSet", "orders", "root")
	lines := 0
	err = session.Stream(func(event kubernetes.WorkloadLogStreamEvent) error {
		if event.Event == "line" {
			lines++
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || lines != 0 || len(api.logStreamCounters()) != 0 {
		t.Fatalf("initial stale UID reached /log: lines=%d counters=%+v err=%v", lines, api.logStreamCounters(), err)
	}
	gets := int64(0)
	for _, counter := range api.requestCounters() {
		if counter.Path == "/api/v1/namespaces/shop/pods/orders-0" && counter.Verb == "GET" {
			gets += counter.Requests
		}
	}
	if gets != 1 {
		t.Fatalf("initial live validation GETs=%d want1", gets)
	}
	t.Log("stale initial index ->one live Pod GET ->zero upstream log requests and zero mislabeled lines")
}

type nativeBlobGate struct {
	storage.Provider
	gate     chan struct{}
	calls    atomic.Int32
	active   atomic.Int32
	released sync.Once
}

func (provider *nativeBlobGate) release() { provider.released.Do(func() { close(provider.gate) }) }

func (provider *nativeBlobGate) ReadBlob(key string) ([]byte, error) {
	provider.calls.Add(1)
	provider.active.Add(1)
	defer provider.active.Add(-1)
	<-provider.gate
	return provider.Provider.ReadBlob(key)
}

func (provider *nativeBlobGate) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	provider.calls.Add(1)
	provider.active.Add(1)
	defer provider.active.Add(-1)
	<-provider.gate
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func TestNativeFiftyConcurrentHistoryGETsPersistOffRequestAndJoinWorker(t *testing.T) {
	_, service, _ := workloadBackend(t, "StatefulSet", 50)
	provider := &nativeBlobGate{Provider: storage.NewMemory(), gate: make(chan struct{})}
	defer provider.release()
	service.SetChangeStorage(provider)
	registry := kubernetes.NewServiceRegistry(service)
	app := fiber.New(fiber.Config{Immutable: true, DisableStartupMessage: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	controllers.NewKubernetesAdminControllerWithRegistry(registry).Register(app.Group("/api"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listened := make(chan struct{})
	go func() { defer close(listened); _ = app.Listener(listener) }()
	defer func() {
		provider.release()
		_ = app.ShutdownWithTimeout(time.Second)
		<-listened
		registry.Close()
	}()
	base := "http://" + listener.Addr().String()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 50)
	for pod := 0; pod < 50; pod++ {
		go func(pod int) {
			<-start
			request, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/admin/kubernetes/usage-history?namespace=shop&pod=orders-%d", base, pod), nil)
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				var result kubernetes.UsageHistory
				err = json.NewDecoder(response.Body).Decode(&result)
				response.Body.Close()
				if err == nil && (response.StatusCode != 200 || result.Availability != "unavailable" || result.RetentionSeconds != 21600) {
					err = fmt.Errorf("GET pod%d status%d result%+v", pod, response.StatusCode, result)
				}
			}
			results <- err
		}(pod)
	}
	started := time.Now()
	close(start)
	for pod := 0; pod < 50; pod++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("history GET waited on blocked provider I/O")
		}
	}
	elapsed := time.Since(started)
	workerDeadline := time.Now().Add(time.Second)
	for provider.active.Load() == 0 {
		if time.Now().After(workerDeadline) {
			t.Fatal("off-request worker never entered gated provider I/O")
		}
		time.Sleep(5 * time.Millisecond)
	}
	probeStarted := time.Now()
	probe, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/admin/kubernetes/usage-history?namespace=shop&pod=orders-0", nil)
	probeResponse, err := http.DefaultClient.Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	var probeHistory kubernetes.UsageHistory
	probeErr := json.NewDecoder(probeResponse.Body).Decode(&probeHistory)
	probeResponse.Body.Close()
	if probeErr != nil || probeResponse.StatusCode != 200 {
		t.Fatalf("GET while worker I/O blocked: status%d err%v", probeResponse.StatusCode, probeErr)
	}
	probeElapsed := time.Since(probeStarted)
	// ALL provider blob calls remain physically blocked until every GET returned.
	// A request-path read/CAS would prevent this assertion, regardless of speed.
	provider.release()
	scope := service.ActiveScope()
	digest := sha256.Sum256([]byte(scope.OrgID + "\x00" + scope.ClusterID + "\x00" + scope.CredentialID))
	key := "kubernetes/usage-history/" + base64.RawURLEncoding.EncodeToString(digest[:]) + "/interest"
	deadline := time.Now().Add(3 * time.Second)
	var manifest map[string]json.RawMessage
	for {
		raw, err := provider.Provider.ReadBlob(key)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
		}
		if len(manifest) == 50 {
			break
		}
		if time.Now().After(deadline) {
			blobs, listErr := provider.Provider.ListBlobs("kubernetes/usage-history/")
			names := []string{}
			for _, blob := range blobs {
				names = append(names, fmt.Sprintf("%s bytes%d", blob.Name, len(blob.Data)))
			}
			t.Logf("diagnostic GETs=%s provider calls%d active%d scope=%+v expected=%s blobs=%v listErr=%v", elapsed, provider.calls.Load(), provider.active.Load(), scope, key, names, listErr)
			t.Fatalf("eventually saved %d/50 interests", len(manifest))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for pod := 0; pod < 50; pod++ {
		if _, ok := manifest[fmt.Sprintf("shop/orders-%d", pod)]; !ok {
			t.Errorf("missing persisted interest%d", pod)
		}
	}
	registry.Close()
	calls := provider.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if provider.active.Load() != 0 || provider.calls.Load() != calls {
		t.Fatalf("worker did not join: active%d calls%d->%d", provider.active.Load(), calls, provider.calls.Load())
	}
	t.Logf("50 real controller HTTP GETs returned in%s with provider I/O gated; additional GET in%s while worker physically blocked in provider; eventual manifest50/50; worker joined; active0", elapsed, probeElapsed)
}

func nativeKubernetesAPI(t *testing.T, service *kubernetes.Service) string {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true, DisableStartupMessage: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	controllers.NewKubernetesAdminController(service).Register(app.Group("/api"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = app.Listener(listener) }()
	t.Cleanup(func() {
		_ = app.ShutdownWithTimeout(time.Second)
		<-done
	})
	return "http://" + listener.Addr().String()
}

func nativePodSSE(t *testing.T, address string) []kubernetes.PodLogStreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("native SSE status%d body%s", response.StatusCode, body)
	}
	var events []kubernetes.PodLogStreamEvent
	for _, frame := range strings.Split(string(body), "\n\n") {
		event := kubernetes.PodLogStreamEvent{}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.Event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
			}
		}
		if event.Event != "" {
			events = append(events, event)
		}
	}
	return events
}

func TestNativeCompletedInitCurrentIsFiniteAndNotRetried(t *testing.T) {
	for _, selection := range []string{"setup", "*"} {
		t.Run("container="+selection, func(t *testing.T) {
			api, service, authorized := workloadBackend(t, "StatefulSet", 3)
			ctx, cancel := context.WithTimeout(authorized, 6200*time.Millisecond)
			defer cancel()
			lines := map[string]int{}
			ended := false
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root", Container: selection,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.Event == "line" {
					lines[event.PodUID+"/"+event.Container]++
				}
				if event.Event == "end" && event.Reason == "complete" {
					ended = true
				}
				return nil
			})
			want := 3
			if selection == "setup" {
				if err != nil || !ended {
					t.Fatalf("init-only session must end finitely: ended%v err%v", ended, err)
				}
			} else {
				want = 9
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("All must keep regular sources live: %v", err)
				}
			}
			awaitNoWorkloadReads(t, api)
			counters := api.logStreamCounters()
			if len(lines) != want || len(counters) != want {
				t.Fatalf("sources=%v counters=%+v want%d", lines, counters, want)
			}
			for _, counter := range counters {
				if counter.Active != 0 {
					t.Errorf("source leaked: %+v", counter)
				}
				if counter.Container == "setup" && (counter.Previous || counter.Follow || counter.Requests != 1 || counter.Completed != 1) {
					t.Errorf("completed init was followed/retried: %+v", counter)
				}
			}
			t.Logf("Current %q: init reads previous=false/follow=false exactly once; init-only ends finitely, All retains regular streams across5s reconcile; active0", selection)
		})
	}
}

func TestNativePodSSEFreshAndPreviousErrorsDoNotClaimReplayGap(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous=%v", previous), func(t *testing.T) {
			api, service, _ := workloadBackend(t, "StatefulSet", 1, exactWorkloadBackend)
			path := "/api/v1/namespaces/shop/pods/orders-0/log"
			api.logFixtures[logStreamKey{Path: path, Container: "app", Previous: previous}] = logFixture{
				Path: path, Container: "app", Previous: previous, Status: 403,
			}
			base := nativeKubernetesAPI(t, service)
			events := nativePodSSE(t, fmt.Sprintf("%s/api/admin/kubernetes/pods/shop/orders-0/logs/stream?container=app&previous=%v", base, previous))
			errorsSeen := 0
			for _, event := range events {
				if event.Event == "error" {
					errorsSeen++
				}
				if event.ReplayUncertain {
					t.Errorf("fresh denied read falsely claims replay gap: %+v", event)
				}
			}
			if errorsSeen != 1 {
				t.Fatalf("expected one actual SSE error, got %+v", events)
			}
			awaitNoWorkloadReads(t, api)
			t.Logf("real Pod API previous=%v: one denied-read error, zero replay uncertainty across all emitted frames", previous)
		})
	}
}

func TestNativePodSSEShortTailHasFirstCursorAndResumesWithoutReset(t *testing.T) {
	for _, count := range []int{1, 7, 31} {
		t.Run(fmt.Sprintf("lines=%d", count), func(t *testing.T) {
			api, service, _ := workloadBackend(t, "StatefulSet", 1, exactWorkloadBackend)
			path := "/api/v1/namespaces/shop/pods/orders-0/log"
			fixture := logFixture{Path: path, Container: "app"}
			for line := 0; line < count; line++ {
				fixture.Lines = append(fixture.Lines, logFixtureLine{At: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Text: fmt.Sprintf("short-%02d", line)})
			}
			api.logFixtures[logStreamKey{Path: path, Container: "app"}] = fixture
			base := nativeKubernetesAPI(t, service)
			target := base + "/api/admin/kubernetes/pods/shop/orders-0/logs/stream?container=app"
			events := nativePodSSE(t, target+"&since_seconds=60&tail_lines=31")
			var first kubernetes.PodLogStreamEvent
			lines := 0
			for _, event := range events {
				if event.Event == "line" {
					if lines == 0 {
						first = event
					}
					lines++
				}
			}
			if lines != count || first.Cursor == "" || first.Sequence != 1 {
				t.Fatalf("short tail lacks initial checkpoint: lines%d first%+v", lines, first)
			}
			// Model transport Resume with the retained first checkpoint, not a
			// reset. Rendered button state/retained rows remain Visual's scope.
			resumed := nativePodSSE(t, target+"&cursor="+first.Cursor)
			resumedLines := 0
			for _, event := range resumed {
				if event.ReplayUncertain {
					t.Errorf("same-target retained cursor unexpectedly uncertain: %+v", event)
				}
				if event.Event == "line" {
					resumedLines++
					if event.Text == first.Text {
						t.Error("resume reset to/replayed the first acknowledged line")
					}
				}
			}
			if resumedLines != count-1 {
				t.Fatalf("resume lines%d want%d", resumedLines, count-1)
			}
			awaitNoWorkloadReads(t, api)
			t.Logf("%d-line real Pod SSE has first-line cursor; retained checkpoint resumes%d remaining lines without reset/replay gap", count, resumedLines)
		})
	}
}

func TestNativeRunningInitTransitionKeepsOnlySameRunCheckpoint(t *testing.T) {
	for _, change := range []string{"completed", "restart", "recreated"} {
		t.Run(change, func(t *testing.T) {
			var active atomic.Int32
			var queryMu sync.Mutex
			var queries []map[string]string
			path := "/api/v1/namespaces/shop/pods/orders-0/log"
			api, service, authorized := workloadBackend(t, "StatefulSet", 1, func(api *Server) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != path || r.URL.Query().Get("container") != "setup" {
						api.ServeHTTP(w, r)
						return
					}
					active.Add(1)
					defer active.Add(-1)
					queryMu.Lock()
					queries = append(queries, map[string]string{
						"follow": r.URL.Query().Get("follow"), "previous": r.URL.Query().Get("previous"),
						"sinceTime": r.URL.Query().Get("sinceTime"), "sinceSeconds": r.URL.Query().Get("sinceSeconds"),
					})
					queryMu.Unlock()
					if !api.serveLogFixture(w, r) {
						t.Error("native transition log fixture missing")
						http.Error(w, "fixture missing", 500)
						return
					}
					w.(http.Flusher).Flush()
					if r.URL.Query().Get("follow") == "true" {
						<-r.Context().Done()
					}
				})
			})
			at := time.Now().UTC().Add(-20 * time.Second).Truncate(time.Second)
			started := at.Add(-time.Minute)
			setState := func(uid string, restarts int, running bool, start time.Time) {
				t.Helper()
				body, ok := api.store.Get("pods", "shop", "orders-0")
				if !ok {
					t.Fatal("native Pod disappeared")
				}
				var object map[string]any
				if err := json.Unmarshal(body, &object); err != nil {
					t.Fatal(err)
				}
				object["metadata"].(map[string]any)["uid"] = uid
				state := map[string]any{"running": map[string]any{"startedAt": start.Format(time.RFC3339Nano)}}
				if !running {
					state = map[string]any{"terminated": map[string]any{
						"startedAt": start.Format(time.RFC3339Nano), "finishedAt": at.Add(5 * time.Second).Format(time.RFC3339Nano),
						"exitCode": 0, "reason": "Completed",
					}}
				}
				object["status"].(map[string]any)["initContainerStatuses"] = []any{
					map[string]any{"name": "setup", "restartCount": restarts, "state": state},
				}
				if err := api.store.upsertObject("pods", "shop", "orders-0", object); err != nil {
					t.Fatal(err)
				}
			}
			setFixture := func(prefix string, count int) {
				lines := []logFixtureLine{}
				for line := 0; line < count; line++ {
					lines = append(lines, logFixtureLine{At: at, Text: fmt.Sprintf("%s-%d", prefix, line)})
				}
				api.mu.Lock()
				api.logFixtures[logStreamKey{Path: path, Container: "setup"}] = logFixture{Path: path, Container: "setup", Lines: lines}
				api.mu.Unlock()
			}
			setState("pod-0", 1, true, started)
			setFixture("original", 3)
			ctx, cancel := context.WithTimeout(authorized, 23*time.Second)
			defer cancel()
			counts := map[string]int{}
			firstSource, nextSource := "", ""
			gaps, newLines := 0, 0
			transitioned, ended := false, false
			err := service.StreamWorkloadLogs(ctx, kubernetes.WorkloadLogStreamOptions{
				Kind: "StatefulSet", Name: "orders", Namespace: "shop", UID: "root", Container: "setup",
				SinceSeconds: 60, TailLines: 123,
			}, func(event kubernetes.WorkloadLogStreamEvent) error {
				if event.ReplayUncertain {
					gaps++
				}
				if event.Event == "end" && event.Reason == "complete" {
					ended = true
				}
				if event.Event != "line" {
					return nil
				}
				counts[event.Text]++
				if counts[event.Text] != 1 {
					t.Errorf("transition duplicated %s", event.Text)
				}
				if firstSource == "" {
					firstSource = event.SourceID
				}
				if change == "completed" && event.Text == "original-3" || strings.HasPrefix(event.Text, "new-") {
					nextSource = event.SourceID
				}
				if !transitioned && len(counts) == 3 {
					transitioned = true
					switch change {
					case "completed":
						setFixture("original", 4)
						setState("pod-0", 1, false, started)
					case "restart":
						setFixture("new", 3)
						setState("pod-0", 2, true, at.Add(-time.Second))
					case "recreated":
						setFixture("new", 3)
						setState("replacement", 1, true, at.Add(-time.Second))
					}
				}
				if strings.HasPrefix(event.Text, "new-") {
					newLines++
					wantUID := "pod-0"
					if change == "recreated" {
						wantUID = "replacement"
					}
					if event.PodUID != wantUID {
						t.Errorf("new run mislabeled UID: %+v", event)
					}
					if newLines == 3 {
						cancel()
					}
				}
				return nil
			})
			if change == "completed" {
				if err != nil || !ended || len(counts) != 4 || gaps != 0 || nextSource == "" {
					t.Fatalf("completion counts=%v gaps%d ended%v err%v", counts, gaps, ended, err)
				}
			} else if !errors.Is(err, context.Canceled) || newLines != 3 || len(counts) != 6 || nextSource == "" || nextSource == firstSource {
				t.Fatalf("new incarnation borrowed old identity: counts=%v new%d ids=%s/%s err%v", counts, newLines, firstSource, nextSource, err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for active.Load() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("transition HTTP bodies leaked: %d", active.Load())
				}
				time.Sleep(5 * time.Millisecond)
			}
			queryMu.Lock()
			defer queryMu.Unlock()
			if len(queries) != 2 || queries[0]["follow"] != "true" {
				t.Fatalf("unexpected transition reads=%v", queries)
			}
			next := queries[1]
			if change == "completed" {
				expected := at.Add(-time.Nanosecond).Format(time.RFC3339Nano)
				if next["follow"] != "false" || next["previous"] != "false" || next["sinceTime"] != expected {
					t.Errorf("same-run finite read lost exact occurrence checkpoint: %v expected sinceTime%s", next, expected)
				}
			} else if next["follow"] != "true" || next["sinceTime"] != "" || next["sinceSeconds"] != "60" {
				t.Errorf("new run borrowed former run checkpoint: %v", next)
			}
			t.Logf("%s: one continuous session, counts=%v, replay pulses%d, actual HTTP reads=%v, active0", change, counts, gaps, queries)
		})
	}
}
