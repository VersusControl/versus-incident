package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"golang.org/x/time/rate"
)

type breakLogScrubber struct{}

func TestPinnedOOMCauseSurvivesSecondRoundedStatusAndRestartDrift(t *testing.T) {
	for _, source := range []string{"manifest", "snapshot", "linked-snapshot"} {
		t.Run(source, func(t *testing.T) {
			end := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			lineAt := end.Add(360 * time.Millisecond)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/log") {
					fmt.Fprintf(w, "%s allocating memory token=sensitive\n", lineAt.Format(time.RFC3339Nano))
					return
				}
				fixture, _ := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
				fixture.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			service.SetScrubber(apiShapeScrubber{})
			service.SetChangeStorage(storage.NewMemory())
			t.Cleanup(service.StopCapture)
			root := "c1428f21-73bd-40b1-92d6-f57d5efbf5ca"
			uid := "29288ddf-ea01-4749-910f-fde84aabe01f"
			status := map[string]any{"containerStatuses": []any{map[string]any{
				"name": "app", "restartCount": float64(2),
				"state":     map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": float64(137), "startedAt": end.Add(-3 * time.Second).Format(time.RFC3339), "finishedAt": end.Format(time.RFC3339)}},
				"lastState": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": float64(1), "startedAt": end.Add(-20 * time.Second).Format(time.RFC3339), "finishedAt": end.Add(-17 * time.Second).Format(time.RFC3339)}},
			}}}
			pod := kubeindex.Record{Kind: "Pod", Namespace: "deleted", Name: "oomkill-8567c4dc75-v7gh9", UID: uid, CreatedAt: end.Add(-time.Hour), Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: root, Controller: true}}, Containers: projectCrashContainers(status)}
			index := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: end.Add(time.Second)}, "Deployment": {State: "ready"}}, Records: []kubeindex.Record{{Kind: "Deployment", Namespace: pod.Namespace, Name: "oomkill", UID: root}, pod}}
			pods := service.scrubManifestPods(verifiedBreakPods(index))
			if len(pods) != 1 || pods[0].UID != uid {
				t.Fatalf("real UID was lost: %+v", pods)
			}
			pods[0].LastSeen = end.Add(time.Second)
			manifest := breaklogs.Manifest{ID: "pinned-oom", OccurrenceID: "pinned-oom", Namespace: pod.Namespace, RootUID: root, BreakAt: end, From: end.Add(-time.Minute), To: end.Add(time.Second), CapturedAt: time.Now().UTC(), Pods: pods}
			item, gap := service.readBreakContainer(t.Context(), pod, root, pod.Containers[0], "previous", manifest.From, manifest.To)
			if gap != nil || len(item.Lines) != 1 || !item.Lines[0].At.Equal(lineAt) || strings.Contains(item.Lines[0].Text, "sensitive") {
				t.Fatalf("real fractional OOM line lost or unsanitized: %+v gap=%+v", item, gap)
			}
			item.BreakAt = end
			if source == "linked-snapshot" {
				items := service.linkedBreakSnapshots(manifest, []BreakLogLine{{At: lineAt, Pod: pod.Name, PodUID: uid, Container: "app", Slot: "previous", Text: item.Lines[0].Text}}, false)
				if len(items) != 1 {
					t.Fatalf("missing settled linked excerpt: %+v", items)
				}
				item = items[0]
			}
			query := breaklogs.Query{Namespace: pod.Namespace, RootUID: root, From: manifest.From, To: manifest.To, Container: "*"}
			if source == "manifest" {
				// A legacy excerpt lacks cause; the pinned manifest must suffice.
				raw, _ := json.Marshal(item)
				var legacy map[string]any
				_ = json.Unmarshal(raw, &legacy)
				delete(legacy, "termination_reason")
				delete(legacy, "termination_exit_code")
				raw, _ = json.Marshal(legacy)
				_ = json.Unmarshal(raw, &item)
				query.OccurrenceID, query.Center = manifest.ID, end
				drift := pod
				drift.Containers = []kubeindex.ContainerState{{Name: "app", RestartCount: 6, TerminatedReason: "Error", ExitCode: 1, TerminatedAt: end.Add(30 * time.Second)}}
				manifest.Pods = supplementLinkedPods(manifest.Pods, nil, []kubeindex.Record{drift})
				if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "", "", time.Now().UTC(), manifest); err != nil {
					t.Fatal(err)
				}
			} else if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "", "", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			page, err := service.BreakLogs(t.Context(), query)
			if err != nil || len(page.Lines) != 1 {
				t.Fatalf("pinned query failed: %+v err=%v", page, err)
			}
			markers := 0
			for _, marker := range page.Markers {
				if marker.Type == "crash" && marker.At.Equal(end) {
					markers++
					if marker.Label != "app OOMKilled exit 137" {
						t.Errorf("pinned cause lost to later lastState or generic fallback: %+v", marker)
					}
				}
			}
			if markers != 1 {
				t.Fatalf("want one second-rounded OOM marker: %+v", page.Markers)
			}
			filtered := query
			filtered.Pods = []string{"other-pod"}
			other, err := service.BreakLogs(t.Context(), filtered)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range other.Markers {
				if marker.Type == "crash" {
					t.Fatalf("unselected pod exposed a termination marker: %+v", marker)
				}
			}
			outside := query
			outside.OccurrenceID, outside.Center = "", time.Time{}
			outside.From, outside.To = end.Add(2*time.Second), end.Add(5*time.Second)
			later, err := service.BreakLogs(t.Context(), outside)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range later.Markers {
				if marker.Type == "crash" {
					t.Fatalf("out-of-window cause exposed: %+v", marker)
				}
			}
		})
	}
}

func TestLinkedTerminationSecondUsesPreviousSlot(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	end := manifest.BreakAt.Add(-2 * time.Second).Truncate(time.Second)
	pods := manifest.Pods
	pods[0].Containers = []breaklogs.ManifestContainer{{Name: "app", Restarts: 8, PreviousStartedAt: end.Add(-time.Second), TerminationAt: end}}
	at := end.Add(500 * time.Millisecond)
	source := breaklogs.Source{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
		return acceptanceLinkedReader{read: func(context.Context, breaklogs.LinkedQuery) (breaklogs.Result, error) {
			return breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: at, Pod: pods[0].Name, Container: "app", Text: "last words"}}}, nil
		}}, nil
	}}
	query := breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, From: manifest.From, To: manifest.To, Container: "*", Mode: "previous"}
	lines, result, err := service.readLinkedBreak(t.Context(), query, pods, breaklogs.CaptureSettings{}, source, false)
	if err != nil || result.Rejected || len(lines) != 1 || lines[0].Slot != "previous" || lines[0].PodUID != pods[0].UID {
		t.Fatalf("termination-second line lost or assigned current slot: lines=%+v result=%+v err=%v", lines, result, err)
	}
}

func TestDeletedPodEmptySnapshotRetainsGapAndQueriesLinked(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	empty := breaklogs.Snapshot{Namespace: manifest.Namespace, RootUID: manifest.RootUID, Pod: "api-0", PodUID: "old", Container: "app", Slot: "current", CapturedAt: time.Now().UTC(), From: manifest.From, To: manifest.To, PodCreatedAt: manifest.Pods[0].CreatedAt, Lines: []breaklogs.Line{}}
	empty.ID = breaklogs.Identity(empty)
	// Legacy stores can contain empty captures spanning the entire window.
	if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{empty}, nil, "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return acceptanceLinkedReader{read: func(_ context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
				calls.Add(1)
				if len(query.Pods) != 1 || query.Pods[0] != "api-0" {
					t.Errorf("deleted Pod identity missing from linked query: %+v", query)
				}
				return breaklogs.Result{}, nil
			}}, nil
		}}}
	})
	delay := 90
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &delay}}); err != nil {
		t.Fatal(err)
	}
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, From: manifest.From, To: manifest.To, Container: "app"})
	if err != nil || page.Coverage.State == "complete" || !page.Coverage.Tip || len(page.Coverage.Gaps) == 0 || len(page.Lines) != 0 || calls.Load() != 1 {
		t.Fatalf("empty deleted-Pod capture suppressed gap or linked fallback: calls=%d page=%+v err=%v", calls.Load(), page, err)
	}
	retained := false
	for _, gap := range page.Coverage.Gaps {
		retained = retained || !gap.From.After(manifest.From) && !gap.To.Before(manifest.To)
	}
	if !retained {
		t.Fatalf("original empty-snapshot window was hidden: %+v", page.Coverage)
	}
}

