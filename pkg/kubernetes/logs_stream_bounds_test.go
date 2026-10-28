package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Pod stream keeps its line cursor only while the cursor still fits the
// request the client has to send back. A resume state that does not fit is not
// lost: every line carries its timestamp, and the client resumes from that.
func TestPodLogCursorStaysInsideTheRequestBudget(t *testing.T) {
	if maxStreamCursorBytes <= 0 || maxStreamCursorBytes >= MaxStreamRequestQueryBytes {
		t.Fatalf("cursor budget %d does not fit the query bound %d", maxStreamCursorBytes, MaxStreamRequestQueryBytes)
	}
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.RawQuery)
		// One line per second, so the resume state keeps one identity per line.
		start := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
		for line := 0; line < 200; line++ {
			fmt.Fprintf(writer, "%s line-%d\n", start.Add(time.Duration(line)*time.Second).Format(time.RFC3339Nano), line)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})

	var first, largest, lines, cursorless int
	for _, event := range runPodLogStream(t, service, PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app"}) {
		if event.Cursor == "" {
			if event.Event == "line" {
				cursorless++
			}
			continue
		}
		if len(event.Cursor) > maxStreamCursorBytes {
			t.Fatalf("issued cursor of %d bytes exceeds the budget", len(event.Cursor))
		}
		if largest < len(event.Cursor) {
			largest = len(event.Cursor)
		}
		if first == 0 {
			first = len(event.Cursor)
		}
		if event.Event == "line" {
			lines++
		}
	}
	if first == 0 {
		t.Fatal("first line carried no resume state at all")
	}
	if cursorless == 0 {
		t.Fatal("resume state never reached the budget, so the fallback is unproven")
	}
	if lines == 0 || largest > maxStreamCursorBytes {
		t.Fatalf("cursors=%d largest=%d", lines, largest)
	}
	// A request built from the largest cursor the stream issued still fits.
	if len(queries) == 0 || len(queries[0]) >= MaxStreamRequestQueryBytes {
		t.Fatalf("upstream query %q", queries)
	}
}

// A viewer that resumes by timestamp asks the backend for that timestamp only:
// no window bounds, no cursor, and nothing that grows with the request.
func TestPodLogStreamResumesFromSinceTime(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.RawQuery)
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z only")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	watermark := time.Now().UTC().Add(-time.Minute)
	events := runPodLogStream(t, service, PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app", SinceTime: watermark})
	if len(events) == 0 || !strings.Contains(queries[0], "sinceTime=") {
		t.Fatalf("resume query = %v", queries)
	}
	parsed := strings.ReplaceAll(url.QueryEscape(watermark.Format(time.RFC3339Nano)), "%3A", ":")
	if !strings.Contains(strings.ReplaceAll(queries[0], "%3A", ":"), parsed) {
		t.Fatalf("resume query did not carry the timestamp: %s", queries[0])
	}
	if strings.Contains(queries[0], "sinceSeconds") || strings.Contains(queries[0], "tailLines") {
		t.Fatalf("timestamp resume kept window bounds: %s", queries[0])
	}
}

