package cost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

func awsConfig(costSource string) config.AWSConnectorConfig {
	return config.AWSConnectorConfig{
		Enable:       true,
		OptionalData: config.AWSOptionalDataConfig{CostExplorer: true},
		Accounts:     []config.AWSAccountConfig{{AccountID: "111111111111", DisplayName: "Production", Owner: "platform"}},
		FinOps: config.AWSFinOpsConfig{
			CostSourceAccount: costSource, MaxPaidRequestsPerDay: 12, TargetCoverage: 0.75, MinIncreaseUSDPerMonth: 300,
			Budgets:       []config.AWSFinOpsBudgetConfig{{Name: "Total", Scope: "deployment", MonthlyUSD: 1000}},
			AccountGroups: []config.AWSFinOpsAccountGroup{{Name: "Prod", Accounts: []string{"111111111111"}}},
		},
	}
}

func TestFromConfigPicksTheCostSource(t *testing.T) {
	targets := []Target{{AccountID: "111111111111", Credentials: testCreds}, {AccountID: "999999999999", Credentials: testCreds}}
	o := FromConfig(awsConfig(""), targets, []string{"Name", "team"}, "prod")
	if len(o.Payers) != 2 || o.Payers[0].Organization || !o.Enabled() || !o.TagAllowlisted || o.MaxPaidRequests != 12 {
		t.Fatalf("standalone payers %+v", o)
	}
	if o.AccountOwners["111111111111"] != "platform" || o.AccountNames["111111111111"] != "Production" {
		t.Fatalf("owners and names %+v", o)
	}
	o = FromConfig(awsConfig("999999999999"), targets, nil, "prod")
	if len(o.Payers) != 1 || !o.Payers[0].Organization || o.Payers[0].AccountID != "999999999999" || o.TagAllowlisted {
		t.Fatalf("organization payer %+v", o.Payers)
	}
	o = FromConfig(awsConfig("888888888888"), targets, nil, "prod")
	if o.Enabled() || o.DisabledReason != ReasonCostSourceNotLinked {
		t.Fatalf("unconnected cost source: %+v", o)
	}
	off := awsConfig("")
	off.OptionalData.CostExplorer = false
	if o := FromConfig(off, targets, nil, "prod"); o.Enabled() || o.DisabledReason != ReasonCostDataOff {
		t.Fatalf("cost data off: %+v", o)
	}
	fc := FinOpsConfig(awsConfig(""))
	if fc.TargetCoverageBP != 7_500 || fc.MinIncrease != finops.USD(300) || fc.MinDriver != finops.USD(50) ||
		len(fc.Budgets) != 1 || fc.Budgets[0].Source != finops.BudgetFromConfig || fc.Budgets[0].Monthly != finops.USD(1000) ||
		fc.AccountGroups["111111111111"] != "Prod" || fc.AccountOwners["111111111111"] != "platform" || fc.UIPath != "/aws/finops" {
		t.Fatalf("finops config %+v", fc)
	}
}

func TestDisabledReasonReachesTheView(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if err := h.adapter.Update(FromConfig(awsConfig("888888888888"), nil, nil, "prod")); err != nil {
		t.Fatal(err)
	}
	v, err := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now}).Summary(ctx, finops.Query{})
	if err != nil || v.Enabled || v.Reason != ReasonCostSourceNotLinked {
		t.Fatalf("summary %+v %v", v.Header, err)
	}
	if h.fake.requests() != 0 {
		t.Fatal("a disabled source never calls AWS")
	}
}

