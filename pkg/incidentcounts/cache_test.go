package incidentcounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// hangGuard is how long a test waits for an event before calling it a hang.
// A passing test never comes near it.
const hangGuard = 60 * time.Second

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestCache(clock *fakeClock) *Cache {
	return New(Options{Now: clock.Now, Throttle: logging.NewThrottle(0, clock.Now)})
}

// eventually waits until cond holds, failing only at the hang guard.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangGuard)
	for pause := time.Millisecond; !cond(); pause = min(2*pause, 10*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("hang guard reached: %s", what)
		}
		time.Sleep(pause) // testlint:allow poll pause of the shared wait; the cache has no notification for waiters
	}
}

func (c *Cache) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiters
}

func (c *Cache) refreshing(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	return e != nil && e.flight != nil
}

// countsOf builds a tally whose open total is n, so each count is
// recognisable in the served result.
func countsOf(n int) storage.IncidentStatusCounts {
	return storage.IncidentStatusCounts{Total: storage.StatusCounts{Open: n, Total: n}}
}

// gatedCounter counts calls and holds each one until the test releases it.
type gatedCounter struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	value   atomic.Int32
	err     error
}

func newGatedCounter() *gatedCounter {
	return &gatedCounter{started: make(chan struct{}, 100), release: make(chan struct{}, 100)}
}

func (g *gatedCounter) count(ctx context.Context) (storage.IncidentStatusCounts, error) {
	g.calls.Add(1)
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return storage.IncidentStatusCounts{}, ctx.Err()
	}
	if g.err != nil {
		return storage.IncidentStatusCounts{}, g.err
	}
	return countsOf(int(g.value.Load())), nil
}

// immediateCounter returns at once; calls counts every count that ran.
type immediateCounter struct {
	calls atomic.Int32
	value atomic.Int32
	fail  atomic.Bool
}

func (c *immediateCounter) count(context.Context) (storage.IncidentStatusCounts, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return storage.IncidentStatusCounts{}, fmt.Errorf("count: %w", storage.ErrCountTimeout)
	}
	return countsOf(int(c.value.Load())), nil
}

var key7d = Key{Scope: storage.DefaultOrgID, Window: "7d"}

// With no count yet, fifty requests that arrive together share one count and
// all receive its answer; once it is fresh, more requests run no count.
func TestConcurrentRequestsShareOneCount(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := newGatedCounter()
	counter.value.Store(7)

	const requests = 50
	results := make(chan Result, requests)
	for range requests {
		go func() { results <- cache.Get(context.Background(), key7d, counter.count) }()
	}
	receive(t, counter.started, "the count to start")
	eventually(t, "all requests wait for the one count", func() bool { return cache.waiting() == requests })
	counter.release <- struct{}{}
	for range requests {
		r := <-results
		if r.Unknown || r.Counts.Total.Open != 7 || !r.CountedAt.Equal(clock.Now()) {
			t.Fatalf("served %+v, want the shared count of 7 counted now", r)
		}
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("counts run for %d concurrent requests with no cached count = %d, want 1", requests, got)
	}

	for range requests {
		if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 7 {
			t.Fatalf("fresh request served %+v", r)
		}
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("counts run for fresh requests = %d, want still 1", got)
	}
}

// A count that fails with nothing cached is served as unknown, never as zero.
func TestFailedFirstCountIsUnknown(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := &immediateCounter{}
	counter.fail.Store(true)

	r := cache.Get(context.Background(), key7d, counter.count)
	if !r.Unknown || !r.CountedAt.IsZero() || r.Counts != (storage.IncidentStatusCounts{}) {
		t.Fatalf("served %+v, want unknown with no time and no numbers", r)
	}
}

