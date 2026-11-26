package issues

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// flowAccount is the account the scripted flows evaluate.
const flowAccount = "123400000041"

// scriptedRule reports exactly the findings a test sets, for the inputs it
// names, so a test drives severity, material and Region directly.
type scriptedRule struct {
	id       string
	inputs   []Input
	mu       sync.Mutex
	findings []Finding
}

func (rule *scriptedRule) ID() string      { return rule.id }
func (rule *scriptedRule) Kind() Kind      { return KindSecurity }
func (rule *scriptedRule) Inputs() []Input { return rule.inputs }

func (rule *scriptedRule) Evaluate(View) []Finding {
	rule.mu.Lock()
	defer rule.mu.Unlock()
	return slices.Clone(rule.findings)
}

func (rule *scriptedRule) set(findings ...Finding) {
	rule.mu.Lock()
	defer rule.mu.Unlock()
	rule.findings = findings
}

// scriptedSource serves views that carry only the reads a test sets.
type scriptedSource struct {
	mu       sync.Mutex
	accounts []string
	reads    map[string]map[ReadKey]Read
}

func newScriptedSource(accounts ...string) *scriptedSource {
	source := &scriptedSource{accounts: accounts, reads: map[string]map[ReadKey]Read{}}
	for _, account := range accounts {
		source.reads[account] = map[ReadKey]Read{}
	}
	return source
}

func (source *scriptedSource) Accounts(context.Context) ([]string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return slices.Clone(source.accounts), nil
}

func (source *scriptedSource) View(_ context.Context, account string) (View, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return NewView(ViewData{Account: account, Reads: source.reads[account]}), nil
}

func (source *scriptedSource) read(account, service, region string, read Read) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.reads[account][ReadKey{Service: service, Region: region}] = read
}

// publicFinding is a public bucket. principals is its material; seen is a
// volatile fact carried in the title.
func publicFinding(owner, region string, severity Severity, principals, seen string) Finding {
	return Finding{
		Owner: owner, Region: region, Severity: severity, Title: "Bucket is public (seen " + seen + ")",
		Evidence: []Evidence{{Text: "bucket policy allows " + principals, Status: EvidenceProven, Source: "s3"}},
		Material: Material{"principals": principals},
	}
}

// flow is one evaluator and its user actions over one backend, on a test
// clock.
type flow struct {
	t         *testing.T
	ctx       context.Context
	scope     tenancy.OrgScope
	clock     *clock
	source    *scriptedSource
	evaluator *Evaluator
	actions   *Actions
}

func newFlow(t *testing.T, provider storage.Provider, scope tenancy.OrgScope, source *scriptedSource, options []Option, rules ...Rule) *flow {
	t.Helper()
	registry := NewRegistry()
	for _, rule := range rules {
		if err := registry.Register(rule); err != nil {
			t.Fatal(err)
		}
	}
	now := &clock{now: start}
	options = append([]Option{WithRules(registry), WithClock(now.Now)}, options...)
	evaluator, err := NewEvaluator(provider, scope, source, options...)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := NewActions(provider, scope, now.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &flow{t: t, ctx: context.Background(), scope: scope, clock: now, source: source, evaluator: evaluator, actions: actions}
}

func (f *flow) evaluate() Report {
	f.t.Helper()
	report, err := f.evaluator.Evaluate(f.ctx)
	if err != nil {
		f.t.Fatalf("evaluate at %s: %v", f.clock.Now(), err)
	}
	return report
}

func (f *flow) get(id string) (Issue, []Event) {
	f.t.Helper()
	issue, events, err := f.actions.Get(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return issue, events
}

func (f *flow) list(query storage.AWSIssueQuery) []Issue {
	f.t.Helper()
	list, err := f.actions.List(f.ctx, query)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

// alertWorthy counts the events an alert is raised on: an open or a
// regress.
func alertWorthy(events []Event) int {
	n := 0
	for _, event := range events {
		if event.Kind == EventOpened || event.Kind == EventRegressed {
			n++
		}
	}
	return n
}

// flowBackends are memory, file, and Postgres when TEST_POSTGRES_DSN names
// an isolated database, in that order.
func flowBackends(t *testing.T) []namedProvider {
	t.Helper()
	single := singleReplicaBackends(t)
	out := []namedProvider{{"memory", single["memory"]}, {"file", single["file"]}}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("TEST_POSTGRES_DSN not set: the Postgres backend is skipped")
		return out
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return append(out, namedProvider{"postgres", provider})
}

type namedProvider struct {
	name     string
	provider storage.Provider
}

// day is the date of t, YYYY-MM-DD in UTC.
func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// newScope is an organization of its own, so runs on a shared Postgres
// database never see each other's rows.
func newScope(prefix string) tenancy.OrgScope { return tenancy.NewOrgScope(uniqueOrg(prefix)) }

// ordered ranges over cases in key order, so a failure names the same case
// on every run.
func ordered[T any](cases map[string]T) func(yield func(string, T) bool) {
	return func(yield func(string, T) bool) {
		keys := make([]string, 0, len(cases))
		for key := range cases {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !yield(key, cases[key]) {
				return
			}
		}
	}
}
