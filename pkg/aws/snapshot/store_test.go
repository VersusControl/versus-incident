package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	account = "111122223333"
	region  = "us-east-1"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(step time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(step)
	c.mu.Unlock()
}

// flakyProvider fails unit commits while failing is set. It hides the
// Postgres pool, so every backend keeps the bounded history these tests
// check.
type flakyProvider struct {
	inventoryProvider
	mu      sync.Mutex
	failing bool
}

var errWrite = errors.New("write failed")

func (provider *flakyProvider) CommitAWSUnit(ctx context.Context, scope tenancy.OrgScope, unit storage.AWSUnit, candidate storage.AWSCandidate, guard *uint64) (storage.AWSCommit, error) {
	provider.mu.Lock()
	failing := provider.failing
	provider.mu.Unlock()
	if failing {
		return storage.AWSCommit{}, errWrite
	}
	return provider.inventoryProvider.CommitAWSUnit(ctx, scope, unit, candidate, guard)
}

func (provider *flakyProvider) fail(failing bool) {
	provider.mu.Lock()
	provider.failing = failing
	provider.mu.Unlock()
}

func newStore(t *testing.T, options ...Option) (*Store, *flakyProvider, *clock) {
	t.Helper()
	provider := &flakyProvider{inventoryProvider: openBackend(t)}
	c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	store := New(provider, tenancy.DefaultOrgScope(), append([]Option{WithClock(c.Now)}, options...)...)
	return store, provider, c
}

func unit(service string) model.Unit {
	return model.Unit{Account: account, Region: region, Service: service}
}

func instance(id string) model.Resource {
	return model.Resource{
		ARN: model.ARN("aws", "ec2", region, account, "instance/"+id), Service: "ec2", Type: "instance",
		Account: account, Region: region, Name: id, State: "running",
		Attributes: map[string]string{model.AttrID: id, "user_data": "must-not-survive"},
	}
}

func candidate(ids ...string) Candidate {
	out := Candidate{}
	for _, id := range ids {
		out.Resources = append(out.Resources, instance(id))
	}
	return out
}

func names(t *testing.T, store *Store, asOf time.Time) []string {
	t.Helper()
	result, err := store.Resources(context.Background(), Query{Account: account, Service: "ec2", AsOf: asOf})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(result.Resources))
	for _, resource := range result.Resources {
		out = append(out, resource.Name)
	}
	return out
}

func TestCommitSanitizesAndServes(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, _ := newStore(t)
		generation, err := store.Commit(unit("ec2"), candidate("i-2", "i-1", "i-1"))
		if err != nil {
			t.Fatal(err)
		}
		if generation.Seq != 1 || len(generation.Resources) != 2 || generation.Resources[0].Name != "i-1" {
			t.Fatalf("generation = %+v", generation)
		}
		if _, kept := generation.Resources[0].Attributes["user_data"]; kept {
			t.Fatal("attribute outside the closed set was stored")
		}
		if got := names(t, store, time.Time{}); strings.Join(got, ",") != "i-1,i-2" {
			t.Fatalf("resources = %v", got)
		}
	})
}

// TestFailedIndexWriteKeepsPreviousSnapshot is the atomic swap: a commit
// that fails leaves readers on the previous generation, in this store and in
// a fresh one over the same data.
func TestFailedIndexWriteKeepsPreviousSnapshot(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, provider, c := newStore(t)
		if _, err := store.Commit(unit("ec2"), candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Minute)
		provider.fail(true)
		if _, err := store.Commit(unit("ec2"), candidate("i-1", "i-2")); !errors.Is(err, errWrite) {
			t.Fatalf("commit with failing swap = %v", err)
		}
		if got := names(t, store, time.Time{}); strings.Join(got, ",") != "i-1" {
			t.Fatalf("after failed swap = %v", got)
		}
		provider.fail(false)
		reloaded := New(provider, tenancy.DefaultOrgScope())
		if got := names(t, reloaded, time.Time{}); strings.Join(got, ",") != "i-1" {
			t.Fatalf("reloaded after failed swap = %v", got)
		}
		statuses, _ := reloaded.Sync(context.Background(), Query{Account: account, Service: "ec2"})
		if len(statuses) != 1 || statuses[0].Count != 1 || statuses[0].Sync.State != model.SyncOK {
			t.Fatalf("status after failed swap = %+v", statuses)
		}
	})
}

