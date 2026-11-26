package issues

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// singleReplicaBackends are the backends that serve one process.
func singleReplicaBackends(t *testing.T) map[string]storage.Provider {
	t.Helper()
	file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return map[string]storage.Provider{"memory": storage.NewMemory(), "file": file}
}

func eventKinds(t *testing.T, store storage.AWSIssueStore, scope tenancy.OrgScope, id string) []string {
	t.Helper()
	events, err := store.AWSIssueEvents(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

// The evaluator writes what Advance decides, one account at a time: the
// first evaluation is the baseline, an evaluator built again over the same
// storage (a restart) is not, and an evaluation that changes nothing writes
// no event.
func TestEvaluatorBaselineSurvivesRestart(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			first, err := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			if err != nil {
				t.Fatal(err)
			}
			report, err := first.Evaluate(ctx)
			if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].Baselines) == 0 {
				t.Fatalf("first evaluation = %+v, %v", report, err)
			}
			now.Add(time.Minute)
			source.public(testAccount, "1", "docs-site", "uploads")
			restarted, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			report, err = restarted.Evaluate(ctx)
			if err != nil || len(report.Accounts[0].Baselines) != 0 {
				t.Fatalf("after a restart = %+v, %v", report, err)
			}
			actions, _ := NewActions(provider, scope, now.Now)
			list, err := actions.List(ctx, storage.AWSIssueQuery{})
			if err != nil || len(list) != 2 {
				t.Fatalf("issues = %+v, %v", list, err)
			}
			docs, uploads := list[0], list[1]
			if docs.Owner != "arn:aws:s3:::docs-site" {
				docs, uploads = uploads, docs
			}
			if docs.SinceKnown || !uploads.SinceKnown || !uploads.Since.Equal(start.Add(time.Minute)) {
				t.Fatalf("docs %+v, uploads %+v", docs, uploads)
			}
			_, events, _ := actions.Get(ctx, docs.ID)
			if len(events) != 1 || events[0].Detail != "existed when first read" {
				t.Fatalf("baseline events = %+v", events)
			}
			now.Add(time.Minute)
			if _, err := restarted.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			if kinds := eventKinds(t, restarted.Store(), scope, docs.ID); len(kinds) != 1 {
				t.Fatalf("an unchanged evaluation wrote events: %v", kinds)
			}
		})
	}
}

