package issues

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// severityRule finds every public s3 bucket like testRule, at the severity
// its "severity" tag names (critical when it names none).
type severityRule struct{ testRule }

func (rule *severityRule) Evaluate(view View) []Finding {
	out := rule.testRule.Evaluate(view)
	for index := range out {
		for _, resource := range view.Resources() {
			if resource.ARN != out[index].Owner {
				continue
			}
			out[index].Severity = SeverityCritical
			if severity := Severity(resource.Tags["severity"]); severity.Valid() {
				out[index].Severity = severity
			}
			out[index].Steps = []Step{{Text: "Block public access on the bucket", Command: "aws s3api put-public-access-block --bucket " + resource.Name}}
		}
	}
	return out
}

func severityRules() *Registry {
	rules := NewRegistry()
	if err := rules.Register(&severityRule{testRule{id: testRuleID, kind: KindSecurity, inputs: []Input{{Service: "s3"}}}}); err != nil {
		panic(err)
	}
	return rules
}

// setSeverity sets the severity tag of every bucket of the account.
func (source *fakeSource) setSeverity(account string, severity Severity) {
	source.mu.Lock()
	defer source.mu.Unlock()
	for index := range source.buckets[account] {
		source.buckets[account][index].Tags["severity"] = string(severity)
	}
}

// setMaterial changes the principals that reach every bucket: material
// evidence.
func (source *fakeSource) setMaterial(account, principals string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	for index := range source.buckets[account] {
		source.buckets[account][index].Tags["principals"] = principals
	}
}

// recordingSink keeps every alert it was asked to send. A sink with dies
// set panics instead, as a process that stopped before it sent.
type recordingSink struct {
	mu      sync.Mutex
	sent    []Alert
	digests [][]Alert
	dies    bool
	err     error
}

func (sink *recordingSink) Send(_ context.Context, alert Alert) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.dies {
		panic("process stopped before it sent")
	}
	sink.sent = append(sink.sent, alert)
	return sink.err
}

func (sink *recordingSink) SendDigest(_ context.Context, alerts []Alert) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.dies {
		panic("process stopped before it sent")
	}
	sink.digests = append(sink.digests, append([]Alert(nil), alerts...))
	return sink.err
}

func (sink *recordingSink) alerts() ([]Alert, [][]Alert) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]Alert(nil), sink.sent...), append([][]Alert(nil), sink.digests...)
}

func (sink *recordingSink) kinds() []AlertKind {
	sent, _ := sink.alerts()
	out := make([]AlertKind, 0, len(sent))
	for _, alert := range sent {
		out = append(out, alert.Kind)
	}
	return out
}

// alerting is one replica: an evaluator that pumps alerts after each
// evaluation.
type alerting struct {
	evaluator *Evaluator
	alerter   *Alerter
	sink      *recordingSink
}

