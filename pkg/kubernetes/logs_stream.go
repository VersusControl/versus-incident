package kubernetes

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	MaxLogStreamLineBytes  = 16 << 10
	MaxLogStreamBytes      = 16 << 20
	MaxLogStreamDuration   = 30 * time.Minute
	LogStreamHeartbeat     = 15 * time.Second
	LogStreamIdleTimeout   = 2 * time.Minute
	MaxPreviousLogDuration = 30 * time.Second
	MaxOrgLogStreams       = 4
	MaxProcessLogStreams   = 20
	MaxProcessLogReads     = 200
	logResumeOverlap       = 2 * time.Minute
	maxLogCursorTimestamps = 64
	logCursorCheckpoint    = 32
	// MaxStreamRequestQueryBytes bounds one log stream request's query. A resume
	// no longer has to carry a whole workload's state, so a request that needs
	// more is refused with an explicit answer instead of being left to an
	// intermediary's header limit.
	MaxStreamRequestQueryBytes = 2 << 10
	// maxStreamCursorBytes keeps an issued Pod cursor small enough to come back
	// in a request that still fits the query bound, with room for the other
	// parameters. A resume state that does not fit becomes a line timestamp,
	// which every line already carries.
	maxStreamCursorBytes = MaxStreamRequestQueryBytes - 512
	// maxRetainedCursorBytes bounds a cursor that stays inside this process. A
	// workload source's position is never sent anywhere, so it keeps the line
	// identity a viewer's request could not carry.
	maxRetainedCursorBytes = 16 << 10
)

var ErrLogStreamBusy = errors.New("kubernetes: log stream concurrency limit reached")

var logReadAdmissions = struct {
	sync.Mutex
	total int
}{}

func acquireLogRead() (func(), error) {
	logReadAdmissions.Lock()
	defer logReadAdmissions.Unlock()
	if logReadAdmissions.total >= MaxProcessLogReads {
		return nil, ErrLogStreamBusy
	}
	logReadAdmissions.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			logReadAdmissions.Lock()
			logReadAdmissions.total--
			logReadAdmissions.Unlock()
		})
	}, nil
}

var podLogNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var previousContainerMissingPattern = regexp.MustCompile(`^previous terminated container "[^"\r\n]{1,253}" in pod "[^"\r\n]{1,253}" not found$`)

func validPodLogName(value string, maxBytes int, subdomain bool) bool {
	if value == "" || len(value) > maxBytes || !subdomain && strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) > 63 || !podLogNamePattern.MatchString(label) {
			return false
		}
	}
	return true
}

type PodLogStreamOptions struct {
	Namespace    string
	Pod          string
	Container    string
	SinceSeconds int
	TailLines    int
	// SinceTime resumes from a timestamp instead of a cursor, the way kubectl
	// logs -f does. It is the resume a client keeps when the line-level cursor
	// would not fit in the request budget.
	SinceTime  time.Time
	Timestamps bool
	Previous   bool
	Cursor     string
	// sourceIdentity binds a workload checkpoint to the Pod UID and incarnation.
	sourceIdentity      string
	checkpointEveryLine bool
	finite              bool
	// derivedSince marks a start this connector computed from the viewer's
	// watermark instead of one the viewer sent, so the read pulls it back to the
	// window floor rather than refusing it.
	derivedSince bool
}