// An older count is served at once while one refresh runs behind it; the
// refresh's answer is served after it ends. A failed refresh keeps the last
// good count, which is flagged stale once it is older than StaleAfter.
func TestOldCountIsServedWhileOneRefreshRuns(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := newGatedCounter()
	counter.value.Store(1)
	first := clock.Now()
	go func() { counter.release <- struct{}{} }()
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 1 {
		t.Fatalf("first count served %+v", r)
	}
	receive(t, counter.started, "the count to start")

	clock.Advance(FreshFor)
	counter.value.Store(2)
	for range 5 {
		r := cache.Get(context.Background(), key7d, counter.count)
		if r.Counts.Total.Open != 1 || !r.CountedAt.Equal(first) || r.Stale {
			t.Fatalf("while refreshing served %+v, want the last count of 1, not stale", r)
		}
	}
	receive(t, counter.started, "the count to start")
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("counts run = %d, want 2 (one refresh for five requests)", got)
	}
	counter.release <- struct{}{}
	eventually(t, "the refresh ends", func() bool { return !cache.refreshing(key7d) })
	refreshed := cache.Get(context.Background(), key7d, counter.count)
	if refreshed.Counts.Total.Open != 2 || !refreshed.CountedAt.Equal(first.Add(FreshFor)) {
		t.Fatalf("after the refresh served %+v, want 2 counted at the refresh start", refreshed)
	}

	// The next refresh fails; the last good count stays and ages into stale.
	counter.err = errors.New("storage unavailable")
	clock.Advance(FreshFor)
	cache.Get(context.Background(), key7d, counter.count)
	receive(t, counter.started, "the count to start")
	counter.release <- struct{}{}
	eventually(t, "the failed refresh ends", func() bool { return !cache.refreshing(key7d) })
	clock.Advance(StaleAfter - FreshFor)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 2 || r.Stale {
		t.Fatalf("at exactly StaleAfter served %+v, want 2 and not stale", r)
	}
	clock.Advance(time.Second)
	r := cache.Get(context.Background(), key7d, counter.count)
	if r.Unknown || r.Counts.Total.Open != 2 || !r.Stale {
		t.Fatalf("past StaleAfter served %+v, want the last good 2 flagged stale", r)
	}
}

// A changed count window is a new key: its first request counts again and
// never sees the old window's numbers.
func TestWindowChangeNeverServesTheOldWindow(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := &immediateCounter{}
	counter.value.Store(5)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 5 {
		t.Fatalf("7d served %+v", r)
	}
	counter.value.Store(40)
	key30d := Key{Scope: storage.DefaultOrgID, Window: "30d"}
	if r := cache.Get(context.Background(), key30d, counter.count); r.Counts.Total.Open != 40 {
		t.Fatalf("30d served %+v, want its own count of 40", r)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("counts run = %d, want one per window", got)
	}

	failing := &immediateCounter{}
	failing.fail.Store(true)
	if r := cache.Get(context.Background(), Key{Scope: storage.DefaultOrgID, Window: "24h"}, failing.count); !r.Unknown {
		t.Fatalf("a window whose first count failed served %+v, want unknown rather than another window's numbers", r)
	}
}

// An operator's action makes the next request recount and wait for it; a
// second action within MinRecountGap does not start another count, and
// without an action no count runs before FreshFor.
func TestMarkStaleRecountsOnceWithinTheGap(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := &immediateCounter{}
	counter.value.Store(3)
	cache.Get(context.Background(), key7d, counter.count)

	clock.Advance(FreshFor - time.Second)
	for range 10 {
		cache.Get(context.Background(), key7d, counter.count)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("counts run before FreshFor without an action = %d, want 1", got)
	}

	clock.Advance(time.Millisecond)
	counter.value.Store(2)
	cache.MarkStale()
	clock.Advance(time.Millisecond)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 2 {
		t.Fatalf("after an ack served %+v, want the recount of 2", r)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("counts run after one ack = %d, want 2", got)
	}

	counter.value.Store(1)
	cache.MarkStale()
	clock.Advance(MinRecountGap / 2)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 2 {
		t.Fatalf("within the gap served %+v, want the last count of 2", r)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("counts run for a second ack within the gap = %d, want still 2", got)
	}
	clock.Advance(MinRecountGap / 2)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 1 {
		t.Fatalf("after the gap served %+v, want the recount of 1", r)
	}
	if got := counter.calls.Load(); got != 3 {
		t.Fatalf("counts run after the gap = %d, want 3", got)
	}
}

// A failed refresh is not retried until FreshFor has passed, so a struggling
// database is asked once per FreshFor however many requests arrive.
func TestFailedRefreshWaitsFreshForBeforeRetrying(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := &immediateCounter{}
	counter.fail.Store(true)
	cache.Get(context.Background(), key7d, counter.count)
	clock.Advance(FreshFor - time.Second)
	for range 20 {
		cache.Get(context.Background(), key7d, counter.count)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("counts run within FreshFor of a failure = %d, want 1", got)
	}
	clock.Advance(time.Second)
	counter.fail.Store(false)
	counter.value.Store(9)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 9 {
		t.Fatalf("after FreshFor served %+v, want a new count of 9", r)
	}
}

