package narrative

import (
	"strconv"
	"strings"
	"time"
)

// Answer holds the computed facts behind the one-line answer used by
// Copy summary, the report, Slack and alerts.
type Answer struct {
	Currency    string
	PeriodLabel string // "October", "Last 30 days"
	Open        bool   // the period still has days to come
	Total       int64

	HasBase   bool
	BaseLabel string // "September"
	Change    int64
	ChangeBP  int64

	// Budget compares the period with a deployment budget. For an open
	// period it compares the forecast when one exists.
	HasBudget      bool
	Budget         int64
	HasForecast    bool
	Forecast       int64
	ForecastIsEst  bool
	ForecastPeriod string // "November"

	// Top is the largest driver in the direction of the change.
	Top *DriverPhrase
	// TopShareBP is the share of the change the top driver explains.
	TopShareBP int64

	AccountsReadable int
	AccountsTotal    int
}

// DriverPhrase describes one driver in words.
type DriverPhrase struct {
	Cause       string
	RecordType  string
	Amount      int64
	ServiceKey  string
	ServiceName string // provider display name (Details)
	UsageType   string // Details only
	Resource    string // resource name or ID
	AccountName string
	Owner       string // owning team; "" when unallocated
	// OwnerEstimated marks an owner from an estimated share.
	OwnerEstimated bool
	Since          time.Time
	ItemLabel      string // one-off item ("annual support fee")
	OffsetsUp      int64  // for the "other" row
	OffsetsCount   int
}

// AnswerLine renders the answer. It drops any clause it cannot back.
func AnswerLine(a Answer, d Depth) string {
	var b strings.Builder
	label := a.PeriodLabel
	if label == "" {
		label = "This period"
	}
	b.WriteString(label)
	if a.Open {
		b.WriteString(" cost so far ")
	} else {
		b.WriteString(" cost ")
	}
	b.WriteString(Money(a.Total, a.Currency))
	if a.AccountsTotal > 0 && a.AccountsReadable < a.AccountsTotal {
		b.WriteString(" (" + AccountsNote(a.AccountsReadable, a.AccountsTotal) + ")")
	}
	switch {
	case a.HasBudget && !a.Open:
		b.WriteString(", " + budgetClause(a.Total, a.Budget, a.Currency))
	case a.HasBudget && a.Open && a.HasForecast:
		fc := "forecast"
		if a.ForecastIsEst {
			fc = "estimate"
		}
		b.WriteString("; " + fc + " " + Money(a.Forecast, a.Currency) + ", " +
			budgetClause(a.Forecast, a.Budget, a.Currency))
	case a.HasBase:
		dir := DirectionOf(a.ChangeBP)
		if dir == Same {
			b.WriteString(", about the same as " + a.BaseLabel)
		} else {
			b.WriteString(", " + dir + " " + Money(abs(a.Change), a.Currency) +
				" (" + SignedPercent(a.ChangeBP) + ") from " + a.BaseLabel)
		}
	}
	b.WriteString(".")
	if a.HasBase && a.Top != nil {
		dir := DirectionOf(a.ChangeBP)
		switch dir {
		case Same:
			b.WriteString(" Ups and downs offset.")
		default:
			noun := "increase"
			if dir == Down {
				noun = "decrease"
			}
			lead := "The largest " + noun + " is "
			if a.TopShareBP > 5_000 {
				lead = "Most of the " + noun + " is "
			}
			b.WriteString(" " + lead + topSubject(*a.Top, d) + ".")
		}
	}
	return b.String()
}

func budgetClause(amount, budget int64, cur string) string {
	diff := amount - budget
	switch {
	case diff > 0:
		return Money(diff, cur) + " over the " + Money(budget, cur) + " budget"
	case diff < 0:
		return Money(-diff, cur) + " under the " + Money(budget, cur) + " budget"
	}
	return "exactly the " + Money(budget, cur) + " budget"
}

