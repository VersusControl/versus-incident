package kubernetes

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// twoClusterReads is one system read handle over two clusters, each with its
// own in-process API server and saved learning setting in one shared store.
type twoClusterReads struct {
	kubes    map[string]*systemReadsKube
	provider storage.Provider
	reads    *SystemReads
	policy   *systemPolicyRecorder
}

func newTwoClusterReads(t *testing.T, learnings map[string]breaklogs.Learning, policy SystemReadPolicy) *twoClusterReads {
	t.Helper()
	fixture := &twoClusterReads{
		kubes: map[string]*systemReadsKube{
			"prod": {
				pods:   map[string][]string{"shop": {"api-1", "api-2"}, "kube-system": {"coredns-1"}},
				events: map[string][]string{"shop": {"shop back-off"}, "kube-system": {"coredns probe failed"}},
			},
			"staging": {
				pods:   map[string][]string{"payments": {"pay-1"}, "batch": {"job-1", "job-2"}},
				events: map[string][]string{"payments": {"payments back-off"}, "batch": {"job failed"}},
			},
		},
		provider: storage.NewMemory(),
		policy:   &systemPolicyRecorder{refuse: map[string]bool{}},
	}
	entries := []ClusterEntry{}
	for _, id := range []string{"prod", "staging"} {
		server := httptest.NewServer(fixture.kubes[id])
		t.Cleanup(server.Close)
		service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: id})
		service.SetChangeStorage(fixture.provider)
		saveClusterLearning(t, fixture.provider, id, learnings[id])
		entries = append(entries, ClusterEntry{Info: ClusterInfo{ID: id, Provider: "generic"}, Service: service})
	}
	if policy == nil {
		policy = fixture.policy.decide
	}
	resetSystemReadsForTest()
	t.Cleanup(resetSystemReadsForTest)
	reads, err := EnableSystemReads(SystemReadsOptions{Registry: NewClusterRegistry(true, entries), Scrubber: canaryScrubber{}, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	fixture.reads = reads
	return fixture
}

func saveClusterLearning(t *testing.T, provider storage.Provider, clusterID string, learning breaklogs.Learning) {
	t.Helper()
	if err := breaklogs.UpdateSettings(provider, breaklogs.SettingsKey("default", clusterID), func(state *breaklogs.SettingsState) error {
		state.Settings.Learning = learning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func counterFor(reads *SystemReads, cluster, read string) SystemReadCounters {
	for _, counter := range reads.Counters() {
		if counter.Cluster == cluster && counter.Read == read {
			return counter
		}
	}
	return SystemReadCounters{}
}

// Two clusters with different saved scopes are read at once through one
// handle. Each answer holds only its own cluster's allowed namespaces, each
// API server is asked only about its own allowed namespaces, the policy is
// asked about the right cluster, and the counters add up per cluster.
func TestSystemReadsConcurrentClustersKeepTheirOwnScope(t *testing.T) {
	fixture := newTwoClusterReads(t, map[string]breaklogs.Learning{
		"prod":    metricsOn(nil, []string{"kube-system"}),
		"staging": metricsOn([]string{"payments"}, nil),
	}, nil)
	wantPods := map[string][]string{"prod": {"shop/api-1", "shop/api-2"}, "staging": {"payments/pay-1"}}
	wantEvents := map[string][]string{"prod": {"shop"}, "staging": {"payments"}}

	const callsPerRead = 25
	var wg sync.WaitGroup
	errs := make(chan error, 4*callsPerRead)
	for _, cluster := range []string{"prod", "staging"} {
		for range callsPerRead {
			wg.Go(func() {
				metrics, err := fixture.reads.PodMetrics(context.Background(), cluster)
				if err != nil {
					errs <- fmt.Errorf("%s pod metrics: %w", cluster, err)
					return
				}
				got := []string{}
				for _, pod := range metrics.Pods {
					got = append(got, pod.Namespace+"/"+pod.Pod)
				}
				sort.Strings(got)
				if !slices.Equal(got, wantPods[cluster]) {
					errs <- fmt.Errorf("%s pods = %v, want %v", cluster, got, wantPods[cluster])
				}
			})
			wg.Go(func() {
				events, err := fixture.reads.WarningEvents(context.Background(), cluster)
				if err != nil {
					errs <- fmt.Errorf("%s warning events: %w", cluster, err)
					return
				}
				got := []string{}
				for _, event := range events.Events {
					got = append(got, event.Namespace)
				}
				if !slices.Equal(got, wantEvents[cluster]) {
					errs <- fmt.Errorf("%s event namespaces = %v, want %v", cluster, got, wantEvents[cluster])
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for _, request := range fixture.kubes["staging"].dataRequests() {
		if !strings.Contains(request.URL.Path, "/namespaces/payments/") {
			t.Fatalf("staging was asked outside its include list: %s", request.URL.Path)
		}
	}
	for _, request := range fixture.kubes["prod"].dataRequests() {
		if strings.Contains(request.URL.Path, "payments") || strings.Contains(request.URL.Path, "batch") {
			t.Fatalf("prod was asked about a staging namespace: %s", request.URL.Path)
		}
	}
	for cluster, kube := range fixture.kubes {
		if got := len(kube.dataRequests()); got != 2*callsPerRead {
			t.Fatalf("%s served %d requests, want %d", cluster, got, 2*callsPerRead)
		}
	}
	allowedQuestions := map[string]bool{
		"prod/metrics/": true, "prod/metrics/shop": true,
		"staging/metrics/": true, "staging/metrics/payments": true,
	}
	for _, question := range fixture.policy.questions() {
		if !allowedQuestions[question] {
			t.Fatalf("policy asked %q", question)
		}
	}
	for _, read := range []string{systemReadPodMetrics, systemReadWarningEvents} {
		prod, staging := counterFor(fixture.reads, "prod", read), counterFor(fixture.reads, "staging", read)
		if prod.Reads != callsPerRead || prod.Requests != callsPerRead || prod.DroppedByScope != callsPerRead || prod.DroppedByPolicy != 0 || len(prod.Refusals) != 0 {
			t.Fatalf("prod %s counters = %+v", read, prod)
		}
		if staging.Reads != callsPerRead || staging.Requests != callsPerRead || staging.DroppedByScope != 0 || staging.DroppedByPolicy != 0 || len(staging.Refusals) != 0 {
			t.Fatalf("staging %s counters = %+v", read, staging)
		}
	}
}

// A policy that blocks while it decides one cluster holds nothing the other
// cluster's reads or the counters need.
func TestSystemReadsSlowPolicyDoesNotBlockOtherClusters(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	policy := func(_ context.Context, clusterID string, _ SystemReadLane, namespace string) bool {
		if clusterID == "prod" && namespace == "" {
			once.Do(func() { close(entered) })
			<-release
		}
		return true
	}
	fixture := newTwoClusterReads(t, map[string]breaklogs.Learning{
		"prod":    metricsOn(nil, nil),
		"staging": metricsOn(nil, nil),
	}, policy)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	prodDone := make(chan error, 1)
	go func() {
		_, err := fixture.reads.PodMetrics(context.Background(), "prod")
		prodDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the prod read never asked the policy")
	}

	other := make(chan error, 1)
	go func() {
		if _, err := fixture.reads.PodMetrics(context.Background(), "staging"); err != nil {
			other <- err
			return
		}
		_, err := fixture.reads.WarningEvents(context.Background(), "staging")
		fixture.reads.Counters()
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("staging read while prod's policy blocks: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a blocked policy for prod held up staging reads or the counters")
	}

	close(release)
	released = true
	select {
	case err := <-prodDone:
		if err != nil {
			t.Fatalf("prod read after the policy answered: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prod read did not finish after the policy answered")
	}
}

// The saved setting is read again once it is 30 s old, never later: a lane
// turned off or a namespace newly excluded applies to the first call after
// the window, a clock that steps back reloads at once, and a load that fails
// is not remembered.
func TestSystemReadsSettingsCacheWindow(t *testing.T) {
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	t.Run("lane off applies at the window and not before", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		now := start
		fixture.reads.now = func() time.Time { return now }
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		saveSystemReadLearning(t, fixture.provider, breaklogs.Learning{Metrics: false})
		now = start.Add(systemReadSettingsMaxAge - time.Millisecond)
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatalf("inside the window the loaded setting is used: %v", err)
		}
		now = start.Add(systemReadSettingsMaxAge)
		_, err := fixture.reads.WarningEvents(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
	})

	t.Run("newly excluded namespace leaves the answer at the window", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		now := start
		fixture.reads.now = func() time.Time { return now }
		if metrics, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil || len(metrics.Pods) != 3 {
			t.Fatalf("metrics = %+v, %v", metrics, err)
		}
		saveSystemReadLearning(t, fixture.provider, metricsOn(nil, []string{"shop"}))
		now = start.Add(systemReadSettingsMaxAge)
		before := len(fixture.policy.questions())
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil {
			t.Fatal(err)
		}
		for _, pod := range metrics.Pods {
			if pod.Namespace == "shop" {
				t.Fatalf("an excluded namespace is still read after the window: %+v", metrics.Pods)
			}
		}
		for _, question := range fixture.policy.questions()[before:] {
			if strings.HasSuffix(question, "/shop") {
				t.Fatalf("the policy was asked about a newly excluded namespace: %v", question)
			}
		}
	})

	t.Run("clock stepping back reloads", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		now := start
		fixture.reads.now = func() time.Time { return now }
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		saveSystemReadLearning(t, fixture.provider, breaklogs.Learning{Metrics: false})
		now = start.Add(-time.Second)
		_, err := fixture.reads.PodMetrics(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
	})

	t.Run("failed load is not remembered", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		now := start
		fixture.reads.now = func() time.Time { return now }
		key := breaklogs.SettingsKey("default", "prod")
		good, err := fixture.provider.ReadBlob(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.provider.WriteBlob(key, []byte("{not json")); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.reads.PodMetrics(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedSettings)
		if err := fixture.provider.WriteBlob(key, good); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatalf("a repaired setting is not read at once: %v", err)
		}
	})
}

// recordingScrubber keeps every value it was asked to scrub.
type recordingScrubber struct {
	mu   sync.Mutex
	seen []string
}

func (scrubber *recordingScrubber) Scrub(value string) string {
	scrubber.mu.Lock()
	defer scrubber.mu.Unlock()
	scrubber.seen = append(scrubber.seen, value)
	return "scrubbed:" + value
}

// Both free-text fields of a Warning event pass through the scrubber, and
// what the scrubber returns is what the caller gets.
func TestSystemReadsScrubReasonAndMessage(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), nil)
	scrubber := &recordingScrubber{}
	fixture.reads.scrubber = scrubber
	events, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	event := events.Events[0]
	if event.Reason != "scrubbed:BackOff" || !strings.HasPrefix(event.Message, "scrubbed:Back-off restarting") {
		t.Fatalf("event = %+v, want the scrubber's output in reason and message", event)
	}
	if !slices.Contains(scrubber.seen, "BackOff") {
		t.Fatalf("the scrubber never saw the reason: %q", scrubber.seen)
	}
}

type panickingScrubber struct{}

func (panickingScrubber) Scrub(string) string { panic("scrubber failed") }

// A scrubber that panics fails the read closed, as a panicking policy does:
// the call returns an error and no event text, and does not panic into the
// worker.
func TestSystemReadsPanickingScrubberFailsClosed(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	fixture.reads.scrubber = panickingScrubber{}
	var (
		events    WarningEventList
		err       error
		recovered any
	)
	func() {
		defer func() { recovered = recover() }()
		events, err = fixture.reads.WarningEvents(t.Context(), "prod")
	}()
	if recovered != nil {
		t.Fatalf("a panicking scrubber panicked out of WarningEvents: %v", recovered)
	}
	if err == nil || len(events.Events) != 0 {
		t.Fatalf("events = %+v, err = %v; want an error and no events", events, err)
	}
	if counter := fixture.counter(t, systemReadWarningEvents); counter.Reads+uint64(len(counter.Refusals)) == 0 {
		t.Fatalf("the failed read left no trace in the counters: %+v", counter)
	}
}
