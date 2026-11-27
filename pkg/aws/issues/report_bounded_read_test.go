package issues

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// readCountingStore counts the issue rows each read returns.
type readCountingStore struct {
	storage.Provider
	storage.AWSIssueStore
	rows *int
}

func (store readCountingStore) AWSIssues(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueRecord, error) {
	records, err := store.AWSIssueStore.AWSIssues(ctx, scope, query)
	*store.rows += len(records)
	return records, err
}

// seedQueue writes count issues across every state and severity, opened at
// different times, in batches of 500.
func seedQueue(t *testing.T, store storage.AWSIssueStore, scope tenancy.OrgScope, count int) {
	t.Helper()
	states := []State{StateOpen, StateOpen, StateOpen, StateAcknowledged, StateRegressed, StateRejected, StateRejected, StateResolved}
	severities := []Severity{SeverityCritical, SeverityHigh, SeverityHigh, SeverityMedium, SeverityLow}
	var writes []storage.AWSIssueWrite
	flush := func() {
		if len(writes) == 0 {
			return
		}
		if err := store.WriteAWSIssues(context.Background(), scope, storage.AWSIssueBatch{AccountID: testAccount, Writes: writes}); err != nil {
			t.Fatal(err)
		}
		writes = nil
	}
	for index := range count {
		issue := queueIssue(index, severities[index%len(severities)], states[index%len(states)])
		issue.OpenedAt = start.Add(-time.Duration(index%97) * time.Minute)
		if issue.State == StateResolved {
			issue.ResolvedAt = start
		}
		write, err := writeOf(Change{Issue: issue, Events: []Event{{IssueID: issue.ID, At: start, Kind: EventOpened, Actor: EvaluatorActor}}})
		if err != nil {
			t.Fatal(err)
		}
		writes = append(writes, write)
		if len(writes) == 500 {
			flush()
		}
	}
	flush()
}

func reportJSON(t *testing.T, report IssueReport) string {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// With 2,500 issues stored, the report reads at most 1,001 of them on every
// backend, still says "more than 1,000", and holds exactly the rows, in
// the same order, as a report over every issue. The page and the report
// count in the store what a count over every issue gives.
func TestReportReadsAtMostOneRowPastTheCap(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			scope := newScope("report-cap")
			raw, ok := storage.AWSIssuesOf(backend.provider)
			if !ok {
				t.Fatalf("%s has no issue store", backend.name)
			}
			seedQueue(t, raw, scope, 2500)
			rows := 0
			actions, err := NewActions(readCountingStore{Provider: backend.provider, AWSIssueStore: raw, rows: &rows}, scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			coverage := CoverageRead{AccountsRead: 1, AccountsTotal: 1}
			for _, filter := range []Filter{{}, {State: "rejected"}, {State: "open"}, {Severity: "high"}, {Kind: "security", Account: testAccount}} {
				rows = 0
				report, err := actions.Report(ctx, filter, coverage, start)
				if err != nil {
					t.Fatalf("report %+v: %v", filter, err)
				}
				if rows > MaxReportRows+1 {
					t.Fatalf("report %+v read %d rows; want at most %d", filter, rows, MaxReportRows+1)
				}
				selected, err := actions.Selected(ctx, filter)
				if err != nil {
					t.Fatal(err)
				}
				if want := BuildReport(selected, filter, coverage, start); reportJSON(t, report) != reportJSON(t, want) {
					t.Fatalf("report %+v: rows %d included %d capped %v; a report over every issue: rows %d included %d capped %v",
						filter, report.Rows, report.Included, report.Capped, want.Rows, want.Included, want.Capped)
				}
				if filter == (Filter{}) && (!report.Capped || report.CappedText != ReportCappedText || report.Included != MaxReportRows) {
					t.Fatalf("the full report is not capped: rows %d included %d", report.Rows, report.Included)
				}

				rows = 0
				page, err := actions.Page(ctx, filter, coverage)
				if err != nil {
					t.Fatal(err)
				}
				pageRows := rows
				want := BuildPage(selected, filter, coverage)
				if page.Summary != want.Summary || page.RejectedCount != want.RejectedCount || len(page.Items) != len(want.Items) {
					t.Fatalf("page %+v: summary %+v rejected %d items %d; over every issue %+v %d %d",
						filter, page.Summary, page.RejectedCount, len(page.Items), want.Summary, want.RejectedCount, len(want.Items))
				}
				if pageRows != len(want.Items) {
					t.Fatalf("page %+v read %d rows to list %d", filter, pageRows, len(want.Items))
				}
			}
		})
	}
}