func topSubject(p DriverPhrase, d Depth) string {
	s := serviceSubject(p, d)
	if d == Summary {
		s = inlineService(p.ServiceKey, s)
	}
	if p.Owner != "" && p.OwnerEstimated {
		s += " likely in " + p.Owner + " " + EstimatedMark
	} else if p.Owner != "" {
		s += " in " + p.Owner
	} else if p.AccountName != "" {
		s += " in " + p.AccountName
	}
	if !p.Since.IsZero() {
		s += " since " + ShortDate(p.Since)
	}
	return s
}

func serviceSubject(p DriverPhrase, d Depth) string {
	if d == Details {
		s := ServiceLabel(p.ServiceKey, p.ServiceName, Details)
		if p.UsageType != "" {
			s += " / " + p.UsageType
		}
		return s
	}
	return ServiceLabel(p.ServiceKey, p.ServiceName, Summary)
}

// inlineService lowercases a plain service label for use inside a sentence.
// Provider names (unknown keys) keep their own casing.
func inlineService(key, label string) string {
	if KnownService(key) {
		return lowerFirst(label)
	}
	return label
}

// DriverLine is the label of one bridge row ("Used more: network traffic
// (NAT) through nat-1a").
func DriverLine(p DriverPhrase, d Depth) string {
	sign := 0
	if p.Amount > 0 {
		sign = 1
	} else if p.Amount < 0 {
		sign = -1
	}
	label := CauseLabel(p.Cause, sign, p.RecordType, d)
	switch p.Cause {
	case CauseOther:
		if d == Details {
			return label + " (" + strconv.Itoa(p.OffsetsCount) + " lines under the driver minimum)"
		}
		return label
	case CauseNewAccount:
		s := label + ": " + p.AccountName
		if !p.Since.IsZero() {
			s += " (first cost " + ShortDate(p.Since) + ")"
		}
		return s
	case CauseOneOff:
		if p.ItemLabel != "" && d == Summary {
			return label + ": " + p.ItemLabel
		}
	case CauseDiscountChange:
		if d == Summary && p.RecordType == "credit" && (p.ServiceKey == "billing.credit" || p.ServiceKey == "") {
			return label
		}
	}
	subject := serviceSubject(p, d)
	if d == Summary {
		subject = inlineService(p.ServiceKey, subject)
	}
	if p.Resource != "" {
		if d == Summary {
			subject += " through " + p.Resource
		} else {
			subject += " / " + p.Resource
		}
	}
	s := label + ": " + subject
	if p.Cause == CauseNewResource && !p.Since.IsZero() {
		s += " (from " + ShortDate(p.Since) + ")"
	}
	return s
}

// Increase holds the facts of one increase.
type Increase struct {
	Currency    string
	ServiceKey  string
	ServiceName string
	UsageType   string
	Resource    string
	AccountName string
	AccountID   string
	PeriodWord  string // "this month", "in October"
	Amount      int64  // change in the period, or over the increase's own days when Window is set
	DailyBefore int64
	DailyNow    int64
	StartedOn   time.Time
	// Window marks Amount as the change over the increase's own days
	// (WindowDays of them) rather than over the period: used when the
	// increase lies outside the period being read.
	Window     bool
	WindowDays int
	// ResolvedOn is set once the line is back inside its normal band.
	ResolvedOn time.Time

	// SuspectState is "found", "none_found", "history_incomplete" or
	// "collectors_pending".
	SuspectState   string
	SuspectSummary string // "the checkout-api release"
	SuspectActor   string // "github-actions-prod"
	// SuspectsTied counts changes that fit equally well; with two or more
	// the sentence names none of them.
	SuspectsTied int

	Owner     string
	OwnerRule string // tag | account | unallocated
	// OwnerEstimated marks an owner from an estimated share: the sentence
	// says "likely", never states it as fact.
	OwnerEstimated bool
	TagKey         string

	SavingAction  string // "Adding VPC endpoints"
	SavingMonthly int64
}

