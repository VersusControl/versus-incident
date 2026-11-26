package issues

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// MaxReportRows bounds the rows of one report; beyond it the report says
// ReportCappedText.
const MaxReportRows = 1000

// ReportCappedText is what a capped report says.
const ReportCappedText = "more than 1,000, not all included"

// Filter is what the Issues page and its report select. Every empty field
// matches all. An empty State lists the open issues: open, acknowledged
// and regressed.
type Filter struct {
	Kind     string `json:"kind,omitempty"`
	Severity string `json:"severity,omitempty"`
	Account  string `json:"account,omitempty"`
	State    string `json:"state,omitempty"`
	Group    string `json:"group,omitempty"`
}

// ErrInvalidFilter is a filter value that is not one the store knows.
var ErrInvalidFilter = errors.New("issues: invalid filter")

// CoverageRead is the coverage read the summary and the report show: how
// many accounts were read, and every source that was not.
type CoverageRead struct {
	AccountsRead  int
	AccountsTotal int
	// NotRead lists every source that was not read.
	NotRead []snapshot.CoverageRow
}

// Warning is the summary card's warning line; nil when every source read.
func (coverage CoverageRead) Warning() *snapshot.WarningLine {
	if len(coverage.NotRead) == 0 {
		return nil
	}
	return &snapshot.WarningLine{NotRead: len(coverage.NotRead)}
}

// Page is the Issues page: the summary every reader shows, the warning
// line, the issues the filter lists and the rejected count.
type Page struct {
	Summary       Summary               `json:"summary"`
	Warning       *snapshot.WarningLine `json:"warning"`
	Items         []Issue               `json:"items"`
	RejectedCount int                   `json:"rejected_count"`
}

// Selected reads the issues a filter selects, State aside: the set the
// summary counts. The state filter then picks the listed rows from it, so
// the page, the summary and the report count the same issues.
func (a *Actions) Selected(ctx context.Context, filter Filter) ([]Issue, error) {
	query, err := filterQuery(filter)
	if err != nil {
		return nil, err
	}
	list, err := a.List(ctx, query)
	if errors.Is(err, storage.ErrInvalidAWSIssue) {
		return nil, ErrInvalidFilter
	}
	return list, err
}

// filterQuery is the store query of a filter, its state aside.
func filterQuery(filter Filter) (storage.AWSIssueQuery, error) {
	if filter.State != "" && !validState(filter.State) {
		return storage.AWSIssueQuery{}, ErrInvalidFilter
	}
	return storage.AWSIssueQuery{Kind: filter.Kind, Severity: filter.Severity, AccountID: filter.Account, GroupID: filter.Group}, nil
}

// ValidFilter refuses a filter value the store does not know, before
// anything is read.
func ValidFilter(filter Filter) error {
	query, err := filterQuery(filter)
	if err != nil {
		return err
	}
	if !storage.ValidAWSIssueQuery(query) {
		return ErrInvalidFilter
	}
	return nil
}

// listedStates are the states a state filter lists: the open ones when it
// is empty.
func listedStates(state string) []string {
	if state == "" {
		return []string{string(StateOpen), string(StateAcknowledged), string(StateRegressed)}
	}
	return []string{state}
}

// summaryOf counts, in the store, the issues a filter selects, its state
// aside: the summary every reader shows, without reading the issues.
func (a *Actions) summaryOf(ctx context.Context, query storage.AWSIssueQuery, coverage CoverageRead) (Summary, error) {
	counts, err := a.store.AWSIssueCounts(ctx, a.scope, query)
	if errors.Is(err, storage.ErrInvalidAWSIssue) {
		return Summary{}, ErrInvalidFilter
	}
	if err != nil {
		return Summary{}, err
	}
	return SummarizeCounts(counts, Coverage{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal}), nil
}

// queued reads the issues a query selects in queue order.
func (a *Actions) queued(ctx context.Context, query storage.AWSIssueQuery) ([]Issue, error) {
	query.QueueOrder = true
	records, err := a.store.AWSIssues(ctx, a.scope, query)
	if errors.Is(err, storage.ErrInvalidAWSIssue) {
		return nil, ErrInvalidFilter
	}
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(records))
	for _, record := range records {
		issue, err := FromRecord(record)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	sortQueue(out)
	return out, nil
}

// Page reads the Issues page: the summary is counted in the store, and
// only the issues the state filter lists are read.
func (a *Actions) Page(ctx context.Context, filter Filter, coverage CoverageRead) (Page, error) {
	query, err := filterQuery(filter)
	if err != nil {
		return Page{}, err
	}
	summary, err := a.summaryOf(ctx, query, coverage)
	if err != nil {
		return Page{}, err
	}
	query.States = listedStates(filter.State)
	items, err := a.queued(ctx, query)
	if err != nil {
		return Page{}, err
	}
	return Page{Summary: summary, Warning: coverage.Warning(), Items: items, RejectedCount: summary.RejectedCount}, nil
}

