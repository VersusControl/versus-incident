package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// tablesBackend is one provider the AWS and FinOps table contract runs on.
type tablesBackend struct {
	name     string
	provider Provider
	raw      *sql.DB
	postgres bool
}

func (b tablesBackend) inventory(t *testing.T) AWSInventoryStore {
	t.Helper()
	store, ok := AWSInventory(b.provider)
	if !ok {
		t.Fatalf("%s provider has no AWS inventory store", b.name)
	}
	return store
}

func (b tablesBackend) finops(t *testing.T) FinOpsStore {
	t.Helper()
	store, ok := FinOps(b.provider)
	if !ok {
		t.Fatalf("%s provider has no FinOps store", b.name)
	}
	return store
}

// tablesBackends opens memory and file providers, and a Postgres provider
// when TEST_POSTGRES_DSN names an isolated database.
func tablesBackends(t *testing.T) []tablesBackend {
	t.Helper()
	memory := NewMemory()
	file, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	raw, err := file.(*fileProvider).tablesDB()
	if err != nil {
		t.Fatal(err)
	}
	backends := []tablesBackend{{name: "memory", provider: memory}, {name: "file", provider: file, raw: raw}}
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		provider, err := NewPostgres(PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		backends = append(backends, tablesBackend{name: "postgres", provider: provider, raw: provider.(SQLAccessor).DB(), postgres: true})
	} else {
		t.Log("TEST_POSTGRES_DSN not set: the Postgres backend is skipped")
	}
	return backends
}

// tablesOrgs are organization IDs unique to one test run, so Postgres runs
// share a database without seeing each other's rows.
type tablesOrgs struct{ ent, def string }

func newTablesOrgs() tablesOrgs {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	return tablesOrgs{ent: "ent-" + suffix, def: "def-" + suffix}
}

// canonicalJSON renders a value with sorted keys and no spacing, so a
// document Postgres reformatted compares equal to the one it was given.
func canonicalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type tablesRecorder func(label string, value any)

// runTablesParity runs a scenario on every backend and requires every
// backend to record exactly what memory recorded.
func runTablesParity(t *testing.T, scenario func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder)) {
	t.Helper()
	backends := tablesBackends(t)
	orgs := newTablesOrgs()
	transcripts := make([][]string, len(backends))
	for index, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			defer failOnPanic(t)
			scenario(t, backend, orgs, func(label string, value any) {
				transcripts[index] = append(transcripts[index], label+" = "+canonicalJSON(t, value))
			})
		})
	}
	for index := 1; index < len(backends); index++ {
		want, got := transcripts[0], transcripts[index]
		for line := 0; line < max(len(want), len(got)); line++ {
			var left, right string
			if line < len(want) {
				left = want[line]
			}
			if line < len(got) {
				right = got[line]
			}
			if left != right {
				t.Fatalf("%s differs from %s at step %d:\n%s: %s\n%s: %s", backends[index].name, backends[0].name, line, backends[0].name, left, backends[index].name, right)
			}
		}
	}
}

// ordered ranges over cases in label order, so every backend records them
// in the same sequence.
func ordered[T any](cases map[string]T) iter.Seq2[string, T] {
	return func(yield func(string, T) bool) {
		for _, label := range sortedKeysOf(cases) {
			if !yield(label, cases[label]) {
				return
			}
		}
	}
}

// tablesFailure carries a failed call out of must to failOnPanic.
type tablesFailure struct{ err error }

// must returns value, or stops the scenario when err is set; failOnPanic
// turns the stop into a test failure.
func must[T any](value T, err error) T {
	if err != nil {
		panic(tablesFailure{err})
	}
	return value
}

func failOnPanic(t *testing.T) {
	if recovered := recover(); recovered != nil {
		if failure, ok := recovered.(tablesFailure); ok {
			t.Fatalf("unexpected error: %v", failure.err)
		}
		panic(recovered)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var tablesBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	tablesAccount = "111122223333"
	tablesRegion  = "us-east-1"
)

func tablesARN(id string) string { return "arn:aws:ec2:us-east-1:111122223333:instance/" + id }

func arnsOf(rows []AWSResource) []string {
	out := make([]string, len(rows))
	for index, row := range rows {
		out[index] = row.ARN + "@" + row.State
	}
	return out
}

func TestTablesParityInventoryCommitsAndHistory(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		t0, t1, t2 := tablesBase, tablesBase.Add(time.Hour), tablesBase.Add(2*time.Hour)
		r1 := AWSResource{ARN: tablesARN("i-1"), Type: "instance", App: "shop", Name: "Web-Server", State: "running",
			Tags: map[string]string{"app": "shop", "owner": "[redacted]"}, Relations: []AWSRelation{{"in_vpc", "vpc-1"}},
			Attributes: map[string]string{"id": "i-1"}, Detail: json.RawMessage(`{"z":1,"a":[1,2],"resource_region":"us-east-1"}`)}
		r2 := AWSResource{ARN: tablesARN("i-2"), Type: "instance", App: "jobs", Name: "worker", State: "running"}
		h1 := AWSHealth{ARN: r1.ARN, Kind: "alarm", Ref: "arn:aws:cloudwatch:us-east-1:111122223333:alarm:cpu", State: "critical", Summary: "CPU high",
			Since: t0.Add(-time.Minute + 123456789*time.Nanosecond), Children: []string{"b", "a"}}
		f1 := AWSSecurityFlag{ARN: r1.ARN, Flag: "public_exposure", Summary: "port 22 open", Detail: json.RawMessage(`{"port":22}`)}
		first := must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: t0, Digest: "digest-a", Gaps: json.RawMessage(`[{"operation":"DescribeVolumes","reason":"denied"}]`),
			Resources: []AWSResource{r1, r2, r1}, Health: []AWSHealth{h1}, Flags: []AWSSecurityFlag{f1}}, nil))
		if first.Unchanged || first.Generation.Seq != 1 || first.Generation.Count != 2 {
			t.Fatalf("first commit = %+v", first)
		}
		record("first commit", first)

		r2b := r2
		r2b.State = "stopped"
		r3 := AWSResource{ARN: tablesARN("i-3"), Type: "instance", App: "shop", Name: "web-cache", State: "running"}
		second := must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: t1, Digest: "digest-b", Resources: []AWSResource{r3, r1, r2b}, Flags: []AWSSecurityFlag{f1}}, nil))
		if second.Unchanged || second.Generation.Seq != 2 || second.Generation.Count != 3 {
			t.Fatalf("second commit = %+v", second)
		}
		record("second commit", second)
		same := must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: t2, Digest: "digest-b", Resources: []AWSResource{r3, r1, r2b}, Flags: []AWSSecurityFlag{f1}}, nil))
		if !same.Unchanged || same.Generation.Seq != 2 || !same.Generation.VerifiedAt.Equal(t2) || !same.Generation.CommittedAt.Equal(t1) {
			t.Fatalf("identical commit = %+v", same)
		}
		record("identical commit", same)

		current := must(store.AWSResources(ctx, scope, AWSResourceQuery{}))
		if got := strings.Join(arnsOf(current), ","); got != tablesARN("i-1")+"@running,"+tablesARN("i-2")+"@stopped,"+tablesARN("i-3")+"@running" {
			t.Fatalf("current resources = %s", got)
		}
		record("current resources", current)
		before := must(store.AWSResources(ctx, scope, AWSResourceQuery{AsOf: t0.Add(30 * time.Minute)}))
		if got := strings.Join(arnsOf(before), ","); got != tablesARN("i-1")+"@running,"+tablesARN("i-2")+"@running" {
			t.Fatalf("resources as of the first generation = %s", got)
		}
		record("as of first", before)
		record("as of before any", must(store.AWSResources(ctx, scope, AWSResourceQuery{AsOf: t0.Add(-time.Second)})))
		record("as of exactly the second commit", must(store.AWSResources(ctx, scope, AWSResourceQuery{AsOf: t1})))
		record("current health", must(store.AWSHealth(ctx, scope, AWSHealthQuery{})))
		oldHealth := must(store.AWSHealth(ctx, scope, AWSHealthQuery{AsOf: t0.Add(time.Minute), ARN: h1.Ref, State: "critical"}))
		if len(oldHealth) != 1 || !oldHealth[0].Since.Equal(tableTime(h1.Since)) || strings.Join(oldHealth[0].Children, ",") != "b,a" {
			t.Fatalf("health as of the first generation = %+v", oldHealth)
		}
		record("health as of first by ref", oldHealth)
		record("current flags", must(store.AWSSecurityFlags(ctx, scope, AWSFlagQuery{Flag: "public_exposure"})))
		record("flags of other kind", must(store.AWSSecurityFlags(ctx, scope, AWSFlagQuery{Flag: "no_tags"})))

		units := must(store.AWSUnits(ctx, scope, AWSUnitQuery{}))
		if len(units) != 1 || units[0].Generation == nil || units[0].Generation.Seq != 2 || units[0].Sync.State != AWSSyncOK || units[0].Sync.Generation != 2 || !units[0].Sync.VerifiedAt.Equal(t2) {
			t.Fatalf("current units = %+v", units)
		}
		record("current units", units)
		history := must(store.AWSUnits(ctx, scope, AWSUnitQuery{AsOf: t0.Add(time.Minute)}))
		if history[0].Generation == nil || history[0].Generation.Seq != 1 || len(history[0].Generation.Gaps) == 0 {
			t.Fatalf("units as of the first generation = %+v", history)
		}
		record("units as of first", history)
		record("units as of before any", must(store.AWSUnits(ctx, scope, AWSUnitQuery{AsOf: t0.Add(-time.Second)})))

		for label, query := range ordered(map[string]AWSResourceQuery{
			"by type": {Type: "instance"}, "by app": {App: "shop"}, "by arn": {ARN: tablesARN("i-2")}, "by name prefix": {NamePrefix: "WEB-"},
			"by unit": {Account: tablesAccount, Region: tablesRegion, Service: "ec2"}, "other unit": {Service: "rds"}, "limit": {Limit: 2},
			"name prefix as of": {NamePrefix: "web", AsOf: t0.Add(time.Minute)},
		}) {
			record("resources "+label, must(store.AWSResources(ctx, scope, query)))
		}
		prefixed := must(store.AWSResources(ctx, scope, AWSResourceQuery{NamePrefix: "web"}))
		if len(prefixed) != 2 {
			t.Fatalf("name prefix search = %+v", prefixed)
		}

		// The closed rows are the first r2, the health row and the first
		// generation. r1 never changed, so it was never closed and keeps its
		// first valid_from after pruning.
		removed := must(store.PruneAWSHistory(ctx, scope, t1.Add(time.Minute)))
		if removed != 3 {
			t.Fatalf("pruned %d rows, want 3", removed)
		}
		record("pruned", removed)
		kept := must(store.AWSResources(ctx, scope, AWSResourceQuery{AsOf: t0.Add(30 * time.Minute)}))
		if got := strings.Join(arnsOf(kept), ","); got != tablesARN("i-1")+"@running" {
			t.Fatalf("resources as of the first generation after pruning = %s", got)
		}
		record("as of first after prune", kept)
		record("units as of first after prune", must(store.AWSUnits(ctx, scope, AWSUnitQuery{AsOf: t0.Add(time.Minute)})))

		for label, candidate := range ordered(map[string]AWSCandidate{
			"no digest":      {At: t2, Resources: []AWSResource{r1}},
			"no time":        {Digest: "x"},
			"resource arn":   {At: t2, Digest: "x", Resources: []AWSResource{{Type: "instance"}}},
			"health kind":    {At: t2, Digest: "x", Health: []AWSHealth{{ARN: r1.ARN}}},
			"invalid detail": {At: t2, Digest: "x", Resources: []AWSResource{{ARN: "a", Detail: json.RawMessage(`{`)}}},
			"nul text":       {At: t2, Digest: "x", Resources: []AWSResource{{ARN: "a", Name: "a\x00b"}}},
		}) {
			_, err := store.CommitAWSUnit(ctx, scope, unit, candidate, nil)
			if !errors.Is(err, ErrInvalidAWSInventory) {
				t.Fatalf("%s: commit error = %v", label, err)
			}
		}
		if _, err := store.CommitAWSUnit(ctx, scope, AWSUnit{tablesAccount, tablesRegion, ""}, AWSCandidate{At: t2, Digest: "x"}, nil); !errors.Is(err, ErrInvalidAWSInventory) {
			t.Fatalf("invalid unit commit error = %v", err)
		}
		record("after rejected commits", must(store.AWSUnits(ctx, scope, AWSUnitQuery{})))
	})
}