// A log clock a little ahead of the server is a container clock difference, not
// a request for logs that cannot exist yet: the resume is clamped to now and the
// read continues. A timestamp beyond the tolerated skew is still refused.
func TestPodLogStreamClampsSmallFutureClockSkew(t *testing.T) {
	if MaxLogClockSkew <= 0 || MaxLogClockSkew > 5*time.Minute {
		t.Fatalf("tolerated clock skew %s is not a small window", MaxLogClockSkew)
	}
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values, _ := url.ParseQuery(request.URL.RawQuery)
		queries = append(queries, values)
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z resumed")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	ahead := time.Now().UTC().Add(3 * time.Second)
	lines := 0
	for _, event := range runPodLogStream(t, service, PodLogStreamOptions{Namespace: "shop", Pod: "api", SinceTime: ahead}) {
		if event.Event == "line" {
			lines++
		}
	}
	if lines != 1 || len(queries) != 1 || !queries[0].Has("sinceTime") {
		t.Fatalf("skewed resume lines=%d queries=%v", lines, queries)
	}
	requested, err := time.Parse(time.RFC3339Nano, queries[0].Get("sinceTime"))
	if err != nil || requested.After(time.Now()) || !requested.Before(ahead) {
		t.Fatalf("skewed resume asked for %q instead of a clamp to now", queries[0].Get("sinceTime"))
	}
	if err := service.ValidatePodLogStream(PodLogStreamOptions{Namespace: "shop", Pod: "api", SinceTime: time.Now().Add(MaxLogClockSkew + time.Minute)}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("far future resume accepted: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("refused resume reached the transport: %v", queries)
	}
}

// A cursor is base64 JSON, not a signature, so a viewer can decode one, rewrite
// the watermark and send it back. What the read window bounds is how far back a
// read starts, not how recent the line a viewer holds is: a Pod whose last line
// is older than the window still issues a cursor on that line, and resuming from
// it is served from the floor instead of being refused. A span the clamp drops is
// reported as an uncertain replay, an aged cursor's line positions cannot hide a
// line inside the window, and a since_time the viewer names directly is still
// refused when it falls outside the window.
func TestPodLogCursorTimestampStaysInsideTheReadWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	aged := now.Add(-26 * 24 * time.Hour)
	recent := now.Add(-time.Minute)
	target := PodLogStreamOptions{Namespace: "shop", Pod: "api"}

	// A quiet Pod whose only line is 26 days old: the cursor the viewer holds
	// names that line, and resuming from it reads the last window, not 26 days.
	t.Run("quiet Pod resumes from the floor", func(t *testing.T) {
		backend := newPodLogFixture(t, podLogFixtureLine{at: aged, text: "quiet-line"})
		service, reads := backend.service, backend.readQueries
		issued := ""
		for _, event := range runPodLogStream(t, service, target) {
			if event.Cursor != "" {
				issued = event.Cursor
			}
		}
		if issued == "" {
			t.Fatal("the stream of a Pod last heard from 26 days ago issued no cursor")
		}
		resume := target
		resume.Cursor = issued
		cursor, err := service.logCursor(resume)
		if err != nil || !cursor.Timestamp.Equal(aged) {
			t.Fatalf("issued watermark = %s, err=%v", cursor.Timestamp, err)
		}
		// The Pod writes a line after the viewer's cursor was minted.
		backend.serve(podLogFixtureLine{at: aged, text: "quiet-line"}, podLogFixtureLine{at: recent, text: "recent"})
		before := time.Now()
		events := runPodLogStream(t, service, resume)
		after := time.Now()
		served := reads()
		if len(served) != 2 {
			t.Fatalf("aged resume served %v", served)
		}
		since := servedSinceTime(t, served[1])
		if since.Before(logReadFloor(before)) || since.After(logReadFloor(after)) {
			t.Fatalf("aged resume asked for %q instead of the window floor", served[1].Get("sinceTime"))
		}
		if !since.After(aged) {
			t.Fatalf("aged resume was not clamped: asks for %s", since.Format(time.RFC3339Nano))
		}
		if !logStreamReplayUncertain(events) {
			t.Fatalf("dropped span reported as certain: %+v", events)
		}
		// The first line the resume hands the viewer says so, which is where a
		// gap between what it holds and what the window can return matters.
		if uncertain, sent := logStreamFirstLineUncertain(events); !sent || !uncertain {
			t.Fatalf("clamped resume sent lines=%t, first line uncertain=%t: %+v", sent, uncertain, events)
		}
		if lines := logStreamLineTexts(events); len(lines) != 1 || lines[0] != "recent" {
			t.Fatalf("clamped resume returned lines older than the floor: %v", lines)
		}
	})

	// The same cursor rewritten to 2018 reaches at most the last window, and the
	// aged line positions it carries do not suppress a line inside that window.
	t.Run("rewritten watermark reads at most the window", func(t *testing.T) {
		backend := newPodLogFixture(t, podLogFixtureLine{at: aged, text: "old"}, podLogFixtureLine{at: recent, text: "recent"})
		service, reads := backend.service, backend.readQueries
		issued := ""
		for _, event := range runPodLogStream(t, service, target) {
			if event.Cursor != "" {
				issued = event.Cursor
			}
		}
		original := decodeLogCursor(t, issued)
		shift := time.Date(2018, 12, 31, 23, 59, 59, 0, time.UTC).Sub(original.Timestamp)
		forged := original
		forged.Timestamp = original.Timestamp.Add(shift)
		forged.Occurrences = make(map[string]uint64, len(original.Occurrences))
		for key, count := range original.Occurrences {
			stamp, parseErr := time.Parse(time.RFC3339Nano, key)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			forged.Occurrences[stamp.Add(shift).Format(time.RFC3339Nano)] = count
		}
		encoded, err := json.Marshal(forged)
		if err != nil {
			t.Fatal(err)
		}
		resume := target
		resume.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
		if err := service.ValidatePodLogStream(resume); err != nil {
			t.Fatalf("rewritten cursor refused: %v", err)
		}
		before := time.Now()
		events := runPodLogStream(t, service, resume)
		after := time.Now()
		served := reads()
		if len(served) != 2 {
			t.Fatalf("rewritten cursor served %v", served)
		}
		since := servedSinceTime(t, served[1])
		if since.Before(logReadFloor(before)) || since.After(logReadFloor(after)) {
			t.Fatalf("rewritten cursor asked for %q instead of the window floor", served[1].Get("sinceTime"))
		}
		if since.Equal(forged.Timestamp) {
			t.Fatalf("rewritten watermark reached the backend: asks for %s", since.Format(time.RFC3339Nano))
		}
		if !logStreamReplayUncertain(events) {
			t.Fatalf("dropped span reported as certain: %+v", events)
		}
		if lines := logStreamLineTexts(events); len(lines) != 1 || lines[0] != "recent" {
			t.Fatalf("aged positions suppressed a line inside the window: %v", lines)
		}
	})

	// A cursor lagging inside the window, including one whose positions reach
	// into the overlap allowance, resumes exactly where it asked. One that falls
	// behind the floor is clamped instead of refused, and says so.
	for _, lag := range []struct {
		name    string
		at      time.Time
		clamped bool
	}{
		{name: "lagging inside the window", at: now.Add(-23 * time.Hour)},
		{name: "inside the resume overlap", at: now.Add(-logReadWindow - logResumeOverlap/2)},
		{name: "beyond the resume overlap", at: now.Add(-logReadWindow - logResumeOverlap - time.Hour), clamped: true},
	} {
		t.Run(lag.name, func(t *testing.T) {
			backend := newPodLogFixture(t, podLogFixtureLine{at: lag.at, text: "held"}, podLogFixtureLine{at: recent, text: "recent"})
			service, reads := backend.service, backend.readQueries
			cursor := logResumeCursor{Target: service.logTarget(target), Timestamp: lag.at, Ordinal: 1, Occurrences: map[string]uint64{lag.at.Format(time.RFC3339Nano): 1}}
			payload, err := json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			resume := target
			resume.Cursor = base64.RawURLEncoding.EncodeToString(payload)
			before := time.Now()
			events := runPodLogStream(t, service, resume)
			after := time.Now()
			served := reads()
			if len(served) != 1 {
				t.Fatalf("lagging resume served %v", served)
			}
			since := servedSinceTime(t, served[0])
			if lag.clamped {
				if since.Before(logReadFloor(before)) || since.After(logReadFloor(after)) {
					t.Fatalf("lagging resume asked for %q instead of the window floor", served[0].Get("sinceTime"))
				}
				if !logStreamReplayUncertain(events) {
					t.Fatalf("clamped resume reported as certain: %+v", events)
				}
				return
			}
			if !since.Equal(lag.at.Add(-time.Nanosecond)) {
				t.Fatalf("lagging resume asked for %q instead of %s", served[0].Get("sinceTime"), lag.at.Format(time.RFC3339Nano))
			}
			if logStreamReplayUncertain(events) {
				t.Fatalf("in-window resume reported a gap: %+v", events)
			}
			if lines := logStreamLineTexts(events); len(lines) != 1 || lines[0] != "recent" {
				t.Fatalf("in-window resume lines = %v", lines)
			}
		})
	}

	// A quiet position is minted from the current clock, so it is always inside
	// the window; one that names an older time is clamped like any other cursor.
	t.Run("aged quiet position reads from the floor", func(t *testing.T) {
		backend := newPodLogFixture(t, podLogFixtureLine{at: recent, text: "recent"})
		service, reads := backend.service, backend.readQueries
		payload, err := json.Marshal(logResumeCursor{Target: service.logTarget(target), Timestamp: aged, Quiet: true, SinceSeconds: 3600, TailLines: 200})
		if err != nil {
			t.Fatal(err)
		}
		resume := target
		resume.Cursor = base64.RawURLEncoding.EncodeToString(payload)
		before := time.Now()
		events := runPodLogStream(t, service, resume)
		after := time.Now()
		served := reads()
		if len(served) != 1 {
			t.Fatalf("quiet resume served %v", served)
		}
		since := servedSinceTime(t, served[0])
		if since.Before(logReadFloor(before)) || since.After(logReadFloor(after)) {
			t.Fatalf("quiet resume asked for %q instead of the window floor", served[0].Get("sinceTime"))
		}
		if served[0].Get("tailLines") != "200" {
			t.Fatalf("quiet resume kept no tail: %v", served[0])
		}
		if !logStreamReplayUncertain(events) {
			t.Fatalf("dropped span reported as certain: %+v", events)
		}
	})

	// A since_time the viewer names directly is not clamped: it either falls
	// inside the window or the request is refused before any read is opened.
	t.Run("viewer since_time outside the window is refused", func(t *testing.T) {
		backend := newPodLogFixture(t, podLogFixtureLine{at: recent, text: "recent"})
		service, reads := backend.service, backend.readQueries
		old := time.Now().UTC().Add(-logReadWindow - time.Hour)
		options := target
		options.SinceTime = old
		err := service.ValidatePodLogStream(options)
		if !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("old since_time accepted: %v", err)
		}
		if strings.Contains(err.Error(), old.Format(time.RFC3339Nano)) {
			t.Fatalf("refusal echoed the timestamp: %v", err)
		}
		if err := service.StreamPodLogs(logStreamTestContext(), options, func(PodLogStreamEvent) error { return nil }); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("old since_time stream returned %v", err)
		}
		if served := reads(); len(served) != 0 {
			t.Fatalf("refused since_time reached the backend: %v", served)
		}
	})
}

