package narrative

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "write the rendered sentences and a regenerated expectation table to testdata/finops/narrative for local review")

// sentenceSnapshotDir receives the -update output. It is the gitignored local
// test-data folder: no test reads it.
var sentenceSnapshotDir = filepath.Join("..", "..", "..", "testdata", "finops", "narrative")

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

const usd = 1_000_000

var nat = DriverPhrase{
	Cause: CauseUsageUp, Amount: 1_980 * usd, ServiceKey: "network.nat", ServiceName: "EC2 - Other",
	UsageType: "USE1-NatGateway-Bytes", Resource: "nat-1a", AccountName: "Production",
	Owner: "checkout", Since: day(time.October, 9),
}

func estimatedOwner(p DriverPhrase) *DriverPhrase {
	p.OwnerEstimated = true
	return &p
}

// goldenCases renders every template in one depth. Each case is a label and
// the rendered text; wantSentences pins both.
func goldenCases(d Depth) [][2]string {
	answers := []struct {
		name string
		a    Answer
	}{
		{"answer/over-budget-closed", Answer{PeriodLabel: "October", Total: 43_520 * usd, HasBase: true, BaseLabel: "September",
			Change: 3_640 * usd, ChangeBP: 910, HasBudget: true, Budget: 42_000 * usd, Top: &nat, TopShareBP: 5_400}},
		{"answer/under-budget-closed", Answer{PeriodLabel: "October", Total: 40_100 * usd, HasBase: true, BaseLabel: "September",
			Change: 200 * usd, ChangeBP: 50, HasBudget: true, Budget: 42_000 * usd}},
		{"answer/open-forecast-budget", Answer{PeriodLabel: "November", Open: true, Total: 4_120 * usd, HasBase: true, BaseLabel: "October",
			Change: 300 * usd, ChangeBP: 780, HasBudget: true, Budget: 42_000 * usd, HasForecast: true, Forecast: 44_900 * usd,
			ForecastIsEst: true, Top: &nat, TopShareBP: 3_000}},
		{"answer/no-budget-up", Answer{PeriodLabel: "October", Total: 43_520 * usd, HasBase: true, BaseLabel: "September",
			Change: 3_640 * usd, ChangeBP: 910, Top: &nat, TopShareBP: 5_400}},
		{"answer/down", Answer{PeriodLabel: "October", Total: 37_390 * usd, HasBase: true, BaseLabel: "September",
			Change: -2_310 * usd, ChangeBP: -580, Top: &DriverPhrase{Cause: CauseRemovedResource, Amount: -1_900 * usd,
				ServiceKey: "database.relational", AccountName: "Staging", Since: day(time.October, 3)}, TopShareBP: 8_200}},
		{"answer/about-same", Answer{PeriodLabel: "October", Total: 40_040 * usd, HasBase: true, BaseLabel: "September",
			Change: 160 * usd, ChangeBP: 40, Top: &nat, TopShareBP: 10_000}},
		{"answer/estimated-owner", Answer{PeriodLabel: "October", Total: 43_520 * usd, HasBase: true, BaseLabel: "September",
			Change: 3_640 * usd, ChangeBP: 910, Top: estimatedOwner(nat), TopShareBP: 5_400}},
		{"answer/no-base", Answer{PeriodLabel: "November", Open: true, Total: 1_234_560_000}},
		{"answer/partial-accounts", Answer{PeriodLabel: "October", Total: 30_000 * usd, HasBase: true, BaseLabel: "September",
			Change: 1_000 * usd, ChangeBP: 345, AccountsReadable: 2, AccountsTotal: 3}},
		{"answer/euro", Answer{Currency: "EUR", PeriodLabel: "October", Total: 9_999_990_000}},
		{"answer/other-currency", Answer{Currency: "JPY", PeriodLabel: "October", Total: 5_000_000 * usd}},
	}
	drivers := []struct {
		name string
		p    DriverPhrase
	}{
		{"driver/usage-up", nat},
		{"driver/usage-down", DriverPhrase{Cause: CauseUsageDown, Amount: -420 * usd, ServiceKey: "observability.logs", ServiceName: "AmazonCloudWatch", UsageType: "USE1-DataProcessing-Bytes"}},
		{"driver/new", DriverPhrase{Cause: CauseNewResource, Amount: 1_210 * usd, ServiceKey: "database.relational", ServiceName: "Amazon Relational Database Service",
			UsageType: "USE1-InstanceUsage:db.r6g.xlarge", Resource: "analytics-replica", Since: day(time.October, 20)}},
		{"driver/removed", DriverPhrase{Cause: CauseRemovedResource, Amount: -300 * usd, ServiceKey: "compute.vm", Resource: "batch-worker-3"}},
		{"driver/price-up", DriverPhrase{Cause: CausePriceChange, Amount: 90 * usd, ServiceKey: "storage.object", UsageType: "TimedStorage-ByteHrs"}},
		{"driver/price-down", DriverPhrase{Cause: CausePriceChange, Amount: -60 * usd, ServiceKey: "storage.object"}},
		{"driver/discount-less", DriverPhrase{Cause: CauseDiscountChange, RecordType: "discount", Amount: 640 * usd, ServiceKey: "commitment.savings"}},
		{"driver/discount-more", DriverPhrase{Cause: CauseDiscountChange, RecordType: "discount", Amount: -640 * usd, ServiceKey: "commitment.savings"}},
		{"driver/credits", DriverPhrase{Cause: CauseDiscountChange, RecordType: "credit", Amount: -500 * usd, ServiceKey: "billing.credit"}},
		{"driver/one-off-fee", DriverPhrase{Cause: CauseOneOff, RecordType: "fee", Amount: 1_200 * usd, ServiceKey: "billing.support", ItemLabel: "annual support fee"}},
		{"driver/refund", DriverPhrase{Cause: CauseOneOff, RecordType: "refund", Amount: -75 * usd, ServiceKey: "compute.vm"}},
		{"driver/new-account", DriverPhrase{Cause: CauseNewAccount, Amount: 2_000 * usd, AccountName: "Sandbox 7", Since: day(time.October, 12)}},
		{"driver/other", DriverPhrase{Cause: CauseOther, Amount: 37 * usd, OffsetsCount: 14}},
		{"driver/unknown-service", DriverPhrase{Cause: CauseUsageUp, Amount: 55 * usd, ServiceKey: "aws.braket", ServiceName: "Amazon Braket"}},
	}
	increases := []struct {
		name string
		in   Increase
	}{
		{"increase/found-tag-saving", Increase{ServiceKey: "network.nat", ServiceName: "EC2 - Other", UsageType: "USE1-NatGateway-Bytes",
			Resource: "nat-1a", AccountName: "Production", AccountID: "111122223333", Amount: 1_980 * usd, DailyBefore: 21 * usd,
			DailyNow: 85 * usd, StartedOn: day(time.October, 9), SuspectState: "found", SuspectSummary: "the checkout-api release",
			SuspectActor: "github-actions-prod", Owner: "checkout", OwnerRule: "tag", TagKey: "team",
			SavingAction: "adding VPC endpoints", SavingMonthly: 1_520 * usd}},
		{"increase/none-found-account-owner", Increase{ServiceKey: "database.relational", AccountName: "Data", Amount: 640 * usd,
			PeriodWord: "in October", DailyBefore: 10 * usd, DailyNow: 31 * usd, StartedOn: day(time.October, 20),
			SuspectState: "none_found", Owner: "data platform", OwnerRule: "account", TagKey: "team"}},
		{"increase/estimated-tag", Increase{ServiceKey: "compute.vm", AccountName: "Production", Amount: 300 * usd,
			DailyBefore: 10 * usd, DailyNow: 20 * usd, StartedOn: day(time.October, 5), SuspectState: "none_found",
			Owner: "checkout", OwnerRule: "tag", OwnerEstimated: true, TagKey: "team"}},
		{"increase/estimated-unallocated", Increase{ServiceKey: "compute.vm", AccountName: "Production", Amount: 300 * usd,
			DailyBefore: 10 * usd, DailyNow: 20 * usd, StartedOn: day(time.October, 5), SuspectState: "none_found",
			OwnerRule: "unallocated", OwnerEstimated: true, TagKey: "team"}},
		{"increase/tied-suspects", Increase{ServiceKey: "network.nat", AccountName: "Production", Amount: 640 * usd,
			DailyBefore: 20 * usd, DailyNow: 85 * usd, StartedOn: day(time.October, 20), SuspectState: "found",
			SuspectSummary: "ECS service inventory-svc updated", SuspectsTied: 2, Owner: "checkout", OwnerRule: "tag", TagKey: "team"}},
		{"increase/history-incomplete", Increase{ServiceKey: "network.nat", AccountName: "Production", Amount: 640 * usd,
			DailyBefore: 20 * usd, DailyNow: 85 * usd, StartedOn: day(time.October, 20), SuspectState: "history_incomplete",
			Owner: "checkout", OwnerRule: "tag", TagKey: "team"}},
		{"increase/pending-unallocated", Increase{ServiceKey: "observability.logs", Amount: 180_500_000, DailyBefore: 3 * usd,
			DailyNow: 9 * usd, StartedOn: day(time.October, 2), SuspectState: "collectors_pending", OwnerRule: "unallocated", TagKey: "team"}},
	}

	var out [][2]string
	for _, c := range answers {
		out = append(out, [2]string{c.name, AnswerLine(c.a, d)})
	}
	for _, c := range drivers {
		out = append(out, [2]string{c.name, DriverLine(c.p, d)})
	}
	for _, c := range increases {
		out = append(out, [2]string{c.name, IncreaseSentence(c.in, d)})
	}
	out = append(out,
		[2]string{"saving/endpoints", SavingLine(Saving{Title: "Add VPC endpoints for S3 and ECR in vpc-prod", Monthly: 1_520 * usd, Effort: "hours", Risk: "low"}, d)},
		[2]string{"saving/idle", SavingLine(Saving{Title: "Delete 4 unattached disk volumes", Monthly: 212 * usd, Effort: "minutes", Risk: "none"}, d)},
		[2]string{"saving/rightsize", SavingLine(Saving{Title: "Resize reporting-db to db.r6g.xlarge", Monthly: 140 * usd, Effort: "days", Risk: "needs_testing"}, d)},
		[2]string{"saving/up-to", SavingLine(Saving{Title: "Add private endpoints for S3 and ECR in vpc-prod", Monthly: 2_450 * usd, Effort: "hours", Risk: "low", UpTo: true}, d)},
		[2]string{"saving/commitment", SavingLine(Saving{Title: "Raise discount coverage from 71% toward 80%", Monthly: 610 * usd, Effort: "hours", Risk: "low", BeforeOtherSavings: true}, d)},
		[2]string{"copy/summary", CopySummary(AnswerLine(answers[0].a, d),
			[]string{DriverLine(nat, d), DriverLine(drivers[2].p, d), DriverLine(drivers[9].p, d), "ignored fourth"}, 2_022*usd, 3, "")},
		[2]string{"copy/no-savings", CopySummary("October cost $10.", nil, 0, 0, "")},
		[2]string{"note/accounts", AccountsNote(2, 3)},
		[2]string{"note/accounts-one", AccountsNote(0, 1)},
		[2]string{"note/accounts-count", AccountsCount(3)},
		[2]string{"note/resource-off", ResourceDataOffNote("AWS")},
		[2]string{"note/skipped", SkippedNote(20, []string{"forecast", "discount_coverage"})},
		[2]string{"note/learning", LearningNote(14)},
		[2]string{"note/suspect-none", SuspectNote("none_found")},
		[2]string{"note/suspect-pending", SuspectNote("collectors_pending")},
		[2]string{"note/bill-open", BillOpenNote(time.September, "AWS", 5)},
		[2]string{"note/forecast-estimate", ForecastNote("estimate", 7)},
		[2]string{"note/forecast-none", ForecastNote("none", 7)},
		[2]string{"note/no-base", NoBaseNote(time.November)},
		[2]string{"note/steady", SteadyNote(50*usd, "")},
		[2]string{"note/offset", OffsetNote(2_100*usd, -2_060*usd, "")},
		[2]string{"title/increase", IncreaseTitle(1_980*usd, "", "in October")},
		[2]string{"owner/tag", OwnerRuleText("tag", "team", "checkout", "nat-1a", d)},
		[2]string{"owner/account", OwnerRuleText("account", "team", "data platform", "", d)},
		[2]string{"owner/unallocated", OwnerRuleText("unallocated", "team", "", "", d)},
		[2]string{"budget/words", BudgetStatusWord("on_track") + " | " + BudgetStatusWord("at_risk") + " | " + BudgetStatusWord("over_budget")},
		[2]string{"change/words", ChangeWord(DirectionOf(201)) + " | " + ChangeWord(DirectionOf(-201)) + " | " + ChangeWord(DirectionOf(200))},
	)
	return out
}

