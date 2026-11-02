package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func alertTestService(t *testing.T) *Service {
	t.Helper()
	s := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "org", ClusterID: "cluster"})
	s.SetChangeStorage(storage.NewMemory())
	s.SetScrubber(breakLogScrubber{})
	return s
}

func issueAlertCaptureService(t *testing.T) (*Service, <-chan IssueAlert) {
	t.Helper()
	s := alertTestService(t)
	t.Cleanup(s.StopCapture)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "warning", "actor"); err != nil {
		t.Fatal(err)
	}
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	close(done) // This fixture drives cycles directly without a polling goroutine.
	s.changes.captures[s.indexScope()] = &captureLoop{
		ctx: ctx, cancel: cancel, done: done, owner: "owner", owned: true,
		status: CaptureStatus{Mode: "continuous"},
		next:   map[string]time.Time{}, success: map[string]time.Time{}, gapFrom: map[string]time.Time{},
	}
	alerts := make(chan IssueAlert, 10)
	s.SetIssueAlertSink(func(_ context.Context, alert IssueAlert) (Outcome, error) {
		alerts <- alert
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	return s, alerts
}

func runIssueAlertCaptureCycle(t *testing.T, s *Service, snapshot kubeindex.Snapshot) {
	t.Helper()
	if err := s.captureCycle(s.changeStore(), nil, snapshot, nil, time.Now().UTC(), "owner"); err != nil {
		t.Fatal(err)
	}
	s.changes.mu.Lock()
	alertDone := s.changes.issueAlerts[s.indexScope()].done
	logDone := s.changes.logDone[s.indexScope()]
	s.changes.mu.Unlock()
	for _, done := range []<-chan struct{}{alertDone, logDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(35 * time.Second):
			t.Fatal("capture worker did not finish")
		}
	}
}

func TestIssueAlertsWaitForIndexedOwnerChain(t *testing.T) {
	for _, attributed := range []bool{false, true} {
		t.Run(fmt.Sprintf("attributed=%t", attributed), func(t *testing.T) {
			s, alerts := issueAlertCaptureService(t)
			snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{
				"Pod": {State: "ready"}, "ReplicaSet": {State: "ready"}, "Deployment": {State: "ready"},
			}}
			// Establish an owned absence baseline for unattributed failures.
			runIssueAlertCaptureCycle(t, s, snapshot)
			for i := range 2 {
				container := kubeindex.ContainerState{Name: "app", WaitingReason: "CrashLoopBackOff"}
				if attributed {
					container.TerminatedAt, container.TerminatedReason, container.ExitCode = time.Now().UTC(), "Error", 1
				}
				snapshot.Records = append(snapshot.Records, kubeindex.Record{
					Kind: "Pod", UID: fmt.Sprintf("pod-%d", i), Name: fmt.Sprintf("checkout-%d", i), Namespace: "shop",
					Owners:     []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "replica", Name: "checkout-replica", Controller: true, ControllerKnown: true}},
					Containers: []kubeindex.ContainerState{container},
				})
			}
			assertNoAlert := func() {
				t.Helper()
				runIssueAlertCaptureCycle(t, s, snapshot)
				select {
				case alert := <-alerts:
					t.Fatalf("unresolved owner chain emitted an alert: %+v", alert)
				default:
				}
			}
			childAt := time.Now().UTC().Add(-time.Minute)
			snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "ready", ObservedAt: childAt, CompletedAt: childAt.Add(time.Second)}
			for i := range snapshot.Records {
				snapshot.Records[i].OwnerObservedAt = childAt.Add(time.Second)
			}
			// A cached ready kind, including one whose LIST overlaps the Pod
			// LIST, cannot establish absence for newly observed children.
			for _, ownerAt := range []time.Time{childAt.Add(-time.Second), childAt.Add(time.Millisecond), childAt.Add(time.Second)} {
				snapshot.Kinds["ReplicaSet"] = kubeindex.KindStatus{State: "ready", ObservedAt: ownerAt}
				assertNoAlert()
			}
			for _, state := range []kubeindex.KindStatus{{}, {State: "warming"}, {State: "ready", Partial: true}, {State: "error", Error: "unavailable"}} {
				snapshot.Kinds["ReplicaSet"] = state
				assertNoAlert()
			}
			snapshot.Records = append(snapshot.Records, kubeindex.Record{
				Kind: "ReplicaSet", UID: "replica", Name: "checkout-replica", Namespace: "shop", Replicas: 4, OwnerObservedAt: childAt.Add(3 * time.Second),
				Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "deployment", Name: "checkout", Controller: true, ControllerKnown: true}},
			})
			snapshot.Kinds["ReplicaSet"] = kubeindex.KindStatus{State: "ready", ObservedAt: childAt.Add(2 * time.Second), CompletedAt: childAt.Add(3 * time.Second)}
			snapshot.Kinds["Deployment"] = kubeindex.KindStatus{State: "ready", ObservedAt: childAt.Add(2 * time.Second)}
			assertNoAlert() // Fresher than Pods, but older than the ReplicaSet observation.
			snapshot.Kinds["Deployment"] = kubeindex.KindStatus{State: "warming", Partial: true}
			assertNoAlert()
			snapshot.Records = append(snapshot.Records, kubeindex.Record{
				Kind: "Deployment", UID: "deployment", Name: "checkout", Namespace: "shop", Replicas: 4,
			})
			snapshot.Kinds["Deployment"] = kubeindex.KindStatus{State: "ready"}
			runIssueAlertCaptureCycle(t, s, snapshot)
			select {
			case alert := <-alerts:
				if alert.RootUID != "deployment" || alert.WorkloadKind != "Deployment" || alert.Workload != "checkout" || alert.Count != 2 || alert.Of != 4 {
					t.Fatalf("incorrect resolved alert: %+v", alert)
				}
			default:
				t.Fatal("resolved Deployment never emitted")
			}
			if len(alerts) != 0 {
				t.Fatal("unexpected additional alerts")
			}
		})
	}
}