func TestFailedSyncKeepsLastGood(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		if _, err := store.Commit(unit("ec2"), candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		committedAt := c.Now()
		c.Advance(time.Minute)
		if err := store.RecordFailure(unit("ec2"), "throttled"); err != nil {
			t.Fatal(err)
		}
		result, err := store.Resources(context.Background(), Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Resources) != 2 {
			t.Fatalf("failed sync blanked the snapshot: %+v", result.Resources)
		}
		if len(result.Gaps) != 1 || result.Gaps[0].Reason != model.GapThrottled || !result.Gaps[0].LastGoodAt.Equal(committedAt) {
			t.Fatalf("gaps = %+v", result.Gaps)
		}
		status := result.Units[0]
		if status.Sync.State != model.SyncFailed || status.Sync.ErrorCode != "throttled" || !status.HasData || status.Count != 2 {
			t.Fatalf("status = %+v", status)
		}

		// Named detail gaps replace the whole-unit gap and keep the time.
		if err := store.RecordFailure(unit("ec2"), "timeout", model.Gap{Reason: model.GapNotChecked, Operation: "DescribeInstanceStatus"}); err != nil {
			t.Fatal(err)
		}
		result, _ = store.Resources(context.Background(), Query{Account: account})
		if len(result.Gaps) != 1 || result.Gaps[0].Operation != "DescribeInstanceStatus" || result.Gaps[0].Service != "ec2" || !result.Gaps[0].LastGoodAt.Equal(committedAt) {
			t.Fatalf("detail gaps = %+v", result.Gaps)
		}

		// A good commit clears the failure.
		c.Advance(time.Minute)
		if _, err := store.Commit(unit("ec2"), candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		result, _ = store.Resources(context.Background(), Query{Account: account})
		if len(result.Gaps) != 0 || result.Units[0].Sync.State != model.SyncOK {
			t.Fatalf("after recovery = %+v", result)
		}
	})
}

func TestInvalidCandidateIsRejectedWithoutChange(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, _ := newStore(t)
		if _, err := store.Commit(unit("ec2"), candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		other := instance("i-9")
		other.Account = "999988887777"
		if _, err := store.Commit(unit("ec2"), Candidate{Resources: []model.Resource{other}}); !errors.Is(err, ErrInvalidCandidate) {
			t.Fatalf("foreign resource = %v", err)
		}
		if _, err := store.Commit(model.Unit{Account: "bad", Region: region, Service: "ec2"}, candidate("i-1")); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("bad unit = %v", err)
		}
		if _, err := store.Commit(unit(ChangeService), candidate("i-1")); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("change unit commit = %v", err)
		}
		if got := names(t, store, time.Time{}); strings.Join(got, ",") != "i-1" {
			t.Fatalf("after rejection = %v", got)
		}
	})
}

func TestIdenticalCandidateOnlyReverifies(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		first, _ := store.Commit(unit("ec2"), candidate("i-1"))
		c.Advance(time.Minute)
		second, err := store.Commit(unit("ec2"), candidate("i-1"))
		if err != nil || second.Seq != first.Seq {
			t.Fatalf("identical commit made generation %d (was %d): %v", second.Seq, first.Seq, err)
		}
		statuses, _ := store.Sync(context.Background(), Query{Account: account})
		if !statuses[0].VerifiedAt.Equal(c.Now()) || !statuses[0].CommittedAt.Equal(c.Now().Add(-time.Minute)) {
			t.Fatalf("status = %+v", statuses[0])
		}
	})
}

