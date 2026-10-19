package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"golang.org/x/time/rate"
)

type settingsLinkedReader struct{ err error }

func (reader settingsLinkedReader) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
		return breaklogs.Result{}, errors.New("missing deadline")
	}
	if query.To.Sub(query.From) != 15*time.Minute || query.Namespace == "" {
		return breaklogs.Result{}, errors.New("wrong test window")
	}
	return breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: query.To, Text: "safe"}}}, reader.err
}
func (reader settingsLinkedReader) Preview(query breaklogs.LinkedQuery) string {
	return "bounded preview"
}

func TestAgentSettingsPersistenceTestLinkHealthAndRemovedSource(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	service.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	var failure error
	service.SetBreakLogSources(func(org string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(fields breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return settingsLinkedReader{err: failure}, nil
		}}}
	})
	settings := breaklogs.Settings{Learning: breaklogs.Learning{Logs: true, Metrics: true, Traces: true, Scope: breaklogs.Scope{Include: []string{"shop"}, Exclude: []string{"kube-system"}}}, BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}
	if err := service.SaveAgentSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	value, err := service.AgentSettings(t.Context())
	if err != nil || !value.Learning.Logs || value.BreakLogs.LinkedSource != "logs" || len(value.BreakLogs.Fields) != 6 || value.LinkedSourceMissing || value.Sources[0].Health != "not_checked" {
		t.Fatalf("settings=%+v err=%v", value, err)
	}
	result, err := service.TestLogLink(t.Context(), TestLinkRequest{Source: "logs"})
	if err != nil || result.Lines != 1 || result.Preview != "bounded preview" || result.Namespace == "" || result.Workload == "" {
		t.Fatal("test link", result, err)
	}
	value, err = service.AgentSettings(t.Context())
	if err != nil || value.Sources[0].Health != "healthy" || value.Sources[0].CheckedAt == nil {
		t.Fatal("health", err)
	}
	for _, class := range []string{"unauthorized", "timeout", "not_found"} {
		failure = errors.New(class)
		if _, err := service.TestLogLink(t.Context(), TestLinkRequest{Source: "logs"}); err == nil || err.Error() != class {
			t.Fatal("error class", err)
		}
	}
	if _, err := service.TestLogLink(t.Context(), TestLinkRequest{Source: "logs", Fields: breaklogs.Fields{Pod: "in valid"}}); err == nil || err.Error() != "invalid_mapping" {
		t.Fatal("invalid mapping", err)
	}
	if _, err := service.TestLogLink(t.Context(), TestLinkRequest{Source: "unknown"}); err == nil || err.Error() != "not_found" {
		t.Fatal("unknown source", err)
	}
	service.SetBreakLogSources(nil)
	value, err = service.AgentSettings(t.Context())
	if err != nil || !value.LinkedSourceMissing {
		t.Fatal("removed source", err)
	}
	if err := service.SaveAgentSettings(t.Context(), settings); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("unknown source save", err)
	}
	settings.BreakLogs.LinkedSource = ""
	settings.Learning.Scope.Include = []string{"../escape"}
	if err := service.SaveAgentSettings(t.Context(), settings); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("namespace scope", err)
	}
}

func TestDetectAgentBackendsPureAndDeterministic(t *testing.T) {
	for _, kind := range []string{"jaeger", "jaeger-query", "tempo", "opentelemetry-collector", "otel-collector", "zipkin"} {
		records := []kubeindex.Record{{Kind: "Service", Namespace: "telemetry", Name: "backend", Labels: map[string]string{"app.kubernetes.io/name": kind}}, {Kind: "Deployment", Namespace: "shop", Name: "api"}, {Kind: "StatefulSet", Namespace: "shop", Name: "db"}}
		result := DetectAgentBackends(records)
		if result.Traces == nil || result.Traces.Kind != kind || result.Traces.Namespace != "telemetry" || result.Workloads != 2 || result.Namespaces != 1 {
			t.Fatal("detection", result)
		}
	}
	if result := DetectAgentBackends([]kubeindex.Record{{Kind: "Pod", Name: "tempo"}, {Kind: "Service", Name: "tempo-like"}}); result.Traces != nil {
		t.Fatal("false positive")
	}
}

