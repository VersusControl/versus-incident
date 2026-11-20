package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The scheduler polls accounts and Regions in turn, so their coverage ends
// differ. settled_until is the earliest end among the pairs being polled,
// never the newest one: past it, a lagging pair has not been read yet.
func TestSettledUntilIsTheEarliestPolledPair(t *testing.T) {
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	fixture.pollSettled(account, region)
	lagging := now.Add(-30 * time.Minute)
	fixture.poll(account, model.GlobalRegion, ChangePoll{Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: lagging}},
		Read: []changes.Interval{{From: now.Add(-26 * time.Hour), To: lagging}}})

	answer := fixture.answer(FeedRequest{Window: 24 * time.Hour})
	if !answer.Window.SettledUntil.Equal(lagging) {
		t.Fatalf("settled_until = %s, want the lagging pair's end %s", answer.Window.SettledUntil, lagging)
	}
}

// Failed attempts beyond the kept number are counted, not stored. The rows
// kept are the earliest by event time, whatever order the poll read them in.
func TestFailedAttemptsKeptAreTheEarliestByEventTime(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		hour := fixture.clock.Now().Add(-3 * time.Hour).Truncate(time.Hour)
		principal := "arn:aws:iam::111122223333:role/nightly-job"
		var newestFirst []model.Change
		for minute := 59; minute >= 0; minute-- {
			at := hour.Add(time.Duration(minute) * time.Minute)
			newestFirst = append(newestFirst, failedChange(fmt.Sprintf("f-%02d", minute), at, principal, "AccessDenied"))
		}
		fixture.poll(account, region, ChangePoll{Found: newestFirst, Covered: []changes.Interval{{From: hour, To: hour.Add(time.Hour)}}})

		stored, err := fixture.provider.(storage.AWSChangeFeedStore).AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(),
			storage.AWSChangeFeedQuery{Since: hour, Until: hour.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if len(stored.Rows) != storage.DefaultAWSChangeFailureKeep {
			t.Fatalf("rows = %d, want %d", len(stored.Rows), storage.DefaultAWSChangeFailureKeep)
		}
		cutoff := hour.Add(time.Duration(storage.DefaultAWSChangeFailureKeep) * time.Minute)
		for _, row := range stored.Rows {
			if !row.EventTime.Before(cutoff) {
				t.Fatalf("kept %s at %s, a later attempt than the first %d", row.EventID, row.EventTime, storage.DefaultAWSChangeFailureKeep)
			}
		}
	})
}

// ParseWindow serves 1h and 24h always and whole days only up to the
// limits: in the open-source limits every longer window is locked, and with
// a 7-day limit 7d and 168h are served while 8d is locked.
func TestParseWindowFollowsLimits(t *testing.T) {
	week := 7 * 24 * time.Hour
	licensed := ChangesLimits{MaxWindow: week, MaxCompareAge: week}
	for _, tc := range []struct {
		value  string
		limits ChangesLimits
		want   time.Duration
		locked bool
		bad    bool
	}{
		{"", DefaultChangesLimits(), FeedWindowDay, false, false},
		{"1h", DefaultChangesLimits(), FeedWindowHour, false, false},
		{"24h", DefaultChangesLimits(), FeedWindowDay, false, false},
		{"7d", DefaultChangesLimits(), 0, true, false},
		{"1d", DefaultChangesLimits(), 0, true, false},
		{"168h", DefaultChangesLimits(), 0, true, false},
		{"25h", DefaultChangesLimits(), 0, true, false},
		{"2h", DefaultChangesLimits(), 0, false, true},
		{"soon", DefaultChangesLimits(), 0, false, true},
		{"7d", ChangesLimits{}, 0, true, false},
		{"7d", licensed, week, false, false},
		{"168h", licensed, week, false, false},
		{"2d", licensed, 48 * time.Hour, false, false},
		{"8d", licensed, 0, true, false},
		{"25h", licensed, 0, false, true},
	} {
		got, locked, err := ParseWindow(tc.value, tc.limits)
		if got != tc.want || locked != tc.locked || (err != nil) != tc.bad {
			t.Errorf("ParseWindow(%q, %+v) = %s, %v, %v", tc.value, tc.limits, got, locked, err)
		}
	}
	for window, want := range map[time.Duration]string{FeedWindowHour: "1h", FeedWindowDay: "24h", week: "7d"} {
		if got := FormatWindow(window); got != want {
			t.Errorf("FormatWindow(%s) = %q, want %q", window, got, want)
		}
	}
}

// The feed holds every request to its limits: without a limits seam a 7-day
// window and a compare 30 h back are locked; with a 7-day seam both answer,
// and a zero seam value falls back to the open-source 24 h.
func TestChangeFeedLimitsSeam(t *testing.T) {
	week := 7 * 24 * time.Hour
	fixture := newFeedFixture(t, scopeOf(region))
	now := fixture.clock.Now()
	fixture.pollSettled(account, region)
	oss := fixture.feed
	if got := oss.Limits(); got != DefaultChangesLimits() {
		t.Fatalf("default limits = %+v", got)
	}
	if _, err := oss.Answer(context.Background(), FeedRequest{Window: week}); !errors.Is(err, ErrWindowLocked) {
		t.Fatalf("7d without the seam = %v", err)
	}
	if _, err := oss.Compare(context.Background(), CompareRequest{From: now.Add(-30 * time.Hour)}); !errors.Is(err, ErrWindowLocked) {
		t.Fatalf("compare 30h back without the seam = %v", err)
	}
	limits := ChangesLimits{MaxWindow: week, MaxCompareAge: week}
	licensed := NewChangeFeed(fixture.store, WithFeedClock(fixture.clock.Now), WithScope(func(context.Context) ([]ScopeAccount, bool, error) { return fixture.scope, true, nil }),
		WithChangesLimits(func() ChangesLimits { return limits }))
	answer, err := licensed.Answer(context.Background(), FeedRequest{Window: week})
	if err != nil || answer.Window.Until.Sub(answer.Window.Since) != week {
		t.Fatalf("7d with a 7-day seam = %+v, %v", answer.Window, err)
	}
	if _, err := licensed.Compare(context.Background(), CompareRequest{From: now.Add(-30 * time.Hour)}); err != nil {
		t.Fatalf("compare 30h back with a 7-day seam = %v", err)
	}
	if _, err := licensed.Answer(context.Background(), FeedRequest{Window: week + FeedWindowDay}); !errors.Is(err, ErrWindowLocked) {
		t.Fatalf("8d with a 7-day seam = %v", err)
	}
	limits = ChangesLimits{}
	if got := licensed.Limits(); got != DefaultChangesLimits() {
		t.Fatalf("zero seam value = %+v", got)
	}
}
