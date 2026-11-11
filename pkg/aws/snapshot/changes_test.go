package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func change(id string, at time.Time, service string, resources ...string) model.Change {
	return model.Change{ID: id, Time: at, Account: account, Region: region, Service: service, EventName: "Update", Summary: id, Resources: resources}
}

func TestChangesReportCoverageGaps(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		now := c.Now()
		ctx := context.Background()

		// Nothing polled yet but the unit is expected: the whole window is a gap.
		store.RecordPending([]model.Unit{unit(ChangeService)})
		empty, err := store.Changes(ctx, Query{Account: account}, now.Add(-time.Hour), now)
		if err != nil {
			t.Fatal(err)
		}
		if empty.Complete || len(empty.Gaps) != 1 || empty.Gaps[0].Reason != model.GapNotChecked {
			t.Fatalf("unpolled = %+v", empty)
		}

		found := []model.Change{
			change("a", now.Add(-50*time.Minute), "ecs", "arn:aws:ecs:us-east-1:111122223333:service/versus/checkout"),
			change("b", now.Add(-10*time.Minute), "lambda", "worker"),
		}
		if err := store.AppendChanges(account, region, found, []changes.Interval{{From: now.Add(-time.Hour), To: now}}, ""); err != nil {
			t.Fatal(err)
		}
		full, _ := store.Changes(ctx, Query{Account: account}, now.Add(-time.Hour), now)
		if !full.Complete || len(full.Changes) != 2 || full.Changes[0].ID != "a" {
			t.Fatalf("covered window = %+v", full)
		}
		// Re-appending the same events does not duplicate them.
		store.AppendChanges(account, region, found, nil, "")
		again, _ := store.Changes(ctx, Query{Account: account}, now.Add(-time.Hour), now)
		if len(again.Changes) != 2 {
			t.Fatalf("duplicates after re-append: %d", len(again.Changes))
		}

		byService, _ := store.Changes(ctx, Query{Account: account, Service: "lambda"}, now.Add(-time.Hour), now)
		byARN, _ := store.Changes(ctx, Query{Account: account, ARN: "arn:aws:lambda:us-east-1:111122223333:function:worker"}, now.Add(-time.Hour), now)
		if len(byService.Changes) != 1 || len(byARN.Changes) != 1 || byARN.Changes[0].ID != "b" {
			t.Fatalf("filters = %+v / %+v", byService.Changes, byARN.Changes)
		}

		// A wider window than was read reports exactly the unread stretch.
		wide, _ := store.Changes(ctx, Query{Account: account}, now.Add(-2*time.Hour), now.Add(time.Hour))
		if wide.Complete || len(wide.Gaps) != 2 || !wide.Gaps[0].To.Equal(now.Add(-time.Hour)) || !wide.Gaps[1].From.Equal(now) {
			t.Fatalf("wide window gaps = %+v", wide.Gaps)
		}

		// A failed poll names its reason on the uncovered stretch.
		c.Advance(time.Hour)
		store.AppendChanges(account, region, nil, nil, "throttled")
		failed, _ := store.Changes(ctx, Query{Account: account}, now, c.Now())
		if failed.Complete || len(failed.Gaps) != 1 || failed.Gaps[0].Reason != model.GapThrottled {
			t.Fatalf("failed poll = %+v", failed.Gaps)
		}

		// As-of caps the window.
		asOf, _ := store.Changes(ctx, Query{Account: account, AsOf: now.Add(-30 * time.Minute)}, now.Add(-time.Hour), now)
		if len(asOf.Changes) != 1 || asOf.Changes[0].ID != "a" {
			t.Fatalf("as of = %+v", asOf.Changes)
		}

		// Another account with no checkpoint is not reported complete.
		other, _ := store.Changes(ctx, Query{Account: "444455556666"}, now.Add(-time.Hour), now)
		if other.Complete {
			t.Fatal("an account that was never polled claimed a complete window")
		}
	})
}

func TestChangeRetentionTrimsCoverageAndDays(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t, WithOptions(func() Options { return Options{ChangeRetention: 24 * time.Hour} }))
		start := c.Now()
		store.AppendChanges(account, region, []model.Change{change("old", start.Add(-time.Hour), "ec2")}, []changes.Interval{{From: start.Add(-2 * time.Hour), To: start}}, "")
		c.Advance(72 * time.Hour)
		store.AppendChanges(account, region, []model.Change{change("new", c.Now().Add(-time.Minute), "ec2")}, []changes.Interval{{From: c.Now().Add(-time.Hour), To: c.Now()}}, "")
		checkpoint, _ := store.Checkpoint(account, region)
		if len(checkpoint.Covered) != 1 || !checkpoint.Covered[0].From.Equal(c.Now().Add(-time.Hour)) {
			t.Fatalf("checkpoint after retention = %+v", checkpoint)
		}
		kept, _ := store.Changes(context.Background(), Query{Account: account}, start.Add(-3*time.Hour), c.Now())
		if len(kept.Changes) != 1 || kept.Changes[0].ID != "new" {
			t.Fatalf("expired change still stored: %+v", kept.Changes)
		}
		result, _ := store.Changes(context.Background(), Query{Account: account}, start.Add(-2*time.Hour), start)
		if result.Complete || len(result.Changes) != 0 {
			t.Fatalf("pruned window = %+v", result)
		}
	})
}