type gapLinkedReader struct {
	truncated bool
	err       error
	empty     bool
	tail      bool
	pod       string
	observed  *breaklogs.LinkedQuery
}

func (reader gapLinkedReader) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	if reader.observed != nil {
		*reader.observed = query
	}
	if reader.empty {
		return breaklogs.Result{}, reader.err
	}
	pod := reader.pod
	if pod == "" {
		pod = "api-1"
	}
	lines := []breaklogs.LinkedLine{{At: query.From.Add(30 * time.Second), Pod: pod, Container: "app", Text: "ERROR MESSAGE_CANARY"}}
	if reader.tail {
		lines = append(lines, breaklogs.LinkedLine{At: query.To.Add(-time.Nanosecond), Pod: pod, Container: "app", Text: "safe tail"})
	}
	return breaklogs.Result{Lines: lines, Truncated: reader.truncated}, reader.err
}
func (reader gapLinkedReader) Preview(query breaklogs.LinkedQuery) string { return "bounded gap" }

func TestBreakLogLinkedGapClosesOnlyOnCompleteRead(t *testing.T) {
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
	var truncated bool
	var failure error
	var empty bool
	var tail bool
	var pod string
	var observed breaklogs.LinkedQuery
	service.SetBreakLogSources(func(org string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(fields breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return gapLinkedReader{truncated: truncated, err: failure, empty: empty, tail: tail, pod: pod, observed: &observed}, nil
		}}}
	})
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "deleted", RootUID: "deleted-root", Pod: "api-1", PodUID: "pod", Container: "app", Slot: "previous", CapturedAt: now, From: from.Add(time.Minute), To: now, Lines: []breaklogs.Line{{At: now, Text: "safe"}}}
	item.PodCreatedAt = from.Add(-time.Minute)
	item.ID = breaklogs.Identity(item)
	if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", "", now); err != nil {
		t.Fatal(err)
	}
	query := breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: from, To: now}
	page, err := service.BreakLogs(t.Context(), query)
	if err != nil || page.Coverage.State != "complete" || page.Coverage.Tip {
		t.Fatal("link did not close gap", page.Coverage, err)
	}
	found := false
	for _, line := range page.Lines {
		if line.Source == "linked" {
			found = true
			if line.Text != "ERROR [redacted]" {
				t.Fatal("linked canary escaped")
			}
		}
	}
	if !found {
		t.Fatal("no linked line")
	}
	query.To = now.Add(2 * time.Minute)
	tail = true
	started := time.Now().UTC()
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || observed.To.Before(started) || observed.To.After(time.Now().UTC()) || !page.Window.To.Equal(query.To) || page.Coverage.State != "pending" || page.Coverage.Tip || page.Coverage.Reason != "pending" {
		t.Fatalf("future request not bounded: read=%+v page=%+v err=%v", observed, page.Coverage, err)
	}
	for _, gap := range page.Coverage.Gaps {
		if gap.From.Before(observed.To) || gap.Reason != "pending" {
			t.Fatal("past evidence-backed gap remained", gap)
		}
	}
	query.To = now
	tail = false
	for _, reason := range []string{"forbidden", "pod_deleted", "restarted_since"} {
		if err := service.changeStore().CommitBreakLogs(nil, []breaklogs.Gap{{From: from, To: now, Namespace: "deleted", RootUID: "deleted-root", PodUID: "pod", Container: "app", Reason: reason}}, "", "", now); err != nil {
			t.Fatal(err)
		}
		for _, unrelated := range []bool{false, true} {
			empty = !unrelated
			if unrelated {
				pod = "foreign-1"
			}
			page, err = service.BreakLogs(t.Context(), query)
			if err != nil || !page.Coverage.Tip || page.Coverage.State == "complete" {
				t.Fatalf("empty/unrelated read erased %s: %+v err=%v", reason, page.Coverage, err)
			}
			retained := false
			for _, gap := range page.Coverage.Gaps {
				retained = retained || gap.Reason == reason
			}
			if !retained {
				t.Fatalf("lost %s gap: %+v", reason, page.Coverage)
			}
		}
		empty, pod = false, ""
		page, err = service.BreakLogs(t.Context(), query)
		if err != nil || page.Coverage.State != "complete" || page.Coverage.Tip {
			t.Fatalf("matching read did not close %s: %+v err=%v", reason, page.Coverage, err)
		}
	}
	truncated = true
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || !page.Coverage.Tip || page.Coverage.State == "complete" {
		t.Fatal("truncation closed gap", err)
	}
	truncated = false
	failure = errors.New("unauthorized")
	page, err = service.BreakLogs(t.Context(), query)
	if err != nil || !page.Coverage.Tip || page.Coverage.State == "complete" {
		t.Fatal("failure closed gap", err)
	}
}