func TestAsOfReturnsTheGenerationCurrentThen(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		start := c.Now()
		store.Commit(unit("ec2"), candidate("i-1"))
		c.Advance(10 * time.Minute)
		store.Commit(unit("ec2"), candidate("i-1", "i-2"))
		c.Advance(10 * time.Minute)
		store.Commit(unit("ec2"), candidate("i-3"))

		cases := map[time.Duration]string{
			5 * time.Minute:  "i-1",
			10 * time.Minute: "i-1,i-2",
			15 * time.Minute: "i-1,i-2",
			25 * time.Minute: "i-3",
		}
		for offset, want := range cases {
			if got := strings.Join(names(t, store, start.Add(offset)), ","); got != want {
				t.Errorf("as of +%s = %q, want %q", offset, got, want)
			}
		}
		before, err := store.Resources(context.Background(), Query{Account: account, AsOf: start.Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if len(before.Resources) != 0 || len(before.Gaps) != 1 || before.Gaps[0].Reason != model.GapNotChecked {
			t.Fatalf("before the first commit = %+v", before)
		}
		// History is read from storage by a fresh store too.
		reloaded := New(store.provider, tenancy.DefaultOrgScope())
		if got := strings.Join(names(t, reloaded, start.Add(15*time.Minute)), ","); got != "i-1,i-2" {
			t.Fatalf("reloaded as of = %q", got)
		}
	})
}

func TestHistoricalReadKeepsCommittedDetailGaps(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		gapped := candidate("i-1")
		gapped.DetailGaps = []model.Gap{{Account: account, Region: region, Service: "ec2", Reason: model.GapDenied, Operation: "DescribeInstanceStatus"}}
		store.Commit(unit("ec2"), gapped)
		at := c.Now()
		c.Advance(time.Minute)
		store.Commit(unit("ec2"), candidate("i-1"))
		result, _ := store.Resources(context.Background(), Query{Account: account, AsOf: at})
		if len(result.Gaps) != 1 || result.Gaps[0].Operation != "DescribeInstanceStatus" {
			t.Fatalf("historical gaps = %+v", result.Gaps)
		}
	})
}

// heldAt reports which of the commit times still answer with data.
func heldAt(t *testing.T, store *Store, times []time.Time) []bool {
	t.Helper()
	held := make([]bool, len(times))
	for index, at := range times {
		statuses, err := store.Sync(context.Background(), Query{Account: account, Service: "ec2", AsOf: at})
		if err != nil {
			t.Fatal(err)
		}
		held[index] = len(statuses) == 1 && statuses[0].HasData
	}
	return held
}

func TestHistoryIsBoundedByCountAndAge(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t, WithOptions(func() Options { return Options{MaxGenerations: 3, MaxAge: time.Hour} }))
		var times []time.Time
		for index := 0; index < 5; index++ {
			store.Commit(unit("ec2"), candidate("i-"+string(rune('a'+index))))
			times = append(times, c.Now())
			c.Advance(time.Minute)
		}
		if got := fmt.Sprint(heldAt(t, store, times)); got != "[false false true true true]" {
			t.Fatalf("generations kept = %s, want the newest 3", got)
		}
		statuses, _ := store.Sync(context.Background(), Query{Account: account})
		if statuses[0].Count != 1 {
			t.Fatalf("status = %+v", statuses[0])
		}

		// Age: two hours later, a new commit prunes everything superseded
		// before the cutoff but keeps the generation that was current then.
		c.Advance(2 * time.Hour)
		store.Commit(unit("ec2"), candidate("i-z"))
		times = append(times, c.Now())
		if got := fmt.Sprint(heldAt(t, store, times)); got != "[false false false false true true]" {
			t.Fatalf("generations after age pruning = %s, want the last two", got)
		}
	})
}

func TestUnboundedStoreKeepsEveryGeneration(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t, WithUnbounded(), WithOptions(func() Options { return Options{MaxGenerations: 2} }))
		var times []time.Time
		for index := 0; index < 5; index++ {
			store.Commit(unit("ec2"), candidate("i-"+string(rune('a'+index))))
			times = append(times, c.Now())
			c.Advance(time.Minute)
		}
		if got := fmt.Sprint(heldAt(t, store, times)); got != "[true true true true true]" {
			t.Fatalf("unbounded store kept %s", got)
		}
	})
}

func TestPendingUnitsAreNotCheckedGaps(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, _ := newStore(t)
		if err := store.RecordPending([]model.Unit{unit("ec2"), unit("lambda")}); err != nil {
			t.Fatal(err)
		}
		store.Commit(unit("ec2"), candidate("i-1"))
		if err := store.RecordPending([]model.Unit{unit("ec2"), unit("lambda")}); err != nil {
			t.Fatal(err)
		}
		result, _ := store.Resources(context.Background(), Query{Account: account})
		if len(result.Gaps) != 1 || result.Gaps[0].Service != "lambda" || result.Gaps[0].Reason != model.GapNotChecked {
			t.Fatalf("gaps = %+v", result.Gaps)
		}
		if !store.HasGeneration(unit("ec2")) || store.HasGeneration(unit("lambda")) {
			t.Fatal("HasGeneration wrong")
		}
		if err := store.RecordPending([]model.Unit{{Account: "x", Region: region, Service: "ec2"}}); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("invalid pending unit = %v", err)
		}
	})
}

