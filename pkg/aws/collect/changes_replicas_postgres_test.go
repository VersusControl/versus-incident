package collect

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// These tests run two collectors, each on its own Postgres provider (its own
// connection pool, as two replicas have), against one database. They prove
// the CloudTrail pair lock is a real cross-session lock: the replica without
// it neither calls LookupEvents nor writes the pair's state, and a session
// the server ends no longer counts as holding it. They skip unless
// TEST_POSTGRES_DSN names an isolated database (harness.sh pgtest).

// replicaFeedTables are the AWS tables a run empties first, so it starts
// from no data even when the database was used before.
const replicaFeedTables = `TRUNCATE aws_account_epochs, aws_sync_units, aws_unit_generations, aws_resources,
 aws_health, aws_security_flags, aws_changes, aws_change_coverage, aws_change_counts, aws_change_gaps, aws_change_pairs`

func postgresReplicaProvider(t *testing.T) storage.Provider {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

// inFlight tracks LookupEvents requests in flight across every replica.
// The global stream and the us-east-1 pair both read CloudTrail in
// us-east-1, so they share one AWS rate bucket; most is the highest number
// in flight at once.
type inFlight struct {
	mu      sync.Mutex
	current int
	most    int
}

func (f *inFlight) enter() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current++
	f.most = max(f.most, f.current)
}

func (f *inFlight) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current--
}

func (f *inFlight) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.most
}

// lookupCounter counts the LookupEvents requests one replica sends and can
// slow them, so two replicas' polls overlap in time.
type lookupCounter struct {
	next    http.RoundTripper
	delay   time.Duration
	shared  *inFlight
	lookups atomic.Int64
}

func (counter *lookupCounter) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.Header.Get("X-Amz-Target"), ".LookupEvents") {
		counter.lookups.Add(1)
		if counter.shared != nil {
			counter.shared.enter()
			defer counter.shared.leave()
		}
		if counter.delay > 0 {
			time.Sleep(counter.delay)
		}
	}
	return counter.next.RoundTrip(request)
}

type pgReplica struct {
	provider  storage.Provider
	store     *snapshot.Store
	collector *Collector
	counter   *lookupCounter
}

func newPGReplica(t *testing.T, fake *fakeaws.Server, delay time.Duration, shared *inFlight) *pgReplica {
	t.Helper()
	provider := postgresReplicaProvider(t)
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	counter := &lookupCounter{next: fake.HTTPClient().Transport, delay: delay, shared: shared}
	collector := New(store, limit.NewManager(1000), WithHTTPClient(&http.Client{Transport: counter}), WithSleep(noSleep), WithRetries(0))
	return &pgReplica{provider: provider, store: store, collector: collector, counter: counter}
}

func emptyFeedTables(t *testing.T, provider storage.Provider) {
	t.Helper()
	accessor, ok := provider.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider has no SQL accessor")
	}
	if _, err := accessor.DB().Exec(replicaFeedTables); err != nil {
		t.Fatal(err)
	}
}

func outcomeOf(t *testing.T, report Report, region string) Outcome {
	t.Helper()
	for _, unit := range report.Units {
		if unit.Unit.Region == region && unit.Unit.Service == snapshot.ChangeService {
			return unit.Outcome
		}
	}
	t.Fatalf("no change unit for %s in %+v", region, report.Units)
	return ""
}