func TestTablesParityInventorySyncStatePurgeAndFence(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		east := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		west := AWSUnit{tablesAccount, "eu-west-1", "ec2"}
		bucket := AWSUnit{tablesAccount, "global", "s3"}

		var foundCalls []bool
		pending := func(state *AWSSyncState, found bool) bool {
			foundCalls = append(foundCalls, found)
			if found {
				return false
			}
			state.State = "pending"
			return true
		}
		mustDo(t, store.UpdateAWSSync(ctx, scope, bucket, nil, pending))
		mustDo(t, store.UpdateAWSSync(ctx, scope, bucket, nil, pending))
		if fmt.Sprint(foundCalls) != "[false true]" {
			t.Fatalf("found = %v", foundCalls)
		}
		mustDo(t, store.UpdateAWSSync(ctx, scope, AWSUnit{tablesAccount, tablesRegion, "lambda"}, nil, func(*AWSSyncState, bool) bool { return false }))
		record("pending units", must(store.AWSUnits(ctx, scope, AWSUnitQuery{})))

		must(store.CommitAWSUnit(ctx, scope, east, AWSCandidate{At: tablesBase, Digest: "east-1", Resources: []AWSResource{{ARN: tablesARN("i-1"), Type: "instance"}}}, nil))
		mustDo(t, store.UpdateAWSSync(ctx, scope, east, nil, func(state *AWSSyncState, found bool) bool {
			state.State, state.ErrorCode, state.AttemptedAt = "failed", "throttled", tablesBase.Add(time.Hour)
			state.Gaps = json.RawMessage(`[{"reason":"throttled","last_good_at":"2026-03-01T12:00:00Z"}]`)
			state.Generation = 99
			return found
		}))
		failed := must(store.AWSUnits(ctx, scope, AWSUnitQuery{Service: "ec2"}))
		if len(failed) != 1 || failed[0].Sync.State != "failed" || failed[0].Sync.Generation != 1 || failed[0].Generation == nil || failed[0].Generation.Seq != 1 {
			t.Fatalf("a failed sync must keep the committed generation: %+v", failed)
		}
		record("failed unit keeps data", failed)
		record("failed unit resources", must(store.AWSResources(ctx, scope, AWSResourceQuery{Service: "ec2"})))

		must(store.CommitAWSUnit(ctx, scope, west, AWSCandidate{At: tablesBase, Digest: "west-1", Resources: []AWSResource{{ARN: "arn:aws:ec2:eu-west-1:111122223333:instance/i-9", Type: "instance"}}}, nil))
		at := tablesBase.Add(time.Minute)
		for _, region := range []string{tablesRegion, "eu-west-1"} {
			mustDo(t, store.AppendAWSChanges(ctx, scope, tablesAccount, region, []AWSChange{{EventID: "event-" + region, Account: tablesAccount, Region: region, EventTime: at}},
				[]AWSInterval{{From: tablesBase, To: tablesBase.Add(time.Hour)}}, nil))
		}
		record("pairs before purge", must(store.AWSPairs(ctx, scope)))

		epoch := must(store.AWSAccountEpoch(ctx, scope, tablesAccount))
		if epoch != 0 {
			t.Fatalf("first epoch = %d", epoch)
		}
		mustDo(t, store.PurgeAWSAccount(ctx, scope, tablesAccount, "eu-west-1"))
		pairs := must(store.AWSPairs(ctx, scope))
		record("pairs after region purge", pairs)
		for _, pair := range pairs {
			if pair.Region == "eu-west-1" {
				t.Fatalf("a purged region left a pair: %+v", pairs)
			}
		}
		record("west resources after purge", must(store.AWSResources(ctx, scope, AWSResourceQuery{Region: "eu-west-1", AsOf: tablesBase.Add(time.Second)})))
		record("changes after region purge", must(store.AWSChanges(ctx, scope, AWSChangeQuery{})))
		record("coverage after region purge", must(store.AWSChangeCoverage(ctx, scope, "", "")))
		record("east still there", must(store.AWSResources(ctx, scope, AWSResourceQuery{Region: tablesRegion})))

		stale := epoch
		_, err := store.CommitAWSUnit(ctx, scope, west, AWSCandidate{At: tablesBase.Add(2 * time.Hour), Digest: "west-2", Resources: []AWSResource{{ARN: "arn:aws:ec2:eu-west-1:111122223333:instance/i-9", Type: "instance"}}}, &stale)
		if !errors.Is(err, ErrAWSInventoryFenced) {
			t.Fatalf("a writer opened before the purge committed: %v", err)
		}
		if err := store.UpdateAWSSync(ctx, scope, west, &stale, func(state *AWSSyncState, _ bool) bool { state.State = "failed"; return true }); !errors.Is(err, ErrAWSInventoryFenced) {
			t.Fatalf("a writer opened before the purge recorded a failure: %v", err)
		}
		if err := store.AppendAWSChanges(ctx, scope, tablesAccount, "eu-west-1", []AWSChange{{EventID: "late", Account: tablesAccount, Region: "eu-west-1", EventTime: at}}, nil, &stale); !errors.Is(err, ErrAWSInventoryFenced) {
			t.Fatalf("a writer opened before the purge appended changes: %v", err)
		}
		record("pairs after fenced writes", must(store.AWSPairs(ctx, scope)))
		fresh := must(store.AWSAccountEpoch(ctx, scope, tablesAccount))
		if fresh != 1 {
			t.Fatalf("epoch after purge = %d", fresh)
		}
		mustDo(t, store.UpdateAWSSync(ctx, scope, west, &fresh, func(state *AWSSyncState, _ bool) bool { state.State = "pending"; return true }))

		mustDo(t, store.PurgeAWSAccount(ctx, scope, tablesAccount, ""))
		record("pairs after account purge", must(store.AWSPairs(ctx, scope)))
		record("units after account purge", must(store.AWSUnits(ctx, scope, AWSUnitQuery{})))
		record("history after account purge", must(store.AWSResources(ctx, scope, AWSResourceQuery{AsOf: tablesBase.Add(time.Second)})))
		record("changes after account purge", must(store.AWSChanges(ctx, scope, AWSChangeQuery{})))
		if removed := must(store.PruneAWSHistory(ctx, scope, tablesBase.Add(24*time.Hour))); removed != 0 {
			t.Fatalf("a purge left %d history rows", removed)
		}
		record("epoch after two purges", must(store.AWSAccountEpoch(ctx, scope, tablesAccount)))
		if backend.raw != nil {
			for _, table := range awsAccountTables {
				var count int
				mustDo(t, backend.raw.QueryRow(backend.provider.(interface{ q(string) string }).q(`SELECT COUNT(*) FROM `+table+` WHERE org_id = ?`), orgs.ent).Scan(&count))
				if count != 0 {
					t.Fatalf("%s holds %d rows after the account purge", table, count)
				}
			}
		}
	})
}

func TestTablesParityChangesAndCoverage(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		change := func(id string, offset time.Duration, service, resource, app string) AWSChange {
			return AWSChange{EventID: id, Account: tablesAccount, Region: tablesRegion, Service: service, EventTime: tablesBase.Add(offset), EventName: "Update",
				ActorClass: "pipeline", Actor: "deployer", ResourceARN: resource, App: app, Summary: id,
				Fields: map[string]string{"size": "large"}, Detail: json.RawMessage(`{"resources":["` + resource + `"],"event_source":"ec2.amazonaws.com"}`)}
		}
		c1 := change("e-1", time.Minute, "ec2", tablesARN("i-1"), "shop")
		c2 := change("e-2", 2*time.Minute, "ec2", tablesARN("i-2"), "shop")
		c3 := change("z-3", 3*time.Minute, "s3", "arn:aws:s3:::bucket", "jobs")
		c4 := change("a-4", 3*time.Minute, "s3", "arn:aws:s3:::bucket", "jobs")
		mustDo(t, store.AppendAWSChanges(ctx, scope, tablesAccount, tablesRegion, []AWSChange{c1, c2}, []AWSInterval{{tablesBase, tablesBase.Add(90 * time.Second)}}, nil))
		replaced := c2
		replaced.Summary = "replaced"
		mustDo(t, store.AppendAWSChanges(ctx, scope, tablesAccount, tablesRegion, []AWSChange{c3, c4, replaced},
			[]AWSInterval{{tablesBase.Add(90 * time.Second), tablesBase.Add(10 * time.Minute)}, {tablesBase.Add(20 * time.Minute), tablesBase.Add(30 * time.Minute)}}, nil))
		coverage := must(store.AWSChangeCoverage(ctx, scope, tablesAccount, tablesRegion))
		if len(coverage) != 1 || len(coverage[0].Intervals) != 2 || !coverage[0].Intervals[0].To.Equal(tablesBase.Add(10*time.Minute)) {
			t.Fatalf("coverage = %+v", coverage)
		}
		record("coverage", coverage)
		all := must(store.AWSChanges(ctx, scope, AWSChangeQuery{}))
		var order []string
		for _, item := range all {
			order = append(order, item.EventID+":"+item.Summary)
		}
		if strings.Join(order, ",") != "e-1:e-1,e-2:replaced,a-4:a-4,z-3:z-3" {
			t.Fatalf("changes = %v", order)
		}
		record("all changes", all)
		for label, query := range ordered(map[string]AWSChangeQuery{
			"window":   {Since: tablesBase.Add(2 * time.Minute), Until: tablesBase.Add(3 * time.Minute)},
			"resource": {ResourceARN: tablesARN("i-1")}, "app": {App: "jobs"}, "service": {Service: "ec2"},
			"pair": {Account: tablesAccount, Region: tablesRegion, Since: tablesBase}, "other region": {Region: "eu-west-1"}, "limit": {Limit: 3},
		}) {
			record("changes "+label, must(store.AWSChanges(ctx, scope, query)))
		}
		for label, bad := range ordered(map[string]AWSChange{
			"no id": {Account: tablesAccount, Region: tablesRegion, EventTime: tablesBase}, "other account": {EventID: "x", Account: "999999999999", Region: tablesRegion, EventTime: tablesBase},
			"no time": {EventID: "x", Account: tablesAccount, Region: tablesRegion},
		}) {
			if err := store.AppendAWSChanges(ctx, scope, tablesAccount, tablesRegion, []AWSChange{bad}, nil, nil); !errors.Is(err, ErrInvalidAWSInventory) {
				t.Fatalf("%s: append error = %v", label, err)
			}
		}
		removed := must(store.PruneAWSChanges(ctx, scope, tablesBase.Add(150*time.Second)))
		if removed != 2 {
			t.Fatalf("pruned %d changes, want 2", removed)
		}
		trimmed := must(store.AWSChangeCoverage(ctx, scope, "", ""))
		if !trimmed[0].Intervals[0].From.Equal(tablesBase.Add(150 * time.Second)) {
			t.Fatalf("coverage after pruning = %+v", trimmed)
		}
		record("coverage after prune", trimmed)
		record("changes after prune", must(store.AWSChanges(ctx, scope, AWSChangeQuery{})))
		must(store.PruneAWSChanges(ctx, scope, tablesBase.Add(time.Hour)))
		record("coverage after full prune", must(store.AWSChangeCoverage(ctx, scope, "", "")))
		record("pairs after full prune", must(store.AWSPairs(ctx, scope)))
	})
}

func TestTablesParityReadOrganizationPrecedence(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		inventory, finops := backend.inventory(t), backend.finops(t)
		def := tenancy.NewOrgScope(orgs.def)
		ent := tenancy.NewOrgScope(orgs.ent, orgs.def)
		shared := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		only := AWSUnit{tablesAccount, tablesRegion, "rds"}
		commit := func(scope tenancy.OrgScope, unit AWSUnit, id string) {
			must(inventory.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase, Digest: id, Resources: []AWSResource{{ARN: tablesARN(id), Type: unit.Service}},
				Flags: []AWSSecurityFlag{{ARN: tablesARN(id), Flag: "no_tags"}}, Health: []AWSHealth{{ARN: tablesARN(id), Kind: "alarm", State: "ok"}}}, nil))
		}
		commit(def, shared, "default-shared")
		commit(ent, shared, "enterprise-shared")
		commit(def, only, "default-only")
		resources := must(inventory.AWSResources(ctx, ent, AWSResourceQuery{}))
		if got := strings.Join(arnsOf(resources), ","); got != tablesARN("enterprise-shared")+"@,"+tablesARN("default-only")+"@" {
			t.Fatalf("enterprise read = %s", got)
		}
		record("enterprise resources", resources)
		record("enterprise units", must(inventory.AWSUnits(ctx, ent, AWSUnitQuery{})))
		record("enterprise health", must(inventory.AWSHealth(ctx, ent, AWSHealthQuery{})))
		record("enterprise flags", must(inventory.AWSSecurityFlags(ctx, ent, AWSFlagQuery{})))
		record("default resources", must(inventory.AWSResources(ctx, def, AWSResourceQuery{})))
		// A unit the write organization only expects still answers for it.
		mustDo(t, inventory.UpdateAWSSync(ctx, ent, only, nil, func(state *AWSSyncState, _ bool) bool { state.State = "pending"; return true }))
		record("enterprise resources after pending", must(inventory.AWSResources(ctx, ent, AWSResourceQuery{})))
		record("enterprise units after pending", must(inventory.AWSUnits(ctx, ent, AWSUnitQuery{})))

		event := func(id, region string) AWSChange {
			return AWSChange{EventID: id, Account: tablesAccount, Region: region, EventTime: tablesBase.Add(time.Minute)}
		}
		window := []AWSInterval{{tablesBase, tablesBase.Add(time.Hour)}}
		mustDo(t, inventory.AppendAWSChanges(ctx, def, tablesAccount, tablesRegion, []AWSChange{event("default-east", tablesRegion)}, window, nil))
		mustDo(t, inventory.AppendAWSChanges(ctx, def, tablesAccount, "eu-west-1", []AWSChange{event("default-west", "eu-west-1")}, window, nil))
		mustDo(t, inventory.AppendAWSChanges(ctx, ent, tablesAccount, tablesRegion, []AWSChange{event("enterprise-east", tablesRegion)}, window, nil))
		changes := must(inventory.AWSChanges(ctx, ent, AWSChangeQuery{}))
		if len(changes) != 2 || changes[0].EventID != "default-west" || changes[1].EventID != "enterprise-east" {
			t.Fatalf("enterprise changes = %+v", changes)
		}
		record("enterprise changes", changes)
		record("enterprise coverage", must(inventory.AWSChangeCoverage(ctx, ent, "", "")))
		record("enterprise pairs", must(inventory.AWSPairs(ctx, ent)))

		line := func(day, account string, micros int64) FinOpsCostLine {
			return FinOpsCostLine{Day: day, Account: account, Region: tablesRegion, Service: "compute", RecordType: "Usage", NetAmortizedMicros: micros}
		}
		mustDo(t, finops.ReplaceFinOpsCostDays(ctx, def, "payer", []string{"2026-01-01", "2026-01-02"}, []FinOpsCostLine{line("2026-01-01", "a", 1), line("2026-01-01", "b", 2), line("2026-01-02", "a", 4)}))
		mustDo(t, finops.ReplaceFinOpsCostDays(ctx, ent, "payer", []string{"2026-01-01"}, []FinOpsCostLine{line("2026-01-01", "a", 8)}))
		totals := must(finops.FinOpsCostTotals(ctx, ent, FinOpsCostQuery{}, FinOpsByDay))
		if len(totals) != 2 || totals[0].NetAmortizedMicros != 8 || totals[1].NetAmortizedMicros != 4 {
			t.Fatalf("enterprise day totals = %+v", totals)
		}
		record("enterprise day totals", totals)
		record("enterprise lines", must(finops.FinOpsCostLines(ctx, ent, FinOpsCostQuery{})))
		record("default lines", must(finops.FinOpsCostTotals(ctx, def, FinOpsCostQuery{})))

		increase := func(id, status string) FinOpsIncrease {
			return FinOpsIncrease{ID: id, Account: "a", Service: "compute", StartedOn: "2026-01-01", Status: status, Events: []FinOpsIncreaseEvent{{At: tablesBase, Actor: "system", ToStatus: status}}}
		}
		mustDo(t, finops.PutFinOpsIncrease(ctx, def, increase("inc-1", "open")))
		mustDo(t, finops.PutFinOpsIncrease(ctx, def, increase("inc-2", "open")))
		mustDo(t, finops.PutFinOpsIncrease(ctx, ent, increase("inc-1", "expected")))
		increases := must(finops.FinOpsIncreases(ctx, ent, FinOpsIncreaseQuery{}))
		if len(increases) != 2 || increases[0].Status != "expected" || increases[0].Events[0].ToStatus != "expected" {
			t.Fatalf("enterprise increases = %+v", increases)
		}
		record("enterprise increases", increases)
		record("enterprise open increases", must(finops.FinOpsIncreases(ctx, ent, FinOpsIncreaseQuery{Statuses: []string{"open"}})))
		mustDo(t, finops.PutFinOpsReport(ctx, def, FinOpsReport{Month: "2026-01", TotalMicros: 1, Model: json.RawMessage(`{"org":"default"}`)}))
		mustDo(t, finops.PutFinOpsReport(ctx, def, FinOpsReport{Month: "2025-12", TotalMicros: 2, Model: json.RawMessage(`{"org":"default"}`)}))
		mustDo(t, finops.PutFinOpsReport(ctx, ent, FinOpsReport{Month: "2026-01", TotalMicros: 3, Model: json.RawMessage(`{"org":"enterprise"}`)}))
		record("enterprise reports", must(finops.FinOpsReports(ctx, ent)))
		report, found, err := finops.FinOpsReport(ctx, ent, "2026-01")
		if err != nil || !found || report.TotalMicros != 3 {
			t.Fatalf("enterprise report = %+v %v %v", report, found, err)
		}
		record("enterprise report", report)
		mustDo(t, finops.PutFinOpsSavingDecision(ctx, def, FinOpsSavingDecision{SavingID: "s-1", State: "dismissed"}))
		mustDo(t, finops.PutFinOpsSavingDecision(ctx, def, FinOpsSavingDecision{SavingID: "s-2", State: "dismissed"}))
		mustDo(t, finops.PutFinOpsSavingDecision(ctx, ent, FinOpsSavingDecision{SavingID: "s-1", State: "open"}))
		record("enterprise decisions", must(finops.FinOpsSavingDecisions(ctx, ent)))
	})
}

