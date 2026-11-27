// Package incidentcounts keeps the last incident status count per read scope
// and count window, and serves it to every page that polls for it.
//
// The header badge, the Now tiles and the incident list each ask for the same
// count, every few seconds, from every open tab. On a large incident table
// one count reads most of the table, so the server counts at most once per
// key per FreshFor and hands every request the last good answer:
//
//   - a fresh count (under FreshFor old) is served as is;
//   - an older count is served at once while one refresh runs behind it;
//   - with no count yet, requests wait for that one refresh, which the
//     storage layer ends after storage.IncidentCountTimeout;
//   - one refresh runs per key at a time in this process, however many
//     requests arrive;
//   - a failed refresh keeps the last good count, and the next one waits
//     FreshFor, so a struggling database is asked once per FreshFor;
//   - an operator's own ack or resolve marks every key stale, so the next
//     request recounts, at most once per MinRecountGap. New incidents do
//     not: they arrive many times a second and show within FreshFor.
//
// The cache lives in one process. Each replica keeps its own, so a
// deployment runs at most one count per replica per FreshFor.
package incidentcounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

const (
	// FreshFor is how long a count is served without a refresh. It matches
	// the header badge's own poll interval.
	FreshFor = 30 * time.Second
	// StaleAfter is the age past which a served count is flagged stale, so
	// the page can say the number is old.
	StaleAfter = 2 * time.Minute
	// MinRecountGap is the shortest time between the starts of two counts
	// for one key, however often operators ack or resolve.
	MinRecountGap = 2 * time.Second
	// refreshGrace is how long a refresh may outlive the storage count limit
	// before the cache gives up on it. The storage layer normally ends the
	// count first; this bounds a backend that ignores its context.
	refreshGrace = 2 * time.Second
	// warnAfterFailures is how many refreshes in a row must fail before the
	// default log says so. Fewer heal on their own and stay at debug.
	warnAfterFailures = 3
	// throttleKey names the one thing that fails here, for the log throttle.
	throttleKey = "incident-counts"
	// panicThrottleKey throttles the error a panicking count writes.
	panicThrottleKey = "incident-counts-panic"
)

// Key names one cached count: the read scope (the organization the request
// reads as) and the count window's name (7d, 30d, all). The window's name,
// not its computed start, keeps the key stable while time passes.
type Key struct {
	Scope  string
	Window string
}

// Counter runs one count. ctx carries the refresh deadline.
type Counter func(ctx context.Context) (storage.IncidentStatusCounts, error)

// Result is what a request is served.
type Result struct {
	Counts storage.IncidentStatusCounts
	// CountedAt is when the served count started; zero when Unknown.
	CountedAt time.Time
	// Unknown is true when no good count exists yet. Counts is then zero and
	// must not be shown as zero incidents.
	Unknown bool
	// Stale is true when the served count is older than StaleAfter.
	Stale bool
}

// Options configures a Cache. Zero values use the package defaults.
type Options struct {
	// Now reads the clock; tests move it by hand.
	Now func() time.Time
	// Throttle limits the warning a run of failed refreshes writes; tests
	// give it their clock.
	Throttle *logging.Throttle
}

// Cache is safe for concurrent use. The zero value is not usable; use New.
type Cache struct {
	now      func() time.Time
	throttle *logging.Throttle

	mu      sync.Mutex
	entries map[Key]*entry
	// marks counts operator actions that changed incident status. A count
	// that started before the latest one is out of date.
	marks uint64
	// failures counts refreshes in a row that failed, across keys: they share
	// one database.
	failures int
	// waiters counts requests blocked on a refresh; tests read it.
	waiters int
}

type entry struct {
	counts    storage.IncidentStatusCounts
	countedAt time.Time
	has       bool
	// marks is the cache's mark count when the served count started.
	marks uint64
	// failedMarks is the mark count at the start of the last count that
	// failed. Once a recount after an action has failed, requests stop
	// waiting for recounts of that action and are served the last count.
	failedMarks uint64
	failed      bool
	// notBefore is the earliest time the next refresh may start.
	notBefore time.Time
	flight    *flight
}

type flight struct {
	started time.Time
	marks   uint64
	done    chan struct{}
}

// New returns an empty cache.
func New(opts Options) *Cache {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	throttle := opts.Throttle
	if throttle == nil {
		throttle = logging.NewThrottle(0, now)
	}
	return &Cache{now: now, throttle: throttle, entries: make(map[Key]*entry)}
}

