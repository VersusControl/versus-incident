package collect

import (
	"context"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// The global stream keeps only global-service events and the us-east-1
// pair leaves them to it, so one event never flips region; every account
// with regions plans a global stream, us-east-1 selected or not.
func TestGlobalStreamKeepsOnlyGlobalSources(t *testing.T) {
	cases := []struct {
		region, source string
		keep           bool
	}{
		{model.GlobalRegion, "iam.amazonaws.com", true},
		{model.GlobalRegion, "ec2.amazonaws.com", false},
		{"us-east-1", "iam.amazonaws.com", false},
		{"us-east-1", "ec2.amazonaws.com", true},
		{"eu-west-1", "cloudfront.amazonaws.com", false},
	}
	for _, tc := range cases {
		if keepEvent(tc.region, tc.source) != tc.keep {
			t.Errorf("keepEvent(%s, %s) = %v", tc.region, tc.source, !tc.keep)
		}
	}
	units := Plan(productionAccount, []string{"eu-west-1"}, KindChanges)
	if len(units) != 2 || units[1].Region != model.GlobalRegion || units[1].Service != snapshot.ChangeService {
		t.Fatalf("planned change units = %+v", units)
	}
}

// heldElsewhere is a shared backend where another replica holds every
// poller lock.
type heldElsewhere struct {
	feedProvider
}

type feedProvider interface {
	storage.Provider
	storage.AWSInventoryStore
	storage.AWSChangeFeedStore
}

func (heldElsewhere) TryLockSession(context.Context, int64) (func(), bool, error) {
	return nil, false, nil
}

// Two replicas on one database: the one without the pair's lock neither
// calls LookupEvents nor writes the pair's state.
func TestOnePollerPerPairAcrossReplicas(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	provider := heldElsewhere{storage.NewMemory().(feedProvider)}
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	collector := New(store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0))
	report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range report.Units {
		if unit.Outcome != OutcomeSkipped {
			t.Fatalf("unit without the lock = %+v", unit)
		}
	}
	if report.Failed() != 0 || h.fake.OperationCount("cloudtrail:LookupEvents") != 0 {
		t.Fatalf("failed %d, lookups %d", report.Failed(), h.fake.OperationCount("cloudtrail:LookupEvents"))
	}
	checkpoint, err := store.Checkpoint(productionAccount, testRegion)
	if err != nil || !checkpoint.LastAttemptAt.IsZero() || len(checkpoint.Covered) != 0 {
		t.Fatalf("skipped replica wrote state: %+v, %v", checkpoint, err)
	}
}

func feedGaps(t *testing.T, h harness, reason snapshot.ChangeGapReason) int {
	t.Helper()
	stored, err := h.provider.(storage.AWSChangeFeedStore).AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(), storage.AWSChangeFeedQuery{Account: productionAccount, Region: testRegion})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, gap := range stored.Gaps {
		if gap.Reason == string(reason) {
			found++
		}
	}
	return found
}

// The page budget decides too_busy: below it the window is read in full,
// above it the unread stretch is labelled too_busy.
func TestPageBudgetTooBusyOnlyAboveBudget(t *testing.T) {
	pageSize := changePageSize
	t.Cleanup(func() { changePageSize = pageSize })
	changePageSize = 2
	for _, tc := range []struct {
		budget int
		busy   bool
	}{{1, true}, {50, false}} {
		h := newHarness(t, fakeaws.ScenarioBadDeploy, WithOptions(func() Options { return Options{ChangePageBudget: tc.budget} }))
		mustSync(t, h, target(productionAccount), KindChanges)
		if got := feedGaps(t, h, snapshot.GapTooBusy) > 0; got != tc.busy {
			t.Fatalf("budget %d: too_busy %v", tc.budget, got)
		}
	}
}