func newAlerting(t *testing.T, provider storage.Provider, scope tenancy.OrgScope, source Source, rules *Registry, now *clock, options ...AlerterOption) *alerting {
	t.Helper()
	sink := &recordingSink{}
	alerter, err := NewAlerter(provider, scope, sink, append([]AlerterOption{WithAlertClock(now.Now), WithUnreadFinder(NewUnreadFinder(source, rules))}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := NewEvaluator(provider, scope, source, WithRules(rules), WithClock(now.Now),
		WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
	if err != nil {
		t.Fatal(err)
	}
	return &alerting{evaluator: evaluator, alerter: alerter, sink: sink}
}

func (replica *alerting) evaluate(t *testing.T) {
	t.Helper()
	if _, err := replica.evaluator.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// transitions counts the opened and regressed events of every issue.
func transitions(t *testing.T, store storage.AWSIssueStore, scope tenancy.OrgScope) int {
	t.Helper()
	records, err := store.AWSIssues(context.Background(), scope, storage.AWSIssueQuery{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, record := range records {
		for _, kind := range eventKinds(t, store, scope, record.ID) {
			if kind == storage.AWSIssueEventOpened || kind == storage.AWSIssueEventRegressed {
				count++
			}
		}
	}
	return count
}

// Evidence that comes and goes for 100 evaluations, every read complete:
// the issue opens, resolves and regresses again and again, and each
// opening and regress sends exactly one alert, each with its own key.
func TestFlappingEvidenceSendsOneAlertPerTransition(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			now := &clock{now: start}
			replica := newAlerting(t, provider, scope, source, severityRules(), now)
			for cycle := range 100 {
				if cycle%2 == 0 {
					source.public(testAccount, "1", "docs-site")
				} else {
					source.public(testAccount, "1")
				}
				replica.evaluate(t)
				now.Add(time.Minute)
			}
			store, _ := storage.AWSIssuesOf(provider)
			moved := transitions(t, store, scope)
			sent, digests := replica.sink.alerts()
			if moved != 50 || len(sent) != moved || len(digests) != 0 {
				t.Fatalf("%d transitions sent %d alerts and %d digests; want one alert each", moved, len(sent), len(digests))
			}
			keys := map[string]bool{}
			for index, alert := range sent {
				want := AlertRegressed
				if index == 0 {
					want = AlertOpened
				}
				if alert.Kind != want || keys[alert.Key] {
					t.Fatalf("alert %d = %s key %s (seen before: %v)", index, alert.Kind, alert.Key, keys[alert.Key])
				}
				keys[alert.Key] = true
			}
		})
	}
}

// Reads that fail and recover for 100 evaluations while the issue stays
// found: one alert for the opening and one coverage notice, never one per
// read.
func TestFlappingReadsSendOneCoverageNotice(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			replica := newAlerting(t, provider, scope, source, severityRules(), now)
			for cycle := range 100 {
				read := ReadOK
				if cycle%2 == 1 {
					read = ReadNotRead
				}
				source.read(testAccount, "s3", "eu-west-1", read)
				replica.evaluate(t)
				now.Add(time.Minute)
			}
			if kinds := replica.sink.kinds(); len(kinds) != 2 || kinds[0] != AlertOpened || kinds[1] != AlertCoverage {
				t.Fatalf("alerts = %v; want one opening and one coverage notice", kinds)
			}
			sent, _ := replica.sink.alerts()
			if notice := sent[1]; len(notice.Unread) != 1 || notice.Unread[0].Source != "s3" {
				t.Fatalf("the coverage notice names %+v; want the s3 read", notice.Unread)
			}
		})
	}
}

// A capped read leaves the issue unconfirmed but is not a source that can
// no longer be read: no coverage notice.
func TestCappedReadSendsNoCoverageNotice(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now)
	replica.evaluate(t)
	source.read(testAccount, "s3", "eu-west-1", ReadCapped)
	source.public(testAccount, "1")
	replica.evaluate(t)
	if kinds := replica.sink.kinds(); len(kinds) != 1 || kinds[0] != AlertOpened {
		t.Fatalf("alerts = %v; want the opening only", kinds)
	}
}

// An issue that already can't be confirmed because a read was capped sends
// its coverage notice when the input then stops being read, although the
// issue itself does not change; and only one.
func TestCappedThenNotReadSendsOneCoverageNotice(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now)
	replica.evaluate(t)
	source.read(testAccount, "s3", "eu-west-1", ReadCapped)
	source.public(testAccount, "1")
	now.Add(time.Minute)
	replica.evaluate(t)
	actions, _ := NewActions(provider, tenancy.DefaultOrgScope(), now.Now)
	id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
	capped, _, err := actions.Get(context.Background(), id)
	if err != nil || capped.Confirm != CantConfirm {
		t.Fatalf("capped issue = %+v, %v", capped, err)
	}
	for range 3 {
		now.Add(time.Minute)
		replica.evaluate(t)
	}
	if kinds := replica.sink.kinds(); len(kinds) != 1 {
		t.Fatalf("alerts while capped = %v; want the opening only", kinds)
	}
	if still, _, _ := actions.Get(context.Background(), id); still.Version != capped.Version {
		t.Fatalf("quiet pumps wrote the issue: version %d -> %d", capped.Version, still.Version)
	}
	source.read(testAccount, "s3", "eu-west-1", ReadNotRead)
	for range 3 {
		now.Add(time.Minute)
		replica.evaluate(t)
	}
	if kinds := replica.sink.kinds(); len(kinds) != 2 || kinds[1] != AlertCoverage {
		t.Fatalf("alerts once not read = %v; want one coverage notice", kinds)
	}
}

