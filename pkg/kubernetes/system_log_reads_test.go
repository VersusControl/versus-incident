package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// fakeLogFeed answers Pod log requests: every string sent on lines is written
// and flushed at once, and closing lines ends the response.
type fakeLogFeed struct {
	lines chan string
}

func newFakeLogFeed() *fakeLogFeed { return &fakeLogFeed{lines: make(chan string, 4096)} }

func (feed *fakeLogFeed) serve(writer http.ResponseWriter, request *http.Request) {
	flusher, _ := writer.(http.Flusher)
	writer.Header().Set("Content-Type", "text/plain")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case line, open := <-feed.lines:
			if !open {
				return
			}
			_, _ = io.WriteString(writer, line)
			flusher.Flush()
		case <-request.Context().Done():
			return
		}
	}
}

func logsOn(include, exclude []string) breaklogs.Learning {
	return breaklogs.Learning{Logs: true, Scope: breaklogs.Scope{Include: include, Exclude: exclude}}
}

func newSystemLogFixture(t *testing.T, learning breaklogs.Learning, configure func(*systemReadsKube, *Service)) (*systemReadsFixture, *fakeLogFeed) {
	t.Helper()
	feed := newFakeLogFeed()
	fixture := newSystemReadsFixture(t, learning, func(kube *systemReadsKube, service *Service) {
		kube.logs = feed.serve
		if configure != nil {
			configure(kube, service)
		}
	})
	return fixture, feed
}

func stampedLine(at time.Time, text string) string {
	return at.UTC().Format(time.RFC3339Nano) + " " + text + "\n"
}

// collectLines reads a stream to its end and hands over every line once the
// stream has closed.
func collectLines(stream *SystemLogStream) <-chan []SystemLogLine {
	done := make(chan []SystemLogLine, 1)
	go func() {
		lines := []SystemLogLine{}
		for line := range stream.Lines() {
			lines = append(lines, line)
		}
		done <- lines
	}()
	return done
}

func (fixture *systemReadsFixture) follow(t *testing.T) *SystemLogStream {
	t.Helper()
	stream, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stream.Close)
	return stream
}

func (kube *systemReadsKube) logRequests() []*http.Request {
	result := []*http.Request{}
	for _, request := range kube.dataRequests() {
		if strings.HasSuffix(request.URL.Path, "/log") {
			result = append(result, request)
		}
	}
	return result
}

func TestSystemLogFollowReturnsScrubbedLinesUntilThePodEnds(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	stream := fixture.follow(t)
	now := time.Now()
	feed.lines <- stampedLine(now, "connecting with token="+systemReadCanary)
	feed.lines <- stampedLine(now, "ready")
	close(feed.lines)
	lines := waitFor(t, collectLines(stream), "the stream to end")
	if len(lines) != 2 || lines[1].Text != "ready" || lines[0].Timestamp.IsZero() {
		t.Fatalf("lines = %+v", lines)
	}
	if strings.Contains(lines[0].Text, systemReadCanary) || !strings.Contains(lines[0].Text, "[redacted]") {
		t.Fatalf("the line was not scrubbed: %q", lines[0].Text)
	}
	if stream.Reason() != SystemLogEnded || stream.Err() != nil {
		t.Fatalf("reason = %q, err = %v", stream.Reason(), stream.Err())
	}
	counter := fixture.counter(t, systemReadPodLog)
	if counter.Lane != SystemReadLaneLogs || counter.Lines != 2 || counter.Bytes == 0 || counter.Closes[SystemLogEnded] != 1 || counter.StreamsOpen != 0 || counter.Reads != 1 || counter.Requests != 1 {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemLogReadsMakeOnlyTheFixedLogRequest(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	close(feed.lines)
	since := time.Now().Add(-10 * time.Minute)
	stream, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", since)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, collectLines(stream), "the stream to end")
	old, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, collectLines(old), "the stream to end")
	for _, limits := range []SystemPreviousLogLimits{{}, {Bytes: 1000, Lines: 10}, {Bytes: 1 << 30, Lines: 1 << 20}} {
		if _, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", limits); err != nil {
			t.Fatal(err)
		}
	}
	requests := fixture.kube.dataRequests()
	if len(requests) != 5 {
		t.Fatalf("requests = %d, want two follows and three previous reads", len(requests))
	}
	for index, request := range requests {
		query := request.URL.Query()
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/namespaces/shop/pods/api-1/log" || query.Has("watch") || strings.Contains(request.URL.RawQuery, "watch") {
			t.Fatalf("request %d = %s %s", index, request.Method, request.URL)
		}
		if query.Get("container") != "app" || query.Get("timestamps") != "true" {
			t.Fatalf("request %d query = %s", index, request.URL.RawQuery)
		}
		if index < 2 {
			if query.Get("follow") != "true" || query.Has("previous") || query.Has("limitBytes") {
				t.Fatalf("follow query = %s", request.URL.RawQuery)
			}
			continue
		}
		if query.Get("previous") != "true" || query.Has("follow") {
			t.Fatalf("previous query = %s", request.URL.RawQuery)
		}
	}
	start, err := time.Parse(time.RFC3339Nano, requests[0].URL.Query().Get("sinceTime"))
	if err != nil || !start.Equal(since.UTC()) {
		t.Fatalf("since = %v, %v; want %v", start, err, since.UTC())
	}
	floor, err := time.Parse(time.RFC3339Nano, requests[1].URL.Query().Get("sinceTime"))
	if err != nil || time.Since(floor) > systemLogReplayWindow+time.Minute {
		t.Fatalf("a start two days back was not held to the replay window: %v, %v", floor, err)
	}
	// limitBytes is the bounded safety amount whatever the caller asks; the
	// kept bytes are applied here, to the newest lines.
	for index, want := range [][2]string{{"4194304", "2000"}, {"4194304", "10"}, {"4194304", "2000"}} {
		query := requests[2+index].URL.Query()
		if query.Get("limitBytes") != want[0] || query.Get("tailLines") != want[1] {
			t.Fatalf("previous read %d asked limitBytes=%s tailLines=%s, want %s and %s", index, query.Get("limitBytes"), query.Get("tailLines"), want[0], want[1])
		}
	}
}

