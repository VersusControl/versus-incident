package finops

import "time"

// The view types below are the API contract of the FinOps routes and the
// read-only agent tools: both call the same Service method and serialise the
// same struct. Money is always an integer of micro-units ("*_micros"),
// shares and changes are basis points ("*_bp", 1/100 of a percent), days are
// "YYYY-MM-DD". Missing data is named (sync, reason fields), never zero.

// Header is shared by every view.
type Header struct {
	// Enabled is false when cost data is off; Reason then says why and
	// nothing else is set (the route still answers 200).
	Enabled  bool        `json:"enabled"`
	Reason   string      `json:"reason,omitempty"`
	Period   *Period     `json:"period,omitempty"`
	Compare  *Comparison `json:"compare,omitempty"`
	Depth    string      `json:"depth,omitempty"`
	Currency string      `json:"currency,omitempty"`
	Measure  string      `json:"measure,omitempty"` // always "net_amortized"
	Sync     *Sync       `json:"sync,omitempty"`
	Links    Links       `json:"links"`
	// CostSource names the account that reads cost (the chip on the page).
	CostSource *CostSourceView `json:"cost_source,omitempty"`
}

// CostSourceView is the account cost is read through: a management
// (payer) account covering every linked account, or the account itself.
// With several standalone accounts, Kind is "account" and Count says how
// many read their own cost.
type CostSourceView struct {
	AccountID string `json:"account_id"`
	// AccountName is the account's display name when known.
	AccountName string `json:"account_name,omitempty"`
	Kind        string `json:"kind"` // management | account
	Count       int    `json:"count,omitempty"`
}

// StaleDetail says since when the data is stale and which settled days are
// not loaded yet.
type StaleDetail struct {
	Since *time.Time `json:"since,omitempty"`
	Days  []Date     `json:"days"`
}

// AlertView is the alert sent for an increase. The alert path does not
// report acknowledgement back to FinOps, so only the send is shown.
type AlertView struct {
	Channel string    `json:"channel"`
	SentAt  time.Time `json:"sent_at"`
}

// ScopeOptions are the values the scope bar offers for the period.
type ScopeOptions struct {
	Accounts []ScopeAccountOption `json:"accounts"`
	Regions  []string             `json:"regions"`
	Teams    []string             `json:"teams"`
}

// ScopeAccountOption is one account in the scope bar.
type ScopeAccountOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// MeasureNetAmortized is the one cost measure.
const MeasureNetAmortized = "net_amortized"

// Links point the user (or an agent's answer) at the page.
type Links struct {
	UI string `json:"ui"`
}

