package signalsources

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// CloudWatchLogsSource pulls log events from one AWS CloudWatch Logs log
// group via FilterLogEvents. Authentication is the standard AWS SDK
// chain (env vars / shared credentials / IAM role on the host).
//
// FilterLogEvents is the right primitive here because:
//   - it is real-time (no async query lifecycle like Insights),
//   - it accepts a `startTime` filter so cursoring is trivial, and
//   - it returns events sorted by ingestion time across streams.
//
// The cursor is the maximum event timestamp seen on the previous tick
// (in milliseconds, the unit CloudWatch uses internally).
//
// Each tick re-reads an inclusive span below that cursor and suppresses the
// events already delivered inside it with a persisted event-id set (TailDedup)
// that survives a restart — the same convention the Elasticsearch and SigNoz
// tails follow. `reorder_window` sets how much of that span is lateness
// tolerance; the agent adds one catalog persist interval on top so a killed
// process's unflushed events are re-read rather than skipped. A source with no
// `reorder_window` therefore still re-reads the persist interval, which is what
// stops the shipped default from losing events on an abrupt restart.
type CloudWatchLogsSource struct {
	name   string
	cfg    config.AgentCloudWatchLogsSourceConfig
	client *cloudwatchlogs.Client

	// reorderWindow is the configured lateness budget: how far below the cursor
	// a late-arriving event may sit and still be picked up.
	reorderWindow time.Duration

	// replaySpan is added to reorderWindow to size what a tick actually
	// re-reads. The agent wiring sets it to the catalog persist interval so a
	// replacement process re-reads everything a killed one learned but never
	// flushed. See ApplyTailReplaySpan.
	replaySpan time.Duration

	// nowFn is the wall clock the tail reads to upper-bound the scan (EndTime)
	// and clamp the cursor (ClampCursor). Overridable in tests; nil ⇒ time.Now.
	nowFn func() time.Time

	// mu guards the dedup set for the whole tick so a concurrent Rewind cannot
	// interleave and leave stale dedup state behind.
	mu sync.Mutex
	// dedup holds the event ids already delivered whose timestamp is still
	// inside the span the next query re-reads.
	dedup *TailDedup
}

// NewCloudWatchLogsSource validates config and returns a ready source.
// It loads the default AWS SDK config for the configured region.
func NewCloudWatchLogsSource(name string, cfg config.AgentCloudWatchLogsSourceConfig) (*CloudWatchLogsSource, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("cloudwatchlogs source %q: region is required", name)
	}
	if cfg.LogGroupName == "" {
		return nil, fmt.Errorf("cloudwatchlogs source %q: log_group_name is required", name)
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.PageSize > 10000 {
		cfg.PageSize = 10000
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(cfg.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("cloudwatchlogs source %q: load aws config: %w", name, err)
	}
	src := &CloudWatchLogsSource{
		name:   name,
		cfg:    cfg,
		client: cloudwatchlogs.NewFromConfig(awsCfg),
	}
	if cfg.ReorderWindow != "" {
		if d, perr := time.ParseDuration(cfg.ReorderWindow); perr == nil && d > 0 {
			src.reorderWindow = d
		}
	}
	src.dedup = NewTailDedup(src.Name())
	return src, nil
}

func (s *CloudWatchLogsSource) Name() string { return "cloudwatchlogs:" + s.name }

// SetTailReplaySpan widens what each tick re-reads so it also covers the events
// a killed process learned but never flushed. It implements
// TailReplaySpanSetter.
func (s *CloudWatchLogsSource) SetTailReplaySpan(span time.Duration) (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if span > 0 {
		s.replaySpan = span
	}
	return s.reorderWindow, s.scanWindow()
}

// scanWindow is how far below the cursor a tick re-reads: the operator's
// lateness budget plus the crash-replay span. Callers hold s.mu.
func (s *CloudWatchLogsSource) scanWindow() time.Duration { return s.reorderWindow + s.replaySpan }

// SetTailDedupBackend makes this source's boundary dedup set durable. It
// implements TailDedupBinder.
func (s *CloudWatchLogsSource) SetTailDedupBackend(b TailDedupBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedup.SetBackend(b)
}

// Rewind clears the boundary dedup set — in memory and in its backend — so a
// catalog clear makes this source re-emit, and therefore relearn, its whole
// window from scratch. It implements core.SourceRewinder.
func (s *CloudWatchLogsSource) Rewind(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Clear(ctx)
}

// now returns the source's wall clock. Tests override nowFn to freeze it; the
// nil-guard keeps struct-literal construction working.
func (s *CloudWatchLogsSource) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn().UTC()
	}
	return time.Now().UTC()
}

