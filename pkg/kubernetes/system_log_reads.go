package kubernetes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// Log reads are the two system reads of a Pod's log: an open follow of the
// running container, and one bounded read of the container's previous run.
// They pass the same gate as the list reads, with the Learning.Logs toggle,
// and add what a long-lived read needs: an open follow checks the gate again
// on its own and closes when the gate no longer admits it.
//
// Every line is scrubbed here before it leaves the package. The request is
// written here: only the pods/log subresource, never watch.

// SystemReadLaneLogs is admitted by the saved Learning.Logs toggle. It covers
// both log reads.
const SystemReadLaneLogs SystemReadLane = "logs"

const (
	systemReadPodLog         = "pod_log"
	systemReadPreviousPodLog = "previous_pod_log"
)

const (
	// systemLogLineBytes is the most text one returned line holds, the
	// truncation marker included.
	systemLogLineBytes = 16 << 10
	// systemLogRawLineBytes is the most of one line read before it is
	// scrubbed; the rest of a longer line is read and dropped. It is larger
	// than the returned line so the cut a long line gets is made in scrubbed
	// text.
	systemLogRawLineBytes = 64 << 10
	// systemLogReadBuffer is the read buffer of one stream; one chunk of a
	// line is at most this long.
	systemLogReadBuffer = 32 << 10
	// systemLogBytesPerSecond is the most one follow reads in a second, and
	// the burst it may read at once. A faster log is read more slowly, not
	// dropped.
	systemLogBytesPerSecond = 256 << 10
	// systemLogStreamBytes is the most one follow reads before it closes.
	systemLogStreamBytes = 64 << 20
	// systemLogReplayBytes is the most a follow reads of lines written
	// before it opened.
	systemLogReplayBytes = 1 << 20
	// systemLogReplayWindow is how far back a follow may start.
	systemLogReplayWindow = time.Hour
	// systemLogRecheck is how often an open follow checks the gate again.
	// The saved setting it reads is at most this old too, so a change closes
	// the follow within twice this period.
	systemLogRecheck = 15 * time.Second
	// systemLogStreamsPerProcess and systemLogStreamsPerCluster bound the
	// follows open at once.
	systemLogStreamsPerProcess = 128
	systemLogStreamsPerCluster = 32
	// systemLogLineBuffer is how many scrubbed lines wait for the reader.
	systemLogLineBuffer = 64
	// SystemPreviousLogBytes and SystemPreviousLogLines are the most a
	// previous-run read asks for. A caller may ask for less, never more.
	SystemPreviousLogBytes = 256 << 10
	SystemPreviousLogLines = 2000
	// systemPreviousLogRequestBytes is the limitBytes a previous-run read
	// sends with tailLines. The kubelet applies it from the tail's start, so
	// it is larger than what is kept; the newest lines are kept here.
	systemPreviousLogRequestBytes = 4 << 20
	// systemLogTruncatedMarker ends a line cut to the line limit.
	systemLogTruncatedMarker = " [truncated]"
)

// Refusal reasons for log reads, beside the ones the list reads use.
const (
	// SystemReadRefusedInvalidName is a namespace, Pod or container that is
	// not a valid Kubernetes name. No request is made.
	SystemReadRefusedInvalidName = "invalid_name"
	// SystemReadRefusedStreamLimit is a follow refused because the most
	// follows allowed for the cluster or the process are open.
	SystemReadRefusedStreamLimit = "stream_limit"
)

// SystemReadNotFound is the cause class of a log read whose Pod, container or
// previous run does not exist.
const SystemReadNotFound = "not_found"

// SystemReadUnauthorized is the cause class of a log read the API server
// answered 401. For a Pod log that answer can come from the kubelet of the
// Pod's node, so it fails the read only and leaves the credential alone.
const SystemReadUnauthorized = "unauthorized"