// IncreaseSentence explains one increase in two to four sentences.
func IncreaseSentence(in Increase, d Depth) string {
	var parts []string
	where := in.AccountName
	if d == Details && in.AccountID != "" {
		if where != "" {
			where += " (" + in.AccountID + ")"
		} else {
			where = in.AccountID
		}
	}
	subject := serviceSubject(DriverPhrase{ServiceKey: in.ServiceKey, ServiceName: in.ServiceName, UsageType: in.UsageType}, d)
	if d == Details && in.Resource != "" {
		subject += " / " + in.Resource
	}
	first := subject
	if where != "" {
		first += " in " + where
	}
	first += " " + IncreaseChange(in)
	if d == Details {
		first += " (daily " + Money(in.DailyBefore, in.Currency) + " → " + Money(in.DailyNow, in.Currency) + ")"
	}
	parts = append(parts, first+".")

	if !in.StartedOn.IsZero() {
		started := "It started on " + ShortDate(in.StartedOn)
		switch in.SuspectState {
		case "found":
			if in.SuspectsTied > 1 {
				started += "; " + strconv.Itoa(in.SuspectsTied) + " changes in the 48 h before fit equally well"
				break
			}
			if in.SuspectSummary != "" {
				started += ", after " + in.SuspectSummary
				if in.SuspectActor != "" {
					started += " by " + in.SuspectActor
				}
			}
		case "none_found":
			started += "; no change found in the 48 h before"
		case "history_incomplete":
			started += "; change history is incomplete for that window"
		}
		parts = append(parts, started+".")
	}

	switch {
	case in.OwnerRule == "tag" && in.OwnerEstimated:
		parts = append(parts, "Likely the "+in.Owner+" team owns it "+EstimatedMark+".")
	case in.OwnerRule == "tag":
		parts = append(parts, capitalize(in.Owner)+" team owns it.")
	case in.OwnerRule == "account" && in.OwnerEstimated:
		parts = append(parts, "Likely "+in.Owner+" owns it as the account owner "+EstimatedMark+".")
	case in.OwnerRule == "account":
		parts = append(parts, capitalize(in.Owner)+" owns it as the account owner.")
	case in.OwnerEstimated:
		parts = append(parts, "Likely no team owns most of it yet "+EstimatedMark+".")
	default:
		parts = append(parts, "No team owns it yet ("+OwnerRuleText("unallocated", in.TagKey, "", "", d)+").")
	}

	if in.SavingAction != "" && in.SavingMonthly > 0 {
		parts = append(parts, capitalize(in.SavingAction)+" would save about "+Money(in.SavingMonthly, in.Currency)+" a month.")
	}
	return strings.Join(parts, " ")
}

// Saving holds one savings item.
type Saving struct {
	Currency string
	Title    string // "Add VPC endpoints for S3 and ECR in vpc-prod"
	Monthly  int64
	Effort   string
	Risk     string
	// UpTo marks the amount as an upper bound; BeforeOtherSavings marks a
	// commitment computed before the other savings.
	UpTo               bool
	BeforeOtherSavings bool
}

// SavingLine renders one savings item.
func SavingLine(s Saving, d Depth) string {
	amount := "saves about " + Money(s.Monthly, s.Currency) + " a month"
	switch {
	case s.BeforeOtherSavings:
		amount = "saves up to " + Money(s.Monthly, s.Currency) + " a month, before other savings"
	case s.UpTo:
		amount = "saves up to " + Money(s.Monthly, s.Currency) + " a month"
	}
	return s.Title + ": " + amount + " (effort " + EffortWord(s.Effort) + ", risk " + RiskWord(s.Risk) + ")."
}

// CopySummary is the plain text for Slack or email: the answer, the top
// three drivers and the savings total.
func CopySummary(answer string, drivers []string, savingsTotal int64, savingsCount int, currency string) string {
	var b strings.Builder
	b.WriteString(answer)
	if len(drivers) > 0 {
		b.WriteString("\n\nWhat moved the cost:")
		for i, l := range drivers {
			if i == 3 {
				break
			}
			b.WriteString("\n- " + l)
		}
	}
	if savingsCount > 0 {
		b.WriteString("\n\nSavings found: about " + Money(savingsTotal, currency) + " a month across " +
			plural(savingsCount, "item", "items") + ".")
	}
	return b.String()
}

// AccountsNote names partial coverage ("2 of 3 accounts").
func AccountsNote(readable, total int) string {
	return strconv.Itoa(readable) + " of " + plural(total, "account", "accounts")
}