func TestSystemLogReadsRefuseBeforeAnyRequest(t *testing.T) {
	type attempt struct {
		name, namespace, pod, container string
		learning                        breaklogs.Learning
		ctx                             func(context.Context) context.Context
		configure                       func(*systemReadsFixture)
		class, reason                   string
	}
	attempts := []attempt{
		{name: "caller context", ctx: func(ctx context.Context) context.Context {
			return core.WithCallerAuthorization(ctx, core.CallerAuthorization{Authenticated: true, Actor: "admin"})
		}, class: SystemReadRefused, reason: SystemReadRefusedCallerContext},
		{name: "namespace not a name", namespace: "Shop", class: SystemReadRefused, reason: SystemReadRefusedInvalidName},
		{name: "pod not a name", pod: "../api-1", class: SystemReadRefused, reason: SystemReadRefusedInvalidName},
		{name: "container missing", container: "-", class: SystemReadRefused, reason: SystemReadRefusedInvalidName},
		{name: "logs lane off", learning: metricsOn(nil, nil), class: SystemReadRefused, reason: SystemReadRefusedLaneOff},
		{name: "namespace excluded", learning: logsOn(nil, []string{"shop"}), class: SystemReadRefused, reason: SystemReadRefusedOutOfScope},
		{name: "namespace not included", learning: logsOn([]string{"kube-system"}, nil), class: SystemReadRefused, reason: SystemReadRefusedOutOfScope},
		{name: "policy refuses the namespace", configure: func(fixture *systemReadsFixture) {
			fixture.policy.refuse["shop"] = true
		}, class: SystemReadRefused, reason: SystemReadRefusedPolicy},
		{name: "settings cannot be loaded", configure: func(fixture *systemReadsFixture) {
			if err := fixture.provider.WriteBlob(breaklogs.SettingsKey("default", "prod"), []byte("{not json")); err != nil {
				t.Fatal(err)
			}
		}, class: SystemReadRefused, reason: SystemReadRefusedSettings},
		{name: "cluster held for recovery", configure: func(fixture *systemReadsFixture) {
			recovery := fixture.service.recovery
			recovery.mu.Lock()
			recovery.gate("default", time.Now().Add(time.Hour), false, ErrCredentialUnavailable, false)
			recovery.mu.Unlock()
		}, class: SystemReadUnavailable, reason: SystemReadRefusedCredential},
		{name: "scrubber missing", configure: func(fixture *systemReadsFixture) {
			fixture.reads.scrubber = nil
		}, class: SystemReadRefused, reason: SystemReadRefusedNoScrubber},
	}
	for _, test := range attempts {
		t.Run(test.name, func(t *testing.T) {
			learning := test.learning
			if !learning.Logs && !learning.Metrics {
				learning = logsOn(nil, nil)
			}
			fixture, _ := newSystemLogFixture(t, learning, nil)
			if test.configure != nil {
				test.configure(fixture)
			}
			ctx := t.Context()
			if test.ctx != nil {
				ctx = test.ctx(ctx)
			}
			namespace, pod, container := cmpOr(test.namespace, "shop"), cmpOr(test.pod, "api-1"), cmpOr(test.container, "app")
			if container == "-" {
				container = ""
			}
			stream, err := fixture.reads.FollowPodLog(ctx, "prod", namespace, pod, container, time.Time{})
			if stream != nil {
				t.Fatal("a refused follow returned a stream")
			}
			requireRefusal(t, err, test.class, test.reason)
			_, err = fixture.reads.PreviousPodLog(ctx, "prod", namespace, pod, container, SystemPreviousLogLimits{})
			requireRefusal(t, err, test.class, test.reason)
			if requests := fixture.kube.logRequests(); len(requests) != 0 {
				t.Fatalf("a refused read reached the cluster: %v", requests[0].URL)
			}
			if counter := fixture.counter(t, systemReadPodLog); counter.Refusals[test.reason] != 1 || counter.Reads != 0 {
				t.Fatalf("counters = %+v", counter)
			}
		})
	}
	t.Run("unknown cluster", func(t *testing.T) {
		fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), nil)
		_, err := fixture.reads.FollowPodLog(t.Context(), "staging", "shop", "api-1", "app", time.Time{})
		requireRefusal(t, err, SystemReadRefused, SystemReadRefusedUnknown)
		if len(fixture.kube.seen()) != 0 {
			t.Fatal("an unknown cluster was asked")
		}
	})
	t.Run("zero and nil handle", func(t *testing.T) {
		var zero SystemReads
		var none *SystemReads
		for _, reads := range []*SystemReads{&zero, none} {
			stream, err := reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
			requireRefusal(t, err, SystemReadRefused, SystemReadRefusedNotEnabled)
			if stream != nil {
				t.Fatal("a disabled handle returned a stream")
			}
			_, err = reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
			requireRefusal(t, err, SystemReadRefused, SystemReadRefusedNotEnabled)
		}
	})
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// An open follow checks the gate again on its own and closes within the
// recheck period once the gate refuses, though the worker never closes it.
func TestSystemLogFollowClosesWhenTheGateChangesMidStream(t *testing.T) {
	for name, change := range map[string]struct {
		apply  func(*testing.T, *systemReadsFixture)
		reason string
	}{
		"logs lane turned off": {func(t *testing.T, fixture *systemReadsFixture) {
			saveSystemReadLearning(t, fixture.provider, metricsOn(nil, nil))
		}, SystemReadRefusedLaneOff},
		"namespace excluded": {func(t *testing.T, fixture *systemReadsFixture) {
			saveSystemReadLearning(t, fixture.provider, logsOn(nil, []string{"shop"}))
		}, SystemReadRefusedOutOfScope},
		"policy refuses": {func(_ *testing.T, fixture *systemReadsFixture) {
			fixture.policy.mu.Lock()
			fixture.policy.refuse["shop"] = true
			fixture.policy.mu.Unlock()
		}, SystemReadRefusedPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
			// A test pace for the recheck; the production period is checked
			// below.
			fixture.reads.logLimits.recheck = 5 * time.Millisecond
			// Only the recheck under test may close the stream; the watchdog's
			// bound is the hang guard, not a latency budget.
			fixture.reads.logLimits.overdue = scaledTimeout(hangGuard)
			stream := fixture.follow(t)
			feed.lines <- stampedLine(time.Now(), "before the change")
			if line := waitFor(t, stream.Lines(), "the first line"); line.Text != "before the change" {
				t.Fatalf("line = %+v", line)
			}
			change.apply(t, fixture)
			waitFor(t, collectLines(stream), "the stream to close after the gate changed")
			if stream.Reason() != change.reason {
				t.Fatalf("reason = %q, want %q", stream.Reason(), change.reason)
			}
			if counter := fixture.counter(t, systemReadPodLog); counter.Closes[change.reason] != 1 || counter.StreamsOpen != 0 || counter.Lines != 1 {
				t.Fatalf("counters = %+v", counter)
			}
		})
	}
	t.Run("production recheck closes within thirty seconds", func(t *testing.T) {
		limits := defaultSystemLogLimits()
		if limits.recheck <= 0 || 2*limits.recheck > 30*time.Second || systemReadSettingsMaxAge > 30*time.Second {
			t.Fatalf("recheck = %s; a change could stay open longer than 30 s", limits.recheck)
		}
	})
}

