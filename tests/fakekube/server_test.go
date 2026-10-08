package fakekube

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type gatedLogWriter struct {
	*httptest.ResponseRecorder
	context context.Context
	writes  chan string
	release chan struct{}
	flushed chan struct{}
}

func (writer *gatedLogWriter) Write(body []byte) (int, error) {
	select {
	case writer.writes <- string(body):
	case <-writer.context.Done():
		return 0, writer.context.Err()
	}
	select {
	case <-writer.release:
		return writer.ResponseRecorder.Write(body)
	case <-writer.context.Done():
		return 0, writer.context.Err()
	}
}

func (writer *gatedLogWriter) Flush() {
	writer.ResponseRecorder.Flush()
	writer.flushed <- struct{}{}
}

func TestPodLogFixtureFramingBackpressureAndCancellation(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writer := &gatedLogWriter{ResponseRecorder: httptest.NewRecorder(), context: ctx, writes: make(chan string), release: make(chan struct{}), flushed: make(chan struct{}, 16)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0/log?container=sidecar&follow=true&timestamps=true", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); server.ServeHTTP(writer, request) }()
	var chunks []string
	for index := 0; index < 9; index++ {
		select {
		case chunk := <-writer.writes:
			chunks = append(chunks, chunk)
		case <-ctx.Done():
			t.Fatal("timed out waiting for fixture chunk")
		}
		if index == 0 {
			counters := server.logStreamCounters()
			if len(counters) != 1 || counters[0].Container != "sidecar" || !counters[0].Follow || counters[0].Active != 1 || counters[0].Chunks != 0 {
				t.Fatalf("blocked write advanced observer: %#v", counters)
			}
		}
		select {
		case writer.release <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("timed out releasing write")
		}
		select {
		case <-writer.flushed:
		case <-ctx.Done():
			t.Fatal("timed out waiting for flush")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fixture ignored cancellation")
	}
	lines := strings.Split(strings.TrimSuffix(writer.Body.String(), "\n"), "\n")
	if len(lines) != 6 || lines[0] != lines[1] || len(lines[4]) <= 16<<10 || !strings.Contains(lines[5], "fakekube follow line 1 container=sidecar") {
		t.Fatal("fixture lost oversized framing or following normal record")
	}
	wantOversized := "2026-01-01T00:00:00Z fakekube synthetic oversized password=synthetic-oversized-secret-value " + strings.Repeat("synthetic-record-padding ", 800) + " container=sidecar"
	if lines[4] != wantOversized || !strings.HasSuffix(chunks[6], "synthetic-oversized-sec") || !strings.HasPrefix(chunks[7], "ret-value ") || strings.Contains(chunks[6], "synthetic-oversized-secret-value") || strings.Contains(chunks[7], "synthetic-oversized-secret-value") {
		t.Fatal("oversized record is not the deterministic generic synthetic split canary")
	}
	counters := server.logStreamCounters()
	if len(counters) != 1 || counters[0].Requests != 1 || counters[0].Active != 0 || counters[0].Cancelled != 1 || counters[0].Completed != 0 || counters[0].Disconnected != 0 || counters[0].Chunks != 9 {
		t.Fatalf("framing/cancellation counters: %#v", counters)
	}
}

func TestPodLogFixtureContainersAndPreviousAvailability(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/namespaces/payments/pods/checkout-api-0/log"
	for _, container := range []string{"api", "sidecar", "setup", "debug"} {
		for _, previous := range []bool{false, true} {
			response := httptest.NewRecorder()
			query := "?container=" + container
			if previous {
				query += "&previous=true&follow=false&timestamps=true"
			}
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+query, nil))
			unavailable := previous && (container == "setup" || container == "debug")
			if unavailable {
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "previous terminated container instance unavailable") {
					t.Fatalf("previous availability container=%s status=%d", container, response.Code)
				}
			} else if response.Code != http.StatusOK || container != "api" && !strings.Contains(response.Body.String(), "container="+container) || previous && !strings.Contains(response.Body.String(), "fakekube previous") {
				t.Fatalf("selected log container=%s previous=%v status=%d", container, previous, response.Code)
			}
		}
	}
	for _, counter := range server.logStreamCounters() {
		if counter.Active != 0 || counter.Requests != 1 || counter.Cancelled != 0 || counter.Previous && (counter.Container == "setup" || counter.Container == "debug") && counter.Unavailable != 1 || (!counter.Previous || counter.Container == "api" || counter.Container == "sidecar") && counter.Completed != 1 {
			t.Fatalf("finite observer: %#v", counter)
		}
	}
	for _, query := range []string{"?container=missing", "?container=missing&timestamps=true&follow=true"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unknown container status=%d", response.Code)
		}
	}
	if err := server.store.Upsert("pods", "payments", "metadata-only", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/metadata-only/log", nil))
	if response.Code != http.StatusOK || !strings.HasSuffix(response.Body.String(), " fakekube log line\n") {
		t.Fatal("metadata-only finite compatibility changed")
	}
}