func TestIssueAlertsPreserveLegitimateRoots(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rootKind   string
		rootStatus kubeindex.KindStatus
		indexRoot  bool
	}{
		{name: "standalone pod", rootKind: "Pod", indexRoot: true},
		{name: "standalone replicaset while warming", rootKind: "ReplicaSet", rootStatus: kubeindex.KindStatus{State: "warming", Partial: true}, indexRoot: true},
		{name: "known deployment while warming", rootKind: "Deployment", rootStatus: kubeindex.KindStatus{State: "warming", Partial: true}, indexRoot: true},
		{name: "forbidden owner", rootKind: "ReplicaSet", rootStatus: kubeindex.KindStatus{State: "error", Error: "forbidden", Partial: true}},
		{name: "authoritative missing owner", rootKind: "ReplicaSet", rootStatus: kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC().Add(-time.Second)}},
		{name: "unindexed owner kind", rootKind: "CustomController"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, alerts := issueAlertCaptureService(t)
			snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: time.Now().UTC().Add(-time.Minute), CompletedAt: time.Now().UTC().Add(-30 * time.Second)}}}
			runIssueAlertCaptureCycle(t, s, snapshot)
			pod := kubeindex.Record{Kind: "Pod", UID: "pod", Name: "pod", Namespace: "shop", OwnerObservedAt: time.Now().UTC().Add(-30 * time.Second), Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}}}
			rootUID := pod.UID
			if tc.rootKind != "Pod" {
				rootUID = "root"
				pod.Owners = []kubeindex.OwnerRef{{Kind: tc.rootKind, UID: rootUID, Name: "root", Controller: true, ControllerKnown: true}}
				snapshot.Kinds[tc.rootKind] = tc.rootStatus
				if tc.indexRoot {
					snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: tc.rootKind, UID: rootUID, Name: "root", Namespace: "shop", Replicas: 1})
				}
			}
			snapshot.Records = append(snapshot.Records, pod)
			runIssueAlertCaptureCycle(t, s, snapshot)
			select {
			case alert := <-alerts:
				if alert.RootUID != rootUID || alert.WorkloadKind != tc.rootKind || alert.Count != 1 {
					t.Fatalf("incorrect legitimate root: %+v", alert)
				}
			default:
				t.Fatal("legitimate root was suppressed")
			}
		})
	}
}

