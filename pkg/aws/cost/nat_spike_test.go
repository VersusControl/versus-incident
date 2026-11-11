package cost

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// natSpike runs the cost-nat-spike scenario: the first scheduled refresh,
// and the FinOps service with the suspect finder over the change store.
func natSpike(t *testing.T, mutate func(*harness, *scenario)) (*harness, *scenario, *finops.Service) {
	t.Helper()
	return natSpikeScenario(t, "cost-nat-spike", mutate)
}

func natSpikeScenario(t *testing.T, name string, mutate func(*harness, *scenario)) (*harness, *scenario, *finops.Service) {
	t.Helper()
	sc := loadScenario(t, name)
	inv := loadInventory(t, sc)
	h := newHarness(t, sc, func(o *Options) {
		o.Inventory, o.MaxPaidRequests = inv, 40
		// Resource-level cost names the NAT gateway that bills the line.
		o.ResourceServices = func() []string { return []string{"EC2 - Other"} }
	})
	if mutate != nil {
		mutate(h, sc)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend(h.adapter, NewStore(h.provider, h.adapter.cache.scope), func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} })
	svc := backend.Service()
	return h, sc, svc
}

func natIncreaseView(t *testing.T, svc *finops.Service) finops.IncreaseView {
	t.Helper()
	// Detection is persisted by the sync cadence, as in production; a view
	// reads what it stored.
	if _, err := svc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Increases(ctx, finops.Query{Period: "2026-10", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list.Items {
		if v.Service == "network.nat" {
			return v
		}
	}
	t.Fatalf("no NAT increase: %+v", list.Items)
	return finops.IncreaseView{}
}

// TestNatSpikeNamesTheCheckoutRelease: the increase names the checkout-api
// release by the pipeline as the likely cause; the unrelated update and the
// failed call are not suspects.
func TestNatSpikeNamesTheCheckoutRelease(t *testing.T) {
	_, _, svc := natSpike(t, nil)
	inc := natIncreaseView(t, svc)
	top := inc.Suspect
	if top.State != "found" || !strings.Contains(top.Summary, "checkout-api") || !strings.Contains(top.Summary, ":42 (was :41)") ||
		top.Actor != "github-actions-prod" || top.Confidence != "likely" || top.ConfidenceWord != "Likely" || top.Ref != "change:evt-checkout-42" {
		t.Fatalf("suspect %+v", top)
	}
	if !top.At.Equal(time.Date(2026, 10, 20, 14, 2, 0, 0, time.UTC)) {
		t.Fatalf("suspect time %v", top.At)
	}
	// The inventory app deploys at the same time behind nat-1b; the line is
	// nat-1a, so it is not a suspect.
	if len(inc.Suspects) != 1 || inc.Suspects[0].Ref != top.Ref {
		t.Fatalf("ranked suspects %+v", inc.Suspects)
	}
	if !strings.Contains(inc.Sentence, "after ECS service checkout-api updated to task definition :42 (was :41) by github-actions-prod") {
		t.Fatalf("sentence %q", inc.Sentence)
	}
	detail, err := svc.Increase(ctx, finops.Query{Period: "2026-10"}, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	sig := detail.UsageSignal
	if sig == nil || sig.Unit != "GB/day" || sig.Before != 444_000_000 || sig.After != 1_889_000_000 {
		t.Fatalf("billed usage signal %+v", sig)
	}
}

func TestSuspectStatesNeverGuess(t *testing.T) {
	sc := loadScenario(t, "cost-nat-spike")
	finder := NewSuspectFinder(loadInventory(t, sc))
	nat := finops.Detected{Key: finops.LineKey{Account: "111111111111", Service: "network.nat", UsageType: "USE1-NatGateway-Bytes", RecordType: "usage"},
		ServiceName: "EC2 - Other"}

	// Covered window with nothing related: no change found.
	nat.StartedOn = finops.MustDate("2026-10-05")
	if list, _, err := finder.Suspects(ctx, nat); err != nil || len(list) != 1 || list[0].State != "none_found" {
		t.Fatalf("complete history: %+v %v", list, err)
	}
	// A gap in the window: incomplete, never "no change found".
	gappy := *sc
	gappy.ChangeCoverage = []changes.Interval{{From: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}}
	gapFinder := NewSuspectFinder(loadInventory(t, &gappy))
	if list, _, err := gapFinder.Suspects(ctx, nat); err != nil || list[0].State != "history_incomplete" {
		t.Fatalf("gap: %+v %v", list, err)
	}
	// No collected inventory for the account: collectors pending.
	other := nat
	other.Key.Account = "222222222222"
	if list, _, _ := finder.Suspects(ctx, other); list[0].State != "collectors_pending" {
		t.Fatalf("no inventory: %+v", list)
	}
	// Narrowed to nat-1a by its billed resource, the release is the one
	// Likely suspect.
	nat.StartedOn = finops.MustDate("2026-10-20")
	narrowed := NewSuspectFinder(loadInventory(t, sc)).WithResourceLines(natLine{})
	if s, _, err := narrowed.Suspect(ctx, nat); err != nil || s.Ref != "change:evt-checkout-42" || s.Confidence != "likely" {
		t.Fatalf("top suspect %+v %v", s, err)
	}
	if actorName(model.Actor{Class: model.ActorPipeline}) != "pipeline" || actorName(model.Actor{PrincipalARN: "arn:aws:iam::1:user/ana"}) != "ana" {
		t.Fatal("actor names")
	}
}

// TestSavingsFromInventoryHubAndBill: each savings kind the deployment can
// check comes with an amount basis, effort, risk, owner and a guide; the
// checks that cannot run are named.
func TestSavingsFromInventoryHubAndBill(t *testing.T) {
	_, _, svc := natSpike(t, nil)
	v, err := svc.Savings(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]finops.SavingView{}
	for _, it := range v.Items {
		byTitle[it.Title] = it
		if it.Guide == nil || (it.Guide.ConsolePath == "" && it.Guide.Snippet == "") || it.Effort == "" || it.Risk == "" || it.Basis == "" {
			t.Fatalf("incomplete saving %+v", it)
		}
	}
	want := map[string]string{
		"Remove 1 load balancer with no targets":                                           finops.BasisListPrice,
		"Remove 1 unused public IP address":                                                finops.BasisBilled,
		"Remove 1 unattached disk volume":                                                  finops.BasisProvider,
		"Add private endpoints for S3 and ECR in vpc-prod":                                 finops.BasisBilled,
		"Stop or remove 1 idle machine":                                                    finops.BasisProvider,
		"Raise discount coverage from 71% toward 80%: a 1-year compute commitment of $4/h": finops.BasisProvider,
	}
	for title, basis := range want {
		it, ok := byTitle[title]
		if !ok || it.Basis != basis {
			t.Fatalf("missing %q (%s) in %v", title, basis, keysOf(byTitle))
		}
	}
	path := byTitle["Add private endpoints for S3 and ECR in vpc-prod"]
	if !path.UpTo || !strings.Contains(path.Guide.Snippet, `service_name      = "com.amazonaws.us-east-1.s3"`) || strings.Contains(path.Guide.Snippet, "dynamodb") {
		t.Fatalf("network path %+v", path)
	}
	if lb := byTitle["Remove 1 load balancer with no targets"]; lb.Owner.Name != "platform" || lb.Monthly != loadBalancerMonthlyMicros {
		t.Fatalf("empty load balancer %+v", lb)
	}
	if ip := byTitle["Remove 1 unused public IP address"]; ip.Monthly != 30*120_000 || ip.Count != 1 {
		t.Fatalf("idle IP %+v", ip)
	}
	gaps := map[string]string{}
	for _, g := range v.Gaps {
		gaps[g.Check] = g.Reason
	}
	for _, check := range []string{finops.IdleOldSnapshot, finops.IdleStoppedVolumes, finops.SavingStorageClass} {
		if gaps[check] != finops.GapNotCollected {
			t.Fatalf("gap %s not named: %v", check, v.Gaps)
		}
	}
	if _, named := gaps[finops.SavingRightsize]; named {
		t.Fatal("the hub answered; rightsizing is not a gap")
	}
	// The hub's stop and rightsize for i-0abc count once: the stop ($90)
	// with the rightsize as its alternative; the commitment is not summed.
	stop := byTitle["Stop or remove 1 idle machine"]
	if stop.Monthly != 90_000_000 || len(stop.Alternatives) != 1 || stop.Alternatives[0].Title != "Resize i-0abc to m6i.medium" {
		t.Fatalf("stop %+v", stop)
	}
	if _, dup := byTitle["Resize i-0abc to m6i.medium"]; dup {
		t.Fatal("the rightsize of a stopped instance is an alternative, not an item")
	}
	var sum int64
	for _, it := range v.Items {
		if !it.BeforeOtherSavings {
			sum += it.Monthly
		}
	}
	if v.Total.Monthly != sum || v.Total.Count != len(v.Items)-1 {
		t.Fatalf("total %+v, items without the commitment %d", v.Total, sum)
	}
}

// TestSavingDecisionFollowsItsSaving dismisses a saving and then makes the
// saving stop being produced: the empty load balancer leaves the inventory.
// The decision is marked orphaned with what it referred to kept, not left
// silently invisible; it is never listed while its saving is gone; it applies
// again, with its mark cleared, when the load balancer comes back; retention
// deletes it only after 90 days orphaned; and deleting the payer deletes it.
func TestSavingDecisionFollowsItsSaving(t *testing.T) {
	h, sc, _ := natSpike(t, nil)
	cfg := FinOpsConfig(config.AWSConnectorConfig{})
	cfg.Now = h.clock.Now
	svc := finops.NewService(h.adapter.Source(), NewStore(h.provider, h.adapter.cache.scope), cfg)
	inv := h.opts.Inventory.(*snapshot.Store)
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	scope := h.adapter.cache.scope
	decisions := func() []storage.FinOpsSavingDecision {
		t.Helper()
		out, err := tables.FinOpsSavingDecisions(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	sync := func() {
		t.Helper()
		if err := svc.SyncSavingDecisions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	const title = "Remove 1 load balancer with no targets"
	saving := func() (finops.SavingView, bool) {
		t.Helper()
		v, err := svc.Savings(ctx, finops.Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range v.Items {
			if it.Title == title {
				return it, true
			}
		}
		return finops.SavingView{}, false
	}
	var lbUnit model.Unit
	var all, without []model.Resource
	for _, r := range sc.Inventory {
		if r.Name == "lb-empty" {
			lbUnit = model.Unit{Account: r.Account, Region: r.Region, Service: r.Service}
		}
	}
	for _, r := range sc.Inventory {
		if (model.Unit{Account: r.Account, Region: r.Region, Service: r.Service}) != lbUnit {
			continue
		}
		all = append(all, r)
		if r.Name != "lb-empty" {
			without = append(without, r)
		}
	}
	commit := func(resources []model.Resource) {
		t.Helper()
		if _, err := inv.Commit(lbUnit, snapshot.Candidate{Resources: resources}); err != nil {
			t.Fatal(err)
		}
	}

	lb, ok := saving()
	if !ok {
		t.Fatal("the empty load balancer is not a saving")
	}
	if err := svc.DismissSaving(ctx, lb.ID, "kept for DR", "alice"); err != nil {
		t.Fatal(err)
	}
	sync()
	got := decisions()
	if len(got) != 1 || got[0].SavingID != lb.ID || got[0].Payer != "111111111111" || got[0].Account != lb.Account || got[0].Kind != lb.Kind ||
		got[0].Title != title || !got[0].OrphanedAt.IsZero() {
		t.Fatalf("decision with its subject = %+v", got)
	}

	// The load balancer is deleted: the saving is no longer produced.
	commit(without)
	if _, listed := saving(); listed {
		t.Fatal("a saving whose load balancer is gone is still listed")
	}
	h.clock.set(at("2026-11-09", 11))
	sync()
	got = decisions()
	if len(got) != 1 || !got[0].OrphanedAt.Equal(h.clock.Now()) || got[0].Title != title || got[0].Reason != "kept for DR" {
		t.Fatalf("the decision of a vanished saving is not marked orphaned: %+v", got)
	}

	// It comes back: the decision applies again and the mark is cleared.
	commit(all)
	if back, listed := saving(); !listed || back.Status != "dismissed" || back.Dismissed == nil || back.Dismissed.Reason != "kept for DR" {
		t.Fatalf("a returning saving lost its decision: %+v", back)
	}
	sync()
	if got = decisions(); len(got) != 1 || !got[0].OrphanedAt.IsZero() {
		t.Fatalf("a returning saving's decision stayed orphaned: %+v", got)
	}

	// Gone again: kept for audit through the retention, then deleted.
	commit(without)
	sync()
	orphaned := decisions()[0].OrphanedAt
	if err := h.adapter.cache.pruneDecisions(ctx, orphaned.Add(89*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(decisions()) != 1 {
		t.Fatal("an orphaned decision was deleted before the retention")
	}
	if err := h.adapter.cache.pruneDecisions(ctx, orphaned.Add(91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got = decisions(); len(got) != 0 {
		t.Fatalf("a decision orphaned past the retention is kept: %+v", got)
	}

	// The scheduled refresh runs the same sweep.
	commit(all)
	now := h.clock.Now()
	for id, orphanedAt := range map[string]time.Time{"old-orphan": now.Add(-91 * 24 * time.Hour), "young-orphan": now.Add(-89 * 24 * time.Hour), "live": {}} {
		if err := tables.PutFinOpsSavingDecision(ctx, scope, storage.FinOpsSavingDecision{SavingID: id, State: "dismissed", OrphanedAt: orphanedAt}); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.set(at("2026-11-10", 10))
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, d := range decisions() {
		kept[d.SavingID] = true
	}
	if kept["old-orphan"] || !kept["young-orphan"] || !kept["live"] {
		t.Fatalf("the refresh sweep kept %v", kept)
	}

	// Deleting the payer deletes its decisions.
	if err := svc.DismissSaving(ctx, lb.ID, "kept for DR", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions() {
		if d.Payer == "111111111111" {
			t.Fatalf("a deleted payer's decision is left: %+v", d)
		}
	}
}

// TestDecisionWithoutPayerIsFilledThenGoesWithThePayer: a decision stored
// before decisions kept their subject has only its saving ID and state, so a
// payer delete could not find it. While its saving is still produced, the
// sync fills in payer, account, kind and title from it; the decision is then
// listed as before and deleting the payer deletes it.
func TestDecisionWithoutPayerIsFilledThenGoesWithThePayer(t *testing.T) {
	h, _, _ := natSpike(t, nil)
	cfg := FinOpsConfig(config.AWSConnectorConfig{})
	cfg.Now = h.clock.Now
	svc := finops.NewService(h.adapter.Source(), NewStore(h.provider, h.adapter.cache.scope), cfg)
	tables, ok := storage.FinOps(h.provider)
	if !ok {
		t.Fatal("no FinOps tables")
	}
	scope := h.adapter.cache.scope
	const title = "Remove 1 load balancer with no targets"
	v, err := svc.Savings(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var lb finops.SavingView
	for _, it := range v.Items {
		if it.Title == title {
			lb = it
		}
	}
	if lb.ID == "" {
		t.Fatal("the empty load balancer is not a saving")
	}
	decidedAt := h.clock.Now().Add(-30 * 24 * time.Hour).UTC()
	old := storage.FinOpsSavingDecision{SavingID: lb.ID, State: "dismissed", Reason: "kept for DR", Actor: "alice", At: decidedAt}
	if err := tables.PutFinOpsSavingDecision(ctx, scope, old); err != nil {
		t.Fatal(err)
	}
	if err := tables.PutFinOpsSavingDecision(ctx, scope, storage.FinOpsSavingDecision{SavingID: "gone-saving", State: "dismissed", At: decidedAt}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncSavingDecisions(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]storage.FinOpsSavingDecision{}
	for _, d := range stored {
		byID[d.SavingID] = d
	}
	filled := byID[lb.ID]
	if filled.Payer != "111111111111" || filled.Account != lb.Account || filled.Kind != lb.Kind || filled.Title != title ||
		filled.State != "dismissed" || filled.Reason != "kept for DR" || filled.Actor != "alice" || !filled.At.Equal(decidedAt) || !filled.OrphanedAt.IsZero() {
		t.Fatalf("the decision from before subjects were kept = %+v", filled)
	}
	if gone := byID["gone-saving"]; gone.Payer != "" || gone.OrphanedAt.IsZero() {
		t.Fatalf("a decision whose saving is not produced = %+v", gone)
	}
	if v, err = svc.Savings(ctx, finops.Query{}); err != nil {
		t.Fatal(err)
	}
	for _, it := range v.Items {
		if it.ID == lb.ID && (it.Status != "dismissed" || it.Dismissed == nil || it.Dismissed.Reason != "kept for DR") {
			t.Fatalf("the filled decision no longer applies: %+v", it)
		}
	}

	if err := h.removePayer("111111111111", 0); err != nil {
		t.Fatal(err)
	}
	left, err := tables.FinOpsSavingDecisions(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range left {
		if d.SavingID == lb.ID {
			t.Fatalf("the filled decision outlived its payer: %+v", d)
		}
	}
}

func keysOf(m map[string]finops.SavingView) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestComputeOptimizerAnswersWhenTheHubIsOff: without the hub, machine
// rightsizing comes from Compute Optimizer; with neither, the gap is named.
func TestComputeOptimizerAnswersWhenTheHubIsOff(t *testing.T) {
	h, _, svc := natSpike(t, func(h *harness, _ *scenario) {
		h.fake.failures["hub"] = fakeFailure{Type: "OptInRequiredException", Message: "not enrolled"}
	})
	if h.fake.count("compute_optimizer") == 0 {
		t.Fatal("the fallback was not asked")
	}
	v, _ := svc.Savings(ctx, finops.Query{})
	var co *finops.SavingView
	for i := range v.Items {
		if v.Items[i].Kind == finops.SavingRightsize {
			co = &v.Items[i]
		}
	}
	if co == nil || co.Title != "Resize i-0abc to m6i.medium" || co.Monthly != 42_500_000 || co.Guide == nil {
		t.Fatalf("compute optimizer item %+v", co)
	}
	_, _, svc = natSpike(t, func(h *harness, _ *scenario) {
		h.fake.failures["hub"] = fakeFailure{Type: "OptInRequiredException", Message: "not enrolled"}
		h.fake.failures["compute_optimizer"] = fakeFailure{Type: "OptInRequiredException", Message: "not enrolled"}
	})
	v, _ = svc.Savings(ctx, finops.Query{})
	named := false
	for _, g := range v.Gaps {
		named = named || (g.Check == finops.SavingRightsize && g.Reason == finops.GapProviderOff)
	}
	if !named {
		t.Fatalf("rightsizing gap not named: %v", v.Gaps)
	}
}

// natLine bills the NAT line on nat-1a.
type natLine struct{}

func (natLine) ResourceLines(context.Context, finops.DateRange) ([]finops.CostLine, error) {
	return []finops.CostLine{{Day: finops.MustDate("2026-10-27"), Account: "111111111111", Service: "network.nat",
		UsageType: "USE1-NatGateway-Bytes", RecordType: "usage", ResourceID: "nat-1a"}}, nil
}

// TestUnrelatedDeployIsNeverLikely: without resource-level cost the NAT line
// cannot be narrowed below the region's NAT gateways, so the two deploys at
// the same time tie at the top rank and both are only Possible.
func TestUnrelatedDeployIsNeverLikely(t *testing.T) {
	_, _, svc := natSpikeScenario(t, "cost-nat-spike-two-nats", func(h *harness, _ *scenario) {
		h.opts.ResourceServices = nil
		if err := h.adapter.Update(h.opts); err != nil {
			t.Fatal(err)
		}
	})
	inc := natIncreaseView(t, svc)
	refs := map[string]string{}
	for _, s := range inc.Suspects {
		refs[s.Ref] = s.Confidence
	}
	if len(refs) != 2 || refs["change:evt-checkout-42"] != "possible" || refs["change:evt-inventory-7"] != "possible" {
		t.Fatalf("tied suspects %+v", inc.Suspects)
	}
	// A deploy in the same VPC whose subnet does not route through a NAT
	// gateway of the line is not a suspect at all.
	sc := loadScenario(t, "cost-nat-spike-two-nats")
	all := []model.Resource{}
	for _, r := range sc.Inventory {
		all = append(all, r)
	}
	d := finops.Detected{Key: finops.LineKey{Account: "111111111111", Service: "network.nat", UsageType: "USE1-NatGateway-Bytes"}}
	scopes := relatedScopes(all, narrowResources(all, d, []string{"nat-1a"}))
	if scopes["arn:aws:ecs:us-east-1:111111111111:service/prod/inventory-svc"] != scopeNone ||
		scopes["arn:aws:ecs:us-east-1:111111111111:service/prod/checkout-api"] != scopeNetwork {
		t.Fatalf("scopes %v", scopes)
	}
	vm := finops.Detected{Key: finops.LineKey{Account: "111111111111", Service: "compute.vm", UsageType: "USE1-BoxUsage:m6i.large"}}
	if n := narrowResources(all, vm, nil); len(n) != 0 {
		t.Fatalf("no m6i.large instance in the inventory: %v", n)
	}
	fn := finops.Detected{Key: finops.LineKey{Account: "111111111111", Service: "compute.function"}}
	if n := narrowResources(all, fn, nil); len(n) != 1 {
		t.Fatalf("the only function narrows to itself: %v", n)
	}
}

// TestOrganizationNamesSavingsChecksThatNeedUsageDetail: Organization lines
// have no usage type, so the idle-address and NAT checks are named as gaps
// instead of silently finding nothing.
func TestOrganizationNamesSavingsChecksThatNeedUsageDetail(t *testing.T) {
	h, _ := orgHarness(t)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	in, err := h.adapter.Source().SavingsInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gaps := map[string]string{}
	for _, g := range in.Gaps {
		gaps[g.Check] = g.Reason
	}
	if gaps[finops.IdleUnusedIP] != finops.GapNeedsUsageDetail || gaps[finops.SavingNetworkPath] != finops.GapNeedsUsageDetail {
		t.Fatalf("gaps %v", in.Gaps)
	}
	sc := loadScenario(t, "single-account")
	single := newHarness(t, sc, nil)
	if _, err := single.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	in, _ = single.adapter.Source().SavingsInputs(ctx)
	for _, g := range in.Gaps {
		if g.Reason == finops.GapNeedsUsageDetail {
			t.Fatalf("a single account has usage types: %v", in.Gaps)
		}
	}
}

// rightsizeTitle is the title of the one rightsize item, "" when none.
func rightsizeTitle(t *testing.T, svc *finops.Service) string {
	t.Helper()
	v, err := svc.Savings(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range v.Items {
		if it.Kind == finops.SavingRightsize {
			return it.Title
		}
	}
	return ""
}

// Compute Optimizer returns the Name tag value as the instance name. A
// rightsize title uses it only while the Name tag is allowlisted, and
// narrowing the allowlist hides a name already cached without waiting for
// the next refresh; otherwise the title names the instance ID.
func TestRightsizeTitleShowsTheNameTagOnlyWhenAllowlisted(t *testing.T) {
	hubOff := func(h *harness) {
		h.fake.failures["hub"] = fakeFailure{Type: "OptInRequiredException", Message: "not enrolled"}
	}
	_, _, svc := natSpike(t, func(h *harness, _ *scenario) { hubOff(h) })
	if got := rightsizeTitle(t, svc); got != "Resize i-0abc to m6i.medium" {
		t.Fatalf("Name not allowlisted: title = %q", got)
	}
	h, _, svc := natSpike(t, func(h *harness, _ *scenario) {
		hubOff(h)
		h.opts.NameAllowlisted = true
		if err := h.adapter.Update(h.opts); err != nil {
			t.Fatal(err)
		}
	})
	if got := rightsizeTitle(t, svc); got != "Resize reporting to m6i.medium" {
		t.Fatalf("Name allowlisted: title = %q", got)
	}
	narrowed := h.opts
	narrowed.NameAllowlisted = false
	if err := h.adapter.Update(narrowed); err != nil {
		t.Fatal(err)
	}
	if got := rightsizeTitle(t, svc); got != "Resize i-0abc to m6i.medium" {
		t.Fatalf("allowlist narrowed after the refresh: title = %q", got)
	}
}

// Through the settings sanitiser, "Name" on the allowlist shows the Name
// tag in a rightsize title and "name" does not: tag keys are
// case-sensitive and matched exactly.
func TestRightsizeTitleNameAllowlistThroughSettingsIsCaseSensitive(t *testing.T) {
	targets := []Target{{AccountID: "111111111111", Credentials: testCreds}}
	for _, c := range []struct{ key, want string }{
		{"Name", "Resize reporting to m6i.medium"},
		{"name", "Resize i-0abc to m6i.medium"},
	} {
		allowed := FromConfig(awsConfig(""), targets, settings.SanitizeAllowlist([]string{" " + c.key + " ", "team"}), "prod")
		_, _, svc := natSpike(t, func(h *harness, _ *scenario) {
			h.fake.failures["hub"] = fakeFailure{Type: "OptInRequiredException", Message: "not enrolled"}
			h.opts.NameAllowlisted = allowed.NameAllowlisted
			if err := h.adapter.Update(h.opts); err != nil {
				t.Fatal(err)
			}
		})
		if got := rightsizeTitle(t, svc); got != c.want {
			t.Errorf("allowlist key %q: title = %q, want %q", c.key, got, c.want)
		}
	}
}

// A commitment suggestion stored before it was reworded at fetch (AWS's own
// sentence, with the account ID) is reworded again on every read, so the
// savings list never serves the provider's text.
func TestStoredRawCommitmentSuggestionIsRewordedOnRead(t *testing.T) {
	h, _, svc := natSpike(t, nil)
	st, err := h.adapter.cache.status()
	if err != nil {
		t.Fatal(err)
	}
	const raw = "$4/hour with one year term (NoUpfront) in AWS account 111111111111 (Linked account level)"
	stored := 0
	for _, ps := range st.Payers {
		if ps.CommitmentMonthly > 0 {
			ps.CommitmentSuggestion = raw
			stored++
		}
	}
	if stored == 0 {
		t.Fatal("the scenario must store a commitment")
	}
	if err := h.adapter.cache.putStatus(st); err != nil {
		t.Fatal(err)
	}
	v, err := svc.Savings(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range v.Items {
		if it.Kind != finops.SavingCommitment {
			continue
		}
		found = true
		if it.Title != "Raise discount coverage from 71% toward 80%: a 1-year commitment of $4/h" ||
			strings.Contains(it.Line, "111111111111") || strings.Contains(it.Line, "NoUpfront") {
			t.Fatalf("commitment read from a stored raw suggestion: %q / %q", it.Title, it.Line)
		}
	}
	if !found {
		t.Fatal("no commitment item")
	}
}

// lostAccess makes the only payer unreachable, as the settings report it
// when the server's credentials stop working: no payer to fetch through,
// cost data still on.
func lostAccess(t *testing.T, h *harness, since time.Time) {
	t.Helper()
	o := h.opts
	o.Unreachable = []UnreachablePayer{{AccountID: o.Payers[0].AccountID, Cause: "credentials_invalid", Since: since}}
	o.Payers = nil
	o.Enabled, o.DisabledReason = func() bool { return false }, ReasonNoConnectedAccounts
	if err := h.adapter.Update(o); err != nil {
		t.Fatal(err)
	}
}

// When the only account loses access, the cost already stored stays on the
// page, marked stale with the cause and since the last good refresh; no
// refresh runs and nothing stored is deleted.
func TestLostAccessKeepsStoredCostVisibleAndStale(t *testing.T) {
	h, _, svc := natSpike(t, nil)
	before, err := svc.Summary(ctx, finops.Query{})
	if err != nil || !before.Enabled || before.Total == 0 {
		t.Fatalf("summary before: %+v %v", before.Header, err)
	}
	st, err := h.adapter.cache.status()
	if err != nil {
		t.Fatal(err)
	}
	lastGood := st.Payers[h.opts.Payers[0].AccountID].LastSuccess
	lostAccess(t, h, h.clock.Now().Add(time.Hour))

	after, err := svc.Summary(ctx, finops.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Enabled || after.Reason != "" || after.Total != before.Total || after.Sync == nil {
		t.Fatalf("stored cost must stay visible: enabled=%v reason=%q total=%d", after.Enabled, after.Reason, after.Total)
	}
	sync := after.Sync
	if !sync.Stale || sync.StaleReason != "credentials_invalid" || sync.StaleDetail == nil || sync.StaleDetail.Since == nil ||
		!sync.StaleDetail.Since.Equal(lastGood) || sync.LastCompleteDay != before.Sync.LastCompleteDay {
		t.Fatalf("stale marking: %+v detail %+v", sync, sync.StaleDetail)
	}
	if _, err := h.adapter.Refresh(ctx, KindScheduled); !errors.Is(err, ErrDisabled) {
		t.Fatalf("refresh without a reachable payer = %v, want ErrDisabled", err)
	}
	again, err := svc.Summary(ctx, finops.Query{})
	if err != nil || again.Total != before.Total {
		t.Fatalf("stored cost after a skipped refresh: %d %v", again.Total, err)
	}
}

// With no cost ever stored, a lost account reads as no account ready.
func TestLostAccessWithNothingStoredSaysNoAccountReady(t *testing.T) {
	sc := loadScenario(t, "cost-nat-spike")
	h := newHarness(t, sc, nil)
	lostAccess(t, h, h.clock.Now())
	svc := NewBackend(h.adapter, NewStore(h.provider, h.adapter.cache.scope), func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} }).Service()
	v, err := svc.Summary(ctx, finops.Query{})
	if err != nil || v.Enabled || v.Reason != ReasonNoConnectedAccounts {
		t.Fatalf("summary = enabled %v reason %q err %v", v.Enabled, v.Reason, err)
	}
}