func TestLinkedLeafCannotCloseOtherTimesOrContainers(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	coverage := breaklogs.Cover(from, now, nil, nil, false)
	page := BreakLogPage{Window: BreakLogWindow{from, now}, Pods: []BreakLogPod{{Name: "api-0", UID: "pod"}}, Coverage: coverage, Container: "*", Containers: []BreakLogContainer{{Name: "app"}, {Name: "sidecar"}}}
	line := BreakLogLine{At: now, Pod: "api-0", PodUID: "pod", Container: "app"}
	page.closeLinkedGaps(nil, []BreakLogLine{line}, now, from)
	if page.Coverage.State == "complete" {
		t.Fatal("one container closed all-container coverage")
	}
	page.Container = "app"
	page.Coverage = coverage
	page.closeLinkedGaps(nil, []BreakLogLine{line}, now, now.Add(-time.Minute))
	found := false
	for _, gap := range page.Coverage.Gaps {
		found = found || !gap.From.After(from) && !gap.To.Before(now.Add(-time.Minute))
	}
	if !found {
		t.Fatal("newest split hid unqueried older range", page.Coverage)
	}
}

func TestNewRestartsCannotExtendCapturedContainerLifetime(t *testing.T) {
	now := time.Now().UTC()
	known := breaklogs.ManifestPod{Name: "api-0", UID: "pod", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-5 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", Restarts: 1, StartedAt: now.Add(-10 * time.Minute), TerminationAt: now.Add(-11 * time.Minute)}}}
	live := kubeindex.Record{Name: "api-0", UID: "pod", CreatedAt: known.CreatedAt, Containers: []kubeindex.ContainerState{{Name: "app", RestartCount: 3, StartedAt: now.Add(-time.Minute), TerminatedAt: now.Add(-2 * time.Minute)}}}
	pods := supplementLinkedPods([]breaklogs.ManifestPod{known}, nil, []kubeindex.Record{live})
	if _, _, ok := breaklogs.ResolveLinkedLine(pods, breaklogs.LinkedLine{At: now.Add(-3 * time.Minute), Pod: "api-0", Container: "app"}); ok {
		t.Fatal("unknown intervening restart assigned to captured current slot")
	}
	if _, _, ok := breaklogs.ResolveLinkedLine(pods, breaklogs.LinkedLine{At: now.Add(-6 * time.Minute), Pod: "api-0", Container: "app"}); !ok {
		t.Fatal("verified captured lifetime lost")
	}
}

type breakWindowReader struct {
	calls *atomic.Int32
	ready *atomic.Bool
	at    time.Time
}

type failedBreakReader struct{ reason string }

func (failedBreakReader) Preview(breaklogs.LinkedQuery) string { return "" }
func (reader failedBreakReader) Read(context.Context, breaklogs.LinkedQuery) (breaklogs.Result, error) {
	return breaklogs.Result{}, errors.New(reader.reason)
}

type priorityBreakReader struct{ names *[]string }

func (priorityBreakReader) Preview(breaklogs.LinkedQuery) string { return "" }
func (reader priorityBreakReader) Read(_ context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	*reader.names = append([]string(nil), query.Pods...)
	return breaklogs.Result{}, nil
}

func (reader breakWindowReader) Preview(breaklogs.LinkedQuery) string { return "" }
func (reader breakWindowReader) Read(_ context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	reader.calls.Add(1)
	if !reader.ready.Load() {
		return breaklogs.Result{}, nil
	}
	return breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: reader.at, Pod: "api-0", Container: "app", Text: "ERROR MESSAGE_CANARY"}}}, nil
}

func TestBreakLogDeletedPriorInventoryDelayedLinkedAndEmptyKeepsSettle(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(provider)
	defer service.StopCapture()
	now := time.Now().UTC()
	at := now.Add(-2 * time.Minute)
	prior := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "Deployment": {State: "ready"}}, Records: []kubeindex.Record{
		{Kind: "Deployment", Namespace: "deleted", Name: "api", UID: "deleted-root"},
		{Kind: "Pod", Namespace: "deleted", Name: "api-0", UID: "gone", CreatedAt: now.Add(-time.Hour), Owners: []kubeindex.OwnerRef{{UID: "deleted-root", Controller: true, Kind: "Deployment"}}, Containers: []kubeindex.ContainerState{{Name: "app", StartedAt: now.Add(-time.Hour)}}},
	}}
	verified := verifiedBreakPods(prior)
	if len(verified) != 1 || verified[0].RootUID != "deleted-root" {
		t.Fatal("prior ownership unverified", verified)
	}
	guess := prior
	guess.Records = guess.Records[1:]
	if len(verifiedBreakPods(guess)) != 0 {
		t.Fatal("guessed missing controller membership")
	}
	irrelevant := prior
	irrelevant.Records = append(append([]kubeindex.Record(nil), prior.Records...), kubeindex.Record{Kind: "Pod", Namespace: "deleted", Name: "healthy-standalone", UID: "irrelevant", CreatedAt: now.Add(-time.Hour)})
	if len(verifiedBreakPods(irrelevant)) != 1 {
		t.Fatal("irrelevant standalone Pod retained")
	}
	retained := []breaklogs.ManifestPod{{UID: "irrelevant"}}
	if len(verifiedBreakPods(irrelevant, retained)) != 2 {
		t.Fatal("resolved tracked standalone Pod falsely vanished")
	}
	store := service.changeStore()
	if err := store.ClaimLease("capture", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveBreakPods(verified, "capture", at.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveBreakPods(nil, "capture", at.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	state, err := store.BreakLogs(now)
	if err != nil || len(state.PodHistory) != 1 || state.PodHistory[0].DeletedAt.IsZero() {
		t.Fatal("disappearance erased prior proof", state, err)
	}
	state.PodHistoryGaps = []breaklogs.Gap{{Namespace: "deleted", RootUID: "unrelated", From: at, To: now, Reason: "budget"}}
	root := kubebreaks.Break{Root: kubebreaks.Ref(prior.Records[0]), BrokeAt: now}
	if service.newBreakManifest(t.Context(), root, state, now).Truncated {
		t.Fatal("unrelated history loss truncated manifest")
	}
	state.PodHistoryGaps[0].RootUID = "deleted-root"
	if !service.newBreakManifest(t.Context(), root, state, now).Truncated {
		t.Fatal("affected root history loss concealed")
	}
	priorityPods := []breaklogs.ManifestPod{}
	for i := 0; i < 4; i++ {
		priorityPods = append(priorityPods, breaklogs.ManifestPod{Namespace: "deleted", RootUID: "deleted-root", Name: fmt.Sprintf("api-%d", i), UID: fmt.Sprint(i), CreatedAt: now.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app"}}})
	}
	priorityPods[3].Containers[0].Restarts = 1
	var names []string
	source := breaklogs.Source{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) { return priorityBreakReader{&names}, nil }}
	_, _, err = service.readLinkedBreak(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: at, To: now}, priorityPods, breaklogs.CaptureSettings{}, source, false)
	if err != nil || len(names) != 4 || names[0] != "api-3" || names[1] != "api-0" {
		t.Fatal("broken Pod did not receive deterministic query priority", names, err)
	}
	manifest := breaklogs.Manifest{ID: breaklogs.ManifestIdentity("deleted-root", now), Namespace: "deleted", RootUID: "deleted-root", BreakAt: now, From: now.Add(-5 * time.Minute), To: now, CapturedAt: now, Pods: state.PodHistory, SettleAt: now}
	if err := store.CommitBreakLogs(nil, nil, "capture", "capture", now, manifest); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var ready atomic.Bool
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return breakWindowReader{&calls, &ready, at}, nil
		}}}
	})
	delay := 90
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &delay}}); err != nil {
		t.Fatal(err)
	}
	query := breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: "app"}
	page, err := service.BreakLogs(t.Context(), query)
	if err != nil || page.Coverage.Reason != "linked_pending" || len(page.Lines) != 0 {
		t.Fatal("lag final/incorrect", page, err)
	}
	foundReady := false
	for _, gap := range page.Coverage.Gaps {
		foundReady = foundReady || gap.ReadyAt.Equal(manifest.To.Add(90*time.Second))
	}
	if !foundReady {
		t.Fatal("missing exact ready time", page.Coverage)
	}
	for _, reason := range []string{"timeout", "not_found", "invalid_mapping", "unsafe provider error"} {
		service.SetBreakLogSources(func(string) []breaklogs.Source {
			return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) { return failedBreakReader{reason}, nil }}}
		})
		page, err = service.BreakLogs(t.Context(), query)
		if err != nil || page.Coverage.Reason != "linked_pending" || len(page.Sources) == 0 {
			t.Fatal("failure ended pending polling", reason, page, err)
		}
		explicit, pending := false, false
		for _, gap := range page.Coverage.Gaps {
			explicit = explicit || gap.Reason == reason || reason == "unsafe provider error" && gap.Reason == "source_error"
			pending = pending || gap.Reason == "linked_pending" && gap.ReadyAt.Equal(manifest.To.Add(90*time.Second))
		}
		if !explicit || !pending || strings.Contains(fmt.Sprint(page), "unsafe provider error") {
			t.Fatal("failure omitted, leaked, or lost readiness", reason, page.Coverage)
		}
	}
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return breakWindowReader{&calls, &ready, at}, nil
		}}}
	})
	ready.Store(true)
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || len(page.Lines) != 1 || page.Lines[0].PodUID != "gone" || page.Lines[0].Text != "ERROR [redacted]" || !page.Window.To.Equal(manifest.To) {
		t.Fatal("deleted linked attribution", page, err)
	}
	if page.Coverage.State == "complete" || page.Coverage.Reason != "linked_pending" {
		t.Fatal("older partial rows matured before ready", page.Coverage)
	}
	claimed, err := store.ClaimBreakSettle(manifest.ID, "capture", now)
	if err != nil {
		t.Fatal(err)
	}
	items := service.linkedBreakSnapshots(manifest, page.Lines, false)
	if err := store.CommitBreakSettle(claimed, items, false, "capture", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ready.Store(false)
	zero := 0
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
		t.Fatal(err)
	}
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || len(page.Lines) != 1 || page.Lines[0].Source != "linked" {
		t.Fatal("empty erased settled excerpt", page, err)
	}
	if page.Coverage.Reason != "no_logs" {
		t.Fatal("mature empty is not truthful", page.Coverage)
	}
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) { return failedBreakReader{"timeout"}, nil }}}
	})
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || page.Coverage.Reason != "timeout" || len(page.Lines) != 1 {
		t.Fatal("mature failure silent or lost prior excerpt", page, err)
	}
	for _, gap := range page.Coverage.Gaps {
		if gap.Reason == "linked_pending" {
			t.Fatal("mature error invented pending readiness", page.Coverage)
		}
	}
}

