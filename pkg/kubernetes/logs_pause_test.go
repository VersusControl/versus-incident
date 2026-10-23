package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestCompletedInitCurrentIsFiniteForExplicitAndAllSelection(t *testing.T) {
	for _, selection := range []string{"setup", "*"} {
		t.Run(selection, func(t *testing.T) {
			records := workloadStreamRecords("StatefulSet", 1, 1)
			ended := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			records[1].LogContainers = append(records[1].LogContainers, kubeindex.ContainerState{Name: "setup", Init: true, StateKnown: true, CurrentTermination: true, TerminatedReason: "Completed", TerminatedAt: ended})
			var initReads, initFollows atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !strings.HasSuffix(request.URL.Path, "/log") {
					object := streamPodObject(records[1])
					object["spec"].(map[string]any)["containers"] = []any{map[string]any{"name": "app-0"}}
					object["spec"].(map[string]any)["initContainers"] = []any{map[string]any{"name": "setup"}}
					object["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"name": "app-0", "state": map[string]any{"running": map[string]any{"startedAt": records[1].LogContainers[0].StartedAt.Format(time.RFC3339Nano)}}}}
					object["status"].(map[string]any)["initContainerStatuses"] = []any{map[string]any{"name": "setup", "state": map[string]any{"terminated": map[string]any{"reason": "Completed", "exitCode": 0, "finishedAt": ended.Format(time.RFC3339Nano)}}}}
					writeJSON(writer, object)
					return
				}
				if request.URL.Query().Get("container") == "setup" {
					initReads.Add(1)
					if request.URL.Query().Get("follow") != "false" {
						initFollows.Add(1)
					}
					if request.URL.Query().Get("previous") != "false" {
						t.Error("Current init was read from Previous slot")
					}
					fmt.Fprintln(writer, "2026-10-01T00:00:00Z init completed")
					return
				}
				fmt.Fprintln(writer, "2026-10-01T00:00:00Z app running")
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			service.SetScrubber(replacingScrubber{})
			seedWorkloadStreamIndex(t, service, records)
			ctx, cancel := context.WithTimeout(logStreamTestContext(), 500*time.Millisecond)
			defer cancel()
			seen := make(map[string]bool)
			paused := errors.New("live viewer finished")
			session, err := service.OpenWorkloadLogStream(ctx, WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Container: selection})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			err = session.Stream(func(event WorkloadLogStreamEvent) error {
				if event.Event == "line" {
					seen[event.Container] = true
				}
				if selection == "*" && len(seen) == 2 {
					return paused
				}
				return nil
			})
			if selection == "setup" && err != nil || selection == "*" && !errors.Is(err, paused) || !seen["setup"] || initReads.Load() != 1 || initFollows.Load() != 0 {
				t.Fatalf("completed init Current unreadable/nonfinite: selection=%s seen=%v reads=%d follows=%d err=%v", selection, seen, initReads.Load(), initFollows.Load(), err)
			}
			if selection == "setup" {
				for id, worker := range session.workers {
					if !worker.finished || !worker.terminal || !worker.options.finite {
						t.Fatal("completed init was not a finished finite source")
					}
					if err := session.reconcile(records[1:]); err != nil {
						t.Fatal(err)
					}
					if session.workers[id] != worker || initReads.Load() != 1 {
						t.Fatal("completed init was scheduled for retry")
					}
				}
			}
		})
	}
}

