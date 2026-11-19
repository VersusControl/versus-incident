package logging

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultThrottleWindow is how often a failure that keeps happening with the
// same cause is logged again.
const DefaultThrottleWindow = 10 * time.Minute

// Throttle limits the lines a repeating failure writes. For each key it logs:
//
//   - when the failure starts;
//   - when its cause changes (forbidden, then unreachable);
//   - then at most once per window while it continues, with a count:
//     "... (still failing, 20 times since 14:05)";
//   - one debug line when it clears.
//
// Key by what failed (cluster and kind, source name, channel), never by the
// full message, so a changing detail does not defeat the throttle. A cleared
// key is forgotten about one window after it clears, so state stays bounded
// by the keys that are failing now. A key that fails again with the same
// cause within that window continues its episode, so a failure that flaps
// between failing and clear on alternating cycles still logs once per window.
// Call Forget when the thing a key names is removed (a cluster or source is
// deleted from the config), since a key that is still failing is never
// dropped on its own.
//
// The zero value is ready to use with the default window and the real clock.
// A Throttle is safe for concurrent use.
type Throttle struct {
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*throttleEntry
	// nextSweep is when the earliest cleared entry may be dropped; zero when
	// no cleared entry is waiting.
	nextSweep time.Time
}

type throttleEntry struct {
	cause   string
	since   time.Time // start of the current cause
	lastLog time.Time
	count   int // failures since the current cause started
	cleared time.Time
	failing bool
	// firstClear is true from the start of an episode until its first
	// Clear, so a flapping failure reports its recovery once.
	firstClear bool
}

// NewThrottle returns a Throttle that repeats a continuing failure at most
// once per window, reading the time from now. A window of zero or less uses
// DefaultThrottleWindow; a nil now uses time.Now.
func NewThrottle(window time.Duration, now func() time.Time) *Throttle {
	return &Throttle{window: window, now: now}
}

func (t *Throttle) windowLen() time.Duration {
	if t.window <= 0 {
		return DefaultThrottleWindow
	}
	return t.window
}

func (t *Throttle) clock() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

// Fail records one failure for key with a cause class (forbidden,
// unreachable, credential rejected, ...). It writes the formatted message at
// lvl when the failure starts, when the cause differs from the last one, or
// when a window has passed since the last line; in the last case it appends
// how many times it failed since the cause started. It returns whether a line
// was written (or would have been, had lvl been enabled).
func (t *Throttle) Fail(lvl slog.Level, key, cause, format string, args ...any) bool {
	now := t.clock()
	window := t.windowLen()

	t.mu.Lock()
	t.sweepLocked(now, window)
	if t.entries == nil {
		t.entries = make(map[string]*throttleEntry)
	}
	e := t.entries[key]
	repeat := false
	switch {
	case e == nil:
		e = &throttleEntry{}
		t.entries[key] = e
		e.reset(cause, now)
	case e.cause != cause:
		e.reset(cause, now)
	case !e.failing && now.Sub(e.cleared) >= window:
		e.reset(cause, now)
	default:
		// Still failing, or failing again with the same cause within a
		// window of clearing: a flapping failure continues its episode
		// instead of logging a new start on every cycle.
		e.failing = true
		e.cleared = time.Time{}
		e.count++
		if now.Sub(e.lastLog) < window {
			t.mu.Unlock()
			return false
		}
		repeat = true
	}
	e.lastLog = now
	count, since := e.count, e.since
	t.mu.Unlock()

	if !Enabled(lvl) {
		return true
	}
	msg := fmt.Sprintf(format, args...)
	if repeat {
		msg = fmt.Sprintf("%s (still failing, %d times since %s)", msg, count, since.Format("15:04"))
	}
	emit(lvl, 2, msg)
	return true
}

func (e *throttleEntry) reset(cause string, now time.Time) {
	e.cause = cause
	e.since = now
	e.count = 1
	e.failing = true
	e.cleared = time.Time{}
	e.firstClear = true
}

// Clear marks key as healthy again. On the first clear of a failure episode
// it writes the formatted message at debug and returns true; otherwise it
// writes nothing and returns false. The key keeps its last cause for one
// window, so the same failure returning within that window continues the
// episode: it logs no new start, and its later clears return false. A caller
// whose failure started as a warning may log its own single info "recovered"
// line when Clear returns true.
func (t *Throttle) Clear(key, format string, args ...any) bool {
	now := t.clock()
	window := t.windowLen()

	t.mu.Lock()
	t.sweepLocked(now, window)
	e := t.entries[key]
	if e == nil || !e.failing {
		t.mu.Unlock()
		return false
	}
	e.failing = false
	e.cleared = now
	if t.nextSweep.IsZero() || now.Add(window).Before(t.nextSweep) {
		t.nextSweep = now.Add(window)
	}
	first := e.firstClear
	e.firstClear = false
	t.mu.Unlock()

	if !first {
		return false
	}

	if Enabled(slog.LevelDebug) {
		emit(slog.LevelDebug, 2, fmt.Sprintf(format, args...))
	}
	return true
}

// sweepLocked drops entries that cleared at least one window ago. It runs a
// full pass only when such an entry is due, and spaces passes at least a tenth
// of a window apart so many keys clearing at staggered times cost little; an
// entry is therefore dropped between one window and 1.1 windows after it
// clears.
func (t *Throttle) sweepLocked(now time.Time, window time.Duration) {
	if t.nextSweep.IsZero() || now.Before(t.nextSweep) {
		return
	}
	var next time.Time
	for key, e := range t.entries {
		if e.failing {
			continue
		}
		due := e.cleared.Add(window)
		if !now.Before(due) {
			delete(t.entries, key)
			continue
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	if !next.IsZero() {
		if floor := now.Add(window / 10); next.Before(floor) {
			next = floor
		}
	}
	t.nextSweep = next
}

// Forget drops everything the throttle holds for key, failing or not. Call
// it when the thing the key names is removed, so its state does not stay
// behind. A later Fail for the key logs as a new start.
func (t *Throttle) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// Len returns how many keys the throttle currently remembers.
func (t *Throttle) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