func finOpsTestLines() []FinOpsCostLine {
	refreshed := tablesBase.Add(987654321 * time.Nanosecond)
	return []FinOpsCostLine{
		{Day: "2026-01-30", Account: "acct-a", Region: tablesRegion, Service: "compute", ServiceName: "Amazon EC2", UsageType: "BoxUsage", UsageGroup: "instances", RecordType: "Usage",
			Team: "payments", NetAmortizedMicros: 1_000_000, QuantityMicros: 24_000_000, Unit: "Hrs", Currency: "USD", App: "shop", Env: "prod", RefreshedAt: refreshed},
		{Day: "2026-01-30", Account: "acct-a", Region: tablesRegion, Service: "compute", UsageType: "BoxUsage", RecordType: "Usage", ResourceID: "i-1",
			NetAmortizedMicros: 500_000, QuantityMicros: 12_000_000, Unit: "Hrs", EstimatedTeam: true, RefreshedAt: refreshed},
		{Day: "2026-01-31", Account: "acct-b", Region: "eu-west-1", Service: "storage", UsageType: "TimedStorage", RecordType: "Usage", Team: "search",
			NetAmortizedMicros: 250_000, QuantityMicros: 1_000, EstimatedRegion: true},
		{Day: "2026-02-01", Account: "acct-a", Service: "tax", RecordType: "Tax", NetAmortizedMicros: 70_000},
		{Day: "2026-02-01", Account: "acct-a", Region: tablesRegion, Service: "compute", UsageType: "BoxUsage", RecordType: "Usage", Team: "payments", ResourceID: "i-1",
			NetAmortizedMicros: -30_000},
	}
}

func TestTablesParityFinOpsLinesAndAggregation(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		days := []string{"2026-02-01", "2026-01-30", "2026-01-31", "2026-01-30"}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", days, finOpsTestLines()))
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-2", []string{"2026-01-30"}, []FinOpsCostLine{{Day: "2026-01-30", Account: "acct-c", Service: "compute", RecordType: "Usage", Team: "payments", NetAmortizedMicros: 9}}))
		for label, lines := range ordered(map[string][]FinOpsCostLine{
			"duplicate key": {finOpsTestLines()[0], finOpsTestLines()[0]},
			"unlisted day":  {{Day: "2026-03-01", Service: "compute"}},
		}) {
			if err := store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", days, lines); !errors.Is(err, ErrInvalidFinOps) {
				t.Fatalf("%s: replace error = %v", label, err)
			}
		}
		if err := store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", []string{"2026-02-30"}, nil); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("invalid day error = %v", err)
		}
		lines := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer-1"}))
		if len(lines) != 5 || !lines[0].RefreshedAt.Equal(tableTime(tablesBase.Add(987654321*time.Nanosecond))) {
			t.Fatalf("lines = %+v", lines)
		}
		record("payer lines", lines)
		none, instance := "", "i-1"
		for label, query := range ordered(map[string]FinOpsCostQuery{
			"untagged": {Team: &none}, "resource": {ResourceID: &instance}, "range": {From: "2026-01-31", Through: "2026-02-01"},
			"service": {Service: "compute", Through: "2026-01-30"}, "account": {Account: "acct-b"}, "record": {RecordType: "Tax"},
			"usage": {UsageType: "BoxUsage", Region: tablesRegion},
		}) {
			record("lines "+label, must(store.FinOpsCostLines(ctx, scope, query)))
		}
		grand := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}))
		if len(grand) != 1 || grand[0].NetAmortizedMicros != 1_790_009 || grand[0].Lines != 6 || !grand[0].EstimatedTeam || !grand[0].EstimatedRegion || grand[0].MinDay != "2026-01-30" {
			t.Fatalf("grand total = %+v", grand)
		}
		record("grand total", grand)
		byTeam := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, FinOpsByTeam))
		if len(byTeam) != 3 || byTeam[0].Team != "" || byTeam[1].Team != "payments" || byTeam[1].NetAmortizedMicros != 970_009 {
			t.Fatalf("totals by team = %+v", byTeam)
		}
		if byTeam[0].MinDay != "2026-01-30" || byTeam[1].MinDay != "2026-01-30" || byTeam[2].MinDay != "2026-01-31" {
			t.Fatalf("totals by team min days = %+v", byTeam)
		}
		record("by team", byTeam)
		byUnit := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer-1"}, FinOpsByUnit))
		if len(byUnit) != 2 || byUnit[0].Unit != "" || byUnit[0].Lines != 3 || byUnit[0].MinDay != "2026-01-31" ||
			byUnit[1].Unit != "Hrs" || byUnit[1].Lines != 2 || byUnit[1].MinDay != "2026-01-30" {
			t.Fatalf("totals by unit = %+v", byUnit)
		}
		record("by unit", byUnit)
		byGroup := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer-1"}, FinOpsByUsageGroup))
		if len(byGroup) != 2 || byGroup[0].UsageGroup != "" || byGroup[0].Lines != 4 || byGroup[0].MinDay != "2026-01-30" ||
			byGroup[1].UsageGroup != "instances" || byGroup[1].MinDay != "2026-01-30" {
			t.Fatalf("totals by usage group = %+v", byGroup)
		}
		record("by usage group", byGroup)
		for label, groupBy := range ordered(map[string][]FinOpsDimension{
			"service and day": {FinOpsByService, FinOpsByDay}, "month": {FinOpsByMonth}, "account": {FinOpsByAccount},
			"payer record": {FinOpsByPayer, FinOpsByRecordType}, "day team": {FinOpsByDay, FinOpsByTeam}, "resource": {FinOpsByResourceID},
			"region usage": {FinOpsByRegion, FinOpsByUsageType}, "usage group and unit": {FinOpsByUsageGroup, FinOpsByUnit},
		}) {
			// The list filters cover every payer-1 line, so each dimension
			// answers as it did unfiltered while proving it composes with
			// them.
			record("totals "+label, must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer-1", Accounts: []string{"acct-a", "acct-b"}, Teams: []string{"", "payments", "search"}}, groupBy...)))
		}
		record("totals of nothing", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2030-01-01"})))
		record("grand of nothing", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2030-01-01"}, FinOpsByTeam)))
		if _, err := store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, "owner"); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("unknown dimension error = %v", err)
		}
		if _, err := store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, FinOpsByDay, FinOpsByDay); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("repeated dimension error = %v", err)
		}

		// A refresh replaces whole days: a listed day without lines is
		// emptied, an unlisted day is untouched.
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", []string{"2026-01-30", "2026-01-31"}, []FinOpsCostLine{{Day: "2026-01-30", Account: "acct-a", Service: "compute", RecordType: "Usage", NetAmortizedMicros: 42}}))
		record("after day replace", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer-1"}, FinOpsByDay)))
		record("other payer untouched", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer-2"})))
		pruned := must(store.PruneFinOpsCostLines(ctx, scope, "2026-01-31"))
		if pruned != 2 {
			t.Fatalf("pruned %d lines, want 2", pruned)
		}
		record("after prune", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{})))
		mustDo(t, store.DeleteFinOpsPayer(ctx, scope, "payer-1"))
		record("after payer delete", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{})))
	})
}

// TestTablesParityFinOpsBoundedReadAndFilters proves a bounded read is a page
// of the ordered set, never a total, and that the list filters narrow a read
// alongside the single-value fields, the day range and the organization
// scope.
func TestTablesParityFinOpsBoundedReadAndFilters(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		other := tenancy.NewOrgScope(orgs.def)
		days := []string{"2026-01-10", "2026-01-11", "2026-01-12"}
		lines := []FinOpsCostLine{
			{Day: "2026-01-10", Account: "acct-a", Region: "us-east-1", Service: "compute", RecordType: "Usage", Team: "payments", NetAmortizedMicros: 10},
			{Day: "2026-01-10", Account: "acct-b", Region: "us-east-1", Service: "storage", RecordType: "Usage", Team: "search", NetAmortizedMicros: 20},
			{Day: "2026-01-11", Account: "acct-a", Region: "eu-west-1", Service: "compute", RecordType: "Usage", NetAmortizedMicros: 30},
			{Day: "2026-01-11", Account: "acct-b", Region: "eu-west-1", Service: "compute", RecordType: "Usage", Team: "payments", NetAmortizedMicros: 40},
			{Day: "2026-01-12", Account: "acct-c", Region: "us-east-1", Service: "tax", RecordType: "Tax", Team: "payments", NetAmortizedMicros: 50},
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", days, lines))
		// The same payer and day in another organization stay out of this
		// read scope.
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, other, "payer", []string{"2026-01-10"}, []FinOpsCostLine{
			{Day: "2026-01-10", Account: "acct-a", Region: "us-east-1", Service: "compute", RecordType: "Usage", Team: "payments", NetAmortizedMicros: 999},
		}))

		all := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer"}))
		if len(all) != 5 || all[4].NetAmortizedMicros != 50 {
			t.Fatalf("unbounded lines = %+v", all)
		}
		record("unbounded", all)
		bounded := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer", Limit: 3}))
		if len(bounded) != 3 {
			t.Fatalf("bounded lines = %+v", bounded)
		}
		for index := range bounded {
			if bounded[index] != all[index] {
				t.Fatalf("bounded line %d = %+v, want %+v", index, bounded[index], all[index])
			}
		}
		record("bounded", bounded)
		record("limit exact", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer", Limit: len(all)})))
		record("limit over", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer", Limit: len(all) + 5})))
		if _, err := store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Limit: -1}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("negative limit error = %v", err)
		}
		limited := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer", Limit: 1}))
		if len(limited) != 1 || limited[0].Lines != 5 || limited[0].NetAmortizedMicros != 150 {
			t.Fatalf("limited totals = %+v", limited)
		}
		record("totals ignore limit", limited)

		for label, query := range ordered(map[string]FinOpsCostQuery{
			"accounts":        {Payer: "payer", Accounts: []string{"acct-a", "acct-c"}},
			"regions":         {Payer: "payer", Regions: []string{"eu-west-1"}},
			"teams with none": {Payer: "payer", Teams: []string{"", "search"}},
			"all three lists": {Payer: "payer", Accounts: []string{"acct-a", "acct-b"}, Regions: []string{"us-east-1"}, Teams: []string{"payments"}},
			"list and single": {Payer: "payer", Accounts: []string{"acct-b"}, Service: "compute", From: "2026-01-11"},
			"empty lists":     {Payer: "payer", Accounts: []string{}, Regions: nil, Teams: []string{}},
		}) {
			record("lines "+label, must(store.FinOpsCostLines(ctx, scope, query)))
		}
		composed := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer", Accounts: []string{"acct-a", "acct-b"}, Regions: []string{"us-east-1"}, Teams: []string{"payments"}}))
		if len(composed) != 1 || composed[0].NetAmortizedMicros != 10 {
			t.Fatalf("composed filters = %+v", composed)
		}
		none := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer", Teams: []string{""}}))
		if len(none) != 1 || none[0].Day != "2026-01-11" {
			t.Fatalf("no-team filter = %+v", none)
		}
		record("totals by region filtered", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{Payer: "payer", Teams: []string{"payments"}}, FinOpsByRegion)))
	})
}

func TestTablesParityFinOpsDecisions(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		alerted := tablesBase.Add(5*time.Hour + 999*time.Nanosecond)
		increases := []FinOpsIncrease{
			{ID: "inc-1", Payer: "payer", Account: "acct-a", Service: "network.nat", UsageType: "NatGateway-Bytes", RecordType: "Usage", StartedOn: "2026-01-10", Status: "open",
				OwnerTeam: "payments", OwnerRule: "tag", MonthlyImpactMicros: 9_000_000, AlertedAt: alerted, Suspects: json.RawMessage(`[{"change":"e-1","score":0.9}]`),
				Detail: json.RawMessage(`{"daily_before_micros":10,"daily_now_micros":30}`), Events: []FinOpsIncreaseEvent{{At: tablesBase, Actor: "system", ToStatus: "open"}}},
			{ID: "inc-2", Account: "acct-b", Service: "compute", StartedOn: "2026-01-05", Status: "expected", OwnerTeam: "search", OwnerEstimated: true,
				ExpectedReason: "migration", ExpectedUntil: "2026-02-01", Events: []FinOpsIncreaseEvent{
					{At: tablesBase, Actor: "system", ToStatus: "open"}, {At: tablesBase, Actor: "alice", FromStatus: "open", ToStatus: "expected", Reason: "migration", Until: "2026-02-01"}}},
			{ID: "inc-3", Account: "acct-a", Service: "compute", StartedOn: "2026-01-05", ResolvedOn: "2026-01-20", Status: "resolved"},
		}
		for _, increase := range increases {
			mustDo(t, store.PutFinOpsIncrease(ctx, scope, increase))
		}
		open := must(store.FinOpsIncreases(ctx, scope, FinOpsIncreaseQuery{Statuses: []string{"open", "expected"}}))
		if len(open) != 2 || open[0].ID != "inc-2" || len(open[0].Events) != 2 || open[0].Events[1].Until != "2026-02-01" || !open[1].AlertedAt.Equal(tableTime(alerted)) {
			t.Fatalf("open increases = %+v", open)
		}
		record("open increases", open)
		for label, query := range ordered(map[string]FinOpsIncreaseQuery{
			"all": {}, "owner": {OwnerTeam: "payments"}, "service": {Service: "compute"}, "ids": {IDs: []string{"inc-3", "inc-1"}}, "none": {Statuses: []string{"dismissed"}},
		}) {
			record("increases "+label, must(store.FinOpsIncreases(ctx, scope, query)))
		}
		updated := increases[0]
		updated.Status = "acknowledged"
		updated.Suspects = nil
		updated.Events = append(append([]FinOpsIncreaseEvent(nil), updated.Events...), FinOpsIncreaseEvent{At: tablesBase.Add(time.Hour), Actor: "bob", FromStatus: "open", ToStatus: "acknowledged"})
		mustDo(t, store.PutFinOpsIncrease(ctx, scope, updated))
		again := must(store.FinOpsIncreases(ctx, scope, FinOpsIncreaseQuery{IDs: []string{"inc-1"}}))
		if len(again) != 1 || again[0].Status != "acknowledged" || len(again[0].Events) != 2 || again[0].Suspects != nil {
			t.Fatalf("updated increase = %+v", again)
		}
		record("updated increase", again)
		for label, bad := range ordered(map[string]FinOpsIncrease{
			"no id": {StartedOn: "2026-01-01"}, "no start": {ID: "x"}, "bad resolved": {ID: "x", StartedOn: "2026-01-01", ResolvedOn: "soon"},
			"bad suspects": {ID: "x", StartedOn: "2026-01-01", Suspects: json.RawMessage(`[`)},
		}) {
			if err := store.PutFinOpsIncrease(ctx, scope, bad); !errors.Is(err, ErrInvalidFinOps) {
				t.Fatalf("%s: increase error = %v", label, err)
			}
		}

		frozen := tablesBase.Add(48 * time.Hour)
		mustDo(t, store.PutFinOpsReport(ctx, scope, FinOpsReport{Month: "2026-02", TotalMicros: 20, Model: json.RawMessage(`{"summary":{"total_micros":20}}`)}))
		mustDo(t, store.PutFinOpsReport(ctx, scope, FinOpsReport{Month: "2026-01", FrozenAt: frozen, FrozenBy: "alice", TotalMicros: 10, Model: json.RawMessage(`{"summary":{"total_micros":10},"notes":["a"]}`)}))
		report, found, err := store.FinOpsReport(ctx, scope, "2026-01")
		if err != nil || !found || !report.FrozenAt.Equal(frozen) || len(report.Model) == 0 {
			t.Fatalf("report = %+v %v %v", report, found, err)
		}
		record("report", report)
		_, found, err = store.FinOpsReport(ctx, scope, "2026-03")
		if err != nil || found {
			t.Fatalf("missing report found=%v err=%v", found, err)
		}
		mustDo(t, store.PutFinOpsReport(ctx, scope, FinOpsReport{Month: "2026-02", FrozenAt: frozen, FrozenBy: "bob", TotalMicros: 21, Model: json.RawMessage(`{"summary":{"total_micros":21}}`)}))
		reports := must(store.FinOpsReports(ctx, scope))
		if len(reports) != 2 || reports[0].Month != "2026-01" || reports[1].TotalMicros != 21 || reports[0].Model != nil {
			t.Fatalf("reports = %+v", reports)
		}
		record("reports", reports)
		for label, bad := range ordered(map[string]FinOpsReport{"bad month": {Month: "2026-13", Model: json.RawMessage(`{}`)}, "no model": {Month: "2026-01"}}) {
			if err := store.PutFinOpsReport(ctx, scope, bad); !errors.Is(err, ErrInvalidFinOps) {
				t.Fatalf("%s: report error = %v", label, err)
			}
		}
		if _, _, err := store.FinOpsReport(ctx, scope, "January"); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("bad month read error = %v", err)
		}

		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "saving-b", State: "dismissed", Reason: "needed", Actor: "alice", At: tablesBase}))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "saving-a", State: "dismissed"}))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "saving-b", State: "open", Actor: "bob", At: tablesBase.Add(time.Hour)}))
		decisions := must(store.FinOpsSavingDecisions(ctx, scope))
		if len(decisions) != 2 || decisions[0].SavingID != "saving-a" || decisions[1].State != "open" || decisions[1].Reason != "" {
			t.Fatalf("decisions = %+v", decisions)
		}
		record("decisions", decisions)
		if err := store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("empty decision error = %v", err)
		}
	})
}