// podLogFixtureLine is one line the fake log backend holds.
type podLogFixtureLine struct {
	at   time.Time
	text string
}

// podLogFixture is a fake log backend that answers reads the way the log API
// does: a sinceTime read returns the lines at or after it, a read without one
// returns the whole buffer, and every read it answered is recorded.
type podLogFixture struct {
	service *Service
	mutex   sync.Mutex
	lines   []podLogFixtureLine
	reads   []url.Values
}

func newPodLogFixture(t *testing.T, lines ...podLogFixtureLine) *podLogFixture {
	t.Helper()
	backend := &podLogFixture{lines: lines}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values, _ := url.ParseQuery(request.URL.RawQuery)
		backend.mutex.Lock()
		backend.reads = append(backend.reads, values)
		served := backend.lines
		backend.mutex.Unlock()
		since, _ := time.Parse(time.RFC3339Nano, values.Get("sinceTime"))
		for _, line := range served {
			if values.Has("sinceTime") && line.at.Before(since) {
				continue
			}
			fmt.Fprintf(writer, "%s %s\n", line.at.Format(time.RFC3339Nano), line.text)
		}
	}))
	t.Cleanup(server.Close)
	backend.service = newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	backend.service.SetScrubber(replacingScrubber{})
	return backend
}

// serve replaces what the backend holds, so a test can let the Pod write a line
// between two reads.
func (backend *podLogFixture) serve(lines ...podLogFixtureLine) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.lines = lines
}