type gatedBreakReader struct {
	reader  breakWindowReader
	started chan<- struct{}
	release <-chan struct{}
}

func (reader gatedBreakReader) Preview(query breaklogs.LinkedQuery) string {
	return reader.reader.Preview(query)
}
func (reader gatedBreakReader) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	select {
	case reader.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return breaklogs.Result{}, ctx.Err()
	case <-reader.release:
	}
	return reader.reader.Read(ctx, query)
}

func TestOwnedLinkedSettleFencesInFlightTakeoverAndBoundsAttempts(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	now := time.Now().UTC()
	store := service.changeStore()
	manifest := breaklogs.Manifest{ID: "selected", RootUID: "deleted-root", Namespace: "deleted", BreakAt: now.Add(-30 * time.Second), From: now.Add(-5 * time.Minute), To: now.Add(-30 * time.Second), CapturedAt: now, SettleAt: now,
		Pods: []breaklogs.ManifestPod{{Namespace: "deleted", RootUID: "deleted-root", Name: "api-0", UID: "gone", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-40 * time.Second), DeletedAt: now.Add(-40 * time.Second), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}}}
	if err := store.ClaimLease("first", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBreakLogs(nil, nil, "", "first", now, manifest); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var ready, block atomic.Bool
	ready.Store(true)
	block.Store(true)
	started, release := make(chan struct{}, 1), make(chan struct{})
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			reader := breakWindowReader{&calls, &ready, now.Add(-time.Minute)}
			if block.Load() {
				return gatedBreakReader{reader, started, release}, nil
			}
			return reader, nil
		}}}
	})
	zero := 0
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
		t.Fatal(err)
	}
	var running sync.WaitGroup
	service.scheduleBreakSettles(t.Context(), "first", &running)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(release)
		running.Wait()
		t.Fatal("settle not scheduled")
	}
	if err := store.ReleaseLease("first"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimLease("second", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	close(release)
	running.Wait()
	state, err := store.BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 0 || state.Manifests[0].Attempts != 1 {
		t.Fatal("lost owner committed query", state, err)
	}
	block.Store(false)
	claimed, err := store.ClaimBreakSettle(manifest.ID, "second", now.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	service.settleBreakLogs(t.Context(), store, claimed, "second")
	state, err = store.BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 1 || state.Manifests[0].Attempts != 2 || !state.Manifests[0].Done || calls.Load() != 2 || state.Snapshots[0].Lines[0].Text != "ERROR [redacted]" {
		t.Fatal("remaining settle not committed exactly once", state, calls.Load(), err)
	}
	service.scheduleBreakSettles(t.Context(), "second", &running)
	running.Wait()
	if calls.Load() != 2 {
		t.Fatal("third linked settle read", calls.Load())
	}
}

type countedBreakProvider struct {
	storage.Provider
	writes atomic.Int32
}

func (provider *countedBreakProvider) CompareAndSwapBlob(key string, old, next []byte) (bool, error) {
	provider.writes.Add(1)
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, next)
}

func TestDemandIneligibleSourcesDoNotChurnCaptureLease(t *testing.T) {
	for _, scenario := range []string{"source_removed", "delay_raised", "attempt_running"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(fixture)
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			service.SetScrubber(breakLogScrubber{})
			provider := &countedBreakProvider{Provider: storage.NewMemory()}
			service.SetChangeStorage(provider)
			defer service.StopCapture()
			service.SetBreakLogSources(func(string) []breaklogs.Source {
				return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) { return failedBreakReader{"timeout"}, nil }}}
			})
			delay := 90
			if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &delay}}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			manifest := breaklogs.Manifest{ID: "selected", RootUID: "root", Namespace: "shop", BreakAt: now.Add(-30 * time.Second), From: now.Add(-5 * time.Minute), To: now.Add(-30 * time.Second), CapturedAt: now, SettleAt: now}
			if scenario == "source_removed" {
				service.SetBreakLogSources(func(string) []breaklogs.Source { return nil })
			}
			if scenario == "attempt_running" {
				manifest.To = now.Add(-5 * time.Minute)
				manifest.From = now.Add(-10 * time.Minute)
				manifest.SettlingUntil = now.Add(30 * time.Second)
			}
			if err := service.changeStore().CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
				t.Fatal(err)
			}
			before := provider.writes.Load()
			ctx, cancel := context.WithTimeout(t.Context(), 1100*time.Millisecond)
			defer cancel()
			loop := &captureLoop{ctx: ctx, owner: "idle", done: make(chan struct{})}
			service.runDemandBreakSettles(loop)
			if provider.writes.Load() != before {
				t.Fatal("ineligible work wrote full capture state", before, provider.writes.Load())
			}
			if err := service.changeStore().CheckCaptureLease(""); err != nil {
				t.Fatal("idle scheduling claimed ownership", err)
			}
		})
	}
}

