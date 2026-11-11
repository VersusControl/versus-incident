package cost

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// canonical renders a list of structs as sorted JSON strings, so map order
// and slice order (both come from Go maps in the savings rules) do not make
// two equal answers look different.
func canonical(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = string(item)
	}
	sort.Strings(out)
	return out
}

// sameSavings compares two savings answers field for field, allowing for
// the map-derived order of idle and network entries.
func sameSavings(t *testing.T, got, want finops.SavingsInputs, wantIdle bool) {
	t.Helper()
	if got.Coverage == nil || want.Coverage == nil ||
		got.Coverage.CoveredBP != want.Coverage.CoveredBP ||
		got.Coverage.Suggestion != want.Coverage.Suggestion ||
		got.Coverage.EstimatedMonthly != want.Coverage.EstimatedMonthly ||
		(got.Coverage.Guide == nil) != (want.Coverage.Guide == nil) ||
		!reflect.DeepEqual(got.Coverage.Guide, want.Coverage.Guide) {
		t.Fatalf("coverage %+v, want %+v", got.Coverage, want.Coverage)
	}
	if !reflect.DeepEqual(got.Recommendations, want.Recommendations) {
		t.Fatalf("recommendations %+v, want %+v", got.Recommendations, want.Recommendations)
	}
	if wantIdle && len(want.Idle) == 0 {
		t.Fatal("the scenario bills an unused address; the case proves nothing")
	}
	if !reflect.DeepEqual(canonical(t, got.Idle), canonical(t, want.Idle)) {
		t.Fatalf("idle %+v, want %+v", got.Idle, want.Idle)
	}
	if !reflect.DeepEqual(canonical(t, got.NetworkPaths), canonical(t, want.NetworkPaths)) {
		t.Fatalf("paths %+v, want %+v", got.NetworkPaths, want.NetworkPaths)
	}
	if !reflect.DeepEqual(got.Gaps, want.Gaps) {
		t.Fatalf("gaps %+v, want %+v", got.Gaps, want.Gaps)
	}
}

// The store-summed savings facts must equal the line-scan fallback, both
// when the source counts the billed days itself and when the core supplies
// the count it already holds.
func TestSavingsInputsFromTotalsMatchTheLineScan(t *testing.T) {
	h, _, _ := natSpike(t, nil)
	checkSavingsTotals(t, h, true)

	org, _ := orgHarness(t)
	if _, err := org.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	backfillOrganization(t, org)
	checkSavingsTotals(t, org, false)
}

func checkSavingsTotals(t *testing.T, h *harness, wantIdle bool) {
	t.Helper()
	src := h.adapter.Source()
	end := completeThrough(h.clock.Now(), DaySettle)
	window := finops.DateRange{Start: end.AddDays(-29), End: end}

	want, err := src.SavingsInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.SavingsInputsFromTotals(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	sameSavings(t, got, want, wantIdle)

	// The day count the core supplies is the set the store derives.
	days := billedDaysFromStore(t, h, src, window)
	known, err := src.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window, BilledDays: days, DaysKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	sameSavings(t, known, want, wantIdle)
	unknown, err := src.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window})
	if err != nil {
		t.Fatal(err)
	}
	sameSavings(t, unknown, want, wantIdle)

	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	// A supplied count skips the payer-by-day query: two service queries
	// instead of three, and no line read either way.
	rec := &readCounter{Provider: h.provider, FinOpsStore: tables}
	fresh := mustSource(t, rec, h.adapter.cache.scope, h.opts)
	if _, err := fresh.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window, BilledDays: days, DaysKnown: true}); err != nil {
		t.Fatal(err)
	}
	if totals, lines := rec.counts(); totals != 2 || len(lines) != 0 {
		t.Fatalf("supplied days read %d totals and %d line sets, want 2 and 0", totals, len(lines))
	}
	rec = &readCounter{Provider: h.provider, FinOpsStore: tables}
	fresh = mustSource(t, rec, h.adapter.cache.scope, h.opts)
	if _, err := fresh.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window}); err != nil {
		t.Fatal(err)
	}
	if totals, lines := rec.counts(); totals != 3 || len(lines) != 0 {
		t.Fatalf("derived days read %d totals and %d line sets, want 3 and 0", totals, len(lines))
	}
}

// billedDaysFromStore is the day set the source derives: distinct days in
// the window with a row from a configured payer.
func billedDaysFromStore(t *testing.T, h *harness, src *Source, window finops.DateRange) int {
	t.Helper()
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	rows, err := tables.FinOpsCostTotals(ctx, h.adapter.cache.scope, storage.FinOpsCostQuery{
		From: window.Start.String(), Through: window.End.String(), ResourceID: noResource,
	}, storage.FinOpsByPayer, storage.FinOpsByDay)
	if err != nil {
		t.Fatal(err)
	}
	configured := map[string]bool{}
	for _, p := range src.payers() {
		configured[p.AccountID] = true
	}
	days := map[string]bool{}
	for _, row := range rows {
		if configured[row.Payer] {
			days[row.Day] = true
		}
	}
	return len(days)
}

// One store query answers a totals read whatever the payer count.
func TestTotalsGroupsMakesOneStoreQuery(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{
			{AccountID: "111111111111", Credentials: credsFor("AKIDONE")},
			{AccountID: "222222222222", Credentials: credsFor("AKIDTWO")},
			{AccountID: "333333333333", Credentials: credsFor("AKIDTHREE")},
		}
	})
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	rec := &readCounter{Provider: h.provider, FinOpsStore: tables}
	src := mustSource(t, rec, h.adapter.cache.scope, h.opts)
	if _, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{}, ByDay, ByAccount); err != nil {
		t.Fatal(err)
	}
	if totals, _ := rec.counts(); totals != 1 {
		t.Fatalf("%d store queries for three payers, want 1", totals)
	}
}