// A rejection that lands while the evaluator is writing wins: the
// evaluator's write conflicts, it reads the account again and keeps the
// issue rejected.
func TestRejectionMidEvaluationWins(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			evaluator, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			actions, _ := NewActions(provider, scope, now.Now)
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			// The next evaluation sees a volatile change it wants to write.
			source.public(testAccount, "2", "docs-site")
			rejected := 0
			evaluator.beforeWrite = func(ctx context.Context, account string) {
				if rejected > 0 {
					return
				}
				rejected++
				if _, err := actions.Reject(ctx, id, "ana@example.com", "public on purpose: docs site", ""); err != nil {
					t.Errorf("reject: %v", err)
				}
			}
			now.Add(time.Minute)
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			issue, events, err := actions.Get(ctx, id)
			if err != nil || issue.State != StateRejected || issue.Rejection == nil || !strings.Contains(issue.Title, "seen 2") {
				t.Fatalf("after the evaluation = %+v, %v", issue, err)
			}
			if len(events) != 2 || events[1].Kind != EventRejected {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// A healthy evaluation logs nothing.
func TestEvaluatorLogsNothingWhenHealthy(t *testing.T) {
	var log logLines
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	evaluator, _ := NewEvaluator(storage.NewMemory(), tenancy.DefaultOrgScope(), source, WithRules(publicBucketRules()),
		WithClock(now.Now), WithLogger(log.logf))
	for range 2 {
		if _, err := evaluator.Evaluate(context.Background()); err != nil {
			t.Fatal(err)
		}
		now.Add(time.Minute)
	}
	if lines := log.all(); len(lines) != 0 {
		t.Fatalf("healthy cycles logged %q", lines)
	}
}

// A failure that continues logs once when it starts, then at most once per
// ten minutes with a count; a different cause logs at once.
func TestEvaluatorFailureLogIsThrottled(t *testing.T) {
	var log logLines
	source := newFakeSource(testAccount)
	source.err = errors.New("snapshot store closed")
	now := &clock{now: start}
	evaluator, _ := NewEvaluator(storage.NewMemory(), tenancy.DefaultOrgScope(), source, WithRules(publicBucketRules()),
		WithClock(now.Now), WithLogger(log.logf))
	for range 5 {
		_, _ = evaluator.Evaluate(context.Background())
		now.Add(time.Minute)
	}
	lines := log.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "cannot evaluate accounts (inventory unreadable)") || !strings.Contains(lines[0], "check") {
		t.Fatalf("five failing minutes logged %q", lines)
	}
	now.Add(10 * time.Minute)
	_, _ = evaluator.Evaluate(context.Background())
	if lines = log.all(); len(lines) != 2 || !strings.Contains(lines[1], "still failing, 6 times") ||
		!strings.Contains(lines[1], "snapshot store closed") || !strings.Contains(lines[1], "check the AWS inventory storage") {
		t.Fatalf("after ten more minutes: %q", lines)
	}
}

// Triggers fold into one pending evaluation, and the loop waits out the
// interval since the last evaluation before it runs the next.
func TestTriggersRunAtMostOncePerInterval(t *testing.T) {
	source := newFakeSource(testAccount)
	now := &clock{now: start}
	evaluated := make(chan Report, 4)
	evaluator, _ := NewEvaluator(storage.NewMemory(), tenancy.DefaultOrgScope(), source, WithRules(publicBucketRules()),
		WithClock(now.Now), WithAfterEvaluate(func(_ context.Context, report Report) { evaluated <- report }))
	waits := make(chan time.Duration, 4)
	release := make(chan struct{})
	evaluator.wait = func(ctx context.Context, d time.Duration) bool {
		waits <- d
		select {
		case <-release:
		case <-ctx.Done():
			return false
		}
		now.Add(d)
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		<-evaluator.Stopped()
	}()
	evaluator.Start(ctx)
	receive(t, evaluated, "first evaluation")
	now.Add(10 * time.Second)
	evaluator.Trigger()
	if got := receive(t, waits, "wait before the second evaluation"); got != 50*time.Second {
		t.Fatalf("waited %s before the second evaluation, want the rest of the minute", got)
	}
	// Triggers that arrive while the loop waits fold into the evaluation
	// it is waiting to run.
	for range 4 {
		evaluator.Trigger()
	}
	release <- struct{}{}
	receive(t, evaluated, "second evaluation")
	if evaluator.Due() || len(waits) != 0 || len(evaluated) != 0 {
		t.Fatalf("five triggers ran more than one evaluation: due %t, waits %d", evaluator.Due(), len(waits))
	}
}

// A rule's baseline is its first assessment on the account: an evaluation
// that could not assess it uses nothing up, and what the first real
// assessment finds existed when first read.
func TestBaselineWaitsForFirstAssessment(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	source := newFakeSource(testAccount)
	source.read(testAccount, "s3", "eu-west-1", ReadNotRead)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	evaluator, _ := NewEvaluator(storage.NewMemory(), scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
	for range 3 {
		report, err := evaluator.Evaluate(ctx)
		if err != nil || len(report.Accounts[0].Baselines) != 0 || len(report.Accounts[0].Changes) != 0 {
			t.Fatalf("not assessed evaluation = %+v, %v", report, err)
		}
		now.Add(time.Minute)
	}
	source.read(testAccount, "s3", "eu-west-1", ReadOK)
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
	actions, _ := NewActions(evaluator.provider, scope, now.Now)
	issue, events, err := actions.Get(ctx, id)
	if err != nil || issue.SinceKnown || len(events) != 1 || events[0].Detail != "existed when first read" {
		t.Fatalf("first assessed issue = %+v, %+v, %v", issue, events, err)
	}
}

// A rule added in a later release baselines on its own first assessment of
// an account other rules evaluated long before.
func TestNewRuleBaselinesOnAnEvaluatedAccount(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	before, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
	if _, err := before.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	now.Add(24 * time.Hour)
	source.public(testAccount, "1", "docs-site", "uploads")
	upgraded := publicBucketRules()
	if err := upgraded.Register(&testRule{id: "bucket-reachable", kind: KindSecurity, inputs: []Input{{Service: "s3"}}}); err != nil {
		t.Fatal(err)
	}
	after, _ := NewEvaluator(provider, scope, source, WithRules(upgraded), WithClock(now.Now))
	if _, err := after.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	actions, _ := NewActions(provider, scope, now.Now)
	for id, want := range map[string]string{
		IssueID(testRuleID, "arn:aws:s3:::uploads"):           "found by rule " + testRuleID,
		IssueID("bucket-reachable", "arn:aws:s3:::uploads"):   "existed when first read",
		IssueID("bucket-reachable", "arn:aws:s3:::docs-site"): "existed when first read",
	} {
		_, events, err := actions.Get(ctx, id)
		if err != nil || len(events) != 1 || events[0].Detail != want {
			t.Fatalf("issue %s events = %+v, %v; want %q", id, events, err, want)
		}
	}
}

// An evaluation resolves an issue in a Region it read in full even while
// another Region's read is failing, and keeps the other Region's issue.
func TestRegionGapDoesNotBlockResolveInEvaluation(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	source.mu.Lock()
	source.buckets[testAccount] = append(source.buckets[testAccount], model.Resource{
		ARN: "arn:aws:s3:::us-bucket", Service: "s3", Type: "bucket", Account: testAccount, Region: "us-east-1", Name: "us-bucket", State: "public",
		Tags: map[string]string{"principals": "*"},
	})
	source.mu.Unlock()
	source.read(testAccount, "s3", "us-east-1", ReadOK)
	now := &clock{now: start}
	rules := publicBucketRules()
	first, _ := NewEvaluator(provider, scope, source, WithRules(rules), WithClock(now.Now))
	if report, err := first.Evaluate(ctx); err != nil || len(report.Accounts[0].Changes) != 2 {
		t.Fatalf("first evaluation = %+v, %v", report, err)
	}
	// Both buckets are gone, and us-east-1 is no longer readable.
	gone := newFakeSource(testAccount)
	gone.read(testAccount, "s3", "us-east-1", ReadNotRead)
	gone.public(testAccount, "2")
	now.Add(time.Minute)
	evaluator, _ := NewEvaluator(provider, scope, gone, WithRules(rules), WithClock(now.Now))
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	actions, _ := NewActions(provider, scope, now.Now)
	if issue, _, _ := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::docs-site")); issue.State != StateResolved {
		t.Fatalf("eu-west-1 issue = %+v", issue)
	}
	if issue, _, _ := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::us-bucket")); issue.State != StateOpen || issue.Confirm != CantConfirm {
		t.Fatalf("us-east-1 issue = %+v", issue)
	}
}