// TestTablesParityFinOpsDeleteIncreasesForPayer removes a payer's lines and
// the increases derived from them: the remaining list and events agree on
// every backend, an unrelated saving decision is untouched, and a second call
// changes nothing.
func TestTablesParityFinOpsDeleteIncreasesForPayer(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		const day = "2026-01-10"
		line := func(account string) FinOpsCostLine {
			return FinOpsCostLine{Day: day, Account: account, Service: "compute", RecordType: "Usage", NetAmortizedMicros: 5}
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-a", []string{day}, []FinOpsCostLine{line("acct-a")}))
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-b", []string{day}, []FinOpsCostLine{line("acct-b")}))
		increase := func(id, payer, account string) FinOpsIncrease {
			return FinOpsIncrease{ID: id, Payer: payer, Account: account, Service: "compute", StartedOn: day, Status: "open",
				Events: []FinOpsIncreaseEvent{{At: tablesBase, Actor: "system", ToStatus: "open"}}}
		}
		mustDo(t, store.PutFinOpsIncrease(ctx, scope, increase("inc-a", "payer-a", "acct-a")))
		mustDo(t, store.PutFinOpsIncrease(ctx, scope, increase("inc-b", "payer-b", "acct-b")))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "saving-keep", State: "dismissed"}))
		mustDo(t, store.DeleteFinOpsPayer(ctx, scope, "payer-a"))
		count, err := store.DeleteFinOpsIncreasesForPayer(ctx, scope, "payer-a")
		mustDo(t, err)
		record("deleted increases", count)
		record("lines after delete", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{})))
		remaining := must(store.FinOpsIncreases(ctx, scope, FinOpsIncreaseQuery{}))
		record("increases after delete", remaining)
		if len(remaining) != 1 || remaining[0].ID != "inc-b" || len(remaining[0].Events) != 1 {
			t.Fatalf("remaining increases = %+v", remaining)
		}
		decisions := must(store.FinOpsSavingDecisions(ctx, scope))
		record("decisions untouched", decisions)
		if len(decisions) != 1 || decisions[0].SavingID != "saving-keep" {
			t.Fatalf("saving decisions = %+v", decisions)
		}
		again, err := store.DeleteFinOpsIncreasesForPayer(ctx, scope, "payer-a")
		mustDo(t, err)
		record("second delete", again)
		if _, err := store.DeleteFinOpsIncreasesForPayer(ctx, scope, "bad\x00payer"); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("invalid payer error = %v", err)
		}
		if backend.raw != nil {
			var events int
			mustDo(t, backend.raw.QueryRow(backend.provider.(interface{ q(string) string }).q(`SELECT COUNT(*) FROM finops_increase_events WHERE org_id = ? AND increase_id = ?`), orgs.ent, "inc-a").Scan(&events))
			if events != 0 {
				t.Fatalf("%d events left for the deleted increase", events)
			}
		}
	})
}

// TestTablesParityFinOpsAmountOverflow rejects a sum that does not fit: every
// backend reports the closed overflow error, a single extreme line still
// answers exactly, and a representable sum is unaffected.
func TestTablesParityFinOpsAmountOverflow(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		lines := []FinOpsCostLine{
			{Day: "2026-01-10", Account: "acct-a", Service: "compute", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64},
			{Day: "2026-01-10", Account: "acct-a", Service: "storage", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64},
			{Day: "2026-01-11", Account: "acct-a", Service: "compute", RecordType: "Usage", NetAmortizedMicros: 5, QuantityMicros: math.MaxInt64},
			{Day: "2026-01-11", Account: "acct-b", Service: "compute", RecordType: "Usage", NetAmortizedMicros: 5, QuantityMicros: math.MaxInt64},
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-10", "2026-01-11"}, lines))
		_, err := store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{})
		record("grand overflow", errors.Is(err, ErrAmountOverflow))
		_, err = store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2026-01-10", Through: "2026-01-10"})
		record("net overflow", errors.Is(err, ErrAmountOverflow))
		_, err = store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2026-01-11", Through: "2026-01-11"})
		record("quantity overflow", errors.Is(err, ErrAmountOverflow))
		_, err = store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, FinOpsByUnit)
		record("grouped overflow", errors.Is(err, ErrAmountOverflow))
		record("one extreme line", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2026-01-10", Through: "2026-01-10", Service: "compute"})))
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-12"}, []FinOpsCostLine{{Day: "2026-01-12", Account: "acct-a", Service: "compute", RecordType: "Usage", NetAmortizedMicros: 42}}))
		record("representable", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2026-01-12", Through: "2026-01-12"})))
	})
}

// TestTablesParityCommitIsAtomicForReaders swaps a unit between two whole
// candidates while readers run: every read sees one candidate entirely,
// never a mix and never an empty unit.
func TestTablesParityCommitIsAtomicForReaders(t *testing.T) {
	const size, commits = 150, 24
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		candidate := func(version string, at time.Time) AWSCandidate {
			resources := make([]AWSResource, size)
			for index := range resources {
				resources[index] = AWSResource{ARN: tablesARN(fmt.Sprintf("i-%03d", index)), Type: "instance", State: version}
			}
			return AWSCandidate{At: at, Digest: "digest-" + version, Resources: resources}
		}
		must(store.CommitAWSUnit(ctx, scope, unit, candidate("a", tablesBase), nil))
		var done sync.WaitGroup
		stop := make(chan struct{})
		failures := make(chan string, 8)
		for reader := 0; reader < 3; reader++ {
			done.Add(1)
			go func() {
				defer done.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					rows, err := store.AWSResources(ctx, scope, AWSResourceQuery{Account: unit.Account, Region: unit.Region, Service: unit.Service})
					if err != nil {
						failures <- err.Error()
						return
					}
					states := map[string]int{}
					for _, row := range rows {
						states[row.State]++
					}
					if len(rows) != size || len(states) != 1 {
						failures <- fmt.Sprintf("read %d rows in states %v", len(rows), states)
						return
					}
				}
			}()
		}
		for index := 1; index <= commits; index++ {
			version := "a"
			if index%2 == 1 {
				version = "b"
			}
			if _, err := store.CommitAWSUnit(ctx, scope, unit, candidate(version, tablesBase.Add(time.Duration(index)*time.Second)), nil); err != nil {
				close(stop)
				done.Wait()
				t.Fatal(err)
			}
		}
		close(stop)
		done.Wait()
		close(failures)
		for failure := range failures {
			t.Fatal(failure)
		}
		units := must(store.AWSUnits(ctx, scope, AWSUnitQuery{}))
		record("final generation", units[0].Generation.Seq)
		record("history rows", must(store.PruneAWSHistory(ctx, scope, tablesBase.Add(time.Hour))))
	})
}