func TestCompletedRegularCurrentStillRequiresLiveIncarnation(t *testing.T) {
	record := workloadStreamRecords("StatefulSet", 1, 1)[1]
	record.LogContainers[0].Running = false
	record.LogContainers[0].StartedAt = time.Time{}
	record.LogContainers[0].CurrentTermination = true
	record.LogContainers[0].TerminatedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	record.LogContainers[0].TerminatedReason = "Completed"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		object := streamPodObject(record)
		object["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"name": "app-0", "state": map[string]any{"terminated": map[string]any{"reason": "Completed", "exitCode": 0, "finishedAt": record.LogContainers[0].TerminatedAt.Format(time.RFC3339Nano)}}}}
		writeJSON(writer, object)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	worker := &workloadSourceWorker{source: WorkloadLogSource{PodUID: record.UID, Pod: record.Name, Namespace: record.Namespace, Container: "app-0", Incarnation: workloadContainerIncarnation(record.LogContainers[0], false)}, options: PodLogStreamOptions{}, expected: record}
	if err := service.validateWorkloadSource(context.Background(), worker); !errors.Is(err, errWorkloadSourceGone) {
		t.Fatal("completed-init allowance weakened regular Current validation", err)
	}
}

func TestEmptyWorkloadCheckpointRetainsQuietResumeBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, workloadStreamRecords("StatefulSet", 1, 1))
	options := WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", SinceSeconds: 60, TailLines: 123}
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	options.Cursor = session.checkpoint()
	session.Close()
	options.SinceSeconds, options.TailLines = 0, 0
	resumed, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.uncertain || resumed.options.SinceSeconds != 60 || resumed.options.TailLines != 123 {
		t.Fatal("empty workload rotation lost its selection")
	}
	for _, worker := range resumed.workers {
		cursor, err := service.logCursor(worker.options)
		if err != nil || !cursor.Quiet {
			t.Fatal("empty workload source cannot resume quietly", err)
		}
	}
}

func TestPartialLogBatchPauseHasCheckpointAndDeduplicatesResume(t *testing.T) {
	for _, kind := range []string{"Pod", "StatefulSet"} {
		for _, pauseAt := range []int{1, 31} {
			t.Run(kind+"/"+strconv.Itoa(pauseAt), func(t *testing.T) {
				records := workloadStreamRecords("StatefulSet", 1, 1)
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if serveStreamPodObject(writer, request, records) {
						return
					}
					for i := 1; i <= 33; i++ {
						fmt.Fprintf(writer, "2026-10-01T00:00:00Z line-%d\n", i)
					}
				}))
				defer server.Close()
				service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
				service.SetScrubber(replacingScrubber{})
				seedWorkloadStreamIndex(t, service, records)
				run := func(cursor string, emit func(PodLogStreamEvent) error) error {
					since, tail := 60, 123
					if cursor != "" {
						since, tail = 0, 0
					}
					if kind == "Pod" {
						return service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api-00", Container: "app-0", SinceSeconds: since, TailLines: tail, Cursor: cursor}, emit)
					}
					return service.StreamWorkloadLogs(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", SinceSeconds: since, TailLines: tail, Cursor: cursor}, func(event WorkloadLogStreamEvent) error { return emit(event.PodLogStreamEvent) })
				}
				paused := errors.New("viewer paused")
				cursor := ""
				lines := 0
				retained := make(map[string]string)
				key := func(event PodLogStreamEvent) string {
					return event.Timestamp + "/" + strconv.FormatUint(event.Ordinal, 10)
				}
				err := run("", func(event PodLogStreamEvent) error {
					if event.Cursor != "" {
						cursor = event.Cursor
					}
					if event.Event != "line" {
						return nil
					}
					if event.ReplayUncertain {
						t.Fatal("fresh retained lines claimed a replay gap")
					}
					lines++
					retained[key(event)] = event.Text
					if lines == 1 && event.Cursor == "" {
						t.Error("first line has no usable checkpoint")
					}
					if lines == pauseAt {
						return paused
					}
					return nil
				})
				if !errors.Is(err, paused) || cursor == "" {
					t.Fatalf("partial batch cannot resume: lines=%d cursor=%t err=%v", lines, cursor != "", err)
				}
				overlap := 0
				err = run(cursor, func(event PodLogStreamEvent) error {
					if event.ReplayUncertain {
						t.Fatal("valid retained resume claimed a gap")
					}
					if event.Event != "line" {
						return nil
					}
					if old, seen := retained[key(event)]; seen {
						if old != event.Text {
							t.Fatal("replay identity changed text")
						}
						overlap++
					} else {
						retained[key(event)] = event.Text
					}
					if event.Ordinal == 33 {
						return paused
					}
					return nil
				})
				if !errors.Is(err, paused) || len(retained) != 33 || overlap != pauseAt-1 {
					t.Fatalf("partial batch lost or duplicated retained lines: unique=%d overlap=%d err=%v", len(retained), overlap, err)
				}
			})
		}
	}
}
