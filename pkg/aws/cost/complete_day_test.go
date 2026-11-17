package cost

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestLastCompleteDayFollowsTheClockNotTheEstimatedFlag: AWS marks every day
// of the open month Estimated, so completeness comes from the clock. On
// Nov 9 10:00 UTC, Nov 7 ended Nov 8 00:00 and settled by Nov 9 00:00;
// Nov 8 settles only at Nov 10 00:00.
func TestLastCompleteDayFollowsTheClockNotTheEstimatedFlag(t *testing.T) {
	h, _ := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	st, err := h.adapter.Source().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := finops.MustDate("2026-11-07"); st.LastCompleteDay != want {
		t.Fatalf("last complete day %s, want %s", st.LastCompleteDay, want)
	}
	svc := finops.NewService(h.adapter.Source(), NewStore(h.provider, h.adapter.cache.scope), finops.Config{Now: h.clock.Now})
	sum, err := svc.Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if r := sum.Header.Period.Range; r.Start != finops.MustDate("2026-11-01") || r.End != finops.MustDate("2026-11-07") {
		t.Fatalf("month to date %+v, want Nov 1-7", r)
	}
	// Nov 10 10:00: Nov 8 has settled, and October is backfilled so the
	// detection baseline exists.
	backfillOrganization(t, h)
	st, _ = h.adapter.Source().Status(ctx)
	if want := finops.MustDate("2026-11-08"); st.LastCompleteDay != want {
		t.Fatalf("next day: last complete day %s, want %s", st.LastCompleteDay, want)
	}
	if _, err := svc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	inc, err := svc.Increases(ctx, finops.Query{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	november := false
	for _, v := range inc.Items {
		november = november || v.StartedOn.Month == 11
	}
	if !november {
		t.Fatalf("an increase can start in November: %+v", inc.Items)
	}
}

func TestCompleteThroughIsEndOfDayPlusSettle(t *testing.T) {
	for now, want := range map[string]string{
		"2026-11-09T10:00:00Z": "2026-11-07",
		"2026-11-09T00:00:00Z": "2026-11-07",
		"2026-11-08T23:59:59Z": "2026-11-06",
		"2026-11-10T00:00:00Z": "2026-11-08",
	} {
		ts, _ := timeParse(now)
		if got := completeThrough(ts, DaySettle); got != finops.MustDate(want) {
			t.Errorf("%s: complete through %s, want %s", now, got, want)
		}
	}
}

func timeParse(s string) (t time.Time, err error) { return time.Parse(time.RFC3339, s) }
