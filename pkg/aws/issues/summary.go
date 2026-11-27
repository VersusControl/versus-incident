package issues

import "github.com/VersusControl/versus-incident/pkg/storage"

// Coverage is how many accounts the issues were read from.
type Coverage struct {
	AccountsRead  int
	AccountsTotal int
}

// Summary is the one count every reader shows: the Issues page, Overview,
// the PDF report and the agent tools. Rejected issues are out of every
// count but RejectedCount; resolved issues are out of all of them.
type Summary struct {
	// Open counts open, acknowledged and regressed issues.
	Open     int `json:"open"`
	Critical int `json:"critical"`
	High     int `json:"high"`
	// WorthALook counts the open issues in the worth-a-look tier.
	WorthALook    int `json:"worth_a_look"`
	AccountsRead  int `json:"accounts_read"`
	AccountsTotal int `json:"accounts_total"`
	RejectedCount int `json:"rejected_count"`
}

// ActNow counts the open issues in the act-now tier: every open issue
// that is not worth a look.
func (summary Summary) ActNow() int { return summary.Open - summary.WorthALook }

// Summarize counts list. Callers pass every issue their filters select;
// Summarize alone decides what counts.
func Summarize(list []Issue, coverage Coverage) Summary {
	summary := Summary{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal}
	for _, issue := range list {
		if issue.State == StateRejected {
			summary.RejectedCount++
			continue
		}
		if !issue.State.Active() {
			continue
		}
		summary.Open++
		switch issue.Severity {
		case SeverityCritical:
			summary.Critical++
		case SeverityHigh:
			summary.High++
		}
		if issue.Tier == TierWorthALook {
			summary.WorthALook++
		}
	}
	return summary
}

// SummarizeCounts is Summarize over the counts the store keeps per state,
// severity and tier, so a summary never needs every issue read.
func SummarizeCounts(counts []storage.AWSIssueCount, coverage Coverage) Summary {
	summary := Summary{AccountsRead: coverage.AccountsRead, AccountsTotal: coverage.AccountsTotal}
	for _, count := range counts {
		state := State(count.State)
		if state == StateRejected {
			summary.RejectedCount += count.Count
			continue
		}
		if !state.Active() {
			continue
		}
		summary.Open += count.Count
		switch Severity(count.Severity) {
		case SeverityCritical:
			summary.Critical += count.Count
		case SeverityHigh:
			summary.High += count.Count
		}
		if Tier(count.Tier) == TierWorthALook {
			summary.WorthALook += count.Count
		}
	}
	return summary
}
