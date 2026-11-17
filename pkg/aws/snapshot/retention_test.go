package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// sweepUntilDone runs bounded retention passes until the backlog is empty,
// so a batch or time limit can never hide a row that should have gone.
func sweepUntilDone(t *testing.T, store *Store) storage.AWSRetentionResult {
	t.Helper()
	var total storage.AWSRetentionResult
	for pass := 0; pass < 100; pass++ {
		result, err := store.SweepRetention(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		total.Generations += result.Generations
		total.History += result.History
		total.Changes += result.Changes
		total.Units += result.Units
		if !result.More {
			return total
		}
	}
	t.Fatal("retention sweep did not finish in 100 passes")
	return total
}

// TestRetentionSweepDropsExpiredRowsAndKeepsCurrent proves the configured
// count, age and change retention run on every backend, including Postgres:
// the newest generations stay, older history goes, changes past the cutoff
// go, and a current row is never deleted.
func TestRetentionSweepDropsExpiredRowsAndKeepsCurrent(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		c := &clock{now: base}
		store := New(openBackend(t), tenancy.DefaultOrgScope(),
			WithClock(c.Now),
			WithOptions(func() Options {
				return Options{MaxGenerations: 2, MaxAge: 48 * time.Hour, ChangeRetention: 48 * time.Hour}
			}),
			WithRetentionSweep(-1, 2),
		)
		var committed []time.Time
		for _, name := range []string{"a", "b", "c", "d"} {
			if _, err := store.Commit(unit("ec2"), candidate(name)); err != nil {
				t.Fatal(err)
			}
			committed = append(committed, c.Now())
			c.Advance(time.Hour)
		}
		if got := names(t, store, committed[0]); len(got) != 0 {
			t.Fatalf("generation a survived the count retention: %v", got)
		}
		if got := names(t, store, committed[1]); len(got) != 0 {
			t.Fatalf("generation b survived the count retention: %v", got)
		}
		if got := names(t, store, committed[2]); len(got) != 1 || got[0] != "c" {
			t.Fatalf("the newest kept generation reads %v, want c and d", got)
		}
		if got := names(t, store, time.Time{}); len(got) != 1 || got[0] != "d" {
			t.Fatalf("current resources = %v, want d", got)
		}

		if err := store.AppendChanges(account, region, []model.Change{
			{ID: "old", Time: base.Add(-time.Hour), Account: account, Region: region},
			{ID: "kept", Time: base.Add(60 * time.Hour), Account: account, Region: region},
		}, nil, ""); err != nil {
			t.Fatal(err)
		}

		// Two days past the last commit, everything superseded is older
		// than the age cutoff.
		c.Advance(71 * time.Hour)
		result := sweepUntilDone(t, store)
		if result.History == 0 {
			t.Fatal("the age sweep removed no history")
		}
		if got := names(t, store, committed[2]); len(got) != 0 {
			t.Fatalf("history inside the age cutoff survived: %v", got)
		}
		if got := names(t, store, time.Time{}); len(got) != 1 || got[0] != "d" {
			t.Fatalf("the sweep deleted current rows: %v", got)
		}

		changes, err := store.inventory.AWSChanges(context.Background(), store.scope, storage.AWSChangeQuery{
			Account: account, Region: region,
			Since: base.Add(-24 * time.Hour), Until: base.Add(100 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 1 || changes[0].EventID != "kept" {
			t.Fatalf("changes after the sweep = %+v, want only the one inside retention", changes)
		}

		// A current row survives even a cutoff far in the future: retention
		// only ever deletes rows that stopped being current.
		c.Advance(100 * 365 * 24 * time.Hour)
		sweepUntilDone(t, store)
		if got := names(t, store, time.Time{}); len(got) != 1 || got[0] != "d" {
			t.Fatalf("a far-future cutoff deleted the current row: %v", got)
		}
	})
}
