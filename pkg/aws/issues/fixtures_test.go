package issues

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// testRule finds every s3 resource whose state is "public". Its material
// is the resource's "principals" tag; its title carries the "seen" tag, a
// volatile fact.
type testRule struct {
	id     string
	kind   Kind
	inputs []Input
	// also are findings the rule reports besides the public buckets.
	also []Finding
}

func (rule *testRule) ID() string      { return rule.id }
func (rule *testRule) Kind() Kind      { return rule.kind }
func (rule *testRule) Inputs() []Input { return rule.inputs }

func (rule *testRule) Evaluate(view View) []Finding {
	var out []Finding
	for _, resource := range view.Resources() {
		if resource.Service != "s3" || resource.State != "public" {
			continue
		}
		out = append(out, Finding{
			Owner: resource.ARN, Region: resource.Region, Severity: SeverityHigh,
			Title:    "Bucket " + resource.Name + " is public (seen " + resource.Tags["seen"] + ")",
			Evidence: []Evidence{{Text: "bucket policy allows *", Status: EvidenceProven, Source: "s3", AsOf: view.AsOf()}},
			Material: Material{"principals": resource.Tags["principals"]},
		})
	}
	return append(out, rule.also...)
}

func publicBucketRules() *Registry {
	rules := NewRegistry()
	if err := rules.Register(&testRule{id: testRuleID, kind: KindSecurity, inputs: []Input{{Service: "s3"}}}); err != nil {
		panic(err)
	}
	return rules
}

// fakeSource serves views built in code, the way SnapshotSource builds
// them: reads per service and Region, and an as-of time that is when the
// buckets were last read. Tests change it between evaluations.
type fakeSource struct {
	mu       sync.Mutex
	accounts []string
	buckets  map[string][]model.Resource
	reads    map[string]map[ReadKey]Read
	readAt   map[string]time.Time
	clock    func() time.Time
	err      error
}

func newFakeSource(accounts ...string) *fakeSource {
	source := &fakeSource{accounts: accounts, buckets: map[string][]model.Resource{}, reads: map[string]map[ReadKey]Read{},
		readAt: map[string]time.Time{}, clock: func() time.Time { return start }}
	for _, account := range accounts {
		source.reads[account] = map[ReadKey]Read{{Service: "s3", Region: "eu-west-1"}: ReadOK}
	}
	return source
}

func (source *fakeSource) Accounts(context.Context) ([]string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return append([]string(nil), source.accounts...), source.err
}

func (source *fakeSource) View(_ context.Context, account string) (View, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return NewView(ViewData{Account: account, AsOf: source.readAt[account], Resources: source.buckets[account], Reads: source.reads[account]}), nil
}

// public sets the account's public buckets in eu-west-1; seen is a
// volatile fact.
func (source *fakeSource) public(account, seen string, names ...string) {
	source.publicIn(account, "eu-west-1", seen, names...)
}

// publicIn sets the account's public buckets, all in one Region, as read
// now.
func (source *fakeSource) publicIn(account, region, seen string, names ...string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.buckets[account] = nil
	source.readAt[account] = source.clock()
	for _, name := range names {
		source.buckets[account] = append(source.buckets[account], model.Resource{
			ARN: "arn:aws:s3:::" + name, Service: "s3", Type: "bucket", Account: account, Region: region, Name: name, State: "public",
			Tags: map[string]string{"principals": "*", "seen": seen},
		})
	}
}

// add adds one public bucket in region to the account's buckets.
func (source *fakeSource) add(account, region, name string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.buckets[account] = append(source.buckets[account], model.Resource{
		ARN: "arn:aws:s3:::" + name, Service: "s3", Type: "bucket", Account: account, Region: region, Name: name, State: "public",
		Tags: map[string]string{"principals": "*"},
	})
}

// read sets the latest read of service in region.
func (source *fakeSource) read(account, service, region string, read Read) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.reads[account][ReadKey{Service: service, Region: region}] = read
}

// hangGuard bounds how long a test waits for an event that should come at
// once, so a broken loop fails instead of hanging the run.
const hangGuard = time.Minute

// receive returns the next value from ch, or fails the test when none
// comes within hangGuard.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(hangGuard):
		t.Fatalf("no %s within %s", what, hangGuard)
		var zero T
		return zero
	}
}

// clock is a settable test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logLines captures the evaluator's log.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// connectedOnly lists accounts as connected with their Regions not known.
func connectedOnly(accounts []string) []ConnectedAccount {
	if accounts == nil {
		return nil
	}
	out := make([]ConnectedAccount, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, ConnectedAccount{Account: account})
	}
	return out
}
