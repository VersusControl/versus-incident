package snapshot

import (
	"context"
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
