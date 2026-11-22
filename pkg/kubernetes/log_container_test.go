package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func logContainerFixture(annotation string, containers ...string) logContainerPod {
	var pod logContainerPod
	if annotation != "" {
		pod.Metadata.Annotations = map[string]string{defaultContainerAnnotation: annotation}
	}
	for _, name := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, logContainerName{Name: name})
	}
	return pod
}

func TestDefaultLogContainerFollowsKubectlOrder(t *testing.T) {
	cases := []struct {
		name string
		pod  logContainerPod
		want string
	}{
		{"annotation names a regular container", logContainerFixture("worker", "istio-proxy", "app", "worker"), "worker"},
		{"annotation names no regular container", logContainerFixture("migrate", "linkerd-proxy", "app"), "app"},
		{"annotation names no container of the Pod", logContainerFixture("gone", "istio-proxy", "app"), "app"},
		{"injected sidecar listed first is skipped", logContainerFixture("", "istio-proxy", "app"), "app"},
		{"every known sidecar is skipped", logContainerFixture("", "linkerd-proxy", "vault-agent", "consul-dataplane", "api"), "api"},
		{"only sidecars falls back to the first container", logContainerFixture("", "istio-proxy", "vault-agent"), "istio-proxy"},
		{"single container is left to Kubernetes", logContainerFixture("app", "app"), ""},
		{"no containers", logContainerFixture(""), ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := defaultLogContainer(test.pod); got != test.want {
				t.Fatalf("default container = %q, want %q", got, test.want)
			}
		})
	}
}

// logContainerServer answers the Pod read with the given status and containers,
// records every log request's container parameter, and refuses a
// container-less log read of a multi-container Pod the way Kubernetes does.
type logContainerServer struct {
	mu         sync.Mutex
	podReads   int
	containers []string
	hasParam   []bool
}

