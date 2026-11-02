package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

type acceptanceLinkedReader struct {
	read func(context.Context, breaklogs.LinkedQuery) (breaklogs.Result, error)
}

func (reader acceptanceLinkedReader) Preview(breaklogs.LinkedQuery) string { return "" }
func (reader acceptanceLinkedReader) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	return reader.read(ctx, query)
}

func acceptanceLinkedService(t *testing.T) (*Service, breaklogs.Manifest) {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	t.Cleanup(service.StopCapture)
	now := time.Now().UTC()
	at := now.Add(-10 * time.Second)
	manifest := breaklogs.Manifest{ID: "selected", OccurrenceID: "selected", Namespace: "deleted", RootUID: "deleted-root", BreakAt: at, From: at.Add(-time.Minute), To: at, CapturedAt: now,
		Pods: []breaklogs.ManifestPod{{Namespace: "deleted", RootUID: "deleted-root", Name: "api-0", UID: "old", CreatedAt: at.Add(-time.Hour), LastSeen: at, DeletedAt: at, Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: at.Add(-time.Hour)}}}}}
	if err := service.changeStore().CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
		t.Fatal(err)
	}
	return service, manifest
}

func TestLinkedPartialAnswerAndFailureKeepExactReadiness(t *testing.T) {
	for _, failure := range []string{"", "timeout", "unauthorized", "not_found", "invalid_mapping", "transport failure"} {
		t.Run(failure, func(t *testing.T) {
			service, manifest := acceptanceLinkedService(t)
			service.SetBreakLogSources(func(string) []breaklogs.Source {
				return []breaklogs.Source{{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
					return acceptanceLinkedReader{read: func(_ context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
						if failure != "" {
							return breaklogs.Result{}, errors.New(failure)
						}
						// A complete backend answer containing an older row is not
						// proof that its still-ingesting tail is complete.
						return breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: query.From.Add(time.Second), Pod: "api-0", Container: "app", Text: "older ingested row"}}}, nil
					}}, nil
				}}}
			})
			delay := 90
			query := breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, OccurrenceID: manifest.ID, From: manifest.From, To: manifest.To, Container: "app"}
			for _, seconds := range []int{delay, 0} {
				if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &seconds}}); err != nil {
					t.Fatal(err)
				}
				page, err := service.BreakLogs(t.Context(), query)
				if err != nil {
					t.Fatal(err)
				}
				if !page.Window.From.Equal(query.From) || !page.Window.To.Equal(query.To) {
					t.Fatal("readiness changed the pinned window", page.Window)
				}
				pending, visibleFailure, linkedSource := false, false, false
				reason := failure
				if reason == "transport failure" {
					reason = "source_error"
				}
				for _, gap := range page.Coverage.Gaps {
					if gap.Reason == "linked_pending" {
						pending = true
						if !gap.ReadyAt.Equal(query.To.Add(90 * time.Second)) {
							t.Fatal("readiness clock drifted", gap.ReadyAt)
						}
					}
					visibleFailure = visibleFailure || gap.Reason == reason
				}
				for _, source := range page.Sources {
					linkedSource = linkedSource || source.Kind == "linked" && source.Name == "logs"
				}
				if !linkedSource || pending != (seconds != 0) {
					t.Fatal("readiness/source hidden", seconds, page.Coverage, page.Sources)
				}
				if failure != "" && (!visibleFailure || page.Coverage.State == "complete") {
					t.Fatal("backend failure became successful coverage", page.Coverage)
				}
				if failure == "" && (len(page.Lines) != 1 || seconds != 0 && page.Coverage.State == "complete" || seconds == 0 && page.Coverage.State != "complete") {
					t.Fatal("older row incorrectly finalized pending tail or mature answer", seconds, page.Coverage, page.Lines)
				}
			}
		})
	}
}

func TestSelectedRepeatedOccurrenceKeepsNanosecondWindowAndRejectsGuess(t *testing.T) {
	service, first := acceptanceLinkedService(t)
	next := first
	next.ID, next.OccurrenceID = "next", "next"
	next.BreakAt = first.BreakAt.Add(time.Nanosecond)
	next.From, next.To = first.From.Add(time.Nanosecond), first.To.Add(time.Nanosecond)
	next.Pods = append([]breaklogs.ManifestPod(nil), first.Pods...)
	next.Pods[0].UID = "new"
	state := breaklogs.State{Manifests: []breaklogs.Manifest{first, next}}
	for _, item := range []breaklogs.Manifest{first, next} {
		t.Run(item.ID, func(t *testing.T) {
			query := breaklogs.Query{Namespace: item.Namespace, RootUID: item.RootUID, OccurrenceID: item.OccurrenceID, From: item.From, To: item.To, Container: "app"}
			resolved, err := service.resolveBreakLogSelection(t.Context(), query, state)
			pods := manifestQueryPods(state, resolved)
			if err != nil || !resolved.Center.Equal(item.BreakAt) || !resolved.From.Equal(item.From) || !resolved.To.Equal(item.To) || len(pods) != 1 || pods[0].UID != item.Pods[0].UID {
				t.Fatalf("cross-occurrence/window drift: query=%+v pods=%+v err=%v", resolved, pods, err)
			}
		})
	}
	for _, query := range []breaklogs.Query{
		{Namespace: first.Namespace, RootUID: first.RootUID, OccurrenceID: "guessed", Center: first.BreakAt, From: first.From, To: first.To},
		{Namespace: first.Namespace, RootUID: first.RootUID, OccurrenceID: first.ID, Center: next.BreakAt, From: first.From, To: first.To},
		{Namespace: first.Namespace, RootUID: first.RootUID, Center: first.BreakAt.Add(2 * time.Nanosecond), From: first.From, To: first.To},
	} {
		if _, err := service.resolveBreakLogSelection(t.Context(), query, state); !errors.Is(err, ErrInvalidArguments) {
			t.Fatal("unknown/mismatched occurrence guessed", query, err)
		}
	}
	ambiguous := next
	ambiguous.BreakAt = first.BreakAt
	state.Manifests = []breaklogs.Manifest{first, ambiguous}
	if _, err := service.resolveBreakLogSelection(t.Context(), breaklogs.Query{Namespace: first.Namespace, RootUID: first.RootUID, Center: first.BreakAt, From: first.From, To: first.To}, state); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("ambiguous center guessed", err)
	}
}