func TestDemandModeSettlesUnderShortCaptureLease(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	if err := service.SetCaptureMode("demand"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manifest := breaklogs.Manifest{ID: "selected", RootUID: "deleted-root", Namespace: "deleted", BreakAt: now.Add(-30 * time.Second), From: now.Add(-5 * time.Minute), To: now.Add(-30 * time.Second), CapturedAt: now, SettleAt: now,
		Pods: []breaklogs.ManifestPod{{Namespace: "deleted", RootUID: "deleted-root", Name: "api-0", UID: "gone", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-40 * time.Second), DeletedAt: now.Add(-40 * time.Second), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}}}
	if err := service.changeStore().CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var ready atomic.Bool
	ready.Store(true)
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return breakWindowReader{&calls, &ready, now.Add(-time.Minute)}, nil
		}}}
	})
	zero := 0
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
		t.Fatal(err)
	}
	service.StartCapture(t.Context())
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := service.changeStore().BreakLogs(time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range state.Manifests {
			if item.ID == manifest.ID && item.Done {
				found := false
				for _, snapshot := range state.Snapshots {
					found = found || snapshot.Source == "linked" && snapshot.OccurrenceID == manifest.ID
				}
				if item.Attempts != 1 || !found {
					t.Fatal("demand settle did not retain bounded excerpt", state)
				}
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("demand mode never settled")
		case <-ticker.C:
		}
	}
}

func TestLinkedDetailCacheRevalidatesNamespaceModeAndConfiguration(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	var denied atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if denied.Load() && request.URL.Path == "/api/v1/namespaces/deleted/pods" {
			writer.WriteHeader(http.StatusForbidden)
			fmt.Fprint(writer, `{"kind":"Status","status":"Failure","reason":"Forbidden","message":"denied"}`)
			return
		}
		fixture.ServeHTTP(writer, request)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	now := time.Now().UTC()
	manifest := breaklogs.Manifest{ID: "selected", RootUID: "deleted-root", Namespace: "deleted", BreakAt: now.Add(-10 * time.Second), From: now.Add(-5 * time.Minute), To: now.Add(-10 * time.Second), CapturedAt: now,
		Pods: []breaklogs.ManifestPod{{Namespace: "deleted", RootUID: "deleted-root", Name: "api-0", UID: "gone", CreatedAt: now.Add(-time.Hour), LastSeen: now, DeletedAt: now, Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}}}
	if err := service.changeStore().CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
		t.Fatal(err)
	}
	var calls, version atomic.Int32
	var ready atomic.Bool
	version.Store(1)
	ready.Store(true)
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", CacheKey: fmt.Sprint(version.Load()), Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return breakWindowReader{&calls, &ready, now.Add(-time.Minute)}, nil
		}}}
	})
	zero := 0
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
		t.Fatal(err)
	}
	query := breaklogs.Query{RootUID: manifest.RootUID, Namespace: manifest.Namespace, Center: manifest.BreakAt, From: manifest.From, To: manifest.To, Container: "app", Mode: "current"}
	for i := 0; i < 2; i++ {
		page, err := service.BreakLogs(t.Context(), query)
		if err != nil || len(page.Lines) != 1 {
			t.Fatal(page, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("safe repeated query missed cache", calls.Load())
	}
	version.Store(2)
	if _, err := service.BreakLogs(t.Context(), query); err != nil || calls.Load() != 2 {
		t.Fatal("source config reused cache", calls.Load(), err)
	}
	previous := query
	previous.Mode = "previous"
	if page, err := service.BreakLogs(t.Context(), previous); err != nil || len(page.Lines) != 0 || calls.Load() != 3 {
		t.Fatal("mode shared response", page, calls.Load(), err)
	}
	denied.Store(true)
	if page, err := service.BreakLogs(t.Context(), query); !errors.Is(err, ErrForbidden) || len(page.Lines) != 0 {
		t.Fatal("revoked namespace reused cache", page, err)
	}
	denied.Store(false)
	if _, err := service.BreakLogs(t.Context(), query); err != nil || calls.Load() != 4 {
		t.Fatal("revocation failed to invalidate", calls.Load(), err)
	}
	override := 90
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &override}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.BreakLogs(t.Context(), query); err != nil || calls.Load() != 5 {
		t.Fatal("settings epoch reused cache", calls.Load(), err)
	}
}

func TestBreakLogSourceResolverConcurrentPublicationAndScopedCopies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	defer service.StopCapture()
	service.SetBreakLogSources(func(string) []breaklogs.Source { return []breaklogs.Source{{Name: "initial", Type: "loki"}} })
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				scoped := service.Scoped(service.Scope())
				sources := scoped.configuredLogSources()
				if len(sources) != 1 || sources[0].Name == "" {
					t.Error("partial resolver publication", sources)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		name := fmt.Sprint(i)
		service.SetBreakLogSources(func(string) []breaklogs.Source { return []breaklogs.Source{{Name: name, Type: "loki"}} })
	}
	readers.Wait()
}

func TestLinkedBreakCacheCapsExpiryAndPendingRetry(t *testing.T) {
	clearLinkedCache()
	defer clearLinkedCache()
	now := time.Now().UTC()
	result := breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: now, Text: "[redacted]"}}}
	for i := 0; i < 257; i++ {
		cacheLinkedResult(fmt.Sprint(i), result, now, now)
	}
	if _, exists := cachedLinkedResult("0", now); exists {
		t.Fatal("cache exceeded LRU cap")
	}
	if _, exists := cachedLinkedResult("256", now.Add(5*time.Minute)); exists {
		t.Fatal("expired cache entry reused")
	}
	cacheLinkedResult("pending", result, now, now.Add(90*time.Second))
	if _, exists := cachedLinkedResult("pending", now.Add(15*time.Second)); exists {
		t.Fatal("pending suppresses scheduled retry")
	}
	cacheLinkedResult("ready", result, now, now.Add(5*time.Second))
	if _, exists := cachedLinkedResult("ready", now.Add(5*time.Second)); exists {
		t.Fatal("pending crossed ready time")
	}
	clearLinkedCache()
	if _, exists := cachedLinkedResult("pending", now); exists {
		t.Fatal("settle/config invalidation failed")
	}
}

func (breakLogScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "MESSAGE_CANARY", "[redacted]")
}

func breakLogFixture(now time.Time, pods, containers int) kubeindex.Snapshot {
	records := []kubeindex.Record{{Kind: "Deployment", APIVersion: "apps/v1", UID: "root", Name: "api", Namespace: "shop", Replicas: int32(pods), Available: 0, Summary: map[string]string{"available_replicas": "0"}}}
	for position := 0; position < pods; position++ {
		pod := kubeindex.Record{Kind: "Pod", APIVersion: "v1", UID: fmt.Sprintf("pod-%d", position), Name: fmt.Sprintf("api-%d", position), Namespace: "shop", CreatedAt: now.Add(time.Duration(position) * time.Second), Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}}}
		for container := 0; container < containers; container++ {
			pod.Containers = append(pod.Containers, kubeindex.ContainerState{Name: fmt.Sprintf("app-%d", container), Init: container == 1, RestartCount: 2, WaitingReason: "CrashLoopBackOff", TerminatedAt: now.Add(-time.Minute), TerminatedReason: "Error", ExitCode: 1})
		}
		records = append(records, pod)
	}
	return kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
}