func TestBreakLogDeletedReplacementRequiresCreationProvenance(t *testing.T) {
	for _, name := range []string{"unknown_lifetime", "predates_replacement", "known_lifetime", "no_scope"} {
		t.Run(name, func(t *testing.T) {
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
			if name != "no_scope" {
				item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "deleted", RootUID: "deleted-root", Pod: "api-0", PodUID: "B", Container: "app", Slot: "current", CapturedAt: now, From: from.Add(2 * time.Minute), To: now, Lines: []breaklogs.Line{{At: now, Text: "replacement"}}}
				if name == "predates_replacement" {
					item.PodCreatedAt = from.Add(time.Minute)
				}
				if name == "known_lifetime" {
					item.PodCreatedAt = from.Add(-time.Minute)
				}
				item.ID = breaklogs.Identity(item)
				if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", "", now); err != nil {
					t.Fatal(err)
				}
			}
			var observed breaklogs.LinkedQuery
			service.SetBreakLogSources(func(string) []breaklogs.Source {
				return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
					return gapLinkedReader{pod: "api-0", observed: &observed}, nil
				}}}
			})
			if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}); err != nil {
				t.Fatal(err)
			}
			page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "deleted-root", From: from, To: now})
			if err != nil {
				t.Fatal(err)
			}
			linked := 0
			for _, line := range page.Lines {
				if line.Source == "linked" {
					linked++
				}
			}
			if name == "known_lifetime" {
				if linked != 1 || page.Coverage.Tip {
					t.Fatalf("known lifetime failed to close gap: %+v", page)
				}
			} else if linked != 0 || !page.Coverage.Tip {
				t.Fatalf("unsafe historical row invented identity/coverage: %+v", page)
			}
			if name == "no_scope" {
				settings, err := service.AgentSettings(t.Context())
				if err != nil || !observed.From.IsZero() || len(settings.Sources) != 1 || settings.Sources[0].Health != "not_checked" || settings.Sources[0].CheckedAt != nil {
					t.Fatalf("unscoped read changed health: query=%+v settings=%+v err=%v", observed, settings, err)
				}
			}
		})
	}
}

func TestBreakLogLinkedReusedStatefulSetPodNameKeepsIdentityGap(t *testing.T) {
	for _, savedOldPod := range []bool{true, false} {
		name := "saved_old_pod"
		if !savedOldPod {
			name = "no_saved_old_pod"
		}
		t.Run(name, func(t *testing.T) {
			testBreakLogLinkedReusedStatefulSetPodNameKeepsIdentityGap(t, savedOldPod)
		})
	}
}