// captureLog collects the standard logger's output. Tests that use it do not
// run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	oldOut, oldFlags := log.Writer(), log.Flags()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})
	return buf
}

// Healthy refreshes write nothing to the default log. Two timeouts in a row
// stay quiet; the third writes one warning naming what to check, and further
// timeouts inside the throttle window write nothing more.
func TestTimeoutsWarnOnceAfterThreeInARow(t *testing.T) {
	buf := captureLog(t)
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := &immediateCounter{}
	counter.value.Store(1)

	cache.Get(context.Background(), key7d, counter.count)
	clock.Advance(FreshFor)
	cache.Get(context.Background(), key7d, counter.count)
	eventually(t, "the healthy refresh ends", func() bool { return !cache.refreshing(key7d) })
	if got := counter.calls.Load(); got != 2 {
		t.Fatalf("healthy counts run = %d, want 2", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("healthy cycles wrote to the default log: %q", buf.String())
	}

	counter.fail.Store(true)
	timeout := func() {
		t.Helper()
		before := counter.calls.Load()
		clock.Advance(FreshFor)
		cache.Get(context.Background(), key7d, counter.count)
		eventually(t, "the timed-out refresh ends", func() bool {
			return counter.calls.Load() == before+1 && !cache.refreshing(key7d)
		})
	}
	timeout()
	timeout()
	if buf.Len() != 0 {
		t.Fatalf("two timeouts in a row wrote to the default log: %q", buf.String())
	}
	timeout()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") ||
		!strings.Contains(lines[0], "timed out 3 times in a row") ||
		!strings.Contains(lines[0], "pg_stat_activity") {
		t.Fatalf("third timeout wrote %q, want one warning naming the timeouts and what to check", buf.String())
	}
	for range 3 {
		timeout()
	}
	if got := strings.Count(buf.String(), "WARN "); got != 1 {
		t.Fatalf("warnings after six timeouts in a row = %d, want 1 inside the throttle window: %q", got, buf.String())
	}
}

// receive waits for one value from ch, failing only at the hang guard.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(hangGuard)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("hang guard reached: %s", what)
		var zero T
		return zero
	}
}

// An ack that lands while a count runs does not make requests wait for that
// count, which cannot include it. Once it ends, exactly one recount runs for
// the ack, and requests wait for it rather than take the pre-ack count as
// current.
func TestAckDuringRunningCountRecountsOnceAfterIt(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := newGatedCounter()
	counter.value.Store(1)
	counter.release <- struct{}{}
	cache.Get(context.Background(), key7d, counter.count)
	receive(t, counter.started, "the count to start")

	clock.Advance(FreshFor)
	counter.value.Store(2)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Counts.Total.Open != 1 {
		t.Fatalf("old count while refreshing = %+v, want 1", r)
	}
	receive(t, counter.started, "the count to start")
	cache.MarkStale()

	during := make(chan Result, 1)
	go func() { during <- cache.Get(context.Background(), key7d, counter.count) }()
	if r := receive(t, during, "a request after an ack during a running count returns without waiting for it"); r.Counts.Total.Open != 1 {
		t.Fatalf("request after the ack during the count = %+v, want the last count of 1 at once", r)
	}
	if !cache.refreshing(key7d) {
		t.Fatal("the request after the ack returned only once the running count had ended; it waited for a count that cannot include the ack")
	}
	counter.release <- struct{}{}
	eventually(t, "the pre-ack count ends", func() bool { return !cache.refreshing(key7d) })

	clock.Advance(MinRecountGap)
	counter.value.Store(3)
	after := make(chan Result, 1)
	go func() { after <- cache.Get(context.Background(), key7d, counter.count) }()
	receive(t, counter.started, "the count to start")
	eventually(t, "the request waits for the recount after the ack", func() bool { return cache.waiting() == 1 })
	counter.release <- struct{}{}
	if r := receive(t, after, "the recount after the ack"); r.Counts.Total.Open != 3 {
		t.Fatalf("after the ack served %+v, want the recount of 3, not the count that started before it", r)
	}
	for range 5 {
		cache.Get(context.Background(), key7d, counter.count)
	}
	if got := counter.calls.Load(); got != 3 {
		t.Fatalf("counts run = %d, want 3: the first, the one the ack interrupted, and one recount", got)
	}
}