func TestLinkedModesDeduplicateRowsAndHonorTerminationBoundary(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	termination := manifest.To.Add(-30 * time.Second)
	pods := append([]breaklogs.ManifestPod(nil), manifest.Pods...)
	pods[0].Containers = []breaklogs.ManifestContainer{{Name: "app", Restarts: 1, PreviousStartedAt: manifest.From, TerminationAt: termination, StartedAt: termination.Add(time.Nanosecond)}}
	reader := acceptanceLinkedReader{read: func(_ context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
		if query.PodPrefix != "" || len(query.Pods) != 1 || query.Pods[0] != "api-0" || query.Container != "app" {
			t.Error("broadened linked query", query)
		}
		lines := []breaklogs.LinkedLine{
			{At: termination, Pod: "api-0", Container: "app", Text: "old"},
			{At: termination.Add(time.Nanosecond), Pod: "api-0", Container: "app", Text: "new"},
		}
		return breaklogs.Result{Lines: append(lines, lines...)}, nil
	}}
	source := breaklogs.Source{Name: "logs", Type: "loki", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) { return reader, nil }}
	for _, mode := range []string{"merged", "previous", "current"} {
		t.Run(mode, func(t *testing.T) {
			query := breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, From: manifest.From, To: manifest.To, Container: "app", Mode: mode}
			lines, _, err := service.readLinkedBreak(t.Context(), query, pods, breaklogs.CaptureSettings{}, source, false)
			want := 1
			if mode == "merged" {
				want = 2
			}
			if err != nil || len(lines) != want {
				t.Fatal("duplicate/mode output", lines, err)
			}
			for _, line := range lines {
				if mode != "merged" && line.Slot != mode || line.At.Equal(termination) && line.Slot != "previous" || line.At.After(termination) && line.Slot != "current" {
					t.Fatal("termination slot mismatch", line)
				}
			}
		})
	}
}

func TestLinkedPendingDefaultsOverridesAndSourceTypeMapping(t *testing.T) {
	service, manifest := acceptanceLinkedService(t)
	var built atomic.Int32
	var selectedType atomic.Value
	selectedType.Store("loki")
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		kind := selectedType.Load().(string)
		return []breaklogs.Source{{Name: "logs", Type: kind, Build: func(fields breaklogs.Fields) (breaklogs.LinkedReader, error) {
			built.Add(1)
			if fields.Namespace != "custom_ns" || fields.Pod != "custom_pod" || fields.Container != "custom_container" {
				t.Error("source-type mapping lost", kind, fields)
			}
			return acceptanceLinkedReader{read: func(context.Context, breaklogs.LinkedQuery) (breaklogs.Result, error) { return breaklogs.Result{}, nil }}, nil
		}}}
	})
	zero, override := 0, 90
	for _, kind := range []string{"loki", "elasticsearch", "signoz", "graylog", "splunk", "cloudwatchlogs"} {
		for _, delay := range []*int{nil, &zero, &override} {
			name := "default"
			if delay != nil {
				name = fmt.Sprint(*delay)
			}
			t.Run(kind+"/"+name, func(t *testing.T) {
				selectedType.Store(kind)
				settings := breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: delay, Fields: map[string]breaklogs.Fields{kind: {Namespace: "custom_ns", Pod: "custom_pod", Container: "custom_container"}}}
				if err := service.SaveAgentSettings(t.Context(), breaklogs.Settings{BreakLogs: settings}); err != nil {
					t.Fatal(err)
				}
				before := built.Load()
				page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, OccurrenceID: manifest.ID, From: manifest.From, To: manifest.To, Container: "app"})
				wantReason := "linked_pending"
				if delay == &zero {
					wantReason = "no_logs"
				}
				if err != nil || page.Coverage.Reason != wantReason || len(page.Lines) != 0 || built.Load() != before+1 || !page.Window.To.Equal(manifest.To) {
					t.Fatalf("source readiness: reason=%s want=%s built=%d err=%v", page.Coverage.Reason, wantReason, built.Load()-before, err)
				}
				for _, gap := range page.Coverage.Gaps {
					if gap.Reason == "linked_pending" && !gap.ReadyAt.Equal(manifest.To.Add(settings.IngestionDelay(kind))) {
						t.Fatal("ready time drift", gap)
					}
				}
			})
		}
	}
}