// Sync describes freshness and every gap in the data behind a view.
type Sync struct {
	LastCompleteDay Date       `json:"last_complete_day"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	// PartialDays are days after the last complete day with some data;
	// charts draw them dashed and no rule reads them.
	PartialDays []Date `json:"partial_days,omitempty"`
	Stale       bool   `json:"stale"`
	// StaleReason is the adapter's closed error code for the last refresh
	// (for example "throttled", "timeout", "denied").
	StaleReason      string           `json:"stale_reason,omitempty"`
	AccountsReadable int              `json:"accounts_readable"`
	AccountsTotal    int              `json:"accounts_total"`
	MissingAccounts  []MissingAccount `json:"missing_accounts,omitempty"`
	// SkippedCalls are paid calls skipped today to stay under the cap.
	SkippedCalls []string `json:"skipped_calls,omitempty"`
	// OtherCurrencies are totals in currencies other than the view's;
	// they are never converted or added.
	OtherCurrencies []CurrencyTotal `json:"other_currencies,omitempty"`
	// Partial is true when any account, call or currency is missing.
	Partial bool `json:"partial"`
	// Learning is true while history is shorter than the detection
	// baseline.
	Learning          bool      `json:"learning"`
	PreviousMonthOpen bool      `json:"previous_month_open"`
	ResourceData      bool      `json:"resource_data"`
	TeamTag           TagStatus `json:"team_tag"`
	// LearningUntil is the first last-complete day with full detection
	// history; set while learning.
	LearningUntil Date `json:"learning_until,omitzero"`
	// BillClosesOn is the day the most recent open bill closes.
	BillClosesOn Date `json:"bill_closes_on,omitzero"`
	// NextRefreshAt is the next scheduled refresh.
	NextRefreshAt *time.Time `json:"next_refresh_at,omitempty"`
	// StaleDetail is set with Stale.
	StaleDetail *StaleDetail `json:"stale_detail,omitempty"`
	// Estimated names the filters ("team", "region") whose figures in this
	// view rest on proportional split cells (a team or region share the
	// source could not read directly). It is never the provider's
	// "Estimated" day flag: that flag means the bill is not final yet and
	// shows only as previous_month_open, bill_closes_on and the report's
	// not_final; no total is ever marked estimated from it.
	Estimated []string `json:"estimated,omitempty"`
	// Notes are the gap notes in words, in the view's depth.
	Notes []string `json:"notes,omitempty"`
}

// MissingAccount names an account whose cost could not be read.
type MissingAccount struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// CurrencyTotal is a total in one currency.
type CurrencyTotal struct {
	Currency string `json:"currency"`
	Amount   Micros `json:"amount_micros"`
}

// DayAmount is one day of a series.
type DayAmount struct {
	Day     Date   `json:"day"`
	Amount  Micros `json:"amount_micros"`
	Partial bool   `json:"partial,omitempty"`
}

// MeterView is today's paid-request use.
type MeterView struct {
	Used int `json:"used"`
	Cap  int `json:"cap"`
}

// ChangeView compares the period with its base.
type ChangeView struct {
	Base      Micros `json:"base_micros"`
	Change    Micros `json:"change_micros"`
	ChangeBP  int64  `json:"change_bp"`
	Direction string `json:"direction"` // up | down | same
	Word      string `json:"word"`      // "Up", "Down", "About the same"
}

// SummaryBody is the content of the summary.
type SummaryBody struct {
	// Answer is the one-line answer (Copy summary, report, alerts; not
	// shown on the page).
	Answer string `json:"answer"`
	Total  Micros `json:"total_micros"`
	// Change is nil when the base is not available (see compare.reason).
	Change      *ChangeView      `json:"change"`
	Sparkline   []DayAmount      `json:"sparkline"`
	Forecast    Forecast         `json:"forecast"`
	Budget      *BudgetLine      `json:"budget"` // deployment budget; nil when none
	Savings     SavingsTotal     `json:"savings"`
	Unallocated UnallocatedShare `json:"unallocated"`
	Meter       MeterView        `json:"meter"`
	// ScopeOptions are the scope bar's choices for the period.
	ScopeOptions ScopeOptions `json:"scope_options"`
}

// SummaryView is GET …/finops/summary.
type SummaryView struct {
	Header
	SummaryBody
}

// SavingsTotal sums open savings.
type SavingsTotal struct {
	Monthly Micros `json:"monthly_micros"`
	Count   int    `json:"count"`
	// CommitmentMonthly and CommitmentCount are the open commitment items
	// left out of the total (each is computed before the other savings).
	// A zero total beside a commitment reads from these, never as "$0".
	CommitmentMonthly Micros `json:"commitment_monthly_micros"`
	CommitmentCount   int    `json:"commitment_count"`
}

// UnallocatedShare is the cost no ownership rule assigns.
type UnallocatedShare struct {
	Amount  Micros `json:"amount_micros"`
	ShareBP int64  `json:"share_bp"`
}

// WhyView is the short "why" of a bridge row or increase.
type WhyView struct {
	// State is found | none_found | history_incomplete |
	// collectors_pending | account_not_connected. none_found is used only
	// when the change history covers the whole window.
	State string `json:"state"`
	Text  string `json:"text,omitempty"`
}

// BridgeRow is one driver row.
type BridgeRow struct {
	ID           string `json:"id"`
	Cause        string `json:"cause"`
	Label        string `json:"label"`
	Amount       Micros `json:"amount_micros"`
	Account      string `json:"account,omitempty"`
	AccountName  string `json:"account_name,omitempty"`
	Service      string `json:"service,omitempty"`
	ServiceLabel string `json:"service_label,omitempty"`
	UsageType    string `json:"usage_type,omitempty"` // details depth only
	RecordType   string `json:"record_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
	// Resources is "too_large" when the row names no resource because the
	// resource lines of its account and service were more than one read
	// holds; empty otherwise (a resource, or none to name).
	Resources  string `json:"resources,omitempty"`
	Owner      *Owner `json:"owner,omitempty"`
	Since      Date   `json:"since,omitzero"`
	IncreaseID string `json:"increase_id,omitempty"`
	Status     string `json:"status,omitempty"` // increase status when linked
	// AccountKind is connected | cost_only | payer (bill-level rows such as
	// an unused Savings Plan, support, credits or tax).
	AccountKind string    `json:"account_kind,omitempty"`
	Why         *WhyView  `json:"why,omitempty"`
	Lines       int       `json:"lines,omitempty"`
	Sentence    string    `json:"sentence,omitempty"` // report: why and what to do
	Expected    *Expected `json:"expected,omitempty"`
}

