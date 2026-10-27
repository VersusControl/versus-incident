package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A resume by timestamp starts every source a little before the viewer's own
// watermark, so a line written across the reconnect boundary is replayed rather
// than lost. Nothing about the resume is encoded into the stream.
func TestWorkloadResumeTimeBacksOffByTheReplayOverlap(t *testing.T) {
	if sinceTimeOverlap <= 0 || sinceTimeOverlap > 30*time.Second {
		t.Fatalf("replay overlap %s is not a small window", sinceTimeOverlap)
	}
	watermark := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	start := workloadResumeTime(WorkloadLogStreamOptions{SinceTime: watermark})
	if !start.Equal(watermark.Add(-sinceTimeOverlap)) {
		t.Fatalf("resume start = %s", start)
	}
	if !workloadResumeTime(WorkloadLogStreamOptions{}).IsZero() {
		t.Fatal("a viewer without a timestamp invented one")
	}
	// A source already read in this connection keeps its own position, and the
	// start timestamp is then unused.
	position, from := workloadSourcePosition("checkpoint", start)
	if position != "checkpoint" || !from.IsZero() {
		t.Fatalf("running source = %q/%s", position, from)
	}
	position, from = workloadSourcePosition("", start)
	if position != "" || !from.Equal(start) {
		t.Fatalf("fresh source = %q/%s", position, from)
	}
}

// The stream never hands the viewer resume state: every event is small, and a
// reconnect is expressed as a timestamp the viewer already has.
func TestWorkloadStreamIssuesNoCursor(t *testing.T) {
	records := workloadStreamRecords("StatefulSet", 1, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveStreamPodObject(writer, request, records) {
			return
		}
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z first")
		fmt.Fprintln(writer, "2026-10-01T00:00:01Z second")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 400*time.Millisecond)
	defer cancel()
	lines := 0
	err := service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root"}, func(event WorkloadLogStreamEvent) error {
		if event.Cursor != "" {
			t.Fatalf("workload event carried resume state: %+v", event)
		}
		if event.Event == "line" {
			lines++
			if event.Timestamp == "" {
				t.Fatal("line has no timestamp to resume from")
			}
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || lines != 2 {
		t.Fatalf("stream lines=%d err=%v", lines, err)
	}
}

// The resume overlap starts a source just before the timestamp it was
// validated against, so a viewer resuming at the edge of the window derives a
// start just before the floor. That derived start is accepted: the read clamps
// it to the floor and serves the whole window, rather than failing a request
// validation already accepted. A start that already sits inside the window is
// used as given, and a timestamp the viewer sends directly is still refused
// when it falls outside the window.
func TestWorkloadResumeOverlapNeverReadsOutsideTheWindow(t *testing.T) {
	var reads []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/log") {
			values, _ := url.ParseQuery(request.URL.RawQuery)
			reads = append(reads, values)
		}
		fmt.Fprintln(writer, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)+" line")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	emit := func(PodLogStreamEvent) error { return nil }
	stream := func(since time.Time, derived bool) error {
		return service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "shop", Pod: "api", SinceTime: since, derivedSince: derived}, emit, time.Second, time.Second, time.Second, MaxLogStreamBytes)
	}
	// One second inside the window is an accepted viewer timestamp, and the
	// overlap moves the source's own start one second outside it.
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", SinceTime: time.Now().UTC().Add(-logReadWindow + time.Second)}
	if err := service.validateWorkloadLogOptions(logStreamTestContext(), options); err != nil {
		t.Fatalf("resume at the window edge rejected: %v", err)
	}
	start := workloadResumeTime(options)
	before := time.Now().UTC()
	if !start.Before(before.Add(-logReadWindow)) {
		t.Fatalf("overlap start %s did not cross the window floor", start)
	}
	if err := stream(start, true); err != nil {
		t.Fatalf("edge resume failed: %v", err)
	}
	after := time.Now().UTC()
	if len(reads) != 1 {
		t.Fatalf("reads = %v", reads)
	}
	requested, err := time.Parse(time.RFC3339Nano, reads[0].Get("sinceTime"))
	if err != nil {
		t.Fatal(err)
	}
	if requested.Before(before.Add(-logReadWindow)) || requested.After(after.Add(-logReadWindow)) {
		t.Fatalf("edge resume asked for %s, outside the window served at that moment", requested)
	}
	if !requested.After(start) {
		t.Fatalf("edge resume used the derived start %s instead of the floor", requested)
	}
	// A derived start inside the window is read exactly as derived.
	insideOptions := options
	insideOptions.SinceTime = time.Now().UTC().Add(-logReadWindow + time.Minute)
	inside := workloadResumeTime(insideOptions)
	reads = nil
	if err := stream(inside, true); err != nil || len(reads) != 1 {
		t.Fatalf("in-window start reads=%v err=%v", reads, err)
	}
	if requested, parseErr := time.Parse(time.RFC3339Nano, reads[0].Get("sinceTime")); parseErr != nil || !requested.Equal(inside) {
		t.Fatalf("in-window start asked for %q instead of %s", reads[0].Get("sinceTime"), inside)
	}
	// The floor did not move: the same timestamp sent by the viewer is refused.
	reads = nil
	if err := stream(start, false); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("out-of-window viewer timestamp returned %v", err)
	}
	if len(reads) != 0 {
		t.Fatalf("out-of-window start reached the backend: %v", reads)
	}
}

// A request that carries more than the endpoint accepts is refused before it is
// parsed, and the refusal names the class without echoing the input.
func TestWorkloadStreamRejectsInvalidSinceTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	base := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	now := time.Now().UTC()
	for _, test := range []struct {
		name    string
		mutate  func(*WorkloadLogStreamOptions)
		invalid bool
	}{
		{name: "in window", mutate: func(options *WorkloadLogStreamOptions) { options.SinceTime = now.Add(-time.Minute) }},
		{name: "slight clock skew", mutate: func(options *WorkloadLogStreamOptions) { options.SinceTime = now.Add(time.Second) }},
		{name: "far future", mutate: func(options *WorkloadLogStreamOptions) { options.SinceTime = now.Add(MaxLogClockSkew + time.Minute) }, invalid: true},
		{name: "older than a day", mutate: func(options *WorkloadLogStreamOptions) { options.SinceTime = now.Add(-25 * time.Hour) }, invalid: true},
		{name: "with a window size", mutate: func(options *WorkloadLogStreamOptions) {
			options.SinceTime, options.SinceSeconds = now.Add(-time.Minute), 60
		}, invalid: true},
		{name: "with a tail", mutate: func(options *WorkloadLogStreamOptions) {
			options.SinceTime, options.TailLines = now.Add(-time.Minute), 500
		}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := base
			test.mutate(&options)
			err := service.validateWorkloadLogOptions(logStreamTestContext(), options)
			if test.invalid && !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("invalid resume accepted: %v", err)
			}
			if !test.invalid && err != nil {
				t.Fatalf("valid resume rejected: %v", err)
			}
			if test.invalid && strings.Contains(err.Error(), now.Format(time.RFC3339Nano)) {
				t.Fatalf("refusal echoed the timestamp: %v", err)
			}
		})
	}
}