// injectTableFailure makes the backend's database fail any insert of a
// security flag named explode or a cost line for account explode, inside
// the transaction that writes it.
func injectTableFailure(t *testing.T, backend tablesBackend, org string) {
	t.Helper()
	if !backend.postgres {
		_, err := backend.raw.Exec(`
CREATE TRIGGER IF NOT EXISTS explode_flags BEFORE INSERT ON aws_security_flags WHEN NEW.flag = 'explode' BEGIN SELECT RAISE(ABORT, 'injected failure'); END;
CREATE TRIGGER IF NOT EXISTS explode_lines BEFORE INSERT ON finops_cost_lines WHEN NEW.account = 'explode' BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`)
		mustDo(t, err)
		return
	}
	name := "explode_" + strings.NewReplacer("-", "_").Replace(org)
	for _, target := range []struct{ table, column string }{{"aws_security_flags", "flag"}, {"finops_cost_lines", "account"}} {
		function := name + "_" + target.column
		_, err := backend.raw.Exec(`CREATE FUNCTION ` + function + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.org_id = '` + org + `' AND NEW.` + target.column + ` = 'explode' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER ` + function + ` BEFORE INSERT ON ` + target.table + ` FOR EACH ROW EXECUTE FUNCTION ` + function + `();`)
		mustDo(t, err)
		t.Cleanup(func() {
			_, _ = backend.raw.Exec(`DROP TRIGGER IF EXISTS ` + function + ` ON ` + target.table + `; DROP FUNCTION IF EXISTS ` + function + `();`)
		})
	}
}

// TestTablesSQLWritesRollBackOnFailure fails a commit and a day replace
// after their deletes and closes ran: the previous rows stay current.
func TestTablesSQLWritesRollBackOnFailure(t *testing.T) {
	ctx := context.Background()
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			defer failOnPanic(t)
			orgs := newTablesOrgs()
			scope := tenancy.NewOrgScope(orgs.ent)
			injectTableFailure(t, backend, orgs.ent)
			inventory, finops := backend.inventory(t), backend.finops(t)
			unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
			must(inventory.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase, Digest: "one", Resources: []AWSResource{{ARN: tablesARN("i-1"), Type: "instance", State: "running"}},
				Health: []AWSHealth{{ARN: tablesARN("i-1"), Kind: "alarm", State: "ok"}}}, nil))
			_, err := inventory.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase.Add(time.Hour), Digest: "two",
				Resources: []AWSResource{{ARN: tablesARN("i-2"), Type: "instance"}}, Flags: []AWSSecurityFlag{{ARN: tablesARN("i-2"), Flag: "explode"}}}, nil)
			if err == nil || errors.Is(err, ErrInvalidAWSInventory) {
				t.Fatalf("injected commit error = %v", err)
			}
			resources := must(inventory.AWSResources(ctx, scope, AWSResourceQuery{}))
			health := must(inventory.AWSHealth(ctx, scope, AWSHealthQuery{}))
			units := must(inventory.AWSUnits(ctx, scope, AWSUnitQuery{}))
			if len(resources) != 1 || resources[0].ARN != tablesARN("i-1") || len(health) != 1 || units[0].Generation.Seq != 1 || units[0].Sync.Generation != 1 {
				t.Fatalf("a failed commit changed the unit: %+v %+v %+v", resources, health, units)
			}
			if removed := must(inventory.PruneAWSHistory(ctx, scope, tablesBase.Add(48*time.Hour))); removed != 0 {
				t.Fatalf("a failed commit left %d closed rows", removed)
			}

			lines := []FinOpsCostLine{{Day: "2026-01-01", Account: "a", Service: "compute", NetAmortizedMicros: 5}}
			mustDo(t, finops.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-01"}, lines))
			err = finops.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-01"}, []FinOpsCostLine{{Day: "2026-01-01", Account: "b", Service: "compute"}, {Day: "2026-01-01", Account: "explode"}})
			if err == nil || errors.Is(err, ErrInvalidFinOps) {
				t.Fatalf("injected replace error = %v", err)
			}
			kept := must(finops.FinOpsCostLines(ctx, scope, FinOpsCostQuery{}))
			if len(kept) != 1 || kept[0].Account != "a" || kept[0].NetAmortizedMicros != 5 {
				t.Fatalf("a failed day replace changed the day: %+v", kept)
			}
		})
	}
}

// TestTablesStoresAreDiscoverable finds both stores on every provider by
// type assertion, and a provider value without tables answers unsupported
// instead of panicking.
func TestTablesStoresAreDiscoverable(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		if _, ok := AWSInventory(backend.provider); !ok {
			t.Fatalf("%s has no inventory store", backend.name)
		}
		if _, ok := FinOps(backend.provider); !ok {
			t.Fatalf("%s has no FinOps store", backend.name)
		}
	}
	if _, err := (*sqlTables)(nil).tablesDB(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a provider without tables answered %v", err)
	}
	if _, err := (&postgresProvider{}).AWSResources(context.Background(), tenancy.DefaultOrgScope(), AWSResourceQuery{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a Postgres provider without a pool answered %v", err)
	}
}

// TestTablesParityUnitHistoryKeepsNewestGenerations bounds one unit's
// history by count: older generations and the rows that ended before the
// oldest kept generation go; rows still current in it stay.
func TestTablesParityUnitHistoryKeepsNewestGenerations(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		other := AWSUnit{tablesAccount, tablesRegion, "rds"}
		steady := AWSResource{ARN: tablesARN("steady"), Type: "instance"}
		for index, id := range []string{"a", "b", "c", "d"} {
			at := tablesBase.Add(time.Duration(index) * time.Minute)
			must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: at, Digest: id, Resources: []AWSResource{steady, {ARN: tablesARN(id), Type: "instance"}},
				Health: []AWSHealth{{ARN: tablesARN(id), Kind: "alarm", State: "ok"}}}, nil))
			must(store.CommitAWSUnit(ctx, scope, other, AWSCandidate{At: at, Digest: id, Resources: []AWSResource{{ARN: tablesARN("db-" + id), Type: "db"}}}, nil))
		}
		removed := must(store.PruneAWSUnitHistory(ctx, scope, unit, 2))
		// Generations a and b, the resources a and b, the health rows a and b.
		if removed != 6 {
			t.Fatalf("pruned %d rows, want 6", removed)
		}
		record("pruned", removed)
		for _, minute := range []int{0, 1, 2, 3} {
			at := tablesBase.Add(time.Duration(minute)*time.Minute + 30*time.Second)
			record(fmt.Sprintf("units at %d", minute), must(store.AWSUnits(ctx, scope, AWSUnitQuery{AsOf: at})))
			record(fmt.Sprintf("resources at %d", minute), must(store.AWSResources(ctx, scope, AWSResourceQuery{Service: "ec2", AsOf: at})))
		}
		units := must(store.AWSUnits(ctx, scope, AWSUnitQuery{Service: "ec2", AsOf: tablesBase.Add(30 * time.Second)}))
		if units[0].Generation != nil {
			t.Fatalf("a pruned generation still answers: %+v", units[0])
		}
		kept := must(store.AWSResources(ctx, scope, AWSResourceQuery{Service: "ec2", AsOf: tablesBase.Add(150 * time.Second)}))
		if got := strings.Join(arnsOf(kept), ","); got != tablesARN("c")+"@,"+tablesARN("steady")+"@" {
			t.Fatalf("kept generation reads %s", got)
		}
		if again := must(store.PruneAWSUnitHistory(ctx, scope, unit, 2)); again != 0 {
			t.Fatalf("a second prune removed %d rows", again)
		}
		if none := must(store.PruneAWSUnitHistory(ctx, scope, AWSUnit{tablesAccount, tablesRegion, "lambda"}, 2)); none != 0 {
			t.Fatalf("a unit without history pruned %d rows", none)
		}
		if _, err := store.PruneAWSUnitHistory(ctx, scope, unit, 0); !errors.Is(err, ErrInvalidAWSInventory) {
			t.Fatalf("keep 0 error = %v", err)
		}
		record("other unit untouched", must(store.AWSResources(ctx, scope, AWSResourceQuery{Service: "rds", AsOf: tablesBase.Add(30 * time.Second)})))
	})
}

// TestTablesParityServiceNameKeepsProviderServicesApart stores two provider
// services that map to one neutral service: they stay two lines, sum per
// provider service and filter by it.
func TestTablesParityServiceNameKeepsProviderServicesApart(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		line := func(serviceName string, micros int64) FinOpsCostLine {
			return FinOpsCostLine{Day: "2026-01-01", Account: "acct-a", Region: tablesRegion, Service: "compute", ServiceName: serviceName, UsageType: "BoxUsage", RecordType: "Usage", NetAmortizedMicros: micros}
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-01"}, []FinOpsCostLine{line("Amazon Elastic Compute Cloud - Compute", 5), line("EC2 - Other", 7)}))
		if err := store.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-01"}, []FinOpsCostLine{line("EC2 - Other", 1), line("EC2 - Other", 2)}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("a repeated key with the same service name = %v", err)
		}
		lines := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Service: "compute"}))
		if len(lines) != 2 || lines[0].ServiceName != "Amazon Elastic Compute Cloud - Compute" || lines[1].ServiceName != "EC2 - Other" {
			t.Fatalf("lines = %+v", lines)
		}
		record("lines", lines)
		byName := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, FinOpsByService, FinOpsByServiceName))
		if len(byName) != 2 || byName[1].ServiceName != "EC2 - Other" || byName[1].NetAmortizedMicros != 7 {
			t.Fatalf("totals by service name = %+v", byName)
		}
		record("by service name", byName)
		byService := must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{}, FinOpsByService))
		if len(byService) != 1 || byService[0].NetAmortizedMicros != 12 {
			t.Fatalf("totals by service = %+v", byService)
		}
		record("by service", byService)
		record("filtered", must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{ServiceName: "EC2 - Other"})))
	})
}

// TestTablesParityAccountViewIsOneSnapshot: the account view answers units
// and rows from the same generation, and an as-of view answers the rows that
// were current then.
func TestTablesParityAccountViewIsOneSnapshot(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		t0, t1 := tablesBase, tablesBase.Add(time.Hour)
		r1 := AWSResource{ARN: tablesARN("i-1"), Type: "instance", Name: "first"}
		r2 := AWSResource{ARN: tablesARN("i-2"), Type: "instance", Name: "second"}
		h1 := AWSHealth{ARN: r1.ARN, Kind: "alarm", Ref: "cpu", State: "critical"}
		f1 := AWSSecurityFlag{ARN: r1.ARN, Flag: "public_exposure"}
		must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: t0, Digest: "a", Resources: []AWSResource{r1}, Health: []AWSHealth{h1}, Flags: []AWSSecurityFlag{f1}}, nil))
		must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: t1, Digest: "b", Resources: []AWSResource{r2}}, nil))

		current, err := store.AWSAccountView(ctx, scope, tablesAccount, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		record("current units", current.Units)
		record("current resources", arnsOf(current.Resources))
		record("current health", current.Health)
		record("current flags", current.Flags)
		if len(current.Units) != 1 || current.Units[0].Generation == nil || current.Units[0].Generation.Seq != 2 || current.Units[0].Generation.Count != len(current.Resources) {
			t.Fatalf("current view pairs status %+v with %d resources", current.Units, len(current.Resources))
		}

		historical, err := store.AWSAccountView(ctx, scope, tablesAccount, t0.Add(30*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		record("as-of units", historical.Units)
		record("as-of resources", arnsOf(historical.Resources))
		record("as-of health", historical.Health)
		record("as-of flags", historical.Flags)
		if len(historical.Units) != 1 || historical.Units[0].Generation == nil || historical.Units[0].Generation.Seq != 1 || len(historical.Resources) != 1 || historical.Resources[0].ARN != r1.ARN {
			t.Fatalf("as-of view = %+v with %+v", historical.Units, historical.Resources)
		}
	})
}

// TestTablesParityPlanRegistration: the first registration records every
// unit as pending, and a later registration never resets a unit that already
// has a state.
func TestTablesParityPlanRegistration(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		units := []AWSUnit{{tablesAccount, tablesRegion, "ec2"}, {tablesAccount, tablesRegion, "rds"}}
		mustDo(t, store.RegisterAWSPending(ctx, scope, units, nil))
		record("first registration", must(store.AWSUnits(ctx, scope, AWSUnitQuery{})))
		must(store.CommitAWSUnit(ctx, scope, units[0], AWSCandidate{At: tablesBase, Digest: "a", Resources: []AWSResource{{ARN: tablesARN("i-1"), Type: "instance"}}}, nil))
		mustDo(t, store.RegisterAWSPending(ctx, scope, units, nil))
		record("second registration", must(store.AWSUnits(ctx, scope, AWSUnitQuery{})))
	})
}

// TestTablesParityCommitClampsClockSkew: a candidate time behind the current
// generation's start is clamped under the write lock on every backend.
func TestTablesParityCommitClampsClockSkew(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.inventory(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		unit := AWSUnit{tablesAccount, tablesRegion, "ec2"}
		must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase, Digest: "a", Resources: []AWSResource{{ARN: tablesARN("i-1"), Type: "instance"}}}, nil))
		behind := must(store.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase.Add(-time.Hour), Digest: "b", Resources: []AWSResource{{ARN: tablesARN("i-2"), Type: "instance"}}}, nil))
		record("committed at", behind.Generation.CommittedAt)
		if !behind.Generation.CommittedAt.Equal(tablesBase) {
			t.Fatalf("committed at %s, want %s", behind.Generation.CommittedAt, tablesBase)
		}
		units := must(store.AWSUnits(ctx, scope, AWSUnitQuery{}))
		if len(units) != 1 || units[0].Generation == nil || !units[0].Generation.CommittedAt.Equal(tablesBase) {
			t.Fatalf("units = %+v", units)
		}
	})
}

// oldShapeTablesSchema is the table schema as it stood before
// finops_cost_lines gained service_name: the migrations that existed then,
// rendered for SQLite. It is pinned by name rather than derived from the
// current schema, so a later column or index cannot quietly change the shape
// this fixture tests.
func oldShapeTablesSchema(t *testing.T) string {
	t.Helper()
	var schema strings.Builder
	for _, name := range []string{"migrations/031_aws_inventory_tables.sql", "migrations/032_finops_tables.sql", "migrations/033_finops_cost_lines_region_index.sql"} {
		data, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(data), ` COLLATE "C"`, "")
		text = sqliteIndexInclude.ReplaceAllString(text, ", $1)")
		text = strings.NewReplacer("TIMESTAMPTZ", "TEXT", "JSONB", "TEXT", "BOOLEAN", "INTEGER", "FALSE", "0").Replace(text)
		text = sqliteDateType.ReplaceAllString(text, "TEXT")
		schema.WriteString(text)
		schema.WriteString("\n")
	}
	old := strings.Replace(schema.String(), "    service_name         TEXT NOT NULL DEFAULT '',\n", "", 1)
	old = strings.Replace(old, "service_name, ", "", 1)
	if old == schema.String() {
		t.Fatal("the old-shape schema did not remove service_name")
	}
	return old
}

// TestFileSchemaDriftFailsClosed builds the table database the way the
// earlier FinOps migration did — without finops_cost_lines.service_name — and
// requires startup to fail with the named error instead of starting a store
// whose writes later fail with an unclassified SQL error.
func TestFileSchemaDriftFailsClosed(t *testing.T) {
	dir := t.TempDir()
	old := oldShapeTablesSchema(t)
	path := dir + string(os.PathSeparator) + localTablesFile
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(old); err != nil {
		_ = db.Close()
		t.Fatalf("%v\n%s", err, old)
	}
	_ = db.Close()

	provider, err := NewFile(FileOptions{DataDir: dir})
	if err == nil {
		_ = provider.Close()
		t.Fatal("an old-shape database started")
	}
	var drift *SchemaOutOfDateError
	if !errors.As(err, &drift) || !errors.Is(err, ErrSchemaOutOfDate) {
		t.Fatalf("error = %v, want a classed schema error", err)
	}
	if drift.Table != "finops_cost_lines" || drift.Column != "service_name" {
		t.Fatalf("drift = %+v", drift)
	}
}

// finOpsTotalsIndexKey and finOpsTotalsIndexCovered are the totals index's
// grouping key and the columns it carries for the aggregation. The key is in
// the order the page totals query groups by, and the covered columns are
// every other column that statement reads.
var (
	finOpsTotalsIndexKey     = []string{"org_id", "day", "account", "region", "service", "service_name", "usage_group", "record_type", "team", "payer"}
	finOpsTotalsIndexCovered = []string{"net_amortized_micros", "quantity_micros", "estimated_team", "estimated_region", "usage_type", "resource_id"}
)

// TestFinOpsTotalsIndexCoversAggregation requires the totals index on every
// SQL backend. The store's other cost-line indexes lead with day, team,
// service, account, region or resource; none of them can answer the totals
// aggregation without visiting the table, so this index has to exist and to
// carry the amount columns the sum reads.
func TestFinOpsTotalsIndexCoversAggregation(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			if backend.raw == nil {
				t.Skip("no SQL schema")
			}
			var key, covered []string
			if backend.postgres {
				rows, err := backend.raw.Query(`SELECT a.attname, k.ord, i.indnkeyatts
					FROM pg_index i
					JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
					JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
					WHERE i.indexrelid = to_regclass('finops_cost_lines_totals')
					ORDER BY k.ord`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var name string
					var ord, keyCount int
					if err := rows.Scan(&name, &ord, &keyCount); err != nil {
						t.Fatal(err)
					}
					if ord <= keyCount {
						key = append(key, name)
						continue
					}
					covered = append(covered, name)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				rows, err := backend.raw.Query(`PRAGMA index_info(finops_cost_lines_totals)`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var seqno, cid int
					var name string
					if err := rows.Scan(&seqno, &cid, &name); err != nil {
						t.Fatal(err)
					}
					key = append(key, name)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				// SQLite has no INCLUDE: the covered columns follow the key,
				// where an index-only scan still reads them.
				if len(key) >= len(finOpsTotalsIndexKey) {
					covered = key[len(finOpsTotalsIndexKey):]
					key = key[:len(finOpsTotalsIndexKey)]
				}
			}
			if strings.Join(key, ",") != strings.Join(finOpsTotalsIndexKey, ",") {
				t.Fatalf("%s totals index key = %v, want %v", backend.name, key, finOpsTotalsIndexKey)
			}
			if strings.Join(covered, ",") != strings.Join(finOpsTotalsIndexCovered, ",") {
				t.Fatalf("%s totals index covered columns = %v, want %v", backend.name, covered, finOpsTotalsIndexCovered)
			}
		})
	}
}

// TestFinOpsTotalsPlanUsesCoveringIndex proves the page totals aggregation
// is answered from the totals index: the index supplies the grouping key in
// order and every summed column, so the plan neither scans the table nor
// sorts into a temporary b-tree.
func TestFinOpsTotalsPlanUsesCoveringIndex(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	store, ok := FinOps(provider)
	if !ok {
		t.Fatal("file provider has no FinOps store")
	}
	ctx := context.Background()
	scope := tenancy.NewOrgScope(newTablesOrgs().ent)
	days := []string{"2026-01-01", "2026-01-02"}
	lines := make([]FinOpsCostLine, 0, 4000)
	for _, day := range days {
		for index := 0; index < 2000; index++ {
			lines = append(lines, FinOpsCostLine{Day: day, Payer: "payer", Account: fmt.Sprintf("acct-%d", index%9), Region: "us-east-1",
				Service: []string{"compute", "storage"}[index%2], ServiceName: fmt.Sprintf("service-%d", index%3), UsageGroup: "group",
				UsageType: fmt.Sprintf("usage-%04d", index), RecordType: "Usage", Team: []string{"payments", "search", ""}[index%3],
				NetAmortizedMicros: int64(1000 + index), QuantityMicros: int64(index)})
		}
	}
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", days, lines))
	tables := provider.(*fileProvider).sqlTables
	raw, err := tables.tablesDB()
	mustDo(t, err)
	if _, err := raw.Exec(`ANALYZE finops_cost_lines`); err != nil {
		t.Fatal(err)
	}
	noResource := ""
	statement, args := tables.costTotalsSQL(inventoryScope(scope), FinOpsCostQuery{From: days[0], Through: days[len(days)-1], ResourceID: &noResource},
		[]FinOpsDimension{FinOpsByDay, FinOpsByAccount, FinOpsByRegion, FinOpsByService, FinOpsByServiceName, FinOpsByUsageGroup, FinOpsByRecordType, FinOpsByTeam, FinOpsByPayer})
	rows, err := raw.Query("EXPLAIN QUERY PLAN "+statement, args...)
	mustDo(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		mustDo(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	mustDo(t, rows.Err())
	joined := strings.Join(plan, "; ")
	for _, detail := range plan {
		if strings.HasPrefix(detail, "SCAN l ") || detail == "SCAN l" {
			t.Fatalf("totals aggregation scans the cost-line table: %s", joined)
		}
	}
	if !strings.Contains(joined, "COVERING INDEX finops_cost_lines_totals") {
		t.Fatalf("totals aggregation is not answered from the covering index: %s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("totals aggregation sorts instead of streaming the index order: %s", joined)
	}
}

// TestTablesParityFinOpsSavingDecisionUpkeep keeps decisions in step with
// the savings they refer to on every backend: a decision whose saving is no
// longer produced is marked orphaned (not dropped, so an audit still reads
// what it referred to), keeps its first mark while the saving stays away, is
// cleared when the saving returns, is pruned only once orphaned long enough,
// and goes with its payer. Another organization's decisions are untouched.
func TestTablesParityFinOpsSavingDecisionUpkeep(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		upkeep, ok := store.(FinOpsSavingDecisionStore)
		if !ok {
			t.Fatalf("%s has no saving decision upkeep", backend.name)
		}
		scope := tenancy.NewOrgScope(orgs.ent)
		other := tenancy.NewOrgScope(orgs.def)
		decision := func(id, payer string) FinOpsSavingDecision {
			return FinOpsSavingDecision{SavingID: id, State: "dismissed", Reason: "kept for DR", Actor: "alice", At: tablesBase,
				Payer: payer, Account: "acct-" + id, Kind: "idle", Title: "Delete 1 unattached disk volume " + id}
		}
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, decision("s-1", "payer-a")))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, decision("s-2", "payer-b")))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, decision("s-3", "payer-a")))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, other, decision("s-9", "payer-a")))
		stored := must(store.FinOpsSavingDecisions(ctx, scope))
		if len(stored) != 3 || stored[0].Payer != "payer-a" || stored[0].Title != "Delete 1 unattached disk volume s-1" || !stored[0].OrphanedAt.IsZero() {
			t.Fatalf("decisions with their subject = %+v", stored)
		}
		record("stored", stored)

		first, second, third := tablesBase.Add(time.Hour), tablesBase.Add(2*time.Hour), tablesBase.Add(3*time.Hour)
		sync := func(label string, at time.Time, present ...string) []FinOpsSavingDecision {
			t.Helper()
			orphaned, restored, err := upkeep.SyncFinOpsSavingDecisions(ctx, scope, present, at)
			mustDo(t, err)
			record(label+" counts", []int64{orphaned, restored})
			out := must(store.FinOpsSavingDecisions(ctx, scope))
			record(label, out)
			return out
		}
		byID := func(list []FinOpsSavingDecision) map[string]FinOpsSavingDecision {
			out := map[string]FinOpsSavingDecision{}
			for _, item := range list {
				out[item.SavingID] = item
			}
			return out
		}

		after := byID(sync("s-3 gone", first, "s-1", "s-2"))
		if !after["s-3"].OrphanedAt.Equal(first) || !after["s-1"].OrphanedAt.IsZero() || after["s-3"].Title == "" {
			t.Fatalf("an absent saving's decision is not marked orphaned with its subject kept: %+v", after)
		}
		after = byID(sync("s-2 gone", second, "s-1"))
		if !after["s-2"].OrphanedAt.Equal(second) || !after["s-3"].OrphanedAt.Equal(first) {
			t.Fatalf("an orphan mark moved or was not set: %+v", after)
		}
		after = byID(sync("s-3 back", third, "s-1", "s-3"))
		if !after["s-3"].OrphanedAt.IsZero() || !after["s-2"].OrphanedAt.Equal(second) {
			t.Fatalf("a returning saving's decision stayed orphaned: %+v", after)
		}
		if decisions := must(store.FinOpsSavingDecisions(ctx, other)); len(decisions) != 1 || !decisions[0].OrphanedAt.IsZero() {
			t.Fatalf("another organization's decision changed: %+v", decisions)
		}

		pruned, err := upkeep.PruneFinOpsSavingDecisions(ctx, scope, second)
		mustDo(t, err)
		record("prune at the mark", pruned)
		if pruned != 0 {
			t.Fatalf("a decision orphaned exactly at the cutoff was pruned: %d", pruned)
		}
		pruned, err = upkeep.PruneFinOpsSavingDecisions(ctx, scope, second.Add(time.Microsecond))
		mustDo(t, err)
		record("prune after the mark", pruned)
		left := byID(must(store.FinOpsSavingDecisions(ctx, scope)))
		if pruned != 1 || len(left) != 2 || left["s-1"].SavingID == "" || left["s-3"].SavingID == "" {
			t.Fatalf("prune removed %d, left %+v", pruned, left)
		}

		removed, err := upkeep.DeleteFinOpsSavingDecisionsForPayer(ctx, scope, "payer-a")
		mustDo(t, err)
		record("payer delete", removed)
		if left := must(store.FinOpsSavingDecisions(ctx, scope)); removed != 2 || len(left) != 0 {
			t.Fatalf("payer delete removed %d, left %+v", removed, left)
		}
		again, err := upkeep.DeleteFinOpsSavingDecisionsForPayer(ctx, scope, "payer-a")
		mustDo(t, err)
		record("second payer delete", again)
		if decisions := must(store.FinOpsSavingDecisions(ctx, other)); len(decisions) != 1 {
			t.Fatalf("another organization's decision went with the payer: %+v", decisions)
		}

		if _, _, err := upkeep.SyncFinOpsSavingDecisions(ctx, scope, nil, time.Time{}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("sync without a time error = %v", err)
		}
		if _, _, err := upkeep.SyncFinOpsSavingDecisions(ctx, scope, []string{""}, first); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("sync with an empty saving ID error = %v", err)
		}
		if _, err := upkeep.DeleteFinOpsSavingDecisionsForPayer(ctx, scope, "bad\x00payer"); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("invalid payer error = %v", err)
		}
		if _, err := upkeep.PruneFinOpsSavingDecisions(ctx, scope, time.Time{}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("prune without a time error = %v", err)
		}
	})
}

// TestTablesParityFinOpsSavingDecisionSubjectFill fills in what a decision
// refers to when it was stored without a payer, on every backend: a decision
// from before the subject was kept takes payer, account, kind and title; one
// that kept its subject but had no payer takes only the payer; a decision
// with a payer is never touched; another organization's is untouched. Once
// filled, the payer delete removes it.
func TestTablesParityFinOpsSavingDecisionSubjectFill(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		fill, ok := store.(FinOpsSavingDecisionSubjects)
		if !ok {
			t.Fatalf("%s cannot fill decision subjects", backend.name)
		}
		upkeep := store.(FinOpsSavingDecisionStore)
		scope := tenancy.NewOrgScope(orgs.ent)
		other := tenancy.NewOrgScope(orgs.def)
		bare := FinOpsSavingDecision{SavingID: "s-old", State: "dismissed", Reason: "kept for DR", Actor: "alice", At: tablesBase}
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, bare))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "s-kept", State: "dismissed", At: tablesBase,
			Account: "acct-kept", Kind: "idle", Title: "as decided"}))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: "s-paid", State: "dismissed", At: tablesBase,
			Payer: "payer-b", Account: "acct-paid", Kind: "idle", Title: "paid"}))
		mustDo(t, store.PutFinOpsSavingDecision(ctx, other, bare))

		subjects := []FinOpsSavingDecision{
			{SavingID: "s-old", Payer: "payer-a", Account: "acct-old", Kind: "rightsize", Title: "Rightsize 1 instance"},
			{SavingID: "s-kept", Payer: "payer-a", Account: "acct-kept", Kind: "idle", Title: "as listed now"},
			{SavingID: "s-paid", Payer: "payer-a", Account: "acct-other", Kind: "other", Title: "other"},
			{SavingID: "s-absent", Payer: "payer-a", Account: "acct-x", Kind: "idle", Title: "no decision"},
			{SavingID: "s-empty"},
		}
		changed, err := fill.FillFinOpsSavingDecisionSubjects(ctx, scope, subjects)
		mustDo(t, err)
		record("filled", changed)
		if changed != 2 {
			t.Fatalf("filled %d decisions, want 2", changed)
		}
		byID := map[string]FinOpsSavingDecision{}
		for _, decision := range must(store.FinOpsSavingDecisions(ctx, scope)) {
			byID[decision.SavingID] = decision
		}
		record("after fill", byID)
		if got := byID["s-old"]; got.Payer != "payer-a" || got.Account != "acct-old" || got.Kind != "rightsize" || got.Title != "Rightsize 1 instance" ||
			got.State != "dismissed" || got.Reason != "kept for DR" || got.Actor != "alice" || !got.At.Equal(tablesBase) {
			t.Fatalf("a decision from before the subject was kept = %+v", got)
		}
		if got := byID["s-kept"]; got.Payer != "payer-a" || got.Title != "as decided" {
			t.Fatalf("a decision that kept its subject = %+v", got)
		}
		if got := byID["s-paid"]; got.Payer != "payer-b" || got.Account != "acct-paid" || got.Title != "paid" {
			t.Fatalf("a decision with a payer changed = %+v", got)
		}
		if decisions := must(store.FinOpsSavingDecisions(ctx, other)); len(decisions) != 1 || decisions[0].Payer != "" {
			t.Fatalf("another organization's decision changed: %+v", decisions)
		}
		again, err := fill.FillFinOpsSavingDecisionSubjects(ctx, scope, subjects)
		mustDo(t, err)
		record("filled again", again)
		if again != 0 {
			t.Fatalf("a second fill changed %d decisions", again)
		}

		removed, err := upkeep.DeleteFinOpsSavingDecisionsForPayer(ctx, scope, "payer-a")
		mustDo(t, err)
		record("payer delete", removed)
		if left := must(store.FinOpsSavingDecisions(ctx, scope)); removed != 2 || len(left) != 1 || left[0].SavingID != "s-paid" {
			t.Fatalf("payer delete removed %d, left %+v", removed, left)
		}

		if _, err := fill.FillFinOpsSavingDecisionSubjects(ctx, scope, []FinOpsSavingDecision{{SavingID: "", Payer: "p"}}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("fill without a saving ID error = %v", err)
		}
		if _, err := fill.FillFinOpsSavingDecisionSubjects(ctx, scope, []FinOpsSavingDecision{{SavingID: "s", Payer: "bad\x00payer"}}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("fill with an invalid payer error = %v", err)
		}
	})
}

// TestTablesParityFinOpsSavingDecisionDeleteWithoutPayer deletes, on every
// backend, the decisions of a removed payer that were never given a payer:
// those with an empty payer on one of the payer's accounts. A decision with
// a payer is kept even on the same account, as are empty-payer decisions on
// other accounts and another organization's.
func TestTablesParityFinOpsSavingDecisionDeleteWithoutPayer(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		fill, ok := store.(FinOpsSavingDecisionSubjects)
		if !ok {
			t.Fatalf("%s cannot delete decisions without a payer", backend.name)
		}
		scope := tenancy.NewOrgScope(orgs.ent)
		other := tenancy.NewOrgScope(orgs.def)
		put := func(scope tenancy.OrgScope, id, payer, account string) {
			mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: id, State: "dismissed", At: tablesBase, Payer: payer, Account: account}))
		}
		put(scope, "s-unfilled", "", "acct-a1")
		put(scope, "s-unfilled-payer-account", "", "payer-a")
		put(scope, "s-other-account", "", "acct-b1")
		put(scope, "s-no-account", "", "")
		put(scope, "s-paid-same-account", "payer-b", "acct-a1")
		put(other, "s-unfilled", "", "acct-a1")

		removed, err := fill.DeleteFinOpsSavingDecisionsWithoutPayer(ctx, scope, []string{"acct-a1", "payer-a", "acct-a1", ""})
		mustDo(t, err)
		record("deleted", removed)
		left := map[string]string{}
		for _, decision := range must(store.FinOpsSavingDecisions(ctx, scope)) {
			left[decision.SavingID] = decision.Payer
		}
		record("left", left)
		if removed != 2 || len(left) != 3 || left["s-paid-same-account"] != "payer-b" {
			t.Fatalf("deleted %d, left %v", removed, left)
		}
		for _, kept := range []string{"s-other-account", "s-no-account", "s-paid-same-account"} {
			if _, ok := left[kept]; !ok {
				t.Fatalf("%s was deleted; left %v", kept, left)
			}
		}
		if decisions := must(store.FinOpsSavingDecisions(ctx, other)); len(decisions) != 1 {
			t.Fatalf("another organization's decision was deleted: %+v", decisions)
		}
		again, err := fill.DeleteFinOpsSavingDecisionsWithoutPayer(ctx, scope, []string{"acct-a1", "payer-a"})
		mustDo(t, err)
		if again != 0 {
			t.Fatalf("a second delete removed %d", again)
		}
		if none, err := fill.DeleteFinOpsSavingDecisionsWithoutPayer(ctx, scope, nil); err != nil || none != 0 {
			t.Fatalf("a delete naming no account = %d %v", none, err)
		}
		if _, err := fill.DeleteFinOpsSavingDecisionsWithoutPayer(ctx, scope, []string{"bad\x00account"}); !errors.Is(err, ErrInvalidFinOps) {
			t.Fatalf("an invalid account error = %v", err)
		}
	})
}

// TestTablesParityFinOpsSavingDecisionSyncBatches marks more decisions than
// one statement names, so the batched update is exercised on every backend.
func TestTablesParityFinOpsSavingDecisionSyncBatches(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		upkeep := store.(FinOpsSavingDecisionStore)
		scope := tenancy.NewOrgScope(orgs.ent)
		total := savingDecisionBatch*2 + 7
		present := make([]string, 0, total)
		for index := 0; index < total; index++ {
			id := fmt.Sprintf("s-%04d", index)
			present = append(present, id)
			mustDo(t, store.PutFinOpsSavingDecision(ctx, scope, FinOpsSavingDecision{SavingID: id, State: "dismissed", Payer: "payer"}))
		}
		orphaned, restored, err := upkeep.SyncFinOpsSavingDecisions(ctx, scope, nil, tablesBase)
		mustDo(t, err)
		record("orphaned all", []int64{orphaned, restored})
		if orphaned != int64(total) || restored != 0 {
			t.Fatalf("orphaned %d restored %d of %d", orphaned, restored, total)
		}
		orphaned, restored, err = upkeep.SyncFinOpsSavingDecisions(ctx, scope, present, tablesBase.Add(time.Hour))
		mustDo(t, err)
		record("restored all", []int64{orphaned, restored})
		if orphaned != 0 || restored != int64(total) {
			t.Fatalf("orphaned %d restored %d of %d", orphaned, restored, total)
		}
	})
}

// TestFileTablesGainSavingDecisionSubject opens a local tables database
// built before saving decisions carried their subject. The columns are added
// in place, the stored decision keeps its values, and a new decision stores
// its subject; reopening adds nothing twice.
func TestFileTablesGainSavingDecisionSubject(t *testing.T) {
	dir := t.TempDir()
	var schema strings.Builder
	for _, name := range localTableMigrations {
		if strings.Contains(name, "035_") {
			continue
		}
		data, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		schema.WriteString(renderSQLite(string(data)))
		schema.WriteString("\n")
	}
	if strings.Contains(schema.String(), "orphaned_at") {
		t.Fatal("the earlier schema already names orphaned_at")
	}
	path := dir + string(os.PathSeparator) + localTablesFile
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema.String()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO finops_saving_decisions (org_id, saving_id, state, reason, actor) VALUES ('default', 'old', 'dismissed', 'kept', 'alice')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	for round := 0; round < 2; round++ {
		provider, err := NewFile(FileOptions{DataDir: dir})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		store, _ := FinOps(provider)
		scope := tenancy.DefaultOrgScope()
		if round == 0 {
			mustDo(t, store.PutFinOpsSavingDecision(context.Background(), scope, FinOpsSavingDecision{SavingID: "new", State: "dismissed", Payer: "payer", Account: "acct", Kind: "idle", Title: "Delete it"}))
		}
		decisions, err := store.FinOpsSavingDecisions(context.Background(), scope)
		_ = provider.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(decisions) != 2 || decisions[1].SavingID != "old" || decisions[1].Reason != "kept" || decisions[1].Payer != "" ||
			decisions[0].Payer != "payer" || decisions[0].Title != "Delete it" {
			t.Fatalf("round %d decisions = %+v", round, decisions)
		}
	}
}

// rollupTestLines builds a payer's cost lines for each day: several usage
// types per service (which the rollup sums into one row), two regions,
// teams, usage groups, estimated flags, and resource-level lines the
// rollup leaves out.
func rollupTestLines(payer string, days []string, scale int64) []FinOpsCostLine {
	var out []FinOpsCostLine
	for d, day := range days {
		for a, account := range []string{"acct-a", "acct-b", "acct-c"} {
			for s, service := range []string{"compute", "storage"} {
				for u := 0; u < 3; u++ {
					region := []string{"us-east-1", "eu-west-1"}[u/2]
					out = append(out, FinOpsCostLine{Payer: payer, Day: day, Account: account, Region: region, Service: service,
						ServiceName: "Provider " + service, UsageType: fmt.Sprintf("type-%d", u), UsageGroup: []string{"", "group"}[s],
						RecordType: []string{"Usage", "Credit"}[(a+u)%2], Team: []string{"", "checkout", "data"}[(a+s)%3],
						Currency: "USD", NetAmortizedMicros: scale * int64(1000+d*7+a*31+s*101+u), QuantityMicros: int64(10 + u),
						Unit: "Hrs", EstimatedTeam: a == 2 && u == 1, EstimatedRegion: s == 1 && u == 2})
				}
				out = append(out, FinOpsCostLine{Payer: payer, Day: day, Account: account, Region: "us-east-1", Service: service,
					ServiceName: "Provider " + service, UsageType: "type-0", RecordType: "Usage", ResourceID: "i-" + account,
					Currency: "USD", NetAmortizedMicros: 999_999, QuantityMicros: 1})
			}
		}
	}
	return out
}

// rollupDays lists the days from first through last.
func rollupDays(first, last string) []string {
	var out []string
	day := must(time.Parse(time.DateOnly, first))
	end := must(time.Parse(time.DateOnly, last))
	for ; !day.After(end); day = day.AddDate(0, 0, 1) {
		out = append(out, day.Format(time.DateOnly))
	}
	return out
}

// linesReference sums what FinOpsCostLines returns, so a rollup answer is
// compared with the cost lines on the same backend, not only across them.
func linesReference(t *testing.T, store FinOpsStore, scope tenancy.OrgScope, query FinOpsCostQuery, groupBy []FinOpsDimension) []FinOpsCostTotal {
	t.Helper()
	lines := must(store.FinOpsCostLines(context.Background(), scope, query))
	rows := make([]finOpsDailyRow, len(lines))
	for index, line := range lines {
		rows[index] = finOpsDailyRow{line: line, lines: 1}
	}
	return must(sumFinOpsRows(rows, groupBy))
}

// TestTablesParityFinOpsRollupAnswersLikeLines reads totals without
// resource lines, which the daily and monthly rollups answer, and requires
// on every backend exactly the sums the cost lines give: for every grouping
// the rollups serve, over ranges inside a month, across whole months with
// days around them and without bounds, for one organization and for an
// ordered read of two. It holds after a day is replaced, a payer is deleted
// and old days are pruned, each of which rewrites the rollups.
func TestTablesParityFinOpsRollupAnswersLikeLines(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		ent, both := tenancy.NewOrgScope(orgs.ent), tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		days := rollupDays("2026-01-27", "2026-04-03")
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, ent, "payer-1", days, rollupTestLines("payer-1", days, 1)))
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, ent, "payer-2", days[10:40], rollupTestLines("payer-2", days[10:40], 3)))
		// The default organization holds payer-1 on days the licensed one
		// also holds, which it must not answer for, and days it alone holds.
		defDays := rollupDays("2026-03-25", "2026-04-10")
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, tenancy.NewOrgScope(orgs.def), "payer-1", defDays, rollupTestLines("payer-1", defDays, 5)))
		// Totals read the rollups only once this process reconciled them.
		reconcileRollups(t, store)

		noResource := ""
		groupings := map[string][]FinOpsDimension{
			"none":       nil,
			"day":        {FinOpsByDay},
			"month":      {FinOpsByMonth},
			"payer-day":  {FinOpsByPayer, FinOpsByDay},
			"account":    {FinOpsByAccount},
			"page":       {FinOpsByAccount, FinOpsByRegion, FinOpsByService, FinOpsByServiceName, FinOpsByUsageGroup, FinOpsByRecordType, FinOpsByTeam, FinOpsByPayer},
			"whole-key":  {FinOpsByDay, FinOpsByAccount, FinOpsByRegion, FinOpsByService, FinOpsByServiceName, FinOpsByUsageGroup, FinOpsByRecordType, FinOpsByTeam, FinOpsByPayer},
			"key-mixed":  {FinOpsByPayer, FinOpsByTeam, FinOpsByDay, FinOpsByRecordType, FinOpsByUsageGroup, FinOpsByServiceName, FinOpsByService, FinOpsByRegion, FinOpsByAccount},
			"team-month": {FinOpsByTeam, FinOpsByMonth},
		}
		ranges := map[string][2]string{
			"in-month":     {"2026-02-03", "2026-02-20"},
			"whole-months": {"2026-02-01", "2026-03-31"},
			"with-edges":   {"2026-01-29", "2026-04-02"},
			"one-month":    {"2026-03-01", "2026-03-31"},
			"unbounded":    {"", ""},
		}
		check := func(step string) {
			for scopeName, scope := range ordered(map[string]tenancy.OrgScope{"ent": ent, "both": both}) {
				for rangeName, bounds := range ordered(ranges) {
					for groupName, groupBy := range ordered(groupings) {
						for _, filter := range []FinOpsCostQuery{{}, {Accounts: []string{"acct-b"}, Teams: []string{"", "data"}}, {Payer: "payer-1", Service: "storage", RecordType: "Usage"}} {
							query := filter
							query.From, query.Through, query.ResourceID = bounds[0], bounds[1], &noResource
							got := must(store.FinOpsCostTotals(ctx, scope, query, groupBy...))
							want := linesReference(t, store, scope, query, groupBy)
							if canonicalJSON(t, got) != canonicalJSON(t, want) {
								t.Fatalf("%s %s %s %s %+v: rollup %s, lines %s", step, scopeName, rangeName, groupName, filter, canonicalJSON(t, got), canonicalJSON(t, want))
							}
						}
					}
				}
				query := FinOpsCostQuery{From: "2026-01-29", Through: "2026-04-02", ResourceID: &noResource}
				record(step+" "+scopeName+" page", must(store.FinOpsCostTotals(ctx, scope, query, groupings["page"]...)))
				record(step+" "+scopeName+" month", must(store.FinOpsCostTotals(ctx, scope, query, FinOpsByMonth, FinOpsByPayer)))
			}
		}
		check("stored")
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, ent, "payer-1", []string{"2026-02-14", "2026-03-31"}, rollupTestLines("payer-1", []string{"2026-02-14"}, 9)))
		check("replaced")
		mustDo(t, store.DeleteFinOpsPayer(ctx, ent, "payer-2"))
		check("payer deleted")
		_ = must(store.PruneFinOpsCostLines(ctx, ent, "2026-02-17"))
		check("pruned")
	})
}

// TestTablesParityFinOpsRollupOverflowReadsLines stores a group whose sum
// does not fit: the rollup marks it, so a total touching it is answered by
// the cost lines with the closed overflow error, while a total that does not
// touch it is still answered.
func TestTablesParityFinOpsRollupOverflowReadsLines(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		lines := []FinOpsCostLine{
			{Day: "2026-01-10", Account: "acct-a", Service: "compute", UsageType: "a", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64},
			{Day: "2026-01-10", Account: "acct-a", Service: "compute", UsageType: "b", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64},
			{Day: "2026-01-10", Account: "acct-b", Service: "compute", UsageType: "a", RecordType: "Usage", NetAmortizedMicros: 7},
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer", []string{"2026-01-10"}, lines))
		noResource := ""
		for _, groupBy := range [][]FinOpsDimension{nil, {FinOpsByAccount}, {FinOpsByMonth}} {
			_, err := store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{ResourceID: &noResource, From: "2026-01-01", Through: "2026-01-31"}, groupBy...)
			record(fmt.Sprintf("overflow %v", groupBy), errors.Is(err, ErrAmountOverflow))
		}
		record("untouched group", must(store.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{ResourceID: &noResource, Account: "acct-b"}, FinOpsByAccount)))
	})
}

// rollupSnapshot lists both rollup tables of the backend, in key order.
func rollupSnapshot(t *testing.T, db *sql.DB, q func(string) string, day func(string) string) []string {
	t.Helper()
	var out []string
	for _, statement := range []string{
		`SELECT org_id, payer, ` + day("day") + `, account, region, service, service_name, usage_group, record_type, team, net_amortized_micros, quantity_micros, lines,
 CASE WHEN estimated_team THEN 1 ELSE 0 END, CASE WHEN estimated_region THEN 1 ELSE 0 END, CASE WHEN overflowed THEN 1 ELSE 0 END
 FROM finops_cost_daily ORDER BY org_id, payer, day, account, region, service, service_name, usage_group, record_type, team`,
		`SELECT org_id, payer, ` + day("month") + `, account, region, service, service_name, usage_group, record_type, team, ` + day("min_day") + `, net_amortized_micros, quantity_micros, lines,
 CASE WHEN estimated_team THEN 1 ELSE 0 END, CASE WHEN estimated_region THEN 1 ELSE 0 END, CASE WHEN overflowed THEN 1 ELSE 0 END
 FROM finops_cost_monthly ORDER BY org_id, payer, month, account, region, service, service_name, usage_group, record_type, team`,
	} {
		rows, err := db.Query(q(statement))
		if err != nil {
			t.Fatal(err)
		}
		columns := must(rows.Columns())
		for rows.Next() {
			values := make([]sql.NullString, len(columns))
			targets := make([]any, len(columns))
			for index := range values {
				targets[index] = &values[index]
			}
			if err := rows.Scan(targets...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			parts := make([]string, len(values))
			for index, value := range values {
				parts[index] = value.String
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return out
}

// rollupSeed writes lines that exercise the rollups, an overflowed group
// included, through the store.
func rollupSeed(t *testing.T, store FinOpsStore) {
	t.Helper()
	ctx := context.Background()
	days := rollupDays("2026-01-29", "2026-03-02")
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, tenancy.NewOrgScope("default"), "payer-1", days, rollupTestLines("payer-1", days, 1)))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, tenancy.NewOrgScope("org-b"), "payer-2", days[:5], rollupTestLines("payer-2", days[:5], 2)))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, tenancy.NewOrgScope("default"), "payer-3", []string{"2026-02-10"}, []FinOpsCostLine{
		{Day: "2026-02-10", Account: "acct-a", Service: "compute", UsageType: "a", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64},
		{Day: "2026-02-10", Account: "acct-a", Service: "compute", UsageType: "b", RecordType: "Usage", NetAmortizedMicros: math.MaxInt64, EstimatedTeam: true},
	}))
}

// TestFileTablesReconcileFillsRollupsOnUpgrade opens a local tables
// database whose rollups are missing while it holds cost lines, as one built
// before the rollups existed, or whose first fill never finished. Totals read
// the lines until the reconcile has run, which then fills both rollups exactly
// as the store's own writes would have; a second reconcile rebuilds nothing.
func TestFileTablesReconcileFillsRollupsOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	provider := must(NewFile(FileOptions{DataDir: dir}))
	store, _ := FinOps(provider)
	rollupSeed(t, store)
	db := must(provider.(*fileProvider).tablesDB())
	identity := func(column string) string { return column }
	want := rollupSnapshot(t, db, identity, identity)
	if len(want) == 0 {
		t.Fatal("the store wrote no rollup rows")
	}
	noResource := ""
	query := FinOpsCostQuery{From: "2026-02-01", Through: "2026-02-28", ResourceID: &noResource, Payer: "payer-1"}
	total := linesReference(t, store, tenancy.NewOrgScope("default"), query, []FinOpsDimension{FinOpsByMonth})
	if _, err := db.Exec(`DROP TABLE finops_cost_daily; DROP TABLE finops_cost_monthly`); err != nil {
		t.Fatal(err)
	}
	_ = provider.Close()

	provider = must(NewFile(FileOptions{DataDir: dir}))
	defer provider.Close()
	store, _ = FinOps(provider)
	db = must(provider.(*fileProvider).tablesDB())
	if got := rollupSnapshot(t, db, identity, identity); len(got) != 0 {
		t.Fatalf("the reopened rollups hold %d rows before any reconcile", len(got))
	}
	if got := must(store.FinOpsCostTotals(context.Background(), tenancy.NewOrgScope("default"), query, FinOpsByMonth)); canonicalJSON(t, got) != canonicalJSON(t, total) {
		t.Fatalf("totals before the reconcile %s, the lines say %s", canonicalJSON(t, got), canonicalJSON(t, total))
	}
	repair := reconcileRollups(t, store)
	if repair.RebuiltDays == 0 || repair.RebuiltMonths == 0 {
		t.Fatalf("the reconcile rebuilt nothing: %+v", repair)
	}
	if got := rollupSnapshot(t, db, identity, identity); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reconciled rollups differ from the written ones: got %d rows, want %d", len(got), len(want))
	}
	if again := reconcileRollups(t, store); again.RebuiltDays != 0 || again.RebuiltMonths != 0 {
		t.Fatalf("a second reconcile rebuilt %+v", again)
	}
}

// reconcileRollups runs the store's rollup reconcile and requires it to
// leave totals reading the rollups.
func reconcileRollups(t *testing.T, store FinOpsStore) FinOpsRollupRepair {
	t.Helper()
	rollups, ok := store.(FinOpsRollupStore)
	if !ok {
		t.Fatal("the store keeps no rollups")
	}
	repair := must(rollups.ReconcileFinOpsRollups(context.Background()))
	if !rollups.FinOpsRollupsReconciled() {
		t.Fatal("the reconcile left totals reading the lines")
	}
	return repair
}

// rollupProbe records totals the rollups answer once reconciled, and
// requires each to equal the sum of the lines on the same backend.
func rollupProbe(t *testing.T, store FinOpsStore, scope tenancy.OrgScope, record tablesRecorder, step string) {
	t.Helper()
	noResource := ""
	for name, read := range ordered(map[string]struct {
		query   FinOpsCostQuery
		groupBy []FinOpsDimension
	}{
		"by month":      {FinOpsCostQuery{From: "2025-11-01", Through: "2026-03-31"}, []FinOpsDimension{FinOpsByMonth, FinOpsByPayer}},
		"closed months": {FinOpsCostQuery{From: "2025-11-01", Through: "2026-02-28"}, []FinOpsDimension{FinOpsByPayer, FinOpsByAccount}},
		"by day":        {FinOpsCostQuery{From: "2026-02-01", Through: "2026-02-28"}, []FinOpsDimension{FinOpsByDay, FinOpsByPayer}},
	}) {
		read.query.ResourceID = &noResource
		got := must(store.FinOpsCostTotals(context.Background(), scope, read.query, read.groupBy...))
		if want := linesReference(t, store, scope, read.query, read.groupBy); canonicalJSON(t, got) != canonicalJSON(t, want) {
			t.Fatalf("%s %s: totals %s, the lines say %s", step, name, canonicalJSON(t, got), canonicalJSON(t, want))
		}
		record(step+" "+name, got)
	}
}

// writeLinesOnly changes cost lines the way a version that knew nothing of
// the rollups did: straight into the lines table. Memory has no older
// version and is left alone, so its transcript is the target.
func writeLinesOnly(t *testing.T, backend tablesBackend, org string) {
	t.Helper()
	if backend.raw == nil {
		return
	}
	q := backend.provider.(interface{ q(string) string }).q
	insert := `INSERT INTO finops_cost_lines (org_id, payer, day, account, region, service, service_name, usage_type, record_type, team, resource_id, usage_group, net_amortized_micros, quantity_micros)
 VALUES (?, ?, ?, ?, 'us-east-1', 'compute', 'Provider compute', ?, 'Usage', '', '', '', ?, 1)`
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		// A day of a closed month the rollups never saw.
		{insert, []any{org, "payer-1", "2025-11-20", "acct-a", "late", 7_000_000}},
		// A changed amount on a day the rollups hold.
		{`UPDATE finops_cost_lines SET net_amortized_micros = net_amortized_micros + 1000 WHERE org_id = ? AND payer = ? AND day = ? AND account = ?`, []any{org, "payer-1", "2026-02-10", "acct-b"}},
		// An older prune of a day, and an older removal of a payer that is
		// then added again with different lines.
		{`DELETE FROM finops_cost_lines WHERE org_id = ? AND payer = ? AND day = ?`, []any{org, "payer-1", "2026-03-01"}},
		{`DELETE FROM finops_cost_lines WHERE org_id = ? AND payer = ?`, []any{org, "payer-2"}},
		{insert, []any{org, "payer-2", "2026-02-02", "acct-z", "again", 3_000_000}},
	} {
		if _, err := backend.raw.Exec(q(statement.sql), statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

// memoryLinesOnly makes the same line changes through the in-process store,
// which keeps its rollup with them: the target every backend must reach.
func memoryLinesOnly(t *testing.T, store FinOpsStore, scope tenancy.OrgScope) {
	t.Helper()
	ctx := context.Background()
	lines := must(store.FinOpsCostLines(ctx, scope, FinOpsCostQuery{Payer: "payer-1", From: "2026-02-10", Through: "2026-02-10"}))
	for index := range lines {
		if lines[index].Account == "acct-b" {
			lines[index].NetAmortizedMicros += 1000
		}
	}
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", []string{"2026-02-10"}, lines))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", []string{"2025-11-20"}, []FinOpsCostLine{
		{Day: "2025-11-20", Account: "acct-a", Region: "us-east-1", Service: "compute", ServiceName: "Provider compute", UsageType: "late", RecordType: "Usage", NetAmortizedMicros: 7_000_000, QuantityMicros: 1}}))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", []string{"2026-03-01"}, nil))
	mustDo(t, store.DeleteFinOpsPayer(ctx, scope, "payer-2"))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-2", []string{"2026-02-02"}, []FinOpsCostLine{
		{Day: "2026-02-02", Account: "acct-z", Region: "us-east-1", Service: "compute", ServiceName: "Provider compute", UsageType: "again", RecordType: "Usage", NetAmortizedMicros: 3_000_000, QuantityMicros: 1}}))
}

// seedRollupHistory writes two payers over closed and open months through
// the store, so the rollups agree with the lines.
func seedRollupHistory(t *testing.T, store FinOpsStore, scope tenancy.OrgScope) {
	t.Helper()
	ctx := context.Background()
	days := rollupDays("2025-11-28", "2026-03-03")
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-1", days, rollupTestLines("payer-1", days, 1)))
	mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "payer-2", days[60:70], rollupTestLines("payer-2", days[60:70], 2)))
}

// TestTablesParityFinOpsRollupReconcileHealsEmptyRollups: the rollups are
// empty while cost lines exist (a fill that never finished). Totals are the
// lines' sums before the reconcile and after it, and the reconcile rebuilds
// the rollups so that they answer.
func TestTablesParityFinOpsRollupReconcileHealsEmptyRollups(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		seedRollupHistory(t, store, scope)
		if backend.raw != nil {
			q := backend.provider.(interface{ q(string) string }).q
			for _, table := range []string{"finops_cost_daily", "finops_cost_monthly"} {
				if _, err := backend.raw.Exec(q(`DELETE FROM `+table+` WHERE org_id = ?`), orgs.ent); err != nil {
					t.Fatal(err)
				}
			}
		}
		rollupProbe(t, store, scope, record, "emptied")
		repair := reconcileRollups(t, store)
		if backend.raw != nil && (repair.RebuiltDays == 0 || repair.RebuiltMonths == 0) {
			t.Fatalf("the reconcile rebuilt nothing over empty rollups: %+v", repair)
		}
		rollupProbe(t, store, scope, record, "reconciled")
	})
}

// TestFinOpsRollupReconcileVacuumsAfterABulkRebuild: a reconcile that
// rebuilds about a month of days or more vacuums and analyzes both rollup
// tables on Postgres, so the next reads find no dead rows and current planner
// row counts; a reconcile that rebuilds a day leaves them to autovacuum.
// Neither memory nor SQLite vacuums.
func TestFinOpsRollupReconcileVacuumsAfterABulkRebuild(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			defer failOnPanic(t)
			store := backend.finops(t)
			scope := tenancy.NewOrgScope(newTablesOrgs().ent)
			seedRollupHistory(t, store, scope)
			if backend.raw == nil {
				if repair := reconcileRollups(t, store); repair.Vacuumed {
					t.Fatalf("memory reported a vacuum: %+v", repair)
				}
				return
			}
			q := backend.provider.(interface{ q(string) string }).q
			for _, table := range []string{"finops_cost_daily", "finops_cost_monthly"} {
				if _, err := backend.raw.Exec(q(`DELETE FROM `+table+` WHERE org_id = ?`), scope.Write); err != nil {
					t.Fatal(err)
				}
			}
			before := rollupMaintenance(t, backend)
			bulk := reconcileRollups(t, store)
			if bulk.RebuiltDays < rollupVacuumDays {
				t.Fatalf("the emptied rollups rebuilt %d days, want at least %d", bulk.RebuiltDays, rollupVacuumDays)
			}
			if bulk.Vacuumed != backend.postgres {
				t.Fatalf("bulk rebuild of %d days: vacuumed %v, want %v", bulk.RebuiltDays, bulk.Vacuumed, backend.postgres)
			}
			afterBulk := rollupMaintenance(t, backend)
			if backend.postgres {
				for table, counts := range afterBulk {
					if counts[0] <= before[table][0] || counts[1] <= before[table][1] {
						t.Fatalf("%s vacuum and analyze counts went from %v to %v over a bulk rebuild", table, before[table], counts)
					}
				}
			}

			if _, err := backend.raw.Exec(q(`UPDATE finops_cost_daily SET net_amortized_micros = net_amortized_micros + 5 WHERE org_id = ? AND day = ?`),
				scope.Write, "2026-02-10"); err != nil {
				t.Fatal(err)
			}
			small := reconcileRollups(t, store)
			if small.RebuiltDays == 0 || small.RebuiltDays >= rollupVacuumDays || small.Vacuumed {
				t.Fatalf("a one-day repair: %+v, want a few days rebuilt and no vacuum", small)
			}
			if after := rollupMaintenance(t, backend); backend.postgres {
				for table, counts := range after {
					if counts[0] != afterBulk[table][0] {
						t.Fatalf("%s was vacuumed after a one-day repair: %v -> %v", table, afterBulk[table], counts)
					}
				}
			}
		})
	}
}

// TestPostgresRollupVacuumGivesUpOnALockedTable: while another session
// holds a lock the vacuum needs, a bulk reconcile still returns within the
// vacuum's lock timeout, reports no vacuum, and lets totals read the rollups.
func TestPostgresRollupVacuumGivesUpOnALockedTable(t *testing.T) {
	backend := postgresTablesBackend(t)
	store := backend.finops(t)
	scope := tenancy.NewOrgScope(newTablesOrgs().ent)
	seedRollupHistory(t, store, scope)
	for _, table := range []string{"finops_cost_daily", "finops_cost_monthly"} {
		if _, err := backend.raw.Exec(`DELETE FROM `+table+` WHERE org_id = $1`, scope.Write); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := backend.raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.Exec(`LOCK TABLE finops_cost_daily IN SHARE UPDATE EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	repair, err := store.(FinOpsRollupStore).ReconcileFinOpsRollups(context.Background())
	took := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	if repair.RebuiltDays < rollupVacuumDays || repair.Vacuumed {
		t.Fatalf("bulk rebuild beside a held lock: %+v, want a bulk rebuild and no vacuum", repair)
	}
	if took > 30*time.Second {
		t.Fatalf("the reconcile waited %s on the locked table", took)
	}
	if !store.(FinOpsRollupStore).FinOpsRollupsReconciled() {
		t.Fatal("totals do not read the rollups after the vacuum gave up")
	}
}

// TestPostgresRollupVacuumSkipsTablesTheRoleDoesNotOwn: a role that does not
// own the rollups does not vacuum them and does not report a vacuum.
func TestPostgresRollupVacuumSkipsTablesTheRoleDoesNotOwn(t *testing.T) {
	backend := postgresTablesBackend(t)
	_ = backend.finops(t)
	ctx := context.Background()
	role := "rollup_reader_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := backend.raw.Exec(`CREATE ROLE ` + role + ` NOLOGIN`); err != nil {
		t.Skipf("cannot create a role to test with: %v", err)
	}
	t.Cleanup(func() { _, _ = backend.raw.Exec(`DROP ROLE IF EXISTS ` + role) })
	conn, err := backend.raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, `SET ROLE `+role); err != nil {
		t.Fatal(err)
	}
	if vacuumRollupsOn(ctx, conn) {
		t.Fatal("a role that does not own the rollups reported them vacuumed")
	}
}