// An issue that changes without opening or regressing again (a user
// acknowledges it, its evidence is read again) sends nothing more.
func TestAcknowledgedIssueIsNotAlertedAgain(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			replica := newAlerting(t, provider, scope, source, severityRules(), now)
			replica.evaluate(t)
			actions, _ := NewActions(provider, scope, now.Now)
			if _, err := actions.Acknowledge(context.Background(), IssueID(testRuleID, "arn:aws:s3:::docs-site"), "alice"); err != nil {
				t.Fatal(err)
			}
			for cycle := range 5 {
				now.Add(time.Minute)
				source.public(testAccount, string(rune('a'+cycle)), "docs-site")
				source.setMaterial(testAccount, "principal-"+string(rune('a'+cycle)))
				replica.evaluate(t)
			}
			if kinds := replica.sink.kinds(); len(kinds) != 1 || kinds[0] != AlertOpened {
				t.Fatalf("alerts = %v; want the opening only", kinds)
			}
		})
	}
}

// A rejected issue sends nothing while its rejection holds, also when its
// source stops being read; once its material evidence changes it comes
// back and sends one alert as a regress that says why.
func TestRejectedIssueNeverAlertsAndReopensAsARegress(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			replica := newAlerting(t, provider, scope, source, severityRules(), now)
			replica.evaluate(t)
			actions, _ := NewActions(provider, scope, now.Now)
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			if _, err := actions.Reject(context.Background(), id, "alice", "public on purpose: the docs site", ""); err != nil {
				t.Fatal(err)
			}
			for cycle := range 10 {
				now.Add(time.Minute)
				source.public(testAccount, string(rune('a'+cycle)), "docs-site")
				read := ReadOK
				if cycle%2 == 1 {
					read = ReadNotRead
				}
				source.read(testAccount, "s3", "eu-west-1", read)
				replica.evaluate(t)
			}
			if kinds := replica.sink.kinds(); len(kinds) != 1 {
				t.Fatalf("alerts while rejected = %v; want only the first opening", kinds)
			}
			source.read(testAccount, "s3", "eu-west-1", ReadOK)
			source.setMaterial(testAccount, "arn:aws:iam::999988887777:root")
			now.Add(time.Minute)
			replica.evaluate(t)
			replica.evaluate(t)
			sent, _ := replica.sink.alerts()
			if len(sent) != 2 || sent[1].Kind != AlertRegressed || !strings.Contains(sent[1].Detail, "material evidence changed") {
				t.Fatalf("alerts = %+v; want one regress that says the evidence changed", sent)
			}
		})
	}
}

// Medium and low issues never alert, but their likely cause is looked up
// when they open, like every issue's.
func TestMediumAndLowNeverAlertButGetACause(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	source.setSeverity(testAccount, SeverityMedium)
	source.add(testAccount, "eu-west-1", "logs")
	now := &clock{now: start}
	looked := 0
	finder := func(_ context.Context, issue Issue) (*Cause, error) {
		looked++
		return &Cause{Status: "found", Text: "Bucket policy changed by deployer, 5 min before", Checked: now.Now()}, nil
	}
	source.mu.Lock()
	source.buckets[testAccount][1].Tags["severity"] = string(SeverityLow)
	source.mu.Unlock()
	replica := newAlerting(t, provider, scope, source, severityRules(), now, WithCauseFinder(finder))
	for range 5 {
		replica.evaluate(t)
		now.Add(time.Minute)
	}
	if kinds := replica.sink.kinds(); len(kinds) != 0 {
		t.Fatalf("medium and low issues alerted: %v", kinds)
	}
	actions, _ := NewActions(provider, scope, now.Now)
	list, err := actions.List(context.Background(), storage.AWSIssueQuery{})
	if err != nil || len(list) != 2 {
		t.Fatalf("issues = %d, %v", len(list), err)
	}
	for _, issue := range list {
		if !issue.Cause.Found() {
			t.Fatalf("issue %s has no likely cause: %+v", issue.Title, issue.Cause)
		}
	}
	if looked != 2 {
		t.Fatalf("likely cause looked up %d times; want once per opening", looked)
	}
}