func TestIssueAlertsDeferredRootsDoNotEstablishAbsence(t *testing.T) {
	for _, state := range []kubeindex.KindStatus{{State: "warming"}, {State: "ready", ObservedAt: time.Now().UTC().Add(-time.Hour)}} {
		t.Run(state.State, func(t *testing.T) {
			s, alerts := issueAlertCaptureService(t)
			pod := kubeindex.Record{
				Kind: "Pod", UID: "pod", Name: "pod", Namespace: "shop", OwnerObservedAt: time.Now().UTC(),
				Owners:     []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "root", Name: "root", Controller: true, ControllerKnown: true}},
				Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}},
			}
			snapshot := kubeindex.Snapshot{
				Records: []kubeindex.Record{pod},
				Kinds:   map[string]kubeindex.KindStatus{"Pod": {State: "ready", CompletedAt: time.Now().UTC()}, "ReplicaSet": state},
			}
			runIssueAlertCaptureCycle(t, s, snapshot)
			snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: "ReplicaSet", UID: "root", Name: "root", Namespace: "shop", Replicas: 1})
			snapshot.Kinds["ReplicaSet"] = kubeindex.KindStatus{State: "ready"}
			runIssueAlertCaptureCycle(t, s, snapshot)
			if len(alerts) != 0 {
				t.Fatal("incomplete owner chain established absence and backfilled an old failure")
			}
			// Only a complete healthy cycle can establish absence for a later break.
			snapshot.Records[0].Containers = nil
			runIssueAlertCaptureCycle(t, s, snapshot)
			snapshot.Records[0] = pod
			runIssueAlertCaptureCycle(t, s, snapshot)
			select {
			case alert := <-alerts:
				if alert.RootUID != "root" || alert.WorkloadKind != "ReplicaSet" {
					t.Fatalf("incorrect new failure alert: %+v", alert)
				}
			default:
				t.Fatal("new failure after complete healthy baseline was suppressed")
			}
		})
	}
}

func TestIssueAlertsMissingRootRequiresKnownChildFreshness(t *testing.T) {
	s, alerts := issueAlertCaptureService(t)
	snapshot := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "ReplicaSet": {State: "ready", ObservedAt: time.Now().UTC()}}}
	runIssueAlertCaptureCycle(t, s, snapshot)
	snapshot.Records = []kubeindex.Record{{
		Kind: "Pod", UID: "pod", Name: "pod", Namespace: "shop",
		Owners:     []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "root", Name: "root", Controller: true, ControllerKnown: true}},
		Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}},
	}}
	runIssueAlertCaptureCycle(t, s, snapshot)
	if len(alerts) != 0 {
		t.Fatal("unknown child observation treated as proof of owner absence")
	}
}

