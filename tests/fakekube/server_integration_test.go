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
)

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
