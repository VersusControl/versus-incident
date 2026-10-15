//go:build integration

package fakekube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestMultiClusterFleetFixturesThroughSharedRegistry(t *testing.T) {
	entries := []kubernetes.ClusterEntry{}
	provider := storage.NewMemory()
	for _, id := range []string{"east", "west"} {
		server, err := NewServer(Config{Scenario: "fleet-" + id, Seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		httpServer := httptest.NewServer(server)
		defer httpServer.Close()
		client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: httpServer.URL, AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: id, CredentialID: "fixture-" + id}, 0)
		service.SetChangeStorage(provider)
		entries = append(entries, kubernetes.ClusterEntry{Info: kubernetes.ClusterInfo{ID: id, Provider: "generic"}, Service: service})
	}
	entries = append(entries, kubernetes.ClusterEntry{Info: kubernetes.ClusterInfo{ID: "unreachable", Provider: "generic"}, Err: errors.New("unreachable-fixture")})
	registry := kubernetes.NewClusterRegistry(true, entries)
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	summaries := registry.Summaries(ctx, "default", nil)
	if len(summaries) != 3 || summaries[0].Health != "healthy" || summaries[1].Health != "healthy" || summaries[2].Health != "unreachable" || summaries[0].Nodes != 1 || summaries[1].Nodes != 2 || summaries[0].Pods != 2 || summaries[1].Pods != 3 {
		t.Fatalf("fleet summaries: %+v", summaries)
	}
	for _, id := range []string{"east", "west"} {
		service, err := registry.ResolveCluster("default", id)
		if err != nil {
			t.Fatal(err)
		}
		page, err := service.List(ctx, kubernetes.ListOptions{ResourceID: "core~v1~pods"})
		if err != nil {
			t.Fatal(err)
		}
		for _, pod := range page.Items {
			if !strings.HasPrefix(pod.Name, id+"-") {
				t.Fatal("cluster inventory crossed")
			}
		}
		pod := page.Items[0]
		logs, err := service.PodLogs(ctx, pod.Namespace, pod.Name, "app", false, 0, 10)
		if err != nil || logs.ClusterID != id {
			t.Fatalf("cluster log identity: %s %v", id, err)
		}
	}
}

func TestBadRolloutBackendContract(t *testing.T) {
	server, err := NewServer(Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(server)
	defer upstream.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	page, err := service.Issues(t.Context(), kubernetes.IssueOptions{Broken: true, IncludeChanges: true, Limit: 2})
	if err != nil || page.Broken.Total != 3 || len(page.Items) != 2 || page.Next == "" {
		t.Fatalf("page totals: %+v %v", page, err)
	}
	root, err := service.Issues(t.Context(), kubernetes.IssueOptions{RootUID: "deployment-checkout", IncludeChanges: true, Namespace: "shop"})
	if err != nil || len(root.Items) != 1 {
		t.Fatalf("root query: %+v %v", root, err)
	}
	checkout := root.Items[0]
	if checkout.Root.Kind != "Deployment" || checkout.Rule != "pod.crash_loop" || checkout.Count != 7 || checkout.Of != 7 || checkout.History != "reconstructed" || checkout.Evidence == nil || len(checkout.Evidence.Terminations) != 2 {
		t.Fatalf("checkout contract: %+v", checkout)
	}
	for _, termination := range checkout.Evidence.Terminations {
		if termination.Reason == "OOMKilled" && termination.Count != 6 {
			t.Fatalf("OOM evidence: %+v", termination)
		}
	}
	image := false
	for _, linked := range checkout.Changes {
		if linked.Change.Type == kubechanges.ImageChanged {
			image = true
			if linked.LeadSec != 240 || len(linked.Change.Fields) != 1 || linked.Change.Fields[0].From != "example.invalid/checkout:v41" || linked.Change.Fields[0].To != "example.invalid/checkout:v42" {
				t.Fatalf("reconstructed image: %+v", linked)
			}
		}
	}
	if !image {
		t.Fatal("no reconstructed image change")
	}
	if _, err := service.Issues(t.Context(), kubernetes.IssueOptions{RootUID: "deployment-checkout", IncludeChanges: true, Namespace: "payments"}); !errors.Is(err, kubernetes.ErrNotFound) {
		t.Fatal("out-of-scope root disclosed")
	}
	quiet, err := service.Issues(t.Context(), kubernetes.IssueOptions{RootUID: "node-agent", IncludeChanges: true})
	if err != nil || len(quiet.Items) != 1 || quiet.Items[0].History != "gap" || quiet.Items[0].BrokeAtSource != "first_seen" {
		t.Fatalf("cold quiet workload falsely complete: %+v %v", quiet, err)
	}
	ordinary, err := service.Issues(t.Context(), kubernetes.IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	warning := false
	for _, item := range ordinary.Items {
		if item.Root.Name == "cart" && item.Rule == "event.warning" && !item.Broken {
			warning = true
		}
	}
	if !warning {
		t.Fatal("ordinary warning lost")
	}
	encoded, _ := json.Marshal([]any{page, root, quiet, ordinary})
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatal("status or template canary escaped backend response")
	}
}

func TestContinuousCaptureIdleBreakAndShutdown(t *testing.T) {
	server, err := NewServer(Config{Scenario: "bad-rollout-before"})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(server)
	defer upstream.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: upstream.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{ClusterID: "cluster", CredentialID: "fixture"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	registry := kubernetes.NewServiceRegistry(service)
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Second)
	defer cancel()
	defer registry.Close()
	registry.StartCapture(ctx)
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	var baseline time.Time
	for baseline.IsZero() {
		if status := service.CaptureStatus(); status.LastCycleAt != nil {
			baseline = *status.LastCycleAt
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("capture did not initialize")
		case <-poll.C:
		}
	}
	if err := seedBadRollout(server.store, time.Now().UTC().Truncate(time.Second).Add(4*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	for {
		status := service.CaptureStatus()
		if status.LastCycleAt != nil && status.LastCycleAt.Sub(baseline) >= 20*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("idle capture did not record the next cycle")
		case <-poll.C:
		}
	}
	page, err := service.Issues(ctx, kubernetes.IssueOptions{Broken: true, IncludeChanges: true, RootUID: "deployment-checkout", Namespace: "shop"})
	if err != nil || len(page.Items) != 1 || !page.Items[0].Broken || page.Items[0].History == "complete" {
		t.Fatalf("idle break: %+v %v", page, err)
	}
	changes, err := service.Changes(ctx, kubernetes.ChangeQuery{Since: baseline, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	observedConfig, observedImage := false, false
	for _, change := range changes.Items {
		if change.Source != "observed" {
			continue
		}
		if change.Type == kubechanges.ConfigChanged && change.Name == "checkout-cfg" {
			observedConfig = true
		}
		if change.Type == kubechanges.ImageChanged && change.Name == "checkout" {
			observedImage = true
		}
	}
	if !observedConfig || !observedImage {
		t.Fatalf("idle observed config=%v image=%v", observedConfig, observedImage)
	}
	registry.Close()
	if _, err := registry.ResolveCluster("default", "cluster"); !errors.Is(err, kubernetes.ErrClusterUnavailable) {
		t.Fatal("closed capture registry resolved credentials")
	}
	for _, counter := range server.requestCounters() {
		if strings.HasSuffix(counter.Path, "/secrets") && !strings.Contains(counter.Accept, "PartialObjectMetadataList") {
			t.Fatal("capture transferred full Secret objects")
		}
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