// A critical alert carries the likely cause the pump looked up.
func TestCriticalAlertCarriesTheLikelyCause(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	finder := func(context.Context, Issue) (*Cause, error) {
		return &Cause{Status: "found", Text: "Bucket policy changed by deployer, 5 min before", Checked: now.Now()}, nil
	}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now, WithCauseFinder(finder))
	replica.evaluate(t)
	sent, _ := replica.sink.alerts()
	if len(sent) != 1 || !sent[0].Issue.Cause.Found() || sent[0].Issue.Severity != SeverityCritical {
		t.Fatalf("alerts = %+v", sent)
	}
}

func highBuckets(source *fakeSource, names ...string) {
	for _, name := range names {
		source.add(testAccount, "eu-west-1", name)
	}
	source.setSeverity(testAccount, SeverityHigh)
}

// More than three high issues opening in an hour go out as one digest;
// three or fewer go out one by one, and an hour later the count starts
// again.
func TestHighIssuesBatchIntoOneDigest(t *testing.T) {
	t.Run("five at once", func(t *testing.T) {
		source := newFakeSource(testAccount)
		highBuckets(source, "a", "b", "c", "d", "e")
		now := &clock{now: start}
		replica := newAlerting(t, storage.NewMemory(), tenancy.DefaultOrgScope(), source, severityRules(), now)
		replica.evaluate(t)
		replica.evaluate(t)
		sent, digests := replica.sink.alerts()
		if len(sent) != 0 || len(digests) != 1 || len(digests[0]) != 5 {
			t.Fatalf("sent %d alone and digests %v; want one digest of 5", len(sent), len(digests))
		}
	})
	t.Run("three, a lone fourth within the hour, two more, one after the hour", func(t *testing.T) {
		source := newFakeSource(testAccount)
		highBuckets(source, "a", "b", "c")
		now := &clock{now: start}
		replica := newAlerting(t, storage.NewMemory(), tenancy.DefaultOrgScope(), source, severityRules(), now)
		replica.evaluate(t)
		if sent, digests := replica.sink.alerts(); len(sent) != 3 || len(digests) != 0 {
			t.Fatalf("three high issues: %d alone, %d digests; want three alone", len(sent), len(digests))
		}
		now.Add(20 * time.Minute)
		highBuckets(source, "d")
		replica.evaluate(t)
		if sent, digests := replica.sink.alerts(); len(sent) != 4 || len(digests) != 0 {
			t.Fatalf("a lone fourth in the hour: %d alone, digests %v; want it alone, never a digest of one", len(sent), digests)
		}
		now.Add(20 * time.Minute)
		highBuckets(source, "e", "f")
		replica.evaluate(t)
		if sent, digests := replica.sink.alerts(); len(sent) != 4 || len(digests) != 1 || len(digests[0]) != 2 {
			t.Fatalf("two more in the hour: %d alone, digests %v; want one digest of two", len(sent), digests)
		}
		now.Add(2 * time.Hour)
		highBuckets(source, "g")
		replica.evaluate(t)
		if sent, digests := replica.sink.alerts(); len(sent) != 5 || len(digests) != 1 {
			t.Fatalf("one more after the hour: %d alone, %d digests; want it alone", len(sent), len(digests))
		}
	})
}

// flakySink fails the first failures sends, then delivers. It counts the
// attempts per key and the messages delivered.
type flakySink struct {
	mu        sync.Mutex
	failures  int
	attempts  map[string]int
	delivered []Alert
}

func newFlakySink(failures int) *flakySink {
	return &flakySink{failures: failures, attempts: map[string]int{}}
}

func (sink *flakySink) try(alerts ...Alert) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, alert := range alerts {
		sink.attempts[alert.Issue.ID+" "+alert.Key]++
	}
	if sink.failures != 0 {
		sink.failures--
		return errors.New("slack: connection refused")
	}
	sink.delivered = append(sink.delivered, alerts...)
	return nil
}

func (sink *flakySink) Send(_ context.Context, alert Alert) error { return sink.try(alert) }

func (sink *flakySink) SendDigest(_ context.Context, alerts []Alert) error {
	return sink.try(alerts...)
}