// ColumnRule says whether an optional column shows and, if not, why.
type ColumnRule struct {
	Show bool   `json:"show"`
	Note string `json:"note,omitempty"`
}

// BridgeBody is the content of the bridge.
type BridgeBody struct {
	// Available is false when there is no comparable base; Note then says
	// from when the bridge is available.
	Available bool        `json:"available"`
	Note      string      `json:"note,omitempty"`
	Base      Micros      `json:"base_micros"`
	Total     Micros      `json:"total_micros"`
	Change    Micros      `json:"change_micros"`
	ChangeBP  int64       `json:"change_bp"`
	Direction string      `json:"direction"`
	Rows      []BridgeRow `json:"rows"`
	Other     *BridgeRow  `json:"other,omitempty"`
	Up        Micros      `json:"up_micros"`
	Down      Micros      `json:"down_micros"`
	Steady    bool        `json:"steady"`
	// Subtitle notes: "Ups and downs offset (…)" or the steady line.
	OffsetNote string `json:"offset_note,omitempty"`
	SteadyNote string `json:"steady_note,omitempty"`
	// ResourceNote says why rows name no resource although resource-level
	// data is on: the resource lines were too many to read for the period.
	ResourceNote  string     `json:"resource_note,omitempty"`
	MinDriver     Micros     `json:"min_driver_micros"`
	OwnerColumn   ColumnRule `json:"owner_column"`
	AccountColumn ColumnRule `json:"account_column"`
}

// BridgeView is GET …/finops/bridge.
type BridgeView struct {
	Header
	BridgeBody
}

// TrendSeries is one stacked series.
type TrendSeries struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Values []Micros `json:"values_micros"` // one per day in days
}

// TrendPin marks a change on the trend.
type TrendPin struct {
	Day        Date   `json:"day"`
	Label      string `json:"label"`
	IncreaseID string `json:"increase_id,omitempty"`
}

// TrendBand shades an increase.
type TrendBand struct {
	IncreaseID string `json:"increase_id"`
	Start      Date   `json:"start"`
	End        Date   `json:"end"`
}

// TrendView is GET …/finops/trend.
type TrendView struct {
	Header
	GroupBy     string        `json:"group_by"`
	Days        []Date        `json:"days"`
	PartialDays []Date        `json:"partial_days,omitempty"`
	Series      []TrendSeries `json:"series"`
	Totals      []Micros      `json:"totals_micros"`
	Pins        []TrendPin    `json:"pins"`
	Bands       []TrendBand   `json:"bands"`
	// BudgetPace is the cumulative budget per day; nil without a budget.
	BudgetPace []Micros `json:"budget_pace_micros"`
}

// BreakdownOption says whether a "spend by" option splits the cost.
type BreakdownOption struct {
	GroupBy   string `json:"group_by"`
	Available bool   `json:"available"`
	// Reason is single_value | dominant_value | no_team_tag |
	// tag_not_active | same_tag | no_groups.
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// BreakdownRow is one row of a breakdown.
type BreakdownRow struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Amount  Micros  `json:"amount_micros"`
	Base    *Micros `json:"base_micros,omitempty"`
	Change  *Micros `json:"change_micros,omitempty"`
	ShareBP int64   `json:"share_bp"`
	Owner   *Owner  `json:"owner,omitempty"`
	// Estimated marks a row whose amount rests on estimated team or
	// region shares.
	Estimated bool `json:"estimated,omitempty"`
	// Count is the number of values rolled into an "other" row.
	Count int `json:"count,omitempty"`
	// Inside lists the top usage groups inside a service row; set only
	// when no "spend by" option qualifies.
	Inside []BreakdownRow `json:"inside,omitempty"`
}

// BreakdownBody is the content of a breakdown.
type BreakdownBody struct {
	GroupBy string         `json:"group_by"`
	Total   Micros         `json:"total_micros"`
	Rows    []BreakdownRow `json:"rows"`
	// Options and DefaultGroupBy drive the "Spend by" switch; an empty
	// default means no option qualifies and the card is removed.
	Options        []BreakdownOption `json:"options"`
	DefaultGroupBy string            `json:"default_group_by"`
}

// BreakdownView is GET …/finops/breakdown.
type BreakdownView struct {
	Header
	BreakdownBody
}