func testBreakLogLinkedReusedStatefulSetPodNameKeepsIdentityGap(t *testing.T, savedOldPod bool) {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/namespaces/shop/pods/api-0/log" {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		fixture.ServeHTTP(writer, request)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		records := []kubeindex.Record{}
		if kind == "StatefulSet" {
			records = append(records, kubeindex.Record{Kind: kind, APIVersion: "apps/v1", Namespace: "shop", Name: "api", UID: "root"})
		}
		if kind == "Pod" {
			records = append(records, kubeindex.Record{Kind: kind, APIVersion: "v1", Namespace: "shop", Name: "api-0", UID: "B", Containers: []kubeindex.ContainerState{{Name: "app", Running: true, StateKnown: true}}, Owners: []kubeindex.OwnerRef{{Kind: "StatefulSet", Name: "api", UID: "root", Namespace: "shop", Controller: true, ControllerKnown: true}}})
			if !savedOldPod {
				records[0].CreatedAt = from.Add(2 * time.Minute)
			}
		}
		if err := index.Ingest(kind, records, true, now); err != nil {
			t.Fatal(err)
		}
	}
	if savedOldPod {
		item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: "api-0", PodUID: "A", Container: "app", Slot: "previous", CapturedAt: now, From: from, To: now, Lines: []breaklogs.Line{{At: now, Text: "saved old pod"}}}
		item.ID = breaklogs.Identity(item)
		if err := service.changeStore().CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", "", now); err != nil {
			t.Fatal(err)
		}
	}
	var observed breaklogs.LinkedQuery
	empty := true
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return gapLinkedReader{pod: "api-0", empty: empty, observed: &observed}, nil
		}}}
	})
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}); err != nil {
		t.Fatal(err)
	}
	query := breaklogs.Query{Namespace: "shop", RootUID: "root", From: from, To: now}
	baseline, err := service.BreakLogs(t.Context(), query)
	if err != nil || !baseline.Coverage.Tip || len(baseline.Coverage.Gaps) == 0 {
		t.Fatalf("missing baseline gap: coverage=%+v err=%v", baseline.Coverage, err)
	}
	empty = false
	page, err := service.BreakLogs(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if savedOldPod {
		if len(page.Pods) != 2 || page.Pods[0].UID != "A" || page.Pods[1].UID != "B" {
			t.Fatalf("missing reused-name identities: %+v", page.Pods)
		}
	} else if len(page.Pods) != 1 || page.Pods[0].UID != "B" {
		t.Fatalf("expected only indexed replacement identity: %+v", page.Pods)
	}
	if len(observed.Pods) != 1 || observed.Pods[0] != "api-0" {
		t.Errorf("source query must deduplicate pod names: %+v", observed)
	}
	for _, line := range page.Lines {
		if line.Source == "linked" {
			t.Errorf("ambiguous linked row received invented identity/slot: %+v", line)
		}
	}
	if !page.Coverage.Tip || page.Coverage.State != baseline.Coverage.State || page.Coverage.Reason != baseline.Coverage.Reason || len(page.Coverage.Gaps) != len(baseline.Coverage.Gaps) {
		t.Fatalf("ambiguous rows closed coverage: before=%+v after=%+v", baseline.Coverage, page.Coverage)
	}
	for position, gap := range baseline.Coverage.Gaps {
		actual := page.Coverage.Gaps[position]
		gap.ObservedAt, actual.ObservedAt = time.Time{}, time.Time{}
		if actual != gap {
			t.Errorf("ambiguous rows changed gap: before=%+v after=%+v", gap, actual)
		}
	}
}

func TestBreakLogLinkedCoverageKeepsUnmatchedContainerGap(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-time.Minute)
	page := BreakLogPage{Window: BreakLogWindow{From: from, To: now}, Pods: []BreakLogPod{{UID: "pod", Name: "api-1"}}, Coverage: breaklogs.Coverage{Gaps: []breaklogs.Gap{
		{From: from, To: now, PodUID: "pod", Container: "app", Reason: "forbidden"},
		{From: from, To: now, PodUID: "pod", Container: "sidecar", Reason: "pod_deleted"},
	}}}
	page.closeLinkedGaps(nil, []BreakLogLine{{At: from.Add(time.Second), Pod: "api-1", PodUID: "pod", Container: "app"}}, now)
	if !page.Coverage.Tip || page.Coverage.State != "partial" || len(page.Coverage.Gaps) != 1 || page.Coverage.Gaps[0].Container != "sidecar" {
		t.Fatalf("unrelated gap erased: %+v", page.Coverage)
	}
}

