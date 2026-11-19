package logging

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestThrottle() (*Throttle, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 19, 14, 5, 0, 0, time.Local)}
	return NewThrottle(0, clock.now), clock
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestThrottleRepeatingFailure(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, clock := newTestThrottle()
	const key = "prod/Secret"
	fail := func(cause string) bool {
		return th.Fail(slog.LevelWarn, key, cause, "kubernetes: cluster prod cannot list secrets (%s); grant list on secrets", cause)
	}

	if !fail("forbidden") {
		t.Fatal("first failure must log")
	}
	if got := lines(buf.String()); len(got) != 1 || got[0] != "WARN kubernetes: cluster prod cannot list secrets (forbidden); grant list on secrets" {
		t.Fatalf("start line = %q", got)
	}

	buf.Reset()
	for i := range 18 {
		clock.advance(30 * time.Second)
		if fail("forbidden") {
			t.Fatalf("repeat %d within the window logged", i)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("same cause within 10 minutes wrote %q", buf.String())
	}

	// Ten minutes after the start line: one line with the count.
	clock.advance(DefaultThrottleWindow - 18*30*time.Second)
	if !fail("forbidden") {
		t.Fatal("failure after the window must log")
	}
	want := "WARN kubernetes: cluster prod cannot list secrets (forbidden); grant list on secrets (still failing, 20 times since 14:05)"
	if got := lines(buf.String()); len(got) != 1 || got[0] != want {
		t.Fatalf("repeat line = %q, want %q", got, want)
	}

	// A new cause logs at once, without a count.
	buf.Reset()
	clock.advance(time.Second)
	if !fail("unreachable") {
		t.Fatal("cause change must log")
	}
	if got := lines(buf.String()); len(got) != 1 || got[0] != "WARN kubernetes: cluster prod cannot list secrets (unreachable); grant list on secrets" {
		t.Fatalf("cause change line = %q", got)
	}
	buf.Reset()
	clock.advance(time.Second)
	if fail("unreachable") || buf.Len() != 0 {
		t.Fatalf("same new cause right after logged %q", buf.String())
	}
}

func TestThrottleClearLogsAtDebugOnly(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, clock := newTestThrottle()
	th.Fail(slog.LevelWarn, "loki-prod", "unreachable", "agent: source loki-prod unreachable")
	buf.Reset()

	clock.advance(time.Minute)
	if !th.Clear("loki-prod", "agent: source loki-prod recovered") {
		t.Fatal("Clear of a failing key must report true")
	}
	if buf.Len() != 0 {
		t.Fatalf("clear wrote %q at info", buf.String())
	}
	if th.Clear("loki-prod", "agent: source loki-prod recovered") {
		t.Fatal("second Clear must report false")
	}
	if th.Clear("never-failed", "x") {
		t.Fatal("Clear of an unknown key must report false")
	}

	// A window later the failure is a new episode, whose clear logs at debug.
	Setup("debug")
	clock.advance(DefaultThrottleWindow)
	th.Fail(slog.LevelWarn, "loki-prod", "unreachable", "agent: source loki-prod unreachable")
	buf.Reset()
	th.Clear("loki-prod", "agent: source loki-prod recovered")
	if got := buf.String(); got != "DEBUG agent: source loki-prod recovered\n" {
		t.Fatalf("clear at debug wrote %q", got)
	}
}

func TestThrottleFailAfterClearStartsAgainOnlyAfterWindow(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, clock := newTestThrottle()
	th.Fail(slog.LevelError, "slack", "credential rejected", "slack: token rejected")
	clock.advance(time.Minute)
	th.Clear("slack", "slack: ok")

	// The same cause within a window of the clear continues the episode.
	clock.advance(time.Minute)
	buf.Reset()
	if th.Fail(slog.LevelError, "slack", "credential rejected", "slack: token rejected") || buf.Len() != 0 {
		t.Fatalf("same cause soon after a clear logged %q", buf.String())
	}

	// A different cause right after a clear logs at once.
	clock.advance(time.Second)
	th.Clear("slack", "slack: ok")
	if !th.Fail(slog.LevelError, "slack", "unreachable", "slack: unreachable") {
		t.Fatal("a new cause after a clear must log")
	}

	// A window after the clear, the same cause is a new start without a count.
	th.Clear("slack", "slack: ok")
	clock.advance(DefaultThrottleWindow)
	buf.Reset()
	if !th.Fail(slog.LevelError, "slack", "unreachable", "slack: unreachable") {
		t.Fatal("a failure a window after a clear is a new start and must log")
	}
	if got := buf.String(); got != "ERROR slack: unreachable\n" {
		t.Fatalf("restart line = %q", got)
	}
}

func TestThrottleFlappingFailureLogsOncePerWindow(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, clock := newTestThrottle()
	const key = "loki-prod"
	for range 20 {
		th.Fail(slog.LevelWarn, key, "unreachable", "agent: source loki-prod unreachable")
		clock.advance(15 * time.Second)
		th.Clear(key, "agent: source loki-prod recovered")
		clock.advance(15 * time.Second)
	}
	if got := lines(buf.String()); len(got) != 1 || got[0] != "WARN agent: source loki-prod unreachable" {
		t.Fatalf("20 fail/clear cycles in one window wrote %q, want one start line", got)
	}

	// Ten minutes after the start line, the flapping failure logs once with
	// its count.
	buf.Reset()
	clock.advance(DefaultThrottleWindow - 20*30*time.Second)
	if !th.Fail(slog.LevelWarn, key, "unreachable", "agent: source loki-prod unreachable") {
		t.Fatal("flapping failure after a window must log")
	}
	want := "WARN agent: source loki-prod unreachable (still failing, 21 times since 14:05)"
	if got := lines(buf.String()); len(got) != 1 || got[0] != want {
		t.Fatalf("repeat line = %q, want %q", got, want)
	}
}

func TestThrottleFlappingFailureReportsRecoveryOnce(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("debug")
	th, clock := newTestThrottle()
	const key = "loki-prod"
	cleared := 0
	for range 20 {
		th.Fail(slog.LevelWarn, key, "unreachable", "agent: source loki-prod unreachable")
		clock.advance(15 * time.Second)
		if th.Clear(key, "agent: source loki-prod recovered") {
			cleared++
		}
		clock.advance(15 * time.Second)
	}
	if cleared != 1 {
		t.Fatalf("Clear returned true %d times over 20 flapping cycles, want 1", cleared)
	}
	var debug []string
	for _, l := range lines(buf.String()) {
		if strings.HasPrefix(l, "DEBUG ") {
			debug = append(debug, l)
		}
	}
	if len(debug) != 1 || debug[0] != "DEBUG agent: source loki-prod recovered" {
		t.Fatalf("debug lines = %q, want one recovery line", debug)
	}

	// A window after the last clear, a new episode reports its recovery again.
	clock.advance(DefaultThrottleWindow)
	th.Fail(slog.LevelWarn, key, "unreachable", "agent: source loki-prod unreachable")
	if !th.Clear(key, "agent: source loki-prod recovered") {
		t.Fatal("the first clear of a new episode must return true")
	}
}

func TestThrottleForgetDropsFailingKey(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, clock := newTestThrottle()
	th.Fail(slog.LevelWarn, "cluster-old/Pod", "unreachable", "kubernetes: cluster old unreachable")
	th.Fail(slog.LevelWarn, "cluster-new/Pod", "unreachable", "kubernetes: cluster new unreachable")
	th.Forget("cluster-old/Pod")
	th.Forget("never-seen")
	if got := th.Len(); got != 1 {
		t.Fatalf("Len = %d after Forget, want 1", got)
	}
	if th.Clear("cluster-old/Pod", "x") {
		t.Fatal("a forgotten key must not be failing")
	}
	buf.Reset()
	clock.advance(time.Second)
	if !th.Fail(slog.LevelWarn, "cluster-old/Pod", "unreachable", "kubernetes: cluster old unreachable") {
		t.Fatal("a forgotten key failing again is a new start and must log")
	}
	if buf.String() != "WARN kubernetes: cluster old unreachable\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestThrottleSweepDropsLateClearWithinWindowAndATenth(t *testing.T) {
	captureLog(t, 0, "")
	Setup("error")
	th, clock := newTestThrottle()
	th.Fail(slog.LevelWarn, "a", "x", "a")
	th.Clear("a", "a")
	clock.advance(DefaultThrottleWindow - time.Second)
	th.Fail(slog.LevelWarn, "b", "x", "b")
	th.Clear("b", "b")
	clock.advance(time.Second) // a is due: the sweep drops it and keeps b
	th.Fail(slog.LevelWarn, "c", "x", "c")
	if got := th.Len(); got != 2 {
		t.Fatalf("Len = %d after a is due, want 2 (b and c)", got)
	}
	clock.advance(DefaultThrottleWindow - time.Second) // b is due
	th.Fail(slog.LevelWarn, "c", "x", "c")
	if got := th.Len(); got != 1 {
		t.Fatalf("Len = %d after b is due, want 1 (c)", got)
	}
}

func TestThrottleConcurrentUse(t *testing.T) {
	captureLog(t, 0, "")
	th := NewThrottle(time.Millisecond, nil)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				key := fmt.Sprintf("k%d", i%50)
				if i%3 == 0 {
					th.Clear(key, "ok")
				} else {
					th.Fail(slog.LevelWarn, key, fmt.Sprint(i%2), "failed")
				}
				Debugf("detail")
				Warnf("warning")
				if i%500 == 0 {
					Setup([]string{"debug", "info", "warn"}[g%3])
				}
				if i%700 == 0 {
					th.Forget(key)
				}
			}
		}()
	}
	wg.Wait()
	if n := th.Len(); n > 50 {
		t.Fatalf("Len = %d, want at most the 50 keys used", n)
	}
}