func newLogContainerServer(t *testing.T, podStatus int, annotation string, containers ...string) (*logContainerServer, *Service) {
	t.Helper()
	recorder := &logContainerServer{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		if !strings.HasSuffix(request.URL.Path, "/log") {
			recorder.podReads++
			if podStatus != http.StatusOK {
				writer.WriteHeader(podStatus)
				return
			}
			spec := []any{}
			for _, name := range containers {
				spec = append(spec, map[string]any{"name": name})
			}
			metadata := map[string]any{"name": "api", "namespace": "shop"}
			if annotation != "" {
				metadata["annotations"] = map[string]any{defaultContainerAnnotation: annotation}
			}
			writeJSON(writer, map[string]any{"kind": "Pod", "metadata": metadata, "spec": map[string]any{"containers": spec}})
			return
		}
		recorder.containers = append(recorder.containers, request.URL.Query().Get("container"))
		recorder.hasParam = append(recorder.hasParam, request.URL.Query().Has("container"))
		if !request.URL.Query().Has("container") && len(containers) > 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"a container name must be specified for pod api"}`))
			return
		}
		writer.Write([]byte(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano) + " ready\n"))
	}))
	t.Cleanup(server.Close)
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	return recorder, service
}

func (recorder *logContainerServer) snapshot() (int, []string, []bool) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.podReads, append([]string(nil), recorder.containers...), append([]bool(nil), recorder.hasParam...)
}

func TestPodLogsResolvesADefaultContainerForMultiContainerPods(t *testing.T) {
	t.Run("sidecar is skipped", func(t *testing.T) {
		recorder, service := newLogContainerServer(t, http.StatusOK, "", "istio-proxy", "app")
		logs, err := service.PodLogs(logStreamTestContext(), "shop", "api", "", false, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		podReads, containers, _ := recorder.snapshot()
		if logs.Container != "app" || podReads != 1 || len(containers) != 2 || containers[0] != "" || containers[1] != "app" {
			t.Fatalf("container = %q, upstream = %v, pod reads = %d", logs.Container, containers, podReads)
		}
	})
	t.Run("annotation wins", func(t *testing.T) {
		recorder, service := newLogContainerServer(t, http.StatusOK, "worker", "istio-proxy", "app", "worker")
		logs, err := service.PodLogs(logStreamTestContext(), "shop", "api", "", false, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, containers, _ := recorder.snapshot()
		if logs.Container != "worker" || len(containers) != 2 || containers[1] != "worker" {
			t.Fatalf("container = %q, upstream = %v", logs.Container, containers)
		}
	})
	t.Run("single container is sent as before", func(t *testing.T) {
		recorder, service := newLogContainerServer(t, http.StatusOK, "", "app")
		logs, err := service.PodLogs(logStreamTestContext(), "shop", "api", "", false, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		podReads, _, hasParam := recorder.snapshot()
		if logs.Container != "" || podReads != 0 || len(hasParam) != 1 || hasParam[0] {
			t.Fatalf("container = %q, pod reads = %d, upstream carried a container: %v", logs.Container, podReads, hasParam)
		}
	})
	t.Run("explicit container is untouched and the Pod is not read", func(t *testing.T) {
		recorder, service := newLogContainerServer(t, http.StatusOK, "app", "istio-proxy", "app")
		logs, err := service.PodLogs(logStreamTestContext(), "shop", "api", "istio-proxy", false, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		podReads, containers, _ := recorder.snapshot()
		if logs.Container != "istio-proxy" || containers[0] != "istio-proxy" || podReads != 0 {
			t.Fatalf("container = %q, upstream = %v, pod reads = %d", logs.Container, containers, podReads)
		}
	})
	t.Run("unreadable Pod keeps the refusal", func(t *testing.T) {
		recorder, service := newLogContainerServer(t, http.StatusForbidden, "", "istio-proxy", "app")
		_, err := service.PodLogs(logStreamTestContext(), "shop", "api", "", false, 0, 0)
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("err = %v, want the original refusal", err)
		}
		podReads, _, hasParam := recorder.snapshot()
		if podReads != 1 || len(hasParam) != 1 || hasParam[0] {
			t.Fatalf("pod reads = %d, upstream carried a container: %v", podReads, hasParam)
		}
	})
}

// A streamed read of a multi-container Pod opened without a container sends the
// resolved one upstream and names it on each line, while the cursor it issues
// still resumes a viewer that asks again without a container.
func TestPodLogStreamResolvesADefaultContainer(t *testing.T) {
	recorder, service := newLogContainerServer(t, http.StatusOK, "", "istio-proxy", "app")
	options := PodLogStreamOptions{Namespace: "shop", Pod: "api", Previous: true}
	events := runPodLogStream(t, service, options)
	_, containers, _ := recorder.snapshot()
	if len(containers) != 2 || containers[1] != "app" {
		t.Fatalf("upstream containers = %v", containers)
	}
	cursor := ""
	lines := 0
	for _, event := range events {
		if event.Event != "line" {
			continue
		}
		lines++
		if event.Container != "app" {
			t.Fatalf("line container = %q", event.Container)
		}
		if event.Cursor != "" {
			cursor = event.Cursor
		}
	}
	if lines == 0 || cursor == "" {
		t.Fatalf("stream sent %d lines, cursor %q: %+v", lines, cursor, events)
	}
	if err := service.ValidatePodLogStream(PodLogStreamOptions{Namespace: "shop", Pod: "api", Previous: true, Cursor: cursor}); err != nil {
		t.Fatalf("container-less resume refused: %v", err)
	}
}

// A live follow of a multi-container Pod opened without a container follows
// the resolved one. When the connection drops and the viewer reconnects
// without a container from the cursor it holds, the stream resumes on that
// same container with the lines written meanwhile, none lost or repeated.
func TestPodLogLiveFollowResolvesADefaultContainerAndResumes(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	lines := []struct {
		at   time.Time
		text string
	}{{base, "line-1"}, {base.Add(time.Second), "line-2"}, {base.Add(2 * time.Second), "line-3"}}
	var mu sync.Mutex
	var queries []url.Values
	written := 2
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			writeJSON(writer, map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "api", "namespace": "shop"}, "spec": map[string]any{"containers": []any{map[string]any{"name": "istio-proxy"}, map[string]any{"name": "app"}}}})
			return
		}
		query := request.URL.Query()
		mu.Lock()
		queries = append(queries, query)
		available := lines[:written]
		mu.Unlock()
		if !query.Has("container") {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"a container name must be specified for pod api"}`))
			return
		}
		// Like the kubelet, a resumed read starts at sinceTime, and the lines
		// keep the timestamps they were written with.
		var since time.Time
		if value := query.Get("sinceTime"); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				t.Errorf("sinceTime %q: %v", value, err)
			}
			since = parsed
		}
		for _, line := range available {
			if line.at.Before(since) {
				continue
			}
			fmt.Fprintf(writer, "%s %s\n", line.at.Format(time.RFC3339Nano), line.text)
		}
		writer.(http.Flusher).Flush()
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	// stream follows until the upstream connection closes and returns the
	// lines shown and the last cursor the viewer was given.
	stream := func(options PodLogStreamOptions) ([]string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(logStreamTestContext(), 30*time.Second)
		defer cancel()
		seen, cursor := []string{}, ""
		err := service.StreamPodLogs(ctx, options, func(event PodLogStreamEvent) error {
			if event.Cursor != "" {
				cursor = event.Cursor
			}
			if event.Event != "line" {
				return nil
			}
			if event.Container != "app" {
				t.Errorf("line %q names container %q", event.Text, event.Container)
			}
			seen = append(seen, event.Text)
			return nil
		})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		return seen, cursor
	}
	first, cursor := stream(PodLogStreamOptions{Namespace: "shop", Pod: "api"})
	if strings.Join(first, ",") != "line-1,line-2" || cursor == "" {
		t.Fatalf("first follow lines=%v cursor=%q", first, cursor)
	}
	mu.Lock()
	written = 3
	mu.Unlock()
	resume := PodLogStreamOptions{Namespace: "shop", Pod: "api", Cursor: cursor}
	if err := service.ValidatePodLogStream(resume); err != nil {
		t.Fatalf("container-less reconnect refused: %v", err)
	}
	second, _ := stream(resume)
	if strings.Join(second, ",") != "line-3" {
		t.Fatalf("reconnect lines=%v, want only line-3", second)
	}
	mu.Lock()
	defer mu.Unlock()
	followed := []url.Values{}
	for _, query := range queries {
		if query.Has("container") {
			followed = append(followed, query)
		}
	}
	if len(followed) != 2 {
		t.Fatalf("upstream log reads with a container = %d, want 2: %v", len(followed), queries)
	}
	for i, query := range followed {
		if query.Get("container") != "app" || query.Get("follow") != "true" || query.Get("previous") == "true" {
			t.Fatalf("read %d did not follow the resolved container live: %v", i, query)
		}
	}
	if followed[0].Get("sinceTime") != "" || followed[1].Get("sinceTime") == "" || followed[1].Has("tailLines") {
		t.Fatalf("reconnect did not resume from the cursor: first=%v second=%v", followed[0], followed[1])
	}
}