func TestBreakLogLinkedCoverageKeepsUnknownScopeCaps(t *testing.T) {
	now := time.Now().UTC()
	from := now.Add(-time.Minute)
	for _, test := range []struct {
		name       string
		pods       int
		containers int
		podUID     string
	}{
		{name: "six_pods_five_selected", pods: 5, containers: 1},
		{name: "four_containers_three_selected", pods: 1, containers: 3, podUID: "pod-0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gap := breaklogs.Gap{From: from, To: now, PodUID: test.podUID, Reason: "budget"}
			page := BreakLogPage{Window: BreakLogWindow{From: from, To: now}, Coverage: breaklogs.Coverage{Gaps: []breaklogs.Gap{gap}}}
			lines := []BreakLogLine{}
			for _, pod := range breakLogFixture(now, test.pods, test.containers).Records[1:] {
				page.Pods = append(page.Pods, BreakLogPod{UID: pod.UID, Name: pod.Name})
				for _, container := range pod.Containers {
					lines = append(lines, BreakLogLine{At: from.Add(time.Second), Pod: pod.Name, PodUID: pod.UID, Container: container.Name})
				}
			}
			page.closeLinkedGaps([]breaklogs.Gap{{From: from, To: now}}, lines, now)
			if !page.Coverage.Tip || page.Coverage.Reason != "budget" || len(page.Coverage.Gaps) != 1 || page.Coverage.Gaps[0] != gap {
				t.Fatalf("selected rows closed unknown scope cap: %+v", page.Coverage)
			}
		})
	}
}

func TestBreakLogLinkedSiblingControllerCannotCloseDeletedPodGap(t *testing.T) {
	for _, test := range []struct {
		kind string
		pod  string
	}{
		{kind: "Deployment", pod: "api-canary-7c9b8d6f5-x2k9m"},
		{kind: "StatefulSet", pod: "api-canary-0"},
	} {
		t.Run(test.kind, func(t *testing.T) {
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
			index, err := service.indexes.ForScope(service.indexScope())
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
				records := []kubeindex.Record{}
				if kind == test.kind {
					records = append(records, kubeindex.Record{Kind: kind, APIVersion: "apps/v1", Namespace: "shop", Name: "api", UID: "root"}, kubeindex.Record{Kind: kind, APIVersion: "apps/v1", Namespace: "shop", Name: "api-canary", UID: "sibling"})
				}
				if kind == "Pod" {
					records = append(records, kubeindex.Record{Kind: "Pod", APIVersion: "v1", Namespace: "shop", Name: test.pod, UID: "sibling-pod", Owners: []kubeindex.OwnerRef{{Kind: test.kind, Name: "api-canary", UID: "sibling", Namespace: "shop", Controller: true, ControllerKnown: true}}})
				}
				if err := index.Ingest(kind, records, true, now); err != nil {
					t.Fatal(err)
				}
			}
			var observed breaklogs.LinkedQuery
			service.SetBreakLogSources(func(string) []breaklogs.Source {
				return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
					return gapLinkedReader{pod: test.pod, observed: &observed}, nil
				}}}
			})
			if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}); err != nil {
				t.Fatal(err)
			}
			page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(-time.Minute), To: now})
			if err != nil || observed.PodPrefix != "api" || !page.Coverage.Tip || page.Coverage.Reason != "pod_deleted" || len(page.Lines) != 0 || len(page.Pods) != 0 {
				t.Fatalf("sibling controller established root coverage: query=%+v coverage=%+v pods=%+v lines=%+v err=%v", observed, page.Coverage, page.Pods, page.Lines, err)
			}
		})
	}
}