func (sink *flakySink) counts() (int, map[string]int) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	attempts := map[string]int{}
	for key, count := range sink.attempts {
		attempts[key] = count
	}
	return len(sink.delivered), attempts
}

// flakyReplica is one replica whose alerts go to sink.
func flakyReplica(t *testing.T, provider storage.Provider, source Source, now *clock, sink AlertSink, options ...AlerterOption) *Evaluator {
	t.Helper()
	alerter, err := NewAlerter(provider, tenancy.DefaultOrgScope(), sink, append([]AlerterOption{WithAlertClock(now.Now)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	evaluator, err := NewEvaluator(provider, tenancy.DefaultOrgScope(), source, WithRules(severityRules()), WithClock(now.Now),
		WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
	if err != nil {
		t.Fatal(err)
	}
	return evaluator
}

// A process that stops after it recorded an alert and before it sent it
// leaves the alert pending: the replica that starts over sends it, once,
// under the same key.
func TestCrashBetweenRecordAndSendDeliversOnce(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			dying := newAlerting(t, provider, scope, source, severityRules(), now)
			dying.sink.dies = true
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the dying replica sent")
					}
				}()
				_, _ = dying.evaluator.Evaluate(context.Background())
			}()
			store, _ := storage.AWSIssuesOf(provider)
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			if kinds := eventKinds(t, store, scope, id); len(kinds) != 2 || kinds[1] != storage.AWSIssueEventAlerted {
				t.Fatalf("events = %v; the alert is recorded before the send", kinds)
			}
			sink := newFlakySink(0)
			restarted := flakyReplica(t, provider, source, now, sink)
			for range 5 {
				now.Add(time.Minute)
				if _, err := restarted.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			delivered, attempts := sink.counts()
			if delivered != 1 || len(attempts) != 1 {
				t.Fatalf("the restarted replica delivered %d alerts over attempts %v; want the pending one, once", delivered, attempts)
			}
		})
	}
}

// A send that fails stays pending and is sent again once its wait is over,
// under the same key: exactly one message is delivered, the failure is
// logged once, and nothing is sent after the delivery.
func TestFailedSendIsRetriedAndDeliveredOnce(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			lines := &logLines{}
			sink := newFlakySink(1)
			evaluator := flakyReplica(t, provider, source, now, sink, WithAlertLogger(lines.logf))
			evaluate := func() {
				t.Helper()
				if _, err := evaluator.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			evaluate()
			now.Add(RetryAfter / 2)
			evaluate()
			if delivered, attempts := sink.counts(); delivered != 0 || len(attempts) != 1 {
				t.Fatalf("before the wait is over: delivered %d, attempts %v; want one failed attempt", delivered, attempts)
			}
			for range 5 {
				now.Add(RetryAfter)
				evaluate()
			}
			delivered, attempts := sink.counts()
			for key, count := range attempts {
				if count != 2 {
					t.Fatalf("%s was tried %d times; want the failure and one retry", key, count)
				}
			}
			if delivered != 1 || len(attempts) != 1 {
				t.Fatalf("delivered %d over attempts %v; want exactly one message", delivered, attempts)
			}
			if logged := lines.all(); len(logged) != 1 || !strings.Contains(logged[0], "alert channel settings") {
				t.Fatalf("log = %q; want one line for the failure", logged)
			}
		})
	}
}

// A send that keeps failing is tried with a growing wait and given up a
// day after it was recorded, with one warning that names the failure and
// what to check; nothing is tried after it.
func TestFailingSendIsGivenUpAfterADayWithOneWarning(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	lines := &logLines{}
	sink := newFlakySink(-1)
	evaluator := flakyReplica(t, provider, source, now, sink, WithAlertLogger(lines.logf))
	for range 30 * 4 {
		if _, err := evaluator.Evaluate(context.Background()); err != nil {
			t.Fatal(err)
		}
		now.Add(15 * time.Minute)
	}
	_, attempts := sink.counts()
	var tried int
	for _, count := range attempts {
		tried = count
	}
	// 1, 2, 4 ... 32 minutes, then hourly, over 24 hours: about 30 tries,
	// never one per evaluation (120).
	if len(attempts) != 1 || tried < 10 || tried > 40 {
		t.Fatalf("attempts = %v; want a backoff that stops at a day", attempts)
	}
	var gaveUp []string
	for _, line := range lines.all() {
		if strings.Contains(line, "given up") {
			gaveUp = append(gaveUp, line)
		}
	}
	if len(gaveUp) != 1 || !strings.Contains(gaveUp[0], "slack: connection refused") || !strings.Contains(gaveUp[0], "alert channel settings") {
		t.Fatalf("give-up lines = %q; want one that names the failure and what to check", gaveUp)
	}
	actions, _ := NewActions(provider, tenancy.DefaultOrgScope(), now.Now)
	issue, _, _ := actions.Get(context.Background(), IssueID(testRuleID, "arn:aws:s3:::docs-site"))
	if issue.Alerted == nil || len(issue.Alerted.Pending) != 0 {
		t.Fatalf("pending after giving up = %+v", issue.Alerted)
	}
}