func TestSystemLogFollowStopsOnARetiredCredential(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), func(_ *systemReadsKube, service *Service) {
		service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer reader", nil }}
	})
	service := NewService(fixture.service.client, fixture.service.Scope(), 0)
	service.SetChangeStorage(fixture.provider)
	fixture.registry = NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "prod", Provider: "generic"}, Service: service}})
	fixture.reads.registry = fixture.registry
	stream := fixture.follow(t)
	feed.lines <- stampedLine(time.Now(), "serving")
	waitFor(t, stream.Lines(), "the first line")
	service.identity.mu.Lock()
	service.identity.retired = true
	service.identity.cancel()
	service.identity.mu.Unlock()
	waitFor(t, collectLines(stream), "the stream to close on the retired credential")
	if stream.Reason() != SystemReadRefusedCredential {
		t.Fatalf("reason = %q, err = %v", stream.Reason(), stream.Err())
	}
	before := len(fixture.kube.logRequests())
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	_, err = fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedCredential)
	if after := len(fixture.kube.logRequests()); after != before {
		t.Fatalf("a retired credential reached the cluster: %d new requests", after-before)
	}
	if service.recovery.attemptInFlight() || service.recovery.attempts != 0 || !service.recovery.deadline().IsZero() {
		t.Fatal("a system log read started credential recovery")
	}
}

func TestSystemLogForbiddenAnswerLeavesTheCredentialAlone(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, service *Service) {
		kube.status = http.StatusForbidden
		service.changes.evidenceEnabled = true
	})
	warned := []string{}
	fixture.reads.warn = func(key, cause, format string, args ...any) {
		warned = append(warned, cause+" "+fmt.Sprintf(format, args...))
	}
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	requireRefusal(t, err, SystemReadForbidden, "")
	if strings.Contains(err.Error(), systemReadCanary) || strings.Contains(err.Error(), "reader") {
		t.Fatalf("the error carried the response body: %v", err)
	}
	_, err = fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	requireRefusal(t, err, SystemReadForbidden, "")
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
		t.Fatal("a 403 to a system log read was recorded as a history authority denial")
	}
	if counter := fixture.counter(t, systemReadPodLog); counter.Causes[SystemReadForbidden] != 1 || counter.LastCause != SystemReadForbidden || counter.StreamsOpen != 0 {
		t.Fatalf("counters = %+v", counter)
	}
	if len(warned) == 0 || !strings.Contains(warned[0], "grant get on pods/log") || !strings.Contains(warned[0], "logs learning") {
		t.Fatalf("warnings = %q", warned)
	}
}

func TestSystemLogThrottleIsKeptApartFromViewerReads(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.status = http.StatusTooManyRequests
	})
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	if err == nil {
		t.Fatal("a throttled follow opened")
	}
	client := fixture.service.client
	client.backoffMu.Lock()
	viewer, system := client.backoffUntil, client.systemBackoffUntil
	client.backoffMu.Unlock()
	if !viewer.IsZero() || !system.After(time.Now()) {
		t.Fatalf("viewer backoff = %v, system backoff = %v; a 429 on a system read must hold only system reads", viewer, system)
	}
	asked := len(fixture.kube.logRequests())
	if _, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{}); err == nil {
		t.Fatal("a follow ignored the system throttle")
	}
	if len(fixture.kube.logRequests()) != asked {
		t.Fatal("a follow asked the cluster while the system throttle lasted")
	}
}