// A local clock ahead of AWS never claims coverage past AWS time, and the
// skew is recorded for Settings.
func TestFastLocalClockNeverClaimsPastAWSTime(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	awsNow := time.Now().UTC().Truncate(time.Second)
	h.fake.SetClock(func() time.Time { return awsNow })
	local := awsNow.Add(20 * time.Minute)
	collector := New(h.store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0), WithClock(func() time.Time { return local }))
	if _, err := collector.Sync(context.Background(), target(productionAccount), KindChanges); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := h.store.Checkpoint(productionAccount, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if until := checkpoint.CoveredUntil(); until.IsZero() || until.After(awsNow.Add(-DefaultChangeOverlap)) {
		t.Fatalf("coverage until %s, AWS settled %s", until, awsNow.Add(-DefaultChangeOverlap))
	}
	feed := snapshot.NewChangeFeed(h.store, snapshot.WithFeedClock(func() time.Time { return local }))
	answer, err := feed.Answer(context.Background(), snapshot.FeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Sync.ClockSkewSeconds < 1100 {
		t.Fatalf("clock skew = %d s", answer.Sync.ClockSkewSeconds)
	}
}

// An event delivered later than twice the overlap is missed by the next
// poll; with a longer overlap it is caught and counted as a late arrival.
func TestLateEventCountedAndCaughtWithLongerOverlap(t *testing.T) {
	const lateID = "00000000-0000-0000-0000-0000000000d1"
	for _, tc := range []struct {
		overlap time.Duration
		caught  bool
	}{{15 * time.Minute, false}, {30 * time.Minute, true}} {
		h := newHarness(t, fakeaws.ScenarioBadDeploy)
		first := time.Now().UTC().Truncate(time.Minute)
		at := first
		h.fake.SetClock(func() time.Time { return at })
		poll := func() {
			t.Helper()
			collector := New(h.store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
				WithClock(func() time.Time { return at }), WithOptions(func() Options { return Options{ChangeOverlap: tc.overlap} }))
			if report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges); err != nil || report.Failed() != 0 {
				t.Fatalf("poll = %+v, %v", report, err)
			}
		}
		poll()
		h.fake.AddLateEvent(lateID, "PutRule", "events.amazonaws.com", first.Add(-35*time.Minute))
		at = first.Add(5 * time.Minute)
		poll()
		window, err := h.store.Changes(context.Background(), snapshot.Query{Account: productionAccount, Region: testRegion}, first.Add(-time.Hour), first)
		if err != nil {
			t.Fatal(err)
		}
		caught := false
		for _, change := range window.Changes {
			caught = caught || change.ID == lateID
		}
		if caught != tc.caught {
			t.Fatalf("overlap %s: late event caught %v", tc.overlap, caught)
		}
		feed := snapshot.NewChangeFeed(h.store, snapshot.WithFeedClock(func() time.Time { return at }))
		answer, err := feed.Answer(context.Background(), snapshot.FeedRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int64{true: 1, false: 0}[tc.caught]; answer.Sync.LateArrivals != want {
			t.Fatalf("overlap %s: late arrivals %d, want %d", tc.overlap, answer.Sync.LateArrivals, want)
		}
	}
}

// A revoked or rotated key is a credential failure, never "Region not
// enabled": the key fails the account's identity check, which decides the
// ambiguous answer.
func TestRevokedKeyIsCredentialFailure(t *testing.T) {
	err := &smithy.GenericAPIError{Code: "InvalidClientTokenId", Message: "The security token included in the request is invalid."}
	code := env{identity: AccountHealthFailing}.resolve(errorCode(err))
	if code != "no_server_identity" || snapshot.ChangeGapReasonForCode(code) != snapshot.GapCredentialsInvalid {
		t.Fatalf("revoked key = %s / %s", code, snapshot.ChangeGapReasonForCode(code))
	}
}

// Each unit service maps to its collection kind, which sizes its
// generation cap.
func TestKindOfSizesEachService(t *testing.T) {
	for service, kind := range map[string]Kind{"cloudwatch": KindHealth, "ecs": KindHealth, "ec2": KindHealth, "lambda": KindInventory, "iam": KindInventory,
		snapshot.ChangeService: KindChanges} {
		if got := KindOf(service); got != kind {
			t.Errorf("KindOf(%s) = %s, want %s", service, got, kind)
		}
	}
}

// One poll takes each EventId once, so a page or window that returns an
// event twice never doubles a count or a row.
func TestPollEventsTakesEachEventOnce(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	events := newPollEvents(productionAccount, testRegion, DefaultRoutineEvents())
	routine := changesEvent("r1", "UpdateInstanceInformation", "ssm.amazonaws.com", at, `{"userIdentity":{"type":"AWSService","invokedBy":"ssm.amazonaws.com"}}`)
	failed := changesEvent("f1", "UpdateService", "ecs.amazonaws.com", at, `{"userIdentity":{"type":"IAMUser","arn":"arn:aws:iam::111122223333:user/ci"},"errorCode":"AccessDenied"}`)
	for range 3 {
		events.add(routine, at.Add(-time.Hour))
		events.add(failed, at.Add(-time.Hour))
	}
	counts := events.countList()
	if len(events.found) != 1 || len(counts) != 2 {
		t.Fatalf("found %d, counts %+v", len(events.found), counts)
	}
	for _, count := range counts {
		if count.Events != 1 {
			t.Fatalf("count doubled: %+v", count)
		}
	}
}

// AWS Config's rule evaluations are routine: counted, not listed. Sign-ins,
// token calls and reads CloudTrail flags as writes are neither listed nor
// counted as changes; they add to the not-changes count once, in the
// stretch the poll newly settles. A real write is kept.
func TestPollEventsLeavesOutNonChanges(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	events := newPollEvents(productionAccount, testRegion, DefaultRoutineEvents())
	service := `{"readOnly":false,"userIdentity":{"type":"AssumedRole","arn":"arn:aws:sts::111122223333:assumed-role/AWSServiceRoleForConfig/config-session",` +
		`"invokedBy":"config.amazonaws.com","sessionContext":{"sessionIssuer":{"type":"Role","arn":"arn:aws:iam::111122223333:role/aws-service-role/config.amazonaws.com/AWSServiceRoleForConfig"}}}}`
	anonymous := `{"readOnly":false,"userIdentity":{"type":"Unknown","principalId":"Anonymous"}}`
	user := `{"readOnly":false,"userIdentity":{"type":"IAMUser","arn":"arn:aws:iam::111122223333:user/app"},"userAgent":"aws-sdk-java/2.25.0"}`
	for _, event := range []changes.Event{
		changesEvent("pe1", "PutEvaluations", "config.amazonaws.com", at, service),
		changesEvent("pe2", "PutEvaluations", "config.amazonaws.com", at, service),
		changesEvent("tok", "Token_POST", "cognito-idp.amazonaws.com", at, anonymous),
		changesEvent("auth", "AdminInitiateAuth", "cognito-idp.amazonaws.com", at, user),
		changesEvent("login", "ConsoleLogin", "signin.amazonaws.com", at.Add(-30*time.Minute), anonymous),
		changesEvent("desc", "DescribeDomainConfiguration", "iot.amazonaws.com", at.Add(-90*time.Minute), user),
		changesEvent("tail", "ListMlflowApps", "sagemaker.amazonaws.com", at.Add(10*time.Minute), user),
		changesEvent("run", "RunInstances", "ec2.amazonaws.com", at, user),
	} {
		events.add(event, at.Add(-2*time.Hour))
		events.add(event, at.Add(-2*time.Hour))
	}
	if len(events.found) != 1 || events.found[0].EventName != "RunInstances" {
		t.Fatalf("found = %+v", events.found)
	}
	counts := events.countList()
	if len(counts) != 1 || counts[0].EventName != "PutEvaluations" || counts[0].Kind != storage.AWSChangeCountRoutine || counts[0].Events != 2 {
		t.Fatalf("counts = %+v", counts)
	}
	// An earlier poll covered up to an hour ago; this one settles up to
	// five minutes past at. The read an hour before that and the unsettled
	// tail are not counted again.
	previous := changes.Checkpoint{Covered: []changes.Interval{{From: at.Add(-3 * time.Hour), To: at.Add(-time.Hour)}}}
	covered := []changes.Interval{{From: at.Add(-2 * time.Hour), To: at.Add(5 * time.Minute)}}
	if got := events.notChangeCount(previous, covered); got != 3 {
		t.Fatalf("not changes = %d, want 3 (token, auth, login)", got)
	}
}

func changesEvent(id, name, source string, at time.Time, raw string) changes.Event {
	return changes.Event{ID: id, Name: name, Source: source, Time: at, Raw: raw}
}

// Under the lock, a pair any replica polled within half an interval is
// skipped; once the interval passes it is polled again.
func TestRecentlyPolledPairIsSkipped(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	now := time.Now().UTC()
	collector := New(h.store, h.limiter, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep), WithRetries(0), WithClock(func() time.Time { return now }),
		WithOptions(func() Options { return Options{ChangesInterval: 5 * time.Minute} }))
	if report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges); err != nil || report.Units[0].Outcome != OutcomeCommitted {
		t.Fatalf("first poll = %+v, %v", report, err)
	}
	lookups := h.fake.OperationCount("cloudtrail:LookupEvents")
	report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges)
	if err != nil {
		t.Fatal(err)
	}
	for _, unit := range report.Units {
		if unit.Outcome != OutcomeSkipped {
			t.Fatalf("recently polled pair = %+v", unit)
		}
	}
	if h.fake.OperationCount("cloudtrail:LookupEvents") != lookups {
		t.Fatal("a recently polled pair read CloudTrail again")
	}
	now = now.Add(5 * time.Minute)
	h.fake.SetClock(func() time.Time { return now })
	if report, err := collector.Sync(context.Background(), target(productionAccount), KindChanges); err != nil || report.Units[0].Outcome != OutcomeCommitted {
		t.Fatalf("poll after the interval = %+v, %v", report, err)
	}
}