func TestBreakLogFutureOnlyDoesNotReadLinkedSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		t.Error("pending-only coverage consulted linked sources")
		return nil
	})
	now := time.Now().UTC()
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil || page.Coverage.State != "pending" || page.Coverage.Tip || page.Coverage.Reason != "pending" || len(page.Coverage.Gaps) != 1 {
		t.Fatalf("future-only API claimed unavailable: %+v err=%v", page.Coverage, err)
	}
}

func TestBreakLogObservedCurrentWindowWithFutureTailSkipsLinkedRead(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/namespaces/shop/pods/api-0/log" {
			return
		}
		fixture.ServeHTTP(writer, request)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	defer service.StopCapture()
	now := time.Now().UTC()
	snapshot := breakLogFixture(now, 1, 1)
	snapshot.Records[1].Containers = []kubeindex.ContainerState{{Name: "app", Running: true, Ready: true, StateKnown: true}}
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Pod", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		records := []kubeindex.Record{}
		for _, record := range snapshot.Records {
			if record.Kind == kind {
				records = append(records, record)
			}
		}
		if err := index.Ingest(kind, records, true, now); err != nil {
			t.Fatal(err)
		}
	}
	builds := 0
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			builds++
			return gapLinkedReader{}, nil
		}}}
	})
	if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs"}}); err != nil {
		t.Fatal(err)
	}
	before := builds
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(-5 * time.Minute), To: now.Add(2 * time.Minute)})
	if err != nil || builds != before || page.Coverage.State != "pending" || page.Coverage.Tip || page.Coverage.Reason != "pending" || len(page.Coverage.Gaps) != 1 || page.Coverage.Gaps[0].Reason != "pending" {
		t.Fatalf("observed default window claimed unavailable: %+v err=%v", page.Coverage, err)
	}
}

func TestBreakLogRefreshFailureDoesNotClaimDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	now := time.Now().UTC()
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(-time.Minute), To: now})
	if err != nil || !page.Coverage.Tip || page.Coverage.Reason != "capture_late" {
		t.Fatalf("refresh failure claimed deletion: %+v err=%v", page.Coverage, err)
	}
	for _, gap := range page.Coverage.Gaps {
		if gap.Reason == "pod_deleted" {
			t.Fatal("refresh failure manufactured deletion")
		}
	}
}

type healthWriteCounter struct {
	storage.Provider
	writes int
}

func (provider *healthWriteCounter) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	provider.writes++
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func TestLogHealthCoalescesWritesButPersistsTransitionsAndFreshness(t *testing.T) {
	provider := &healthWriteCounter{Provider: storage.NewMemory()}
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(provider)
	service.SetBreakLogSources(func(string) []breaklogs.Source { return []breaklogs.Source{{Name: "logs"}} })
	for range 5 {
		if err := service.recordLogHealth(t.Context(), "logs", time.Now(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if provider.writes != 1 {
		t.Fatalf("unchanged health caused %d writes", provider.writes)
	}
	if err := service.recordLogHealth(t.Context(), "logs", time.Now(), errors.New("unauthorized")); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 2 {
		t.Fatal("health transition not persisted immediately")
	}
	_, key := service.logSettingsStore()
	if err := breaklogs.UpdateSettings(provider, key, func(state *breaklogs.SettingsState) error {
		state.Health["logs"] = breaklogs.Health{State: "failing", At: time.Now().Add(-2 * time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.recordLogHealth(t.Context(), "logs", time.Now(), errors.New("unauthorized")); err != nil {
		t.Fatal(err)
	}
	state, err := breaklogs.LoadSettings(provider, key)
	if err != nil || provider.writes != 4 || state.Health["logs"].State != "failing" || time.Since(state.Health["logs"].At) > time.Minute {
		t.Fatalf("health freshness missing: %+v writes=%d err=%v", state.Health, provider.writes, err)
	}
}
