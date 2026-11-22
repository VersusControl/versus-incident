package kubernetes

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSystemReadsWarnOnceForANamedNamespaceOutOfScope(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), nil)
	lines := []string{}
	fixture.reads.warn = func(key, cause, format string, args ...any) {
		lines = append(lines, cause+": "+fmt.Sprintf(format, args...))
	}
	admission, err := fixture.reads.admit(t.Context(), "prod", SystemReadLaneMetrics, systemReadPodMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := fixture.reads.allowNamespace(t.Context(), admission, "kube-system", true); ok || reason != SystemReadRefusedOutOfScope {
		t.Fatalf("named excluded namespace: ok=%v reason=%q", ok, reason)
	}
	if ok, _ := fixture.reads.allowNamespace(t.Context(), admission, "kube-system", false); ok {
		t.Fatal("excluded namespace allowed")
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "prod") || !strings.Contains(lines[0], "kube-system") || !strings.HasPrefix(lines[0], SystemReadRefusedOutOfScope) {
		t.Fatalf("warnings = %q", lines)
	}
}

// One call has a time limit for all its namespaces: a namespace that never
// answers ends the call at the limit, and the rest are skipped.
func TestSystemReadsCallStopsAtItsTimeLimit(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn([]string{"alpha", "bravo", "charlie"}, nil), func(kube *systemReadsKube, _ *Service) {
		kube.pods = map[string][]string{"alpha": {"api-1"}, "bravo": {"api-2"}, "charlie": {"api-3"}}
		kube.hang = map[string]bool{"/apis/metrics.k8s.io/v1beta1/namespaces/bravo/pods": true}
	})
	fixture.reads.callTimeout = 300 * time.Millisecond
	started := time.Now()
	metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the call ran %s past its limit", elapsed)
	}
	if err != nil || !metrics.Partial || metrics.NamespacesSkipped != 1 || len(metrics.Pods) != 1 {
		t.Fatalf("metrics = %+v, %v; want alpha's Pod and charlie skipped", metrics, err)
	}
	if counter := fixture.counter(t, systemReadPodMetrics); counter.NamespacesSkipped != 1 || counter.Causes[SystemReadTimeout] != 1 {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemReadsCallTimeLimitIsSixtySecondsAtMost(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	if systemReadCallTimeout != 60*time.Second || fixture.reads.callTimeout != systemReadCallTimeout {
		t.Fatalf("call limit = %s", fixture.reads.callTimeout)
	}
}

// A forbidden read is counted by cause and warned about once per cluster and
// read; a later success clears it.
func TestSystemReadsForbiddenReadIsCountedAndWarnedOnce(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.status = http.StatusForbidden
	})
	warned, cleared := []string{}, []string{}
	fixture.reads.warn = func(key, cause, format string, args ...any) {
		warned = append(warned, key+" "+cause+" "+fmt.Sprintf(format, args...))
	}
	fixture.reads.clear = func(key string) { cleared = append(cleared, key) }
	for range 2 {
		if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err == nil {
			t.Fatal("a forbidden read succeeded")
		}
	}
	counter := fixture.counter(t, systemReadWarningEvents)
	if counter.Causes[SystemReadForbidden] != 2 {
		t.Fatalf("counters = %+v", counter)
	}
	if len(warned) != 2 || !strings.Contains(warned[0], "prod/warning_events forbidden") || !strings.Contains(warned[0], "grant list on events") || strings.Contains(warned[0], systemReadCanary) {
		t.Fatalf("warnings = %q", warned)
	}
	fixture.kube.mu.Lock()
	fixture.kube.status = 0
	fixture.kube.mu.Unlock()
	if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0] != "prod/warning_events" {
		t.Fatalf("cleared = %q", cleared)
	}
	if counter := fixture.counter(t, systemReadWarningEvents); counter.Causes[SystemReadForbidden] != 2 || counter.LastCause != "" {
		t.Fatalf("counters after recovery = %+v", counter)
	}
}

// A policy that does not answer cannot hold a call past its time limit: the
// limit starts before the gate, and an unanswered policy refuses.
func TestSystemReadsCallLimitCoversABlockingPolicy(t *testing.T) {
	for name, block := range map[string]func(ctx context.Context, release <-chan struct{}){
		"policy that honours its context": func(ctx context.Context, _ <-chan struct{}) { <-ctx.Done() },
		"policy that ignores its context": func(_ context.Context, release <-chan struct{}) { <-release },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			fixture.reads.policy = func(ctx context.Context, _ string, _ SystemReadLane, _ string) bool {
				block(ctx, release)
				return true
			}
			fixture.reads.callTimeout = 300 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), scaledTimeout(hangGuard))
			defer cancel()
			started := time.Now()
			_, err := fixture.reads.PodMetrics(ctx, "prod")
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("a blocking policy held the call for %s", elapsed)
			}
			requireRefusal(t, err, SystemReadRefused, SystemReadRefusedPolicy)
			if len(fixture.kube.seen()) != 0 {
				t.Fatal("a call whose policy did not answer reached the cluster")
			}
		})
	}
}

// A namespace that stops a call by timing out is asked last by the next call,
// so it cannot starve the namespaces after it.
func TestSystemReadsResumePastTheNamespaceThatTimedOut(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn([]string{"alpha", "bravo", "charlie"}, nil), func(kube *systemReadsKube, _ *Service) {
		kube.pods = map[string][]string{"alpha": {"api-1"}, "bravo": {"api-2"}, "charlie": {"api-3"}}
		kube.hang = map[string]bool{"/apis/metrics.k8s.io/v1beta1/namespaces/bravo/pods": true}
	})
	fixture.reads.callTimeout = 300 * time.Millisecond
	first, err := fixture.reads.PodMetrics(t.Context(), "prod")
	if err != nil || len(first.Pods) != 1 || first.Pods[0].Namespace != "alpha" {
		t.Fatalf("first call = %+v, %v", first, err)
	}
	asked := len(fixture.kube.seen())
	second, err := fixture.reads.PodMetrics(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	requests := fixture.kube.seen()[asked:]
	if len(requests) == 0 || !strings.Contains(requests[0].URL.Path, "/namespaces/charlie/") {
		t.Fatalf("the second call did not start after the namespace that timed out: %v", requests)
	}
	namespaces := map[string]bool{}
	for _, pod := range second.Pods {
		namespaces[pod.Namespace] = true
	}
	if !namespaces["charlie"] || !namespaces["alpha"] {
		t.Fatalf("second call pods = %+v", second.Pods)
	}
	if last := requests[len(requests)-1].URL.Path; !strings.Contains(last, "/namespaces/bravo/") {
		t.Fatalf("the namespace that timed out was not asked last: %s", last)
	}
}
