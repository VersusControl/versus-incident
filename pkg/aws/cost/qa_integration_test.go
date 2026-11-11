package cost

import (
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestResourceDetailRunsForOpenIncreases: once an increase is open, the
// daily refresh reads resource-level cost for its service, so the suspect
// is narrowed to the NAT gateway that billed and the release is Likely even
// with a second NAT and a same-time deploy in the VPC. A payer without
// resource-level data is not asked again for a week.
func TestResourceDetailRunsForOpenIncreases(t *testing.T) {
	sc := loadScenario(t, "cost-nat-spike-two-nats")
	inv := loadInventory(t, sc)
	h := newHarness(t, sc, func(o *Options) { o.Inventory, o.MaxPaidRequests = inv, 40 })
	backend := NewBackend(h.adapter, NewStore(h.provider, h.adapter.cache.scope), func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} })
	h.opts.ResourceServices = func() []string { return backend.OpenIncreaseServices(ctx) }
	if err := h.adapter.Update(h.opts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if h.fake.count(callResources) != 0 {
		t.Fatal("no open increase yet, no resource-level call")
	}
	if _, err := backend.Service().SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	if got := backend.OpenIncreaseServices(ctx); len(got) == 0 || got[0] != "EC2 - Other" {
		t.Fatalf("open increase services %v", got)
	}
	h.clock.set(at("2026-11-10", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if h.fake.count(callResources) == 0 {
		t.Fatal("the daily refresh did not read resource-level cost")
	}
	inc := natIncreaseView(t, backend.Service())
	if inc.Suspect.Ref != "change:evt-checkout-42" || inc.Suspect.Confidence != "likely" || inc.Suspect.Tied || len(inc.Suspects) != 1 {
		t.Fatalf("narrowed suspect %+v", inc.Suspects)
	}
	// Resource-level data off in the payer: one failed call, then a week
	// without asking.
	// A throttled call is a skip for today, not "resource data is off".
	h.fake.failures[callResources] = fakeFailure{Type: "ThrottlingException", Message: "slow down"}
	h.clock.set(at("2026-11-10", 18))
	h.adapter.mu.Lock()
	st := mustStatus(t, h)
	st.ScheduledOn = finops.Date{}
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	h.adapter.mu.Unlock()
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	if ps := mustStatus(t, h).Payers["111111111111"]; !ps.ResourceData || !ps.ResourceFailedOn.IsZero() {
		t.Fatalf("throttling flipped resource data off: %+v", ps)
	}
	h.fake.failures[callResources] = fakeFailure{Type: "AccessDeniedException", Message: "resource data off"}
	h.clock.set(at("2026-11-11", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	h.clock.set(at("2026-11-12", 10))
	est, _ := h.adapter.Estimate(ctx, KindScheduled)
	for _, c := range est.Run {
		if c.Name == callResources {
			t.Fatal("resource-level cost is asked again the next day after it was off")
		}
	}
}

// TestTiedSuspectsAreAllPossibleWithTheResourceHint: without resource-level
// cost the two deploys are listed together, closest first, each Possible,
// with the hint; the sentence names none of them.
func TestTiedSuspectsAreAllPossibleWithTheResourceHint(t *testing.T) {
	_, _, svc := natSpikeScenario(t, "cost-nat-spike-two-nats", func(h *harness, _ *scenario) {
		h.opts.ResourceServices = nil
		if err := h.adapter.Update(h.opts); err != nil {
			t.Fatal(err)
		}
	})
	inc := natIncreaseView(t, svc)
	if len(inc.Suspects) != 2 || inc.Suspects[0].Ref != "change:evt-inventory-7" || inc.Suspects[1].Ref != "change:evt-checkout-42" {
		t.Fatalf("tied order %+v", inc.Suspects)
	}
	for _, s := range inc.Suspects {
		if !s.Tied || s.Confidence != "possible" || !strings.Contains(s.Note, "turn on resource-level data") {
			t.Fatalf("tied suspect %+v", s)
		}
	}
	if !strings.Contains(inc.Sentence, "2 changes in the 48 h before fit equally well") || strings.Contains(inc.Sentence, "after ECS service") {
		t.Fatalf("sentence %q", inc.Sentence)
	}
}

// TestCapDayMeterShowsUsedOverLimit: a first refresh stopped at the cap
// still shows the meter as used/limit.
func TestCapDayMeterShowsUsedOverLimit(t *testing.T) {
	sc := loadScenario(t, "large-organization")
	h := newHarness(t, sc, func(o *Options) {
		o.Payers = []Payer{{AccountID: "999999999999", Organization: true, Credentials: testCreds}}
		o.Connected = nil
	})
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	v, err := finops.NewService(h.adapter.Source(), nil, finops.Config{Now: h.clock.Now, MaxPaidRequests: 20}).Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled || v.Meter.Used != 20 || v.Meter.Cap != 20 {
		t.Fatalf("cap day: enabled %v reason %q meter %+v", v.Enabled, v.Reason, v.Meter)
	}
}

// TestResourceCallSpendsNothingWithoutOpenIncreases: the plan priced call 7,
// but the list is empty when it runs: no request is sent or metered.
func TestResourceCallSpendsNothingWithoutOpenIncreases(t *testing.T) {
	sc := loadScenario(t, "single-account")
	calls := 0
	h := newHarness(t, sc, func(o *Options) {
		o.ResourceServices = func() []string {
			calls++
			if calls == 1 {
				return []string{"EC2 - Other"} // the plan sees one
			}
			return nil // the run sees none
		}
	})
	res, err := h.adapter.Refresh(ctx, KindScheduled)
	if err != nil {
		t.Fatal(err)
	}
	planned := false
	for _, c := range res.Plan.Run {
		planned = planned || c.Name == callResources
	}
	if !planned || h.fake.count(callResources) != 0 || h.adapter.Meter().Used(ctx) != 6 {
		t.Fatalf("planned %v, sent %d, used %d", planned, h.fake.count(callResources), h.adapter.Meter().Used(ctx))
	}
}