func TestSystemLogLinesAreCutToTheLineLimit(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	stream := fixture.follow(t)
	now := time.Now()
	feed.lines <- stampedLine(now, strings.Repeat("word ", 8<<10)) // 40 KiB, under the raw cut
	feed.lines <- stampedLine(now, strings.Repeat("abc ", 32<<10)) // 128 KiB, over the raw cut
	feed.lines <- stampedLine(now, strings.Repeat("x", 300<<10))   // 300 KiB without a space
	feed.lines <- stampedLine(now, "after the long lines")
	close(feed.lines)
	lines := waitFor(t, collectLines(stream), "the stream to end")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}
	for index, line := range lines[:3] {
		if !line.Truncated || len(line.Text) > systemLogLineBytes || !strings.HasSuffix(line.Text, systemLogTruncatedMarker) || line.Timestamp.IsZero() {
			t.Fatalf("line %d: truncated=%v length=%d", index, line.Truncated, len(line.Text))
		}
	}
	if lines[3].Text != "after the long lines" || lines[3].Truncated {
		t.Fatalf("the line after the long ones = %+v", lines[3])
	}
	if counter := fixture.counter(t, systemReadPodLog); counter.Truncated != 3 || counter.Lines != 4 {
		t.Fatalf("counters = %+v", counter)
	}
}

// A secret the read cut splits in two is not returned in part: the cut line
// loses its last word after it is scrubbed.
func TestSystemLogCutLineDropsAPartialSecret(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.rawLineCut = 1024
	stream := fixture.follow(t)
	prefix := time.Now().UTC().Format(time.RFC3339Nano) + " "
	filler := "visible words "
	filler += strings.Repeat("a", 1024-len(prefix)-len(filler)-6)
	feed.lines <- prefix + filler + " " + systemReadCanary + " tail\n"
	close(feed.lines)
	lines := waitFor(t, collectLines(stream), "the stream to end")
	if len(lines) != 1 || !lines[0].Truncated || !strings.HasPrefix(lines[0].Text, "visible words") {
		t.Fatalf("lines = %+v", lines)
	}
	if strings.Contains(lines[0].Text, systemReadCanary[:6]) {
		t.Fatalf("part of the secret was returned: %q", lines[0].Text)
	}
}

func TestSystemLogFollowClosesAtItsByteBudget(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.streamBytes = 4096
	stream := fixture.follow(t)
	line := stampedLine(time.Now(), strings.Repeat("b", 60))
	for range 100 {
		feed.lines <- line
	}
	lines := waitFor(t, collectLines(stream), "the stream to close at its budget")
	if stream.Reason() != SystemLogByteBudget {
		t.Fatalf("reason = %q", stream.Reason())
	}
	if got := len(lines) * len(line); got > 4096 || len(lines) == 0 {
		t.Fatalf("%d lines (%d bytes) returned from a 4096-byte budget", len(lines), got)
	}
	if counter := fixture.counter(t, systemReadPodLog); counter.Closes[SystemLogByteBudget] != 1 || counter.Bytes > 4096+int64(len(line)) {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemLogFollowClosesAtItsReplayBudget(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.replayBytes = 1024
	stream, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Now().Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stream.Close)
	line := stampedLine(time.Now().Add(-5*time.Minute), strings.Repeat("r", 60))
	for range 50 {
		feed.lines <- line
	}
	lines := waitFor(t, collectLines(stream), "the stream to close at its replay budget")
	if stream.Reason() != SystemLogReplayBudget || len(lines)*len(line) > 1024 {
		t.Fatalf("reason = %q after %d lines", stream.Reason(), len(lines))
	}
}

// gatedPace lets allowed bytes through, then holds the reader until release
// is closed.
type gatedPace struct {
	mu      sync.Mutex
	allowed int
	used    int
	blocked chan struct{}
	release chan struct{}
}

func (pace *gatedPace) WaitN(ctx context.Context, count int) error {
	pace.mu.Lock()
	pace.used += count
	over := pace.used > pace.allowed
	pace.mu.Unlock()
	if !over {
		return nil
	}
	select {
	case pace.blocked <- struct{}{}:
	default:
	}
	select {
	case <-pace.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSystemLogFollowIsHeldToItsByteRate(t *testing.T) {
	limiter, ok := newSystemLogPace().(*rate.Limiter)
	if !ok || limiter.Limit() != rate.Limit(systemLogBytesPerSecond) || limiter.Burst() != systemLogBytesPerSecond || systemLogBytesPerSecond < systemLogReadBuffer {
		t.Fatalf("production pace = %T", newSystemLogPace())
	}
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	pace := &gatedPace{blocked: make(chan struct{}, 1), release: make(chan struct{})}
	fixture.reads.newLogPace = func() systemLogPace { return pace }
	line := stampedLine(time.Now(), strings.Repeat("p", 60))
	pace.allowed = 3*len(line) + len(line)/2
	stream := fixture.follow(t)
	for range 10 {
		feed.lines <- line
	}
	waitFor(t, pace.blocked, "the reader to reach its byte rate")
	for index := range 3 {
		waitFor(t, stream.Lines(), "line "+strconv.Itoa(index))
	}
	select {
	case extra := <-stream.Lines():
		t.Fatalf("a line past the byte rate was returned: %+v", extra)
	default:
	}
	close(pace.release)
	for index := 3; index < 10; index++ {
		waitFor(t, stream.Lines(), "line "+strconv.Itoa(index))
	}
}

func TestSystemLogStreamsAreLimitedPerCluster(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.streamsPerCluster = 1
	first := fixture.follow(t)
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-2", "app", time.Time{})
	requireRefusal(t, err, SystemReadUnavailable, SystemReadRefusedStreamLimit)
	first.Close()
	if first.Reason() != SystemLogCanceled {
		t.Fatalf("reason = %q", first.Reason())
	}
	second := fixture.follow(t)
	second.Close()
	counter := fixture.counter(t, systemReadPodLog)
	if counter.Refusals[SystemReadRefusedStreamLimit] != 1 || counter.Closes[SystemLogCanceled] != 2 || counter.StreamsOpen != 0 {
		t.Fatalf("counters = %+v", counter)
	}
	if defaults := defaultSystemLogLimits(); defaults.streamsPerCluster != 32 || defaults.streamsPerProcess != 128 {
		t.Fatalf("stream limits = %+v", defaults)
	}
}

func TestSystemLogFollowEndsWithTheCallersContext(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), nil)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := fixture.reads.FollowPodLog(ctx, "prod", "shop", "api-1", "app", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, collectLines(stream), "the stream to end with its context")
	if stream.Reason() != SystemLogCanceled || stream.Err() != nil {
		t.Fatalf("reason = %q, err = %v", stream.Reason(), stream.Err())
	}
}

func TestSystemLogPreviousReadIsBoundedAndScrubbed(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		// The answer ignores limitBytes, so the read's own limit is what
		// holds.
		kube.logs = func(writer http.ResponseWriter, _ *http.Request) {
			at := time.Now().Add(-time.Hour)
			for index := range 200 {
				_, _ = io.WriteString(writer, stampedLine(at, fmt.Sprintf("crash %d token=%s", index, systemReadCanary)))
			}
		}
	})
	previous, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{Bytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range previous.Lines {
		total += len(line.Text)
		if strings.Contains(line.Text, systemReadCanary) || line.Timestamp.IsZero() {
			t.Fatalf("line = %+v", line)
		}
	}
	if !previous.Truncated || total > 1000 || len(previous.Lines) == 0 {
		t.Fatalf("previous read: %d lines, %d bytes, truncated=%v", len(previous.Lines), total, previous.Truncated)
	}
	if last := previous.Lines[len(previous.Lines)-1].Text; !strings.HasPrefix(last, "crash 199 ") {
		t.Fatalf("the newest line was not kept: %q", last)
	}
	lines, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{Lines: 5})
	if err != nil || len(lines.Lines) != 5 || !strings.HasPrefix(lines.Lines[4].Text, "crash 199 ") {
		t.Fatalf("a five-line read returned %+v, %v", lines.Lines, err)
	}
	if counter := fixture.counter(t, systemReadPreviousPodLog); counter.Reads != 2 || counter.Lines != uint64(len(previous.Lines))+5 {
		t.Fatalf("counters = %+v", counter)
	}
}