// Close reasons of a SystemLogStream. A stream the gate closes carries the
// gate's refusal reason instead: SystemReadRefusedLaneOff,
// SystemReadRefusedOutOfScope, SystemReadRefusedPolicy,
// SystemReadRefusedCredential, SystemReadRefusedSettings,
// SystemReadRefusedUnknown or SystemReadRefusedScrubberFailed.
const (
	// SystemLogEnded is a stream the API server ended: the container
	// stopped or the Pod is gone.
	SystemLogEnded = "ended"
	// SystemLogCanceled is a stream closed by Close or by the caller's
	// context.
	SystemLogCanceled = "canceled"
	// SystemLogByteBudget is a stream that read its whole byte budget.
	SystemLogByteBudget = "byte_budget"
	// SystemLogReplayBudget is a stream that read its whole budget of lines
	// written before it opened.
	SystemLogReplayBudget = "replay_budget"
	// SystemLogFailed is a stream whose read failed; Err carries the cause
	// class.
	SystemLogFailed = "failed"
	// SystemLogRecheckOverdue is a stream closed because no recheck of the
	// gate passed for two recheck periods.
	SystemLogRecheckOverdue = "recheck_overdue"
)

// SystemLogLine is one scrubbed log line.
type SystemLogLine struct {
	// Timestamp is the time the container runtime gave the line; it is zero
	// when the line carried none.
	Timestamp time.Time `json:"timestamp,omitzero"`
	// Text is the scrubbed line, at most 16 KiB.
	Text string `json:"text"`
	// Truncated is set when the line was cut to the line limit; Text then
	// ends with " [truncated]".
	Truncated bool `json:"truncated,omitempty"`
}

// SystemLogStream is an open follow of one container's log. Read Lines until
// it is closed, then Reason says why the stream ended and Err carries the
// cause class of a failure.
//
// Reconnect: open a new follow with since set to the Timestamp of the last
// line received. The API server starts at that instant inclusive, so the first
// lines of the new stream may repeat lines with exactly that timestamp; drop
// them by timestamp and text. A stream closed by the gate (lane off, out of
// scope, policy, credential) should not be reopened until the gate would
// admit it again: the open is refused the same way.
type SystemLogStream struct {
	lines  chan SystemLogLine
	done   chan struct{}
	cancel context.CancelFunc

	mu     sync.Mutex
	reason string
	err    error
}

// Lines returns the scrubbed lines. The channel is closed when the stream
// ends.
func (stream *SystemLogStream) Lines() <-chan SystemLogLine {
	if stream == nil {
		closed := make(chan SystemLogLine)
		close(closed)
		return closed
	}
	return stream.lines
}

// Close ends the stream and waits until it has stopped reading. It is safe to
// call more than once and from any goroutine.
func (stream *SystemLogStream) Close() {
	if stream == nil {
		return
	}
	stream.end(SystemLogCanceled, nil)
	stream.cancel()
	<-stream.done
}

