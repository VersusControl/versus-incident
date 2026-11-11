package finops

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

func TestParseMicrosExact(t *testing.T) {
	cases := map[string]Micros{
		"0":                    0,
		"12.34":                12_340_000,
		"  42 ":                42_000_000,
		"-0.01":                -10_000,
		"+1.5":                 1_500_000,
		"0.0000004":            0,
		"0.0000005":            1,
		"-0.0000005":           -1,
		"123.4567894999":       123_456_789,
		"1.2E-7":               0,
		"5.04E-6":              5,
		"1e3":                  1_000_000_000,
		"9223372036854.775807": 9_223_372_036_854_775_807,
		".5":                   500_000,
		"7.":                   7_000_000,
	}
	for in, want := range cases {
		got, err := ParseMicros(in)
		if err != nil || got != want {
			t.Errorf("ParseMicros(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "--1", "1e", "1e999", "١"} {
		if _, err := ParseMicros(bad); err == nil {
			t.Errorf("ParseMicros(%q) should fail", bad)
		}
	}
	if _, err := ParseMicros("9223372036854.775808"); !errors.Is(err, ErrMoneyOverflow) {
		t.Errorf("overflow must be reported, got %v", err)
	}
	if FormatMicros(-1_500_001) != "-1.500001" || FormatMicros(0) != "0.000000" {
		t.Error("FormatMicros")
	}
}

// Summing parsed strings is exact: a thousand 0.000001 lines are exactly
// one thousandth, where float addition would drift.
func TestSumsAreExact(t *testing.T) {
	var sum Micros
	for range 1000 {
		v, _ := ParseMicros("0.000001")
		sum += v
	}
	if sum != 1000 {
		t.Fatalf("sum = %d", sum)
	}
	tenth, _ := ParseMicros("0.1")
	if tenth*3 != 300_000 {
		t.Fatal("0.1 × 3 must be exactly 0.3")
	}
}

func TestResolvePeriod(t *testing.T) {
	last := MustDate("2026-11-03")
	hist := MustDate("2026-08-01")
	p, c, err := ResolvePeriod("", "", last, hist)
	if err != nil {
		t.Fatal(err)
	}
	if p.Key != PeriodMonthToDate || !p.Open || p.Range != (DateRange{MustDate("2026-11-01"), last}) || p.CompleteDays != 3 {
		t.Fatalf("mtd = %+v", p)
	}
	if c.Range != (DateRange{MustDate("2026-10-01"), MustDate("2026-10-03")}) || !c.Available || c.Label != "October" {
		t.Fatalf("open month must compare the same number of complete days: %+v", c)
	}

	p, c, _ = ResolvePeriod("2026-10", ComparePrevPeriod, last, hist)
	if p.Open || p.Range.Days() != 31 || c.Range != (DateRange{MustDate("2026-09-01"), MustDate("2026-09-30")}) {
		t.Fatalf("closed month: %+v %+v", p, c)
	}

	// March compared with February: the base clamps to February's length.
	p, c, _ = ResolvePeriod("2026-03", "", MustDate("2026-03-30"), MustDate("2025-01-01"))
	if c.Range != (DateRange{MustDate("2026-02-01"), MustDate("2026-02-28")}) || !p.Open {
		t.Fatalf("clamp: %+v", c)
	}

	_, c, _ = ResolvePeriod(PeriodLast30Days, CompareLastYear, last, MustDate("2025-01-01"))
	if c.Range != (DateRange{MustDate("2025-10-05"), MustDate("2025-11-03")}) {
		t.Fatalf("last year: %+v", c)
	}

	// Connected mid-September: October cannot be compared.
	_, c, _ = ResolvePeriod("2026-10", "", last, MustDate("2026-09-12"))
	if c.Available || c.Reason != "no_full_base_period" || c.FirstFull != "2026-10" {
		t.Fatalf("no base: %+v", c)
	}

	if _, _, err := ResolvePeriod("2026-12", "", last, hist); !errors.Is(err, ErrBadQuery) {
		t.Fatalf("future month must be rejected, got %v", err)
	}
	if _, _, err := ResolvePeriod("", "", Date{}, hist); err == nil {
		t.Fatal("no complete day must be rejected")
	}
}

func TestQueryNormalize(t *testing.T) {
	q, err := Query{Accounts: []string{"b,a", " a "}}.Normalize()
	if err != nil || q.Period != PeriodMonthToDate || q.Compare != ComparePrevPeriod || q.Depth != "summary" ||
		len(q.Accounts) != 2 || q.Accounts[0] != "a" {
		t.Fatalf("defaults: %+v %v", q, err)
	}
	for _, bad := range []Query{{Period: "2026-13"}, {Period: "x"}, {Compare: "y"}, {GroupBy: "org"}, {Depth: "raw"}, {Status: "closed"}} {
		if _, err := bad.Normalize(); !errors.Is(err, ErrBadQuery) {
			t.Errorf("%+v should be rejected", bad)
		}
	}
}

func TestMonthClosedOnTheFifth(t *testing.T) {
	oct := MustDate("2026-10-01")
	if MonthClosed(oct, MustDate("2026-11-04"), 5) {
		t.Fatal("October is open on Nov 4")
	}
	if !MonthClosed(oct, MustDate("2026-11-05"), 5) {
		t.Fatal("October closes on Nov 5")
	}
}

func TestOwnershipOrder(t *testing.T) {
	o := Ownership{Tag: TagStatus{Key: "team", Active: true, Allowlisted: true}, AccountOwners: map[string]string{"a1": "platform"}}
	if got := o.Resolve("a1", "checkout", "nat-1a", narrative.Summary); got.Name != "checkout" || got.Rule != "tag" || got.RuleText != "team=checkout tag on nat-1a" {
		t.Fatalf("tag first: %+v", got)
	}
	if got := o.Resolve("a1", "", "", narrative.Summary); got.Name != "platform" || got.Rule != "account" {
		t.Fatalf("account owner second: %+v", got)
	}
	if got := o.Resolve("a2", "", "", narrative.Summary); got.Name != Unallocated || got.Rule != "unallocated" {
		t.Fatalf("unallocated last: %+v", got)
	}
	o.Tag.Allowlisted = false
	if got := o.Resolve("a1", "checkout", "", narrative.Summary); got.Rule != "account" {
		t.Fatalf("a tag key outside the allowlist must not name an owner: %+v", got)
	}
}

// --- drivers ---------------------------------------------------------------

var (
	baseR   = DateRange{MustDate("2026-09-01"), MustDate("2026-09-30")}
	periodR = DateRange{MustDate("2026-10-01"), MustDate("2026-10-31")}
)

func spread(r DateRange, l CostLine, perDay Micros, qtyPerDay int64) []CostLine {
	var out []CostLine
	r.Each(func(d Date) {
		c := l
		c.Day, c.NetAmortizedMicros, c.QuantityMicros = d, perDay, qtyPerDay
		out = append(out, c)
	})
	return out
}

func causesOf(b Bridge) map[string]Micros {
	m := map[string]Micros{}
	for _, d := range b.Drivers {
		m[d.Cause] += d.Amount
	}
	return m
}

func assertSums(t *testing.T, b Bridge) {
	t.Helper()
	sum := b.Other.Amount
	for _, d := range b.Drivers {
		sum += d.Amount
	}
	if sum != b.Change || b.Base+b.Change != b.Total {
		t.Fatalf("drivers sum %d != change %d (base %d total %d)", sum, b.Change, b.Base, b.Total)
	}
	if b.Up+b.Down != b.Change {
		t.Fatalf("up %d + down %d != change %d", b.Up, b.Down, b.Change)
	}
}

func TestDriverCauseRules(t *testing.T) {
	opts := BridgeOptions{MinDriver: 1}
	l := CostLine{Account: "a", Service: "compute.vm", UsageType: "u", RecordType: RecordUsage}
	anchor := spread(baseR, CostLine{Account: "a", Service: "anchor", RecordType: RecordUsage}, usd(1), 0)
	anchor = append(anchor, spread(periodR, CostLine{Account: "a", Service: "anchor", RecordType: RecordUsage}, usd(1), 0)...)

	t.Run("new and removed resources", func(t *testing.T) {
		lines := append([]CostLine{}, anchor...)
		n := l
		n.ResourceID = "new"
		lines = append(lines, spread(DateRange{MustDate("2026-10-20"), periodR.End}, n, usd(10), 1)...)
		r := l
		r.ResourceID = "old"
		lines = append(lines, spread(baseR, r, usd(5), 1)...)
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		c := causesOf(b)
		if c[CauseNewResource] != usd(120) || c[CauseRemovedResource] != usd(-150) {
			t.Fatalf("%+v", c)
		}
		for _, d := range b.Drivers {
			if d.Cause == CauseNewResource && d.Since != MustDate("2026-10-20") {
				t.Fatalf("new resource since = %v", d.Since)
			}
		}
	})

	t.Run("usage up at the same unit price", func(t *testing.T) {
		lines := append(spread(baseR, l, usd(30), 30_000_000), spread(periodR, l, usd(62), 62_000_000)...)
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		if c := causesOf(b); c[CauseUsageUp] != b.Change || c[CausePriceChange] != 0 {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("price change splits from usage", func(t *testing.T) {
		// Base: 30 units/day at $1. Period: 31 units/day at $1.50.
		lines := append(spread(baseR, l, usd(30), 30_000_000), spread(periodR, l, 46_500_000, 31_000_000)...)
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		c := causesOf(b)
		// Δq = 961−900 = 61 units at the base price $1 → $61 usage.
		if c[CauseUsageUp] != usd(61) || c[CausePriceChange] != b.Change-usd(61) {
			t.Fatalf("%+v change %d", c, b.Change)
		}
	})

	t.Run("price noise under 1% counts as usage", func(t *testing.T) {
		lines := append(spread(baseR, l, usd(100), 100_000_000), spread(periodR, l, 100_500_000, 100_000_000)...)
		b := ComputeBridge(lines, periodR, baseR, opts)
		if c := causesOf(b); c[CausePriceChange] != 0 || c[CauseUsageUp] != b.Change {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("discounts credits fees refunds tax", func(t *testing.T) {
		lines := append([]CostLine{}, anchor...)
		mk := func(day, rt string, amt Micros) CostLine {
			return CostLine{Day: MustDate(day), Account: "a", Service: "billing", RecordType: rt, NetAmortizedMicros: amt}
		}
		lines = append(lines,
			mk("2026-10-02", RecordFee, usd(1200)),
			mk("2026-10-03", RecordRefund, usd(-75)),
			mk("2026-10-04", RecordTax, usd(30)),
			mk("2026-10-05", RecordCredit, usd(-500)),
			mk("2026-09-05", RecordDiscount, usd(-900)),
			mk("2026-10-05", RecordDiscount, usd(-700)),
		)
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		c := causesOf(b)
		if c[CauseOneOff] != usd(1155) || c[CauseDiscountChange] != usd(-300) {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("a single-day spike is one-off", func(t *testing.T) {
		lines := append(spread(baseR, l, usd(10), 0), spread(periodR, l, usd(10), 0)...)
		for i := range lines {
			if lines[i].Day == MustDate("2026-10-15") {
				lines[i].NetAmortizedMicros = usd(400)
			}
		}
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		c := causesOf(b)
		if c[CauseOneOff] != usd(390) || c[CauseUsageUp] != usd(10) {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("an account with no base cost is one new-account driver", func(t *testing.T) {
		lines := append([]CostLine{}, anchor...)
		for _, svc := range []string{"x", "y", "z"} {
			lines = append(lines, spread(DateRange{MustDate("2026-10-12"), periodR.End}, CostLine{Account: "new", Service: svc, RecordType: RecordUsage}, usd(5), 0)...)
		}
		b := ComputeBridge(lines, periodR, baseR, opts)
		assertSums(t, b)
		var na *Driver
		for i := range b.Drivers {
			if b.Drivers[i].Cause == CauseNewAccount {
				na = &b.Drivers[i]
			}
		}
		if na == nil || na.Amount != usd(300) || na.Lines != 3 || na.Since != MustDate("2026-10-12") {
			t.Fatalf("%+v", na)
		}
	})
}

func TestDriverRollupAndOrder(t *testing.T) {
	var lines []CostLine
	add := func(svc string, base, period Micros) {
		l := CostLine{Account: "a", Service: svc, RecordType: RecordUsage}
		lines = append(lines, spread(baseR, l, base, 0)...)
		lines = append(lines, spread(periodR, l, period, 0)...)
	}
	add("big-up", usd(10), usd(110))  // +3,110
	add("small-up", usd(1), usd(2))   // +32: under $50 → other
	add("down", usd(100), usd(50))    // −1,450
	add("mid-up", usd(10), usd(30))   // +630
	add("steady", usd(100), usd(100)) // +100 (one more day)
	b := ComputeBridge(lines, periodR, baseR, BridgeOptions{})
	assertSums(t, b)
	got := []string{}
	for _, d := range b.Drivers {
		got = append(got, d.Key.Service)
	}
	want := []string{"big-up", "mid-up", "steady", "down"}
	if len(got) != len(want) {
		t.Fatalf("rows %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("increases lead, then decreases: %v", got)
		}
	}
	if b.Other.Lines != 1 || b.Other.Amount != usd(32) {
		t.Fatalf("other %+v", b.Other)
	}

	// A falling total leads with decreases.
	b = ComputeBridge(lines, baseR, periodR, BridgeOptions{})
	assertSums(t, b)
	if b.Drivers[0].Amount >= 0 {
		t.Fatalf("decreases lead when the total falls: %+v", b.Drivers[0])
	}

	// The row cap rolls the smallest rows into other.
	b = ComputeBridge(lines, periodR, baseR, BridgeOptions{MaxDrivers: 2})
	assertSums(t, b)
	if len(b.Drivers) != 2 || b.Other.Lines != 3 {
		t.Fatalf("cap: %d rows, other %d lines", len(b.Drivers), b.Other.Lines)
	}

	// Nothing above the minimum: steady.
	b = ComputeBridge(lines, periodR, baseR, BridgeOptions{MinDriver: usd(100_000)})
	assertSums(t, b)
	if !b.Steady || len(b.Drivers) != 0 {
		t.Fatal("steady")
	}
}

// The split must sum to Δ exactly for any input: random lines with random
// record types, quantities, gaps, spikes, new and removed resources and
// accounts, under random minimums and caps.
func TestDriverSplitSumsToChangeProperty(t *testing.T) {
	records := []string{RecordUsage, RecordUsage, RecordUsage, RecordDiscount, RecordCredit, RecordFee, RecordRefund, RecordTax, ""}
	for seed := uint64(1); seed <= 400; seed++ {
		r := rand.New(rand.NewPCG(seed, seed*7919))
		var lines []CostLine
		nKeys := 1 + r.IntN(25)
		for k := range nKeys {
			l := CostLine{
				Account:    []string{"a", "b", "c", "d"}[r.IntN(4)],
				Service:    []string{"s1", "s2", "s3"}[r.IntN(3)],
				UsageType:  string(rune('A' + k)),
				RecordType: records[r.IntN(len(records))],
				ResourceID: []string{"", "r1", "r2"}[r.IntN(3)],
			}
			for d := baseR.Start; !d.After(periodR.End); d = d.AddDays(1) {
				if r.IntN(5) == 0 {
					continue
				}
				c := l
				c.Day = d
				c.NetAmortizedMicros = r.Int64N(2_000_000_000) - 200_000_000
				if r.IntN(40) == 0 {
					c.NetAmortizedMicros *= 50
				}
				c.QuantityMicros = r.Int64N(5_000_000_000)
				if r.IntN(3) == 0 {
					c.QuantityMicros = 0
				}
				lines = append(lines, c)
			}
		}
		opts := BridgeOptions{MinDriver: r.Int64N(usd(500)), MaxDrivers: r.IntN(15)}
		b := ComputeBridge(lines, periodR, baseR, opts)
		var want Micros
		for _, l := range lines {
			switch {
			case periodR.Contains(l.Day):
				want += l.NetAmortizedMicros
			case baseR.Contains(l.Day):
				want -= l.NetAmortizedMicros
			}
		}
		if b.Change != want {
			t.Fatalf("seed %d: change %d, want %d", seed, b.Change, want)
		}
		sum := b.Other.Amount
		for _, d := range b.Drivers {
			sum += d.Amount
		}
		if sum != b.Change {
			t.Fatalf("seed %d: drivers sum %d != Δ %d", seed, sum, b.Change)
		}
		if b.Up+b.Down != b.Change {
			t.Fatalf("seed %d: up+down %d != Δ %d", seed, b.Up+b.Down, b.Change)
		}
	}
}

// --- increases -------------------------------------------------------------

func seriesLines(start Date, vals []Micros, rt string) []CostLine {
	var out []CostLine
	for i, v := range vals {
		out = append(out, CostLine{Day: start.AddDays(i), Account: "a", Service: "network.nat", UsageType: "nat", RecordType: rt, NetAmortizedMicros: v})
	}
	return out
}

func flatThenStep(before, after Micros, days, stepAt int) []Micros {
	v := make([]Micros, days)
	for i := range v {
		v[i] = before
		if i >= stepAt {
			v[i] = after
		}
	}
	return v
}

func hist(start Date) HistoryFunc { return func(string) Date { return start } }

func TestIncreaseTriggerAndResolve(t *testing.T) {
	start := MustDate("2026-09-01")
	vals := flatThenStep(usd(20), usd(85), 30, 20) // step on day 20 (Sep 21)
	lines := seriesLines(start, vals, RecordUsage)

	if got := DetectIncreases(lines, start.AddDays(21), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("two days up must not trigger: %+v", got)
	}
	got := DetectIncreases(lines, start.AddDays(22), hist(start), DetectOptions{})
	if len(got) != 1 {
		t.Fatalf("three days up must trigger: %+v", got)
	}
	inc := got[0]
	if inc.StartedOn != MustDate("2026-09-21") || inc.DailyBefore != usd(20) || inc.DailyNow != usd(85) ||
		inc.MonthlyImpact != usd(65*30) || !inc.ResolvedOn.IsZero() {
		t.Fatalf("%+v", inc)
	}
	if inc.ID != IncreaseID(inc.Key, inc.StartedOn) {
		t.Fatal("ID must be stable for line and start day")
	}

	// Back within the band for three days resolves it.
	back := append(vals, usd(21), usd(20), usd(22))
	got = DetectIncreases(seriesLines(start, back, RecordUsage), start.AddDays(32), hist(start), DetectOptions{})
	if len(got) != 1 || got[0].ResolvedOn != MustDate("2026-10-03") {
		t.Fatalf("resolve: %+v", got)
	}
}

func TestIncreaseNeverTriggersOnPartialDays(t *testing.T) {
	start := MustDate("2026-09-01")
	vals := flatThenStep(usd(20), usd(900), 20, 17)
	lines := seriesLines(start, vals, RecordUsage)
	// Days 17..19 are partial: the last complete day is day 16.
	if got := DetectIncreases(lines, start.AddDays(16), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("partial days must never trigger: %+v", got)
	}
}

func TestIncreaseNeedsBaselineAndImpact(t *testing.T) {
	start := MustDate("2026-09-01")
	// Only 16 days of history: still learning.
	short := seriesLines(start, flatThenStep(usd(20), usd(85), 16, 13), RecordUsage)
	if got := DetectIncreases(short, start.AddDays(15), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("learning: %+v", got)
	}
	// +$3/day = $90/month: under the $100 minimum.
	small := seriesLines(start, flatThenStep(usd(10), usd(13), 30, 20), RecordUsage)
	if got := DetectIncreases(small, start.AddDays(29), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("under the minimum: %+v", got)
	}
	// Inside the 20% band: no trigger even when the impact is large.
	band := seriesLines(start, flatThenStep(usd(1000), usd(1150), 30, 20), RecordUsage)
	if got := DetectIncreases(band, start.AddDays(29), hist(start), DetectOptions{}); len(got) != 0 {
		t.Fatalf("inside the band: %+v", got)
	}
	// Unknown history: never detect.
	if got := DetectIncreases(band, start.AddDays(29), func(string) Date { return Date{} }, DetectOptions{}); len(got) != 0 {
		t.Fatal("no history")
	}
}

func TestFeesAndCreditsNeverTrigger(t *testing.T) {
	start := MustDate("2026-09-01")
	for _, rt := range []string{RecordFee, RecordCredit, RecordRefund, RecordTax, RecordDiscount} {
		lines := seriesLines(start, flatThenStep(usd(1), usd(900), 30, 20), rt)
		if got := DetectIncreases(lines, start.AddDays(29), hist(start), DetectOptions{}); len(got) != 0 {
			t.Fatalf("%s triggered: %+v", rt, got)
		}
	}
}

func TestIncreaseStatusMachine(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	today := DateOf(now)
	rec := Reconcile(nil, Detected{ID: "i1", MonthlyImpact: usd(1000)}, today, now)
	if rec.Status != StatusOpen {
		t.Fatal("new increases are open")
	}
	if _, err := ApplyStatusChange(rec, StatusChange{Status: StatusExpected, Until: today.AddDays(10)}, "admin", today, now); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("reason required, got %v", err)
	}
	for _, until := range []Date{today, today.AddDays(-1), today.AddDays(MaxExpectedDays + 1), {}} {
		if _, err := ApplyStatusChange(rec, StatusChange{Status: StatusExpected, Reason: "migration", Until: until}, "admin", today, now); !errors.Is(err, ErrUntilInvalid) {
			t.Fatalf("until %v must be rejected, got %v", until, err)
		}
	}
	exp, err := ApplyStatusChange(rec, StatusChange{Status: StatusExpected, Reason: " migration ", Until: today.AddDays(MaxExpectedDays)}, "admin", today, now)
	if err != nil || exp.Status != StatusExpected || exp.Expected.Reason != "migration" || len(exp.History) != 1 {
		t.Fatalf("%+v %v", exp, err)
	}
	if len(rec.History) != 0 {
		t.Fatal("status changes must not mutate the input history")
	}

	// Still there on the end date → reopens by itself.
	end := exp.Expected.Until
	still := Reconcile(&exp, exp.Detected, end.AddDays(-1), now)
	if still.Status != StatusExpected {
		t.Fatal("stays expected before the end date")
	}
	re := Reconcile(&exp, exp.Detected, end, now)
	if re.Status != StatusOpen || re.History[len(re.History)-1].By != "system" {
		t.Fatalf("auto-reopen: %+v", re)
	}

	// Resolution wins over expected and is final.
	d := exp.Detected
	d.ResolvedOn = today
	res := Reconcile(&exp, d, today, now)
	if res.Status != StatusResolved {
		t.Fatal("resolved")
	}
	if _, err := ApplyStatusChange(res, StatusChange{Status: StatusOpen}, "admin", today, now); !errors.Is(err, ErrBadTransition) {
		t.Fatal("resolved increases cannot change")
	}
	if _, err := ApplyStatusChange(rec, StatusChange{Status: StatusOpen}, "admin", today, now); !errors.Is(err, ErrBadTransition) {
		t.Fatal("open → open is not a transition")
	}
	back, err := ApplyStatusChange(exp, StatusChange{Status: StatusOpen}, "admin", today, now)
	if err != nil || back.Status != StatusOpen {
		t.Fatal("expected → open")
	}
}

func TestAlertOnceUnlessImpactDoubles(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	rec := IncreaseRecord{Detected: Detected{MonthlyImpact: usd(1000)}, Status: StatusOpen}
	if !ShouldAlert(rec) {
		t.Fatal("first alert")
	}
	rec = MarkAlerted(rec, now)
	if ShouldAlert(rec) {
		t.Fatal("no re-alert")
	}
	rec.MonthlyImpact = usd(1999)
	if ShouldAlert(rec) {
		t.Fatal("not yet doubled")
	}
	rec.MonthlyImpact = usd(2000)
	if !ShouldAlert(rec) {
		t.Fatal("doubled")
	}
	rec.Status = StatusExpected
	if ShouldAlert(rec) {
		t.Fatal("expected increases do not alert")
	}
}

// --- budgets and forecast -----------------------------------------------------

func TestValidateBudgets(t *testing.T) {
	ok := []Budget{
		{Name: "Total", Scope: ScopeDeployment, Monthly: usd(1)},
		{Name: "Prod", Scope: ScopeAccount, Value: "111", Monthly: usd(1)},
		{Name: "Checkout", Scope: ScopeTeam, Value: "checkout", Monthly: usd(1)},
	}
	if err := ValidateBudgets(ok); err != nil {
		t.Fatal(err)
	}
	bad := [][]Budget{
		{{Name: "x", Scope: "org", Value: "acme", Monthly: usd(1)}},
		{{Name: "x", Scope: ScopeDeployment, Value: "v", Monthly: usd(1)}},
		{{Name: "x", Scope: ScopeTeam, Monthly: usd(1)}},
		{{Name: "x", Scope: ScopeDeployment}},
		{{Scope: ScopeDeployment, Monthly: usd(1)}},
		{{Name: "a", Scope: ScopeDeployment, Monthly: usd(1)}, {Name: "b", Scope: ScopeDeployment, Monthly: usd(2)}},
	}
	for _, bs := range bad {
		if err := ValidateBudgets(bs); !errors.Is(err, ErrBadBudget) {
			t.Errorf("%+v should be rejected", bs)
		}
	}
}

func TestBudgetStatus(t *testing.T) {
	est := Forecast{Source: ForecastEstimate}
	cases := []struct {
		actual, forecast Micros
		src              string
		want             string
	}{
		{usd(50), usd(80), ForecastProvider, BudgetOnTrack},
		{usd(50), usd(90), ForecastProvider, BudgetOnTrack},
		{usd(50), usd(91), ForecastProvider, BudgetAtRisk},
		{usd(101), usd(101), ForecastProvider, BudgetOver},
		{usd(50), usd(200), ForecastNone, BudgetOnTrack},
	}
	for _, c := range cases {
		est.Amount, est.Source = c.forecast, c.src
		if got := BudgetStatus(usd(100), c.actual, est); got != c.want {
			t.Errorf("actual %d forecast %d (%s): %s, want %s", c.actual, c.forecast, c.src, got, c.want)
		}
	}
}

func TestForecastFallback(t *testing.T) {
	last := MustDate("2026-11-03")
	daily := func(Date) Micros { return usd(100) }
	pf := &ProviderForecast{Month: "2026-11", Mean: usd(3100), Low: usd(2900), High: usd(3300)}
	if f := MakeForecast(pf, last, MustDate("2026-08-01"), daily, false); f.Source != ForecastProvider || f.Amount != usd(3100) || f.High != usd(3300) {
		t.Fatalf("provider: %+v", f)
	}
	// Wrong month or missing → estimate: 3 days actual + 27 × the 7-day average.
	f := MakeForecast(&ProviderForecast{Month: "2026-10"}, last, MustDate("2026-08-01"), daily, true)
	if f.Source != ForecastEstimate || f.Amount != usd(3000) || f.Reason != "forecast_skipped" || f.Month != "2026-11" {
		t.Fatalf("estimate: %+v", f)
	}
	// On the last day of a month the forecast is for the next month.
	f = MakeForecast(nil, MustDate("2026-10-31"), MustDate("2026-08-01"), daily, false)
	if f.Month != "2026-11" || f.Amount != usd(3000) {
		t.Fatalf("next month: %+v", f)
	}
	// Under 7 days of history: no forecast, named.
	f = MakeForecast(nil, last, MustDate("2026-10-29"), daily, false)
	if f.Source != ForecastNone || f.Reason != "not_enough_history" || f.Amount != 0 {
		t.Fatalf("none: %+v", f)
	}
}

// --- savings -------------------------------------------------------------------

func TestSavingsRules(t *testing.T) {
	src, _ := natStory()
	in := src.savings
	in.Recommendations = []Recommendation{{ResourceID: "db-1", Name: "reporting-db", Account: acctData, Current: "db.r6g.2xlarge", Target: "db.r6g.xlarge", Monthly: usd(140)}}
	in.Buckets = append(in.Buckets,
		Bucket{Name: "has-rule", Account: acctData, HasLifecycle: true, StandardBytes: 9 << 40, EstimatedMonthly: usd(900)},
		Bucket{Name: "small", Account: acctData, StandardBytes: 1 << 30, EstimatedMonthly: usd(900)})
	in.NetworkPaths = append(in.NetworkPaths, NetworkPath{Network: "vpc-done", Account: acctProd,
		Processing: map[string]Micros{TargetObjectStorage: usd(500)}, HasEndpoint: map[string]bool{TargetObjectStorage: true}})
	got := EvaluateSavings(in, SavingsOptions{})
	byKind := map[string]Saving{}
	for _, s := range got {
		if _, dup := byKind[s.Kind]; dup {
			t.Fatalf("one item per kind expected in this fixture: %+v", got)
		}
		byKind[s.Kind] = s
	}
	if s := byKind[SavingNetworkPath]; s.Monthly != usd(1520) || s.Title != "Add private endpoints for ECR and S3 in vpc-prod" || s.Effort != EffortHours || s.Risk != RiskLow {
		t.Fatalf("network path: %+v", s)
	}
	if s := byKind[SavingIdle]; s.Monthly != usd(212) || s.Title != "Remove 4 unattached disk volumes" || len(s.ResourceIDs) != 4 {
		t.Fatalf("idle: %+v", s)
	}
	if s := byKind[SavingCommitment]; s.Monthly != usd(610) {
		t.Fatalf("commitment: %+v", s)
	}
	if s := byKind[SavingRightsize]; s.Risk != RiskNeedsTesting || s.Title != "Resize reporting-db to db.r6g.xlarge" {
		t.Fatalf("rightsize: %+v", s)
	}
	if s := byKind[SavingStorageClass]; s.ResourceIDs[0] != "data-lake-raw" {
		t.Fatalf("storage: %+v", s)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Monthly > got[i-1].Monthly {
			t.Fatal("largest saving first")
		}
	}
	// Coverage at target: no commitment item.
	in.Coverage.CoveredBP = 8_000
	for _, s := range EvaluateSavings(in, SavingsOptions{}) {
		if s.Kind == SavingCommitment {
			t.Fatal("coverage at target must not suggest a commitment")
		}
	}
}

// --- request planner -----------------------------------------------------------

func payerCalls(payer string) []Call {
	return []Call{
		{Name: "cost_by_account", Group: payer, Priority: 1, Requests: 1, Required: true},
		{Name: "cost_by_usage", Group: payer, Priority: 2, Requests: 1, Required: true},
		{Name: "cost_by_team", Group: payer, Priority: 3, Requests: 1, Required: true},
		{Name: "cost_by_record", Group: payer, Priority: 4, Requests: 1, Required: true},
		{Name: "forecast", Group: payer, Priority: 5, Requests: 1, DropOrder: 3},
		{Name: "discount_coverage", Group: payer, Priority: 6, Requests: 1, DropOrder: 2},
		{Name: "resource_detail", Group: payer, Priority: 7, Requests: 1, DropOrder: 1},
	}
}

func TestPlanFitsAndDropsInFixedOrder(t *testing.T) {
	p := BuildPlan(payerCalls("p1"), 20)
	if len(p.Run) != 7 || p.Estimate != 7 || len(p.Skipped) != 0 {
		t.Fatalf("one payer fits: %+v", p)
	}
	// Three payers = 21 requests: the cap drops one resource call; which
	// one is the lowest-priority payer's.
	var calls []Call
	for i, g := range []string{"p1", "p2", "p3"} {
		for _, c := range payerCalls(g) {
			c.Priority += i * 10
			calls = append(calls, c)
		}
	}
	p = BuildPlan(calls, 20)
	if p.Estimate != 20 || len(p.Skipped) != 1 || p.Skipped[0].Call.Name != "resource_detail" || p.Skipped[0].Call.Group != "p3" {
		t.Fatalf("drop 7 first: %+v", p.Skipped)
	}
	// Tighter: every resource call, then every coverage call, then forecasts.
	p = BuildPlan(calls, 13)
	var names []string
	for _, s := range p.Skipped {
		names = append(names, s.Call.Name)
	}
	want := []string{"resource_detail", "resource_detail", "resource_detail", "discount_coverage", "discount_coverage", "discount_coverage", "forecast", "forecast"}
	if len(names) != len(want) {
		t.Fatalf("skipped %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("drop order 7 → 6 → 5: %v", names)
		}
	}
	// Required calls never split a day: a whole group drops.
	p = BuildPlan(calls, 9)
	for _, c := range p.Run {
		if c.Group == "p3" {
			t.Fatalf("p3 cannot fit and must drop whole: %+v", p.Run)
		}
	}
	if p.Estimate > 9 {
		t.Fatal("over budget")
	}
	if p = BuildPlan(payerCalls("p1"), 3); len(p.Run) != 0 {
		t.Fatalf("nothing fits: %+v", p.Run)
	}
}

func TestMeterCapAndPagesCounted(t *testing.T) {
	ctx := t.Context()
	m := NewMemoryMeter(5, 2)
	if err := m.Take(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := m.Take(ctx, 1); !errors.Is(err, ErrCapReached) {
		t.Fatal("cap")
	}
	if m.Used(ctx) != 5 || m.Cap() != 5 {
		t.Fatal("refused requests are not counted")
	}
}

func TestExecuteCommitsWholeDaysOnly(t *testing.T) {
	ctx := t.Context()
	calls := append(payerCalls("p1"), payerCalls("p2")...)
	for i := range calls[7:] {
		calls[7+i].Priority += 10
	}
	p := BuildPlan(calls, 20)
	// Each call needs two pages; p2's usage call fails.
	pages := 0
	run := func(ctx context.Context, c Call, m Meter) (string, error) {
		for range 2 {
			if err := m.Take(ctx, 1); err != nil {
				return "", err
			}
			pages++
		}
		if c.Group == "p2" && c.Name == "cost_by_usage" {
			return "throttled", errors.New("throttled")
		}
		return "", nil
	}
	m := NewMemoryMeter(20, 0)
	out := Execute(ctx, p, m, run)
	if !out.Commit["p1"] || out.Commit["p2"] {
		t.Fatalf("commit %+v", out.Commit)
	}
	if m.Used(ctx) != pages {
		t.Fatalf("every page goes through the meter: used %d pages %d", m.Used(ctx), pages)
	}
	var skippedGroup, capped int
	for _, r := range out.Results {
		if r.Skipped == SkipGroupFailed {
			skippedGroup++
		}
		if r.ErrorCode == SkipCapMidRun || r.Skipped == SkipCapMidRun {
			capped++
		}
	}
	if skippedGroup != 2 {
		t.Fatalf("a failed required call skips the rest of its day: %d", skippedGroup)
	}
	if capped == 0 {
		t.Fatal("two pages per call must hit the 20-request cap mid-run")
	}
	if len(SkippedCalls(p, out)) == 0 {
		t.Fatal("skipped calls are named")
	}
}

type ownerShare struct {
	team string
	bp   int64
}

type ownerShareCase struct {
	name          string
	shares        []ownerShare
	estimated     bool
	wantOwner     string
	wantRule      string
	wantEstimated bool
}

// TestIncreaseAndDriverOwnerIsTheLargestTeam: one NAT line whose daily cost
// steps up on day 20 is split across team tag values. The owner of the
// increase and of its driver is the value with the most cost; untagged cost
// competes as Unallocated.
func TestIncreaseAndDriverOwnerIsTheLargestTeam(t *testing.T) {
	fx := struct {
		Start         Date
		Days, Step    int
		Before, After Micros
		Cases         []ownerShareCase
	}{
		Start: MustDate("2026-09-01"), Days: 40, Step: 20, Before: 20_000_000, After: 85_000_000,
		Cases: []ownerShareCase{
			{name: "largest tag wins over the last tagged line", shares: []ownerShare{{"alpha", 9000}, {"zeta", 1000}},
				wantOwner: "alpha", wantRule: "tag"},
			{name: "mostly untagged is unallocated", shares: []ownerShare{{"", 9900}, {"team", 100}},
				wantOwner: "Unallocated", wantRule: "unallocated"},
			{name: "an estimated share is never a fact", estimated: true, shares: []ownerShare{{"alpha", 6000}, {"zeta", 4000}},
				wantOwner: "alpha", wantRule: "tag", wantEstimated: true},
		},
	}
	owners := Ownership{Tag: TagStatus{Key: "team", Active: true, Allowlisted: true}}
	for _, c := range fx.Cases {
		t.Run(c.name, func(t *testing.T) {
			var lines []CostLine
			for i := range fx.Days {
				total := fx.Before
				if i >= fx.Step {
					total = fx.After
				}
				for _, sh := range c.shares {
					l := line(fx.Start.AddDays(i), "a", "network.nat", "nat", RecordUsage, sh.team, mulDiv(total, sh.bp, 10_000), 0)
					l.EstimatedTeam = c.estimated
					lines = append(lines, l)
				}
			}
			last := fx.Start.AddDays(fx.Days - 1)
			det := DetectIncreases(lines, last, hist(fx.Start), DetectOptions{})
			if len(det) != 1 {
				t.Fatalf("detected %d increases", len(det))
			}
			o := owners.ResolveShare("a", det[0].Team, "", det[0].TeamEstimated, narrative.Summary)
			if o.Name != c.wantOwner || o.Rule != c.wantRule || o.Estimated != c.wantEstimated {
				t.Fatalf("increase owner %+v, want %s/%s estimated %v", o, c.wantOwner, c.wantRule, c.wantEstimated)
			}
			period := DateRange{fx.Start.AddDays(fx.Step), last}
			base := DateRange{fx.Start, fx.Start.AddDays(fx.Step - 1)}
			b := ComputeBridge(lines, period, base, BridgeOptions{MinDriver: 1})
			if len(b.Drivers) == 0 {
				t.Fatal("no driver")
			}
			d := b.Drivers[0]
			o = owners.ResolveShare("a", d.Team, "", d.TeamEstimated, narrative.Summary)
			if o.Name != c.wantOwner || o.Estimated != c.wantEstimated {
				t.Fatalf("driver owner %+v, want %s", o, c.wantOwner)
			}
			if c.wantEstimated && !strings.Contains(o.RuleText, narrative.EstimatedMark) {
				t.Fatalf("estimated owner rule text %q", o.RuleText)
			}
		})
	}
}