func TestIssueAlertsOverlappingRelistsEventuallyAdmitMissingOwnerAndAllowHealing(t *testing.T) {
	s, alerts := issueAlertCaptureService(t)
	runIssueAlertCaptureCycle(t, s, kubeindex.Snapshot{})
	index := kubeindex.New(10)
	missing := kubeindex.Record{
		APIVersion: "v1", Kind: "Pod", UID: "orphan", Name: "orphan", Namespace: "shop",
		Owners:     []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "gone", Name: "gone", Controller: true, ControllerKnown: true}},
		Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}},
	}
	other := kubeindex.Record{APIVersion: "v1", Kind: "Pod", UID: "other", Name: "other", Namespace: "shop", Containers: missing.Containers}
	var firstMissing time.Time
	var boundary time.Time
	for cycle := range 4 {
		// Deterministically overlap the LISTs: the owner observation starts
		// before this Pod load publishes its evidence, on every cycle.
		ownerStarted := time.Now().UTC()
		if cycle == 1 {
			ownerStarted = boundary // Equal is not authoritative, even after a re-list.
		}
		if cycle > 0 {
			other.Containers = nil
		}
		if err := index.Ingest("Pod", []kubeindex.Record{missing, other}, true, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := index.Ingest("ReplicaSet", nil, true, ownerStarted); err != nil {
			t.Fatal(err)
		}
		snapshot := index.Snapshot()
		if cycle == 0 {
			for _, record := range snapshot.Records {
				if record.UID == "orphan" {
					boundary = record.OwnerObservedAt
				}
			}
		}
		if !snapshot.Kinds["ReplicaSet"].ObservedAt.Before(snapshot.Kinds["Pod"].CompletedAt) {
			t.Fatal("fixture did not overlap LIST observations")
		}
		runIssueAlertCaptureCycle(t, s, snapshot)
		var gotMissing, gotOther int
		for len(alerts) > 0 {
			alert := <-alerts
			switch alert.RootUID {
			case "gone":
				gotMissing++
				if firstMissing.IsZero() {
					firstMissing = alert.BrokeAt
				} else if !firstMissing.Equal(alert.BrokeAt) {
					t.Fatal("re-list created a new missing-owner occurrence")
				}
			case "other":
				gotOther++
			default:
				t.Fatalf("unexpected alert: %+v", alert)
			}
		}
		if cycle == 0 {
			if gotMissing != 0 || gotOther != 1 {
				t.Fatalf("premature fallback or suppressed standalone root: missing=%d other=%d", gotMissing, gotOther)
			}
		} else {
			wantMissing, wantSent := 1, 2
			if cycle == 1 {
				wantMissing, wantSent = 0, 1
			}
			if gotMissing != wantMissing || gotOther != 0 {
				t.Errorf("cycle %d never admitted authoritative missing owner: missing=%d other=%d", cycle, gotMissing, gotOther)
			}
			s.changes.mu.Lock()
			window := s.changes.issueAlerts[s.indexScope()]
			stillPresent := window.baseline.present["shop\x00other\x00pod.crash_loop"]
			_, stillRemembered := s.changes.firstSeen[s.indexScope()]["shop\x00other"]
			sent := window.sent
			s.changes.mu.Unlock()
			if stillPresent || stillRemembered {
				t.Errorf("cycle %d unresolved owner stalled unrelated healing", cycle)
			}
			// Re-observations refresh shared episodes, but only one admission
			// for each of the two occurrences consumes the storm budget.
			if sent != wantSent {
				t.Errorf("cycle %d admission count=%d, want %d", cycle, sent, wantSent)
			}
		}
	}
}