// A team value cached while the tag was allowlisted must not be served after
// the allowlist is narrowed, even though nothing was rewritten. The totals
// are unchanged; only the team label disappears.
func TestSavingsInputsHideCachedTeamsAfterTheAllowlistNarrows(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	st, err := h.adapter.cache.status()
	if err != nil {
		t.Fatal(err)
	}
	payerState := st.Payers["111111111111"]
	payerState.Recommendations = append(payerState.Recommendations, finops.Recommendation{
		ResourceID: "i-seeded", Account: "111111111111", Region: "us-east-1", Team: "payments",
		Current: "m6i.large", Target: "m6i.medium", Monthly: 61_320_000, Source: "hub",
	})
	payerState.IdleFound = append(payerState.IdleFound, finops.IdleResource{
		Kind: finops.IdleUnattachedVolume, ID: "vol-seeded", Account: "111111111111", Region: "us-east-1",
		Team: "payments", Monthly: 8_000_000,
	})
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	window := finops.DateRange{Start: completeThrough(h.clock.Now(), DaySettle).AddDays(-29), End: completeThrough(h.clock.Now(), DaySettle)}

	src := h.adapter.Source()
	open, err := src.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(open)
	if !strings.Contains(string(raw), `"team":"payments"`) {
		t.Fatalf("allowlisted read dropped the cached team: %s", raw)
	}
	totalsOpen, err := src.Totals(ctx, sc.Range, finops.TotalsQuery{}, finops.ByDay, finops.ByAccount)
	if err != nil {
		t.Fatal(err)
	}

	// Narrow the allowlist; the cache is not rewritten.
	h.opts.TagAllowlisted = false
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	src = h.adapter.Source()
	for name, read := range map[string]func() (finops.SavingsInputs, error){
		"totals": func() (finops.SavingsInputs, error) {
			return src.SavingsInputsFromFacts(ctx, finops.SavingsFactsQuery{Range: window})
		},
		"lines": func() (finops.SavingsInputs, error) { return src.SavingsInputs(ctx) },
	} {
		closed, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if raw, _ := json.Marshal(closed); strings.Contains(string(raw), "payments") {
			t.Fatalf("%s: raw team served after the allowlist narrowed: %s", name, raw)
		}
		for _, r := range closed.Recommendations {
			if r.Team != "" {
				t.Fatalf("%s: recommendation team %q", name, r.Team)
			}
		}
		for _, idle := range closed.Idle {
			if idle.Team != "" {
				t.Fatalf("%s: idle team %q", name, idle.Team)
			}
		}
		for _, p := range closed.NetworkPaths {
			if p.Team != "" {
				t.Fatalf("%s: network path team %q", name, p.Team)
			}
		}
	}

	// Totals do not change; only the team grouping is hidden.
	totalsClosed, err := src.Totals(ctx, sc.Range, finops.TotalsQuery{}, finops.ByDay, finops.ByAccount)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(totalsOpen, totalsClosed) {
		t.Fatalf("totals changed with the allowlist: %+v then %+v", totalsOpen, totalsClosed)
	}
	byTeam, err := src.TotalsGroups(ctx, sc.Range, TotalsQuery{}, ByTeam)
	if err != nil {
		t.Fatal(err)
	}
	if len(byTeam) != 1 || byTeam[0].Team != "" {
		t.Fatalf("team totals leaked a value: %+v", byTeam)
	}
}

// The hub's Savings Plans summary is AWS text with the account ID in it;
// the suggestion keeps only the hourly commitment and the term, in Versus
// words, and is empty when the amount cannot be read.
func TestCommitmentSuggestionDropsTheProviderText(t *testing.T) {
	raw := "$0.626/hour with three years term (AllUpfront) in AWS account 123456789012 (Linked account level)"
	if got, want := commitmentSuggestion("ComputeSavingsPlans", raw), "a 3-year compute commitment of $0.626/h"; got != want {
		t.Fatalf("suggestion = %q, want %q", got, want)
	}
	if got := commitmentSuggestion("ComputeSavingsPlans", "Purchase a plan in AWS account 123456789012"); got != "" {
		t.Fatalf("no hourly amount: suggestion = %q, want empty", got)
	}
	if got, want := commitmentSuggestion("Ec2InstanceSavingsPlans", "$2/hour with one year term"), "a 1-year instance family commitment of $2/h"; got != want {
		t.Fatalf("suggestion = %q, want %q", got, want)
	}
}

// A provider resource ID may be an ARN carrying the account ID; titles and
// IDs keep only the resource's own part.
func TestResourcePartDropsTheARNPrefix(t *testing.T) {
	cases := map[string]string{
		"arn:aws:ec2:us-east-1:111122223333:instance/i-0abc":       "i-0abc",
		"arn:aws:lambda:us-east-1:111122223333:function:checkout":  "checkout",
		"arn:aws:rds:us-east-1:111122223333:db:reporting-db":       "reporting-db",
		"arn:aws:ecs:us-east-1:111122223333:service/prod/checkout": "checkout",
		"arn:aws-cn:ec2:cn-north-1:111122223333:volume/vol-9":      "vol-9",
		"i-0abc": "i-0abc",
		"vol-9":  "vol-9",
	}
	for in, want := range cases {
		if got := resourcePart(in); got != want || strings.Contains(got, "111122223333") {
			t.Errorf("resourcePart(%q) = %q, want %q", in, got, want)
		}
	}
}