func TestBreakLogCaptureBoundsDedupNewTerminationAndDeletedPersistence(t *testing.T) {
	now := time.Now().UTC()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/log") {
			reads.Add(1)
			query := request.URL.Query()
			if query.Get("previous") != "true" || query.Get("timestamps") != "true" || query.Get("tailLines") != "500" || query.Get("limitBytes") != "65536" || query.Has("sinceSeconds") {
				t.Errorf("query: %v", query)
			}
			fmt.Fprintf(writer, "%s ERROR MESSAGE_CANARY\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
			return
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		case "/api/v1/namespaces/shop/pods":
			writeJSON(writer, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(provider)
	service.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	snapshot := breakLogFixture(now, 6, 4)
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 15 {
		t.Fatalf("reads=%d want 15", reads.Load())
	}
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 15 || len(state.Gaps) == 0 {
		t.Fatal("capture", len(state.Snapshots), err)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), "MESSAGE_CANARY") {
		t.Fatal("stored canary")
	}
	afterRestart := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	afterRestart.SetScrubber(breakLogScrubber{})
	afterRestart.SetChangeStorage(provider)
	defer afterRestart.StopCapture()
	warming := breakLogFixture(now, 6, 4)
	warming.Records = warming.Records[1:]
	for position := range warming.Records {
		warming.Records[position].Owners = []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "replica", Name: "api-replica", Namespace: "shop", Controller: true, ControllerKnown: true}}
	}
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	warming.Records = append(warming.Records, kubeindex.Record{Kind: "ReplicaSet", UID: "replica", Name: "api-replica", Namespace: "shop", Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}}})
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	warming.Records = append(warming.Records, snapshot.Records[0])
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	if reads.Load() != 15 {
		t.Fatalf("warming restart repeated capped reads: %d", reads.Load())
	}
	state, err = afterRestart.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Jobs) != 1 {
		t.Fatalf("warming index persisted transient jobs: %+v err=%v", state.Jobs, err)
	}
	for _, gap := range state.Gaps {
		if gap.RootUID != "root" {
			t.Fatalf("warming index persisted wrong-root gap: %+v", gap)
		}
	}
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 15 {
		t.Fatalf("unchanged capped job should not retry, reads=%d", reads.Load())
	}
	snapshot = breakLogFixture(now, 1, 1)
	service.captureBreakLogs(t.Context(), snapshot, "")
	before := reads.Load()
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != before {
		t.Fatal("identical trigger reread")
	}
	snapshot.Records[1].Containers[0].RestartCount++
	snapshot.Records[1].Containers[0].TerminatedAt = now.Add(-30 * time.Second)
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != before+1 {
		t.Fatal("new termination not captured")
	}
	restarted := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	restarted.SetChangeStorage(provider)
	restarted.SetScrubber(breakLogScrubber{})
	page, err := restarted.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(-2 * time.Minute), To: now, Limit: 500})
	if err != nil || len(page.Lines) == 0 {
		t.Fatal("deleted persistence", err)
	}
	encoded, _ = json.Marshal(page)
	if strings.Contains(string(encoded), "MESSAGE_CANARY") {
		t.Fatal("returned canary")
	}
}

func TestBreakLogCaptureOwnerKindCoverage(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         string
		intermediate string
		status       kubeindex.KindStatus
		capture      bool
	}{
		{name: "custom rollout", kind: "Rollout", capture: true},
		{name: "rollout replica set", kind: "Rollout", intermediate: "ReplicaSet", capture: true},
		{name: "operator stateful set", kind: "Database", intermediate: "StatefulSet", capture: true},
		{name: "cloud native postgres", kind: "Cluster", capture: true},
		{name: "forbidden owner list", kind: "StatefulSet", status: kubeindex.KindStatus{State: "error", Error: "forbidden", Partial: true}, capture: true},
		{name: "complete absent owner", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC()}, capture: true},
		{name: "complete stale owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC().Add(-time.Hour)}},
		{name: "complete owner list unknown freshness", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready"}},
		{name: "unstarted owner list", kind: "ReplicaSet"},
		{name: "warming owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "warming"}},
		{name: "incomplete owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "partial", Partial: true}},
		{name: "partial ready owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", Partial: true}},
		{name: "failed owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "error", Error: "timeout", Partial: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !strings.HasSuffix(request.URL.Path, "/log") {
					http.NotFound(writer, request)
					return
				}
				reads.Add(1)
				fmt.Fprintf(writer, "%s failure\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
			service.SetScrubber(breakLogScrubber{})
			service.SetChangeStorage(storage.NewMemory())
			defer service.StopCapture()
			snapshot := breakLogFixture(now, 1, 1)
			snapshot.Records = snapshot.Records[1:]
			snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "ready", CompletedAt: now}
			snapshot.Records[0].OwnerObservedAt = now
			owner := kubeindex.OwnerRef{Kind: test.kind, UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}
			snapshot.Records[0].Owners = []kubeindex.OwnerRef{owner}
			if test.intermediate != "" {
				snapshot.Records[0].Owners = []kubeindex.OwnerRef{{Kind: test.intermediate, UID: "intermediate", Name: "api-child", Namespace: "shop", Controller: true, ControllerKnown: true}}
				snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: test.intermediate, UID: "intermediate", Name: "api-child", Namespace: "shop", Owners: []kubeindex.OwnerRef{owner}})
				snapshot.Kinds[test.intermediate] = kubeindex.KindStatus{State: "ready"}
			}
			if test.status.State != "" {
				if test.capture && test.status.State == "ready" {
					test.status.ObservedAt = now.Add(time.Second)
				}
				snapshot.Kinds[test.kind] = test.status
			}
			service.captureBreakLogs(t.Context(), snapshot, "")
			if !test.capture {
				state, err := service.changeStore().BreakLogs(now)
				if err != nil || reads.Load() != 0 || len(state.Jobs) != 0 || len(state.Snapshots) != 0 || len(state.Gaps) != 0 {
					t.Fatalf("incomplete warmup captured evidence: reads=%d state=%+v err=%v", reads.Load(), state, err)
				}
				snapshot.Kinds[test.kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now.Add(time.Second)}
				service.captureBreakLogs(t.Context(), snapshot, "")
			}
			service.captureBreakLogs(t.Context(), snapshot, "")
			state, err := service.changeStore().BreakLogs(time.Now().UTC())
			if err != nil || reads.Load() != 1 || len(state.Jobs) != 1 || len(state.Snapshots) != 1 || len(state.Gaps) != 0 {
				t.Fatalf("owner coverage capture: reads=%d state=%+v err=%v", reads.Load(), state, err)
			}
			if state.Snapshots[0].RootUID != "root" {
				t.Fatalf("wrong root: %+v", state.Snapshots[0])
			}
		})
	}
}