func TestSystemLogPreviousReadOfAContainerWithoutOne(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.logs = func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(writer, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"previous terminated container \"app\" in pod \"api-1\" not found"}`)
		}
	})
	_, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	requireRefusal(t, err, SystemReadNotFound, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing previous run does not match ErrNotFound: %v", err)
	}
}

// kubeletPreviousLog answers a previous-run read the way the kubelet does: it
// seeks to the last tailLines lines, then stops after limitBytes, which may
// cut the last line it sends.
func kubeletPreviousLog(lines []string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		sent := lines
		if tail, err := strconv.Atoi(query.Get("tailLines")); err == nil && tail < len(sent) {
			sent = sent[len(sent)-tail:]
		}
		body := strings.Join(sent, "")
		if limit, err := strconv.Atoi(query.Get("limitBytes")); err == nil && limit < len(body) {
			body = body[:limit]
		}
		_, _ = io.WriteString(writer, body)
	}
}

var previousRunAt = time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)

// The lines that say why a container stopped are its last ones: they are
// returned even when the tail is larger than the bytes kept.
func TestSystemLogPreviousReadKeepsTheEndOfTheRun(t *testing.T) {
	lines := []string{}
	for index := range 3000 {
		lines = append(lines, stampedLine(previousRunAt, fmt.Sprintf("step %d %s", index, strings.Repeat("s", 180))))
	}
	lines = append(lines, stampedLine(previousRunAt, "crash final reason=out of memory"))
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.logs = kubeletPreviousLog(lines)
	})
	previous, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, line := range previous.Lines {
		total += len(line.Text)
	}
	if last := previous.Lines[len(previous.Lines)-1]; last.Text != "crash final reason=out of memory" || last.Truncated {
		t.Fatalf("the last line of the run was lost: %+v", last)
	}
	if !previous.Truncated || total > SystemPreviousLogBytes || len(previous.Lines) > SystemPreviousLogLines {
		t.Fatalf("previous read: %d lines, %d bytes, truncated=%v", len(previous.Lines), total, previous.Truncated)
	}
}

// A line the API server cut at limitBytes is treated as cut: a secret the cut
// split is not returned in part, and the read says it was truncated.
func TestSystemLogPreviousReadDropsALineCutAtTheLimit(t *testing.T) {
	head := previousRunAt.Format(time.RFC3339Nano) + " crash token="
	last := head + systemReadCanary + "\n"
	room := systemPreviousLogRequestBytes - len(head) - 9
	lines := []string{}
	for room > 0 {
		size := min(room, 4000)
		if room-size > 0 && room-size < 100 {
			size = room - 100
		}
		pad := size - len(previousRunAt.Format(time.RFC3339Nano)) - 2
		lines = append(lines, stampedLine(previousRunAt, strings.Repeat("f", pad)))
		room -= size
	}
	lines = append(lines, last)
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.logs = kubeletPreviousLog(lines)
	})
	previous, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range previous.Lines {
		if strings.Contains(line.Text, systemReadCanary[:9]) {
			t.Fatalf("part of the secret was returned: %q", line.Text)
		}
	}
	final := previous.Lines[len(previous.Lines)-1]
	if !previous.Truncated || !final.Truncated || !strings.HasPrefix(final.Text, "crash") {
		t.Fatalf("truncated=%v, last line %+v", previous.Truncated, final)
	}
}

// A follow whose connection breaks inside a line returns that line as cut.
func TestSystemLogFollowDropsTheLastWordOfALineTheReadStoppedIn(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.logs = func(writer http.ResponseWriter, _ *http.Request) {
			flusher, _ := writer.(http.Flusher)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, stampedLine(time.Now(), "serving"))
			_, _ = io.WriteString(writer, time.Now().UTC().Format(time.RFC3339Nano)+" crash token="+systemReadCanary[:9])
			flusher.Flush()
			panic(http.ErrAbortHandler)
		}
	})
	stream := fixture.follow(t)
	lines := waitFor(t, collectLines(stream), "the stream to end")
	for _, line := range lines {
		if strings.Contains(line.Text, systemReadCanary[:9]) {
			t.Fatalf("part of the secret was returned: %q", line.Text)
		}
	}
	if len(lines) != 2 || lines[0].Text != "serving" || !lines[1].Truncated || stream.Reason() != SystemLogFailed {
		t.Fatalf("lines = %+v, reason = %q", lines, stream.Reason())
	}
}

// cancellingPace ends the caller's context while the second line is read.
type cancellingPace struct {
	calls  int
	cancel context.CancelFunc
}

func (pace *cancellingPace) WaitN(context.Context, int) error {
	pace.calls++
	if pace.calls == 2 {
		pace.cancel()
	}
	return nil
}

// A line read while the stream is closing is not returned.
func TestSystemLogFollowSendsNothingAfterItsContextEnds(t *testing.T) {
	for attempt := range 20 {
		t.Run(strconv.Itoa(attempt), func(t *testing.T) {
			fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture.reads.newLogPace = func() systemLogPace { return &cancellingPace{cancel: cancel} }
			feed.lines <- stampedLine(time.Now(), "one")
			feed.lines <- stampedLine(time.Now(), "two")
			stream, err := fixture.reads.FollowPodLog(ctx, "prod", "shop", "api-1", "app", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			lines := waitFor(t, collectLines(stream), "the stream to end")
			if len(lines) != 1 || lines[0].Text != "one" || stream.Reason() != SystemLogCanceled {
				t.Fatalf("lines = %+v, reason = %q", lines, stream.Reason())
			}
		})
	}
}

// A follow opens only on a setting as fresh as its recheck would read, so it
// cannot open on one older than that.
func TestSystemLogFollowOpensOnAFreshSetting(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), nil)
	now := time.Now()
	fixture.reads.now = func() time.Time { return now }
	if _, err := fixture.reads.learning(fixture.service, "prod"); err != nil {
		t.Fatal(err)
	}
	saveSystemReadLearning(t, fixture.provider, metricsOn(nil, nil))
	now = now.Add(29 * time.Second)
	stream, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	if err == nil {
		stream.Close()
	}
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
	_, err = fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedLaneOff)
}

// The stream's slot is free once Lines is closed, so the reader can reopen at
// once.
func TestSystemLogSlotIsFreeWhenLinesClose(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.streamsPerCluster = 1
	stream := fixture.follow(t)
	close(feed.lines)
	waitFor(t, collectLines(stream), "the stream to end")
	again, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	if err != nil {
		t.Fatalf("the slot of an ended stream was still held: %v", err)
	}
	again.Close()
}

// A cancel that lands while a recheck is asking the policy closes the stream
// as canceled, not as refused by the policy.
func TestSystemLogCancelDuringARecheckReadsCanceled(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.recheck = 5 * time.Millisecond
	// Only the recheck under test may close the stream; the watchdog's
	// bound is the hang guard, not a latency budget.
	fixture.reads.logLimits.overdue = scaledTimeout(hangGuard)
	var armed atomic.Bool
	entered := make(chan struct{}, 1)
	fixture.reads.policy = func(ctx context.Context, _ string, _ SystemReadLane, _ string) bool {
		if !armed.Load() {
			return true
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return false
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := fixture.reads.FollowPodLog(ctx, "prod", "shop", "api-1", "app", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	waitFor(t, entered, "a recheck to ask the policy")
	cancel()
	waitFor(t, collectLines(stream), "the stream to end")
	if stream.Reason() != SystemLogCanceled {
		t.Fatalf("reason = %q, want %q", stream.Reason(), SystemLogCanceled)
	}
}

// A reader that stops draining cannot hold a stream open: a gate change still
// closes it and frees its slot.
func TestSystemLogUndrainedStreamStillClosesOnAGateChange(t *testing.T) {
	fixture, feed := newSystemLogFixture(t, logsOn(nil, nil), nil)
	fixture.reads.logLimits.recheck = 5 * time.Millisecond
	// Only the recheck under test may close the stream; the watchdog's
	// bound is the hang guard, not a latency budget.
	fixture.reads.logLimits.overdue = scaledTimeout(hangGuard)
	fixture.reads.logLimits.streamsPerCluster = 1
	// The clock can be stepped past the cached setting without a wait; the
	// step is atomic because the recheck reads the clock.
	var step atomic.Int64
	fixture.reads.now = func() time.Time { return time.Now().Add(time.Duration(step.Load())) }
	stream := fixture.follow(t)
	for index := range systemLogLineBuffer + 50 {
		feed.lines <- stampedLine(time.Now(), "line "+strconv.Itoa(index))
	}
	eventually(t, "the line buffer to fill", func() bool { return len(stream.lines) == cap(stream.lines) })
	saveSystemReadLearning(t, fixture.provider, metricsOn(nil, nil))
	waitFor(t, stream.done, "the undrained stream to close")
	if stream.Reason() != SystemReadRefusedLaneOff {
		t.Fatalf("reason = %q", stream.Reason())
	}
	saveSystemReadLearning(t, fixture.provider, logsOn(nil, nil))
	step.Store(int64(time.Minute))
	again, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	if err != nil {
		t.Fatalf("the undrained stream's slot was not freed: %v", err)
	}
	again.Close()
}

// A previous-run body that ends inside a line, short of limitBytes, still has
// that line treated as cut.
func TestSystemLogPreviousReadDropsTheLastWordOfAnUnendedLine(t *testing.T) {
	fixture, _ := newSystemLogFixture(t, logsOn(nil, nil), func(kube *systemReadsKube, _ *Service) {
		kube.logs = func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, stampedLine(previousRunAt, "starting"))
			_, _ = io.WriteString(writer, previousRunAt.Format(time.RFC3339Nano)+" crash token="+systemReadCanary[:9])
		}
	})
	previous, err := fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(previous.Lines) != 2 || previous.Lines[0].Text != "starting" || !previous.Lines[1].Truncated || strings.Contains(previous.Lines[1].Text, systemReadCanary[:9]) {
		t.Fatalf("lines = %+v", previous.Lines)
	}
}

// heldSettingsStore is a settings store whose reads can be held, as a stalled
// database connection holds them.
type heldSettingsStore struct {
	storage.Provider
	held    atomic.Bool
	reads   atomic.Int64
	release chan struct{}
	once    sync.Once
}

// open lets every held read through; it is safe to call more than once.
func (store *heldSettingsStore) open() { store.once.Do(func() { close(store.release) }) }

func (store *heldSettingsStore) ReadBlob(name string) ([]byte, error) {
	if store.held.Load() {
		store.reads.Add(1)
		<-store.release
	}
	return store.Provider.ReadBlob(name)
}

func newHeldStoreFixture(t *testing.T) (*systemReadsFixture, *heldSettingsStore) {
	t.Helper()
	learning := breaklogs.Learning{Logs: true, Metrics: true, Scope: breaklogs.Scope{Include: []string{}, Exclude: []string{}}}
	fixture, _ := newSystemLogFixture(t, learning, nil)
	store := &heldSettingsStore{Provider: fixture.provider, release: make(chan struct{})}
	t.Cleanup(store.open)
	fixture.service.SetChangeStorage(store)
	fixture.reads.logLimits.recheck = 5 * time.Millisecond
	return fixture, store
}

// A settings store that stops answering refuses, so an open stream closes
// instead of going on unchecked, and new reads are refused; only one store
// read is in flight for the cluster.
func TestSystemLogHeldSettingsStoreClosesTheStream(t *testing.T) {
	fixture, store := newHeldStoreFixture(t)
	fixture.reads.logLimits.overdue = scaledTimeout(hangGuard)
	fixture.reads.settingsWait = 20 * time.Millisecond
	stream := fixture.follow(t)
	store.held.Store(true)
	lines := waitFor(t, collectLines(stream), "the stream to close on a held store")
	if stream.Reason() != SystemReadRefusedSettings || len(lines) != 0 {
		t.Fatalf("reason = %q, lines = %+v", stream.Reason(), lines)
	}
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedSettings)
	// A list read trusts a setting for 30 s; once that copy is gone it is
	// held to the same bound.
	fixture.reads.mu.Lock()
	delete(fixture.reads.settings, "prod")
	fixture.reads.mu.Unlock()
	_, err = fixture.reads.PodMetrics(t.Context(), "prod")
	requireRefusal(t, err, SystemReadRefused, SystemReadRefusedSettings)
	if held := store.reads.Load(); held != 1 {
		t.Fatalf("%d store reads in flight for one cluster, want 1", held)
	}
}

// A recheck that cannot finish at all still cannot keep a stream open: the
// watchdog closes it once no recheck has passed for its bound.
func TestSystemLogWatchdogClosesAStreamWhoseRecheckHangs(t *testing.T) {
	fixture, store := newHeldStoreFixture(t)
	fixture.reads.settingsWait = scaledTimeout(hangGuard)
	fixture.reads.logLimits.overdue = 30 * time.Millisecond
	stream := fixture.follow(t)
	store.held.Store(true)
	waitFor(t, collectLines(stream), "the watchdog to close the stream")
	if stream.Reason() != SystemLogRecheckOverdue {
		t.Fatalf("reason = %q", stream.Reason())
	}
	if defaults := defaultSystemLogLimits(); defaults.overdue != 0 || systemReadSettingsWait >= defaults.recheck {
		t.Fatalf("production bounds: overdue %s, settings wait %s, recheck %s", defaults.overdue, systemReadSettingsWait, defaults.recheck)
	}
}

// A 401 on a Pod log can be the kubelet of the Pod's node refusing the API
// server: it fails that read only, warns once naming the node's kubelet, and
// leaves the credential, recovery, history and the other reads alone.
func TestSystemLog401FailsTheReadOnly(t *testing.T) {
	learning := breaklogs.Learning{Logs: true, Metrics: true, Scope: breaklogs.Scope{Include: []string{}, Exclude: []string{}}}
	fixture, _ := newSystemLogFixture(t, learning, func(_ *systemReadsKube, service *Service) {
		service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer reader", nil }}
	})
	service := NewService(fixture.service.client, fixture.service.Scope(), 0)
	service.SetChangeStorage(fixture.provider)
	service.changes.evidenceEnabled = true
	fixture.registry = NewClusterRegistry(false, []ClusterEntry{{Info: ClusterInfo{ID: "prod", Provider: "generic"}, Service: service}})
	fixture.reads.registry = fixture.registry
	fixture.kube.mu.Lock()
	fixture.kube.logs = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"the server has asked for the client to provide credentials ( pods/log api-1)","reason":"Unauthorized","code":401}`)
	}
	fixture.kube.mu.Unlock()
	warned := []string{}
	fixture.reads.warn = func(_, cause, format string, args ...any) {
		warned = append(warned, cause+" "+fmt.Sprintf(format, args...))
	}
	_, err := fixture.reads.FollowPodLog(t.Context(), "prod", "shop", "api-1", "app", time.Time{})
	requireRefusal(t, err, SystemReadUnauthorized, "")
	_, err = fixture.reads.PreviousPodLog(t.Context(), "prod", "shop", "api-1", "app", SystemPreviousLogLimits{})
	requireRefusal(t, err, SystemReadUnauthorized, "")
	service.identity.mu.Lock()
	ended := service.identity.retired || service.identity.ctx.Err() != nil
	service.identity.mu.Unlock()
	service.changes.mu.Lock()
	denials := len(service.changes.historyDenials)
	service.changes.mu.Unlock()
	if ended || service.recovery.failures != 0 || service.recovery.requestGateError("default") != nil || denials != 0 {
		t.Fatalf("a 401 on a Pod log touched the credential: ended=%v failures=%d denials=%d", ended, service.recovery.failures, denials)
	}
	if _, err := fixture.reads.PodMetrics(t.Context(), "prod"); err != nil {
		t.Fatalf("PodMetrics stopped after a 401 on a Pod log: %v", err)
	}
	if len(warned) == 0 || !strings.Contains(warned[0], "kubelet") || !strings.Contains(warned[0], "prod") {
		t.Fatalf("warnings = %q", warned)
	}
	if counter := fixture.counter(t, systemReadPodLog); counter.Causes[SystemReadUnauthorized] != 1 {
		t.Fatalf("counters = %+v", counter)
	}
}