// SuspectView is the change that likely started an increase.
type SuspectView struct {
	// State is found | none_found | history_incomplete |
	// collectors_pending | account_not_connected. none_found is used only
	// when the change history covers the whole window.
	State      string    `json:"state"`
	Summary    string    `json:"summary,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	At         time.Time `json:"at,omitzero"`
	Confidence string    `json:"confidence,omitempty"` // likely | possible
	// ConfidenceWord is Likely or Possible.
	ConfidenceWord string `json:"confidence_word,omitempty"`
	// Tied marks one of several changes that fit equally well; Note then
	// says how to tell them apart.
	Tied bool   `json:"tied,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Note string `json:"note,omitempty"`
}

// IncreaseView is one increase in a list or detail.
type IncreaseView struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Sentence     string `json:"sentence"`
	Account      string `json:"account"`
	AccountName  string `json:"account_name"`
	AccountKind  string `json:"account_kind,omitempty"`
	Service      string `json:"service"`
	ServiceLabel string `json:"service_label"`
	UsageType    string `json:"usage_type,omitempty"`
	StartedOn    Date   `json:"started_on"`
	DailyBefore  Micros `json:"daily_before_micros"`
	DailyNow     Micros `json:"daily_now_micros"`
	Monthly      Micros `json:"monthly_impact_micros"`
	PeriodChange Micros `json:"period_change_micros"`
	// StatedChange is the amount the title and sentence state: the
	// period's change, or the increase's own change when it lies outside
	// the period. Zero when the sentence states no amount.
	StatedChange Micros      `json:"stated_change_micros"`
	Cause        string      `json:"cause"`
	Owner        Owner       `json:"owner"`
	Suspect      SuspectView `json:"suspect_change"`
	// Suspects ranks up to three candidate changes; the first is
	// suspect_change.
	Suspects   []SuspectView `json:"suspects"`
	Alert      *AlertView    `json:"alert,omitempty"`
	Status     string        `json:"status"`
	Expected   *Expected     `json:"expected,omitempty"`
	AlertedAt  *time.Time    `json:"alerted_at,omitempty"`
	ResolvedOn Date          `json:"resolved_on,omitzero"`
}

// IncreasesView is GET …/finops/increases.
type IncreasesView struct {
	Header
	Status       string         `json:"status"`
	Items        []IncreaseView `json:"items"`
	Learning     bool           `json:"learning"`
	LearningNote string         `json:"learning_note,omitempty"`
}

// CauseNode is one usage type of an increase with its resources.
type CauseNode struct {
	UsageType string         `json:"usage_type"`
	Label     string         `json:"label"`
	Amount    Micros         `json:"amount_micros"`
	Resources []ResourceCost `json:"resources,omitempty"`
}

// ResourceCost is one resource's change under a usage type.
type ResourceCost struct {
	ID     string `json:"id"`
	Amount Micros `json:"amount_micros"`
}

// UsageSignal backs a cause with a usage metric (before → after).
type UsageSignal struct {
	Label  string `json:"label"`
	Before int64  `json:"before_micros"`
	After  int64  `json:"after_micros"`
	Unit   string `json:"unit"`
}

// IncreaseDetailView is GET …/finops/increases/{id}.
type IncreaseDetailView struct {
	Header
	Increase IncreaseView `json:"increase"`
	// Series runs from the baseline start to the last complete day.
	Series    []DayAmount `json:"series"`
	CauseTree []CauseNode `json:"cause_tree"`
	// ResourceData is "available", "off", or "too_large" when the resource
	// lines of the increase's account and service were more than one read
	// holds; ResourceNote explains how to turn it on or why none are named.
	ResourceData   string        `json:"resource_data"`
	ResourceNote   string        `json:"resource_note,omitempty"`
	UsageSignal    *UsageSignal  `json:"usage_signal"`
	RelatedSavings []SavingView  `json:"related_savings"`
	History        []StatusEvent `json:"history"`
}

// SavingView is one savings item with its decision.
type SavingView struct {
	Saving
	Line      string       `json:"line"`
	Owner     Owner        `json:"owner"`
	Status    string       `json:"status"` // open | dismissed
	Dismissed *SavingState `json:"dismissed,omitempty"`
}

// SavingsBody is the content of the savings list.
type SavingsBody struct {
	Total SavingsTotal `json:"total"`
	Items []SavingView `json:"items"`
	// Gaps name the savings checks that could not run.
	Gaps []SavingsGap `json:"gaps"`
}

// SavingsView is GET …/finops/savings.
type SavingsView struct {
	Header
	SavingsBody
}