// readQueries lists the queries the backend answered.
func (backend *podLogFixture) readQueries() []url.Values {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	return append([]url.Values(nil), backend.reads...)
}

// servedSinceTime reads the start a recorded read asked its backend for.
func servedSinceTime(t *testing.T, values url.Values) time.Time {
	t.Helper()
	since, err := time.Parse(time.RFC3339Nano, values.Get("sinceTime"))
	if err != nil {
		t.Fatalf("read carried no sinceTime: %v", values)
	}
	return since
}

// logStreamReplayUncertain reports whether the stream told its viewer that the
// replay may be missing a span.
func logStreamReplayUncertain(events []PodLogStreamEvent) bool {
	for _, event := range events {
		if event.ReplayUncertain {
			return true
		}
	}
	return false
}

// logStreamLineTexts lists the line text a stream handed its viewer.
func logStreamLineTexts(events []PodLogStreamEvent) []string {
	var texts []string
	for _, event := range events {
		if event.Event == "line" {
			texts = append(texts, event.Text)
		}
	}
	return texts
}

// logStreamFirstLineUncertain reports the replay signal of the first line event
// and whether the stream sent one.
func logStreamFirstLineUncertain(events []PodLogStreamEvent) (bool, bool) {
	for _, event := range events {
		if event.Event == "line" {
			return event.ReplayUncertain, true
		}
	}
	return false, false
}