// A digest that fails is sent again as the same digest: the same issues,
// under the same group key.
func TestFailedDigestIsRetriedAsTheSameDigest(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	highBuckets(source, "a", "b", "c", "d")
	now := &clock{now: start}
	groups := map[string]int{}
	sink := &groupSink{flakySink: newFlakySink(1), groups: groups}
	evaluator := flakyReplica(t, provider, source, now, sink)
	for range 3 {
		if _, err := evaluator.Evaluate(context.Background()); err != nil {
			t.Fatal(err)
		}
		now.Add(RetryAfter)
	}
	delivered, _ := sink.counts()
	if delivered != 4 || len(groups) != 1 {
		t.Fatalf("delivered %d alerts under groups %v; want the four in one digest key", delivered, groups)
	}
	for _, count := range groups {
		if count != 2 {
			t.Fatalf("digest sent %d times; want the failure and one retry", count)
		}
	}
}

// groupSink counts digest sends per group key.
type groupSink struct {
	*flakySink
	groups map[string]int
}

func (sink *groupSink) SendDigest(ctx context.Context, alerts []Alert) error {
	sink.mu.Lock()
	sink.groups[alerts[0].Group]++
	sink.mu.Unlock()
	return sink.flakySink.SendDigest(ctx, alerts)
}

// A pending alert of an issue a user rejects before it is delivered is
// dropped unsent.
func TestPendingAlertOfARejectedIssueIsDropped(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	sink := newFlakySink(1)
	evaluator := flakyReplica(t, provider, source, now, sink)
	if _, err := evaluator.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions, _ := NewActions(provider, tenancy.DefaultOrgScope(), now.Now)
	id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
	if _, err := actions.Reject(context.Background(), id, "alice", "public on purpose: the docs site", ""); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		now.Add(RetryAfter)
		if _, err := evaluator.Evaluate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if delivered, attempts := sink.counts(); delivered != 0 || len(attempts) != 1 {
		t.Fatalf("delivered %d over %v; want the one failed attempt only", delivered, attempts)
	}
	if issue, _, _ := actions.Get(context.Background(), id); issue.Alerted == nil || len(issue.Alerted.Pending) != 0 {
		t.Fatalf("pending after the reject = %+v", issue.Alerted)
	}
}

// Events are read in the order they were written, not by their times: a
// regress written after the alert, by a replica whose clock is behind,
// still needs its alert.
func TestDecideOrdersEventsBySequence(t *testing.T) {
	issue := Issue{ID: "x", State: StateRegressed, Severity: SeverityCritical, Version: 3, Alerted: &AlertMark{Version: 2}}
	byTime := []Event{
		{Seq: 3000, At: start.Add(-5 * time.Minute), Kind: EventRegressed},
		{Seq: 1000, At: start, Kind: EventOpened},
		{Seq: 2000, At: start, Kind: EventAlerted, Detail: alertedOpened},
	}
	p := decide(issue, byTime)
	if p == nil || !p.alert || p.transition.Kind != EventRegressed {
		t.Fatalf("plan = %+v; want an alert for the regress", p)
	}
}