// AccountsCount names full coverage ("3 accounts").
func AccountsCount(total int) string { return plural(total, "account", "accounts") }

// ResourceDetailTooLargeNote says why resources are not named: the
// resource-level lines behind them are more than one view reads at once.
// The amounts never depend on them.
func ResourceDetailTooLargeNote() string {
	return "Resource detail is too large to read for this period, so resources are not named; the amounts are complete"
}

// ResourceDataOffNote explains how to see resources under a usage type.
func ResourceDataOffNote(provider string) string {
	if provider == "" {
		provider = "the provider"
	}
	return "Turn on resource-level data in " + provider + " to see resources (" + provider + " charges for it)"
}

// SkippedNote names paid calls skipped to stay under the daily cap.
func SkippedNote(capRequests int, calls []string) string {
	if len(calls) == 0 {
		return ""
	}
	labels := make([]string, len(calls))
	for i, c := range calls {
		labels[i] = CallLabel(c)
	}
	return "Skipped today to stay under " + plural(capRequests, "request", "requests") + ": " + strings.Join(labels, ", ")
}

// LearningNote explains why increases are not detected yet.
func LearningNote(baselineDays int) string {
	return "Learning: increases start after " + strconv.Itoa(baselineDays) + " days"
}

// SuspectNote explains a missing suspect change.
func SuspectNote(state string) string {
	switch state {
	case "none_found":
		return "No change found in the 48 h before"
	case "history_incomplete":
		return "Change history incomplete for that window"
	case "collectors_pending":
		return "Change history arrives with collectors"
	}
	return ""
}

// BillOpenNote warns that the previous month can still change.
func BillOpenNote(month time.Month, provider string, closeDay int) string {
	if provider == "" {
		provider = "the provider"
	}
	return MonthName(month) + " may still change until " + provider + " closes the bill on the " + ordinal(closeDay)
}

// ForecastNote explains the forecast source.
func ForecastNote(source string, runRateDays int) string {
	switch source {
	case "estimate":
		return "Estimate from the last " + strconv.Itoa(runRateDays) + " days"
	case "none":
		return "Not enough history"
	}
	return ""
}

// NoBaseNote explains a missing comparison.
func NoBaseNote(firstFull time.Month) string {
	return "Compare from " + firstFull.String()[:3] + " (first full month)"
}

// SteadyNote is the single bridge line when no driver passes the minimum.
func SteadyNote(minDriver int64, currency string) string {
	return "No driver above " + Money(minDriver, currency) + "; the cost was steady"
}

// OffsetNote is the bridge subtitle addition when ups and downs cancel out.
func OffsetNote(up, down int64, currency string) string {
	return "Ups and downs offset (" + SignedMoney(up, currency) + " / " + SignedMoney(-abs(down), currency) + ")"
}

// IncreaseChange is the clause that states what an increase did, scoped
// either to the period ("went up $1,980 in October") or, when the increase
// lies outside the period, to its own days ("went up $44 over 8 days").
// A resolved increase says so; an amount that does not print as positive
// (under half a cent) is dropped rather than stated as "$0".
func IncreaseChange(in Increase) string {
	s := "went up"
	if ShowsPositive(in.Amount) {
		s += " " + Money(in.Amount, in.Currency)
	}
	resolved := !in.ResolvedOn.IsZero()
	if in.Window {
		if in.WindowDays > 0 {
			s += " over " + plural(in.WindowDays, "day", "days")
		}
		if resolved {
			return s + " before it resolved on " + ShortDate(in.ResolvedOn)
		}
		return s + " so far"
	}
	period := in.PeriodWord
	if period == "" {
		period = "this month"
	}
	s += " " + period
	if resolved {
		s += " and resolved on " + ShortDate(in.ResolvedOn)
	}
	return s
}

// IncreaseTitle is the trend-pin label ("went up $1,980 a month").
func IncreaseTitle(amount int64, currency, periodWord string) string {
	if periodWord == "" {
		periodWord = "this month"
	}
	return "went up " + Money(amount, currency) + " " + periodWord
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// JoinList is exported for callers composing titles ("S3 and ECR").
func JoinList(items []string) string { return joinList(items) }