func TestLinkedProcessCacheIsolationCloningPromotionAndConcurrentBound(t *testing.T) {
	clearLinkedCache()
	t.Cleanup(clearLinkedCache)
	now := time.Now().UTC()
	input := breaklogs.Result{Lines: []breaklogs.LinkedLine{{Text: "safe"}}, Complete: []breaklogs.Gap{{Reason: "safe"}}}
	cacheLinkedResult("retained", input, now, now)
	input.Lines[0].Text, input.Complete[0].Reason = "mutated", "mutated"
	output, found := cachedLinkedResult("retained", now)
	if !found || output.Lines[0].Text != "safe" || output.Complete[0].Reason != "safe" {
		t.Fatal("cache aliases caller input", output)
	}
	output.Lines[0].Text, output.Complete[0].Reason = "changed", "changed"
	output, found = cachedLinkedResult("retained", now)
	if !found || output.Lines[0].Text != "safe" || output.Complete[0].Reason != "safe" {
		t.Fatal("cache aliases caller output", output)
	}
	for i := 0; i < 255; i++ {
		cacheLinkedResult(fmt.Sprint(i), input, now, now)
	}
	cachedLinkedResult("retained", now)
	cacheLinkedResult("new", input, now, now)
	if _, found := cachedLinkedResult("retained", now); !found {
		t.Fatal("LRU hit did not promote entry")
	}
	if _, found := cachedLinkedResult("0", now); found {
		t.Fatal("LRU evicted wrong entry")
	}
	var running sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		running.Add(1)
		go func(worker int) {
			defer running.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("%d/%d", worker, i)
				cacheLinkedResult(key, input, now, now)
				cachedLinkedResult(key, now)
			}
		}(worker)
	}
	running.Wait()
	linkedBreakCache.Lock()
	defer linkedBreakCache.Unlock()
	if len(linkedBreakCache.items) != 256 || linkedBreakCache.order.Len() != 256 {
		t.Fatal("process-wide concurrent cache exceeded cap", len(linkedBreakCache.items), linkedBreakCache.order.Len())
	}
}

func TestVerifiedManifestInventoryRejectsIncompleteOwnerProof(t *testing.T) {
	now := time.Now().UTC()
	root := kubeindex.Record{Kind: "Deployment", Namespace: "shop", UID: "root", Name: "api"}
	pod := kubeindex.Record{Kind: "Pod", Namespace: "shop", UID: "pod", Name: "api-0", CreatedAt: now, Owners: []kubeindex.OwnerRef{{UID: "root", Controller: true}}}
	for _, test := range []struct {
		name    string
		root    kubeindex.Record
		partial bool
	}{
		{"cross namespace", kubeindex.Record{Kind: "Deployment", Namespace: "other", UID: "root"}, false},
		{"partial owner kind", root, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "Deployment": {State: "ready", Partial: test.partial}}, Records: []kubeindex.Record{test.root, pod}}
			if pods := verifiedBreakPods(snapshot); len(pods) != 0 {
				t.Fatal("unverified ownership persisted", pods)
			}
		})
	}
}

func TestLinkedReadRevocationDuringBackendResponseCannotPublishCache(t *testing.T) {
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	var denied atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denied.Load() && r.URL.Path == "/api/v1/namespaces/deleted/pods" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	defer service.StopCapture()
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	now := time.Now().UTC()
	pods := []breaklogs.ManifestPod{{Namespace: "deleted", RootUID: "root", Name: "api-0", UID: "old", CreatedAt: now.Add(-time.Hour), LastSeen: now, DeletedAt: now, Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}}
	source := breaklogs.Source{Name: "logs", Type: "loki", CacheKey: "stable", Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
		return acceptanceLinkedReader{read: func(context.Context, breaklogs.LinkedQuery) (breaklogs.Result, error) {
			denied.Store(true)
			return breaklogs.Result{Lines: []breaklogs.LinkedLine{{At: now.Add(-time.Second), Pod: "api-0", Container: "app", Text: "safe"}}}, nil
		}}, nil
	}}
	clearLinkedCache()
	t.Cleanup(clearLinkedCache)
	lines, _, err := service.readLinkedBreak(t.Context(), breaklogs.Query{Namespace: "deleted", RootUID: "root", From: now.Add(-time.Minute), To: now, Container: "app"}, pods, breaklogs.CaptureSettings{}, source, true)
	if !errors.Is(err, ErrForbidden) || len(lines) != 0 {
		t.Fatal("late namespace revocation published response", lines, err)
	}
	linkedBreakCache.Lock()
	defer linkedBreakCache.Unlock()
	if len(linkedBreakCache.items) != 0 {
		t.Fatal("late revoked read retained cached bytes")
	}
}