// An issue that was alerted, then resolved only because its account was
// disconnected, comes back through the baseline when the account is
// connected again and sends nothing; a rejection still holds.
func TestReconnectDoesNotAlertAgain(t *testing.T) {
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site", "kept-public")
			now := &clock{now: start}
			connected := []string{testAccount}
			sink := &recordingSink{}
			alerter, _ := NewAlerter(provider, scope, sink, WithAlertClock(now.Now))
			evaluator, _ := NewEvaluator(provider, scope, source, WithRules(severityRules()), WithClock(now.Now),
				WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), nil }),
				WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
			evaluate := func() {
				t.Helper()
				now.Add(time.Minute)
				if _, err := evaluator.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			evaluate()
			actions, _ := NewActions(provider, scope, now.Now)
			kept := IssueID(testRuleID, "arn:aws:s3:::kept-public")
			if _, err := actions.Reject(context.Background(), kept, "alice", "public on purpose: the docs site", ""); err != nil {
				t.Fatal(err)
			}
			connected = []string{}
			evaluate()
			connected = []string{testAccount}
			for range 3 {
				evaluate()
			}
			docs, _, _ := actions.Get(context.Background(), IssueID(testRuleID, "arn:aws:s3:::docs-site"))
			if docs.State != StateOpen {
				t.Fatalf("docs-site after the reconnect = %s; want open again", docs.State)
			}
			if again, _, _ := actions.Get(context.Background(), kept); again.State != StateRejected {
				t.Fatalf("the rejected issue after the reconnect = %s", again.State)
			}
			if kinds := sink.kinds(); len(kinds) != 2 {
				t.Fatalf("alerts = %v; want only the two first openings", kinds)
			}
		})
	}
}

// Healthy evaluations log nothing at the default level.
func TestHealthyAlertCyclesLogNothing(t *testing.T) {
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	lines := &logLines{}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now, WithAlertLogger(lines.logf))
	replica.evaluate(t)
	now.Add(time.Minute)
	replica.evaluate(t)
	if logged := lines.all(); len(logged) != 0 {
		t.Fatalf("healthy cycles logged %q", logged)
	}
}

// After the first alert the pump reads no events while nothing happens to
// an issue: its mark says it is up to date.
func TestUpToDateIssuesNeedNoEventRead(t *testing.T) {
	provider := &countingEvents{Provider: storage.NewMemory()}
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now)
	replica.evaluate(t)
	provider.reads = 0
	for range 3 {
		now.Add(time.Minute)
		replica.evaluate(t)
	}
	if provider.reads != 0 {
		t.Fatalf("the pump read events %d times over quiet evaluations", provider.reads)
	}
}

// countingEvents counts event reads of a memory store.
type countingEvents struct {
	storage.Provider
	reads int
}

func (store *countingEvents) issues() storage.AWSIssueStore {
	inner, _ := storage.AWSIssuesOf(store.Provider)
	return inner
}

func (store *countingEvents) AWSIssues(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueRecord, error) {
	return store.issues().AWSIssues(ctx, scope, query)
}

func (store *countingEvents) AWSIssueCounts(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueCount, error) {
	return store.issues().AWSIssueCounts(ctx, scope, query)
}

func (store *countingEvents) AWSIssueEvents(ctx context.Context, scope tenancy.OrgScope, id string) ([]storage.AWSIssueEvent, error) {
	store.reads++
	return store.issues().AWSIssueEvents(ctx, scope, id)
}

func (store *countingEvents) AWSIssueBaselines(ctx context.Context, scope tenancy.OrgScope, account string) ([]storage.AWSIssueBaseline, error) {
	return store.issues().AWSIssueBaselines(ctx, scope, account)
}

func (store *countingEvents) WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch) error {
	return store.issues().WriteAWSIssues(ctx, scope, batch)
}

func (store *countingEvents) PruneAWSIssues(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	return store.issues().PruneAWSIssues(ctx, scope, before)
}