// Pull issues a FilterLogEvents request over `[since - scanWindow, now]` with
// an inclusive start, and suppresses the events already delivered inside that
// span with the dedup set. When no window applies at all the start falls back
// to `since + 1ms` (CloudWatch's startTime is inclusive), which re-reads
// nothing.
// It walks NextToken pagination until the page is short, the requested page
// size has been collected, or we've made `maxPages` calls (safety cap).
//
// Events are appended in API order. The cursor is the maximum event timestamp
// seen, never lower than `since` and never past `now` (ClampCursor). The
// `endTime = now` bound plus the clamp are the same invariant the Elasticsearch
// source enforces: a future-dated event — an untrusted producer timestamp —
// must not advance the cursor past the wall clock, or every following
// `startTime = cursor + 1ms` query would return nothing until that future time
// actually arrives (the tailing stall). Future-dated events are intentionally
// not tailed; minor clock skew is recovered once the wall clock passes it.
func (s *CloudWatchLogsSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	// Heal a persisted future cursor so the [since, now] window is never
	// inverted (startTime > endTime), which the API rejects.
	if since.After(now) {
		since = now
	}
	cursor := since
	lower := since
	startMs := since.UTC().UnixMilli() + 1
	if window := s.scanWindow(); window > 0 && !since.IsZero() {
		lower = since.Add(-window)
		startMs = lower.UTC().UnixMilli()
	}
	endMs := now.UTC().UnixMilli()
	// A caught-up cursor at the wall-clock boundary leaves nothing to scan; skip
	// the call (an inverted window would error) and hold the cursor.
	if startMs > endMs {
		return nil, ClampCursor(cursor, since, now), nil
	}

	// Hydrate the persisted dedup set before the first query of the process. A
	// failure here costs duplicates, never events, so it is logged and the tick
	// continues.
	if err := s.dedup.Load(ctx); err != nil {
		log.Printf("agent: %s: loading the persisted dedup set failed: %v (this tick may re-emit up to one reorder window)", s.Name(), err)
	}

	limit := int32(s.cfg.PageSize)
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: &s.cfg.LogGroupName,
		StartTime:    &startMs,
		EndTime:      &endMs,
		Limit:        &limit,
	}
	if s.cfg.LogStreamPrefix != "" {
		in.LogStreamNamePrefix = &s.cfg.LogStreamPrefix
	}
	if s.cfg.FilterPattern != "" {
		in.FilterPattern = &s.cfg.FilterPattern
	}

	var signals []core.Signal
	var seen []DedupRow
	const maxPages = 20
	for page := 0; page < maxPages; page++ {
		out, err := s.client.FilterLogEvents(ctx, in)
		if err != nil {
			return signals, cursor, fmt.Errorf("cloudwatch FilterLogEvents: %w", err)
		}
		for _, e := range out.Events {
			sig, ok := signalFromCWEvent(s.Name(), e)
			if !ok {
				continue
			}
			if sig.Timestamp.After(cursor) {
				cursor = sig.Timestamp
			}
			id := cwEventID(sig, e)
			seen = append(seen, DedupRow{ID: id, TS: sig.Timestamp})
			if s.dedup.Has(id) {
				continue
			}
			signals = append(signals, sig)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		if len(signals) >= s.cfg.PageSize {
			break
		}
		in.NextToken = out.NextToken
	}
	// Clamp so an event timestamp (untrusted producer clock) can never advance
	// the cursor past the wall clock. The EndTime bound already excludes future
	// events server-side; this enforces the invariant regardless.
	cursor = ClampCursor(cursor, since, now)

	// Stage the ids IN MEMORY before returning, so the next tick of this process
	// does not re-deliver them, and leave them uncommitted until the worker has
	// flushed the events they describe. The retention floor is this tick's query
	// lower bound — one reorder window below the cursor the tick started from —
	// so the set covers whichever cursor turns out to be the durable one.
	s.dedup.Stage(seen, lower)

	return signals, cursor, nil
}

// Commit makes the event ids this source has delivered durable. The worker
// calls it only after flushing the events they describe, so a process that dies
// can re-deliver events but can never suppress events it failed to store. It
// implements core.SourceCommitter.
func (s *CloudWatchLogsSource) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Commit(ctx)
}

// cwEventID is the dedup key for one CloudWatch event. `eventId` is unique per
// event and is the natural key; the composite fallback keeps dedup working for
// a response that omits it instead of silently replaying the window every tick.
func cwEventID(sig core.Signal, e cwltypes.FilteredLogEvent) string {
	if e.EventId != nil && *e.EventId != "" {
		return *e.EventId
	}
	stream := ""
	if e.LogStreamName != nil {
		stream = *e.LogStreamName
	}
	return sig.Timestamp.Format(time.RFC3339Nano) + "|" + stream + "|" + truncate(sig.Message, 256)
}

func signalFromCWEvent(srcName string, e cwltypes.FilteredLogEvent) (core.Signal, bool) {
	if e.Timestamp == nil || e.Message == nil {
		return core.Signal{}, false
	}
	ts := time.UnixMilli(*e.Timestamp).UTC()
	fields := map[string]interface{}{}
	if e.LogStreamName != nil {
		fields["log_stream"] = *e.LogStreamName
	}
	raw := map[string]interface{}{
		"message": *e.Message,
	}
	if e.LogStreamName != nil {
		raw["log_stream"] = *e.LogStreamName
	}
	if e.EventId != nil {
		raw["event_id"] = *e.EventId
	}
	return core.Signal{
		Source:    srcName,
		Timestamp: ts,
		Message:   *e.Message,
		Fields:    fields,
		Raw:       raw,
	}, true
}