// One finding the store would refuse is left out, with a debug line, and
// the rest of the account still updates, every cycle.
func TestBadFindingDoesNotStallTheAccount(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	rules := NewRegistry()
	bad := Finding{Owner: "arn:aws:s3:::huge", Region: "eu-west-1", Severity: SeverityHigh, Title: strings.Repeat("x", 1001)}
	if err := rules.Register(&testRule{id: testRuleID, kind: KindSecurity, inputs: []Input{{Service: "s3"}}, also: []Finding{bad}}); err != nil {
		t.Fatal(err)
	}
	var debug, warn logLines
	now := &clock{now: start}
	evaluator, _ := NewEvaluator(storage.NewMemory(), scope, source, WithRules(rules), WithClock(now.Now), WithLogger(warn.logf), WithDebug(debug.logf))
	for cycle := range 2 {
		source.public(testAccount, string(rune('1'+cycle)), "docs-site")
		report, err := evaluator.Evaluate(ctx)
		if err != nil || len(report.Accounts) != 1 || report.Accounts[0].Dropped != 1 {
			t.Fatalf("cycle %d = %+v, %v", cycle, report, err)
		}
		now.Add(time.Minute)
	}
	actions, _ := NewActions(evaluator.provider, scope, now.Now)
	if issue, _, err := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::docs-site")); err != nil || !strings.Contains(issue.Title, "seen 2") {
		t.Fatalf("the good issue did not update: %+v, %v", issue, err)
	}
	if lines := debug.all(); len(lines) != 2 || !strings.Contains(lines[0], "arn:aws:s3:::huge") {
		t.Fatalf("debug lines = %q", lines)
	}
	lines := warn.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "rule "+testRuleID+": 1 findings left out (title over 1000 characters") ||
		!strings.Contains(lines[0], "LOG_LEVEL=debug") || !strings.Contains(lines[0], "check") {
		t.Fatalf("two cycles dropping a finding warned %q, want one throttled line", lines)
	}
}