func TestIssueAlertsMissingOwnerFirstReleaseDoesNotBackfill(t *testing.T) {
	s, alerts := issueAlertCaptureService(t)
	index := kubeindex.New(10)
	pod := kubeindex.Record{
		APIVersion: "v1", Kind: "Pod", UID: "pod", Name: "pod", Namespace: "shop",
		Owners:     []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "gone", Name: "gone", Controller: true, ControllerKnown: true}},
		Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff"}},
	}
	for range 3 {
		started := time.Now().UTC()
		if err := index.Ingest("Pod", []kubeindex.Record{pod}, true, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := index.Ingest("ReplicaSet", nil, true, started); err != nil {
			t.Fatal(err)
		}
		runIssueAlertCaptureCycle(t, s, index.Snapshot())
		if len(alerts) != 0 {
			t.Fatal("first authoritative release backfilled an existing unattributed failure")
		}
	}
	healthy := pod
	healthy.Containers = nil
	if err := index.Ingest("Pod", []kubeindex.Record{healthy}, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	runIssueAlertCaptureCycle(t, s, index.Snapshot())
	if err := index.Ingest("Pod", []kubeindex.Record{pod}, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	runIssueAlertCaptureCycle(t, s, index.Snapshot())
	if len(alerts) != 1 {
		t.Fatalf("new failure after authoritative healthy baseline emitted %d alerts, want 1", len(alerts))
	}
}

func TestIssueAlertSettingsDefaultValidationClobberAndEpoch(t *testing.T) {
	s := alertTestService(t)
	ctx := t.Context()
	got, err := s.IssueAlertSettings(ctx)
	if err != nil || got.Enabled || got.Level != "critical" || !got.Available {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.SaveIssueAlertSettings(ctx, true, "bad", "actor"); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal(err)
	}
	if err := s.SaveIssueAlertSettings(ctx, true, "warning", "actor"); err != nil {
		t.Fatal(err)
	}
	first, _ := s.IssueAlertSettings(ctx)
	if first.EnabledAt.IsZero() {
		t.Fatal(first)
	}
	if err := s.SaveAgentSettings(ctx, breaklogs.Settings{Learning: breaklogs.Learning{Logs: true}, Alerts: breaklogs.Alerts{Enabled: false, Level: "critical"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIssueAlertSettings(ctx, true, "critical", "second"); err != nil {
		t.Fatal(err)
	}
	got, err = s.IssueAlertSettings(ctx)
	if err != nil || !got.Enabled || got.Level != "critical" || !got.EnabledAt.Equal(first.EnabledAt) {
		t.Fatalf("%+v %v", got, err)
	}
	provider, key := s.logSettingsStore()
	state, _ := breaklogs.LoadSettings(provider, key)
	if !state.Settings.Learning.Logs || state.Settings.Alerts.UpdatedBy != "second" {
		t.Fatal(state)
	}
	if err := s.SetCaptureMode("demand"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.IssueAlertSettings(ctx)
	if got.Available || got.Reason != "demand_capture" {
		t.Fatal(got)
	}
	if err := s.SaveIssueAlertSettings(ctx, true, "critical", "actor"); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal(err)
	}
}

func TestIssueAlertEvaluatorFiltersStormLeaseAndEvidence(t *testing.T) {
	s := alertTestService(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.SaveIssueAlertSettings(ctx, true, "warning", "actor"); err != nil {
		t.Fatal(err)
	}
	settings, _ := s.IssueAlertSettings(ctx)
	now = settings.EnabledAt.Add(time.Second)
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	var alerts []IssueAlert
	s.SetIssueAlertSink(func(_ context.Context, a IssueAlert) (Outcome, error) {
		alerts = append(alerts, a)
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	items := []kubebreaks.Break{}
	for i := range 12 {
		items = append(items, kubebreaks.Break{Root: kref.Ref{UID: fmt.Sprint(i), Name: fmt.Sprintf("api-%d", i), Kind: "Deployment", Namespace: "shop"}, Rule: "pod.crash_loop", Severity: "warning", BrokeAt: now, Count: 1, Of: 1, Evidence: kubebreaks.Evidence{Restarts: 12, Warnings: 9, Terminations: []kubebreaks.Termination{{ExitCode: 137, Reason: "OOMKilled", Count: 4}}}})
	}
	old := items[0]
	old.Root.UID = "old"
	old.BrokeAt = settings.EnabledAt.Add(-time.Second)
	items = append(items, old)
	window := &issueAlertWindow{}
	if err := s.emitIssueAlerts(ctx, items, "owner", now, window); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 11 || alerts[10].RollupCount != 2 || len(alerts[0].Evidence) != 3 || alerts[0].OrgID != "org" {
		t.Fatalf("%+v", alerts)
	}
	alerts = nil
	if err := s.emitIssueAlerts(ctx, items, "owner", now.Add(time.Minute), window); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 10 {
		t.Fatalf("admitted occurrences must keep episodes live; got %d", len(alerts))
	}
	alerts = nil
	if err := s.emitIssueAlerts(ctx, items, "owner", now.Add(2*time.Minute), &issueAlertWindow{}); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 10 {
		t.Fatalf("durable storm checkpoint lost on worker replacement: %d", len(alerts))
	}
	alerts = nil
	if err := s.emitIssueAlerts(ctx, items, "not-owner", now, window); err == nil {
		t.Fatal("lost lease accepted")
	}
	if len(alerts) != 0 {
		t.Fatal(alerts)
	}
	if err := s.SaveIssueAlertSettings(ctx, false, "critical", "actor"); err != nil {
		t.Fatal(err)
	}
	if err := s.emitIssueAlerts(ctx, items, "owner", now, window); err != nil || len(alerts) != 0 {
		t.Fatalf("%v %+v", err, alerts)
	}
}

func TestIssueAlertEvidenceReasonRejectsPayloadText(t *testing.T) {
	s := alertTestService(t)
	b := kubebreaks.Break{Rule: "pod.failed", Evidence: kubebreaks.Evidence{Terminations: []kubebreaks.Termination{{Reason: "CANARY raw termination message secret=value", ExitCode: 1, Count: 2}}, Waiting: []kubebreaks.WaitingCount{{Reason: "ImagePullBackOff", Count: 3}}}}
	a := s.issueAlert(b)
	if len(a.Evidence) != 2 || a.Evidence[0] != "Exit 1 Unknown 2 times" || a.Evidence[1] != "ImagePullBackOff 3 times" {
		t.Fatal(a.Evidence)
	}

}

func TestIssueAlertSinkErrorRetryAndScopedBridge(t *testing.T) {
	s := alertTestService(t)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "critical", "actor"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var hydrated []bool
	wantErr := errors.New("delivery failed")
	s.SetIssueAlertSink(func(ctx context.Context, a IssueAlert) (Outcome, error) {
		calls++
		hydrated = append(hydrated, a.Changes != nil)
		if calls == 1 {
			return Outcome{}, wantErr
		}
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	scoped := s.Scoped(Scope{OrgID: "other", ClusterID: "cluster"})
	if scoped.issueAlertSink == nil {
		t.Fatal("scoped service lost sink")
	}
	items := []kubebreaks.Break{{Root: kref.Ref{UID: "root", Kind: "Deployment", Name: "api"}, Rule: "pod.failed", Severity: "critical", BrokeAt: now}}
	window := &issueAlertWindow{}
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now, window); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now.Add(time.Second), window); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || window.sent != 1 {
		t.Fatalf("calls %d budget %d", calls, window.sent)
	}
	if len(hydrated) != 2 || !hydrated[0] || !hydrated[1] {
		t.Fatal("failed-delivery retry lost history enrichment", hydrated)
	}
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now.Add(2*time.Second), window); err != nil {
		t.Fatal(err)
	}
	if len(hydrated) != 3 || hydrated[2] {
		t.Fatal("coalesced occurrence repeated enrichment", hydrated)
	}
}

func TestIssueAlertOwnedBaselineRejectsBackfillAndPreservesNewFallbackBreaks(t *testing.T) {
	now := time.Now().UTC()
	old := kubebreaks.Break{Root: kref.Ref{Namespace: "shop", UID: "old"}, Rule: "pod.unschedulable", Source: "first_seen", Severity: "warning", BrokeAt: now.Add(-time.Hour)}
	var baseline issueAlertBaseline
	if got := baseline.observe([]kubebreaks.Break{old}, "owner-a", now, false); len(got) != 0 {
		t.Fatal("initial owned snapshot backfilled", got)
	}
	if got := baseline.observe(nil, "owner-a", now.Add(time.Second), true); len(got) != 0 {
		t.Fatal(got)
	}
	if got := baseline.observe([]kubebreaks.Break{old}, "owner-a", now.Add(2*time.Second), false); len(got) != 0 {
		t.Fatal("partial absence admitted old break", got)
	}
	for _, owner := range []string{"owner-b", "owner-a"} {
		if got := baseline.observe([]kubebreaks.Break{old}, owner, now.Add(3*time.Second), false); len(got) != 0 {
			t.Fatal("owner reacquisition backfilled", got)
		}
	}
	var restarted issueAlertBaseline
	if got := restarted.observe([]kubebreaks.Break{old}, "new-process", now.Add(4*time.Second), false); len(got) != 0 {
		t.Fatal("restart backfilled", got)
	}
	baseline.observe(nil, "owner-a", now.Add(5*time.Second), false)
	got := baseline.observe([]kubebreaks.Break{old}, "owner-a", now.Add(6*time.Second), true)
	if len(got) != 1 || !got[0].BrokeAt.Equal(now.Add(6*time.Second)) {
		t.Fatal("genuine new unattributed break lost", got)
	}
	repeated := baseline.observe([]kubebreaks.Break{old}, "owner-a", now.Add(7*time.Second), false)
	if len(repeated) != 1 || !repeated[0].BrokeAt.Equal(got[0].BrokeAt) || !old.BrokeAt.Equal(now.Add(-time.Hour)) {
		t.Fatal("fallback episode timestamp or Issues input changed")
	}
	var partialStart issueAlertBaseline
	partialStart.observe(nil, "owner", now, true)
	if got := partialStart.observe([]kubebreaks.Break{old}, "owner", now.Add(time.Second), true); len(got) != 0 {
		t.Fatal("partial initial snapshot established absence")
	}
}

func TestIssueAlertBaselineNewBeforeEnabledDoesNotBackfill(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			s := alertTestService(t)
			var baseline issueAlertBaseline
			before := time.Now().UTC().Add(-time.Minute)
			baseline.observe(nil, "owner", before, false)
			b := kubebreaks.Break{Root: kref.Ref{UID: "root"}, Rule: "pod.unschedulable", Source: "first_seen", Severity: "warning", BrokeAt: before}
			if deferred {
				baseline.observe(nil, "owner", before.Add(time.Second), false, b)
			} else {
				baseline.observe([]kubebreaks.Break{b}, "owner", before.Add(time.Second), false)
			}
			if err := s.SaveIssueAlertSettings(t.Context(), true, "warning", "actor"); err != nil {
				t.Fatal(err)
			}
			if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.SetIssueAlertSink(func(context.Context, IssueAlert) (Outcome, error) { calls++; return Outcome{}, nil })
			filtered := baseline.observe([]kubebreaks.Break{b}, "owner", time.Now().UTC(), false)
			if err := s.emitIssueAlerts(t.Context(), filtered, "owner", time.Now().UTC(), &issueAlertWindow{}); err != nil || calls != 0 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			after := time.Now().UTC().Add(time.Second)
			baseline.observe(nil, "owner", after, false)
			filtered = baseline.observe([]kubebreaks.Break{b}, "owner", after.Add(time.Second), true)
			if err := s.emitIssueAlerts(t.Context(), filtered, "owner", after.Add(time.Second), &issueAlertWindow{}); err != nil || calls != 1 {
				t.Fatalf("genuine new fallback issue was not emitted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestIssueAlertStormRollupPreservesCriticalSeverityAndEscalation(t *testing.T) {
	s := alertTestService(t)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "warning", "actor"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	items := []kubebreaks.Break{}
	for i := range 11 {
		items = append(items, kubebreaks.Break{Root: kref.Ref{UID: fmt.Sprint(i)}, Rule: "pod.failed", Severity: "warning", BrokeAt: now})
	}
	var rollups []IssueAlert
	individuals := 0
	s.SetIssueAlertSink(func(_ context.Context, a IssueAlert) (Outcome, error) {
		if a.RollupCount > 0 {
			rollups = append(rollups, a)
		} else {
			individuals++
		}
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	window := &issueAlertWindow{}
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now, window); err != nil {
		t.Fatal(err)
	}
	if individuals != 10 || len(rollups) != 1 || rollups[0].Severity != "warning" {
		t.Fatal(individuals, rollups)
	}
	// The already-suppressed issue escalating must elevate the same bounded rollup.
	items[10].Severity = "critical"
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now.Add(time.Second), window); err != nil {
		t.Fatal(err)
	}
	if len(rollups) != 2 || rollups[1].Severity != "critical" || !rollups[0].BrokeAt.Equal(rollups[1].BrokeAt) {
		t.Fatal(rollups)
	}
	for i := 11; i < 50; i++ {
		items = append(items, kubebreaks.Break{Root: kref.Ref{UID: fmt.Sprint(i)}, Rule: "pod.failed", Severity: "critical", BrokeAt: now})
	}
	before := individuals
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now.Add(2*time.Second), &issueAlertWindow{}); err != nil {
		t.Fatal(err)
	}
	if individuals-before != 10 || len(rollups) != 3 || rollups[2].Severity != "critical" {
		t.Fatal("critical issues bypassed storm bound", individuals, rollups)
	}
}

type issueAlertHistoryProvider struct {
	storage.Provider
	reads   int
	failAt  int
	failure error
}

func (p *issueAlertHistoryProvider) ReadBlob(key string) ([]byte, error) {
	if strings.HasPrefix(key, "kubernetes/changes/") {
		p.reads++
		if p.reads == p.failAt {
			return nil, p.failure
		}
	}
	return p.Provider.ReadBlob(key)
}
func (p *issueAlertHistoryProvider) CompareAndSwapBlob(key string, old, replacement []byte) (bool, error) {
	return p.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, replacement)
}

func TestIssueAlertEnrichmentSharedOnceAndHistoryFailureDegradesOnlyOptionalContent(t *testing.T) {
	s := alertTestService(t)
	provider := &issueAlertHistoryProvider{Provider: storage.NewMemory()}
	s.SetChangeStorage(provider)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "warning", "actor"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	items := []kubebreaks.Break{}
	for i := range 12 {
		items = append(items, kubebreaks.Break{Root: kref.Ref{UID: fmt.Sprint(i)}, Rule: "pod.failed", Severity: "warning", BrokeAt: now})
	}
	provider.reads = 0
	prepared, err := s.prepareIssueAlertEnrichment(t.Context(), items[:10], now)
	if err != nil || provider.reads != 1 {
		t.Fatalf("history reads=%d err=%v", provider.reads, err)
	}
	for _, b := range items {
		a := s.issueAlert(b)
		prepared.enrich(&a, b)
	}
	if provider.reads != 1 {
		t.Fatal("per-break history rereads", provider.reads)
	}
	provider.reads, provider.failAt, provider.failure = 0, 3, errors.New("CANARY sensitive upstream detail")
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	var alerts []IssueAlert
	s.SetIssueAlertSink(func(_ context.Context, a IssueAlert) (Outcome, error) {
		alerts = append(alerts, a)
		return Outcome{NotificationOutcome: "sent"}, nil
	})
	if err := s.emitIssueAlerts(t.Context(), items, "owner", now, &issueAlertWindow{}); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 11 || !alerts[0].HistoryIncomplete || alerts[10].RollupCount != 2 {
		t.Fatal(alerts)
	}
	if !strings.Contains(logged.String(), "history incomplete") || strings.Contains(logged.String(), "CANARY") {
		t.Fatal(logged.String())
	}
	// Losing the lease is not an optional history failure.
	alerts = nil
	if err := s.emitIssueAlerts(t.Context(), items, "wrong-owner", now, &issueAlertWindow{}); err == nil || len(alerts) != 0 {
		t.Fatal("lease failure swallowed")
	}
}

func TestIssueAlertWorkerBoundAndCancellation(t *testing.T) {
	s := alertTestService(t)
	if err := s.SaveIssueAlertSettings(t.Context(), true, "critical", "actor"); err != nil {
		t.Fatal(err)
	}
	if err := s.changeStore().ClaimLease("owner", time.Now().UTC(), time.Minute); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	started := make(chan context.Context, 1)
	finished := make(chan struct{})
	var calls atomic.Int32
	s.SetIssueAlertSink(func(ctx context.Context, _ IssueAlert) (Outcome, error) {
		calls.Add(1)
		started <- ctx
		<-ctx.Done()
		close(finished)
		return Outcome{}, ctx.Err()
	})
	items := []kubebreaks.Break{{Root: kref.Ref{UID: "root", Kind: "Deployment", Name: "api"}, Rule: "pod.failed", Severity: "critical", BrokeAt: now}}
	s.evaluateIssueAlerts(items, "", now)
	s.evaluateIssueAlerts(items, "owner", now)
	var worker context.Context
	select {
	case worker = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	if deadline, ok := worker.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
		t.Fatal("missing bound")
	}
	for range 20 {
		s.evaluateIssueAlerts(items, "owner", now)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	s.StopCapture()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker leaked")
	}
}
