package snapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

const otherAccount = "444455556666"

func seedAccount(t *testing.T, store *Store, account, region string, now time.Time) {
	t.Helper()
	resource := instance("i-" + account[:4] + region[:2])
	resource.Account, resource.Region = account, region
	resource.ARN = model.ARN("aws", "ec2", region, account, "instance/i-"+region)
	if _, err := store.Commit(model.Unit{Account: account, Region: region, Service: "ec2"}, Candidate{Resources: []model.Resource{resource}}); err != nil {
		t.Fatal(err)
	}
	change := model.Change{ID: account + region, Time: now.Add(-time.Minute), Account: account, Region: region, Service: "ec2"}
	if err := store.AppendChanges(account, region, []model.Change{change}, []changes.Interval{{From: now.Add(-time.Hour), To: now}}, ""); err != nil {
		t.Fatal(err)
	}
	store.RecordSuccess(model.Unit{Account: account, Region: region, Service: ChangeService}, nil)
}

// rowsHeld counts what the write organization still holds for an account:
// units, current and historical resources, changes and change coverage.
func rowsHeld(t *testing.T, store *Store, account string) int {
	t.Helper()
	ctx := context.Background()
	scope := store.writeScope()
	inventory := store.inventory
	units, err := inventory.AWSUnits(ctx, scope, storage.AWSUnitQuery{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	held := len(units)
	for _, asOf := range []time.Time{{}, store.now().Add(-time.Minute), store.now()} {
		resources, err := inventory.AWSResources(ctx, scope, storage.AWSResourceQuery{Account: account, AsOf: asOf})
		if err != nil {
			t.Fatal(err)
		}
		held += len(resources)
	}
	found, err := inventory.AWSChanges(ctx, scope, storage.AWSChangeQuery{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := inventory.AWSChangeCoverage(ctx, scope, account, "")
	if err != nil {
		t.Fatal(err)
	}
	return held + len(found) + len(coverage)
}

// TestPurgeAccountRemovesEverything: after a purge the account is gone from
// unfiltered reads, from storage, and from change completeness.
func TestPurgeAccountRemovesEverything(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		now := c.Now()
		seedAccount(t, store, account, region, now)
		seedAccount(t, store, otherAccount, region, now)
		ctx := context.Background()

		if err := store.PurgeAccount(otherAccount); err != nil {
			t.Fatal(err)
		}
		resources, _ := store.Resources(ctx, Query{})
		for _, resource := range resources.Resources {
			if resource.Account == otherAccount {
				t.Fatalf("purged account still read: %+v", resource)
			}
		}
		if len(resources.Resources) != 1 {
			t.Fatalf("remaining resources = %+v", resources.Resources)
		}
		result, _ := store.Changes(ctx, Query{}, now.Add(-30*time.Minute), now)
		if !result.Complete || len(result.Changes) != 1 || result.Changes[0].Account != account {
			t.Fatalf("changes after purge = %+v", result)
		}
		if held := rowsHeld(t, store, account); held == 0 {
			t.Fatal("purge removed the other account too")
		}
		if held := rowsHeld(t, store, otherAccount); held != 0 {
			t.Fatalf("the purged account still holds %d rows", held)
		}
		// A fresh store over the same data agrees.
		reloaded := New(store.provider, store.Scope())
		accounts, _ := reloaded.Accounts()
		if len(accounts) != 1 || accounts[0] != account {
			t.Fatalf("accounts after reload = %v", accounts)
		}
	})
}

func TestPurgeRegionKeepsTheRest(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		now := c.Now()
		seedAccount(t, store, account, region, now)
		seedAccount(t, store, account, "eu-west-1", now)
		store.Commit(model.Unit{Account: account, Region: model.GlobalRegion, Service: "s3"}, Candidate{})
		if err := store.PurgeRegion(account, "eu-west-1"); err != nil {
			t.Fatal(err)
		}
		regions, _ := store.Regions(account)
		if len(regions) != 1 || regions[0] != region {
			t.Fatalf("regions = %v", regions)
		}
		statuses, _ := store.Sync(context.Background(), Query{Account: account})
		for _, status := range statuses {
			if status.Unit.Region == "eu-west-1" {
				t.Fatalf("purged region still listed: %+v", status)
			}
		}
		if !store.HasGeneration(model.Unit{Account: account, Region: model.GlobalRegion, Service: "s3"}) {
			t.Fatal("global units went with one region")
		}
		if err := store.PurgeRegion(account, model.GlobalRegion); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("global purge = %v", err)
		}
	})
}

