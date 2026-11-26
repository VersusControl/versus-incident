package issues

import (
	"context"
	"fmt"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// queueIssue is one stored-looking issue for the read side.
func queueIssue(index int, severity Severity, state State) Issue {
	issue := Issue{
		ID: IssueID(testRuleID, fmt.Sprintf("arn:aws:s3:::bucket-%04d", index)), Rule: testRuleID, Kind: KindSecurity,
		Severity: severity, Tier: TierFor(severity), Account: testAccount, Region: "eu-west-1",
		Owner: fmt.Sprintf("arn:aws:s3:::bucket-%04d", index), Title: fmt.Sprintf("Bucket %d is public", index),
		State: state, Confirm: Confirmed, FirstSeen: start, OpenedAt: start, Version: 1, UpdatedAt: start,
		Steps: []Step{{Text: "Block public access"}},
	}
	if state == StateRejected {
		issue.Rejection = &Rejection{Reason: "public on purpose", By: "alice", At: start, ReviewOn: "2027-01-01", SeverityAtReject: severity}
	}
	return issue
}

var notReadRows = []snapshot.CoverageRow{
	{Source: "guardduty", Outcome: model.OutcomeNotRead, Reason: model.GapDenied, Accounts: []string{testAccount}},
	{Source: "health", Outcome: model.OutcomeNotRead, Reason: model.GapNotOnPlan, Accounts: []string{testAccount}},
}

// More than 1,000 rows: the report holds 1,000, in section order, and says
// "more than 1,000, not all included". It carries the warning line, every
// source not read, the filters and the as-of time.
func TestReportIsCappedAndSaysSo(t *testing.T) {
	var selected []Issue
	for index := range 900 {
		selected = append(selected, queueIssue(index, SeverityHigh, StateOpen))
	}
	for index := 900; index < 1000; index++ {
		selected = append(selected, queueIssue(index, SeverityMedium, StateOpen))
	}
	for index := 1000; index < 1050; index++ {
		selected = append(selected, queueIssue(index, SeverityLow, StateRejected))
	}
	filter := Filter{Kind: "security"}
	coverage := CoverageRead{AccountsRead: 3, AccountsTotal: 4, NotRead: notReadRows}
	report := BuildReport(selected, filter, coverage, start)
	if report.Rows != 1050 || report.Included != MaxReportRows || !report.Capped || report.CappedText != "more than 1,000, not all included" {
		t.Fatalf("rows %d included %d capped %v %q", report.Rows, report.Included, report.Capped, report.CappedText)
	}
	if len(report.Sections.ActNow) != 900 || len(report.Sections.WorthALook) != 100 || len(report.Sections.Rejected) != 0 {
		t.Fatalf("sections %d/%d/%d", len(report.Sections.ActNow), len(report.Sections.WorthALook), len(report.Sections.Rejected))
	}
	if report.Warning == nil || report.Warning.NotRead != 2 || len(report.NotRead) != 2 || report.Filters != filter || !report.AsOf.Equal(start) {
		t.Fatalf("header = %+v %+v %+v %v", report.Warning, report.NotRead, report.Filters, report.AsOf)
	}
	if report.Summary.AccountsRead != 3 || report.Summary.AccountsTotal != 4 || report.Summary.RejectedCount != 50 {
		t.Fatalf("summary = %+v", report.Summary)
	}

	small := BuildReport(selected[:10], filter, CoverageRead{}, start)
	if small.Capped || small.CappedText != "" || small.Warning != nil || small.NotRead == nil {
		t.Fatalf("an uncapped report with every source read = %+v", small)
	}
}

// Rejected rows carry the reason, who, when and the review date; open
// rows carry what to do and the likely cause only when one was found.
func TestReportRows(t *testing.T) {
	found := queueIssue(1, SeverityCritical, StateOpen)
	found.Cause = &Cause{Status: "found", Text: "Bucket policy changed by deployer, 5 min before"}
	none := queueIssue(2, SeverityHigh, StateAcknowledged)
	none.Cause = &Cause{Status: "none", Text: "no change found before it"}
	rejected := queueIssue(3, SeverityHigh, StateRejected)
	report := BuildReport([]Issue{found, none, rejected}, Filter{}, CoverageRead{}, start)
	rows := report.Sections.ActNow
	if len(rows) != 2 || rows[0].LikelyCause == "" || rows[1].LikelyCause != "" || len(rows[0].Steps) != 1 {
		t.Fatalf("act now rows = %+v", rows)
	}
	if got := report.Sections.Rejected; len(got) != 1 || got[0].Rejection == nil || got[0].Rejection.Reason != "public on purpose" ||
		got[0].Rejection.By != "alice" || got[0].Rejection.ReviewOn != "2027-01-01" || got[0].Steps != nil {
		t.Fatalf("rejected rows = %+v", got)
	}
}

// The page, its summary and the report count the same issues: the listed
// rows are the summary's open count, the Rejected tab is the rejected
// count, and the report's sections hold the same numbers.
func TestListSummaryAndReportCountsMatch(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	store, _ := storage.AWSIssuesOf(provider)
	states := []State{StateOpen, StateOpen, StateAcknowledged, StateRegressed, StateRejected, StateRejected, StateResolved}
	severities := []Severity{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow}
	for index, state := range states {
		issue := queueIssue(index, severities[index%len(severities)], state)
		if state == StateResolved {
			issue.ResolvedAt = start
		}
		write, err := writeOf(Change{Issue: issue, Events: []Event{{IssueID: issue.ID, At: start, Kind: EventOpened, Actor: EvaluatorActor}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.WriteAWSIssues(context.Background(), scope, storage.AWSIssueBatch{AccountID: testAccount, Writes: []storage.AWSIssueWrite{write}}); err != nil {
			t.Fatal(err)
		}
	}
	actions, _ := NewActions(provider, scope, nil)
	for _, filter := range []Filter{{}, {Severity: "high"}, {Kind: "security", Account: testAccount}} {
		selected, err := actions.Selected(context.Background(), filter)
		if err != nil {
			t.Fatal(err)
		}
		page := BuildPage(selected, filter, CoverageRead{})
		rejectedFilter := filter
		rejectedFilter.State = "rejected"
		tab := BuildPage(selected, rejectedFilter, CoverageRead{})
		report := BuildReport(selected, filter, CoverageRead{}, start)
		open := len(report.Sections.ActNow) + len(report.Sections.WorthALook)
		if len(page.Items) != page.Summary.Open || open != page.Summary.Open || report.Summary != page.Summary ||
			len(tab.Items) != page.RejectedCount || len(report.Sections.Rejected) != page.RejectedCount || tab.Summary != page.Summary {
			t.Fatalf("filter %+v: listed %d, open %d, report %d; rejected tab %d, count %d, report %d",
				filter, len(page.Items), page.Summary.Open, open, len(tab.Items), page.RejectedCount, len(report.Sections.Rejected))
		}
	}
	if _, err := actions.Selected(context.Background(), Filter{State: "deleted"}); err != ErrInvalidFilter {
		t.Fatalf("an unknown state = %v", err)
	}
	if _, err := actions.Selected(context.Background(), Filter{Severity: "urgent"}); err != ErrInvalidFilter {
		t.Fatalf("an unknown severity = %v", err)
	}
}

// Not assessed is read at read time from the views and rules: a rule whose
// input is not read or off in AWS is not assessed there; one read in full
// is not listed.
func TestNotAssessedNow(t *testing.T) {
	source := newFakeSource(testAccount, "123400000005")
	source.read(testAccount, "s3", "eu-west-1", ReadOffInAWS)
	got, err := NotAssessedNow(context.Background(), source, publicBucketRules(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != testAccount || len(got[0].NotAssessed) != 2 {
		t.Fatalf("not assessed = %+v; want the account-wide and the eu-west-1 entries of one account", got)
	}
	if regional := got[0].NotAssessed[1]; regional.Region != "eu-west-1" || regional.Missing[0] != "s3 off in AWS in eu-west-1" {
		t.Fatalf("regional entry = %+v", regional)
	}
	only, err := NotAssessedNow(context.Background(), source, publicBucketRules(), []string{"123400000005"})
	if err != nil || len(only) != 0 {
		t.Fatalf("an account read in full = %+v, %v", only, err)
	}
}