func TestBreakLogCaptureLostLeaseAndCredentialFence(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { reads.Add(1) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	now := time.Now().UTC()
	if err := service.changeStore().ClaimLease("current", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	service.captureBreakLogs(t.Context(), breakLogFixture(now, 1, 1), "stale")
	if reads.Load() != 0 {
		t.Fatal("lost lease read upstream")
	}
	service.identity.cancel()
	service.captureBreakLogs(context.Background(), breakLogFixture(now, 1, 1), "current")
	if reads.Load() != 0 {
		t.Fatal("retired credential read upstream")
	}
}

func TestBreakLogCaptureRetriesTransientBudgetWithoutRepeatingFinalCaps(t *testing.T) {
	now := time.Now().UTC()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			switch request.URL.Path {
			case "/api":
				writeJSON(writer, map[string]any{"versions": []string{"v1"}})
			case "/apis":
				writeJSON(writer, map[string]any{"groups": []any{}})
			case "/api/v1":
				writeJSON(writer, map[string]any{"resources": []any{}})
			case "/api/v1/namespaces/shop/pods":
				writeJSON(writer, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
			default:
				http.NotFound(writer, request)
			}
			return
		}
		if reads.Add(1) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(writer, "%s safe\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	service.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	snapshot := breakLogFixture(now, 1, 1)
	service.captureBreakLogs(t.Context(), snapshot, "")
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Jobs) != 0 || len(state.Gaps) != 1 || state.Gaps[0].Reason != "budget" {
		t.Fatalf("transient budget marked complete: %+v err=%v", state, err)
	}
	service.client.backoffMu.Lock()
	service.client.backoffUntil = time.Time{}
	service.client.backoffMu.Unlock()
	service.captureBreakLogs(t.Context(), snapshot, "")
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 2 {
		t.Fatalf("transient job should retry once then deduplicate: %d", reads.Load())
	}
	state, err = service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 1 || len(state.Gaps) != 0 {
		t.Fatalf("successful retry retained budget failure: %+v err=%v", state, err)
	}
	item := state.Snapshots[0]
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", Mode: "previous", From: item.From, To: item.To})
	if err != nil || page.Coverage.State != "complete" || page.Coverage.Tip || page.Coverage.Reason != "" || len(page.Coverage.Gaps) != 0 {
		t.Fatalf("successful retry API retained stale budget tip: %+v err=%v", page.Coverage, err)
	}
}

func TestBreakLogTruncatedReadKeepsOnlyObservedEvidence(t *testing.T) {
	now := time.Now().UTC()
	first := now.Add(-2 * time.Minute)
	last := now.Add(-time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, "%s %s\n%s safe\n", first.Format(time.RFC3339Nano), strings.Repeat("x", 4097), last.Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api"}, "root", kubeindex.ContainerState{Name: "app"}, "current", now.Add(-5*time.Minute), now)
	if gap != nil || !item.Truncated || !item.From.Equal(first) || !item.To.Equal(last) || len(item.Lines) != 2 {
		t.Fatalf("truncated read overclaimed evidence: %+v gap=%+v", item, gap)
	}
}

func TestBreakLogPodPriorityAndCurrentBounds(t *testing.T) {
	now := time.Now().UTC()
	snapshot := breakLogFixture(now, 3, 1)
	snapshot.Records[1].Containers = nil
	snapshot.Records[1].ReadyStatus = "True"
	snapshot.Records[2].Containers = nil
	snapshot.Records[2].ReadyStatus = "False"
	pods := breakLogPods(snapshot.Records, "shop", "root")
	if len(pods) != 3 || pods[0].UID != "pod-2" || pods[1].UID != "pod-1" {
		t.Fatal("priority", pods)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("sinceTime") == "" || request.URL.Query().Has("sinceSeconds") {
			t.Error("missing exact current window")
		}
		fmt.Fprintf(writer, "%s info safe\n", now.Add(-time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), pods[0], "root", kubeindex.ContainerState{Name: "app"}, "current", now.Add(-5*time.Minute), now)
	if gap != nil || len(item.Lines) != 1 || item.Slot != "current" || !item.From.Equal(now.Add(-5*time.Minute)) || !item.To.Equal(now) || !item.PodCreatedAt.Equal(pods[0].CreatedAt) {
		t.Fatal("current", gap)
	}
}

func TestBreakLogEmptyReadPreservesObservedGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api"}, "root", kubeindex.ContainerState{Name: "app"}, "current", from, now)
	if gap == nil || gap.Reason != "capture_late" || !gap.From.Equal(from) || !gap.To.Equal(now) || item.Truncated || len(item.Lines) != 0 || !item.From.Equal(from) || !item.To.Equal(now) {
		t.Fatalf("empty read lost observed window: %+v gap=%+v", item, gap)
	}
}

func TestBreakLogCaptureDualSlotAndStopDrainsJob(t *testing.T) {
	now := time.Now().UTC()
	var previous, current atomic.Int32
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("previous") == "true" {
			previous.Add(1)
		} else {
			current.Add(1)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	snapshot := breakLogFixture(now, 1, 1)
	snapshot.Records[1].Containers[0].WaitingReason = ""
	snapshot.Records[1].Containers[0].Running = true
	snapshot.Records[1].Containers[0].StateKnown = true
	service.enqueueBreakLogs(snapshot, "", t.Context())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job not started")
	}
	done := make(chan struct{})
	go func() { service.StopCapture(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop failed to drain")
	}
	if previous.Load() != 1 || current.Load() != 0 {
		t.Fatal("cancellation failed")
	}
	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("previous") == "true" {
			previous.Add(1)
		} else {
			current.Add(1)
		}
		fmt.Fprintf(writer, "%s info safe\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
	}))
	defer fast.Close()
	other := newTestService(t, fast.URL, Scope{ClusterID: "cluster"})
	other.SetChangeStorage(storage.NewMemory())
	other.SetScrubber(breakLogScrubber{})
	other.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	other.captureBreakLogs(t.Context(), snapshot, "")
	if previous.Load() != 2 || current.Load() != 1 {
		t.Fatal("running unready container did not capture both slots")
	}
}

func TestBreakLogContainersPickTheMainContainer(t *testing.T) {
	pod := func(main string, containers ...kubeindex.ContainerState) kubeindex.Record {
		return kubeindex.Record{Kind: "Pod", UID: "pod", MainContainer: main, LogContainers: containers}
	}
	cases := []struct {
		name string
		pods []kubeindex.Record
		want string
	}{
		{"declared default container wins over spec order", []kubeindex.Record{pod("app", kubeindex.ContainerState{Name: "proxy"}, kubeindex.ContainerState{Name: "app"})}, "app"},
		{"native sidecars and init containers are never main", []kubeindex.Record{pod("", kubeindex.ContainerState{Name: "mesh", Init: true}, kubeindex.ContainerState{Name: "worker"})}, "worker"},
		{"an init-only main hint falls back to the first app container", []kubeindex.Record{pod("migrate", kubeindex.ContainerState{Name: "migrate", Init: true}, kubeindex.ContainerState{Name: "web"}, kubeindex.ContainerState{Name: "cache"})}, "web"},
		{"only init containers leaves no main", []kubeindex.Record{pod("", kubeindex.ContainerState{Name: "migrate", Init: true})}, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			listed, main := breakLogContainers(test.pods, nil)
			if main != test.want {
				t.Fatalf("main=%q want %q (listed %+v)", main, test.want, listed)
			}
			mains := 0
			for position, container := range listed {
				if container.Main {
					mains++
				}
				if position > 0 && listed[position-1].Init && !container.Init {
					t.Fatalf("init container listed before an app container: %+v", listed)
				}
			}
			if test.want != "" && mains != 1 || test.want == "" && mains != 0 {
				t.Fatalf("main flags=%d listed=%+v", mains, listed)
			}
		})
	}
}

func TestBreakLogContainerFilterSelectsOneContainerAndRejectsUnknownNames(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	snapshots := []breaklogs.Snapshot{}
	for _, container := range []struct {
		name string
		init bool
	}{{"sidecar", false}, {"app", false}, {"migrate", true}} {
		item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "deleted", RootUID: "deleted-root", Pod: "api-1", PodUID: "pod", Container: container.name, Init: container.init, Slot: "previous", CapturedAt: now, From: from, To: now, TerminationAt: now.Add(-time.Minute), Lines: []breaklogs.Line{{At: now.Add(-2 * time.Minute), Text: container.name + " MESSAGE_CANARY"}}}
		item.PodCreatedAt = from.Add(-time.Minute)
		item.ID = breaklogs.Identity(item)
		snapshots = append(snapshots, item)
	}
	if err := service.changeStore().CommitBreakLogs(snapshots, nil, "job", "", now); err != nil {
		t.Fatal(err)
	}
	read := func(container string) (BreakLogPage, error) {
		return service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: from, To: now, Container: container})
	}
	only := func(page BreakLogPage, want ...string) {
		t.Helper()
		got := map[string]bool{}
		for _, line := range page.Lines {
			got[line.Container] = true
			if strings.Contains(line.Text, "MESSAGE_CANARY") {
				t.Fatal("container-filtered line escaped redaction")
			}
		}
		if len(got) != len(want) {
			t.Fatalf("lines from %v, want %v", got, want)
		}
		for _, name := range want {
			if !got[name] {
				t.Fatalf("lines from %v, want %v", got, want)
			}
		}
		for _, marker := range page.Markers {
			if marker.Type == "crash" && len(want) == 1 && !strings.HasPrefix(marker.Label, want[0]+" ") {
				t.Fatalf("termination marker %q from another container", marker.Label)
			}
		}
	}
	defaultPage, err := read("")
	if err != nil || defaultPage.Container != "app" || defaultPage.MainContainer != "app" {
		t.Fatalf("default page container=%q main=%q err=%v", defaultPage.Container, defaultPage.MainContainer, err)
	}
	only(defaultPage, "app")
	if len(defaultPage.Containers) != 3 || defaultPage.Containers[0] != (BreakLogContainer{Name: "app", Main: true}) || defaultPage.Containers[1] != (BreakLogContainer{Name: "sidecar"}) || defaultPage.Containers[2] != (BreakLogContainer{Name: "migrate", Init: true}) {
		t.Fatalf("containers=%+v", defaultPage.Containers)
	}
	if defaultPage.Counts.All != 1 || defaultPage.Coverage.State != "complete" {
		t.Fatalf("default page counts=%+v coverage=%+v", defaultPage.Counts, defaultPage.Coverage)
	}
	sidecar, err := read("sidecar")
	if err != nil || sidecar.Container != "sidecar" || len(sidecar.Containers) != 3 {
		t.Fatalf("sidecar page=%+v err=%v", sidecar, err)
	}
	only(sidecar, "sidecar")
	all, err := read(breaklogs.AllContainers)
	if err != nil || all.Container != breaklogs.AllContainers || all.Counts.All != 3 {
		t.Fatalf("all containers page=%+v err=%v", all, err)
	}
	only(all, "app", "sidecar", "migrate")
	if _, err := read("not-a-container"); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("unknown container accepted: %v", err)
	}
	paged, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: from, To: now, Container: breaklogs.AllContainers, Limit: 1})
	if err != nil || paged.Next == "" {
		t.Fatalf("paged=%+v err=%v", paged, err)
	}
	if _, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: from, To: now, Container: "app", Limit: 1, Cursor: paged.Next}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("cursor crossed container filters: %v", err)
	}
}