func TestSchedulerRunsOnceADayAfterTheRefreshTime(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	prepared, after := 0, 0
	s := NewScheduler(h.adapter,
		func() (bool, int, int, *time.Location) { return true, 9, 30, time.UTC },
		func(context.Context) error { prepared++; return nil },
		WithClock(h.clock.Now), WithAfterRefresh(func(context.Context, RefreshResult) { after++ }))
	h.clock.set(at("2026-11-09", 9))
	if s.RunDue(ctx) || h.fake.requests() != 0 {
		t.Fatal("nothing runs before the refresh time")
	}
	h.clock.set(at("2026-11-09", 10))
	if !s.RunDue(ctx) || after != 1 {
		t.Fatal("the refresh runs once the time has passed")
	}
	if s.RunDue(ctx) || after != 1 {
		t.Fatal("and only once a day")
	}
	h.clock.set(at("2026-11-10", 10))
	if !s.RunDue(ctx) || prepared != 4 {
		t.Fatalf("next day runs again; options refresh every tick (%d)", prepared)
	}
	off := NewScheduler(h.adapter, func() (bool, int, int, *time.Location) { return false, 0, 0, nil }, nil, WithClock(h.clock.Now))
	h.clock.set(at("2026-11-11", 10))
	if off.RunDue(ctx) {
		t.Fatal("a disabled schedule never refreshes")
	}
	run, cancel := context.WithCancel(ctx)
	s.tick = time.Millisecond
	s.Start(run)
	cancel()
	select {
	case <-s.Stopped():
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

type countingSyncer struct {
	recs   []finops.IncreaseRecord
	marked []string
}

func (c *countingSyncer) SyncIncreases(context.Context) ([]finops.IncreaseRecord, error) {
	var out []finops.IncreaseRecord
	for _, r := range c.recs {
		done := false
		for _, m := range c.marked {
			done = done || m == r.ID
		}
		if !done {
			out = append(out, r)
		}
	}
	return out, nil
}

func (c *countingSyncer) MarkIncreaseAlerted(_ context.Context, id string) error {
	c.marked = append(c.marked, id)
	return nil
}

func TestPumpAlertsEachIncreaseOnceAndRetriesFailures(t *testing.T) {
	syncer := &countingSyncer{recs: []finops.IncreaseRecord{{Detected: finops.Detected{ID: "a"}}, {Detected: finops.Detected{ID: "b"}}}}
	src := NewSignalSource("prod", syncer, func() time.Time { return at("2026-11-09", 10) })
	fail := true
	var sent []string
	sink := func(_ context.Context, sig core.Signal) error {
		if fail {
			return errors.New("slack down")
		}
		sent = append(sent, sig.Fields["increase_id"].(string))
		return nil
	}
	if _, err := PumpAlerts(ctx, src, sink); err == nil || len(syncer.marked) != 0 {
		t.Fatal("a failed send records nothing")
	}
	fail = false
	if n, err := PumpAlerts(ctx, src, sink); err != nil || n != 2 || len(syncer.marked) != 2 {
		t.Fatalf("sent %d marked %v err %v", n, syncer.marked, err)
	}
	if n, _ := PumpAlerts(ctx, src, sink); n != 0 || len(sent) != 2 {
		t.Fatal("each increase is alerted once")
	}
	if ServiceFunc(func() *finops.Service { return finops.NewService(nil, nil, finops.Config{}) }) == nil {
		t.Fatal("service func")
	}
}

// The Name tag's values may be shown only when "Name" is on the tag-value
// allowlist, matched exactly like every other tag key.
func TestFromConfigAllowsNamesOnlyWithTheNameTagAllowlisted(t *testing.T) {
	targets := []Target{{AccountID: "111111111111", Credentials: testCreds}}
	for _, c := range []struct {
		allowlist []string
		want      bool
	}{
		{[]string{"Name", "team"}, true},
		{[]string{"team", "env"}, false},
		{[]string{"name"}, false},
		{nil, false},
	} {
		if got := FromConfig(awsConfig(""), targets, c.allowlist, "prod").NameAllowlisted; got != c.want {
			t.Errorf("allowlist %v: NameAllowlisted = %v, want %v", c.allowlist, got, c.want)
		}
	}
}

// The team tag's values are allowlisted only by its exact key: the team tag
// key setting ("team") is not matched by "Team".
func TestFromConfigMatchesTheTeamTagKeyExactly(t *testing.T) {
	targets := []Target{{AccountID: "111111111111", Credentials: testCreds}}
	if FromConfig(awsConfig(""), targets, []string{"Team"}, "prod").TagAllowlisted {
		t.Fatal(`"Team" must not allowlist the "team" tag`)
	}
	if !FromConfig(awsConfig(""), targets, []string{" team "}, "prod").TagAllowlisted {
		t.Fatal(`"team" allowlists the "team" tag`)
	}
}
