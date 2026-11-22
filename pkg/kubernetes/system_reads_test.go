package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// resetSystemReadsForTest lets a test enable system reads again. It exists
// only in tests; production code has no way to undo the one enable.
func resetSystemReadsForTest() { systemReadsEnabled.Store(false) }

const systemReadCanary = "canary-secret-7f3a9c"

type canaryScrubber struct{}

func (canaryScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, systemReadCanary, "[redacted]")
}

// systemReadsKube is an in-process API server for the two system reads. It
// records every request and answers from data the test builds in code.
type systemReadsKube struct {
	mu       sync.Mutex
	requests []*http.Request
	// pods maps a namespace to its Pod names.
	pods map[string][]string
	// events maps a namespace to its Warning event messages.
	events map[string][]string
	// status, when set, answers every metrics and event request.
	status int
	// ignoreLimit makes the metrics list ignore limit the way metrics-server
	// does; padding adds bytes to every metrics item.
	ignoreLimit bool
	padding     int
	// statusFor answers one path with a status; hang holds a path until the
	// request gives up.
	statusFor map[string]int
	hang      map[string]bool
	// rawMetrics, when set, is the body of every metrics list; rawEvents
	// replaces the built event items.
	rawMetrics []byte
	rawEvents  []map[string]any
}

func (kube *systemReadsKube) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	kube.mu.Lock()
	kube.requests = append(kube.requests, request.Clone(context.Background()))
	status := kube.status
	path := request.URL.Path
	if specific, ok := kube.statusFor[path]; ok {
		status = specific
	}
	hang, raw := kube.hang[path], kube.rawMetrics
	kube.mu.Unlock()
	if hang {
		<-request.Context().Done()
		return
	}
	if raw != nil && strings.HasPrefix(path, "/apis/metrics.k8s.io/") {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(raw)
		return
	}
	if strings.HasPrefix(path, "/apis/authentication.k8s.io/") {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if status != 0 {
		if status == http.StatusTooManyRequests {
			writer.Header().Set("Retry-After", "30")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = fmt.Fprintf(writer, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":%d,"message":"events is forbidden: User \"reader\" cannot list resource \"events\" %s"}`, status, systemReadCanary)
		return
	}
	switch {
	case path == "/apis/metrics.k8s.io/v1beta1/pods":
		kube.writeMetrics(writer, request, "")
	case strings.HasPrefix(path, "/apis/metrics.k8s.io/v1beta1/namespaces/") && strings.HasSuffix(path, "/pods"):
		kube.writeMetrics(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/apis/metrics.k8s.io/v1beta1/namespaces/"), "/pods"))
	case path == "/api/v1/events":
		kube.writeEvents(writer, "")
	case strings.HasPrefix(path, "/api/v1/namespaces/") && strings.HasSuffix(path, "/events"):
		kube.writeEvents(writer, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/namespaces/"), "/events"))
	default:
		writer.WriteHeader(http.StatusNotFound)
	}
}

func (kube *systemReadsKube) writeMetrics(writer http.ResponseWriter, request *http.Request, only string) {
	kube.mu.Lock()
	defer kube.mu.Unlock()
	items := []map[string]any{}
	// The API server answers in namespace order.
	for _, namespace := range slices.Sorted(maps.Keys(kube.pods)) {
		pods := kube.pods[namespace]
		if only != "" && namespace != only {
			continue
		}
		for _, pod := range pods {
			item := map[string]any{
				"metadata":   map[string]any{"namespace": namespace, "name": pod},
				"timestamp":  "2026-11-21T10:00:00Z",
				"window":     "30s",
				"containers": []any{map[string]any{"name": "app", "usage": map[string]any{"cpu": "250m", "memory": "64Mi"}}},
			}
			if kube.padding > 0 {
				item["metadata"].(map[string]any)["annotations"] = map[string]any{"pad": strings.Repeat("x", kube.padding)}
			}
			items = append(items, item)
		}
	}
	metadata := map[string]any{}
	if limit, err := strconv.Atoi(request.URL.Query().Get("limit")); err == nil && limit > 0 && limit < len(items) && !kube.ignoreLimit {
		items, metadata["continue"] = items[:limit], "next-page"
	}
	writeJSON(writer, map[string]any{"kind": "PodMetricsList", "apiVersion": "metrics.k8s.io/v1beta1", "metadata": metadata, "items": items})
}

func (kube *systemReadsKube) writeEvents(writer http.ResponseWriter, only string) {
	kube.mu.Lock()
	defer kube.mu.Unlock()
	if kube.rawEvents != nil {
		writeJSON(writer, map[string]any{"kind": "EventList", "apiVersion": "v1", "metadata": map[string]any{}, "items": kube.rawEvents})
		return
	}
	items := []map[string]any{}
	for _, namespace := range slices.Sorted(maps.Keys(kube.events)) {
		messages := kube.events[namespace]
		if only != "" && namespace != only {
			continue
		}
		for index, message := range messages {
			items = append(items, map[string]any{
				"apiVersion":     "v1",
				"kind":           "Event",
				"metadata":       map[string]any{"namespace": namespace, "name": fmt.Sprintf("event-%d", index), "uid": fmt.Sprintf("%s-%d", namespace, index)},
				"type":           "Warning",
				"reason":         "BackOff",
				"message":        message,
				"count":          3,
				"firstTimestamp": "2026-11-21T09:00:00Z",
				"lastTimestamp":  "2026-11-21T10:00:00Z",
				"involvedObject": map[string]any{"apiVersion": "v1", "kind": "Pod", "namespace": namespace, "name": "api-1"},
			})
		}
	}
	writeJSON(writer, map[string]any{"kind": "EventList", "apiVersion": "v1", "metadata": map[string]any{}, "items": items})
}

func (kube *systemReadsKube) seen() []*http.Request {
	kube.mu.Lock()
	defer kube.mu.Unlock()
	return append([]*http.Request(nil), kube.requests...)
}

// dataRequests leaves out the identity review a credentialed service makes.
func (kube *systemReadsKube) dataRequests() []*http.Request {
	result := []*http.Request{}
	for _, request := range kube.seen() {
		if !strings.HasPrefix(request.URL.Path, "/apis/authentication.k8s.io/") {
			result = append(result, request)
		}
	}
	return result
}

type systemReadsFixture struct {
	kube     *systemReadsKube
	service  *Service
	provider storage.Provider
	registry *ServiceRegistry
	reads    *SystemReads
	policy   *systemPolicyRecorder
}

type systemPolicyRecorder struct {
	mu     sync.Mutex
	asked  []string
	refuse map[string]bool
}

func (recorder *systemPolicyRecorder) decide(_ context.Context, clusterID string, lane SystemReadLane, namespace string) bool {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.asked = append(recorder.asked, clusterID+"/"+string(lane)+"/"+namespace)
	return !recorder.refuse[namespace]
}

func (recorder *systemPolicyRecorder) questions() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.asked...)
}

// newSystemReadsFixture builds one cluster "prod" with its saved learning
// setting and enables a fresh handle over it.
func newSystemReadsFixture(t *testing.T, learning breaklogs.Learning, configure func(*systemReadsKube, *Service)) *systemReadsFixture {
	t.Helper()
	kube := &systemReadsKube{
		pods:   map[string][]string{"shop": {"api-1", "api-2"}, "kube-system": {"coredns-1"}},
		events: map[string][]string{"shop": {"Back-off restarting failed container api token=" + systemReadCanary}, "kube-system": {"coredns probe failed"}},
	}
	server := httptest.NewServer(kube)
	t.Cleanup(server.Close)
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "prod"})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	if configure != nil {
		configure(kube, service)
	}
	saveSystemReadLearning(t, provider, learning)
	registry := NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "prod", Provider: "generic"}, Service: service}})
	policy := &systemPolicyRecorder{refuse: map[string]bool{}}
	resetSystemReadsForTest()
	t.Cleanup(resetSystemReadsForTest)
	reads, err := EnableSystemReads(SystemReadsOptions{Registry: registry, Scrubber: canaryScrubber{}, Policy: policy.decide})
	if err != nil {
		t.Fatal(err)
	}
	return &systemReadsFixture{kube: kube, service: service, provider: provider, registry: registry, reads: reads, policy: policy}
}

func saveSystemReadLearning(t *testing.T, provider storage.Provider, learning breaklogs.Learning) {
	t.Helper()
	if err := breaklogs.UpdateSettings(provider, breaklogs.SettingsKey("default", "prod"), func(state *breaklogs.SettingsState) error {
		state.Settings.Learning = learning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func metricsOn(include, exclude []string) breaklogs.Learning {
	return breaklogs.Learning{Metrics: true, Scope: breaklogs.Scope{Include: include, Exclude: exclude}}
}

func requireRefusal(t *testing.T, err error, class, reason string) {
	t.Helper()
	var typed *SystemReadError
	if !errors.As(err, &typed) || typed.Class != class || typed.Reason != reason {
		t.Fatalf("error = %v, want class %q reason %q", err, class, reason)
	}
}

func (fixture *systemReadsFixture) counter(t *testing.T, read string) SystemReadCounters {
	t.Helper()
	for _, counter := range fixture.reads.Counters() {
		if counter.Cluster == "prod" && counter.Read == read {
			return counter
		}
	}
	t.Fatalf("no counters for %s in %+v", read, fixture.reads.Counters())
	return SystemReadCounters{}
}

func TestEnableSystemReadsSucceedsOncePerProcess(t *testing.T) {
	resetSystemReadsForTest()
	t.Cleanup(resetSystemReadsForTest)
	registry := NewClusterRegistry(false, nil)
	allow := func(context.Context, string, SystemReadLane, string) bool { return true }
	first, err := EnableSystemReads(SystemReadsOptions{Registry: registry, Scrubber: canaryScrubber{}, Policy: allow})
	if err != nil || first == nil {
		t.Fatalf("first enable: %v", err)
	}
	second, err := EnableSystemReads(SystemReadsOptions{Registry: registry, Scrubber: canaryScrubber{}, Policy: allow})
	if err == nil || second != nil {
		t.Fatal("a second enable in the same process succeeded")
	}
}

func TestEnableSystemReadsRefusesMissingPartsAndRaisedLimits(t *testing.T) {
	resetSystemReadsForTest()
	t.Cleanup(resetSystemReadsForTest)
	registry := NewClusterRegistry(false, nil)
	allow := func(context.Context, string, SystemReadLane, string) bool { return true }
	var typedNil *canaryPointerScrubber
	for name, options := range map[string]SystemReadsOptions{
		"no registry":        {Scrubber: canaryScrubber{}, Policy: allow},
		"no scrubber":        {Registry: registry, Policy: allow},
		"typed nil scrubber": {Registry: registry, Scrubber: typedNil, Policy: allow},
		"no policy":          {Registry: registry, Scrubber: canaryScrubber{}},
		"raised limit":       {Registry: registry, Scrubber: canaryScrubber{}, Policy: allow, MetricsResponseLimit: systemMetricsResponseCeiling + 1},
		"negative limit":     {Registry: registry, Scrubber: canaryScrubber{}, Policy: allow, MetricsResponseLimit: -1},
	} {
		if reads, err := EnableSystemReads(options); err == nil || reads != nil {
			t.Fatalf("%s: enable succeeded", name)
		}
	}
	// A refused enable does not use up the process's one enable.
	reads, err := EnableSystemReads(SystemReadsOptions{Registry: registry, Scrubber: canaryScrubber{}, Policy: allow, MetricsResponseLimit: 1 << 20})
	if err != nil || reads.metricsLimit != 1<<20 {
		t.Fatalf("lowered limit enable: %v", err)
	}
}

type canaryPointerScrubber struct{}

func (*canaryPointerScrubber) Scrub(value string) string { return value }

func TestSystemReadsZeroAndNilHandleRefuseEveryCall(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	var zero SystemReads
	var missing *SystemReads
	for name, reads := range map[string]*SystemReads{"zero": &zero, "nil": missing} {
		if _, err := reads.PodMetrics(t.Context(), "prod"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s handle pod metrics: %v", name, err)
		}
		if _, err := reads.WarningEvents(t.Context(), "prod"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s handle warning events: %v", name, err)
		}
		if reads.Counters() != nil {
			t.Fatalf("%s handle has counters", name)
		}
	}
	if requests := fixture.kube.seen(); len(requests) != 0 {
		t.Fatalf("a refused handle reached the cluster: %d requests", len(requests))
	}
}

func TestSystemReadsRefuseAContextCarryingACallerDecision(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	ctx := core.WithCallerAuthorization(t.Context(), core.CallerAuthorization{Authenticated: true, Actor: "admin"})
	_, err := fixture.reads.PodMetrics(ctx, "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedCallerContext)
	_, err = fixture.reads.WarningEvents(ctx, "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedCallerContext)
	if len(fixture.kube.seen()) != 0 || len(fixture.policy.questions()) != 0 {
		t.Fatal("a caller context reached the cluster or the policy")
	}
	if counter := fixture.counter(t, systemReadPodMetrics); counter.Refusals[SystemReadRefusedCallerContext] != 1 || counter.Reads != 0 {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemReadsRefuseAMissingScrubberAtReadTime(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	fixture.reads.scrubber = nil
	_, err := fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedNoScrubber)
	if len(fixture.kube.seen()) != 0 {
		t.Fatal("a read without a scrubber reached the cluster")
	}
}

func TestSystemReadsLaneOffOrNoAllowedNamespaceMakesNoRequest(t *testing.T) {
	t.Run("lane off", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, breaklogs.Learning{Logs: true, Metrics: false}, nil)
		_, err := fixture.reads.PodMetrics(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
		_, err = fixture.reads.WarningEvents(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
		if len(fixture.kube.seen()) != 0 || len(fixture.policy.questions()) != 0 {
			t.Fatal("a read with its lane off reached the cluster or the policy")
		}
	})
	t.Run("only namespace excluded", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn([]string{"kube-system"}, []string{"kube-system"}), nil)
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil || len(metrics.Pods) != 0 {
			t.Fatalf("metrics = %+v, %v", metrics, err)
		}
		events, err := fixture.reads.WarningEvents(t.Context(), "prod")
		if err != nil || len(events.Events) != 0 {
			t.Fatalf("events = %+v, %v", events, err)
		}
		if len(fixture.kube.seen()) != 0 {
			t.Fatal("an excluded namespace was asked")
		}
	})
	t.Run("include lists only allowed namespaces", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn([]string{"shop", "kube-system"}, []string{"kube-system"}), nil)
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		for _, request := range fixture.kube.seen() {
			if !strings.Contains(request.URL.Path, "/namespaces/shop/") {
				t.Fatalf("request outside the allowed namespace: %s", request.URL.Path)
			}
		}
	})
	t.Run("namespace-bound reader lists only allowed namespaces", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), func(_ *systemReadsKube, service *Service) {
			service.namespaces = []string{"kube-system", "shop"}
		})
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil || len(metrics.Pods) != 2 || !metrics.PerNamespace {
			t.Fatalf("metrics = %+v, %v", metrics, err)
		}
		if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		requests := fixture.kube.seen()
		if len(requests) != 2 {
			t.Fatalf("requests = %d, want one per read", len(requests))
		}
		for _, request := range requests {
			if !strings.Contains(request.URL.Path, "/namespaces/shop/") {
				t.Fatalf("request outside the allowed namespace: %s", request.URL.Path)
			}
		}
	})
}

func TestSystemReadsClusterWideListsReturnNothingFromAnExcludedNamespace(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), nil)
	metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.Pods) != 2 || metrics.PerNamespace {
		t.Fatalf("metrics = %+v", metrics)
	}
	for _, pod := range metrics.Pods {
		if pod.Namespace != "shop" || len(pod.Containers) != 1 || pod.Containers[0].CPUMillicores != 250 || pod.Containers[0].MemoryBytes != 64<<20 || pod.Window != 30*time.Second {
			t.Fatalf("pod sample = %+v", pod)
		}
	}
	events, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Namespace != "shop" || events.Events[0].Count != 3 || events.Events[0].Object.Name != "api-1" {
		t.Fatalf("events = %+v", events)
	}
	for _, read := range []string{systemReadPodMetrics, systemReadWarningEvents} {
		counter := fixture.counter(t, read)
		if counter.DroppedByScope != 1 || counter.Reads != 1 || counter.Requests != 1 || counter.Bytes == 0 || counter.LastListAt.IsZero() || counter.LastCause != "" {
			t.Fatalf("%s counters = %+v", read, counter)
		}
	}
}

func TestSystemReadsPolicyCanOnlyRefuse(t *testing.T) {
	t.Run("never asked about an excluded namespace", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), nil)
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
			t.Fatal(err)
		}
		events, err := fixture.reads.WarningEvents(t.Context(), "prod")
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events.Events {
			if event.Namespace == "kube-system" {
				t.Fatal("a policy that allows everything widened the saved scope")
			}
		}
		for _, question := range fixture.policy.questions() {
			if strings.HasSuffix(question, "/kube-system") {
				t.Fatalf("the policy was asked about an excluded namespace: %v", fixture.policy.questions())
			}
		}
	})
	t.Run("refuses the cluster before any request", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		fixture.policy.refuse[""] = true
		_, err := fixture.reads.PodMetrics(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedPolicy)
		if len(fixture.kube.seen()) != 0 {
			t.Fatal("a policy refusal reached the cluster")
		}
	})
	t.Run("refuses one namespace", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		fixture.policy.refuse["kube-system"] = true
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil || len(metrics.Pods) != 2 {
			t.Fatalf("metrics = %+v, %v", metrics, err)
		}
		if counter := fixture.counter(t, systemReadPodMetrics); counter.DroppedByPolicy != 1 || counter.DroppedByScope != 0 {
			t.Fatalf("counters = %+v", counter)
		}
	})
	t.Run("a panicking policy refuses", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
		fixture.reads.policy = func(context.Context, string, SystemReadLane, string) bool { panic("policy failed") }
		_, err := fixture.reads.WarningEvents(t.Context(), "prod")
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedPolicy)
		if len(fixture.kube.seen()) != 0 {
			t.Fatal("a failed policy reached the cluster")
		}
	})
}

func TestSystemReadsRefuseWhenSettingsCannotBeLoaded(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	if err := fixture.provider.WriteBlob(breaklogs.SettingsKey("default", "prod"), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedSettings)
	fixture.service.SetChangeStorage(nil)
	_, err = fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedSettings)
	if len(fixture.kube.seen()) != 0 {
		t.Fatal("a read with no readable setting reached the cluster")
	}
}

func TestSystemReadsRefuseAnUnknownCluster(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	for _, cluster := range []string{"", "staging"} {
		_, err := fixture.reads.PodMetrics(t.Context(), cluster)
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedUnknown)
	}
	if len(fixture.kube.seen()) != 0 {
		t.Fatal("an unknown cluster reached a cluster")
	}
	for _, counter := range fixture.reads.Counters() {
		if counter.Cluster != "" {
			t.Fatalf("an unknown cluster ID got its own counters: %+v", counter)
		}
	}
}

func TestSystemReadsReloadTheSavedSettingWithinThirtySeconds(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	fixture.reads.now = func() time.Time { return now }
	if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
		t.Fatal(err)
	}
	saveSystemReadLearning(t, fixture.provider, breaklogs.Learning{Metrics: false})
	now = now.Add(systemReadSettingsMaxAge)
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
}

func TestSystemReadsScrubEventMessages(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	events, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), systemReadCanary) || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("event output was not scrubbed: %s", encoded)
	}
}

func TestSystemReadsStopOnARetiredCredential(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(_ *systemReadsKube, service *Service) {
		service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer reader", nil }}
	})
	service := NewService(fixture.service.client, fixture.service.Scope(), 0)
	service.SetChangeStorage(fixture.provider)
	fixture.registry = NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "prod", Provider: "generic"}, Service: service}})
	fixture.reads.registry = fixture.registry
	if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
		t.Fatalf("a live credential was refused: %v", err)
	}
	before := len(fixture.kube.dataRequests())
	service.identity.mu.Lock()
	service.identity.retired = true
	service.identity.cancel()
	service.identity.mu.Unlock()
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	_, err = fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	if after := len(fixture.kube.dataRequests()); after != before {
		t.Fatalf("a retired credential reached the cluster: %d new requests", after-before)
	}
	if service.recovery.attemptInFlight() || service.recovery.attempts != 0 || !service.recovery.deadline().IsZero() {
		t.Fatal("a system read started credential recovery")
	}
}

func TestSystemReadsForbiddenAnswerLeavesCredentialAndHistoryAlone(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, service *Service) {
		kube.status = http.StatusForbidden
		service.changes.evidenceEnabled = true
	})
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadForbidden, "")
	_, err = fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadForbidden, "")
	if strings.Contains(err.Error(), systemReadCanary) || strings.Contains(err.Error(), "reader") {
		t.Fatalf("the error carried the response body: %v", err)
	}
	service := fixture.service
	service.identity.mu.Lock()
	retired := service.identity.retired || service.identity.ctx.Err() != nil
	service.identity.mu.Unlock()
	if retired {
		t.Fatal("a 403 retired the credential")
	}
	if service.recovery.attemptInFlight() || service.recovery.attempts != 0 || service.recovery.failures != 0 || !service.recovery.deadline().IsZero() {
		t.Fatal("a 403 started credential recovery")
	}
	service.changes.mu.Lock()
	denials := len(service.changes.historyDenials)
	service.changes.mu.Unlock()
	if denials != 0 {
		t.Fatal("a 403 to a system read was recorded as a history authority denial")
	}
	if counter := fixture.counter(t, systemReadWarningEvents); counter.LastCause != SystemReadForbidden || !counter.LastListAt.IsZero() {
		t.Fatalf("counters = %+v", counter)
	}
	// The same answer to a viewer read is recorded, so the check above can
	// see a denial when one is written.
	if _, err := service.ListEvents(t.Context(), EventOptions{Type: "Warning"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer read: %v", err)
	}
	service.changes.mu.Lock()
	denials = len(service.changes.historyDenials)
	service.changes.mu.Unlock()
	if denials == 0 {
		t.Fatal("the viewer read's denial was not recorded; the check above proves nothing")
	}
}

func TestSystemReadsMakeOnlyTheFixedListRequests(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err != nil {
		t.Fatal(err)
	}
	requests := fixture.kube.seen()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want the metrics list and the event list", len(requests))
	}
	for index, want := range []string{"/apis/metrics.k8s.io/v1beta1/pods", "/api/v1/events"} {
		request := requests[index]
		query := request.URL.Query()
		if request.Method != http.MethodGet || request.URL.Path != want || query.Has("watch") || strings.Contains(request.URL.RawQuery, "watch") {
			t.Fatalf("request %d = %s %s", index, request.Method, request.URL)
		}
		if want == "/api/v1/events" && query.Get("fieldSelector") != "type=Warning" {
			t.Fatalf("event list selector = %q", query.Get("fieldSelector"))
		}
		if want == "/apis/metrics.k8s.io/v1beta1/pods" && query.Get("limit") != "5000" {
			t.Fatalf("metrics list limit = %q", query.Get("limit"))
		}
	}
}

// publishSystemReadInventory gives the service a published inventory that
// lists namespaces, as the inventory worker would.
func publishSystemReadInventory(service *Service, namespaces ...string) {
	records := []kubeindex.Record{}
	for _, namespace := range namespaces {
		records = append(records, kubeindex.Record{Kind: "Namespace", Name: namespace})
	}
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	if service.changes.inventories == nil {
		service.changes.inventories = map[kubeindex.Scope]*inventoryCache{}
	}
	service.changes.inventories[service.indexScope()] = &inventoryCache{generation: inventoryGeneration{View: kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "Namespace": {State: "ready"}}}}}
}

func TestSystemReadsMetricsAnswerOverTheCeilingIsTooLarge(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.padding = 4096
	})
	fixture.reads.metricsLimit = 4096
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadTooLarge, "")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("too large does not match ErrResponseTooLarge: %v", err)
	}
	if counter := fixture.counter(t, systemReadPodMetrics); counter.LastCause != SystemReadTooLarge {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemReadsAcceptAMetricsBodyAboveTheClientLimit(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.padding = 2 << 20
	})
	if fixture.service.client.maxBodyBytes >= 3*(2<<20) {
		t.Fatalf("client limit %d does not exercise the system read limit", fixture.service.client.maxBodyBytes)
	}
	metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
	if err != nil || len(metrics.Pods) != 3 || metrics.PerNamespace {
		t.Fatalf("metrics = %d pods, per_namespace=%v, %v", len(metrics.Pods), metrics.PerNamespace, err)
	}
}

func TestSystemReadsListPerNamespaceWhenOneBodyIsTooBig(t *testing.T) {
	t.Run("metrics-server ignores limit", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"kube-system"}), func(kube *systemReadsKube, service *Service) {
			kube.ignoreLimit = true
			many := make([]string, systemMetricsPodCeiling+1)
			for index := range many {
				many[index] = fmt.Sprintf("worker-%d", index)
			}
			kube.pods["batch"] = many
			publishSystemReadInventory(service, "batch", "kube-system", "shop")
		})
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil {
			t.Fatal(err)
		}
		if !metrics.PerNamespace || !metrics.Truncated || len(metrics.Pods) != systemMetricsPodCeiling {
			t.Fatalf("per-namespace answer: per_namespace=%v truncated=%v pods=%d", metrics.PerNamespace, metrics.Truncated, len(metrics.Pods))
		}
		paths := []string{}
		for _, request := range fixture.kube.seen() {
			paths = append(paths, request.URL.Path)
		}
		want := []string{"/apis/metrics.k8s.io/v1beta1/pods", "/apis/metrics.k8s.io/v1beta1/namespaces/batch/pods"}
		if strings.Join(paths, " ") != strings.Join(want, " ") {
			t.Fatalf("requests = %v, want %v", paths, want)
		}
	})
	t.Run("an API server that honours limit", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, service *Service) {
			many := make([]string, systemMetricsPodCeiling+1)
			for index := range many {
				many[index] = fmt.Sprintf("worker-%d", index)
			}
			kube.pods = map[string][]string{"batch": many[:systemMetricsPodCeiling/2+1], "jobs": many[systemMetricsPodCeiling/2+1:]}
			publishSystemReadInventory(service, "batch", "jobs")
		})
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		// 5,001 Pods in all: the namespace lists fill the ceiling and say so.
		if err != nil || !metrics.PerNamespace || !metrics.Truncated || len(metrics.Pods) != systemMetricsPodCeiling {
			t.Fatalf("per-namespace answer: per_namespace=%v truncated=%v pods=%d err=%v", metrics.PerNamespace, metrics.Truncated, len(metrics.Pods), err)
		}
		if requests := len(fixture.kube.seen()); requests != 3 {
			t.Fatalf("requests = %d, want the cluster-wide list and one per namespace", requests)
		}
	})
	t.Run("body over the limit", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, service *Service) {
			kube.pods["batch"] = []string{"worker-1", "worker-2", "worker-3"}
			publishSystemReadInventory(service, "batch", "shop")
		})
		fixture.reads.metricsLimit = 700
		metrics, err := fixture.reads.PodMetrics(t.Context(), "prod")
		if err != nil {
			t.Fatal(err)
		}
		if !metrics.PerNamespace || len(metrics.Pods) != 5 {
			t.Fatalf("metrics = %+v", metrics)
		}
	})
}

func TestSystemReadsRotateNamespacesAcrossCalls(t *testing.T) {
	namespaces := make([]string, systemReadNamespacesPerCall+10)
	for index := range namespaces {
		namespaces[index] = fmt.Sprintf("team-%03d", index)
	}
	fixture := newSystemReadsFixture(t, metricsOn(namespaces, nil), nil)
	first, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil || first.NamespacesSkipped != 10 {
		t.Fatalf("first call: %+v, %v", first, err)
	}
	if _, err := fixture.reads.WarningEvents(t.Context(), "prod"); err != nil {
		t.Fatal(err)
	}
	asked := map[string]int{}
	for _, request := range fixture.kube.seen() {
		asked[request.URL.Path]++
	}
	if len(asked) != len(namespaces) {
		t.Fatalf("two calls asked %d namespaces, want all %d", len(asked), len(namespaces))
	}
}

// The handle must never be reachable from a request path: no code in the
// module outside this package may name it. The program that wires the worker
// passes it by hand.
func TestSystemReadsAreNotNamedOutsideTheKubernetesPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	own, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"EnableSystemReads": true, "SystemReads": true, "SystemReadsOptions": true}
	parsed := 0
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			absolute, _ := filepath.Abs(name)
			base := entry.Name()
			if absolute == own || base == "testdata" || base == "node_modules" || base == "vendor" || name != root && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		parsed++
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && forbidden[ident.Name] {
				t.Errorf("%s names %s", name, ident.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"cmd", "pkg/agent", "pkg/controllers", "pkg/routes"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(required))); err != nil {
			t.Fatalf("the walk does not cover %s: %v", required, err)
		}
	}
	if parsed < 100 {
		t.Fatalf("only %d source files were checked", parsed)
	}
}

func TestSystemReadsListEventsPerNamespaceWhenTheClusterListIsCutShort(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, []string{"alpha"}), func(kube *systemReadsKube, service *Service) {
		noisy := make([]string, maxProjectionItems+20)
		for index := range noisy {
			noisy[index] = fmt.Sprintf("probe failed %d", index)
		}
		kube.events = map[string][]string{"alpha": noisy, "shop": {"Back-off restarting failed container"}}
		publishSystemReadInventory(service, "alpha", "shop")
	})
	events, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Namespace != "shop" || !events.PerNamespace {
		t.Fatalf("events = %d, per_namespace=%v; the allowed namespace's event was lost behind an excluded one", len(events.Events), events.PerNamespace)
	}
	if counter := fixture.counter(t, systemReadWarningEvents); counter.CutShort != 1 {
		t.Fatalf("counters = %+v, want the cut-short cluster list counted", counter)
	}
	paths := []string{}
	for _, request := range fixture.kube.seen() {
		paths = append(paths, request.URL.Path)
	}
	if want := "/api/v1/events /api/v1/namespaces/shop/events"; strings.Join(paths, " ") != want {
		t.Fatalf("requests = %v, want %s", paths, want)
	}
}

func TestSystemReadsRefuseAClusterHeldForCredentialRecovery(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	recovery := fixture.service.recovery
	recovery.mu.Lock()
	recovery.gate("default", time.Now().Add(time.Hour), false, ErrCredentialUnavailable, false)
	recovery.mu.Unlock()
	if recovery.requestGateError("default") == nil {
		t.Fatal("the cluster is not held")
	}
	_, err := fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	_, err = fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	if len(fixture.kube.seen()) != 0 {
		t.Fatal("a cluster held for recovery was asked")
	}
}

func TestSystemReadThrottleIsKeptApartFromViewerReads(t *testing.T) {
	t.Run("a throttled system read does not stall viewer reads", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
			kube.status = http.StatusTooManyRequests
		})
		if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err == nil {
			t.Fatal("a throttled read succeeded")
		}
		fixture.kube.mu.Lock()
		fixture.kube.status = 0
		fixture.kube.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		var list map[string]any
		if err := fixture.service.client.GetJSON(ctx, "/api/v1/events", &list); err != nil {
			t.Fatalf("a viewer read waited out the system read's throttle: %v", err)
		}
		ctx, cancel = context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		if _, err := fixture.reads.PodMetrics(ctx, "prod"); err == nil {
			t.Fatal("a system read ignored its own throttle")
		}
	})
	t.Run("a throttled viewer read does not stall system reads", func(t *testing.T) {
		fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
			kube.status = http.StatusTooManyRequests
		})
		var list map[string]any
		var throttled *ErrThrottled
		if err := fixture.service.client.GetJSON(t.Context(), "/api/v1/events", &list); !errors.As(err, &throttled) {
			t.Fatalf("viewer read: %v", err)
		}
		fixture.kube.mu.Lock()
		fixture.kube.status = 0
		fixture.kube.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if _, err := fixture.reads.PodMetrics(ctx, "prod"); err != nil {
			t.Fatalf("a system read waited out the viewer's throttle: %v", err)
		}
	})
}

func TestSystemReadsPanickingScrubberIsCountedAsARefusal(t *testing.T) {
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), nil)
	fixture.reads.scrubber = systemPanicScrubber{}
	_, err := fixture.reads.WarningEvents(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedScrubberFailed)
	if counter := fixture.counter(t, systemReadWarningEvents); counter.Refusals[SystemReadRefusedScrubberFailed] != 1 || counter.Reads != 1 {
		t.Fatalf("counters = %+v", counter)
	}
}

type systemPanicScrubber struct{}

func (systemPanicScrubber) Scrub(string) string { panic("scrubber failed") }

func TestProjectedTokenProofThrottleFollowsTheReadItRunsFor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "30")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	backoffs := func(client *Client) (user, system time.Time) {
		client.backoffMu.Lock()
		defer client.backoffMu.Unlock()
		return client.backoffUntil, client.systemBackoffUntil
	}
	waitsWithin := func(client *Client, ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		return client.waitBackoff(ctx)
	}
	t.Run("a proof for a system read backs off only system reads", func(t *testing.T) {
		client := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "prod"}).client
		verifier := &projectedTokenVerifier{client: client}
		var output map[string]any
		var throttled *ErrThrottled
		if _, err := verifier.get(systemRead(t.Context(), 0), "/openid/v1/jwks", "token", &output); !errors.As(err, &throttled) {
			t.Fatalf("proof: %v", err)
		}
		if user, system := backoffs(client); !user.IsZero() || system.IsZero() {
			t.Fatalf("user backoff %v, system backoff %v", user, system)
		}
		if err := waitsWithin(client, t.Context()); err != nil {
			t.Fatalf("a user read waited out a system read's proof throttle: %v", err)
		}
	})
	t.Run("a proof for a user read does not stall system reads", func(t *testing.T) {
		client := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "prod"}).client
		verifier := &projectedTokenVerifier{client: client}
		var output map[string]any
		var throttled *ErrThrottled
		if _, err := verifier.get(t.Context(), "/openid/v1/jwks", "token", &output); !errors.As(err, &throttled) {
			t.Fatalf("proof: %v", err)
		}
		if user, system := backoffs(client); user.IsZero() || !system.IsZero() {
			t.Fatalf("user backoff %v, system backoff %v", user, system)
		}
		if err := waitsWithin(client, systemRead(t.Context(), 0)); err != nil {
			t.Fatalf("a system read waited out a user read's proof throttle: %v", err)
		}
	})
}

// A metrics body of many empty items costs memory near its own size: the list
// is decoded as a stream that stops after the Pod ceiling.
func TestSystemReadsMetricsBodyOfEmptyItemsStaysWithinMemory(t *testing.T) {
	body := []byte(`{"kind":"PodMetricsList","apiVersion":"metrics.k8s.io/v1beta1","metadata":{},"items":[` + strings.Repeat("{},", 2<<20) + `{}]}`)
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.rawMetrics = body
	})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _ = fixture.reads.PodMetrics(t.Context(), "prod")
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(16 * len(body)); allocated > limit {
		t.Fatalf("a %d-byte body of empty items allocated %d bytes, want at most %d", len(body), allocated, limit)
	}
}

// A throttle on one namespace ends the call: the namespaces after it are not
// asked and are counted as skipped.
func TestSystemReadsStopAtTheFirstThrottle(t *testing.T) {
	namespaces := []string{"alpha", "bravo", "charlie"}
	for _, read := range []string{systemReadPodMetrics, systemReadWarningEvents} {
		t.Run(read, func(t *testing.T) {
			fixture := newSystemReadsFixture(t, metricsOn(namespaces, nil), func(kube *systemReadsKube, _ *Service) {
				kube.pods = map[string][]string{"alpha": {"api-1"}, "bravo": {"api-2"}, "charlie": {"api-3"}}
				kube.events = map[string][]string{"alpha": {"probe failed"}, "bravo": {"probe failed"}, "charlie": {"probe failed"}}
				kube.statusFor = map[string]int{"/apis/metrics.k8s.io/v1beta1/namespaces/bravo/pods": http.StatusTooManyRequests, "/api/v1/namespaces/bravo/events": http.StatusTooManyRequests}
			})
			started := time.Now()
			var result any
			var err error
			if read == systemReadPodMetrics {
				result, err = fixture.reads.PodMetrics(t.Context(), "prod")
			} else {
				result, err = fixture.reads.WarningEvents(t.Context(), "prod")
			}
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed > 5*time.Second {
				t.Fatalf("the call took %s after a throttle", elapsed)
			}
			for _, request := range fixture.kube.seen() {
				if strings.Contains(request.URL.Path, "/charlie/") {
					t.Fatal("a namespace after the throttle was asked")
				}
			}
			encoded, _ := json.Marshal(result)
			if !strings.Contains(string(encoded), `"partial":true`) || !strings.Contains(string(encoded), `"namespaces_skipped":1`) {
				t.Fatalf("result = %s, want a partial result with one namespace skipped", encoded)
			}
			counters, _ := json.Marshal(fixture.counter(t, read))
			if !strings.Contains(string(counters), `"namespaces_skipped":1`) {
				t.Fatalf("counters = %s", counters)
			}
		})
	}
}

// No text field of a Warning event leaves the package unchecked: a reference
// that is not a valid Kubernetes value drops the event, and every field that
// is kept passes through the scrubber.
func TestSystemReadsCheckAndScrubEveryEventField(t *testing.T) {
	event := func(change func(item, involved map[string]any)) map[string]any {
		involved := map[string]any{"apiVersion": "v1", "kind": "Pod", "namespace": "shop", "name": "api-1", "uid": "pod-uid-1"}
		item := map[string]any{"apiVersion": "v1", "kind": "Event", "metadata": map[string]any{"namespace": "shop", "name": "api-1.181", "uid": "event-uid-1"}, "type": "Warning", "reason": "BackOff", "message": "restarting", "count": 1, "involvedObject": involved, "source": map[string]any{"component": "kubelet"}}
		change(item, involved)
		return item
	}
	meta := func(item map[string]any) map[string]any { return item["metadata"].(map[string]any) }
	events := []map[string]any{
		event(func(item, _ map[string]any) { item["message"] = "token=" + systemReadCanary }),
		event(func(item, _ map[string]any) { item["reason"] = systemReadCanary }),
		event(func(item, _ map[string]any) { meta(item)["name"] = "event-" + systemReadCanary }),
		event(func(item, _ map[string]any) { meta(item)["uid"] = systemReadCanary }),
		event(func(item, _ map[string]any) { meta(item)["namespace"] = "shop\nforged" }),
		event(func(_, involved map[string]any) { involved["kind"] = "Pod\nForged" }),
		event(func(_, involved map[string]any) { involved["name"] = "api-" + systemReadCanary }),
		event(func(_, involved map[string]any) { involved["namespace"] = "shop " + systemReadCanary }),
		event(func(_, involved map[string]any) { involved["apiVersion"] = "v1 " + systemReadCanary }),
		event(func(_, involved map[string]any) { involved["uid"] = systemReadCanary }),
		event(func(item, _ map[string]any) { item["source"] = map[string]any{"component": systemReadCanary} }),
	}
	fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.rawEvents = events
	})
	result, err := fixture.reads.WarningEvents(t.Context(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), systemReadCanary) || strings.Contains(string(encoded), `\n`) || strings.Contains(string(encoded), "forged") {
		t.Fatalf("an unchecked field left the package: %s", encoded)
	}
	// Four events carry values that are not valid Kubernetes values and are
	// dropped; the other seven are kept with every field scrubbed.
	if !strings.Contains(string(encoded), `"invalid":4`) || len(result.Events) != 7 || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("result = %s, want seven scrubbed events kept and four dropped", encoded)
	}
}

// A deeply nested metrics body is refused as malformed before its nesting
// costs memory, wherever the nesting is.
func TestSystemReadsRefuseDeeplyNestedMetricsBodies(t *testing.T) {
	depth := 7 << 20
	nested := strings.Repeat("[", depth) + strings.Repeat("]", depth)
	for name, body := range map[string]string{
		"unknown top-level key": `{"kind":"PodMetricsList","extra":` + nested + `,"items":[]}`,
		"inside an item":        `{"kind":"PodMetricsList","items":[{"metadata":{"namespace":"shop","name":"api-1"},"extra":` + nested + `}]}`,
		"inside a container":    `{"kind":"PodMetricsList","items":[{"metadata":{"namespace":"shop","name":"api-1"},"containers":[{"name":"app","usage":{"cpu":"1m","memory":"1Mi","extra":` + nested + `}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(body)
			fixture := newSystemReadsFixture(t, metricsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
				kube.rawMetrics = raw
			})
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := fixture.reads.PodMetrics(t.Context(), "prod")
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			if limit := uint64(16 * len(raw)); allocated > limit {
				t.Fatalf("a %d-byte nested body allocated %d bytes, want at most %d", len(raw), allocated, limit)
			}
			var typed *SystemReadError
			if !errors.As(err, &typed) || typed.Class != "malformed" {
				t.Fatalf("error = %v, want a malformed refusal", err)
			}
			counters, _ := json.Marshal(fixture.counter(t, systemReadPodMetrics))
			if !strings.Contains(string(counters), `"malformed":1`) {
				t.Fatalf("counters = %s", counters)
			}
		})
	}
}