func decodeLogCursor(t *testing.T, encoded string) logResumeCursor {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var cursor logResumeCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		t.Fatal(err)
	}
	return cursor
}

// A resume timestamp that reaches a read without passing validation — the
// workload overlap derives one from an already accepted timestamp — is still
// bounded: the read is refused rather than asking the backend for a time
// outside the window, and a timestamp inside it reads as before.
func TestPodLogReadRefusesOutOfWindowSinceTime(t *testing.T) {
	var reads []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/log") {
			reads = append(reads, request.URL.RawQuery)
		}
		fmt.Fprintln(writer, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)+" line")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	emit := func(PodLogStreamEvent) error { return nil }
	stream := func(since time.Time) error {
		return service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "shop", Pod: "api", SinceTime: since}, emit, time.Second, time.Second, time.Second, MaxLogStreamBytes)
	}
	outside := time.Now().UTC().Add(-logReadWindow - time.Second)
	if err := stream(outside); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("out-of-window read returned %v", err)
	}
	if len(reads) != 0 {
		t.Fatalf("out-of-window timestamp reached the backend: %v", reads)
	}
	if err := stream(time.Now().UTC().Add(-logReadWindow + time.Minute)); err != nil || len(reads) != 1 {
		t.Fatalf("in-window read queries=%v err=%v", reads, err)
	}
}