// Clock skew compares AWS's Date with the middle of each round trip and
// keeps the smallest offset: a long paged read, a slow or retried call never
// reads as skew, and a real offset still shows.
func TestClockSkewIgnoresSlowAndPagedReads(t *testing.T) {
	sent := time.Date(2026, 11, 19, 2, 38, 0, 0, time.UTC)
	var skew clockSkew
	// A call that waited out retries for 40 s still gives an estimate from
	// the middle of its round trip, until a prompt answer replaces it.
	skew.sample(sent, sent.Add(40*time.Second), sent.Add(20*time.Second))
	if !skew.known || skew.precise || absDuration(skew.value) > time.Second {
		t.Fatalf("slow call estimate = %+v", skew)
	}
	// Pages answered across a 49 s read, each promptly.
	for page := 0; page < 50; page++ {
		at := sent.Add(time.Duration(page) * time.Second)
		skew.sample(at, at.Add(300*time.Millisecond), at.Truncate(time.Second))
	}
	if !skew.known || skew.value.Round(time.Second) != 0 {
		t.Fatalf("paged read skew = %s", skew.value)
	}
	var fast clockSkew
	fast.sample(sent, sent.Add(200*time.Millisecond), sent.Add(-20*time.Minute))
	if got := fast.value.Round(time.Second); got != 20*time.Minute {
		t.Fatalf("real skew = %s", got)
	}
}

