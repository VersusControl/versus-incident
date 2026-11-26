package callcost

import (
	"encoding/json"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// The sizing example of a large account, priced by hand: 14,000 calls a
// day, 600 of them metered S3 bucket-configuration reads, GuardDuty on and
// two trails that record read events, so one copy after the free one.
//
//	month events   14,000 × 30                    = 420,000
//	GuardDuty      420,000 × $4.00 / 1,000,000     = $1.68
//	extra copy     420,000 × $2.00 / 100,000 × 1   = $8.40
//	direct         600 × 30 × $0.0004 / 1,000      = $0.0072, $0.01 rounded
//	volume         420,000 × 1,500 bytes           = 0.63 GB
func TestEstimateMatchesTheSizingExampleByHand(t *testing.T) {
	f := newCostFixture(t)
	for _, row := range []struct {
		region, source string
		calls, metered int64
		runs           int64
	}{
		{"us-east-1", "elbv2", 5000, 0, 144},
		{"us-east-1", "lambda", 5400, 0, 4},
		{"us-east-1", "cloudtrail", 1700, 0, 288},
		{"us-east-1", "ec2", 1060, 0, 144},
		{model.GlobalRegion, "iam", 240, 0, 24},
		{model.GlobalRegion, "s3-policy", 600, 600, 4},
	} {
		f.add(t, quietAccount, row.region, row.source, 2, row.calls, row.metered, row.runs)
	}
	f.trails(t, quietAccount, "us-east-1", collect.ReadEventsYes, collect.ReadEventsYes)
	view := f.read(t, []string{quietAccount}, func(string) (bool, bool) { return true, true })
	if view.Calls24h != 14000 || view.Events24h != 14000 || view.Metered24h != 600 {
		t.Fatalf("24 h = %d calls, %d events, %d metered", view.Calls24h, view.Events24h, view.Metered24h)
	}
	estimate := view.EstimateMonth
	if estimate.GuardDutyUSD == nil || !near(*estimate.GuardDutyUSD, 1.68) {
		t.Errorf("GuardDuty = %v, want 1.68", estimate.GuardDutyUSD)
	}
	if estimate.ExtraTrailCopiesUSD == nil || !near(*estimate.ExtraTrailCopiesUSD, 8.40) || estimate.ExtraTrailCopies == nil || *estimate.ExtraTrailCopies != 1 {
		t.Errorf("extra copies = %v (%v), want 8.40 for 1 copy", estimate.ExtraTrailCopiesUSD, estimate.ExtraTrailCopies)
	}
	if !near(estimate.DirectUSD, 0.01) {
		t.Errorf("direct = %v, want 0.01", estimate.DirectUSD)
	}
	if !near(estimate.LakeGB, 0.63) || !near(estimate.SIEMGB, 0.63) {
		t.Errorf("volume = %v / %v GB, want 0.63", estimate.LakeGB, estimate.SIEMGB)
	}
	// Under the 20,000 target nothing is stretched.
	if len(view.Stretched) != 0 {
		t.Errorf("stretched under the target: %+v", view.Stretched)
	}

	// One trail, the free copy: the extra line is a known zero.
	g := newCostFixture(t)
	g.add(t, quietAccount, "us-east-1", "elbv2", 1, 5000, 0, 144)
	g.trails(t, quietAccount, "us-east-1", collect.ReadEventsYes)
	one := g.read(t, []string{quietAccount}, func(string) (bool, bool) { return false, true })
	if one.EstimateMonth.ExtraTrailCopiesUSD == nil || *one.EstimateMonth.ExtraTrailCopiesUSD != 0 ||
		one.EstimateMonth.GuardDutyUSD == nil || *one.EstimateMonth.GuardDutyUSD != 0 {
		t.Fatalf("one trail, GuardDuty off = %+v", one.EstimateMonth)
	}
}

// A trails read that failed (the selectors or the trail list denied) never
// prices the extra copies as zero: the line stays null in the JSON the
// Settings card reads, while a known zero is a number.
func TestFailedTrailsReadIsNullNeverZero(t *testing.T) {
	f := newCostFixture(t)
	f.add(t, quietAccount, "us-east-1", "elbv2", 1, 5000, 0, 144)
	if err := f.snapshots.RecordFailure(model.Unit{Account: quietAccount, Region: "us-east-1", Service: collect.TrailsService}, "access_denied"); err != nil {
		t.Fatal(err)
	}
	view := f.read(t, []string{quietAccount}, nil)
	encoded, err := json.Marshal(view.EstimateMonth)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"extra_trail_copies_usd", "extra_trail_copies"} {
		if string(fields[key]) != "null" {
			t.Errorf("%s = %s, want null", key, fields[key])
		}
	}
	// No GuardDuty status source: the amount if it is on, never a silent
	// zero, and marked not known.
	if string(fields["guardduty_usd"]) == "0" || string(fields["guardduty_usd"]) == "null" || string(fields["guardduty_known"]) != "false" {
		t.Errorf("guardduty = %s (known %s), want the amount if on, not known", fields["guardduty_usd"], fields["guardduty_known"])
	}

	// The same account once its trails read: a number, zero for one copy.
	f.trails(t, quietAccount, "us-east-1", collect.ReadEventsYes)
	known := f.read(t, []string{quietAccount}, func(string) (bool, bool) { return false, true })
	encoded, err = json.Marshal(known.EstimateMonth)
	if err != nil {
		t.Fatal(err)
	}
	fields = nil
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["extra_trail_copies_usd"]) != "0" || string(fields["guardduty_usd"]) != "0" {
		t.Fatalf("known zero lines = %s", encoded)
	}
}
