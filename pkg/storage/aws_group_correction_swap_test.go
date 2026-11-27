package storage

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// A put that carries the created_at its writer read is a compare-and-swap
// on the correction's key, on every backend: the current created_at
// replaces it, a stale one or one for a correction that is gone (or never
// was) conflicts and writes nothing, and no created_at writes as before.
// Under a licence an inherited correction is matched by its own created_at
// and replaced by one in the write organization.
func TestTablesParityAWSGroupCorrectionCompareAndSwap(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSGroupCorrectionsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent}}
		at := time.Date(2026, 11, 26, 9, 0, 0, 123456789, time.UTC)
		rename := func(name string, created time.Time, expected *time.Time) (AWSGroupCorrection, error) {
			return store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
				GroupID: "stack:shop", Name: name, CreatedAt: created, ExpectedCreatedAt: expected})
		}
		names := func() []string {
			var out []string
			for _, stored := range must(store.AWSGroupCorrections(ctx, scope, correctionAccount)) {
				out = append(out, stored.Name)
			}
			return out
		}

		// Nothing stored yet: expecting a correction is a conflict.
		if _, err := rename("Shop", at, &at); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("expecting a correction that never was = %v; want a conflict", err)
		}
		if got := names(); len(got) != 0 {
			t.Fatalf("a conflict wrote %v", got)
		}
		first := must(rename("Shop", at, nil))
		// The created_at as read back, at the stored precision, matches.
		read := must(store.AWSGroupCorrections(ctx, scope, correctionAccount))[0].CreatedAt
		second := must(rename("Shop two", at.Add(time.Second), &read))
		if second.ExpectedCreatedAt != nil {
			t.Fatalf("the saved correction carries its precondition: %+v", second)
		}
		// The created_at as the writer sent it, with nanoseconds the store
		// does not keep, matches too.
		unrounded := at.Add(time.Second)
		must(rename("Shop three", at.Add(2*time.Second), &unrounded))
		// A stale created_at conflicts and writes nothing.
		if _, err := rename("Shop four", at.Add(3*time.Second), &first.CreatedAt); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("stale created_at = %v; want a conflict", err)
		}
		record("after a stale write", names())
		// The same instant in another zone is the same time.
		inZone := at.Add(2 * time.Second).In(time.FixedZone("UTC+7", 7*3600))
		must(rename("Shop five", at.Add(4*time.Second), &inZone))
		// Gone: deleted since it was read.
		deletedAt := at.Add(4 * time.Second)
		if _, err := store.DeleteAWSGroupCorrection(ctx, scope, AWSGroupCorrectionKey{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "stack:shop"}); err != nil {
			t.Fatal(err)
		}
		if _, err := rename("Shop six", at.Add(5*time.Second), &deletedAt); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("expecting a deleted correction = %v; want a conflict", err)
		}
		record("after a deleted one", names())
		// A precondition is checked per key: a merge's created_at does not
		// stand for the rename of the same group.
		merge := must(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMerge,
			GroupID: "stack:shop", TargetGroupID: "stack:store", CreatedAt: at.Add(6 * time.Second)}))
		if _, err := rename("Shop seven", at.Add(7*time.Second), &merge.CreatedAt); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("a rename expecting the merge's created_at = %v; want a conflict", err)
		}

		// Under a licence: an inherited correction is matched by its own
		// created_at, and replaced in the write organization.
		archive := tenancy.OrgScope{Write: orgs.def}
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		inherited := must(store.PutAWSGroupCorrection(ctx, archive, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "eks:data", Name: "data (old)", CreatedAt: at}))
		stale := at.Add(-time.Hour)
		if _, err := store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "eks:data", Name: "data", CreatedAt: at.Add(time.Minute), ExpectedCreatedAt: &stale}); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("stale created_at of an inherited correction = %v; want a conflict", err)
		}
		saved := must(store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "eks:data", Name: "data", CreatedAt: at.Add(time.Minute), ExpectedCreatedAt: &inherited.CreatedAt}))
		// The inherited created_at is now stale: the write organization's
		// own correction is the one a writer must expect.
		if _, err := store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "eks:data", Name: "data again", CreatedAt: at.Add(2 * time.Minute), ExpectedCreatedAt: &inherited.CreatedAt}); !errors.Is(err, ErrAWSGroupCorrectionConflict) {
			t.Fatalf("the inherited created_at after the write organization replaced it = %v; want a conflict", err)
		}
		record("licensed", must(store.AWSGroupCorrections(ctx, licensed, correctionAccount)))
		record("archive", must(store.AWSGroupCorrections(ctx, archive, correctionAccount)))
		_ = saved
	})
}

// Writers racing on one correction, each expecting the created_at they
// all read, are serialized on every backend: exactly one replaces it and
// every other one conflicts.
func TestTablesParityAWSGroupCorrectionCompareAndSwapUnderRace(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSGroupCorrectionsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
		original := must(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMerge,
			GroupID: "prefix:cart-", TargetGroupID: "stack:shop", CreatedAt: at}))
		const writers = 12
		results := make([]error, writers)
		start := make(chan struct{})
		var wait sync.WaitGroup
		for index := range writers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, results[index] = store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMerge,
					GroupID: "prefix:cart-", TargetGroupID: "stack:target" + strconv.Itoa(index),
					CreatedAt: at.Add(time.Duration(index+1) * time.Millisecond), ExpectedCreatedAt: &original.CreatedAt})
			}()
		}
		close(start)
		wait.Wait()
		saved, conflicts := 0, 0
		for index, err := range results {
			switch {
			case err == nil:
				saved++
			case errors.Is(err, ErrAWSGroupCorrectionConflict):
				conflicts++
			default:
				t.Errorf("writer %d = %v", index, err)
			}
		}
		if saved != 1 || conflicts != writers-1 {
			t.Fatalf("racing writers: %d saved, %d conflicts; want 1 and %d", saved, conflicts, writers-1)
		}
		record("held", len(must(store.AWSGroupCorrections(ctx, scope, correctionAccount))))
	})
}
