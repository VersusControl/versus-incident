package collect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// The Changes page states, each reached from a fakeaws scenario through
// the real collector and the real change feed: poll CloudTrail, then read
// the answer the page and the agent tool get.

func answerOf(t *testing.T, h harness, request snapshot.FeedRequest, options ...snapshot.FeedOption) snapshot.ChangesAnswer {
	t.Helper()
	if request.Window == 0 {
		request.Window = 24 * time.Hour
	}
	answer, err := snapshot.NewChangeFeed(h.store, options...).Answer(context.Background(), request)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	return answer
}

func scopeOf(accounts ...snapshot.ScopeAccount) snapshot.FeedOption {
	return snapshot.WithScope(func(context.Context) ([]snapshot.ScopeAccount, bool, error) { return accounts, true, nil })
}

func gapReasons(answer snapshot.ChangesAnswer, account string) map[string]bool {
	reasons := map[string]bool{}
	for _, gap := range answer.Gaps {
		if account == "" || gap.Account == account {
			reasons[gap.Reason] = true
		}
	}
	return reasons
}

// State 01: before the first poll nothing is claimed; the window is not
// complete and the total is a lower bound, never "0 changes".
func TestChangesStateFirstSync(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesQuiet)
	answer := answerOf(t, h, snapshot.FeedRequest{}, scopeOf(snapshot.ScopeAccount{Account: productionAccount, Regions: []string{testRegion}}))
	if answer.Complete || !answer.Summary.TotalIsLowerBound || len(answer.Gaps) == 0 {
		t.Fatalf("first sync = complete %v lower bound %v gaps %+v", answer.Complete, answer.Summary.TotalIsLowerBound, answer.Gaps)
	}
}

// States 02 and 17: CloudTrail denied in one account marks only that
// account unknown, names the permission, and the other account stays
// complete.
func TestChangesStateCloudTrailDeniedInOneAccount(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesCloudTrailDenied)
	mustSync(t, h, target(productionAccount), KindChanges)
	mustSync(t, h, target(stagingAccount, "eu-west-1"), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{}, scopeOf(
		snapshot.ScopeAccount{Account: productionAccount, Regions: []string{testRegion}},
		snapshot.ScopeAccount{Account: stagingAccount, Regions: []string{"eu-west-1"}}))
	if answer.Complete || !answer.Summary.TotalIsLowerBound {
		t.Fatalf("denied account read as complete: %+v", answer.Summary)
	}
	denied := false
	for _, gap := range answer.Gaps {
		if gap.Reason == string(snapshot.GapCloudTrailDenied) {
			if gap.Account != stagingAccount || gap.MissingPermission == nil || *gap.MissingPermission != "cloudtrail:LookupEvents" {
				t.Fatalf("denied gap = %+v", gap)
			}
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no cloudtrail_denied gap: %+v", answer.Gaps)
	}
	if len(answer.Accounts) != 2 {
		t.Fatalf("accounts = %+v", answer.Accounts)
	}
	for _, account := range answer.Accounts {
		if want := account.Account == productionAccount; account.Complete != want {
			t.Fatalf("account %s complete = %v", account.Account, account.Complete)
		}
	}
}

// State 03: a throttled poll leaves a gap named throttled, never zero.
func TestChangesStateThrottledGap(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesThrottled)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{}, scopeOf(snapshot.ScopeAccount{Account: productionAccount, Regions: []string{testRegion}}))
	if answer.Complete || !gapReasons(answer, productionAccount)[string(snapshot.GapChangesThrottled)] {
		t.Fatalf("throttled = complete %v gaps %+v", answer.Complete, answer.Gaps)
	}
}

// States 04 and 15: a quiet, fully read window is complete with no
// changes, and the newest stretch is shown as still arriving.
func TestChangesStateQuietWindow(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesQuiet)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{Window: time.Hour})
	if !answer.Complete || answer.Summary.Total != 0 || len(answer.Items) != 0 {
		t.Fatalf("quiet = complete %v total %d items %d gaps %+v", answer.Complete, answer.Summary.Total, len(answer.Items), answer.Gaps)
	}
	if !answer.Window.SettledUntil.Before(answer.Window.Until) || answer.Buckets[len(answer.Buckets)-1].State != snapshot.BucketArriving {
		t.Fatalf("tail not arriving: window %+v last bucket %+v", answer.Window, answer.Buckets[len(answer.Buckets)-1])
	}
}