// Each malformed event is counted once: the global stream re-reads
// us-east-1 but counts only events whose source it keeps.
func TestMalformedEventsCountedOnceAcrossGlobalStream(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioChangesMalformed)
	mustSync(t, h, target(productionAccount), KindChanges)
	answer, err := snapshot.NewChangeFeed(h.store).Answer(context.Background(), snapshot.FeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Sync.Dropped != 3 {
		t.Fatalf("dropped = %d, want 3 (no ID, no time, NUL name)", answer.Sync.Dropped)
	}
}

// With only slow answers there is still an estimate, and coverage is capped
// by the most the local clock can be ahead, so it never passes AWS time.
func TestClockSkewFromSlowAnswersNeverClaimsPastAWSTime(t *testing.T) {
	sent := time.Date(2026, 11, 19, 2, 38, 0, 0, time.UTC)
	var skew clockSkew
	// Every answer takes 30 s; AWS is really 10 s behind the local clock
	// and stamped each answer at the end of its round trip.
	for page := 0; page < 5; page++ {
		at := sent.Add(time.Duration(page) * 30 * time.Second)
		skew.sample(at, at.Add(30*time.Second), at.Add(30*time.Second-10*time.Second))
	}
	if !skew.known || skew.precise {
		t.Fatalf("no estimate from slow answers: %+v", skew)
	}
	if skew.ahead() < 10*time.Second {
		t.Fatalf("bound %s under the real 10 s offset: coverage could pass AWS time", skew.ahead())
	}
}

// An event a cut-short window could not count is counted when a later
// window of the same poll reads its minute in full, and only once.
func TestEventCountedWhenALaterWindowReadsItsMinute(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 30, 0, time.UTC)
	events := newPollEvents(productionAccount, testRegion, DefaultRoutineEvents())
	routine := changesEvent("r1", "UpdateInstanceInformation", "ssm.amazonaws.com", at, `{"userIdentity":{"type":"AWSService","invokedBy":"ssm.amazonaws.com"}}`)
	// The forward window stopped part-way: its read starts after this
	// event's minute.
	events.add(routine, at.Add(time.Minute))
	if len(events.countList()) != 0 {
		t.Fatal("an event outside the fully read minutes was counted")
	}
	// The back-fill reads that minute in full.
	events.add(routine, at.Add(-time.Hour))
	events.add(routine, at.Add(-time.Hour))
	if counts := events.countList(); len(counts) != 1 || counts[0].Events != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}

