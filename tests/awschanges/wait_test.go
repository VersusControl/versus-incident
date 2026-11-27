package awschanges

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// stopGrace is how long a server gets to shut down after SIGTERM before it
// is killed. It is a hang guard, not a budget: a clean stop takes a second.
const stopGrace = 60 * time.Second

// poll calls cond until it reports true and reports whether it did. guard
// (stretched under race) only catches a hang: a correct server reaches the
// condition long before it, so the caller's own checks then say what is
// wrong with the state it last read.
func poll(guard time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(scaledTimeout(guard))
	for pause := 50 * time.Millisecond; ; pause = min(2*pause, 2*time.Second) {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pause) // testlint:allow the shared wait helper's own poll pause
	}
}

// eventually is poll that fails the test with what at the hang guard.
func eventually(t testing.TB, what string, guard time.Duration, cond func() bool) {
	t.Helper()
	if !poll(guard, cond) {
		t.Fatalf("hang guard (%s) reached: %s", scaledTimeout(guard), what)
	}
}

// waitPasses waits until read reports n more collection passes after from
// and returns the last stamp it counted. read returns the newest stamp the
// server recorded (a poll or a read time). One pass stamps all its units
// within seconds, so a stamp at least half an interval after the last
// counted one starts a new pass.
func waitPasses(t testing.TB, what string, from time.Time, n int, interval time.Duration, read func() time.Time) time.Time {
	t.Helper()
	last, seen := from, 0
	guard := time.Duration(n+1)*3*interval + time.Minute
	eventually(t, fmt.Sprintf("%d more %s after %s", n, what, from.Format(time.RFC3339)), guard, func() bool {
		if at := read(); !at.IsZero() && at.Sub(last) >= interval/2 {
			last, seen = at, seen+1
		}
		return seen >= n
	})
	return last
}

// lastPoll is the newest change-poll stamp in a changes answer.
func lastPoll(view map[string]any) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, text(view, "sync", "last_poll_at"))
	return at
}

// waitPolls waits until the server has recorded n more change-poll passes
// (every interval) after the one it reports now, and returns the changes
// answer that shows the last of them.
func (r *replica) waitPolls(n int, interval time.Duration) map[string]any {
	r.t.Helper()
	view := r.changes("?window=24h&limit=200")
	waitPasses(r.t, r.name+" change polls", lastPoll(view), n, interval, func() time.Time {
		view = r.changes("?window=24h&limit=200")
		return lastPoll(view)
	})
	return view
}

// syncedAt is the newest successful read Settings reports for account.
func (r *replica) syncedAt(account string) time.Time {
	r.t.Helper()
	for _, item := range list(r.call(http.MethodGet, "/api/admin/aws/settings", "").json(), "accounts") {
		if text(item, "account_id") == account {
			at, _ := time.Parse(time.RFC3339Nano, text(item, "sync", "synced_at"))
			return at
		}
	}
	return time.Time{}
}

// waitReads waits until the sampler has made n reads since it started,
// answered or not.
func (s *sampler) waitReads(t testing.TB, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d sampler reads", n), time.Duration(n+1)*30*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.reads >= n
	})
}