// The first recheck is counted from the admission, so a slow open does not
// stretch the close bound: after an open slower than a period it is due at
// once.
func TestSystemLogFirstRecheckCountsFromTheAdmission(t *testing.T) {
	period := systemLogRecheck
	run := &systemLogRun{reads: &SystemReads{logLimits: defaultSystemLogLimits()}}
	run.admittedAt = time.Now().Add(-2 * period)
	if delay := run.firstRecheck(); delay != 0 {
		t.Fatalf("after an open slower than a period the first recheck waits %s", delay)
	}
	run.admittedAt = time.Now().Add(-period / 3)
	if delay := run.firstRecheck(); delay <= 0 || delay > period-period/3 {
		t.Fatalf("first recheck in %s, want at most %s after an open of %s", delay, period-period/3, period/3)
	}
}

// A read that joins a settings load already in flight refuses its answer
// when the load started before the read's own trust period, as it refuses a
// cached setting that old.
func TestSystemReadsJoinedSettingsLoadIsHeldToTheTrustPeriod(t *testing.T) {
	fixture, store := newHeldStoreFixture(t)
	fixture.reads.settingsWait = scaledTimeout(hangGuard)
	var step atomic.Int64
	start := time.Now()
	fixture.reads.now = func() time.Time { return start.Add(time.Duration(step.Load())) }
	fixture.reads.mu.Lock()
	delete(fixture.reads.settings, "prod")
	fixture.reads.mu.Unlock()
	store.held.Store(true)
	type answer struct {
		learning breaklogs.Learning
		err      error
	}
	first, joined := make(chan answer, 1), make(chan answer, 1)
	go func() {
		learning, err := fixture.reads.learningWithin(fixture.service, "prod", systemReadSettingsMaxAge)
		first <- answer{learning, err}
	}()
	eventually(t, "the first load to reach the store", func() bool { return store.reads.Load() == 1 })
	step.Store(int64(20 * time.Second))
	go func() {
		learning, err := fixture.reads.learningWithin(fixture.service, "prod", systemLogRecheck)
		joined <- answer{learning, err}
	}()
	// The second read joins the load in flight instead of starting one.
	eventually(t, "the second read to join", func() bool {
		fixture.reads.mu.Lock()
		defer fixture.reads.mu.Unlock()
		load := fixture.reads.loading["prod"]
		return load != nil && load.joined == 1
	})
	store.open()
	if got := waitFor(t, joined, "the joined read"); !errors.Is(got.err, errSystemSettingsSlow) {
		t.Fatalf("a read with a 15 s trust period took an answer started 20 s earlier: %+v", got)
	}
	if got := waitFor(t, first, "the first read"); got.err != nil || !got.learning.Logs {
		t.Fatalf("the read whose period covers the answer = %+v", got)
	}
	if held := store.reads.Load(); held != 1 {
		t.Fatalf("%d store reads for one cluster, want 1", held)
	}
}