// Credential answers are credential failures, an opt-in refusal is a
// disabled region (a real gap until Settings records it), and a host that
// does not resolve is collector_down, never a disabled region.
func TestPollErrorReasonsNeverGuessARegionOff(t *testing.T) {
	healthy, failing := env{identity: AccountHealthHealthy}, env{identity: AccountHealthFailing}
	for code, want := range map[string]snapshot.ChangeGapReason{
		failing.resolve(errorCode(&smithy.GenericAPIError{Code: "UnrecognizedClientException"})): snapshot.GapCredentialsInvalid,
		failing.resolve(errorCode(&smithy.GenericAPIError{Code: "AuthFailure"})):                 snapshot.GapCredentialsInvalid,
		healthy.resolve(errorCode(&smithy.GenericAPIError{Code: "OptInRequired"})):               snapshot.GapRegionNotEnabled,
		CodeNotInRegion: snapshot.GapCollectorDown,
	} {
		if got := snapshot.ChangeGapReasonForCode(code); got != want {
			t.Errorf("code %s = %s, want %s", code, got, want)
		}
	}
}

// A disabled opt-in region answers UnrecognizedClientException. With the
// account's identity check healthy that is a region that is not enabled
// (a real gap until Settings records it, then confirmed for the whole
// stretch); with the check failing it is a credential failure. Inventory
// units follow the same rule.
func TestUnrecognizedClientLabelledByAccountHealth(t *testing.T) {
	regions := []string{testRegion, fakeaws.NotEnabledRegion}
	for _, tc := range []struct {
		health AccountHealth
		code   string
		reason snapshot.ChangeGapReason
	}{
		{AccountHealthHealthy, "region_disabled", snapshot.GapRegionNotEnabled},
		{AccountHealthUnknown, "region_disabled", snapshot.GapRegionNotEnabled},
		{AccountHealthFailing, "no_server_identity", snapshot.GapCredentialsInvalid},
	} {
		h := newHarness(t, fakeaws.ScenarioRegionNotEnabled, WithAccountHealth(func(string) AccountHealth { return tc.health }))
		report := mustSync(t, h, target(productionAccount, regions...), KindChanges, KindInventory)
		checked := map[string]bool{}
		for _, unit := range report.Units {
			// CloudTrail and Lambda answer UnrecognizedClientException;
			// Auto Scaling, a query service, answers InvalidClientTokenId.
			if unit.Unit.Region != fakeaws.NotEnabledRegion || unit.Unit.Service != snapshot.ChangeService && unit.Unit.Service != "lambda" && unit.Unit.Service != "auto-scaling" {
				continue
			}
			checked[unit.Unit.Service] = true
			if unit.ErrorCode != tc.code {
				t.Fatalf("health %d: %s/%s code = %q, want %q", tc.health, unit.Unit.Region, unit.Unit.Service, unit.ErrorCode, tc.code)
			}
		}
		if len(checked) != 3 {
			t.Fatalf("units checked = %v", checked)
		}
		scope := []snapshot.ScopeAccount{{Account: productionAccount, Regions: regions}}
		feed := snapshot.NewChangeFeed(h.store, snapshot.WithScope(func(context.Context) ([]snapshot.ScopeAccount, bool, error) { return scope, true, nil }))
		answer, err := feed.Answer(context.Background(), snapshot.FeedRequest{})
		if err != nil {
			t.Fatal(err)
		}
		reason := ""
		for _, gap := range answer.Gaps {
			if gap.Region == fakeaws.NotEnabledRegion {
				reason = gap.Reason
			}
		}
		if reason != string(tc.reason) || answer.Complete {
			t.Fatalf("health %d: gap %q (complete %v), want %q and a real gap", tc.health, reason, answer.Complete, tc.reason)
		}
		if tc.health != AccountHealthHealthy {
			continue
		}
		// Settings records the region as off: the whole stretch is the
		// confirmed state, not a gap.
		scope = []snapshot.ScopeAccount{{Account: productionAccount, Regions: []string{testRegion}, NotEnabled: []string{fakeaws.NotEnabledRegion}}}
		confirmed, err := feed.Answer(context.Background(), snapshot.FeedRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, gap := range confirmed.Gaps {
			if gap.Region == fakeaws.NotEnabledRegion && gap.Reason != string(snapshot.GapRegionNotEnabled) {
				t.Fatalf("confirmed region keeps gap %+v", gap)
			}
		}
		for _, entry := range confirmed.Regions {
			if entry.Region == fakeaws.NotEnabledRegion && entry.State != snapshot.RegionNotEnabled {
				t.Fatalf("regions = %+v", confirmed.Regions)
			}
		}
	}
}