// postgresTablesBackend is the Postgres tables backend; the test skips
// without TEST_POSTGRES_DSN.
func postgresTablesBackend(t *testing.T) tablesBackend {
	t.Helper()
	for _, backend := range tablesBackends(t) {
		if backend.postgres {
			return backend
		}
	}
	t.Skip("TEST_POSTGRES_DSN not set")
	return tablesBackend{}
}

// rollupMaintenance reads, on Postgres, each rollup table's manual vacuum
// and analyze counts in the current schema; elsewhere it is empty.
func rollupMaintenance(t *testing.T, backend tablesBackend) map[string][2]int64 {
	t.Helper()
	out := map[string][2]int64{}
	if !backend.postgres {
		return out
	}
	rows, err := backend.raw.Query(`SELECT relname, vacuum_count, analyze_count FROM pg_stat_user_tables
 WHERE schemaname = current_schema() AND relname IN ('finops_cost_daily', 'finops_cost_monthly')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		var vacuums, analyzes int64
		if err := rows.Scan(&table, &vacuums, &analyzes); err != nil {
			t.Fatal(err)
		}
		out[table] = [2]int64{vacuums, analyzes}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("pg_stat_user_tables lists %d rollup tables, want 2", len(out))
	}
	return out
}

// TestTablesParityFinOpsRollupReconcileHealsLinesWrittenAround: an older
// version changed the lines without the rollups: a day in a closed month, a
// changed amount, a pruned day, and a payer removed and added again. Totals
// read the lines until the reconcile, which rebuilds exactly those days and
// months, after which the rollups answer the same sums.
func TestTablesParityFinOpsRollupReconcileHealsLinesWrittenAround(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		seedRollupHistory(t, store, scope)
		if backend.raw == nil {
			memoryLinesOnly(t, store, scope)
		} else {
			writeLinesOnly(t, backend, orgs.ent)
		}
		rollupProbe(t, store, scope, record, "written around")
		repair := reconcileRollups(t, store)
		if backend.raw != nil && repair.RebuiltDays == 0 {
			t.Fatalf("the reconcile found nothing to rebuild: %+v", repair)
		}
		rollupProbe(t, store, scope, record, "reconciled")
	})
}

// TestFinOpsTotalsReadLinesUntilReconciled: on the SQL backends a process
// that has not reconciled its rollups reads totals from the cost lines, so a
// rollup an older version left behind is never served.
func TestFinOpsTotalsReadLinesUntilReconciled(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			defer failOnPanic(t)
			store := backend.finops(t)
			rollups := store.(FinOpsRollupStore)
			if rollups.FinOpsRollupsReconciled() {
				t.Fatal("a fresh store reads totals from rollups it never reconciled")
			}
			scope := tenancy.NewOrgScope(newTablesOrgs().ent)
			seedRollupHistory(t, store, scope)
			q := backend.provider.(interface{ q(string) string }).q
			if _, err := backend.raw.Exec(q(`UPDATE finops_cost_daily SET net_amortized_micros = net_amortized_micros + 5 WHERE org_id = ?`), scope.Write); err != nil {
				t.Fatal(err)
			}
			noResource := ""
			query := FinOpsCostQuery{From: "2026-02-01", Through: "2026-02-28", ResourceID: &noResource}
			want := linesReference(t, store, scope, query, []FinOpsDimension{FinOpsByDay})
			if got := must(store.FinOpsCostTotals(context.Background(), scope, query, FinOpsByDay)); canonicalJSON(t, got) != canonicalJSON(t, want) {
				t.Fatalf("before the reconcile totals %s, the lines say %s", canonicalJSON(t, got), canonicalJSON(t, want))
			}
		})
	}
}

// TestTablesParityFinOpsRollupReconcileFindsSameTotalRewrites: an older
// version rewrites a day so that its line count and total stay the same, by
// moving a usage type to another team and flipping a region estimate. The
// reconcile compares whole rollup rows, so it rebuilds the day, and totals
// by team and the estimate flag are the lines' own.
func TestTablesParityFinOpsRollupReconcileFindsSameTotalRewrites(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		ctx := context.Background()
		store := backend.finops(t)
		scope := tenancy.NewOrgScope(orgs.ent)
		day := "2026-09-10"
		lines := []FinOpsCostLine{
			{Day: day, Account: "111", Service: "ec2", UsageType: "a", RecordType: "Usage", Team: "alpha", NetAmortizedMicros: 5_000_000},
			{Day: day, Account: "111", Service: "ec2", UsageType: "b", RecordType: "Usage", Team: "beta", NetAmortizedMicros: 3_000_000},
		}
		mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "999", []string{day}, lines))
		reconcileRollups(t, store)
		if backend.raw == nil {
			lines[1].Team, lines[1].EstimatedRegion = "alpha", true
			mustDo(t, store.ReplaceFinOpsCostDays(ctx, scope, "999", []string{day}, lines))
		} else {
			q := backend.provider.(interface{ q(string) string }).q
			if _, err := backend.raw.Exec(q(`UPDATE finops_cost_lines SET team = 'alpha', estimated_region = TRUE WHERE org_id = ? AND usage_type = 'b'`), orgs.ent); err != nil {
				t.Fatal(err)
			}
		}
		repair := reconcileRollups(t, store)
		if backend.raw != nil && repair.RebuiltDays != 1 {
			t.Fatalf("the reconcile did not find the rewritten day: %+v", repair)
		}
		noResource := ""
		query := FinOpsCostQuery{From: day, Through: day, ResourceID: &noResource}
		got := must(store.FinOpsCostTotals(ctx, scope, query, FinOpsByTeam))
		if want := linesReference(t, store, scope, query, []FinOpsDimension{FinOpsByTeam}); canonicalJSON(t, got) != canonicalJSON(t, want) {
			t.Fatalf("totals by team %s, the lines say %s", canonicalJSON(t, got), canonicalJSON(t, want))
		}
		record("by team", got)
	})
}