// One replica holds the us-east-1 pair: the other skips that pair and the
// account's global stream (which reads us-east-1 CloudTrail too, so it
// shares the lock), sends no LookupEvents and writes nothing for either.
// Once released, the other replica polls both. When the database ends the
// holder's session, the holder no longer counts as holding the lock and the
// other replica can take it.
func TestPostgresPairLockSkipsTheOtherReplica(t *testing.T) {
	fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
	holder := newPGReplica(t, fake, 0, nil)
	other := newPGReplica(t, fake, 0, nil)
	emptyFeedTables(t, holder.provider)
	ctx := context.Background()

	unlock, acquired, err := holder.store.LockChangePair(ctx, productionAccount, testRegion)
	if err != nil || !acquired {
		t.Fatalf("holder lock = %v, %v", acquired, err)
	}
	if again, got, err := other.store.LockChangePair(ctx, productionAccount, testRegion); err != nil || got {
		if again != nil {
			again()
		}
		t.Fatalf("second replica took a held pair lock: acquired=%v err=%v", got, err)
	}
	if !holder.store.ChangePairLockHeld(ctx, productionAccount, testRegion) {
		t.Fatal("holder does not see its own lock")
	}
	if other.store.ChangePairLockHeld(ctx, productionAccount, testRegion) {
		t.Fatal("non-holder reports the lock as held")
	}

	report, err := other.collector.Sync(ctx, target(productionAccount), KindChanges)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomeOf(t, report, testRegion); got != OutcomeSkipped {
		t.Fatalf("held us-east-1 pair outcome = %s, want skipped", got)
	}
	if got := outcomeOf(t, report, model.GlobalRegion); got != OutcomeSkipped {
		t.Fatalf("global stream outcome = %s while us-east-1 is held, want skipped (same CloudTrail rate)", got)
	}
	checkpoint, err := other.store.Checkpoint(productionAccount, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpoint.LastAttemptAt.IsZero() || len(checkpoint.Covered) != 0 {
		t.Fatalf("replica without the lock wrote the pair's state: %+v", checkpoint)
	}
	globalLookups := other.counter.lookups.Load()
	if globalLookups != 0 {
		t.Fatalf("replica without the lock sent %d LookupEvents", globalLookups)
	}

	unlock()
	report, err = other.collector.Sync(ctx, target(productionAccount), KindChanges)
	if err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{testRegion, model.GlobalRegion} {
		if got := outcomeOf(t, report, region); got != OutcomeCommitted {
			t.Fatalf("released %s pair outcome = %s, want committed", region, got)
		}
	}
	if other.counter.lookups.Load() <= globalLookups {
		t.Fatal("the released pair sent no LookupEvents")
	}
	checkpoint, err = holder.store.Checkpoint(productionAccount, testRegion)
	if err != nil || len(checkpoint.Covered) == 0 || checkpoint.LastAttemptAt.IsZero() {
		t.Fatalf("pair state after the poll, read by the other replica = %+v, %v", checkpoint, err)
	}

	// The server ends the holder's session: the lock is gone with it.
	unlock, acquired, err = holder.store.LockChangePair(ctx, productionAccount, testRegion)
	if err != nil || !acquired {
		t.Fatalf("holder relock = %v, %v", acquired, err)
	}
	defer unlock()
	admin, err := sql.Open("pgx", os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	result, err := admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		t.Fatal("no advisory lock session to end")
	}
	deadline := time.Now().Add(5 * time.Second)
	for holder.store.ChangePairLockHeld(ctx, productionAccount, testRegion) {
		if time.Now().After(deadline) {
			t.Fatal("holder still reports the lock after its session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	taken, acquired, err := other.store.LockChangePair(ctx, productionAccount, testRegion)
	if err != nil || !acquired {
		t.Fatalf("lock after the holder's session ended = %v, %v", acquired, err)
	}
	taken()
}

// Two replicas start their change polls at the same moment, round after
// round, with slow LookupEvents answers so the polls overlap, as two
// replicas booted by one rollout do. One poller per pair means: each pair
// is polled by one replica per round and skipped by the other, the two
// replicas never have LookupEvents in flight against the same account and
// Region at once (the global stream reads us-east-1 too), and every pair's
// stored sync state stays ok.
func TestPostgresReplicasStartingTogetherPollEachPairOnce(t *testing.T) {
	fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
	shared := &inFlight{}
	first := newPGReplica(t, fake, 150*time.Millisecond, shared)
	second := newPGReplica(t, fake, 150*time.Millisecond, shared)
	emptyFeedTables(t, first.provider)
	ctx := context.Background()
	const rounds = 6
	for round := range rounds {
		var wg sync.WaitGroup
		reports := make([]Report, 2)
		errs := make([]error, 2)
		start := make(chan struct{})
		for index, replica := range []*pgReplica{first, second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				reports[index], errs[index] = replica.collector.Sync(ctx, target(productionAccount), KindChanges)
			}()
		}
		close(start)
		wg.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("round %d replica %d: %v", round, index, err)
			}
		}
		for _, region := range []string{testRegion, model.GlobalRegion} {
			committed, skipped := 0, 0
			for _, report := range reports {
				switch outcomeOf(t, report, region) {
				case OutcomeCommitted:
					committed++
				case OutcomeSkipped:
					skipped++
				}
			}
			if committed != 1 || skipped != 1 {
				t.Errorf("round %d pair %s: committed=%d skipped=%d, want exactly one poller", round, region, committed, skipped)
			}
		}
		statuses, err := first.store.Sync(ctx, snapshot.Query{Account: productionAccount})
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range statuses {
			if status.Unit.Service == snapshot.ChangeService && status.Sync.State != model.SyncOK {
				t.Errorf("round %d: change unit %s sync state %s (%s)", round, status.Unit.Key(), status.Sync.State, status.Sync.ErrorCode)
			}
		}
	}
	t.Logf("LookupEvents over %d concurrent rounds: first=%d second=%d; most in flight at once against us-east-1: %d",
		rounds, first.counter.lookups.Load(), second.counter.lookups.Load(), shared.peak())
	if shared.peak() > 1 {
		t.Errorf("two replicas had %d LookupEvents in flight at once against account %s us-east-1 (the us-east-1 pair and the global stream share its CloudTrail rate)",
			shared.peak(), productionAccount)
	}

	// Polls that do not overlap in time are not excluded by the lock: each
	// replica polls on its own schedule. Record what that costs.
	before := first.counter.lookups.Load() + second.counter.lookups.Load()
	if _, err := first.collector.Sync(ctx, target(productionAccount), KindChanges); err != nil {
		t.Fatal(err)
	}
	one := first.counter.lookups.Load() + second.counter.lookups.Load() - before
	if _, err := second.collector.Sync(ctx, target(productionAccount), KindChanges); err != nil {
		t.Fatal(err)
	}
	both := first.counter.lookups.Load() + second.counter.lookups.Load() - before
	t.Logf("back-to-back polls: one replica %d LookupEvents, then the other adds %d (the lock serialises, it does not deduplicate)", one, both-one)
}

// Two replicas each poll on their own schedule, one interval apart, offset
// from each other by part of an interval, on a shared clock. With the
// interval set, a pair any replica polled within half an interval is left
// alone, so across several intervals each pair is read about once per
// interval, not once per replica per interval.
func TestPostgresReplicasPollEachPairOncePerInterval(t *testing.T) {
	const interval = 5 * time.Minute
	const intervals = 8
	for _, offset := range []float64{0.1, 0.3, 0.6, 0.9} {
		t.Run(fmt.Sprintf("offset_%.1f", offset), func(t *testing.T) {
			fake := fakeaws.New(t, fakeaws.ScenarioBadDeploy)
			var mu sync.Mutex
			now := time.Now().UTC().Truncate(time.Minute)
			clock := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			fake.SetClock(clock)
			replicas := make([]*pgReplica, 2)
			for index := range replicas {
				provider := postgresReplicaProvider(t)
				if index == 0 {
					emptyFeedTables(t, provider)
				}
				// One clock for the store and the collector, as in a server.
				store := snapshot.New(provider, tenancy.DefaultOrgScope(), snapshot.WithClock(clock))
				counter := &lookupCounter{next: fake.HTTPClient().Transport}
				collector := New(store, limit.NewManager(1000), WithHTTPClient(&http.Client{Transport: counter}), WithSleep(noSleep), WithRetries(0),
					WithClock(clock), WithOptions(func() Options { return Options{ChangesInterval: interval} }))
				replicas[index] = &pgReplica{provider: provider, store: store, collector: collector, counter: counter}
			}
			start := clock()
			type event struct {
				at      time.Duration
				replica int
			}
			var schedule []event
			for step := range intervals {
				schedule = append(schedule, event{time.Duration(step) * interval, 0},
					event{time.Duration(float64(interval) * (float64(step) + offset)), 1})
			}
			committed := map[string]int{}
			for _, item := range schedule {
				mu.Lock()
				now = start.Add(item.at)
				mu.Unlock()
				report, err := replicas[item.replica].collector.Sync(context.Background(), target(productionAccount), KindChanges)
				if err != nil {
					t.Fatal(err)
				}
				for _, region := range []string{testRegion, model.GlobalRegion} {
					if outcomeOf(t, report, region) == OutcomeCommitted {
						committed[region]++
					}
				}
			}
			lookups := replicas[0].counter.lookups.Load() + replicas[1].counter.lookups.Load()
			t.Logf("offset %.1f: polls per pair over %d intervals = %v; LookupEvents a=%d b=%d", offset, intervals, committed,
				replicas[0].counter.lookups.Load(), replicas[1].counter.lookups.Load())
			for _, region := range []string{testRegion, model.GlobalRegion} {
				if got := committed[region]; got < intervals || got > intervals+1 {
					t.Errorf("pair %s polled %d times over %d intervals by two replicas, want %d or %d", region, got, intervals, intervals, intervals+1)
				}
			}
			if lookups == 0 {
				t.Error("no LookupEvents were sent; the schedule proves nothing")
			}
		})
	}
}