// TestWriterCannotResurrectAPurgedAccount races a sync's writer against a
// purge: every write after the purge is refused.
func TestWriterCannotResurrectAPurgedAccount(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		writer := store.Writer(account)
		if _, err := writer.Commit(unit("ec2"), candidate("i-1")); err != nil {
			t.Fatal(err)
		}
		if writer.Stale() {
			t.Fatal("fresh writer is stale")
		}
		if err := store.PurgeAccount(account); err != nil {
			t.Fatal(err)
		}
		if !writer.Stale() {
			t.Fatal("writer did not notice the purge")
		}
		if _, err := writer.Commit(unit("ec2"), candidate("i-2")); !errors.Is(err, ErrPurged) {
			t.Fatalf("commit after purge = %v", err)
		}
		for _, err := range []error{
			writer.RecordFailure(unit("ec2"), "throttled"),
			writer.RecordSuccess(unit(ChangeService), nil),
			writer.RecordPending([]model.Unit{unit("lambda")}),
			writer.AppendChanges(region, []model.Change{{ID: "x", Time: c.Now(), Account: account}}, nil, ""),
		} {
			if !errors.Is(err, ErrPurged) {
				t.Fatalf("write after purge = %v", err)
			}
		}
		if accounts, _ := store.Accounts(); len(accounts) != 0 {
			t.Fatalf("purged account came back: %v", accounts)
		}
		// A new writer opened after the purge may write again: the account was
		// reconnected.
		if _, err := store.Writer(account).Commit(unit("ec2"), candidate("i-3")); err != nil {
			t.Fatalf("writer after reconnect = %v", err)
		}
		if _, err := writer.Commit(unit("ec2"), candidate("i-1")); !errors.Is(err, ErrPurged) {
			t.Fatal("old writer revived by the reconnect")
		}
		if _, err := writer.Commit(model.Unit{Account: otherAccount, Region: region, Service: "ec2"}, Candidate{}); !errors.Is(err, ErrInvalidUnit) {
			t.Fatalf("writer wrote another account: %v", err)
		}
	})
}

// TestViewCacheIsKeyedByGenerationAndCopied: as-of reads that land on the
// same generations share one cache entry, and changing an answer does not
// change the cache.
func TestViewCacheIsKeyedByGenerationAndCopied(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		store, _, c := newStore(t)
		start := c.Now()
		tagged := candidate("i-1")
		tagged.Resources[0].Tags = map[string]string{"app": "shop"}
		store.Commit(unit("ec2"), tagged)
		c.Advance(time.Hour)
		ctx := context.Background()
		for minute := 1; minute <= 30; minute++ {
			if _, err := store.Resources(ctx, Query{Account: account, AsOf: start.Add(time.Duration(minute) * time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		if len(store.views) != 1 {
			t.Fatalf("thirty as-of reads on one generation made %d cache entries", len(store.views))
		}
		first, _ := store.Resources(ctx, Query{Account: account})
		first.Resources[0].Tags["app"] = "changed"
		first.Resources[0].Name = "changed"
		second, _ := store.Resources(ctx, Query{Account: account})
		if second.Resources[0].Tags["app"] != "shop" || second.Resources[0].Name != "i-1" {
			t.Fatalf("caller mutation reached the cache: %+v", second.Resources[0])
		}
		for index := 0; index < 3*viewCacheSize; index++ {
			c.Advance(time.Minute)
			store.Commit(unit("ec2"), candidate("i-1", "i-"+string(rune('a'+index))))
			store.Resources(ctx, Query{Account: account})
		}
		if len(store.views) > viewCacheSize || len(store.viewOrder) != len(store.views) {
			t.Fatalf("cache holds %d views (%d ordered), bound %d", len(store.views), len(store.viewOrder), viewCacheSize)
		}
	})
}