// conflictingStore answers every issue write with a conflict.
type conflictingStore struct {
	storage.Provider
	storage.AWSIssueStore
}

func (conflictingStore) WriteAWSIssues(context.Context, tenancy.OrgScope, storage.AWSIssueBatch) error {
	return storage.ErrAWSIssueConflict
}

// A write that keeps conflicting is logged as that, not as storage being
// unavailable.
func TestFailureLogNamesTheRealCause(t *testing.T) {
	memory := storage.NewMemory()
	issueStore, _ := storage.AWSIssuesOf(memory)
	var log logLines
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	evaluator, _ := NewEvaluator(conflictingStore{Provider: memory, AWSIssueStore: issueStore}, tenancy.DefaultOrgScope(), source,
		WithRules(publicBucketRules()), WithLogger(log.logf), WithClock(func() time.Time { return start }))
	if _, err := evaluator.Evaluate(context.Background()); !errors.Is(err, storage.ErrAWSIssueConflict) {
		t.Fatalf("evaluation = %v", err)
	}
	lines := log.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "issues kept changing") || strings.Contains(lines[0], "storage unavailable") {
		t.Fatalf("log = %q", lines)
	}
}

// When the connector configuration no longer lists an account, its open
// issues resolve as no longer connected and its rejected ones stay. A
// failed configuration read resolves nothing; an unknown list logs
// nothing.
func TestDisconnectedAccountResolvesItsIssues(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site", "uploads")
	now := &clock{now: start}
	var connected []string
	var connectedErr error
	var log logLines
	evaluator, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now), WithLogger(log.logf),
		WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), connectedErr }))
	connected = []string{testAccount}
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	actions, _ := NewActions(provider, scope, now.Now)
	docs, uploads := IssueID(testRuleID, "arn:aws:s3:::docs-site"), IssueID(testRuleID, "arn:aws:s3:::uploads")
	if _, err := actions.Reject(ctx, docs, "ana@example.com", "public on purpose: docs site", ""); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{errors.New("settings store closed"), ErrConnectedUnknown} {
		connected, connectedErr = nil, failure
		now.Add(time.Minute)
		_, _ = evaluator.Evaluate(ctx)
		if issue, _, _ := actions.Get(ctx, uploads); issue.State != StateOpen {
			t.Fatalf("a failed configuration read (%v) changed the issue: %+v", failure, issue)
		}
	}
	if lines := log.all(); len(lines) != 1 || !strings.Contains(lines[0], "connector account list unreadable: settings store closed; evaluation continues") {
		t.Fatalf("log = %q", lines)
	}
	connected, connectedErr = []string{}, nil
	now.Add(time.Minute)
	report, err := evaluator.Evaluate(ctx)
	if err != nil || len(report.Accounts) != 1 || !report.Accounts[0].Disconnected {
		t.Fatalf("evaluation = %+v, %v", report, err)
	}
	issue, events, _ := actions.Get(ctx, uploads)
	if issue.State != StateResolved || events[len(events)-1].Detail != "account no longer connected" {
		t.Fatalf("open issue = %+v, %+v", issue, events)
	}
	if issue, _, _ := actions.Get(ctx, docs); issue.State != StateRejected {
		t.Fatalf("rejected issue = %+v", issue)
	}
	now.Add(time.Minute)
	if report, err := evaluator.Evaluate(ctx); err != nil || len(report.Accounts) != 0 {
		t.Fatalf("a second pass = %+v, %v", report, err)
	}
}