// TestScopeWritesOneOrgAndReadsBoth checks an enterprise-shaped scope writes
// only its own organization and still reads the default organization's data.
func TestScopeWritesOneOrgAndReadsBoth(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		provider := openBackend(t)
		shared := New(provider, tenancy.DefaultOrgScope())
		shared.Commit(unit("ec2"), candidate("i-shared"))
		licensed := New(provider, tenancy.OrgScope{Write: "acme", Read: []string{"acme", "default"}})
		lambda := model.Resource{ARN: model.ARN("aws", "lambda", region, account, "function:f"), Service: "lambda", Type: "function", Account: account, Region: region, Name: "f"}
		if _, err := licensed.Commit(unit("lambda"), Candidate{Resources: []model.Resource{lambda}}); err != nil {
			t.Fatal(err)
		}
		own, _ := New(provider, tenancy.NewOrgScope("acme")).Resources(context.Background(), Query{Account: account})
		if len(own.Resources) != 1 || own.Resources[0].Name != "f" {
			t.Fatalf("licensed write did not land under its organization: %+v", own.Resources)
		}
		result, _ := licensed.Resources(context.Background(), Query{Account: account})
		if len(result.Resources) != 2 {
			t.Fatalf("licensed read = %+v", result.Resources)
		}
		defaultView, _ := shared.Resources(context.Background(), Query{Account: account})
		if len(defaultView.Resources) != 1 {
			t.Fatalf("default org saw the licensed org's data: %+v", defaultView.Resources)
		}
	})
}

// blobOnly is a provider without inventory tables.
type blobOnly struct{ storage.Provider }

func TestNoStorage(t *testing.T) {
	var store *Store
	if _, err := store.Commit(unit("ec2"), Candidate{}); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("nil store = %v", err)
	}
	notReady := New(blobOnly{storage.NewMemory()}, tenancy.DefaultOrgScope())
	if _, err := notReady.Commit(unit("ec2"), candidate("i-1")); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("store without tables commit = %v", err)
	}
	if _, err := notReady.Resources(context.Background(), Query{}); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("store without tables read = %v", err)
	}
	if ValidUnit(model.Unit{Account: account, Region: "global", Service: "s3"}) != true || ValidUnit(model.Unit{Account: account, Region: region, Service: "../x"}) {
		t.Fatal("unit validation wrong")
	}
}

// TestAsOfNeverReportsAVerificationAfterAsOf is the QA-370 repro: a
// generation re-verified an hour later must not show that later time in a
// read as of five minutes after its commit.
func TestAsOfNeverReportsAVerificationAfterAsOf(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		start := c.Now()
		store.Commit(unit("ec2"), candidate("i-1"))
		c.Advance(time.Hour)
		store.Commit(unit("ec2"), candidate("i-1"))
		asOf := start.Add(5 * time.Minute)
		result, err := store.Resources(context.Background(), Query{Account: account, AsOf: asOf})
		if err != nil {
			t.Fatal(err)
		}
		status := result.Units[0]
		if status.VerifiedAt.After(asOf) || status.Sync.LastGoodAt.After(asOf) {
			t.Fatalf("as of %s reports verified %s, last good %s", asOf, status.VerifiedAt, status.Sync.LastGoodAt)
		}
		if !status.VerifiedAt.Equal(start) || !status.Sync.LastGoodAt.Equal(start) || !status.CommittedAt.Equal(start) {
			t.Fatalf("historical status = %+v", status)
		}
		// After the re-verification, an as-of read sees it.
		later, _ := store.Resources(context.Background(), Query{Account: account, AsOf: c.Now()})
		if !later.Units[0].VerifiedAt.Equal(c.Now()) {
			t.Fatalf("as of the re-verification = %+v", later.Units[0])
		}
	})
}