func TestPodLogFixtureControlledDisconnectReconnectAndPrevious(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	path := "/api/v1/namespaces/payments/pods/checkout-api-0/log"
	control := func(body string, code int) {
		t.Helper()
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/_fake/log-streams", strings.NewReader(body)))
		if response.Code != code {
			t.Fatalf("log control status=%d want=%d", response.Code, code)
		}
	}
	control(`{"path":"`+path+`","container":"sidecar","disconnect_after_chunks":3,"remaining":1}`, http.StatusOK)
	finite := httptest.NewRecorder()
	server.ServeHTTP(finite, httptest.NewRequest(http.MethodGet, path+"?container=sidecar", nil))
	if finite.Code != http.StatusOK {
		t.Fatal("finite read consumed follow fault")
	}
	response, err := httpServer.Client().Get(httpServer.URL + path + "?container=sidecar&follow=true&timestamps=true")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("controlled disconnect did not abort transport: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+path+"?container=sidecar&follow=true&timestamps=true&sinceTime=2026-01-01T00:00:01Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "fakekube follow line 1 container=sidecar") {
		t.Fatalf("reconnect normal record err=%v", err)
	}
	cancel()
	response.Body.Close()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		settled := false
		for _, counter := range server.logStreamCounters() {
			if counter.Container == "sidecar" && counter.Follow {
				settled = counter.Requests == 2 && counter.Active == 0 && counter.Disconnected == 1 && counter.Cancelled == 1 && counter.Completed == 0 && counter.Chunks >= 4 && counter.SinceTime == "2026-01-01T00:00:01Z"
			}
		}
		if settled {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("disconnect/reconnect counters: %#v", server.logStreamCounters())
		case <-ticker.C:
		}
	}
	control(`{"path":"`+path+`","container":"sidecar","previous_available":false}`, http.StatusOK)
	previous := httptest.NewRecorder()
	server.ServeHTTP(previous, httptest.NewRequest(http.MethodGet, path+"?container=sidecar&previous=true&follow=false&timestamps=true", nil))
	if previous.Code != http.StatusBadRequest {
		t.Fatalf("controlled previous status=%d", previous.Code)
	}
	control(`{"path":"`+path+`","container":"sidecar"}`, http.StatusOK)
	previous = httptest.NewRecorder()
	server.ServeHTTP(previous, httptest.NewRequest(http.MethodGet, path+"?container=sidecar&previous=true&follow=false&timestamps=true", nil))
	if previous.Code != http.StatusOK || !strings.Contains(previous.Body.String(), "fakekube previous") {
		t.Fatal("reset did not restore metadata-backed previous logs")
	}
	for _, body := range []string{`{}`, `{"path":"` + path + `","container":"missing"}`, `{"path":"` + path + `","disconnect_after_chunks":0,"remaining":1}`, `{"path":"` + path + `","disconnect_after_chunks":1025,"remaining":1}`, `{"path":"` + path + `","disconnect_after_chunks":1,"remaining":101}`, `{"path":"` + path + `","unknown":true}`, `{"path":"` + path + `"} {}`} {
		code := http.StatusBadRequest
		if body == `{}` {
			code = http.StatusNotFound
		}
		control(body, code)
	}
}