// State 05: only AWS-service changes.
func TestChangesStateOnlyAWSServiceChanges(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesServiceOnly)
	mustSync(t, h, target(productionAccount), KindChanges)
	summary := answerOf(t, h, snapshot.FeedRequest{}).Summary
	by := summary.ByActorClass
	if by[string(model.ActorAWSService)] != 2 || by[string(model.ActorPipeline)] != 0 || by[string(model.ActorPerson)] != 0 || summary.People != 0 {
		t.Fatalf("service-only summary = %+v", summary)
	}
}

// State 06: a busy account pages its list and never truncates silently:
// either every change is counted, or the total is a lower bound with the
// reason.
func TestChangesStateBusyAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("reads 5 000 events through CloudTrail's 2 requests a second limit, on the Region and the global stream (about 100 s); runs in the full suite")
	}
	h := newHarness(t, fakeaws.ScenarioChangesBusy)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{Limit: 200})
	if len(answer.Items) == 0 || answer.NextCursor == nil {
		t.Fatalf("busy list does not page: %d items, next %v", len(answer.Items), answer.NextCursor)
	}
	// Every served event inside the window is counted (the oldest one sits
	// on the window's edge and the newest few are still arriving), unless
	// the total says it is a lower bound and a gap names why.
	counted := answer.Summary.Total >= fakeaws.ChangesBusySize-int64(fakeaws.ChangesBusySize*15/(24*60))-2
	if answer.Summary.Truncated && !answer.Summary.TotalIsLowerBound || !counted && (!answer.Summary.TotalIsLowerBound || answer.Complete || len(answer.Gaps) == 0) {
		t.Fatalf("busy total %d of %d without a lower bound: %+v gaps %+v", answer.Summary.Total, fakeaws.ChangesBusySize, answer.Summary, answer.Gaps)
	}
	if len(answer.Summary.TopActors) == 0 {
		t.Fatal("no top actors")
	}
}

// State 07: an error burst is one row with its count.
func TestChangesStateErrorBurstCollapses(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesErrorBurst)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{})
	bursts := 0
	for _, item := range answer.Items {
		if item.Kind == snapshot.ItemBurst {
			bursts++
			if item.Count == nil || *item.Count != fakeaws.ChangesBurstSize || item.ErrorCode == nil || *item.ErrorCode != "AccessDenied" || item.EndTime == nil {
				t.Fatalf("burst = %+v", item)
			}
		}
	}
	if bursts != 1 || answer.Summary.Errors != fakeaws.ChangesBurstSize {
		t.Fatalf("bursts = %d errors = %d items %+v", bursts, answer.Summary.Errors, answer.Items)
	}
}

