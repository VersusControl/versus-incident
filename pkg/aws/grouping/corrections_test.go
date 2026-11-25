package grouping

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func fromStore(stored []storage.AWSGroupCorrection) []Correction {
	out := make([]Correction, 0, len(stored))
	for _, correction := range stored {
		out = append(out, Correction{Account: correction.AccountID, Kind: correction.Kind, GroupID: correction.GroupID,
			MemberARN: correction.MemberARN, TargetGroupID: correction.TargetGroupID, Name: correction.Name})
	}
	return out
}

func groupByID(t *testing.T, result Result, id string) Group {
	t.Helper()
	for _, group := range result.Of(prodAccount) {
		if group.ID == id {
			return group
		}
	}
	t.Fatalf("no group %s in %+v", id, shapes(result.Of(prodAccount)))
	return Group{}
}

// A rename, a merge and a move are stored, read back and applied; they
// still apply after the estate is read again with every node replaced, on
// the memory and the file store.
func TestCorrectionsSurviveReread(t *testing.T) {
	file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	for name, provider := range map[string]storage.Provider{"memory": storage.NewMemory(), "file": file} {
		t.Run(name, func(t *testing.T) {
			store, ok := storage.AWSGroupCorrectionsOf(provider)
			if !ok {
				t.Fatal("no group correction store")
			}
			ctx := context.Background()
			scope := tenancy.DefaultOrgScope()
			at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
			receipts := arnOf(prodAccount, "lambda", "function/fn007")
			for index, correction := range []storage.AWSGroupCorrection{
				{AccountID: prodAccount, Kind: storage.AWSGroupRename, GroupID: "tag:app=checkout", Name: "Checkout"},
				{AccountID: prodAccount, Kind: storage.AWSGroupMerge, GroupID: "eks:prod-eks/ns:search", TargetGroupID: "eks:prod-eks"},
				{AccountID: prodAccount, Kind: storage.AWSGroupMove, MemberARN: receipts, TargetGroupID: "tag:app=checkout"},
				{AccountID: prodAccount, Kind: storage.AWSGroupMove, MemberARN: arnOf(prodAccount, "iam", "role/r001"), TargetGroupID: "user:identity"},
			} {
				correction.CreatedAt = at.Add(time.Duration(index) * time.Minute)
				if _, err := store.PutAWSGroupCorrection(ctx, scope, correction); err != nil {
					t.Fatal(err)
				}
			}
			for round := range 3 {
				stored, err := store.AWSGroupCorrections(ctx, scope, prodAccount)
				if err != nil {
					t.Fatal(err)
				}
				result := Build(prodEstate(round), nil, fromStore(stored), Options{})
				want := []CorrectionStatus{CorrectionApplied, CorrectionApplied, CorrectionApplied, CorrectionApplied}
				if !reflect.DeepEqual(result.Corrections, want) {
					t.Fatalf("round %d statuses = %v", round, result.Corrections)
				}
				checkout := groupByID(t, result, "tag:app=checkout")
				if checkout.Name != "Checkout" || !checkout.Renamed || len(checkout.Members) != 97 || checkout.ByType["function"] != 31 {
					t.Fatalf("round %d checkout = %s %v %d %v", round, checkout.Name, checkout.Renamed, len(checkout.Members), checkout.ByType)
				}
				if cluster := groupByID(t, result, "eks:prod-eks"); len(cluster.Members) != 432 {
					t.Fatalf("round %d cluster holds %d", round, len(cluster.Members))
				}
				if user := groupByID(t, result, "user:identity"); user.Rule != RuleUser || user.Name != "identity" || len(user.Members) != 1 {
					t.Fatalf("round %d user group = %+v", round, user)
				}
				if ungrouped := groupByID(t, result, UngroupedID); len(ungrouped.Members) != 466 {
					t.Fatalf("round %d ungrouped holds %d", round, len(ungrouped.Members))
				}
				for _, group := range result.Of(prodAccount) {
					if group.ID == "eks:prod-eks/ns:search" {
						t.Fatalf("round %d the merged group is still shown", round)
					}
				}
			}
		})
	}
}