// BudgetLine is one budget with its status.
type BudgetLine struct {
	Budget
	ReadOnly bool   `json:"read_only"`
	Actual   Micros `json:"actual_micros"`
	Forecast Micros `json:"forecast_micros"`
	// ForecastSource is provider | estimate | none.
	ForecastSource string `json:"forecast_source"`
	// ForecastDiff is forecast − budget (positive: over).
	ForecastDiff Micros `json:"forecast_diff_micros"`
	Status       string `json:"status"`
	StatusWord   string `json:"status_word"`
	// Estimated marks actuals and forecast that rest on proportional team
	// or region split cells (team budgets when team totals are not exact
	// per account, or a region filter over split regions). It is never
	// derived from the provider's not-final day flag.
	Estimated bool `json:"estimated,omitempty"`
}

// BudgetView is GET …/finops/budgets.
type BudgetView struct {
	Header
	Forecast Forecast     `json:"forecast"`
	Budgets  []BudgetLine `json:"budgets"`
	// Skipped names provider budgets left out and why.
	Skipped []SkippedBudget `json:"skipped,omitempty"`
}

// TopDriver is an account's largest driver.
type TopDriver struct {
	Label  string `json:"label"`
	Amount Micros `json:"amount_micros"`
}

// AccountRow is one row of the Accounts tab.
type AccountRow struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`   // connected | cost_only
	Status    string     `json:"status"` // ok | not_readable | new | closed
	ErrorCode string     `json:"error_code,omitempty"`
	Group     string     `json:"group,omitempty"`
	Cost      Micros     `json:"cost_micros"`
	Change    *Micros    `json:"change_micros"`
	TopDriver *TopDriver `json:"top_driver"`
	TopTeam   string     `json:"top_team,omitempty"`
	// TopTeamEstimated marks a top team that rests on estimated shares.
	TopTeamEstimated bool `json:"top_team_estimated,omitempty"`
	// Sparkline is the last 30 complete days.
	Sparkline []Micros `json:"sparkline_micros"`
	FirstCost Date     `json:"first_cost,omitzero"`
	ClosedOn  Date     `json:"closed_on,omitzero"`
}

// AccountGroupRow subtotals an account group.
type AccountGroupRow struct {
	Name   string  `json:"name"`
	Cost   Micros  `json:"cost_micros"`
	Change *Micros `json:"change_micros"`
	Count  int     `json:"count"`
}

// AccountsView is GET …/finops/accounts.
type AccountsView struct {
	Header
	Total    Micros            `json:"total_micros"`
	Accounts []AccountRow      `json:"accounts"`
	Groups   []AccountGroupRow `json:"groups"`
}

// Frozen marks a saved monthly report.
type Frozen struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// ReportNotes is the notes block of the report.
type ReportNotes struct {
	Expected []IncreaseView `json:"expected"`
	Coverage CoverageNote   `json:"coverage"`
	Missing  []string       `json:"missing"`
}

// CoverageNote says what the numbers include.
type CoverageNote struct {
	Accounts        string `json:"accounts"` // "3 accounts" or "2 of 3 accounts"
	LastCompleteDay Date   `json:"last_complete_day"`
	MeasureLabel    string `json:"measure_label"`
}

// ReportSavings is the savings block.
type ReportSavings struct {
	Total SavingsTotal `json:"total"`
	Top   []SavingView `json:"top"`
}

// ReportView is GET …/finops/report and the frozen monthly report.
type ReportView struct {
	Header
	GeneratedAt time.Time `json:"generated_at"`
	Frozen      *Frozen   `json:"frozen"`
	// CanFreeze is false before the bill closes; FreezeNote says why.
	CanFreeze  bool   `json:"can_freeze"`
	FreezeNote string `json:"freeze_note,omitempty"`
	// NotFinal is true while the provider may still change the month's
	// bill (an open month, or before the bill closes on the 5th).
	NotFinal       bool           `json:"not_final"`
	Summary        SummaryBody    `json:"summary"`
	Bridge         BridgeBody     `json:"bridge"`
	SpendByService []BreakdownRow `json:"spend_by_service"`
	SpendByAccount []BreakdownRow `json:"spend_by_account"`
	// SpendByTeam is nil unless team tags split the cost.
	SpendByTeam []BreakdownRow `json:"spend_by_team"`
	Savings     ReportSavings  `json:"savings"`
	Notes       ReportNotes    `json:"notes"`
	CopySummary string         `json:"copy_summary"`
}

// ReportRef lists a frozen report.
type ReportRef struct {
	Month  string `json:"month"`
	Frozen Frozen `json:"frozen"`
	Total  Micros `json:"total_micros"`
}