// Report reads the report: the summary and the row count are counted in
// the store, and at most MaxReportRows+1 issues are read, in section
// order, so a report over more than MaxReportRows still says so.
func (a *Actions) Report(ctx context.Context, filter Filter, coverage CoverageRead, asOf time.Time) (IssueReport, error) {
	if err := ValidReportFilter(filter); err != nil {
		return IssueReport{}, err
	}
	query, err := filterQuery(filter)
	if err != nil {
		return IssueReport{}, err
	}
	counts, err := a.store.AWSIssueCounts(ctx, a.scope, query)
	if errors.Is(err, storage.ErrInvalidAWSIssue) {
		return IssueReport{}, ErrInvalidFilter
	}
	if err != nil {
		return IssueReport{}, err
	}
	summary := SummarizeCounts(counts, Coverage{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal})
	rows := 0
	for _, count := range counts {
		state := State(count.State)
		if (state.Active() || state == StateRejected) && (filter.State == "" || count.State == filter.State) {
			rows += count.Count
		}
	}
	budget := MaxReportRows + 1
	var open, rejected []Issue
	if filter.State == "" || State(filter.State).Active() {
		read := query
		read.States, read.Limit = listedStates(filter.State), budget
		if open, err = a.queued(ctx, read); err != nil {
			return IssueReport{}, err
		}
		budget -= len(open)
	}
	if (filter.State == "" || State(filter.State) == StateRejected) && budget > 0 {
		read := query
		read.States, read.Limit = []string{string(StateRejected)}, budget
		if rejected, err = a.queued(ctx, read); err != nil {
			return IssueReport{}, err
		}
	}
	return buildReport(summary, max(rows, len(open)+len(rejected)), open, rejected, filter, coverage, asOf), nil
}

// ValidReportFilter refuses a state the report has no section for: the
// report lists open, acknowledged, regressed and rejected issues only, so
// any other state would print an empty report that looks complete.
func ValidReportFilter(filter Filter) error {
	switch State(filter.State) {
	case "", StateOpen, StateAcknowledged, StateRegressed, StateRejected:
		return nil
	}
	return ErrInvalidFilter
}

func validState(state string) bool {
	switch State(state) {
	case StateOpen, StateAcknowledged, StateRejected, StateResolved, StateRegressed:
		return true
	}
	return false
}

// Listed is the rows a state picks: the open issues when it is empty.
func Listed(selected []Issue, state string) []Issue {
	out := []Issue{}
	for _, issue := range selected {
		if state == "" && issue.State.Active() || state != "" && string(issue.State) == state {
			out = append(out, issue)
		}
	}
	sortQueue(out)
	return out
}

// BuildPage is the Issues page over the selected issues.
func BuildPage(selected []Issue, filter Filter, coverage CoverageRead) Page {
	summary := Summarize(selected, Coverage{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal})
	return Page{Summary: summary, Warning: coverage.Warning(), Items: Listed(selected, filter.State), RejectedCount: summary.RejectedCount}
}

// sortQueue orders the queue: most severe first, then the newest problem,
// then by ID so the order is stable.
func sortQueue(list []Issue) {
	sort.SliceStable(list, func(left, right int) bool {
		a, b := list[left], list[right]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if !a.OpenedAt.Equal(b.OpenedAt) {
			return a.OpenedAt.After(b.OpenedAt)
		}
		return a.ID < b.ID
	})
}

// ReportRow is one issue in the report.
type ReportRow struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Kind     Kind     `json:"kind"`
	Title    string   `json:"title"`
	// Where is the account, Region and owner.
	Account string  `json:"account"`
	Region  string  `json:"region,omitempty"`
	Owner   string  `json:"owner"`
	State   State   `json:"state"`
	Confirm Confirm `json:"confirm"`
	// Since is when the problem started; SinceText says it in words, also
	// when it existed when first read.
	Since     time.Time `json:"since,omitzero"`
	SinceText string    `json:"since_text"`
	// LikelyCause is set only when a change was found.
	LikelyCause string `json:"likely_cause,omitempty"`
	// Steps say what to do (Act now and Worth a look rows).
	Steps []Step `json:"steps,omitempty"`
	// Rejection is the reason, who and when (Rejected rows). The reason
	// is user-written plain text.
	Rejection *Rejection `json:"rejection,omitempty"`
}

// ReportSections are the report's three sections.
type ReportSections struct {
	ActNow     []ReportRow `json:"act_now"`
	WorthALook []ReportRow `json:"worth_a_look"`
	Rejected   []ReportRow `json:"rejected"`
}

// IssueReport is the data of the PDF report: the scope and filters, the as-of
// time, the summary, the warning line, every source not read, and the
// rows in three sections, at most MaxReportRows of them.
type IssueReport struct {
	Filters Filter                `json:"filters"`
	AsOf    time.Time             `json:"as_of"`
	Summary Summary               `json:"summary"`
	Warning *snapshot.WarningLine `json:"warning"`
	// NotRead lists every source that was not read.
	NotRead  []snapshot.CoverageRow `json:"not_read"`
	Sections ReportSections         `json:"sections"`
	// Rows is how many rows the filters select; Included how many the
	// report holds. Capped says CappedText when not all are included.
	Rows       int    `json:"rows"`
	Included   int    `json:"included"`
	Capped     bool   `json:"capped"`
	CappedText string `json:"capped_text,omitempty"`
}