func render(cases [][2]string) string {
	var b strings.Builder
	for _, c := range cases {
		b.WriteString("== " + c[0] + "\n" + c[1] + "\n")
	}
	return b.String()
}

// TestGoldenSentencesBothDepths compares every rendered template, in both
// depths, with wantSentences, so a wording change is a reviewed edit of that
// table. With -update the rendered text and a regenerated table are written
// to testdata/finops/narrative for review; the test still compares.
func TestGoldenSentencesBothDepths(t *testing.T) {
	if *update {
		writeSentenceSnapshot(t)
	}
	for _, d := range []Depth{Summary, Details} {
		t.Run(string(d), func(t *testing.T) {
			got := goldenCases(d)
			if len(got) != len(wantSentences) {
				t.Fatalf("%d cases, want %d", len(got), len(wantSentences))
			}
			for i, want := range wantSentences {
				text := want.summary
				if d == Details {
					text = want.details
				}
				if got[i][0] != want.label {
					t.Errorf("case %d is %q, want %q", i, got[i][0], want.label)
					continue
				}
				if got[i][1] != text {
					t.Errorf("%s:\n got: %q\nwant: %q", want.label, got[i][1], text)
				}
			}
		})
	}
}

// writeSentenceSnapshot writes both depths' rendered text and a regenerated
// wantSentences table under the gitignored testdata folder.
func writeSentenceSnapshot(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(sentenceSnapshotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary, details := goldenCases(Summary), goldenCases(Details)
	for d, cases := range map[Depth][][2]string{Summary: summary, Details: details} {
		if err := os.WriteFile(filepath.Join(sentenceSnapshotDir, string(d)+".txt"), []byte(render(cases)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var table strings.Builder
	table.WriteString("var wantSentences = []sentenceCase{\n")
	for i := range summary {
		table.WriteString("\t{" + strconv.Quote(summary[i][0]) + ",\n\t\t" + strconv.Quote(summary[i][1]) +
			",\n\t\t" + strconv.Quote(details[i][1]) + "},\n")
	}
	table.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(sentenceSnapshotDir, "sentences.go.txt"), []byte(table.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMoneyFormatting(t *testing.T) {
	cases := []struct {
		micros int64
		cur    string
		want   string
		signed string
	}{
		{0, "", "$0", "$0"},
		{200_000, "", "$0.20", "+$0.20"},
		{50 * usd, "", "$50", "+$50"},
		{99_994_999, "", "$99.99", "+$99.99"},
		{99_995_000, "", "$100", "+$100"},
		{100_500_000, "", "$101", "+$101"},
		{43_520 * usd, "USD", "$43,520", "+$43,520"},
		{-2_310 * usd, "", "−$2,310", "−$2,310"},
		{-4_000, "", "$0", "$0"},
		{1_234_567_890 * usd, "", "$1,234,567,890", "+$1,234,567,890"},
		{12 * usd, "eur", "€12", "+€12"},
		{12 * usd, "CAD", "12 CAD", "+12 CAD"},
		{-9223372036854775808, "", "−$9,223,372,036,855", "−$9,223,372,036,855"},
	}
	for _, c := range cases {
		if got := Money(c.micros, c.cur); got != c.want {
			t.Errorf("Money(%d) = %q, want %q", c.micros, got, c.want)
		}
		if got := SignedMoney(c.micros, c.cur); got != c.signed {
			t.Errorf("SignedMoney(%d) = %q, want %q", c.micros, got, c.signed)
		}
	}
}

func TestPercentAndDirection(t *testing.T) {
	cases := map[int64]string{910: "+9.1%", -580: "−5.8%", 40: "+0.4%", 0: "0%", 4: "0%", 1000: "+10%", -5: "−0.1%"}
	for bp, want := range cases {
		if got := SignedPercent(bp); got != want {
			t.Errorf("SignedPercent(%d) = %q, want %q", bp, got, want)
		}
	}
	if DirectionOf(200) != Same || DirectionOf(-200) != Same || DirectionOf(201) != Up || DirectionOf(-201) != Down {
		t.Fatal("the about-the-same band is ±2% inclusive")
	}
	if ParseDepth("DETAILS") != Details || ParseDepth("") != Summary || ParseDepth("x") != Summary {
		t.Fatal("ParseDepth")
	}
}

// The answer line must drop a clause it cannot back with data.
func TestAnswerDropsUnbackedClauses(t *testing.T) {
	got := AnswerLine(Answer{PeriodLabel: "October", Total: 10 * usd, Top: &nat}, Summary)
	if got != "October cost $10." {
		t.Fatalf("no base: driver clause must be dropped, got %q", got)
	}
	got = AnswerLine(Answer{PeriodLabel: "November", Open: true, Total: 10 * usd, HasBudget: true, Budget: 100 * usd}, Summary)
	if strings.Contains(got, "budget") {
		t.Fatalf("open month with no forecast must not compare the budget, got %q", got)
	}
}

// IncreaseChange states the period's change, or the increase's own change
// over its own days, says when it resolved, and drops an amount that would
// print as "$0" (anything under half a cent).
func TestIncreaseChangeClause(t *testing.T) {
	cases := []struct {
		name string
		in   Increase
		want string
	}{
		{"in period", Increase{Amount: 1_980 * usd, PeriodWord: "in October"},
			"went up $1,980 in October"},
		{"in period, default word", Increase{Amount: 640 * usd},
			"went up $640 this month"},
		{"resolved in period", Increase{Amount: 640 * usd, PeriodWord: "this month", ResolvedOn: day(time.November, 5)},
			"went up $640 this month and resolved on Nov 5"},
		{"out of period window, open", Increase{Amount: 300 * usd, Window: true, WindowDays: 5},
			"went up $300 over 5 days so far"},
		{"out of period window, resolved", Increase{Amount: 440 * usd, Window: true, WindowDays: 10, ResolvedOn: day(time.October, 29)},
			"went up $440 over 10 days before it resolved on Oct 29"},
		{"one day window", Increase{Amount: 44 * usd, Window: true, WindowDays: 1, ResolvedOn: day(time.October, 20)},
			"went up $44 over 1 day before it resolved on Oct 20"},
		{"rounds to zero in period", Increase{Amount: 4_999, PeriodWord: "this month"},
			"went up this month"},
		{"rounds to zero window", Increase{Amount: 4_999, Window: true, WindowDays: 3, ResolvedOn: day(time.October, 29)},
			"went up over 3 days before it resolved on Oct 29"},
		{"half a cent shows", Increase{Amount: 5_000, PeriodWord: "this month"},
			"went up $0.01 this month"},
		{"negative is dropped", Increase{Amount: -20 * usd, Window: true, WindowDays: 4},
			"went up over 4 days so far"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IncreaseChange(c.in); got != c.want {
				t.Fatalf("IncreaseChange = %q, want %q", got, c.want)
			}
		})
	}
}