// The coverage notice names what is not read from the view's gaps: the
// source, why, when it last read and, for a denial, the actions to grant.
func TestUnreadNamesTheGapAndItsFix(t *testing.T) {
	last := start.Add(-time.Hour)
	view := NewView(ViewData{Account: testAccount,
		Reads: map[ReadKey]Read{{Service: "ec2", Region: "eu-west-1"}: ReadNotRead, {Service: "s3", Region: "eu-west-1"}: ReadOK},
		Gaps:  []model.Gap{{Account: testAccount, Region: "eu-west-1", Service: "ec2", Reason: model.GapDenied, LastGoodAt: last}}})
	rule := &testRule{id: "detection-off", kind: KindSecurity, inputs: []Input{{Service: "ec2"}, {Service: "s3"}}}
	unread := UnreadOf(view, rule, "eu-west-1")
	if len(unread) != 1 || unread[0].Source != "ec2" || unread[0].Reason != model.GapDenied || !unread[0].LastGoodAt.Equal(last) || len(unread[0].Fix) == 0 {
		t.Fatalf("unread = %+v", unread)
	}
}

// Two replicas evaluate after a third stopped between its write and its
// alert: exactly one issue, and exactly one alert between them.
func TestReplicasAndARestartSendOneAlert(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			scope := tenancy.NewOrgScope(uniqueOrg("alerts"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}

			// The first process wrote the issue and stopped before its
			// alert pump ran.
			dying, _ := NewEvaluator(set.a, scope, source, WithRules(severityRules()), WithClock(now.Now))
			if _, err := dying.Evaluate(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted := newAlerting(t, set.restart(t), scope, source, severityRules(), now)
			other := newAlerting(t, set.b, scope, source, severityRules(), now)
			var wg sync.WaitGroup
			for _, replica := range []*alerting{restarted, other} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 3 {
						if _, err := replica.evaluator.Evaluate(context.Background()); err != nil {
							t.Error(err)
						}
					}
				}()
			}
			wg.Wait()
			// Whichever replica held the lock last, one more evaluation
			// each finds nothing left to send.
			restarted.evaluate(t)
			other.evaluate(t)
			store, _ := storage.AWSIssuesOf(set.a)
			records, err := store.AWSIssues(context.Background(), scope, storage.AWSIssueQuery{})
			if err != nil || len(records) != 1 {
				t.Fatalf("issues = %d, %v; want one", len(records), err)
			}
			total := len(restarted.sink.kinds()) + len(other.sink.kinds())
			if total != 1 {
				t.Fatalf("alerts sent = %d (restarted %v, other %v); want one", total, restarted.sink.kinds(), other.sink.kinds())
			}
			alerted := 0
			for _, kind := range eventKinds(t, store, scope, records[0].ID) {
				if kind == storage.AWSIssueEventAlerted {
					alerted++
				}
			}
			if alerted != 1 {
				t.Fatalf("alerted events = %d; want one", alerted)
			}
		})
	}
}

// blockingSink counts sends; with hold set it holds its first send until
// released.
type blockingSink struct {
	hold    bool
	entered chan struct{}
	release chan struct{}
	sends   atomic.Int64
}

func (sink *blockingSink) Send(context.Context, Alert) error {
	if sink.sends.Add(1) == 1 && sink.hold {
		close(sink.entered)
		<-sink.release
	}
	return nil
}

func (sink *blockingSink) SendDigest(context.Context, []Alert) error {
	sink.sends.Add(1)
	return nil
}

// Two pumps over one store that both see the same pending alert send it
// once: the first claims it before it sends, and the second, running
// while the first is still sending, finds nothing due.
func TestTwoPumpsSendAPendingAlertOnce(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	now := &clock{now: start}
	failing := newFlakySink(1)
	first := flakyReplica(t, provider, source, now, failing)
	if _, err := first.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now.Add(RetryAfter)
	held := &blockingSink{hold: true, entered: make(chan struct{}), release: make(chan struct{})}
	other := &blockingSink{}
	pumpA, _ := NewAlerter(provider, scope, held, WithAlertClock(now.Now))
	pumpB, _ := NewAlerter(provider, scope, other, WithAlertClock(now.Now))
	done := make(chan error, 1)
	go func() { done <- pumpA.Pump(context.Background(), Report{}) }()
	receive(t, held.entered, "the first pump's send")
	if err := pumpB.Pump(context.Background(), Report{}); err != nil {
		t.Fatal(err)
	}
	close(held.release)
	if err := receive(t, done, "the first pump's end"); err != nil {
		t.Fatal(err)
	}
	if sent := held.sends.Load() + other.sends.Load(); sent != 1 {
		t.Fatalf("the pending alert was sent %d times; want once", sent)
	}
}