type PodLogStreamEvent struct {
	Event           string `json:"-"`
	Text            string `json:"text"`
	Container       string `json:"container"`
	Timestamp       string `json:"timestamp,omitempty"`
	Sequence        uint64 `json:"sequence,omitempty"`
	Ordinal         uint64 `json:"ordinal,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Code            string `json:"code,omitempty"`
	Message         string `json:"message,omitempty"`
	Action          string `json:"action,omitempty"`
	Retryable       bool   `json:"retryable,omitempty"`
	ReplayUncertain bool   `json:"replay_uncertain,omitempty"`
	// CursorLagging marks a terminal limit event whose cursor never caught up
	// with the newest line this stream sent: rotating from that cursor alone
	// replays the same bytes and stops at the same limit. NewestTimestamp is the
	// newest emitted line's timestamp, bounded and free of line text, so a viewer
	// resumes from the line instead of guessing. Both are absent when the cursor
	// is current.
	CursorLagging   bool   `json:"cursor_lagging,omitempty"`
	NewestTimestamp string `json:"newest_timestamp,omitempty"`
}

type logResumeCursor struct {
	Target       string            `json:"target"`
	Timestamp    time.Time         `json:"timestamp"`
	Ordinal      uint64            `json:"ordinal"`
	Occurrences  map[string]uint64 `json:"occurrences,omitempty"`
	Quiet        bool              `json:"quiet,omitempty"`
	SinceSeconds int               `json:"since_seconds,omitempty"`
	TailLines    int               `json:"tail_lines,omitempty"`
}

func logBounds(since, tail int) (int, int) {
	if since == 0 {
		since = 3600
	}
	if tail == 0 {
		tail = 500
	}
	return since, tail
}

func (service *Service) quietLogCursor(options PodLogStreamOptions, now time.Time) string {
	since, tail := logBounds(options.SinceSeconds, options.TailLines)
	encoded, _ := json.Marshal(logResumeCursor{Target: service.logTarget(options), Timestamp: now.Add(-time.Duration(since) * time.Second), Quiet: true, SinceSeconds: since, TailLines: tail})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

var logStreamAdmissions = struct {
	sync.Mutex
	total int
	orgs  map[string]int
}{orgs: make(map[string]int)}

func acquireLogStream(org string) (func(), error) {
	logStreamAdmissions.Lock()
	defer logStreamAdmissions.Unlock()
	if logStreamAdmissions.total >= MaxProcessLogStreams || logStreamAdmissions.orgs[org] >= MaxOrgLogStreams {
		return nil, ErrLogStreamBusy
	}
	logStreamAdmissions.total++
	logStreamAdmissions.orgs[org]++
	var once sync.Once
	return func() {
		once.Do(func() {
			logStreamAdmissions.Lock()
			defer logStreamAdmissions.Unlock()
			logStreamAdmissions.total--
			logStreamAdmissions.orgs[org]--
			if logStreamAdmissions.orgs[org] == 0 {
				delete(logStreamAdmissions.orgs, org)
			}
		})
	}, nil
}

func (service *Service) ValidatePodLogStream(options PodLogStreamOptions) error {
	if service == nil || service.client == nil || service.client.streamHTTP == nil {
		return ErrInvalidEndpoint
	}
	if !validPodLogName(options.Namespace, 63, false) || !validPodLogName(options.Pod, 253, true) || options.Container != "" && !validPodLogName(options.Container, 63, false) || options.SinceSeconds < 0 || options.SinceSeconds > 86400 || options.TailLines < 0 || options.TailLines > 5000 {
		return ErrInvalidArguments
	}
	// A cursor and a timestamp are two answers to the same question, so only one
	// of them may set the resume point, and the timestamp has to be one this
	// connector can still serve.
	if !options.SinceTime.IsZero() {
		if options.Cursor != "" || options.SinceSeconds != 0 || options.TailLines != 0 {
			return ErrInvalidArguments
		}
		if _, ok := boundedSinceTime(options.SinceTime, time.Now()); !ok {
			return ErrInvalidArguments
		}
	}
	_, err := service.logCursor(options)
	return err
}

func (service *Service) logTarget(options PodLogStreamOptions) string {
	target := service.cacheKey() + "\x00" + options.Namespace + "\x00" + options.Pod + "\x00" + options.Container + "\x00" + strconv.FormatBool(options.Previous)
	if options.sourceIdentity != "" {
		target += "\x00" + options.sourceIdentity
	}
	digest := sha256.Sum256([]byte(target))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (service *Service) logCursor(options PodLogStreamOptions) (logResumeCursor, error) {
	if options.Cursor == "" {
		return logResumeCursor{}, nil
	}
	if len(options.Cursor) > maxRetainedCursorBytes || options.SinceSeconds != 0 || options.TailLines != 0 {
		return logResumeCursor{}, ErrInvalidArguments
	}
	raw, err := base64.RawURLEncoding.DecodeString(options.Cursor)
	if err != nil {
		return logResumeCursor{}, ErrInvalidArguments
	}
	var cursor logResumeCursor
	// The cursor is unsigned, so everything in it is untrusted input: it names
	// the target it resumes, and its watermark has to be a real position this
	// connector can resume at. The window is not enforced here: a watermark is
	// the timestamp of a line the Pod wrote, and a Pod that has been quiet for
	// longer than the window legitimately issues one. What the window bounds is
	// how far back the read starts, which is applied when the request is built,
	// so an aged watermark is clamped there instead of refusing a resume of the
	// same target.
	now := time.Now()
	if json.Unmarshal(raw, &cursor) != nil || cursor.Target != service.logTarget(options) || cursor.Timestamp.IsZero() || cursor.Ordinal > 1<<53 || cursor.Timestamp.After(now.Add(MaxLogClockSkew)) || cursor.SinceSeconds < 0 || cursor.SinceSeconds > 86400 || cursor.TailLines < 0 || cursor.TailLines > 5000 {
		return logResumeCursor{}, ErrInvalidArguments
	}
	if cursor.Quiet {
		if cursor.Ordinal != 0 || len(cursor.Occurrences) != 0 || cursor.SinceSeconds == 0 || cursor.TailLines == 0 {
			return logResumeCursor{}, ErrInvalidArguments
		}
		return cursor, nil
	}
	if cursor.Ordinal == 0 {
		return logResumeCursor{}, ErrInvalidArguments
	}
	if len(cursor.Occurrences) > maxLogCursorTimestamps {
		return logResumeCursor{}, ErrInvalidArguments
	}
	// Line positions are identities within the watermark's own resume overlap,
	// not read starts, so they are measured against the watermark and never
	// against the window. An occurrence older than the floor is therefore a
	// normal part of a quiet Pod's cursor: it fails nothing, and because a read
	// clamped to the floor cannot reach a line that old, it cannot hide a line
	// the clamped window does return.
	for key, count := range cursor.Occurrences {
		timestamp, err := time.Parse(time.RFC3339Nano, key)
		if err != nil || count == 0 || count > 1<<53 || timestamp.After(cursor.Timestamp) || timestamp.Before(cursor.Timestamp.Add(-logResumeOverlap)) {
			return logResumeCursor{}, ErrInvalidArguments
		}
	}
	if cursor.Occurrences == nil {
		cursor.Occurrences = map[string]uint64{cursor.Timestamp.Format(time.RFC3339Nano): cursor.Ordinal}
	} else if cursor.Occurrences[cursor.Timestamp.Format(time.RFC3339Nano)] != cursor.Ordinal {
		return logResumeCursor{}, ErrInvalidArguments
	}
	return cursor, nil
}

func (client *Client) openPodLogStream(ctx context.Context, apiPath string, previous bool) (io.ReadCloser, error) {
	if err := client.waitBackoff(ctx); err != nil {
		return nil, err
	}
	requestURL, err := client.requestURL(apiPath)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, ErrInvalidArguments
	}
	request.Header.Set("Accept", "*/*")
	authGeneration := client.authGeneration.Load()
	if client.credentials != nil {
		credentialContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		authorization, credentialErr := client.credentials.Authorization(credentialContext)
		cancel()
		if credentialErr != nil {
			return nil, ErrCredentialUnavailable
		}
		if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
			if err := service.acceptCredential(ctx, authorization); err != nil {
				return nil, err
			}
		}
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.streamHTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrRedirect) {
			return nil, ErrRedirect
		}
		return nil, &connectionError{cause: err}
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			client.rejectRequestCredential(request.Header.Get("Authorization"), authGeneration, recoveryFromContext(ctx))
		}
		if previous && response.StatusCode == http.StatusBadRequest {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			var status struct {
				Kind       string `json:"kind"`
				APIVersion string `json:"apiVersion"`
				Status     string `json:"status"`
				Reason     string `json:"reason"`
				Code       int    `json:"code"`
				Message    string `json:"message"`
			}
			if readErr == nil && len(body) <= 4096 && json.Unmarshal(body, &status) == nil && status.Kind == "Status" && status.APIVersion == "v1" && status.Status == "Failure" && status.Reason == "BadRequest" && status.Code == http.StatusBadRequest && previousContainerMissingPattern.MatchString(status.Message) {
				return nil, errPreviousUnavailable
			}
		}
		if response.StatusCode == http.StatusForbidden {
			// A real cluster 403 names what it denied; keep only those fields.
			body, readErr := io.ReadAll(io.LimitReader(response.Body, forbiddenStatusLimit+1))
			if readErr == nil {
				if denied := permissionDeniedFromStatus(body); denied != nil {
					return nil, denied
				}
			}
			return nil, ErrForbidden
		}
		return nil, classifyStatus(response.StatusCode)
	}
	return response.Body, nil
}

type logStreamRead struct {
	line      string
	err       error
	bytes     int
	partial   bool
	oversized bool
}
type logActivityReader struct {
	source   io.Reader
	activity chan struct{}
}

func (reader logActivityReader) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if count > 0 {
		select {
		case reader.activity <- struct{}{}:
		default:
		}
	}
	return count, err
}

func (service *Service) StreamPodLogs(ctx context.Context, options PodLogStreamOptions, emit func(PodLogStreamEvent) error) error {
	if emit == nil {
		return errors.New("kubernetes: log stream emitter unavailable")
	}
	session, err := service.OpenPodLogStream(ctx, options)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Stream(emit)
}

// PodLogSession reserves viewer and upstream capacity before SSE headers are
// committed. Close cancels an active reader; capacity is released after it exits.
type PodLogSession struct {
	service   *Service
	options   PodLogStreamOptions
	ctx       context.Context
	cancel    context.CancelFunc
	release   func()
	mu        sync.Mutex
	streaming bool
	closed    bool
}

func (service *Service) OpenPodLogStream(ctx context.Context, options PodLogStreamOptions) (*PodLogSession, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return nil, ErrForbidden
	}
	if err := service.ValidatePodLogStream(options); err != nil {
		return nil, err
	}
	if service.scrubber == nil {
		return nil, errors.New("kubernetes: log stream scrubber unavailable")
	}
	ctx, stopAuthorization, err := service.logStreamAccessContext(ctx)
	if err != nil {
		return nil, err
	}
	release, err := acquireLogStream(service.scope.OrgID)
	if err != nil {
		stopAuthorization()
		return nil, err
	}
	releaseRead, err := acquireLogRead()
	if err != nil {
		release()
		stopAuthorization()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stopService := func() bool { return false }
	if service.logStreamContext != nil {
		stopService = context.AfterFunc(service.logStreamContext, cancel)
	}
	return &PodLogSession{
		service: service, options: options, ctx: ctx,
		cancel:  func() { stopService(); cancel(); stopAuthorization() },
		release: func() { releaseRead(); release() },
	}, nil
}

func (session *PodLogSession) Close() {
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.closed {
		session.closed = true
		session.cancel()
		if !session.streaming {
			session.release()
		}
	}
}

func (session *PodLogSession) Stream(emit func(PodLogStreamEvent) error) error {
	session.mu.Lock()
	if session.closed || session.streaming || emit == nil {
		session.mu.Unlock()
		return ErrInvalidArguments
	}
	session.streaming = true
	session.mu.Unlock()
	defer session.Close()
	defer session.release()
	return session.service.readPodLogs(session.ctx, session.options, emit, MaxLogStreamDuration, LogStreamIdleTimeout, LogStreamHeartbeat, MaxLogStreamBytes)
}

func (service *Service) streamPodLogs(parent context.Context, options PodLogStreamOptions, emit func(PodLogStreamEvent) error, duration, idleDuration, heartbeatDuration time.Duration, maxBytes int64) error {
	release, err := acquireLogRead()
	if err != nil {
		return err
	}
	defer release()
	return service.readPodLogs(parent, options, emit, duration, idleDuration, heartbeatDuration, maxBytes)
}

// readPodLogs uses an already reserved upstream slot, without viewer admission.
func (service *Service) readPodLogs(parent context.Context, options PodLogStreamOptions, emit func(PodLogStreamEvent) error, duration, idleDuration, heartbeatDuration time.Duration, maxBytes int64) error {
	if err := service.validateCredential(parent); err != nil {
		return err
	}
	parent, cancelCredential := service.credentialContext(parent)
	defer cancelCredential()
	if (options.Previous || options.finite) && duration > MaxPreviousLogDuration {
		duration = MaxPreviousLogDuration
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	cursor, _ := service.logCursor(options)
	// A cursor can ask to resume from a line older than the window, because a
	// quiet Pod's last line still carries the position its viewer holds. The
	// read starts at the floor instead, which is the most the window allows; a
	// start the clamp moved is reported as an uncertain replay so the viewer can
	// say a gap may exist between what it holds and what this read can return.
	replayUncertain := false
	query := url.Values{"follow": {strconv.FormatBool(!options.Previous && !options.finite)}, "previous": {strconv.FormatBool(options.Previous)}, "timestamps": {"true"}}
	if options.Container != "" {
		query.Set("container", options.Container)
	}
	if !options.SinceTime.IsZero() {
		// Resuming by timestamp, the way kubectl logs -f does: the boundary lines
		// are replayed rather than lost, and the client drops exact duplicates by
		// line identity. A timestamp the caller's clock put a little ahead of this
		// server is clamped, so the read resumes instead of ending. One that falls
		// outside the window is refused: a caller does not get to widen it. A start
		// this connector derived itself is the one exception, and it is pulled back
		// to the floor rather than refused.
		now := time.Now()
		since := options.SinceTime
		if options.derivedSince {
			since = clampDerivedSinceTime(since, now)
		}
		clamped, ok := boundedSinceTime(since, now)
		if !ok {
			return ErrInvalidArguments
		}
		options.SinceTime = clamped
		query.Set("sinceTime", clamped.UTC().Format(time.RFC3339Nano))
	} else if cursor.Quiet {
		start, clamped := clampCursorStart(cursor.Timestamp, time.Now())
		replayUncertain = replayUncertain || clamped
		query.Set("sinceTime", start.Format(time.RFC3339Nano))
		query.Set("tailLines", strconv.Itoa(cursor.TailLines))
	} else if !cursor.Timestamp.IsZero() {
		oldest := cursor.Timestamp
		for key := range cursor.Occurrences {
			timestamp, _ := time.Parse(time.RFC3339Nano, key)
			if timestamp.Before(oldest) {
				oldest = timestamp
			}
		}
		start, clamped := clampCursorStart(oldest.Add(-time.Nanosecond), time.Now())
		replayUncertain = replayUncertain || clamped
		query.Set("sinceTime", start.Format(time.RFC3339Nano))
	} else {
		since, tail := options.SinceSeconds, options.TailLines
		if since == 0 {
			since = 3600
		}
		if tail == 0 {
			tail = 500
		}
		query.Set("sinceSeconds", strconv.Itoa(since))
		query.Set("tailLines", strconv.Itoa(tail))
	}
	body, err := service.client.openPodLogStream(ctx, "/api/v1/namespaces/"+url.PathEscape(options.Namespace)+"/pods/"+url.PathEscape(options.Pod)+"/log?"+query.Encode(), options.Previous)
	if err != nil {
		return err
	}
	defer body.Close()
	activity := make(chan struct{}, 1)
	reads := make(chan logStreamRead)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(reads)
		reader := bufio.NewReaderSize(logActivityReader{source: io.LimitReader(body, maxBytes+1), activity: activity}, MaxLogStreamLineBytes+1)
		var oversized bool
		var oversizedTimestamp string
		for {
			line, readErr := reader.ReadSlice('\n')
			result := logStreamRead{line: string(line), err: readErr, bytes: len(line)}
			if errors.Is(readErr, bufio.ErrBufferFull) || len(line) > MaxLogStreamLineBytes {
				if !oversized {
					prefix, _, _ := strings.Cut(string(line), " ")
					if len(prefix) <= 64 {
						oversizedTimestamp = prefix
					}
				}
				oversized = true
			}
			if errors.Is(readErr, bufio.ErrBufferFull) {
				result.line, result.err, result.partial = "", nil, true
			} else if oversized {
				result.line = oversizedTimestamp + " [oversized log line omitted]\n"
				result.oversized = true
				oversized, oversizedTimestamp = false, ""
			}
			select {
			case reads <- result:
			case <-ctx.Done():
				return
			}
			if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) {
				return
			}
		}
	}()
	defer func() { cancel(); body.Close(); <-readerDone }()
	heartbeat := time.NewTicker(heartbeatDuration)
	defer heartbeat.Stop()
	idle := time.NewTimer(idleDuration)
	defer idle.Stop()
	var idleDeadline <-chan time.Time
	if options.Previous || options.finite {
		idleDeadline = idle.C
	}
	lastCursor := options.Cursor
	if lastCursor == "" {
		lastCursor = service.quietLogCursor(options, time.Now().UTC())
	}
	// A resume state that outgrows the request budget is not handed to the
	// viewer, and the stream keeps the last position it held: a terminal event
	// without any cursor leaves the viewer unable to rotate at all. That position
	// can be behind the newest line this stream sent, which the limit event
	// reports explicitly instead of leaving the viewer to guess.
	cursorLagging := false
	// newestEmitted is the newest line this stream has handed the viewer. It is
	// what a lagging rotation point is measured against.
	var newestEmitted time.Time
	var emittedBytes int64
	var sequence, checkpointSequence uint64
	replaySeen := make(map[string]uint64)
	cursorBoundsReported := false
	eventBytes := func(event PodLogStreamEvent) int64 {
		payload, _ := json.Marshal(event)
		return int64(len(payload) + len(event.Event) + len("event: \ndata: \n\n"))
	}
	// widestNewestTimestamp is as long as an RFC3339Nano timestamp with a numeric
	// offset can be, so the budget keeps room for the whole lag report before the
	// terminal events are sized.
	const widestNewestTimestamp = "2026-10-26T23:59:59.999999999+07:00"
	terminalReserve := func(cursor string) int64 {
		limit := PodLogStreamEvent{Event: "limit", Reason: "duration", Cursor: cursor, ReplayUncertain: true, CursorLagging: true, NewestTimestamp: widestNewestTimestamp}
		end := PodLogStreamEvent{Event: "end", Reason: "duration", Cursor: cursor, ReplayUncertain: true}
		return eventBytes(limit) + eventBytes(end)
	}
	emittedBytes = eventBytes(PodLogStreamEvent{Event: "heartbeat"})
	readFailure := func(failure error) error {
		if lastCursor != "" {
			if err := emit(PodLogStreamEvent{Event: "heartbeat", Cursor: lastCursor, ReplayUncertain: true}); err != nil {
				return err
			}
		}
		return failure
	}
	finish := func(reason string, limited bool) error {
		for key, count := range cursor.Occurrences {
			if replaySeen[key] < count {
				replayUncertain = true
			}
		}
		if limited {
			event := PodLogStreamEvent{Event: "limit", Reason: reason, Cursor: lastCursor, ReplayUncertain: replayUncertain}
			// A rotation point that stopped advancing cannot carry a viewer past
			// what it already sent: resuming from the cursor replays the same
			// bytes and stops at the same limit. The event says so and names the
			// newest line it did send, which every line already timestamps.
			if cursorLagging && !newestEmitted.IsZero() {
				event.CursorLagging = true
				event.NewestTimestamp = newestEmitted.Format(time.RFC3339Nano)
			}
			emittedBytes += eventBytes(event)
			if err := emit(event); err != nil {
				return err
			}
			replayUncertain = false
		}
		event := PodLogStreamEvent{Event: "end", Reason: reason, Cursor: lastCursor, ReplayUncertain: replayUncertain}
		emittedBytes += eventBytes(event)
		return emit(event)
	}
	var total int64
	emitLine := func(event PodLogStreamEvent, nextCursor string) (bool, error) {
		// The closing limit event carries whichever cursor the stream holds when
		// it stops: the position this line would advance to, or the one already
		// held if this line is the one that does not fit. Room is kept for the
		// larger of the two, so the reserve does not shrink with a resume state
		// that has stopped fitting.
		reserve := terminalReserve(nextCursor)
		if held := terminalReserve(lastCursor); held > reserve {
			reserve = held
		}
		if emittedBytes+eventBytes(event)+reserve > maxBytes {
			return true, finish("bytes", true)
		}
		if err := emit(event); err != nil {
			return false, err
		}
		replayUncertain = false
		emittedBytes += eventBytes(event)
		if nextCursor != "" {
			lastCursor = nextCursor
			cursorLagging = false
		} else {
			cursorLagging = true
		}
		if event.Cursor != "" {
			checkpointSequence = sequence
		}
		return false, nil
	}
	lastTimestamp := cursor.Timestamp
	if cursor.Quiet {
		lastTimestamp = time.Time{}
	}
	occurrences := make(map[string]uint64)
	identities := make(map[string]uint64)
	for key, count := range cursor.Occurrences {
		identities[key] = count
	}
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return parent.Err()
			}
			return finish("duration", true)
		case <-idleDeadline:
			return finish("idle", true)
		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleDuration)
		case <-heartbeat.C:
			event := PodLogStreamEvent{Event: "heartbeat", ReplayUncertain: replayUncertain}
			// A heartbeat repeats the position the viewer already holds only when
			// that position is still the stream's position; once the resume state
			// has moved past it, repeating it would announce a stale rotation
			// point.
			if !cursorLagging && (sequence > checkpointSequence || sequence == 0) {
				event.Cursor = lastCursor
			}
			reserve := terminalReserve(lastCursor)
			if emittedBytes+eventBytes(event)+reserve > maxBytes {
				return finish("bytes", true)
			}
			if err := emit(event); err != nil {
				return err
			}
			replayUncertain = false
			emittedBytes += eventBytes(event)
			checkpointSequence = sequence
		case result, open := <-reads:
			if !open {
				if ctx.Err() != nil {
					if parent.Err() != nil {
						return parent.Err()
					}
					return finish("duration", true)
				}
				return finish("complete", false)
			}
			total += int64(result.bytes)
			if total > maxBytes {
				return finish("bytes", true)
			}
			if result.partial {
				continue
			}
			if result.err != nil && !errors.Is(result.err, io.EOF) {
				if ctx.Err() != nil {
					if parent.Err() != nil {
						return parent.Err()
					}
					return finish("duration", true)
				}
				return readFailure(errors.New("kubernetes: log stream read failed"))
			}
			if result.bytes == 0 && result.line == "" {
				return finish("complete", false)
			}
			if !strings.HasSuffix(result.line, "\n") && !errors.Is(result.err, io.EOF) {
				return readFailure(errors.New("kubernetes: incomplete log line"))
			}
			line := strings.TrimSuffix(strings.TrimSuffix(result.line, "\n"), "\r")
			timestampText, text, found := strings.Cut(line, " ")
			timestamp, parseErr := time.Parse(time.RFC3339Nano, timestampText)
			if !found || parseErr != nil {
				if result.oversized {
					replayUncertain = true
					sequence++
					event := PodLogStreamEvent{Event: "line", Text: "[oversized log line omitted]", Container: options.Container, Sequence: sequence, ReplayUncertain: true}
					if sequence == 1 || sequence%logCursorCheckpoint == 0 {
						event.Cursor = lastCursor
					}
					limited, err := emitLine(event, lastCursor)
					if limited || err != nil {
						return err
					}
					continue
				}
				return readFailure(errors.New("kubernetes: invalid log timestamp"))
			}
			timestampText = timestamp.Format(time.RFC3339Nano)
			ordinal := occurrences[timestampText] + 1
			occurrences[timestampText] = ordinal
			if _, expected := cursor.Occurrences[timestampText]; expected {
				replaySeen[timestampText] = ordinal
			}
			if ordinal <= cursor.Occurrences[timestampText] {
				continue
			}
			nextIdentities := make(map[string]uint64, len(identities)+1)
			for key, count := range identities {
				nextIdentities[key] = count
			}
			nextIdentities[timestampText] = ordinal
			nextTimestamp := lastTimestamp
			if timestamp.After(nextTimestamp) {
				nextTimestamp = timestamp
			}
			if timestamp.Before(lastTimestamp.Add(-logResumeOverlap)) {
				replayUncertain = true
			}
			for key := range nextIdentities {
				entryTime, _ := time.Parse(time.RFC3339Nano, key)
				if entryTime.Before(nextTimestamp.Add(-logResumeOverlap)) {
					delete(nextIdentities, key)
				}
			}
			if len(nextIdentities) > maxLogCursorTimestamps {
				oldest := nextTimestamp
				oldestKey := timestampText
				for key := range nextIdentities {
					entryTime, _ := time.Parse(time.RFC3339Nano, key)
					if entryTime.Before(oldest) {
						oldest, oldestKey = entryTime, key
					}
				}
				delete(nextIdentities, oldestKey)
				if !cursorBoundsReported {
					replayUncertain = true
					cursorBoundsReported = true
				}
			}
			text = service.scrubber.Scrub(strings.ToValidUTF8(text, ""))
			if options.Timestamps {
				text = timestamp.Format(time.RFC3339Nano) + " " + text
			}
			if len(text) > MaxLogStreamLineBytes {
				text = "[oversized scrubbed log line omitted]"
			}
			sequence++
			since, tail := logBounds(options.SinceSeconds, options.TailLines)
			if cursor.SinceSeconds != 0 {
				since, tail = logBounds(cursor.SinceSeconds, cursor.TailLines)
			}
			encoded, _ := json.Marshal(logResumeCursor{Target: service.logTarget(options), Timestamp: nextTimestamp, Ordinal: nextIdentities[nextTimestamp.Format(time.RFC3339Nano)], Occurrences: nextIdentities, SinceSeconds: since, TailLines: tail})
			nextCursor := base64.RawURLEncoding.EncodeToString(encoded)
			// A cursor that is sent to the viewer has to fit the request the viewer
			// sends back; a workload source's position stays in this process and
			// keeps its line identity. Either way a viewer can always resume from
			// the line timestamp it already holds.
			cursorBudget := maxStreamCursorBytes
			if options.checkpointEveryLine {
				cursorBudget = maxRetainedCursorBytes
			}
			if len(nextCursor) > cursorBudget {
				nextCursor = ""
			}
			event := PodLogStreamEvent{Event: "line", Text: text, Container: options.Container, Timestamp: timestampText, Sequence: sequence, Ordinal: ordinal, ReplayUncertain: replayUncertain}
			if nextCursor != "" && (options.checkpointEveryLine || sequence == 1 || sequence%logCursorCheckpoint == 0) {
				event.Cursor = nextCursor
			}
			if limited, err := emitLine(event, nextCursor); limited || err != nil {
				return err
			}
			if newestEmitted.Before(timestamp) {
				newestEmitted = timestamp
			}
			lastTimestamp, identities = nextTimestamp, nextIdentities
			if errors.Is(result.err, io.EOF) {
				return finish("complete", false)
			}
		}
	}
}