func TestThrottleKeysAreIndependent(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	th, _ := newTestThrottle()
	th.Fail(slog.LevelWarn, "a", "forbidden", "a failed")
	th.Fail(slog.LevelWarn, "b", "forbidden", "b failed")
	th.Fail(slog.LevelWarn, "a", "forbidden", "a failed")
	if got := buf.String(); got != "WARN a failed\nWARN b failed\n" {
		t.Fatalf("got %q", got)
	}
}

func TestThrottleStateBoundedAndDroppedAfterClear(t *testing.T) {
	captureLog(t, 0, "")
	Setup("error")
	th, clock := newTestThrottle()
	const n = 10000
	for i := range n {
		th.Fail(slog.LevelWarn, fmt.Sprintf("cluster-%d/Pod", i), "unreachable", "x")
	}
	// The same keys failing again add no entries.
	clock.advance(time.Minute)
	for i := range n {
		th.Fail(slog.LevelWarn, fmt.Sprintf("cluster-%d/Pod", i), "forbidden", "x")
	}
	if got := th.Len(); got != n {
		t.Fatalf("Len = %d after repeated failures, want %d", got, n)
	}

	for i := range n {
		th.Clear(fmt.Sprintf("cluster-%d/Pod", i), "x")
	}
	clock.advance(DefaultThrottleWindow - time.Second)
	th.Fail(slog.LevelWarn, "still-failing", "unreachable", "x")
	if got := th.Len(); got != n+1 {
		t.Fatalf("Len = %d before a window passed, want %d", got, n+1)
	}

	clock.advance(time.Second)
	th.Clear("never-failed", "x")
	if got := th.Len(); got != 1 {
		t.Fatalf("Len = %d one window after clearing, want 1 (the key still failing)", got)
	}
}

func TestThrottleZeroValueUsesDefaults(t *testing.T) {
	buf := captureLog(t, 0, "")
	Setup("info")
	var th Throttle
	if !th.Fail(slog.LevelWarn, "k", "c", "first") {
		t.Fatal("zero Throttle must log the first failure")
	}
	if th.Fail(slog.LevelWarn, "k", "c", "second") {
		t.Fatal("zero Throttle must suppress an immediate repeat")
	}
	if buf.String() != "WARN first\n" {
		t.Fatalf("got %q", buf.String())
	}
}
