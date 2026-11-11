package snapshot

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func (c *clock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// interleavingProvider writes after a units read has returned and before the
// caller reads any rows, which is the window a separate units read opened.
type interleavingProvider struct {
	inventoryProvider
	once       sync.Once
	afterUnits func()
}

func (p *interleavingProvider) AWSUnits(ctx context.Context, scope tenancy.OrgScope, query storage.AWSUnitQuery) ([]storage.AWSUnitStatus, error) {
	units, err := p.inventoryProvider.AWSUnits(ctx, scope, query)
	if err == nil && p.afterUnits != nil {
		p.once.Do(p.afterUnits)
	}
	return units, err
}

// TestSnapshotReadDoesNotMixGenerations: a write that lands between the
// unit read and the rows read must not produce a unit whose committed count
// disagrees with the rows served for it.
func TestSnapshotReadDoesNotMixGenerations(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		provider := &interleavingProvider{inventoryProvider: openBackend(t)}
		c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
		store := New(provider, tenancy.DefaultOrgScope(), WithClock(c.Now))
		ctx := context.Background()
		if _, err := store.Commit(unit("ec2"), candidate("i-1", "i-2")); err != nil {
			t.Fatal(err)
		}
		provider.afterUnits = func() {
			// Replace the generation with an empty one right after the
			// statuses were read; a mixed view pairs count 2 with 0 rows.
			if _, err := store.Commit(unit("ec2"), Candidate{}); err != nil {
				t.Error(err)
			}
		}
		result, err := store.Resources(ctx, Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range result.Units {
			if status.Unit.Service != "ec2" || !status.HasData {
				continue
			}
			if status.Count != len(result.Resources) {
				t.Fatalf("mixed generation: unit count %d, resources %d", status.Count, len(result.Resources))
			}
		}
	})
}

// gapsCorruptingProvider returns a stored coverage document the writer never
// produces, on both the separate read path and the one-snapshot path.
type gapsCorruptingProvider struct {
	inventoryProvider
}

func corruptGaps(units []storage.AWSUnitStatus) {
	for index := range units {
		if len(units[index].Sync.Gaps) > 0 {
			units[index].Sync.Gaps = json.RawMessage(`{"gaps":"object not list"}`)
		}
		if units[index].Generation != nil {
			units[index].Generation.Gaps = json.RawMessage(`[{"account":`)
		}
	}
}

func (p *gapsCorruptingProvider) AWSUnits(ctx context.Context, scope tenancy.OrgScope, query storage.AWSUnitQuery) ([]storage.AWSUnitStatus, error) {
	units, err := p.inventoryProvider.AWSUnits(ctx, scope, query)
	corruptGaps(units)
	return units, err
}

func (p *gapsCorruptingProvider) AWSAccountView(ctx context.Context, scope tenancy.OrgScope, account string, asOf time.Time) (storage.AWSSnapshotView, error) {
	view, err := p.inventoryProvider.AWSAccountView(ctx, scope, account, asOf)
	corruptGaps(view.Units)
	return view, err
}

// TestUnreadableGapsAreNamedNeverZero: a coverage document that cannot be
// decoded must surface as a denied gap, never as state ok with zero gaps.
func TestUnreadableGapsAreNamedNeverZero(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		provider := &gapsCorruptingProvider{inventoryProvider: openBackend(t)}
		c := &clock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
		store := New(provider, tenancy.DefaultOrgScope(), WithClock(c.Now))
		ctx := context.Background()
		detail := []model.Gap{{Operation: "DescribeVolumes", Reason: model.GapDenied}}
		if _, err := store.Commit(unit("ec2"), Candidate{Resources: []model.Resource{instance("i-1")}, DetailGaps: detail}); err != nil {
			t.Fatal(err)
		}
		current, err := store.Resources(ctx, Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		if len(current.Units) != 1 {
			t.Fatalf("units = %+v", current.Units)
		}
		if current.Units[0].Sync.State == model.SyncOK {
			t.Fatalf("an unreadable gaps document read as %s with %d gaps", current.Units[0].Sync.State, len(current.Units[0].Sync.Gaps))
		}
		if !hasDeniedGap(current.Gaps) {
			t.Fatalf("current gaps = %+v", current.Gaps)
		}
		historical, err := store.Resources(ctx, Query{Account: account, AsOf: c.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if !hasDeniedGap(historical.Gaps) {
			t.Fatalf("historical gaps = %+v", historical.Gaps)
		}
	})
}

func hasDeniedGap(gaps []model.Gap) bool {
	for _, gap := range gaps {
		if gap.Reason == model.GapDenied {
			return true
		}
	}
	return false
}

// planProvider holds the plan registration open while a reader looks at the
// totals. The registration is one store call, so a reader either sees no
// plan or the whole plan.
type planProvider struct {
	inventoryProvider
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	updates   atomic.Int64
}

func newPlanProvider(base inventoryProvider) *planProvider {
	return &planProvider{inventoryProvider: base, started: make(chan struct{}), release: make(chan struct{})}
}

func (p *planProvider) RegisterAWSPending(ctx context.Context, scope tenancy.OrgScope, units []storage.AWSUnit, guard *uint64) error {
	p.startOnce.Do(func() { close(p.started) })
	<-p.release
	return p.inventoryProvider.RegisterAWSPending(ctx, scope, units, guard)
}

func (p *planProvider) UpdateAWSSync(ctx context.Context, scope tenancy.OrgScope, unit storage.AWSUnit, guard *uint64, update func(*storage.AWSSyncState, bool) bool) error {
	p.updates.Add(1)
	return p.inventoryProvider.UpdateAWSSync(ctx, scope, unit, guard, update)
}

// TestRecordPendingRegistersTheWholePlan: while registration is held open a
// reader sees no partial total, and the plan is never written unit by unit.
func TestRecordPendingRegistersTheWholePlan(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		provider := newPlanProvider(openBackend(t))
		store := New(provider, tenancy.DefaultOrgScope(), WithClock(func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }))
		ctx := context.Background()
		units := make([]model.Unit, 26)
		for index := range units {
			units[index] = model.Unit{Account: account, Region: region, Service: serviceName(index)}
		}
		done := make(chan error, 1)
		go func() { done <- store.RecordPending(units) }()
		select {
		case <-provider.started:
		case <-time.After(5 * time.Second):
			t.Fatal("RecordPending did not register the plan in one store call")
		}
		during, err := store.Sync(ctx, Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		if len(during) != 0 {
			t.Fatalf("a reader saw a partial plan: %d units", len(during))
		}
		close(provider.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		after, err := store.Sync(ctx, Query{Account: account})
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(units) {
			t.Fatalf("plan total = %d, want %d", len(after), len(units))
		}
		if provider.updates.Load() != 0 {
			t.Fatalf("plan registration wrote %d units one statement at a time", provider.updates.Load())
		}
	})
}

func serviceName(index int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	return "unit-" + string(letters[index%len(letters)]) + string(letters[index/len(letters)])
}

// TestCommitClampsClockSkew: a candidate time earlier than the current
// generation's start is clamped to it, so a replica clock behind the last
// writer cannot close a row before it begins.
func TestCommitClampsClockSkew(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		start := c.Now()
		if _, err := store.Commit(unit("ec2"), candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		c.Set(start.Add(-time.Hour))
		generation, err := store.Commit(unit("ec2"), candidate("i-2"))
		if err != nil {
			t.Fatal(err)
		}
		if !generation.CommittedAt.Equal(start) {
			t.Fatalf("committed at %s, want %s", generation.CommittedAt, start)
		}
	})
}