// State 08: an alarm with no change in the hour before it gets no suspect,
// and the answer is "none" only because that hour is read and settled.
func TestChangesStateAlarmWithNoSuspect(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesAlarmNoSuspect)
	mustSync(t, h, target(productionAccount), KindHealth, KindChanges)
	health, err := h.store.Health(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := h.store.Resources(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	alarms := suspect.Alarms(health.Health, resources.Resources)
	found := false
	for _, alarm := range alarms {
		if alarm.Name != fakeaws.WorldAlarm5xx {
			continue
		}
		found = true
		source := suspect.NewSnapshot(h.store)
		result, err := suspect.Find(context.Background(), alarm.Anchor(resources.Resources), source, source)
		if err != nil {
			t.Fatal(err)
		}
		if !alarm.StartKnown || result.Status != suspect.StatusNone || len(result.Candidates) != 0 {
			t.Fatalf("alarm %+v result %+v", alarm, result)
		}
	}
	if !found {
		t.Fatalf("alarm line misses %s: %+v", fakeaws.WorldAlarm5xx, alarms)
	}
}

// States 09 and 10 and the partial compare: before the oldest snapshot
// compare is unavailable and names where history starts; units without
// history at the from time are named, not diffed; two equal snapshots
// differ in nothing.
func TestChangesStateCompare(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesQuiet)
	ctx := context.Background()
	feed := snapshot.NewChangeFeed(h.store)
	beforeAny := time.Now().Add(-2 * time.Hour)
	mustSync(t, h, target(productionAccount), KindHealth)
	afterHealth := time.Now()
	time.Sleep(10 * time.Millisecond)
	mustSync(t, h, target(productionAccount), KindInventory)

	unavailable, err := feed.Compare(ctx, snapshot.CompareRequest{From: beforeAny, Account: productionAccount})
	if err != nil || unavailable.Status != snapshot.CompareUnavailable || unavailable.HistoryStarts == nil {
		t.Fatalf("compare before history = %+v %v", unavailable, err)
	}
	partial, err := feed.Compare(ctx, snapshot.CompareRequest{From: afterHealth, Account: productionAccount})
	if err != nil || partial.Status != snapshot.CompareOK || len(partial.Partial) == 0 || len(partial.Added) != 0 {
		t.Fatalf("compare with inventory units newer than from = status %s partial %d added %d %v", partial.Status, len(partial.Partial), len(partial.Added), err)
	}

	mustSync(t, h, target(productionAccount), KindInventory, KindHealth)
	settled := time.Now()
	time.Sleep(10 * time.Millisecond)
	mustSync(t, h, target(productionAccount), KindInventory, KindHealth)
	same, err := feed.Compare(ctx, snapshot.CompareRequest{From: settled, Account: productionAccount})
	if err != nil || same.Status != snapshot.CompareOK || len(same.Added)+len(same.Removed)+len(same.Changed) != 0 {
		t.Fatalf("compare of equal snapshots = %+v %v", same, err)
	}
}

// States 11 and 12: a disconnected account and a Region that is not
// enabled are named, never shown as quiet.
func TestChangesStateDisconnectedAndRegionNotEnabled(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesQuiet)
	mustSync(t, h, target(productionAccount), KindChanges)
	since := time.Now().Add(-2 * time.Hour)
	answer := answerOf(t, h, snapshot.FeedRequest{}, scopeOf(
		snapshot.ScopeAccount{Account: productionAccount, Regions: []string{testRegion}, NotEnabled: []string{"ap-southeast-1"}},
		snapshot.ScopeAccount{Account: stagingAccount, Regions: []string{"eu-west-1"}, Outage: snapshot.GapAccountDisconnected, OutageSince: since}))
	if answer.Complete || !gapReasons(answer, stagingAccount)[string(snapshot.GapAccountDisconnected)] {
		t.Fatalf("disconnected = complete %v gaps %+v", answer.Complete, answer.Gaps)
	}
	notEnabled := false
	for _, region := range answer.Regions {
		notEnabled = notEnabled || (region.Region == "ap-southeast-1" && region.State == snapshot.RegionNotEnabled)
	}
	if !notEnabled {
		t.Fatalf("regions = %+v", answer.Regions)
	}
}

// State 13: an unknown actor through an API no template covers reads as
// such, with the raw API name, never a guessed sentence.
func TestChangesStateUnknownActorGenericTemplate(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesUnknownActor)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer := answerOf(t, h, snapshot.FeedRequest{})
	if len(answer.Items) != 1 {
		t.Fatalf("items = %+v", answer.Items)
	}
	item := answer.Items[0]
	if item.Actor != "Unknown actor" || item.ActorClass != string(model.ActorUnknown) || item.EventName != "widgets:"+fakeaws.ChangesUnknownEventName ||
		item.Summary != "Create widget ("+fakeaws.ChangesUnknownSource+")" {
		t.Fatalf("unknown actor item = %+v", item)
	}
}

// State 16: planted secrets never reach the answer.
func TestChangesStateHiddenSecrets(t *testing.T) {
	h := newHarness(t, "secret-canary")
	mustSync(t, h, target(productionAccount), KindChanges)
	encoded, err := json.Marshal(answerOf(t, h, snapshot.FeedRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range fakeaws.CanarySecrets() {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("answer carries planted secret %q", secret)
		}
	}
}