// A dropped finding takes no baseline for its rule and Region: once it
// fits the store it still reads as existed when first read.
func TestDroppedFindingLeavesItsBaselineOpen(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1")
	rule := &testRule{id: testRuleID, kind: KindSecurity, inputs: []Input{{Service: "s3"}}}
	rule.also = []Finding{{Owner: "arn:aws:s3:::huge", Region: "eu-west-1", Severity: SeverityHigh, Title: strings.Repeat("x", 1001)}}
	rules := NewRegistry()
	if err := rules.Register(rule); err != nil {
		t.Fatal(err)
	}
	now := &clock{now: start}
	var warn logLines
	evaluator, _ := NewEvaluator(provider, scope, source, WithRules(rules), WithClock(now.Now), WithLogger(warn.logf), WithDebug(func(string, ...any) {}))
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	store, _ := storage.AWSIssuesOf(provider)
	held, _ := store.AWSIssueBaselines(ctx, scope, testAccount)
	for _, baseline := range held {
		if baseline.Region == "eu-west-1" {
			t.Fatalf("a baseline was taken where a finding was dropped: %+v", held)
		}
	}
	now.Add(time.Hour)
	rule.also[0].Title = "Bucket huge is public"
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	actions, _ := NewActions(provider, scope, now.Now)
	issue, events, err := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::huge"))
	if err != nil || issue.SinceKnown || len(events) != 1 || events[0].Detail != "existed when first read" {
		t.Fatalf("after it shrank = %+v, %+v, %v", issue, events, err)
	}
	if _, ok := evaluator.failures.seen["account "+testAccount+" rule "+testRuleID]; ok {
		t.Fatal("the drop warning was not cleared once the rule dropped nothing")
	}
	if lines := warn.all(); len(lines) != 1 {
		t.Fatalf("warnings = %q", lines)
	}
}

// failingStore fails every issue write while fail is set.
type failingStore struct {
	storage.Provider
	storage.AWSIssueStore
	fail *bool
}

func (store failingStore) WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch) error {
	if *store.fail {
		return errors.New("database down")
	}
	return store.AWSIssueStore.WriteAWSIssues(ctx, scope, batch)
}