// BuildReport is the report over the selected issues: Act now and Worth a
// look hold the open issues, Rejected the rejected ones, each section
// limited to the state filter when one is set. Rows are taken in section
// order up to MaxReportRows.
func BuildReport(selected []Issue, filter Filter, coverage CoverageRead, asOf time.Time) IssueReport {
	summary := Summarize(selected, Coverage{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal})
	var open, rejected []Issue
	for _, issue := range selected {
		if filter.State != "" && string(issue.State) != filter.State {
			continue
		}
		switch {
		case issue.State.Active():
			open = append(open, issue)
		case issue.State == StateRejected:
			rejected = append(rejected, issue)
		}
	}
	sortQueue(open)
	sortQueue(rejected)
	return buildReport(summary, len(open)+len(rejected), open, rejected, filter, coverage, asOf)
}

// buildReport lays out a report of rows issues, of which open and
// rejected, each in queue order, are the first read.
func buildReport(summary Summary, rows int, open, rejected []Issue, filter Filter, coverage CoverageRead, asOf time.Time) IssueReport {
	report := IssueReport{Filters: filter, AsOf: asOf.UTC(), Summary: summary, Warning: coverage.Warning(), NotRead: coverage.NotRead,
		Sections: ReportSections{ActNow: []ReportRow{}, WorthALook: []ReportRow{}, Rejected: []ReportRow{}}}
	if report.NotRead == nil {
		report.NotRead = []snapshot.CoverageRow{}
	}
	report.Rows = rows
	room := MaxReportRows
	for _, issue := range open {
		if room == 0 {
			break
		}
		room--
		row := reportRow(issue)
		row.Steps = issue.Steps
		if issue.Tier == TierWorthALook {
			report.Sections.WorthALook = append(report.Sections.WorthALook, row)
		} else {
			report.Sections.ActNow = append(report.Sections.ActNow, row)
		}
	}
	for _, issue := range rejected {
		if room == 0 {
			break
		}
		room--
		row := reportRow(issue)
		row.Rejection = issue.Rejection
		report.Sections.Rejected = append(report.Sections.Rejected, row)
	}
	report.Included = MaxReportRows - room
	if report.Included < report.Rows {
		report.Capped, report.CappedText = true, ReportCappedText
	}
	return report
}

func reportRow(issue Issue) ReportRow {
	row := ReportRow{ID: issue.ID, Severity: issue.Severity, Kind: issue.Kind, Title: issue.Title, Account: issue.Account,
		Region: issue.Region, Owner: issue.Owner, State: issue.State, Confirm: issue.Confirm, SinceText: "existed when first read"}
	if issue.SinceKnown && !issue.Since.IsZero() {
		row.Since = issue.Since.UTC()
		row.SinceText = "since " + row.Since.Format("2006-01-02 15:04 UTC")
	}
	if issue.Cause.Found() {
		row.LikelyCause = issue.Cause.Text
	}
	return row
}

// AccountNotAssessed is what could not be assessed on one account.
type AccountNotAssessed struct {
	Account     string        `json:"account"`
	NotAssessed []NotAssessed `json:"not_assessed"`
}

// NotAssessedNow says, at read time, which rules cannot be assessed where:
// a rule whose input is off in AWS or not read, for the whole account and
// in each Region the account was read in. It reads the same views and
// rules as the evaluator, so every replica gives the same answer. Accounts
// with nothing missing are left out; an empty accounts list reads every
// account the source has.
func NotAssessedNow(ctx context.Context, source Source, rules *Registry, accounts []string) ([]AccountNotAssessed, error) {
	if rules == nil {
		rules = Default
	}
	if len(accounts) == 0 {
		all, err := source.Accounts(ctx)
		if err != nil {
			return nil, err
		}
		accounts = all
	}
	accounts = append([]string(nil), accounts...)
	sort.Strings(accounts)
	out := []AccountNotAssessed{}
	for _, account := range accounts {
		view, err := source.View(ctx, account)
		if err != nil {
			return nil, err
		}
		var missing []NotAssessed
		for _, rule := range rules.Rules() {
			assessment, inputs := Assess(rule, view)
			outcome := RuleOutcome{Rule: rule.ID(), Assessment: assessment, Missing: inputs, Regions: map[string]Assessed{}}
			for _, region := range view.Regions() {
				assessed, inputs := AssessIn(rule, view, region)
				outcome.Regions[region] = Assessed{Assessment: assessed, Missing: inputs}
			}
			missing = append(missing, notAssessed(outcome)...)
		}
		if len(missing) > 0 {
			out = append(out, AccountNotAssessed{Account: account, NotAssessed: missing})
		}
	}
	return out, nil
}