// A request that gives up while waiting returns at once, and the shared count
// keeps running and answers the requests still waiting.
func TestCancelledRequestLeavesTheSharedCountRunning(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := newGatedCounter()
	counter.value.Store(6)

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan Result, 1)
	staying := make(chan Result, 1)
	go func() { cancelled <- cache.Get(ctx, key7d, counter.count) }()
	receive(t, counter.started, "the count to start")
	go func() { staying <- cache.Get(context.Background(), key7d, counter.count) }()
	eventually(t, "both requests wait for the one count", func() bool { return cache.waiting() == 2 })

	cancel()
	if r := receive(t, cancelled, "the cancelled request returns"); !r.Unknown {
		t.Fatalf("cancelled request with no count yet = %+v, want unknown", r)
	}
	if !cache.refreshing(key7d) {
		t.Fatal("cancelling one request ended the shared count")
	}
	counter.release <- struct{}{}
	if r := receive(t, staying, "the remaining request gets the shared count"); r.Unknown || r.Counts.Total.Open != 6 {
		t.Fatalf("remaining request = %+v, want the shared count of 6", r)
	}
	if got := counter.calls.Load(); got != 1 {
		t.Fatalf("counts run = %d, want 1", got)
	}
}

// Once the recount after an ack has failed, later requests are served the
// last count at once instead of waiting on each retry.
func TestFailedRecountAfterAckServesTheLastCountAtOnce(t *testing.T) {
	clock := newFakeClock()
	cache := newTestCache(clock)
	counter := newGatedCounter()
	counter.value.Store(4)
	counter.release <- struct{}{}
	cache.Get(context.Background(), key7d, counter.count)
	receive(t, counter.started, "the count to start")

	clock.Advance(MinRecountGap)
	cache.MarkStale()
	counter.err = errors.New("storage unavailable")
	first := make(chan Result, 1)
	go func() { first <- cache.Get(context.Background(), key7d, counter.count) }()
	receive(t, counter.started, "the count to start")
	eventually(t, "the first request after the ack waits for its recount", func() bool { return cache.waiting() == 1 })
	counter.release <- struct{}{}
	if r := receive(t, first, "the failed recount"); r.Unknown || r.Counts.Total.Open != 4 {
		t.Fatalf("after the failed recount served %+v, want the last count of 4", r)
	}

	clock.Advance(FreshFor)
	retry := make(chan Result, 1)
	go func() { retry <- cache.Get(context.Background(), key7d, counter.count) }()
	receive(t, counter.started, "the count to start")
	if r := receive(t, retry, "a request during the retry returns without waiting"); r.Unknown || r.Counts.Total.Open != 4 {
		t.Fatalf("during the retry served %+v, want the last count of 4 at once", r)
	}
	if !cache.refreshing(key7d) {
		t.Fatal("the request during the retry returned only once the retry had ended; after a failed recount it must not wait")
	}
	counter.release <- struct{}{}
	eventually(t, "the retry ends", func() bool { return !cache.refreshing(key7d) })
}

// A count that panics is a failed count: the request is answered, the process
// keeps running, one error line is written, and the next count works.
func TestPanickingCountLeavesTheCacheUsable(t *testing.T) {
	buf := captureLog(t)
	clock := newFakeClock()
	cache := newTestCache(clock)
	panicking := func(context.Context) (storage.IncidentStatusCounts, error) { panic("broken backend") }

	if r := cache.Get(context.Background(), key7d, panicking); !r.Unknown {
		t.Fatalf("after a panicking first count served %+v, want unknown", r)
	}
	if got := strings.Count(buf.String(), "ERROR "); got != 1 || !strings.Contains(buf.String(), "panicked") {
		t.Fatalf("log after one panic = %q, want one error line", buf.String())
	}
	clock.Advance(FreshFor)
	counter := &immediateCounter{}
	counter.value.Store(8)
	if r := cache.Get(context.Background(), key7d, counter.count); r.Unknown || r.Counts.Total.Open != 8 {
		t.Fatalf("after the panic served %+v, want a new count of 8", r)
	}
}