// A failure is forgotten when its step succeeds again, so the next failure
// is logged at once: the accounts read, the connector account list, an
// account's evaluation and a disconnect.
func TestFailureLogClearsWhenTheStepSucceeds(t *testing.T) {
	ctx := context.Background()
	memory := storage.NewMemory()
	issueStore, _ := storage.AWSIssuesOf(memory)
	fail := false
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	var log logLines
	connected := []string{testAccount}
	var connectedErr error
	evaluator, _ := NewEvaluator(failingStore{Provider: memory, AWSIssueStore: issueStore, fail: &fail}, tenancy.DefaultOrgScope(), source,
		WithRules(publicBucketRules()), WithClock(now.Now), WithLogger(log.logf),
		WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), connectedErr }))
	step := func(setup func()) {
		setup()
		_, _ = evaluator.Evaluate(ctx)
		now.Add(time.Minute)
	}
	readErr := errors.New("snapshot store closed")
	settingsErr := errors.New("settings store closed")
	step(func() { source.err = readErr })
	step(func() { source.err = nil })
	step(func() { source.err = readErr })
	step(func() { source.err, connectedErr = nil, settingsErr })
	step(func() { connectedErr = nil })
	step(func() { connectedErr = settingsErr })
	step(func() { connectedErr, fail = nil, true })
	step(func() { fail = false })
	step(func() { fail = true })
	// The account leaves the connector while writes fail, then they work.
	step(func() { connected = nil })
	step(func() { fail = false })
	lines := log.all()
	counts := map[string]int{}
	for _, line := range lines {
		switch {
		case strings.Contains(line, "cannot evaluate accounts (inventory unreadable)"):
			counts["accounts"]++
		case strings.Contains(line, "connector account list unreadable"):
			counts["connector"]++
		case strings.Contains(line, "cannot evaluate account "+testAccount):
			counts["account"]++
		}
	}
	// The failing disconnect continues the account's failure, so it adds
	// no line; its success clears it.
	if counts["accounts"] != 2 || counts["connector"] != 2 || counts["account"] != 2 || len(lines) != 6 {
		t.Fatalf("lines = %q", lines)
	}
	if len(evaluator.failures.seen) != 0 {
		t.Fatalf("failures still held after every step succeeded: %+v", evaluator.failures.seen)
	}
}

// An account removed from the connector and connected again behaves like
// a first connect: what is still there opens again as existed when first
// read, never as a regress, and a rejection still holds.
func TestReconnectReopensAsFirstRead(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site", "uploads")
	now := &clock{now: start}
	connected := []string{testAccount}
	evaluator, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now),
		WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), nil }))
	actions, _ := NewActions(provider, scope, now.Now)
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	docs, uploads := IssueID(testRuleID, "arn:aws:s3:::docs-site"), IssueID(testRuleID, "arn:aws:s3:::uploads")
	if _, err := actions.Reject(ctx, docs, "ana@example.com", "public on purpose: docs site", ""); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][]string{{}, {testAccount}, {testAccount}} {
		connected = step
		now.Add(time.Hour)
		if _, err := evaluator.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	issue, events, _ := actions.Get(ctx, uploads)
	last := events[len(events)-1]
	if issue.State != StateOpen || issue.SinceKnown || last.Kind != EventOpened || last.Detail != "existed when first read" {
		t.Fatalf("after the reconnect = %+v, %+v", issue, events)
	}
	for _, id := range []string{docs, uploads} {
		_, events, _ := actions.Get(ctx, id)
		for _, event := range events {
			if event.Kind == EventRegressed {
				t.Fatalf("issue %s regressed on reconnect: %+v", id, events)
			}
		}
	}
	if issue, _, _ := actions.Get(ctx, docs); issue.State != StateRejected {
		t.Fatalf("rejection after the reconnect = %+v", issue)
	}
}