// When its resume state stops advancing, a byte-limited read says so on the
// terminal limit event instead of handing back a rotation point that cannot make
// progress, and it names the newest line it sent so a viewer resumes from that
// line. The report is bounded and carries no line text.
func TestPodLogStreamReportsALaggingCursorAtTheByteLimit(t *testing.T) {
	const lineCount = 400
	const budget = 20000
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	var rotations []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values, _ := url.ParseQuery(request.URL.RawQuery)
		since, _ := time.Parse(time.RFC3339Nano, values.Get("sinceTime"))
		if values.Has("sinceTime") {
			rotations = append(rotations, values)
		}
		for index := 0; index < lineCount; index++ {
			timestamp := base.Add(time.Duration(index) * time.Second)
			if timestamp.Before(since) {
				continue
			}
			fmt.Fprintf(writer, "%s line-%d\n", timestamp.Format(time.RFC3339Nano), index)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})

	var limit PodLogStreamEvent
	var newest time.Time
	issuedCursor, cursorAt, lines, wireBytes := "", 0, 0, 0
	err := service.streamPodLogs(context.Background(), PodLogStreamOptions{Namespace: "shop", Pod: "api"}, func(event PodLogStreamEvent) error {
		payload, _ := json.Marshal(event)
		wireBytes += len(payload) + len(event.Event) + len("event: \ndata: \n\n")
		switch event.Event {
		case "line":
			lines++
			timestamp, parseErr := time.Parse(time.RFC3339Nano, event.Timestamp)
			if parseErr != nil {
				t.Fatalf("line %d carries no timestamp: %+v", lines, event)
			}
			newest = timestamp
			if event.Cursor != "" {
				issuedCursor, cursorAt = event.Cursor, lines
			}
		case "limit":
			limit = event
		}
		return nil
	}, time.Second, time.Second, time.Second, budget)
	if err != nil {
		t.Fatalf("byte-limited read returned %v", err)
	}
	// The resume state outgrew the request budget: lines kept coming and cursors
	// stopped, so the terminal event can only name the last position it held.
	if limit.Event != "limit" || limit.Reason != "bytes" || lines < 2*logCursorCheckpoint {
		t.Fatalf("lines=%d limit=%+v", lines, limit)
	}
	if issuedCursor == "" || lines-cursorAt < logCursorCheckpoint {
		t.Fatalf("cursors did not stop before the limit: %d lines, last cursor at %d", lines, cursorAt)
	}
	if limit.Cursor == "" || len(limit.Cursor) > maxStreamCursorBytes {
		t.Fatalf("terminal cursor=%d bytes", len(limit.Cursor))
	}
	if !limit.CursorLagging || limit.NewestTimestamp == "" {
		t.Fatalf("stale cursor reported as current: %+v", limit)
	}
	reported, parseErr := time.Parse(time.RFC3339Nano, limit.NewestTimestamp)
	if parseErr != nil || !reported.Equal(newest) {
		t.Fatalf("lag report %q does not name the newest line %s", limit.NewestTimestamp, newest.Format(time.RFC3339Nano))
	}
	if len(limit.NewestTimestamp) > len("2006-01-02T15:04:05.999999999+07:00") {
		t.Fatalf("lag report timestamp is %d bytes", len(limit.NewestTimestamp))
	}
	stale, cursorErr := service.logCursor(PodLogStreamOptions{Namespace: "shop", Pod: "api", Cursor: limit.Cursor})
	if cursorErr != nil {
		t.Fatalf("terminal cursor unusable: %v", cursorErr)
	}
	if !stale.Timestamp.Before(reported) {
		t.Fatalf("cursor at %s is not behind the newest line %s", stale.Timestamp.Format(time.RFC3339Nano), reported.Format(time.RFC3339Nano))
	}
	if wireBytes > budget {
		t.Fatalf("lag report broke the byte budget: %d > %d", wireBytes, budget)
	}
	// Resuming from the reported line reaches past it instead of replaying what
	// the limited read already sent.
	rotatedLines := 0
	for _, event := range runPodLogStream(t, service, PodLogStreamOptions{Namespace: "shop", Pod: "api", SinceTime: reported}) {
		if event.Event != "line" {
			continue
		}
		timestamp, parseErr := time.Parse(time.RFC3339Nano, event.Timestamp)
		if parseErr != nil || timestamp.Before(reported) {
			t.Fatalf("rotation resumed before the reported line: %+v", event)
		}
		rotatedLines++
	}
	if rotatedLines == 0 || len(rotations) != 1 {
		t.Fatalf("rotation from the reported line read %d lines with queries %v", rotatedLines, rotations)
	}
}

func runPodLogStream(t *testing.T, service *Service, options PodLogStreamOptions) []PodLogStreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 2*time.Second)
	defer cancel()
	var events []PodLogStreamEvent
	if err := service.StreamPodLogs(ctx, options, func(event PodLogStreamEvent) error {
		events = append(events, event)
		return nil
	}); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	return events
}