func TestPodLogStreamKeepsAnExplicitContainer(t *testing.T) {
	recorder, service := newLogContainerServer(t, http.StatusOK, "app", "istio-proxy", "app")
	events := runPodLogStream(t, service, PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "istio-proxy", Previous: true})
	podReads, containers, _ := recorder.snapshot()
	if podReads != 0 || len(containers) != 1 || containers[0] != "istio-proxy" {
		t.Fatalf("pod reads = %d, upstream containers = %v", podReads, containers)
	}
	for _, event := range events {
		if event.Event == "line" && event.Container != "istio-proxy" {
			t.Fatalf("line container = %q", event.Container)
		}
	}
}

func TestPodLogStreamKeepsTheRefusalWhenThePodIsUnreadable(t *testing.T) {
	recorder, service := newLogContainerServer(t, http.StatusForbidden, "", "istio-proxy", "app")
	err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api", Previous: true}, func(PodLogStreamEvent) error { return nil })
	if !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("err = %v, want the original refusal", err)
	}
	podReads, _, hasParam := recorder.snapshot()
	if podReads != 1 || len(hasParam) != 1 || hasParam[0] {
		t.Fatalf("pod reads = %d, upstream carried a container: %v", podReads, hasParam)
	}
}

// sidecarFirstPodRecord projects a Pod whose sidecar is listed before the
// application, the way an injector usually writes it, into an index record.
func sidecarFirstPodRecord(t *testing.T, uid, annotation string, containers ...string) kubeindex.Record {
	t.Helper()
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339Nano)
	spec, statuses := []any{}, []any{}
	for _, name := range containers {
		spec = append(spec, map[string]any{"name": name})
		statuses = append(statuses, map[string]any{"name": name, "ready": true, "state": map[string]any{"running": map[string]any{"startedAt": started}}})
	}
	metadata := map[string]any{"name": "api-" + uid, "namespace": "shop", "uid": uid}
	if annotation != "" {
		metadata["annotations"] = map[string]any{defaultContainerAnnotation: annotation}
	}
	raw := map[string]any{"kind": "Pod", "metadata": metadata, "spec": map[string]any{"containers": spec, "initContainers": []any{map[string]any{"name": "linkerd-init"}}}, "status": map[string]any{"containerStatuses": statuses}}
	record := indexRecord(NewService(nil, Scope{}, 0).projectResource(ResourceDefinition{Kind: "Pod"}, raw))
	record.Kind, record.UID, record.Name, record.Namespace = "Pod", uid, "api-"+uid, "shop"
	return record
}