// Get serves the count for key, running count when the cached one is due for
// a refresh. It waits only when the wait can pay off: there is no count yet,
// or an operator's action has made the last count out of date and the running
// count started after that action, with no recount for it having failed.
// Otherwise it serves the last count at once. A wait ends when that refresh
// ends or ctx does; the refresh keeps running for the other requests.
func (c *Cache) Get(ctx context.Context, key Key, count Counter) Result {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		e = &entry{}
		c.entries[key] = e
	}
	now := c.now()
	if e.flight == nil && c.dueLocked(e, now) {
		c.startLocked(e, count, now)
	}
	f := e.flight
	if f != nil && (!e.has || c.waitForRecountLocked(e, f)) {
		c.waiters++
		c.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
		}
		c.mu.Lock()
		c.waiters--
		now = c.now()
	}
	r := c.resultLocked(e, now)
	c.mu.Unlock()
	return r
}

// MarkStale records that incident status just changed through an operator's
// own action, so the next request for every key recounts.
func (c *Cache) MarkStale() {
	c.mu.Lock()
	c.marks++
	c.mu.Unlock()
}

// waitForRecountLocked reports whether the running count f is the recount
// for the latest operator action that requests should wait for.
func (c *Cache) waitForRecountLocked(e *entry, f *flight) bool {
	if e.marks == c.marks || f.marks != c.marks {
		return false
	}
	return !e.failed || e.failedMarks != c.marks
}

func (c *Cache) dueLocked(e *entry, now time.Time) bool {
	if now.Before(e.notBefore) {
		return false
	}
	if !e.has || e.marks != c.marks {
		return true
	}
	return now.Sub(e.countedAt) >= FreshFor
}

func (c *Cache) startLocked(e *entry, count Counter, now time.Time) {
	f := &flight{started: now, marks: c.marks, done: make(chan struct{})}
	e.flight = f
	e.notBefore = now.Add(MinRecountGap)
	go c.refresh(e, f, count)
}

// refresh runs one count without the caller's context: the requests that
// share it come and go, and none of them may cancel it for the others.
func (c *Cache) refresh(e *entry, f *flight, count Counter) {
	counts, err := runCount(count)

	c.mu.Lock()
	end := c.now()
	if err == nil {
		e.counts, e.countedAt, e.has, e.marks = counts, f.started, true, f.marks
		c.failures = 0
	} else {
		e.notBefore = end.Add(FreshFor)
		e.failed, e.failedMarks = true, f.marks
		c.failures++
	}
	failures, last := c.failures, e.countedAt
	hasLast := e.has
	c.mu.Unlock()

	// The refresh ends after its log lines, so whoever sees it end also sees
	// what it logged.
	defer func() {
		c.mu.Lock()
		e.flight = nil
		close(f.done)
		c.mu.Unlock()
	}()
	if err == nil {
		c.throttle.Clear(panicThrottleKey, "incident counts: counting no longer panics")
		if c.throttle.Clear(throttleKey, "incident counts: counting works again") {
			logging.Infof("incident counts: counting incidents in storage works again")
		}
		return
	}
	cause := "failed"
	if errors.Is(err, errCountPanicked) {
		// A panic is a bug, not load: say so at once, then at most once per
		// throttle window while it keeps happening.
		c.throttle.Fail(slog.LevelError, panicThrottleKey, "panicked",
			"incident counts: counting incidents in storage panicked; pages show the last count or unknown; report this as a bug: %v", err)
	}
	if errors.Is(err, storage.ErrCountTimeout) || errors.Is(err, context.DeadlineExceeded) {
		cause = "timed out"
	}
	served := "no count yet, pages show the count as unknown"
	if hasLast {
		served = "pages show the count from " + last.UTC().Format(time.RFC3339)
	}
	logging.Debugf("incident counts: count %s (%d in a row); %s: %v", cause, failures, served, err)
	if failures >= warnAfterFailures {
		c.throttle.Fail(slog.LevelWarn, throttleKey, cause,
			"incident counts: counting incidents in storage %s %d times in a row (limit %s each); %s; check the database load (long queries in pg_stat_activity on vs_incidents) and that the incident count index exists",
			cause, failures, storage.IncidentCountTimeout, served)
	}
}

// errCountPanicked reports a count that panicked. The cache treats it as a
// failed count, so one broken backend call cannot stop the process or leave
// requests waiting on a refresh that never ends.
var errCountPanicked = errors.New("count panicked")

// runCount runs one count with the refresh deadline, turning a panic into an
// error.
func runCount(count Counter) (counts storage.IncidentStatusCounts, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), storage.IncidentCountTimeout+refreshGrace)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			counts, err = storage.IncidentStatusCounts{}, fmt.Errorf("%w: %v", errCountPanicked, r)
		}
	}()
	return count(ctx)
}

func (c *Cache) resultLocked(e *entry, now time.Time) Result {
	if !e.has {
		return Result{Unknown: true}
	}
	return Result{
		Counts:    e.counts,
		CountedAt: e.countedAt,
		Stale:     now.Sub(e.countedAt) > StaleAfter,
	}
}