// Mirror the UUID and opt-in IP rules used by the application scrubber.
type apiShapeScrubber struct{}

func (apiShapeScrubber) Scrub(text string) string {
	for _, pattern := range []string{`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`, `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`, `token=\S+`} {
		text = regexp.MustCompile(pattern).ReplaceAllString(text, "<REDACTED>")
	}
	return text
}
func TestBreakManifestRetainsRealAPIUUIDs(t *testing.T) {
	service := &Service{scrubber: apiShapeScrubber{}}
	at := time.Now().UTC()
	root := "830b1528-4c35-49f0-8a07-c284e1a2aacf"
	uid := "f8108b39-8bf1-4124-920b-dbe5b59feab1"
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "Deployment": {State: "ready"}}, Records: []kubeindex.Record{
		{Kind: "Deployment", Namespace: "shop", Name: "api", UID: root},
		{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: uid, CreatedAt: at.Add(-time.Hour), Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: root, Controller: true}}, Containers: []kubeindex.ContainerState{{Name: "app"}}},
	}}
	pods := service.scrubManifestPods(verifiedBreakPods(snapshot))
	if len(pods) != 1 || pods[0].UID != uid || pods[0].RootUID != root {
		t.Fatalf("verified UUID identity discarded: %+v", pods)
	}
}
func TestBreakContainerPreservesKubeletTimestampWithIPScrubbing(t *testing.T) {
	at := time.Date(2026, 11, 1, 6, 4, 1, 360000000, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s token=sensitive error\n", at.Format(time.RFC3339Nano))
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(apiShapeScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api-0", UID: "pod"}, "root", kubeindex.ContainerState{Name: "app", TerminatedAt: at.Add(time.Second).Truncate(time.Second)}, "previous", at.Add(-time.Minute), at.Add(time.Minute))
	if gap != nil || len(item.Lines) != 1 || !item.Lines[0].At.Equal(at) || strings.Contains(item.Lines[0].Text, "sensitive") {
		t.Fatalf("timestamp lost or secret exposed: %+v %+v", item, gap)
	}
}
func TestBreakContainerEmptyReadLeavesCaptureGap(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	at := time.Now().UTC()
	_, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api-0", UID: "pod"}, "root", kubeindex.ContainerState{Name: "app"}, "current", at.Add(-time.Minute), at)
	if gap == nil || gap.Reason != "capture_late" {
		t.Fatalf("empty capture falsely closed coverage: %+v", gap)
	}
}
func TestBreakLogsRootOnlyChecksCurrentPodAuthority(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pods") {
			http.Error(w, "forbidden", 403)
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "synthetic")})
	if err != nil {
		t.Fatal(err)
	}
	service.client = client
	// Remove the protected manifest: root-only reads still carry protected facts.
	service.SetChangeStorage(storage.NewMemory())
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, From: manifest.From, To: manifest.To})
	if !errors.Is(err, ErrForbidden) || len(page.Pods) != 0 || len(page.Markers) != 0 {
		t.Fatalf("root-only read bypassed authority: %+v %v", page, err)
	}
}

