package issues

import "testing"

// The summary counts open, acknowledged and regressed issues; rejected
// issues count only as rejected, resolved ones not at all.
func TestSummaryCountsRejectedApart(t *testing.T) {
	list := []Issue{
		{State: StateOpen, Severity: SeverityCritical, Tier: TierActNow},
		{State: StateAcknowledged, Severity: SeverityHigh, Tier: TierActNow},
		{State: StateRegressed, Severity: SeverityMedium, Tier: TierWorthALook},
		{State: StateRejected, Severity: SeverityCritical, Tier: TierActNow},
		{State: StateRejected, Severity: SeverityLow, Tier: TierWorthALook},
		{State: StateResolved, Severity: SeverityCritical, Tier: TierActNow},
	}
	got := Summarize(list, Coverage{AccountsRead: 3, AccountsTotal: 4})
	want := Summary{Open: 3, Critical: 1, High: 1, WorthALook: 1, AccountsRead: 3, AccountsTotal: 4, RejectedCount: 2}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}