// The Pod summary's main container follows the same rule as the live log read,
// so a drawer opened from a row and the stream it starts name the same
// container.
func TestPodSummaryMainContainerMatchesTheLogReadDefault(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		containers []string
		want       string
	}{
		{"sidecar listed first is skipped", "", []string{"linkerd-proxy", "app"}, "app"},
		{"annotation names the sidecar", "linkerd-proxy", []string{"linkerd-proxy", "app"}, "linkerd-proxy"},
		{"annotation names an init container", "linkerd-init", []string{"istio-proxy", "app"}, "app"},
		{"only sidecars", "", []string{"istio-proxy", "vault-agent"}, "istio-proxy"},
		{"single container", "", []string{"app"}, "app"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			record := sidecarFirstPodRecord(t, "pod", test.annotation, test.containers...)
			if record.MainContainer != test.want {
				t.Fatalf("main container = %q, want %q", record.MainContainer, test.want)
			}
			var pod logContainerPod
			pod.Metadata.Annotations = map[string]string{defaultContainerAnnotation: test.annotation}
			for _, name := range test.containers {
				pod.Spec.Containers = append(pod.Spec.Containers, logContainerName{Name: name})
			}
			if live := defaultLogContainer(pod); len(test.containers) > 1 && live != record.MainContainer {
				t.Fatalf("live read default %q differs from summary %q", live, record.MainContainer)
			}
		})
	}
}

// A workload stream opened without a container follows the application of each
// Pod, never an injected sidecar listed first, whether the index carries the
// main container or only the container list.
func TestWorkloadStreamDefaultSkipsSidecarListedFirst(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	records := workloadStreamRecords("Deployment", 0, 0)
	owner := kubeindex.OwnerRef{Kind: "ReplicaSet", UID: "rs", Name: "api-rs", Namespace: "shop", Controller: true, ControllerKnown: true}
	projected := sidecarFirstPodRecord(t, "pod-projected", "", "linkerd-proxy", "app")
	listed := sidecarFirstPodRecord(t, "pod-listed", "", "istio-proxy", "app")
	listed.MainContainer = ""
	for _, pod := range []kubeindex.Record{projected, listed} {
		pod.Owners = []kubeindex.OwnerRef{owner}
		records = append(records, pod)
	}
	seedWorkloadStreamIndex(t, service, records)
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if len(session.workers) != 2 {
		t.Fatalf("workers = %d, pods = %+v", len(session.workers), session.pods)
	}
	for _, worker := range session.workers {
		if worker.options.Container != "app" {
			t.Fatalf("workload default followed %q on %s", worker.options.Container, worker.source.Pod)
		}
	}
	for _, pod := range session.pods {
		if pod.MainContainer != "app" {
			t.Fatalf("pod %s main container = %q", pod.Name, pod.MainContainer)
		}
	}
}
