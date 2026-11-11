package snapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// TestAsOfOlderThanKeptHistoryIsAGapNotEmpty: once the bounded history has
// pruned a generation, a read as of that time must say the unit was not
// checked then. It must never answer with an empty unit that looks like
// "nothing was running", and the generations still kept answer exactly.
func TestAsOfOlderThanKeptHistoryIsAGapNotEmpty(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t, WithOptions(func() Options { return Options{MaxGenerations: 2, MaxAge: 24 * time.Hour} }))
		ctx := context.Background()
		start := c.Now()
		for _, id := range []string{"i-a", "i-b", "i-c", "i-d"} {
			if _, err := store.Commit(unit("ec2"), candidate(id)); err != nil {
				t.Fatal(err)
			}
			c.Advance(10 * time.Minute)
		}

		// i-a and i-b were pruned by the count bound.
		for _, offset := range []time.Duration{time.Minute, 11 * time.Minute} {
			asOf := start.Add(offset)
			result, err := store.Resources(ctx, Query{Account: account, Service: "ec2", AsOf: asOf})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Resources) != 0 {
				t.Fatalf("as of +%s served pruned data: %+v", offset, result.Resources)
			}
			if len(result.Gaps) != 1 || result.Gaps[0].Reason != model.GapNotChecked || result.Gaps[0].Service != "ec2" {
				t.Fatalf("as of +%s gaps = %+v, want one not_checked gap", offset, result.Gaps)
			}
			if len(result.Units) != 1 || result.Units[0].HasData || result.Units[0].Count != 0 {
				t.Fatalf("as of +%s unit claims data it no longer has: %+v", offset, result.Units)
			}
			if _, _, found, err := store.Resource(ctx, instance("i-a").ARN, asOf); err != nil || found {
				t.Fatalf("pruned resource as of +%s found=%v err=%v", offset, found, err)
			}
		}

		// The kept generations still answer, through every read.
		for offset, want := range map[time.Duration]string{21 * time.Minute: "i-c", 31 * time.Minute: "i-d"} {
			if got := strings.Join(names(t, store, start.Add(offset)), ","); got != want {
				t.Fatalf("as of +%s = %q, want %q", offset, got, want)
			}
		}
		resource, status, found, err := store.Resource(ctx, instance("i-c").ARN, start.Add(25*time.Minute))
		if err != nil || !found || resource.Name != "i-c" || !status.HasData || status.Count != 1 {
			t.Fatalf("Resource as of a kept generation = %+v %+v found=%v err=%v", resource, status, found, err)
		}
		if _, _, found, _ := store.Resource(ctx, instance("i-c").ARN, time.Time{}); found {
			t.Fatal("latest read returned a resource that is no longer current")
		}

		// A fresh store over the same storage agrees.
		reloaded := New(store.provider, tenancy.DefaultOrgScope())
		result, err := reloaded.Resources(ctx, Query{Account: account, Service: "ec2", AsOf: start.Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Resources) != 0 || len(result.Gaps) != 1 || result.Gaps[0].Reason != model.GapNotChecked {
			t.Fatalf("reloaded as of a pruned time = %+v", result)
		}
	})
}