// Reason says why the stream ended. It is set before Lines is closed and is
// empty while the stream is open.
func (stream *SystemLogStream) Reason() string {
	if stream == nil {
		return ""
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.reason
}

// Err is the failure that ended the stream, a *SystemReadError carrying the
// cause class, or nil when it ended for any other reason.
func (stream *SystemLogStream) Err() error {
	if stream == nil {
		return nil
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.err
}

// end records the first reason the stream ends for; later ones are ignored.
func (stream *SystemLogStream) end(reason string, err error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.reason == "" {
		stream.reason, stream.err = reason, err
	}
}

func (stream *SystemLogStream) ended() string {
	reason, _ := stream.endedWith()
	return reason
}

func (stream *SystemLogStream) endedWith() (string, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.reason, stream.err
}

// SystemPreviousLogLimits lowers what a previous-run read asks for. Zero
// keeps the ceiling; a value above the ceiling is held to it.
type SystemPreviousLogLimits struct {
	Bytes int64
	Lines int
}

// SystemPreviousLog is the scrubbed log of a container's previous run.
type SystemPreviousLog struct {
	Lines []SystemLogLine `json:"lines"`
	// Truncated is set when lines of the run were left out: the oldest,
	// beyond the kept bytes or lines, or the newest, when the run's last
	// SystemPreviousLogLines lines were more than the 4 MiB the read asks
	// for and the API server stopped before the end of the run.
	Truncated  bool      `json:"truncated,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// systemLogLimits are the bounds of log reads. The handle starts with the
// package constants; tests lower them.
type systemLogLimits struct {
	streamBytes int64
	replayBytes int64
	// recheck is how often an open follow checks the gate again; overdue
	// is how long it may go without a passing recheck, zero meaning two
	// periods.
	recheck               time.Duration
	overdue               time.Duration
	streamsPerProcess     int
	streamsPerCluster     int
	previousBytes         int64
	previousLines         int
	lineBytes, rawLineCut int
}

func defaultSystemLogLimits() systemLogLimits {
	return systemLogLimits{
		streamBytes:       systemLogStreamBytes,
		replayBytes:       systemLogReplayBytes,
		recheck:           systemLogRecheck,
		streamsPerProcess: systemLogStreamsPerProcess,
		streamsPerCluster: systemLogStreamsPerCluster,
		previousBytes:     SystemPreviousLogBytes,
		previousLines:     SystemPreviousLogLines,
		lineBytes:         systemLogLineBytes,
		rawLineCut:        systemLogRawLineBytes,
	}
}

// systemLogPace holds a stream to its byte rate.
type systemLogPace interface {
	WaitN(ctx context.Context, n int) error
}

func newSystemLogPace() systemLogPace {
	return rate.NewLimiter(rate.Limit(systemLogBytesPerSecond), systemLogBytesPerSecond)
}

// systemLogStreams counts the open follows; guarded by SystemReads.mu.
type systemLogStreams struct {
	total      int
	perCluster map[string]int
}

func (reads *SystemReads) acquireLogStream(cluster string) (func(), bool) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if reads.logStreams.total >= reads.logLimits.streamsPerProcess || reads.logStreams.perCluster[cluster] >= reads.logLimits.streamsPerCluster {
		return nil, false
	}
	reads.logStreams.total++
	reads.logStreams.perCluster[cluster]++
	var once sync.Once
	return func() {
		once.Do(func() {
			reads.mu.Lock()
			defer reads.mu.Unlock()
			reads.logStreams.total--
			reads.logStreams.perCluster[cluster]--
			if reads.logStreams.perCluster[cluster] <= 0 {
				delete(reads.logStreams.perCluster, cluster)
			}
		})
	}, true
}

// systemLogCause is systemReadCause with the answers that only log reads
// get: a missing Pod, container or previous run.
func systemLogCause(err error) string {
	if errors.Is(err, ErrNotFound) || errors.Is(err, errPreviousUnavailable) {
		return SystemReadNotFound
	}
	if errors.Is(err, ErrUnauthorized) {
		return SystemReadUnauthorized
	}
	return systemReadCause(err)
}

// admitLog runs the gate of a log read: the names are checked first, then the
// gate every system read passes, then the named namespace against the saved
// scope and the policy.
func (reads *SystemReads) admitLog(ctx context.Context, clusterID, namespace, pod, container, read string) (*systemAdmission, error) {
	if reads == nil || !reads.enabled || reads.registry == nil {
		return nil, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	if !validPodLogName(namespace, 63, false) || !validPodLogName(pod, 253, true) || !validPodLogName(container, 63, false) {
		return nil, reads.refuse(systemReadKey{cluster: reads.countedCluster(clusterID), lane: SystemReadLaneLogs, read: read}, SystemReadRefusedInvalidName)
	}
	admission, err := reads.admit(ctx, clusterID, SystemReadLaneLogs, read)
	if err != nil {
		return nil, err
	}
	if ok, reason := reads.allowNamespace(ctx, admission, namespace, true); !ok {
		return nil, reads.refuse(admission.key, reason)
	}
	if admission.service.client == nil || admission.service.client.streamHTTP == nil {
		return nil, reads.unavailable(admission.key, SystemReadRefusedUnknown)
	}
	return admission, nil
}

// systemLogPath is the only request a log read makes.
func systemLogPath(namespace, pod string, query url.Values) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(pod) + "/log?" + query.Encode()
}

// openLog proves the credential, honours the system throttle and opens the
// log request on ctx, which must already be bound to the service's
// credential. Opening is bounded by systemReadTimeout; the body then lives as
// long as ctx.
func (reads *SystemReads) openLog(gate, ctx context.Context, admission *systemAdmission, path string, previous bool) (io.ReadCloser, error) {
	service := admission.service
	if err := service.validateCredential(gate); err != nil {
		return nil, err
	}
	if err := service.client.systemThrottle(); err != nil {
		return nil, err
	}
	request, cancelRequest := context.WithCancel(systemRead(ctx, 0))
	var timedOut atomic.Bool
	timer := time.AfterFunc(systemReadTimeout, func() {
		timedOut.Store(true)
		cancelRequest()
	})
	body, err := service.client.openPodLogStream(request, path, previous)
	if !timer.Stop() && timedOut.Load() {
		if body != nil {
			_ = body.Close()
		}
		cancelRequest()
		return nil, context.DeadlineExceeded
	}
	if err != nil {
		cancelRequest()
		return nil, err
	}
	return &systemLogBody{ReadCloser: body, cancel: cancelRequest}, nil
}

type systemLogBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *systemLogBody) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

// FollowPodLog opens a follow of one container's log on clusterID. since is
// where it starts: zero starts at the current end of the log, and a time is
// held to the last hour and to now. The names must be valid Kubernetes names;
// the container is required.
//
// ctx bounds the whole stream. The open itself is gated and bounded like the
// list reads; once open, the stream checks the gate again on its own and
// closes when the lane is turned off, the namespace leaves the saved scope,
// the policy refuses or the credential is retired. See SystemLogStream for the
// reconnect contract.
func (reads *SystemReads) FollowPodLog(ctx context.Context, clusterID, namespace, pod, container string, since time.Time) (*SystemLogStream, error) {
	if reads == nil || !reads.enabled {
		return nil, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	gate, cancelGate := context.WithTimeout(ctx, reads.callTimeout)
	defer cancelGate()
	admission, err := reads.admitLog(gate, clusterID, namespace, pod, container, systemReadPodLog)
	if err != nil {
		return nil, err
	}
	admittedAt := time.Now()
	release, ok := reads.acquireLogStream(admission.key.cluster)
	if !ok {
		return nil, reads.unavailable(admission.key, SystemReadRefusedStreamLimit)
	}
	now := reads.now()
	query := url.Values{"container": {container}, "follow": {"true"}, "timestamps": {"true"}}
	if since.IsZero() {
		// The API server has no "from the end" for a follow; one second back
		// is the nearest start.
		query.Set("sinceSeconds", "1")
	} else {
		start := since
		if floor := now.Add(-systemLogReplayWindow); start.Before(floor) {
			start = floor
		}
		if start.After(now) {
			start = now
		}
		query.Set("sinceTime", start.UTC().Format(time.RFC3339Nano))
	}
	streamCtx, cancel := context.WithCancel(ctx)
	bound, cancelIdentity := admission.service.credentialContext(streamCtx)
	var tally systemTally
	tally.requests = 1
	body, err := reads.openLog(gate, bound, admission, systemLogPath(namespace, pod, query), false)
	if err != nil {
		cancelIdentity()
		cancel()
		release()
		class := systemLogCause(err)
		tally.cause = class
		return nil, reads.finish(admission, tally, &SystemReadError{Class: class})
	}
	tally.listed = true
	_ = reads.finish(admission, tally, nil)
	stream := &SystemLogStream{lines: make(chan SystemLogLine, systemLogLineBuffer), done: make(chan struct{}), cancel: cancel}
	run := &systemLogRun{
		reads: reads, admission: admission, stream: stream,
		cluster: clusterID, namespace: namespace,
		parent: ctx, openedAt: now, admittedAt: admittedAt,
	}
	run.lastRecheck.Store(admittedAt.UnixNano())
	reads.countLogStream(admission.key, 1)
	go run.recheckLoop(streamCtx)
	go run.watchdog(streamCtx)
	go func() {
		reason, failure := run.follow(streamCtx, body)
		stream.end(reason, failure)
		reason, failure = stream.endedWith()
		_ = body.Close()
		cancel()
		cancelIdentity()
		reads.closeLogStream(admission.key, run, reason, failure)
		// The slot is free before Lines closes, so a reader that reopens as
		// soon as the stream ends is not refused for a slot still held.
		release()
		close(stream.lines)
		close(stream.done)
	}()
	return stream, nil
}

// PreviousPodLog reads the log of one container's previous run on clusterID,
// keeping its newest lines up to SystemPreviousLogBytes and
// SystemPreviousLogLines; limits may lower both. It asks the API server for
// the run's last SystemPreviousLogLines lines, at most 4 MiB; the API server
// reads those from their start, so when they are larger it stops before the
// end of the run and the newest lines are the ones missing. Truncated reports
// either loss: older lines dropped here, or newest lines never sent. The names must be valid Kubernetes names; the container is required.
// A container with no previous run answers with class not_found.
func (reads *SystemReads) PreviousPodLog(ctx context.Context, clusterID, namespace, pod, container string, limits SystemPreviousLogLimits) (SystemPreviousLog, error) {
	if reads == nil || !reads.enabled {
		return SystemPreviousLog{}, &SystemReadError{Class: SystemReadRefused, Reason: SystemReadRefusedNotEnabled}
	}
	ctx, cancel := context.WithTimeout(ctx, reads.callTimeout)
	defer cancel()
	admission, err := reads.admitLog(ctx, clusterID, namespace, pod, container, systemReadPreviousPodLog)
	if err != nil {
		return SystemPreviousLog{}, err
	}
	release, ok := reads.acquireLogStream(admission.key.cluster)
	if !ok {
		return SystemPreviousLog{}, reads.unavailable(admission.key, SystemReadRefusedStreamLimit)
	}
	defer release()
	maxBytes, maxLines := reads.logLimits.previousBytes, reads.logLimits.previousLines
	if limits.Bytes > 0 && limits.Bytes < maxBytes {
		maxBytes = limits.Bytes
	}
	if limits.Lines > 0 && limits.Lines < maxLines {
		maxLines = limits.Lines
	}
	// The kubelet seeks to the last tailLines lines and then stops after
	// limitBytes, so a limitBytes at the kept size would lose the newest
	// lines, the ones that say why the container stopped. It is asked for a
	// larger, still bounded, amount and the newest lines are kept here.
	query := url.Values{
		"container":  {container},
		"previous":   {"true"},
		"timestamps": {"true"},
		"limitBytes": {strconv.FormatInt(systemPreviousLogRequestBytes, 10)},
		"tailLines":  {strconv.Itoa(maxLines)},
	}
	bound, cancelIdentity := admission.service.credentialContext(ctx)
	defer cancelIdentity()
	tally := systemTally{requests: 1}
	body, err := reads.openLog(ctx, bound, admission, systemLogPath(namespace, pod, query), true)
	if err != nil {
		class := systemLogCause(err)
		tally.cause = class
		return SystemPreviousLog{}, reads.finish(admission, tally, &SystemReadError{Class: class})
	}
	defer func() { _ = body.Close() }()
	result := SystemPreviousLog{Lines: []SystemLogLine{}}
	reader := bufio.NewReaderSize(body, systemLogReadBuffer)
	var total, kept int64
	account := func(count int) error {
		total += int64(count)
		if total > systemPreviousLogRequestBytes {
			return errSystemLogBudget
		}
		return nil
	}
	// keep adds a line and drops the oldest ones beyond the kept bytes and
	// lines, so the end of the run is what is returned.
	keep := func(line SystemLogLine) {
		result.Lines = append(result.Lines, line)
		kept += int64(len(line.Text))
		for len(result.Lines) > 1 && (kept > maxBytes || len(result.Lines) > maxLines) {
			kept -= int64(len(result.Lines[0].Text))
			result.Lines = result.Lines[1:]
			result.Truncated = true
		}
		if kept > maxBytes {
			// One line alone over a lowered byte limit is cut to it.
			only := &result.Lines[0]
			only.Text = cutUTF8(only.Text, int(max(maxBytes-int64(len(systemLogTruncatedMarker)), 0))) + systemLogTruncatedMarker
			only.Truncated = true
			kept = int64(len(only.Text))
			result.Truncated = true
		}
	}
	for {
		raw, _, cut, readErr := readSystemLogLine(reader, reads.logLimits.rawLineCut, account)
		if errors.Is(readErr, errSystemLogBudget) {
			// The API server sent more than was asked; the part line that
			// crossed the limit is dropped.
			result.Truncated = true
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			tally.bytes = total
			class := systemLogCause(readErr)
			if ctx.Err() != nil {
				class = SystemReadTimeout
			}
			tally.cause = class
			return SystemPreviousLog{}, reads.finish(admission, tally, &SystemReadError{Class: class})
		}
		if len(raw) > 0 && readErr != nil {
			// The runtime ends every line it writes, so a last line with no
			// line end was cut, by limitBytes or a short read: it is treated
			// as cut here too.
			cut = true
		}
		if total >= systemPreviousLogRequestBytes {
			result.Truncated = true
		}
		if len(raw) > 0 || cut {
			line, ok := reads.shapeLogLine(raw, cut)
			if !ok {
				tally.bytes = total
				tally.cause = SystemReadRefused
				_ = reads.finish(admission, tally, nil)
				return SystemPreviousLog{}, reads.refuse(admission.key, SystemReadRefusedScrubberFailed)
			}
			keep(line)
		}
		if readErr != nil {
			break
		}
	}
	var truncatedLines uint64
	for _, line := range result.Lines {
		if line.Truncated {
			truncatedLines++
		}
	}
	tally.bytes, tally.listed = total, true
	reads.countLogLines(admission.key, uint64(len(result.Lines)), truncatedLines)
	result.ObservedAt = reads.now().UTC()
	return result, reads.finish(admission, tally, nil)
}

var errSystemLogBudget = errors.New("kubernetes: system log byte budget reached")

// readSystemLogLine reads one line. It keeps at most keep bytes of it,
// without the line end, and reads and drops the rest; cut reports a dropped
// part. account is told every chunk read, before the chunk is kept, and may
// stop the read. err is io.EOF with the last line when the body ends.
func readSystemLogLine(reader *bufio.Reader, keep int, account func(int) error) (line []byte, consumed int, cut bool, err error) {
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			consumed += len(chunk)
			if accountErr := account(len(chunk)); accountErr != nil {
				return nil, consumed, cut, accountErr
			}
			content := bytes.TrimSuffix(chunk, []byte("\n"))
			if room := keep - len(line); len(content) > room {
				line = append(line, content[:max(room, 0)]...)
				cut = true
			} else {
				line = append(line, content...)
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\r")), consumed, cut, readErr
	}
}

// shapeLogLine turns one raw line into the only form that leaves the
// package: the runtime timestamp split off, the text made valid UTF-8 and
// scrubbed, then cut to the line limit with a marker. A line cut while it was
// read also loses its last, partial word after scrubbing, so no fragment of a
// secret split by the cut is returned. ok is false when the scrubber failed.
func (reads *SystemReads) shapeLogLine(raw []byte, cut bool) (SystemLogLine, bool) {
	text := string(raw)
	var timestamp time.Time
	stamp, rest, found := strings.Cut(text, " ")
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		timestamp = parsed
		text = rest
		if !found {
			text = ""
		}
	}
	scrubbed, ok := reads.scrub(strings.ToValidUTF8(text, ""))
	if !ok {
		return SystemLogLine{}, false
	}
	truncated := cut
	if cut {
		if last := strings.LastIndexAny(scrubbed, " \t"); last >= 0 {
			scrubbed = scrubbed[:last]
		} else {
			scrubbed = ""
		}
	}
	limit := reads.logLimits.lineBytes
	if len(scrubbed) > limit {
		truncated = true
	}
	if truncated {
		scrubbed = cutUTF8(scrubbed, limit-len(systemLogTruncatedMarker)) + systemLogTruncatedMarker
	}
	return SystemLogLine{Timestamp: timestamp, Text: scrubbed, Truncated: truncated}, true
}

// cutUTF8 shortens text to at most limit bytes without splitting a rune.
func cutUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// systemLogRun is one open follow.
type systemLogRun struct {
	reads     *SystemReads
	admission *systemAdmission
	stream    *SystemLogStream
	cluster   string
	namespace string
	// parent is the caller's context; its end closes the stream as
	// canceled.
	parent   context.Context
	openedAt time.Time
	// admittedAt is when the gate admitted the stream; lastRecheck is the
	// time, in Unix nanoseconds, of the admission or the last recheck that
	// passed.
	admittedAt  time.Time
	lastRecheck atomic.Int64
	// lines, bytes and truncated are counted as the stream reads and moved
	// into the counters at every recheck and at the close.
	lines, bytes, truncated atomic.Uint64
}

// follow reads the stream until it ends and returns the close reason.
func (run *systemLogRun) follow(ctx context.Context, body io.Reader) (string, error) {
	reads := run.reads
	limits := reads.logLimits
	pace := reads.newLogPace()
	reader := bufio.NewReaderSize(body, systemLogReadBuffer)
	var total, replay int64
	account := func(count int) error {
		total += int64(count)
		run.bytes.Add(uint64(count))
		if total > limits.streamBytes {
			return errSystemLogBudget
		}
		return pace.WaitN(ctx, count)
	}
	for {
		raw, consumed, cut, readErr := readSystemLogLine(reader, limits.rawLineCut, account)
		if errors.Is(readErr, errSystemLogBudget) {
			return SystemLogByteBudget, nil
		}
		if len(raw) > 0 && readErr != nil {
			// The read stopped inside this line, on an error, a close or
			// the end of a body that did not end the line: it is treated
			// as cut, so a secret the stop split is not returned in part.
			cut = true
		}
		if len(raw) > 0 || cut {
			line, ok := reads.shapeLogLine(raw, cut)
			if !ok {
				return SystemReadRefusedScrubberFailed, nil
			}
			if ctx.Err() != nil {
				// Nothing is returned once the stream is closing.
				return run.stopped(ctx.Err())
			}
			if !line.Timestamp.IsZero() && line.Timestamp.Before(run.openedAt) {
				replay += int64(consumed)
				if replay > limits.replayBytes {
					return SystemLogReplayBudget, nil
				}
			}
			select {
			case run.stream.lines <- line:
				run.lines.Add(1)
				if line.Truncated {
					run.truncated.Add(1)
				}
			case <-ctx.Done():
				return run.stopped(ctx.Err())
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				return SystemLogEnded, nil
			}
			return run.stopped(readErr)
		}
	}
}

// stopped names why a read that did not end on its own stopped: a reason
// already given (Close or the recheck), the caller's context, a retired
// credential, or a failure with its cause class.
func (run *systemLogRun) stopped(err error) (string, error) {
	if reason := run.stream.ended(); reason != "" {
		return reason, nil
	}
	if run.parent.Err() != nil {
		return SystemLogCanceled, nil
	}
	if identity := run.admission.service.identity; identity != nil && identity.ctx.Err() != nil {
		return SystemReadRefusedCredential, nil
	}
	return SystemLogFailed, &SystemReadError{Class: systemLogCause(err)}
}

// recheckLoop checks the gate again every recheck period while the stream is
// open, and closes the stream when the gate refuses.
//
// The first recheck is one period after the admission, not after the open,
// so a slow open does not stretch the close bound.
func (run *systemLogRun) recheckLoop(ctx context.Context) {
	period := run.reads.logLimits.recheck
	timer := time.NewTimer(run.firstRecheck())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			run.reads.flushLogCounts(run)
			reason := run.recheck(ctx)
			// A refusal seen while the stream is already closing is the
			// close's doing, not the gate's; the close names itself.
			if ctx.Err() != nil {
				return
			}
			if reason != "" {
				run.close(reason)
				return
			}
			run.lastRecheck.Store(time.Now().UnixNano())
			timer.Reset(period)
		}
	}
}

// firstRecheck is how long after now the first recheck is due: one period
// after the admission.
func (run *systemLogRun) firstRecheck() time.Duration {
	return max(run.reads.logLimits.recheck-time.Since(run.admittedAt), 0)
}

// watchdog closes the stream when no recheck has passed for two periods: a
// recheck that cannot finish, on a store or policy that does not answer,
// must not leave the stream unchecked.
func (run *systemLogRun) watchdog(ctx context.Context) {
	overdue := run.reads.logLimits.overdue
	if overdue <= 0 {
		overdue = 2 * run.reads.logLimits.recheck
	}
	ticker := time.NewTicker(max(overdue/8, time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, run.lastRecheck.Load())) > overdue {
				run.close(SystemLogRecheckOverdue)
				return
			}
		}
	}
}

func (run *systemLogRun) close(reason string) {
	run.stream.end(reason, nil)
	run.stream.cancel()
}

// recheck asks the gate again for an open stream: the same client and
// credential still serve the cluster and is not held for recovery, the lane is on, the
// namespace is in the saved scope, the policy agrees for the cluster and the
// namespace, and the credential is still accepted. It returns the refusal
// reason, or "" when the stream may go on.
func (run *systemLogRun) recheck(ctx context.Context) string {
	reads := run.reads
	if ctx.Err() != nil {
		return ""
	}
	gate, cancel := context.WithTimeout(ctx, systemReadTimeout)
	defer cancel()
	service, err := reads.registry.LocalReportService(reads.orgID, run.cluster)
	switch {
	case err != nil && (errors.Is(err, ErrClusterNotFound) || errors.Is(err, ErrClusterRequired)):
		return SystemReadRefusedUnknown
	case err != nil, service.client != run.admission.service.client, service.identity != run.admission.service.identity, service.recovery.requestGateError(reads.orgID) != nil:
		return SystemReadRefusedCredential
	}
	learning, err := reads.learningWithin(service, run.cluster, reads.logLimits.recheck)
	switch {
	case err != nil:
		return SystemReadRefusedSettings
	case !learning.Logs:
		return SystemReadRefusedLaneOff
	case !learning.Scope.Allows(run.namespace):
		return SystemReadRefusedOutOfScope
	case !reads.askPolicy(gate, run.cluster, SystemReadLaneLogs, ""), !reads.askPolicy(gate, run.cluster, SystemReadLaneLogs, run.namespace):
		return SystemReadRefusedPolicy
	}
	if service.validateCredential(gate) != nil {
		return SystemReadRefusedCredential
	}
	return ""
}

func (reads *SystemReads) countLogStream(key systemReadKey, delta int) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	reads.counterLocked(key).StreamsOpen += delta
}

// flushLogCounts moves a stream's line, byte and truncation counts into the
// counters.
func (reads *SystemReads) flushLogCounts(run *systemLogRun) {
	lines, received, truncated := run.lines.Swap(0), run.bytes.Swap(0), run.truncated.Swap(0)
	reads.mu.Lock()
	defer reads.mu.Unlock()
	counter := reads.counterLocked(run.admission.key)
	counter.Lines += lines
	counter.Bytes += int64(received)
	counter.Truncated += truncated
}

func (reads *SystemReads) countLogLines(key systemReadKey, lines, truncated uint64) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	counter := reads.counterLocked(key)
	counter.Lines += lines
	counter.Truncated += truncated
}

// closeLogStream records a stream's close: its last counts, the close reason
// and, for a failure, its cause class.
func (reads *SystemReads) closeLogStream(key systemReadKey, run *systemLogRun, reason string, failure error) {
	reads.flushLogCounts(run)
	class := ""
	if failure != nil {
		class = systemReadCause(failure)
	}
	reads.mu.Lock()
	counter := reads.counterLocked(key)
	counter.StreamsOpen--
	if counter.Closes == nil {
		counter.Closes = map[string]uint64{}
	}
	counter.Closes[reason]++
	if class != "" {
		if counter.Causes == nil {
			counter.Causes = map[string]uint64{}
		}
		counter.Causes[class]++
		counter.LastCause = class
	}
	reads.mu.Unlock()
	logging.Debugf("kubernetes: cluster %s system log stream closed (%s)", key.cluster, reason)
}