// A Region removed from the connector resolves its open issues as no
// longer read and keeps its rejections; nothing opens there; read again,
// what is still there opens as existed when first read.
func TestRemovedRegionResolvesAndReopensAsFirstRead(t *testing.T) {
	ctx := context.Background()
	scope := tenancy.DefaultOrgScope()
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	source.add(testAccount, "us-east-1", "us-open")
	source.add(testAccount, "us-east-1", "us-rejected")
	source.read(testAccount, "s3", "us-east-1", ReadOK)
	now := &clock{now: start}
	regions := []string{"eu-west-1", "us-east-1"}
	var connectedErr error
	evaluator, _ := NewEvaluator(provider, scope, source, WithRules(publicBucketRules()), WithClock(now.Now),
		WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) {
			return []ConnectedAccount{{Account: testAccount, Regions: regions}}, connectedErr
		}))
	actions, _ := NewActions(provider, scope, now.Now)
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	docs := IssueID(testRuleID, "arn:aws:s3:::docs-site")
	usOpen, usRejected := IssueID(testRuleID, "arn:aws:s3:::us-open"), IssueID(testRuleID, "arn:aws:s3:::us-rejected")
	if _, err := actions.Reject(ctx, usRejected, "ana@example.com", "public on purpose: test site", ""); err != nil {
		t.Fatal(err)
	}
	// A failed configuration read removes nothing.
	regions, connectedErr = []string{"eu-west-1"}, errors.New("settings store closed")
	now.Add(time.Hour)
	_, _ = evaluator.Evaluate(ctx)
	if issue, _, _ := actions.Get(ctx, usOpen); issue.State != StateOpen {
		t.Fatalf("a failed read removed the Region: %+v", issue)
	}
	connectedErr = nil
	source.add(testAccount, "us-east-1", "us-new")
	for range 2 {
		now.Add(time.Hour)
		if _, err := evaluator.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	issue, events, _ := actions.Get(ctx, usOpen)
	if issue.State != StateResolved || events[len(events)-1].Detail != "Region no longer read" || len(events) != 2 {
		t.Fatalf("issue in the removed Region = %+v, %+v", issue, events)
	}
	if issue, events, _ := actions.Get(ctx, usRejected); issue.State != StateRejected || len(events) != 2 {
		t.Fatalf("rejection in the removed Region = %+v, %+v", issue, events)
	}
	if _, _, err := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::us-new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an issue opened in a removed Region: %v", err)
	}
	if issue, _, _ := actions.Get(ctx, docs); issue.State != StateOpen {
		t.Fatalf("the kept Region's issue = %+v", issue)
	}
	regions = []string{"eu-west-1", "us-east-1"}
	now.Add(time.Hour)
	if _, err := evaluator.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{usOpen, IssueID(testRuleID, "arn:aws:s3:::us-new")} {
		issue, events, _ := actions.Get(ctx, id)
		last := events[len(events)-1]
		if issue.State != StateOpen || last.Kind != EventOpened || last.Detail != "existed when first read" {
			t.Fatalf("issue %s after the Region is read again = %+v, %+v", id, issue, events)
		}
	}
	if issue, _, _ := actions.Get(ctx, usRejected); issue.State != StateRejected {
		t.Fatalf("rejection after the Region is read again = %+v", issue)
	}
}

// A replica whose clock is two minutes slow disconnects an account right
// after another replica baselined it, and later evaluates the reconnect:
// the store orders the reset after the baseline and the new baseline after
// the reset, so the reconnect is still a first connect.
func TestSlowReplicaResetStillOrdersAfterTheBaseline(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			ctx := context.Background()
			scope := tenancy.NewOrgScope(uniqueOrg("slow"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			connected := []string{testAccount}
			read := func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), nil }
			right := &clock{now: start}
			slow := &clock{now: start.Add(-2 * time.Minute)}
			onTime, _ := NewEvaluator(set.a, scope, source, WithRules(publicBucketRules()), WithClock(right.Now), WithConnectedAccounts(read))
			behind, _ := NewEvaluator(set.b, scope, source, WithRules(publicBucketRules()), WithClock(slow.Now), WithConnectedAccounts(read))
			if _, err := onTime.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			connected = nil
			if _, err := behind.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			connected = []string{testAccount}
			slow.Add(time.Minute)
			if _, err := behind.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			actions, _ := NewActions(set.a, scope, right.Now)
			issue, events, _ := actions.Get(ctx, IssueID(testRuleID, "arn:aws:s3:::docs-site"))
			last := events[len(events)-1]
			if issue.State != StateOpen || last.Kind != EventOpened || last.Detail != "existed when first read" {
				t.Fatalf("after the reconnect = %+v, %+v", issue, events)
			}
			rows, _ := behind.Store().AWSIssueBaselines(ctx, scope, testAccount)
			held := baselinesByAccount(rows)[testAccount]
			if held == nil || !held.valid(storage.AWSIssueBaselineKey{AccountID: testAccount, Rule: testRuleID, Region: "eu-west-1"}) {
				t.Fatalf("the reconnect's baseline does not hold after the reset: %+v", rows)
			}
		})
	}
}