// A correction whose group is gone reads "no longer matches" and is never
// applied to another group. When the learned key changes (the node pool
// loses its app tag, so app has one value left and is no longer learned),
// checkout's members regroup under the service key; the rename and the
// merge aimed at tag:app=checkout stay unapplied instead of landing on the
// new group.
func TestStaleCorrectionNoLongerMatches(t *testing.T) {
	corrections := []Correction{
		{Account: prodAccount, Kind: CorrectionRename, GroupID: "tag:app=checkout", Name: "Checkout"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:app=checkout"},
		{Account: prodAccount, Kind: CorrectionMove, MemberARN: arnOf(prodAccount, "lambda", "function/fn001"), TargetGroupID: "tag:app=checkout"},
		{Account: prodAccount, Kind: CorrectionMove, MemberARN: arnOf(prodAccount, "lambda", "function/gone"), TargetGroupID: "stack:baseline"},
		{Account: prodAccount, Kind: CorrectionRename, GroupID: "prefix:cart-", Name: "Cart"},
		{Account: prodAccount, Kind: CorrectionMove, MemberARN: arnOf(prodAccount, "lambda", "function/fn002"), TargetGroupID: "vpc:vpc-gone"},
		{Account: "123400000099", Kind: CorrectionRename, GroupID: "stack:baseline", Name: "x"},
	}
	before := Build(prodEstate(0), nil, corrections, Options{})
	want := []CorrectionStatus{CorrectionApplied, CorrectionApplied, CorrectionApplied, CorrectionNoLongerMatches,
		CorrectionNoLongerMatches, CorrectionNoLongerMatches, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(before.Corrections, want) {
		t.Fatalf("before = %v", before.Corrections)
	}

	var shifted []model.Resource
	for _, resource := range prodEstate(0) {
		if resource.Tags["app"] == "platform" {
			tags := map[string]string{}
			for key, value := range resource.Tags {
				if key != "app" {
					tags[key] = value
				}
			}
			resource.Tags = tags
		}
		shifted = append(shifted, resource)
	}
	if learned := Learn(shifted)[prodAccount]; !reflect.DeepEqual(learned, []string{"service"}) {
		t.Fatalf("learned after the shift = %v", learned)
	}
	after := Build(shifted, nil, corrections, Options{})
	want = []CorrectionStatus{CorrectionNoLongerMatches, CorrectionNoLongerMatches, CorrectionNoLongerMatches, CorrectionNoLongerMatches,
		CorrectionNoLongerMatches, CorrectionNoLongerMatches, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(after.Corrections, want) {
		t.Fatalf("after = %v", after.Corrections)
	}
	checkout := groupByID(t, after, "tag:service=checkout-api")
	payments := groupByID(t, after, "tag:service=payments")
	if checkout.Renamed || checkout.Name != "checkout-api" || len(checkout.Members) != 96 || len(payments.Members) != 74 {
		t.Fatalf("a stale correction was applied: checkout %s %d, payments %d", checkout.Name, len(checkout.Members), len(payments.Members))
	}
	if ungrouped := groupByID(t, after, UngroupedID); len(ungrouped.Members) != 468 {
		t.Fatalf("a stale move was applied: ungrouped holds %d", len(ungrouped.Members))
	}
}

// A merge into a group that was merged away no longer matches, so merges
// never loop; Ungrouped cannot be renamed or merged, but a resource can be
// moved into it.
func TestCorrectionEdges(t *testing.T) {
	corrections := []Correction{
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:app=checkout"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "tag:app=checkout", TargetGroupID: "stack:baseline"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: "stack:baseline", TargetGroupID: "tag:service=payments"},
		{Account: prodAccount, Kind: CorrectionRename, GroupID: UngroupedID, Name: "Other"},
		{Account: prodAccount, Kind: CorrectionMerge, GroupID: UngroupedID, TargetGroupID: "eks:prod-eks"},
		{Account: prodAccount, Kind: CorrectionMove, MemberARN: arnOf(prodAccount, "eks", "cluster/prod-eks"), TargetGroupID: UngroupedID},
		{Account: prodAccount, Kind: "split", GroupID: "eks:prod-eks"},
	}
	result := Build(prodEstate(0), nil, corrections, Options{})
	want := []CorrectionStatus{CorrectionApplied, CorrectionApplied, CorrectionNoLongerMatches, CorrectionNoLongerMatches,
		CorrectionNoLongerMatches, CorrectionApplied, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(result.Corrections, want) {
		t.Fatalf("statuses = %v", result.Corrections)
	}
	if baseline := groupByID(t, result, "stack:baseline"); len(baseline.Members) != 300 {
		t.Fatalf("the merged group holds %d", len(baseline.Members))
	}
	if ungrouped := groupByID(t, result, UngroupedID); ungrouped.Name != "Ungrouped" || len(ungrouped.Members) != 469 {
		t.Fatalf("ungrouped = %s %d", ungrouped.Name, len(ungrouped.Members))
	}
}

// A deployment undoes a merge it inherits by writing the neutral merge of
// the group into itself under its own organization: the neutral row wins
// the key, the group stays whole, and the row reads applied.
func TestNeutralMergeUndoesAnInheritedMerge(t *testing.T) {
	provider := storage.NewMemory()
	store, _ := storage.AWSGroupCorrectionsOf(provider)
	ctx := context.Background()
	licensed := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	if _, err := store.PutAWSGroupCorrection(ctx, tenancy.DefaultOrgScope(), storage.AWSGroupCorrection{AccountID: prodAccount,
		Kind: storage.AWSGroupMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:app=checkout"}); err != nil {
		t.Fatal(err)
	}
	read := func() Result {
		stored, err := store.AWSGroupCorrections(ctx, licensed, prodAccount)
		if err != nil {
			t.Fatal(err)
		}
		return Build(prodEstate(0), nil, fromStore(stored), Options{})
	}
	if checkout := groupByID(t, read(), "tag:app=checkout"); len(checkout.Members) != 170 {
		t.Fatalf("inherited merge not applied: checkout holds %d", len(checkout.Members))
	}
	if _, err := store.PutAWSGroupCorrection(ctx, licensed, storage.AWSGroupCorrection{AccountID: prodAccount,
		Kind: storage.AWSGroupMerge, GroupID: "tag:service=payments", TargetGroupID: "tag:service=payments"}); err != nil {
		t.Fatal(err)
	}
	result := read()
	if !reflect.DeepEqual(result.Corrections, []CorrectionStatus{CorrectionApplied}) {
		t.Fatalf("statuses = %v", result.Corrections)
	}
	if checkout, payments := groupByID(t, result, "tag:app=checkout"), groupByID(t, result, "tag:service=payments"); len(checkout.Members) != 96 ||
		len(payments.Members) != 74 {
		t.Fatalf("after the undo: checkout %d, payments %d", len(checkout.Members), len(payments.Members))
	}
}

// A group emptied by moves is not shown, so a later rename of it no longer
// matches, and lands on no other group.
func TestRenameOfAGroupEmptiedByMoves(t *testing.T) {
	var resources []model.Resource
	for _, name := range []string{"orders-a", "orders-b", "orders-c"} {
		resources = append(resources, res(prodAccount, "lambda", "function", name, name, nil))
	}
	var corrections []Correction
	for _, resource := range resources {
		corrections = append(corrections, Correction{Account: prodAccount, Kind: CorrectionMove, MemberARN: resource.ARN, TargetGroupID: "user:shop"})
	}
	corrections = append(corrections, Correction{Account: prodAccount, Kind: CorrectionRename, GroupID: "prefix:orders-", Name: "Orders"})
	result := Build(resources, TagKeys{}, corrections, Options{})
	want := []CorrectionStatus{CorrectionApplied, CorrectionApplied, CorrectionApplied, CorrectionNoLongerMatches}
	if !reflect.DeepEqual(result.Corrections, want) {
		t.Fatalf("statuses = %v", result.Corrections)
	}
	for _, group := range result.Of(prodAccount) {
		if group.Renamed || group.ID == "prefix:orders-" {
			t.Fatalf("group = %+v", group)
		}
	}
}