func TestOccurrenceBreakLogsUsesControlReadLane(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	service.client.indexLimiter = rate.NewLimiter(0, 1)
	service.client.indexLimiter.Allow()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := service.BreakLogs(ctx, breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, OccurrenceID: manifest.OccurrenceID, From: manifest.From, To: manifest.To})
	if err != nil {
		t.Fatalf("occurrence blocked behind inventory: %v", err)
	}
}
func TestBreakManifestHistoryUsesControlReadLane(t *testing.T) {
	service, _ := acceptanceLinkedService(t)
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"physical"}}}`)))
	var proofs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proofs.Add(1); fixture.ServeHTTP(w, r) }))
	defer upstream.Close()
	client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "synthetic")})
	if err != nil {
		t.Fatal(err)
	}
	service.client = client
	service.changes.evidenceEnabled = true
	service.client.indexLimiter = rate.NewLimiter(0, 1)
	service.client.indexLimiter.Allow()
	ctx, cancel := context.WithTimeout(captureReadContext(t.Context(), ""), time.Second)
	defer cancel()
	at := time.Now().UTC()
	service.newBreakManifest(ctx, kubebreaks.Break{Root: kubebreaks.Ref(kubeindex.Record{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: "pod"}), Rule: "pod.crash_loop", BrokeAt: at}, breaklogs.State{}, at)
	if proofs.Load() < 8 || ctx.Err() != nil {
		t.Fatalf("capture metadata blocked behind bulk inventory: proofs=%d err=%v", proofs.Load(), ctx.Err())
	}
}
func TestCachedWorkloadsReturnsTypedAuthorityFailure(t *testing.T) {
	for _, status := range []int{403, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			service, _ := acceptanceLinkedService(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", status) }))
			defer upstream.Close()
			client, err := NewClient(Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "synthetic")})
			if err != nil {
				t.Fatal(err)
			}
			service.client = client
			cache := &inventoryCache{authorityFailure: unverifiedInventoryAuthority(context.DeadlineExceeded), generation: inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now()}}}}}
			service.changes.mu.Lock()
			service.changes.inventories[service.indexScope()] = cache
			service.changes.mu.Unlock()
			_, err = service.Workloads(t.Context(), WorkloadListOptions{})
			want := ErrForbidden
			if status == 401 {
				want = ErrUnauthorized
			}
			if !errors.Is(err, want) {
				t.Fatalf("authority loss mislabeled: %v", err)
			}
			cache.authorityFailure = nil
			page, err := service.Workloads(t.Context(), WorkloadListOptions{})
			if !errors.Is(err, want) || len(page.Items) > 0 {
				t.Fatalf("cached publication bypassed authority: %+v %v", page, err)
			}
		})
	}
}

func TestBreakCaptureAdmitsEveryNamespaceBeforeSlowLogs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	at := time.Now().UTC()
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: at}}}
	for _, namespace := range []string{"payments", "new-namespace"} {
		snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: "Pod", Namespace: namespace, Name: "api-0", UID: namespace + "-pod", CreatedAt: at.Add(-time.Hour), ReadyStatus: "False", Phase: "Running", ReadyTransitionAt: at.Add(-time.Minute), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 1, TerminatedAt: at.Add(-time.Second), StateKnown: true}}})
	}
	// A real cluster-scoped Node break has no namespace or container logs.
	// It must remain classified without invalidating the namespace manifests.
	snapshot.Kinds["Node"] = kubeindex.KindStatus{State: "ready", ObservedAt: at}
	snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: "Node", Name: "worker", UID: "node", NodeReady: false})
	roots, _ := classifyReadyBreaks(snapshot, at)
	if len(roots) != 3 {
		t.Fatalf("fixture must include the Node and both namespace breaks: %+v", roots)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	service.captureBreakLogs(ctx, snapshot, "")
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, manifest := range state.Manifests {
		found[manifest.Namespace] = true
	}
	if !found["payments"] || !found["new-namespace"] || found[""] {
		t.Fatalf("slow root starved namespace admission: %+v", found)
	}
}
func TestBreakContainerCapturesCurrentlyTerminatedKubeletSlot(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("previous") == "true" {
			http.Error(w, "previous unavailable", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "%s fatal before restart\n", at.Format(time.RFC3339Nano))
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api-0", UID: "pod"}, "root", kubeindex.ContainerState{Name: "app", TerminatedAt: at, CurrentTermination: true}, "previous", at.Add(-time.Minute), at.Add(time.Minute))
	if gap != nil || len(item.Lines) != 1 || item.Slot != "previous" {
		t.Fatalf("currently terminated logs requested from previous kubelet slot: item=%+v gap=%+v", item, gap)
	}
}

func TestBreakContainerRejectsNewerPreviousRestartWindow(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s later restart error\n", at.Add(2*time.Second).Format(time.RFC3339Nano))
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api-0", UID: "pod"}, "root", kubeindex.ContainerState{Name: "app", TerminatedAt: at}, "previous", at.Add(-time.Minute), at.Add(time.Minute))
	if gap == nil || len(item.Lines) != 0 {
		t.Fatalf("new previous slot falsely closed old restart coverage: %+v %+v", item, gap)
	}
}

func TestBreakManifestUsesPinnedOccurrenceCenterAndMissingManifestSelection(t *testing.T) {
	service, store, scope := reviewHistoryService(t)
	service.SetScrubber(breakLogScrubber{})
	at := time.Now().UTC().Add(-time.Minute)
	root := ObjectRef{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: "root"}
	raw, _ := json.Marshal(Issue{Root: root, Rule: "pod.crash_loop", Severity: "warning", Broken: true, BrokeAt: &at})
	if err := store.CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "capture:opened", At: at, Epoch: "synthetic", Issues: []storage.EvidenceIssue{{AuthorizationKind: "Pod", Key: "root-rule", Kind: "Pod", UID: root.UID, Namespace: root.Namespace, Rule: "pod.crash_loop", Broken: true, Payload: raw}}}); err != nil {
		t.Fatal(err)
	}
	history, err := service.historicalIssues(t.Context(), IssueOptions{Namespace: "shop", RootUID: "root", Since: at.Add(-time.Minute), Until: time.Now().UTC()})
	if err != nil || len(history.Items) != 1 {
		t.Fatal(history, err)
	}
	id := history.Items[0].OccurrenceID
	service.client.indexLimiter = rate.NewLimiter(0, 1)
	service.client.indexLimiter.Allow()
	ctx, cancel := context.WithTimeout(captureReadContext(t.Context(), ""), time.Second)
	defer cancel()
	manifest := service.newBreakManifest(ctx, kubebreaks.Break{Root: kubebreaks.Ref(kubeindex.Record{Kind: root.Kind, Namespace: root.Namespace, Name: root.Name, UID: root.UID}), Rule: "pod.crash_loop", BrokeAt: at.Add(30 * time.Second)}, breaklogs.State{}, time.Now().UTC())
	if manifest.OccurrenceID != id || !manifest.BreakAt.Equal(at) {
		t.Fatalf("manifest ignored pinned occurrence time: %+v", manifest)
	}
	resolved, err := service.resolveBreakLogSelection(ctx, breaklogs.Query{Namespace: "shop", RootUID: "root", OccurrenceID: id, Center: at, From: at.Add(-time.Minute), To: at}, breaklogs.State{})
	if err != nil || !resolved.Center.Equal(at) {
		t.Fatalf("uncaptured occurrence failed selection: %+v %v", resolved, err)
	}
}

func TestPreviousCaptureKeepsFractionalLinesInTerminationSecond(t *testing.T) {
	finished := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	lineAt := finished.Add(750 * time.Millisecond)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s final crash line\n", lineAt.Format(time.RFC3339Nano))
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api-0", UID: "pod"}, "root", kubeindex.ContainerState{Name: "app", TerminatedAt: finished}, "previous", finished.Add(-time.Minute), finished.Add(time.Minute))
	if gap != nil || len(item.Lines) != 1 || !item.Lines[0].At.Equal(lineAt) {
		t.Fatalf("API-rounded finishedAt dropped final-second lines: %+v %+v", item, gap)
	}
}
func TestBreakCaptureAdmitsNamespacesDespiteSlowHistoricalProof(t *testing.T) {
	var ready atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system" {
			if !ready.Load() {
				<-r.Context().Done()
				return
			}
			writeJSON(w, map[string]any{"metadata": map[string]any{"uid": "physical"}})
			return
		}
		writeJSON(w, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadataList", "items": []any{}})
	}))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	defer service.StopCapture()
	at := time.Now().UTC()
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: at}}}
	batch := storage.EvidenceBatch{ID: "capture:opened", At: at.Add(-time.Minute), Epoch: "synthetic"}
	for _, namespace := range []string{"payments", "new-namespace"} {
		snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: "Pod", Namespace: namespace, Name: "api-0", UID: namespace + "-pod", CreatedAt: at.Add(-time.Hour), ReadyStatus: "False", Phase: "Running", ReadyTransitionAt: at.Add(-time.Minute), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 1, TerminatedAt: at.Add(-time.Second), StateKnown: true}}})
		opened := at.Add(-time.Minute)
		root := ObjectRef{Kind: "Pod", Namespace: namespace, Name: "api-0", UID: namespace + "-pod"}
		raw, _ := json.Marshal(Issue{Root: root, Rule: "pod.crash_loop", Broken: true, BrokeAt: &opened})
		batch.Issues = append(batch.Issues, storage.EvidenceIssue{Key: namespace, Namespace: namespace, Kind: "Pod", AuthorizationKind: "Pod", UID: root.UID, Rule: "pod.crash_loop", Broken: true, Payload: raw})
	}
	if err := service.evidenceStore().CommitEvidence(t.Context(), storage.EvidenceScope{OrgID: "default", ClusterID: "cluster", ClusterUID: "physical"}, batch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	service.captureBreakLogs(ctx, snapshot, "")
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Manifests) != 0 || len(state.Jobs) != 0 || len(state.Snapshots) != 0 || len(state.Gaps) != 0 {
		t.Fatalf("deadline fallback admitted unpinned occurrence or marked it complete: %+v", state)
	}
	ready.Store(true)
	// A later observation has a different condition time; retry must still
	// admit the opening persisted occurrence rather than a fallback center.
	for i := range snapshot.Records {
		snapshot.Records[i].ReadyTransitionAt = at.Add(-30 * time.Second)
	}
	retry, stopRetry := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopRetry()
	service.captureBreakLogs(retry, snapshot, "")
	state, err = service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, manifest := range state.Manifests {
		found[manifest.Namespace] = true
		if manifest.OccurrenceID == "" || manifest.ID != manifest.OccurrenceID || !manifest.BreakAt.Equal(batch.At) {
			t.Fatalf("retry admitted drifting fallback instead of persisted occurrence: %+v", manifest)
		}
	}
	if !found["payments"] || !found["new-namespace"] {
		t.Fatalf("skipped namespaces did not retry after historical proof recovered: %+v", found)
	}
}

func TestEnqueuedInventoryRetainsRecoveredStandalonePod(t *testing.T) {
	service, _ := acceptanceLinkedService(t)
	at := time.Now().UTC()
	pod := breaklogs.ManifestPod{Namespace: "shop", RootUID: "pod", Name: "api-0", UID: "pod", CreatedAt: at.Add(-time.Hour), LastSeen: at.Add(-time.Second), Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
	if err := service.changeStore().ObserveBreakPods([]breaklogs.ManifestPod{pod}, "", at.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: at}}, Records: []kubeindex.Record{{Kind: "Pod", Namespace: "shop", Name: "api-0", UID: "pod", CreatedAt: pod.CreatedAt, ReadyStatus: "True", Phase: "Running", Containers: []kubeindex.ContainerState{{Name: "app", Ready: true, Running: true}}}}}
	service.enqueueBreakLogs(snapshot, "", t.Context())
	service.changes.mu.Lock()
	done := service.changes.logDone[service.indexScope()]
	service.changes.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("capture did not drain")
		}
	}
	state, err := service.changeStore().BreakLogs(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PodHistory) != 1 || !state.PodHistory[0].DeletedAt.IsZero() || !state.PodHistory[0].LastSeen.Equal(at) {
		t.Fatalf("recovered standalone Pod falsely deleted: %+v", state.PodHistory)
	}
}

func TestPodLogExactKubeletByteCapReportsTruncation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 64<<10)) }))
	defer upstream.Close()
	service := newTestService(t, upstream.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	logs, err := service.podLogs(t.Context(), "shop", "api-0", "app", true, 0, 500, true, podLogBounds{LimitBytes: 64 << 10})
	if err != nil || !logs.Truncated {
		t.Fatalf("kubelet exact byte cap falsely complete: truncated=%v err=%v", logs.Truncated, err)
	}
}
