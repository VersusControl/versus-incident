package cost

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestLargeOrganizationSetupFitsTheCapOrNamesWhatIsMissing(t *testing.T) {
	sc := loadScenario(t, "large-organization")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{{AccountID: "999999999999", Organization: true, Credentials: testCreds}}
		o.Connected = nil
	})
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if used := h.adapter.Meter().Used(ctx); used > 20 {
		t.Fatalf("day 1 spent %d", used)
	}
	t.Logf("day 1: committed %v, spent %d, skipped %v", res.Committed, h.adapter.Meter().Used(ctx), res.Skipped)
	src := h.adapter.Source()
	if len(res.Committed) == 0 {
		// Day 1 did not fit: a named partial, never a silent one.
		st, _ := src.Status(ctx)
		if st.Enabled && !st.Stale {
			t.Fatalf("an uncommitted first setup must be named: %+v", st)
		}
		if len(res.Skipped) == 0 {
			t.Fatal("the skipped calls are named")
		}
		ps := mustStatus(t, h).Payers["999999999999"]
		if ps.ErrorCode != finops.SkipCapMidRun || ps.Pages[callAccount].Pages == 0 {
			t.Fatalf("payer state %+v", ps)
		}
		// Day 2: priced from the pages day 1 used, the setup window
		// shrinks to what fits and commits.
		h.clock.set(at("2026-11-10", 10))
		res, err = h.adapter.Refresh(ctx, KindScheduled)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(res.Committed, "999999999999|setup") {
			t.Fatalf("day 2 still does not commit: %+v", res)
		}
		if used := h.adapter.Meter().Used(ctx); used > 20 {
			t.Fatalf("day 2 spent %d", used)
		}
		t.Logf("day 2: committed %v, spent %d, window from %s", res.Committed, h.adapter.Meter().Used(ctx), mustStatus(t, h).Payers["999999999999"].HistoryStart)
	}
	ps := mustStatus(t, h).Payers["999999999999"]
	if ps.HistoryStart.IsZero() || len(ps.Accounts) < 60 {
		t.Fatalf("history %s accounts %d", ps.HistoryStart, len(ps.Accounts))
	}
	// Totals of the committed days are exact.
	r := finops.DateRange{Start: ps.HistoryStart, End: ps.LastCompleteDay}
	lines, _ := src.Lines(ctx, r)
	want := sumRows(sc.rows(), func(x row) bool { return r.Contains(x.day) })
	if got := sumLines(lines, nil); got != want {
		t.Fatalf("committed days %d, want %d", got, want)
	}
	// The cap suggestion is priced from learned pages: this Organization
	// needs more than 20 a day for full detail.
	if c, _ := h.adapter.SuggestedCap(ctx); c <= 20 {
		t.Fatalf("suggested cap %d", c)
	}
}

func TestBackfillRunsOnLaterDaysWithTheBudgetLeft(t *testing.T) {
	h, _ := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-10", 10))
	est, err := h.adapter.Estimate(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	backfill := 0
	for _, c := range est.Run {
		if c.Group == "999999999999|backfill" {
			backfill += c.Requests
		}
	}
	if backfill == 0 || est.Estimate > 40 {
		t.Fatalf("backfill %d within %d", backfill, est.Estimate)
	}
}

func TestNilHTTPClientDefaultsToTheHardenedClient(t *testing.T) {
	a, err := New(storage.NewMemory(), tenancy.DefaultOrgScope(), Options{Payers: []Payer{{AccountID: "111111111111", Credentials: testCreds}}})
	if err != nil {
		t.Fatal(err)
	}
	if a.opts.HTTPClient == nil || a.env(a.opts.Payers[0]).httpClient == nil {
		t.Fatal("a nil transport must default, never panic on the first call")
	}
	if a.env(a.opts.Payers[0]).costExplorer() == nil {
		t.Fatal("client")
	}
}

func TestTruncatedPaginationIsNeverComplete(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	old := maxPages
	maxPages = 2
	defer func() { maxPages = old }()
	h.fake.pageGroups = 1
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 0 {
		t.Fatalf("a truncated table committed: %v", res.Committed)
	}
	if code := mustStatus(t, h).Payers["111111111111"].ErrorCode; code != CodeUnavailable {
		t.Fatalf("truncation code %q", code)
	}
	if !errors.Is(errTruncated, errMalformed) || ErrorCode(errTruncated) != CodeUnavailable {
		t.Fatal("truncation is a named malformed answer")
	}
}

func TestDrillDownErrorsUseTheClosedVocabulary(t *testing.T) {
	h, _ := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	h.fake.failures[callDrill] = fakeFailure{Type: "AccessDeniedException", Message: "User arn:aws:iam::999999999999:role/x is not authorized"}
	err := h.adapter.DrillDown(ctx, "333333333333", "Amazon Simple Storage Service")
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != CodeDenied || strings.Contains(err.Error(), "arn:") {
		t.Fatalf("drill-down error %v", err)
	}
}

func TestDrilledEntriesArePrunedAfterTheCooldown(t *testing.T) {
	h, _ := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if n := len(mustStatus(t, h).Payers["999999999999"].Drilled); n == 0 {
		t.Fatal("drilled pairs recorded")
	}
	h.fake.failures[callDrill] = fakeFailure{Type: "LimitExceededException", Message: "slow"}
	h.clock.set(at("2026-11-17", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if d := mustStatus(t, h).Payers["999999999999"].Drilled; len(d) != 0 {
		t.Fatalf("entries older than %d days stay: %v", drillCooldownDays, d)
	}
}