func TestTimestampedPodLogFollowResumePreviousAndCancellation(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "fakekube"}, time.Minute)
	redactor, errs := agent.NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	service.SetScrubber(redactor)
	authorized := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
	})
	options := kubernetes.PodLogStreamOptions{Namespace: "payments", Pod: "checkout-api-0", Container: "api", Timestamps: true}
	collectFollow := func(options kubernetes.PodLogStreamOptions, count int) []kubernetes.PodLogStreamEvent {
		t.Helper()
		ctx, cancel := context.WithTimeout(authorized, 12*time.Second)
		defer cancel()
		var events []kubernetes.PodLogStreamEvent
		err := service.StreamPodLogs(ctx, options, func(event kubernetes.PodLogStreamEvent) error {
			if event.Event == "line" {
				events = append(events, event)
				if len(events) == count {
					cancel()
					return context.Canceled
				}
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || len(events) != count {
			t.Fatalf("follow events=%#v err=%v", events, err)
		}
		return events
	}
	events := collectFollow(options, 33)
	if events[0].Text != events[1].Text || events[0].Timestamp != events[1].Timestamp || events[0].Ordinal != 1 || events[1].Ordinal != 2 {
		t.Fatalf("identical timestamped lines were lost: %#v", events[:2])
	}
	for _, event := range events {
		if (event.Cursor != "") != (event.Sequence%32 == 0) || event.Container != "api" || event.Timestamp == "" {
			t.Fatalf("missing stream metadata: %#v", event)
		}
		for _, canary := range []string{"super-secret-value", "split-secret-value", "previous-secret-value", "synthetic-oversized-secret-value", "synthetic-record-padding"} {
			if strings.Contains(event.Text, canary) {
				t.Fatalf("split-chunk secret leaked: %#v", event)
			}
		}
	}
	if !strings.Contains(events[2].Text, "<REDACTED:") || !strings.Contains(events[3].Text, "<REDACTED:") || !strings.Contains(events[4].Text, "[oversized log line omitted]") || !strings.Contains(events[5].Text, "fakekube follow line 1") {
		t.Fatalf("missing scrubbed fixtures or ongoing follow: %#v", events)
	}
	options.Cursor = events[31].Cursor
	resumed := collectFollow(options, 3)
	for index, event := range resumed {
		if event.Ordinal != 1 || event.Timestamp != time.Date(2026, 1, 1, 0, 0, 28+index, 0, time.UTC).Format(time.RFC3339Nano) || event.ReplayUncertain {
			t.Fatalf("resume lost same-timestamp occurrence: %#v", resumed)
		}
	}
	if resumed[0].Text != events[32].Text || resumed[0].Timestamp != events[32].Timestamp || resumed[0].Ordinal != events[32].Ordinal {
		t.Fatal("checkpoint overlap changed occurrence identity")
	}
	options.Cursor = ""
	options.Previous = true
	ctx, cancel := context.WithTimeout(authorized, 3*time.Second)
	defer cancel()
	var previous []kubernetes.PodLogStreamEvent
	if err := service.StreamPodLogs(ctx, options, func(event kubernetes.PodLogStreamEvent) error {
		previous = append(previous, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(previous) != 2 || previous[0].Event != "line" || !strings.Contains(previous[0].Text, "fakekube previous") || !strings.Contains(previous[0].Text, "<REDACTED:") || strings.Contains(previous[0].Text, "previous-secret-value") || previous[1].Event != "end" || previous[1].Reason != "complete" {
		t.Fatalf("previous did not scrub and finish: %#v", previous)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := httpServer.Client().Get(httpServer.URL + "/_fake/log-streams")
		if err != nil {
			t.Fatal(err)
		}
		var counters []LogStreamCounter
		err = json.NewDecoder(response.Body).Decode(&counters)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("stream observation status=%d err=%v", response.StatusCode, err)
		}
		if len(counters) == 2 && counters[0].Requests == 2 && counters[0].Active == 0 && counters[0].Cancelled == 2 && counters[0].Chunks >= 13 && counters[0].SinceTime == "2025-12-31T23:59:59.999999999Z" && counters[1].Previous && counters[1].Requests == 1 && counters[1].Completed == 1 && counters[1].Active == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("upstream cancellation/completion not observed: %#v", counters)
		case <-ticker.C:
		}
	}
}

func TestTimestampedPodLogFixtureGuardsAndFiniteCompatibility(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "previous=true", "follow=true", "timestamps=true", "timestamps=true&previous=true"} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0/log?"+query, nil))
		if response.Code != http.StatusOK || response.Body.String() != "2026-01-01T00:00:00Z fakekube checkout api token=super-secret-value\n" {
			t.Fatalf("finite compatibility query=%q status=%d body=%q", query, response.Code, response.Body.String())
		}
	}
	for _, test := range []struct {
		path string
		code int
	}{
		{"/api/v1/namespaces/payments/pods/missing/log?follow=true&timestamps=true", http.StatusNotFound},
		{"/api/v1/namespaces/payments/pods/checkout-api-0/log?follow=true&timestamps=true&sinceTime=invalid", http.StatusBadRequest},
		{"/api/v1/namespaces/payments/pods/checkout-api-0/log?previous=true&follow=false&timestamps=true&sinceTime=2026-01-02T00:00:00Z", http.StatusOK},
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code || test.code == http.StatusOK && response.Body.Len() != 0 {
			t.Fatalf("guard path=%q status=%d body=%q", test.path, response.Code, response.Body.String())
		}
	}
}

func TestTriageFixtureSafeProjectionAndLogs(t *testing.T) {
	for _, scenario := range []string{"triage", "fault-forbidden-secret", "fault-transient-pods", "fault-partial-pods"} {
		t.Run(scenario, func(t *testing.T) {
			server, err := NewServer(Config{Scenario: scenario, Seed: 1})
			if err != nil {
				t.Fatal(err)
			}
			for resource, count := range map[string]int{"pods": 1, "nodes": 1, "secrets": 1, "configmaps": 1} {
				if got := server.store.Count(resource); got != count {
					t.Fatalf("%s count=%d, want %d", resource, got, count)
				}
			}
			for _, fixture := range []struct {
				resource string
				name     string
				canary   string
			}{
				{"pods", "checkout-api-0", "secret-token"},
				{"secrets", "api-secret", "c2VjcmV0LXRva2Vu"},
				{"configmaps", "api-config", "must-not-cross"},
			} {
				raw, exists := server.store.Get(fixture.resource, "payments", fixture.name)
				if !exists || !strings.Contains(string(raw), fixture.canary) {
					t.Fatalf("%s/%s missing raw canary", fixture.resource, fixture.name)
				}
			}
			httpServer := httptest.NewServer(server)
			defer httpServer.Close()
			client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "fakekube"}, time.Minute)
			redactor, errs := agent.NewRedactor(false, nil)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			service.SetScrubber(redactor)
			if scenario == "fault-transient-pods" {
				transient := httptest.NewRecorder()
				server.ServeHTTP(transient, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0", nil))
				if transient.Code != http.StatusServiceUnavailable {
					t.Fatalf("transient pod status=%d", transient.Code)
				}
			}
			for _, fixture := range []struct {
				resource string
				name     string
				key      string
			}{
				{"pods", "checkout-api-0", "api"},
				{"secrets", "api-secret", "token"},
				{"configmaps", "api-config", "config.yaml"},
			} {
				projected, err := service.Get(context.Background(), "core~v1~"+fixture.resource, "payments", fixture.name)
				if scenario == "fault-forbidden-secret" && fixture.resource == "secrets" {
					if err != kubernetes.ErrForbidden {
						t.Fatalf("secret denial=%v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(projected)
				if err != nil || len(encoded) > 4096 || !strings.Contains(string(encoded), fixture.key) {
					t.Fatalf("%s projection bytes=%d missing key=%q err=%v", fixture.resource, len(encoded), fixture.key, err)
				}
				for _, canary := range []string{"secret-token", "c2VjcmV0LXRva2Vu", "must-not-cross"} {
					if strings.Contains(string(encoded), canary) {
						t.Fatalf("%s projection leaked %q", fixture.resource, canary)
					}
				}
				if fixture.resource == "pods" {
					for _, identity := range []string{"sidecar", "setup", "debug", "regular", "init", "ephemeral"} {
						if !strings.Contains(string(encoded), identity) {
							t.Fatalf("missing safe container identity %q", identity)
						}
					}
				}
			}
			for _, container := range []string{"", "api"} {
				raw := httptest.NewRecorder()
				server.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods/checkout-api-0/log?container="+container+"&tailLines=20", nil))
				if raw.Code != http.StatusOK || raw.Body.String() != "2026-01-01T00:00:00Z fakekube checkout api token=super-secret-value\n" {
					t.Fatalf("raw log status=%d text=%q", raw.Code, raw.Body.String())
				}
				logs, err := service.PodLogs(context.Background(), "payments", "checkout-api-0", container, false, 60, 20)
				if err != nil || logs.Truncated || logs.TailLines != 20 || len(logs.Text) > 4096 || !strings.Contains(logs.Text, "<REDACTED:password>") || strings.Contains(logs.Text, "super-secret-value") {
					t.Fatalf("projected logs=%#v err=%v", logs, err)
				}
			}
		})
	}
}

func TestServerResourcePathRouting(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		path      string
		resource  string
		namespace string
		item      bool
		kind      string
	}{
		{"/api/v1/namespaces", "namespaces", "", false, "List"},
		{"/api/v1/namespaces/payments", "namespaces", "", true, "Namespace"},
		{"/api/v1/nodes/node-a", "nodes", "", true, "Node"},
		{"/api/v1/namespaces/payments/pods", "pods", "payments", false, "List"},
		{"/api/v1/namespaces/payments/pods/checkout-api-0", "pods", "payments", true, "Pod"},
		{"/apis/apps/v1/namespaces/payments/deployments", "deployments", "payments", false, "List"},
		{"/apis/apps/v1/namespaces/payments/deployments/checkout-api", "deployments", "payments", true, "Deployment"},
	} {
		t.Run(fixture.path, func(t *testing.T) {
			if resourceForPath(fixture.path) != fixture.resource || namespaceForPath(fixture.path) != fixture.namespace || isItemPath(fixture.path) != fixture.item {
				t.Errorf("routing resource=%q namespace=%q item=%t", resourceForPath(fixture.path), namespaceForPath(fixture.path), isItemPath(fixture.path))
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fixture.path, nil))
			var body struct {
				Kind string `json:"kind"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Kind != fixture.kind {
				t.Fatalf("status=%d body=%s, want kind=%s", response.Code, response.Body.String(), fixture.kind)
			}
		})
	}
}

func TestServerDiscoveryMetadataAndFaultCounters(t *testing.T) {
	server, err := NewServer(Config{Scenario: "helm", Faults: map[string]Fault{"pods": {StatusCode: http.StatusTooManyRequests, Remaining: 1, RetryAfter: 2 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := httpServer.Client()

	response, err := client.Get(httpServer.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("discovery status=%d", response.StatusCode)
	}

	response, err = client.Get(httpServer.URL + "/api/v1/namespaces/shop/pods")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "2" {
		t.Fatalf("injected response status=%d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/namespaces/shop/secrets?labelSelector=owner%3Dhelm", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), "secret-canary") || strings.Contains(string(body), "release\"") {
		t.Fatalf("metadata response status=%d body=%s", response.StatusCode, body)
	}
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Kind != "PartialObjectMetadataList" || len(list.Items) != 2 {
		t.Fatalf("metadata list=%#v err=%v", list, err)
	}

	response, err = client.Get(httpServer.URL + "/_fake/counters")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var counters []Counter
	if err := json.NewDecoder(response.Body).Decode(&counters); err != nil {
		t.Fatal(err)
	}
	foundMetadata := false
	for _, counter := range counters {
		if counter.Path == "/api/v1/namespaces/shop/secrets" && strings.Contains(counter.Accept, "PartialObjectMetadataList") {
			foundMetadata = counter.Requests == 1 && counter.Bytes > 0
		}
	}
	if !foundMetadata {
		t.Fatalf("metadata request missing from counters: %#v", counters)
	}
}

func TestGenerateFiftyThousandPodsWithinBounds(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	store := NewStore()
	if err := Generate(store, 50000, 2000, 23); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if count := store.Count("pods"); count != 50000 {
		t.Fatalf("generated pod count=%d", count)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("50k pod generation took %s, want less than 5s", elapsed)
	}
	if after.HeapAlloc < before.HeapAlloc || after.HeapAlloc-before.HeapAlloc >= 500<<20 {
		t.Fatalf("50k pod fixture retained %d bytes, want less than 500 MiB", after.HeapAlloc-before.HeapAlloc)
	}
}

func TestPopulatedScenarioTimelineMutationBoundary(t *testing.T) {
	server, err := NewServer(Config{Scenario: "populated", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "boundary", ClusterID: "fakekube", CredentialID: "boundary"}, time.Minute)
	service.SetChangeStorage(storage.NewMemory())
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	mutate := func(generation, replicas int, image string) string {
		t.Helper()
		body, marshalErr := json.Marshal(map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"uid": "deployment-checkout-shop", "generation": generation, "labels": map[string]string{"app": "checkout"}},
			"spec":     map[string]any{"replicas": replicas, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": image}}}}},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if upsertErr := server.store.Upsert("deployments", "shop", "checkout", body); upsertErr != nil {
			t.Fatal(upsertErr)
		}
		raw, exists := server.store.Get("deployments", "shop", "checkout")
		if !exists {
			t.Fatal("mutated deployment missing from fake store")
		}
		var object struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if decodeErr := json.Unmarshal(raw, &object); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		t.Logf("raw deployment: %s", raw)
		return object.Metadata.ResourceVersion
	}
	oldRV := mutate(1, 1, "example.invalid/checkout:baseline")
	baseline, status, err := service.IndexSnapshot(ctx, "Deployment")
	if err != nil || status.State != "ready" || status.Partial {
		t.Fatalf("baseline status=%+v err=%v", status, err)
	}
	newRV := mutate(2, 3, "example.invalid/checkout:v2")
	if oldRV == newRV {
		t.Fatalf("fake mutation did not increment RV: %s", oldRV)
	}
	freshClient, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	freshService := kubernetes.NewService(freshClient, kubernetes.Scope{ClusterID: "fresh-boundary"}, time.Minute)
	updated, refreshed, err := freshService.IndexSnapshot(ctx, "Deployment")
	if err != nil || refreshed.State != "ready" || refreshed.Partial {
		t.Fatalf("updated status=%+v err=%v", refreshed, err)
	}
	checkout := func(snapshot kubeindex.Snapshot) kubeindex.Record {
		t.Helper()
		for _, record := range snapshot.Records {
			if record.Namespace == "shop" && record.Name == "checkout" {
				return record
			}
		}
		t.Fatal("checkout missing from projected index")
		return kubeindex.Record{}
	}
	oldRecord, newRecord := checkout(baseline), checkout(updated)
	t.Logf("raw RV %s -> %s; old indexed=%+v; new indexed=%+v", oldRV, newRV, oldRecord, newRecord)
	change, detected := kubechanges.Detect("fakekube", kubeindex.Delta{Kind: "Deployment", Op: "upsert", Old: &oldRecord, New: &newRecord}, time.Now().UTC())
	t.Logf("direct detection: detected=%v change=%+v", detected, change)
	if len(oldRecord.Images) != 1 || len(newRecord.Images) != 1 || oldRecord.Replicas != 1 || newRecord.Replicas != 3 || !detected || change.Type != kubechanges.ImageChanged || len(change.Fields) != 2 {
		t.Fatal("raw mutation reached index but image/replica detection contract failed; see boundary evidence")
	}
}

func TestPopulatedScenarioTimelineSeed(t *testing.T) {
	server, err := NewServer(Config{Scenario: "populated", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "harness", ClusterID: "fakekube", CredentialID: "harness-fakekube"}, time.Minute)
	service.SetChangeStorage(storage.NewMemory())
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	query := kubernetes.ChangeQuery{Namespace: "shop", Kind: "Deployment", Name: "checkout"}
	baselineTicker := time.NewTicker(100 * time.Millisecond)
	defer baselineTicker.Stop()
	for {
		_, status, err := service.IndexSnapshot(ctx, "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" && !status.Partial && len(status.Kinds) == 6 {
			break
		}
		select {
		case <-baselineTicker.C:
		case <-ctx.Done():
			t.Fatalf("timeline baseline never became ready: %+v", status)
		}
	}
	baseline, err := service.Changes(ctx, query)
	if err != nil || len(baseline.Items) != 0 {
		t.Fatalf("timeline baseline=%+v err=%v", baseline, err)
	}
	t.Log("timeline baseline ready for all six resource kinds")
	wanted := []string{"created", "image_changed", "replicas_changed"}
	for revisionIndex, image := range []string{"example.invalid/checkout:v1", "example.invalid/checkout:v2", "example.invalid/checkout:v2"} {
		revision, err := json.Marshal(map[string]any{
			"operation": "upsert", "resource": "deployments", "namespace": "shop", "name": "checkout",
			"object": map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"uid": "deployment-checkout-shop", "generation": revisionIndex + 1, "labels": map[string]string{"app": "checkout", "app.kubernetes.io/name": "checkout"}},
				"spec":     map[string]any{"replicas": revisionIndex + 2, "selector": map[string]any{"matchLabels": map[string]string{"app": "checkout"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"app": "checkout"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": image}}}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(string(revision))))
		if response.Code != http.StatusOK {
			t.Fatalf("revision %d mutation status=%d", revisionIndex, response.Code)
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		revisionContext, cancelRevision := context.WithTimeout(ctx, 20*time.Second)
		found := false
		for !found {
			page, err := service.Changes(revisionContext, query)
			if err != nil {
				ticker.Stop()
				cancelRevision()
				t.Fatalf("revision %d timeline poll: %v", revisionIndex, err)
			}
			for _, change := range page.Items {
				if string(change.Type) != wanted[revisionIndex] {
					continue
				}
				if change.Service != "checkout" || time.Since(change.At) > 2*time.Minute {
					t.Fatalf("timeline service/freshness mismatch: %+v", change)
				}
				if revisionIndex == 1 && (len(change.Fields) != 2 || change.Fields[0].Path != "containers.images" || change.Fields[0].From != "example.invalid/checkout:v1" || change.Fields[0].To != "example.invalid/checkout:v2") {
					t.Fatalf("image and replica fields missing: %+v", change)
				}
				if revisionIndex == 2 && (len(change.Fields) != 1 || change.Fields[0].Path != "spec.replicas" || change.Fields[0].From != "3" || change.Fields[0].To != "4") {
					t.Fatalf("replica fields missing: %+v", change)
				}
				found = true
			}
			if !found {
				select {
				case <-ticker.C:
				case <-revisionContext.Done():
					ticker.Stop()
					cancelRevision()
					t.Fatalf("revision %d never reached the timeline: sync=%+v partial=%+v items=%+v", revisionIndex, page.Sync, page.Partial, page.Items)
				}
			}
		}
		ticker.Stop()
		cancelRevision()
		t.Logf("revision %d observed: %s", revisionIndex, wanted[revisionIndex])
	}
}

func TestPopulatedScenarioTimelineDistinctCreates(t *testing.T) {
	server, err := NewServer(Config{Scenario: "populated", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "harness", ClusterID: "fakekube", CredentialID: "distinct-creates"}, time.Minute)
	service.SetChangeStorage(storage.NewMemory())
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	query := kubernetes.ChangeQuery{Namespace: "shop", Kind: "Deployment"}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, status, err := service.IndexSnapshot(ctx, "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" && !status.Partial && len(status.Kinds) == 6 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("create baseline not ready: %+v", status)
		}
	}
	baseline, err := service.Changes(ctx, query)
	if err != nil || len(baseline.Items) != 0 {
		t.Fatalf("create baseline=%+v err=%v", baseline, err)
	}
	names := []string{"checkout-api", "checkout-web", "checkout-worker"}
	for _, name := range names {
		if _, exists := server.store.Get("deployments", "shop", name); exists {
			t.Fatalf("%s already exists at baseline", name)
		}
	}
	started := time.Now().UTC()
	for fixtureIndex, name := range names {
		replicas := fixtureIndex + 2
		labels := map[string]string{"app": name, "app.kubernetes.io/name": name}
		mutation, err := json.Marshal(map[string]any{
			"operation": "upsert", "resource": "deployments", "namespace": "shop", "name": name,
			"object": map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": name, "namespace": "shop", "uid": "deployment-" + name + "-shop", "generation": 1, "creationTimestamp": time.Now().UTC().Format(time.RFC3339), "labels": labels},
				"spec":     map[string]any{"replicas": replicas, "selector": map[string]any{"matchLabels": map[string]string{"app": name}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]string{"app": name}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "example.invalid/" + name + ":v1"}}}}},
				"status":   map[string]any{"replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas, "observedGeneration": 1},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(string(mutation))).WithContext(ctx))
		if response.Code != http.StatusOK {
			t.Fatalf("create %s status=%d", name, response.Code)
		}
		for {
			page, err := service.Changes(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			visible := make(map[string]bool)
			for _, change := range page.Items {
				if change.Type == kubechanges.Created && change.Namespace == "shop" && change.Kind == "Deployment" {
					if change.Service != change.Name || change.UID != "deployment-"+change.Name+"-shop" || change.At.Before(started) || time.Since(change.At) > time.Minute {
						t.Fatalf("create identity/freshness mismatch: %+v", change)
					}
					visible[change.Name] = true
				}
			}
			if visible[name] {
				for _, prior := range names[:fixtureIndex+1] {
					if !visible[prior] {
						t.Fatalf("previous create %s missing: %+v", prior, page.Items)
					}
				}
				t.Logf("observed real created transition: shop/%s", name)
				break
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("create %s not visible: sync=%+v items=%+v", name, page.Sync, page.Items)
			}
		}
	}
}

func TestPopulatedScenarioTrafficMetricEndpoint(t *testing.T) {
	for _, scenario := range []string{"populated", "triage"} {
		server, err := NewServer(Config{Scenario: scenario})
		if err != nil {
			t.Fatal(err)
		}
		server.startedAt = time.Now().Add(-30 * time.Second)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_fake/traffic-metrics", nil))
		if scenario == "triage" {
			if response.Code != http.StatusNotFound {
				t.Fatal("traffic generator exposed outside populated scenario")
			}
			continue
		}
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4" || !strings.Contains(response.Body.String(), "istio_requests_total{") || !strings.Contains(response.Body.String(), "istio_request_duration_milliseconds_bucket{") {
			t.Fatal("traffic fixture endpoint did not expose Prometheus counters")
		}
	}
}

func TestPopulatedScenarioServiceInventories(t *testing.T) {
	server, err := NewServer(Config{Scenario: "populated", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "fakekube"}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	releases, err := service.Releases(ctx, kubernetes.HelmReleaseOptions{})
	if err != nil || len(releases.Items) != 1 || releases.Items[0].Current.Revision != 2 || releases.Items[0].Current.Status != "deployed" || len(releases.Items[0].History) != 2 {
		t.Fatalf("releases=%+v err=%v", releases, err)
	}
	history, err := service.Release(ctx, "shop", "checkout")
	if err != nil || len(history.History) != 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	apps, err := service.GitOpsApps(ctx, kubernetes.GitOpsAppOptions{})
	if err != nil || !apps.Available || len(apps.Items) != 2 {
		t.Fatalf("apps=%+v err=%v", apps, err)
	}
	for _, app := range apps.Items {
		if app.Health == "" || app.Sync == "" || app.Revision == "" {
			t.Fatalf("GitOps status not projected: %+v", app)
		}
	}
	rollouts, err := service.Rollouts(ctx, kubernetes.RolloutListOptions{})
	if err != nil || !rollouts.Available || len(rollouts.Items) != 1 {
		t.Fatalf("rollouts=%+v err=%v", rollouts, err)
	}
	rollout := rollouts.Items[0]
	if rollout.Strategy != "canary" || rollout.Step != 1 || rollout.TotalSteps != 2 || rollout.Weight != 50 || rollout.Phase != "Progressing" || rollout.StableRS != "checkout-stable" || rollout.CanaryRS != "checkout-canary" {
		t.Fatalf("rollout progress not projected: %+v", rollout)
	}
}

func TestDiscoveryOnlyAdvertisesSeededGitOpsResources(t *testing.T) {
	for _, test := range []struct {
		scenario string
		wantCRD  bool
	}{{"triage", false}, {"gitops", true}, {"populated", true}} {
		server, err := NewServer(Config{Scenario: test.scenario})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apis", nil))
		containsCRD := strings.Contains(response.Body.String(), "argoproj.io")
		if containsCRD != test.wantCRD {
			t.Errorf("scenario %q CRD discovery=%v, want %v", test.scenario, containsCRD, test.wantCRD)
		}
		if test.wantCRD {
			response = httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/apis/argoproj.io/v1alpha1", nil))
			if !strings.Contains(response.Body.String(), `"name":"applications"`) || !strings.Contains(response.Body.String(), `"name":"rollouts"`) {
				t.Fatalf("GitOps discovery response=%s", response.Body.String())
			}
		}
	}
}

func TestStaleContinuationReturnsGone(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.Upsert("pods", "payments", "checkout-api-1", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	server.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods?limit=1", nil))
	var page struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || page.Metadata.Continue == "" {
		t.Fatalf("first list=%s err=%v", first.Body.String(), err)
	}
	if err := server.store.Upsert("pods", "payments", "checkout-api-2", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	continued := httptest.NewRecorder()
	server.ServeHTTP(continued, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods?limit=1&continue="+page.Metadata.Continue, nil))
	if continued.Code != http.StatusGone {
		t.Fatalf("stale continuation status=%d body=%s", continued.Code, continued.Body.String())
	}
}

func TestWatchBookmarkExpiredVersionAndLimitedRBAC(t *testing.T) {
	server, err := NewServer(Config{Scenario: "limited-rbac"})
	if err != nil {
		t.Fatal(err)
	}
	bookmark := httptest.NewRecorder()
	server.ServeHTTP(bookmark, httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=1&allowWatchBookmarks=true", nil))
	var event struct {
		Type string `json:"type"`
	}
	if bookmark.Code != http.StatusOK || json.Unmarshal(bookmark.Body.Bytes(), &event) != nil || event.Type != "BOOKMARK" {
		t.Fatalf("bookmark status=%d body=%s", bookmark.Code, bookmark.Body.String())
	}
	expired := httptest.NewRecorder()
	server.ServeHTTP(expired, httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=1&resourceVersion=expired", nil))
	if expired.Code != http.StatusGone {
		t.Fatalf("expired watch status=%d", expired.Code)
	}
	for _, path := range []string{"/api/v1/nodes", "/api/v1/namespaces/shop/secrets"} {
		denied := httptest.NewRecorder()
		server.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, path, nil))
		if denied.Code != http.StatusForbidden {
			t.Errorf("limited-RBAC path %s status=%d", path, denied.Code)
		}
	}
}

func TestScaleAndFaultScenariosAreDeterministic(t *testing.T) {
	scale, err := NewServer(Config{Scenario: "scale", Pods: 501, Namespaces: 2, Seed: 23})
	if err != nil || scale.store.Count("pods") != 501 || scale.store.Count("namespaces") != 2 {
		t.Fatalf("scale fixture pods=%d namespaces=%d err=%v", scale.store.Count("pods"), scale.store.Count("namespaces"), err)
	}

	partial, err := NewServer(Config{Scenario: "fault-partial-pods", Pods: 501, Namespaces: 1, Seed: 23})
	if err != nil {
		t.Fatal(err)
	}
	first := httptest.NewRecorder()
	partial.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/load-0000/pods?limit=500", nil))
	var page struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil || page.Metadata.Continue == "" {
		t.Fatalf("first scale page status=%d body=%s", first.Code, first.Body.String())
	}
	continued := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/load-0000/pods?limit=500&continue="+page.Metadata.Continue, nil)
	partial.ServeHTTP(continued, request)
	if continued.Code != http.StatusServiceUnavailable {
		t.Fatalf("partial-page fault status=%d body=%s", continued.Code, continued.Body.String())
	}

	forbidden, err := NewServer(Config{Scenario: "fault-forbidden-secret"})
	if err != nil {
		t.Fatal(err)
	}
	secretRequest := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/secrets", nil)
	secretRequest.Header.Set("Accept", "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	secretResponse := httptest.NewRecorder()
	forbidden.ServeHTTP(secretResponse, secretRequest)
	if secretResponse.Code != http.StatusForbidden {
		t.Fatalf("forbidden metadata scenario status=%d", secretResponse.Code)
	}
}

func TestTableAcceptReturnsKubernetesTable(t *testing.T) {
	server, err := NewServer(Config{Scenario: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/payments/pods", nil)
	request.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"kind":"Table"`) {
		t.Fatalf("table response status=%d body=%s", response.Code, response.Body.String())
	}
}
