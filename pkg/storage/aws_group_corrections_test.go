package storage

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const correctionAccount = "123400000002"

// Group corrections behave the same on every backend: a put is an upsert
// on the correction's key, reads come in the order corrections were made
// and merge the read organizations with the first one winning, and a
// delete touches the write organization only.
func TestTablesParityAWSGroupCorrections(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, ok := AWSGroupCorrectionsOf(backend.provider)
		if !ok {
			t.Fatalf("%s provider has no group correction store", backend.name)
		}
		ctx := context.Background()
		archive := tenancy.OrgScope{Write: orgs.def}
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		at := time.Date(2026, 11, 22, 9, 30, 0, 123456789, time.UTC)
		member := "arn:aws:lambda:eu-south-2:" + correctionAccount + ":function:checkout-receipts"

		mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, archive, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "tag:app=checkout", Name: "Checkout (old)", CreatedBy: "alice", CreatedAt: at})))
		mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, archive, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMerge,
			GroupID: "prefix:cart-", TargetGroupID: "tag:app=checkout", CreatedAt: at.Add(time.Minute)})))
		saved := must(store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "tag:app=checkout", Name: "  Checkout ", CreatedBy: "bob", CreatedAt: at.Add(2 * time.Minute)}))
		if saved.Name != "Checkout" || !saved.CreatedAt.Equal(at.Add(2*time.Minute).Truncate(time.Microsecond)) {
			t.Fatalf("saved = %+v", saved)
		}
		mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMove,
			MemberARN: member, TargetGroupID: "tag:app=checkout", CreatedAt: at.Add(3 * time.Minute)})))
		mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: "123400000004", Kind: AWSGroupRename,
			GroupID: "eks:data-eks", Name: "data cluster", CreatedAt: at})))

		merged := must(store.AWSGroupCorrections(ctx, licensed, correctionAccount))
		if len(merged) != 3 || merged[0].Kind != AWSGroupMerge || !merged[0].Inherited ||
			merged[1].Kind != AWSGroupRename || merged[1].Name != "Checkout" || merged[1].Inherited ||
			merged[2].Kind != AWSGroupMove || merged[2].MemberARN != member {
			t.Fatalf("licensed read = %+v", merged)
		}
		record("licensed read", merged)
		record("every account", must(store.AWSGroupCorrections(ctx, licensed, "")))
		record("archive read", must(store.AWSGroupCorrections(ctx, archive, correctionAccount)))

		// A second move of the same member replaces the first.
		moved := must(store.PutAWSGroupCorrection(ctx, licensed, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMove,
			MemberARN: member, TargetGroupID: "user:receipts", CreatedAt: at.Add(4 * time.Minute)}))
		if moved.TargetGroupID != "user:receipts" {
			t.Fatalf("move = %+v", moved)
		}
		afterMove := must(store.AWSGroupCorrections(ctx, licensed, correctionAccount))
		if len(afterMove) != 3 || afterMove[2].TargetGroupID != "user:receipts" {
			t.Fatalf("after second move = %+v", afterMove)
		}
		record("after second move", afterMove)

		renameKey := AWSGroupCorrectionKey{AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout"}
		record("delete own", must(store.DeleteAWSGroupCorrection(ctx, licensed, renameKey)))
		// The default org's rename shows through now, inherited, and
		// cannot be deleted from the licensed org.
		if deleted, err := store.DeleteAWSGroupCorrection(ctx, licensed, renameKey); deleted || !errors.Is(err, ErrAWSGroupCorrectionInherited) {
			t.Fatalf("delete of the rename that shows through = %v, %v", deleted, err)
		}
		record("delete missing", must(store.DeleteAWSGroupCorrection(ctx, licensed,
			AWSGroupCorrectionKey{AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "stack:baseline"})))
		remaining := must(store.AWSGroupCorrections(ctx, licensed, correctionAccount))
		if len(remaining) != 3 || remaining[0].Kind != AWSGroupRename || remaining[0].Name != "Checkout (old)" || !remaining[0].Inherited {
			t.Fatalf("after delete = %+v", remaining)
		}
		record("after delete", remaining)
	})
}

// Every malformed correction is refused before it reaches a table, on
// every backend: unknown kinds, fields a kind does not take, bad accounts,
// group IDs and ARNs, and names that are too long or hold control
// characters.
func TestTablesParityAWSGroupCorrectionValidation(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSGroupCorrectionsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent}}
		arn := "arn:aws:ec2:eu-south-2:" + correctionAccount + ":volume/vol-0abc"
		at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
		atLimit := strings.Repeat("界", MaxAWSGroupName)
		saved := must(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "tag:app=checkout", Name: atLimit, CreatedAt: at}))
		if saved.Name != atLimit {
			t.Fatalf("saved = %+v", saved)
		}
		userAtLimit := must(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMove,
			MemberARN: arn, TargetGroupID: "user:" + atLimit, CreatedAt: at}))
		if userAtLimit.TargetGroupID != "user:"+atLimit {
			t.Fatalf("user group at the limit = %+v", userAtLimit)
		}
		// A merge into its own group is the neutral row that undoes an
		// inherited merge.
		mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMerge,
			GroupID: "stack:baseline", TargetGroupID: "stack:baseline", CreatedAt: at})))
		bucket := must(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupMove,
			MemberARN: "arn:aws:s3:::checkout-assets", TargetGroupID: "tag:app=checkout", CreatedAt: at}))
		if bucket.MemberARN != "arn:aws:s3:::checkout-assets" {
			t.Fatalf("bucket move = %+v", bucket)
		}
		for name, correction := range ordered(map[string]AWSGroupCorrection{
			"unknown kind":      {AccountID: correctionAccount, Kind: "split", GroupID: "tag:app=checkout"},
			"short account":     {AccountID: "1234", Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "x"},
			"rename no name":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "  "},
			"rename long":       {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: atLimit + "界"},
			"rename control":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\nb"},
			"rename del":        {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\x7fb"},
			"rename nul":        {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\x00b"},
			"rename utf8":       {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\xffb"},
			"rename target":     {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "x", TargetGroupID: "vpc:vpc-1"},
			"rename member":     {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "x", MemberARN: arn},
			"rename no group":   {AccountID: correctionAccount, Kind: AWSGroupRename, Name: "x"},
			"group control":     {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=a\tb", Name: "x"},
			"group too long":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=" + strings.Repeat("a", MaxAWSGroupID), Name: "x"},
			"group padded":      {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: " tag:app=checkout", Name: "x"},
			"rename c1 control": {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\u0085b"},
			"rename bidi":       {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "abc\u202edcba"},
			"rename isolate":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "a\u2067b"},
			"rename zero width": {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "chec\u200bkout"},
			"rename bom":        {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "\ufeffcheckout"},
			"soft hyphen":       {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\u00adout"},
			"mongolian vs":      {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\u180eout"},
			"line separator":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\u2028out"},
			"para separator":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\u2029out"},
			"annotation":        {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\ufff9out\ufffb"},
			"tag block":         {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "check\U000e0041out"},
			"user group tag":    {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn, TargetGroupID: "user:a\U000e0041b"},
			"user group long":   {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn, TargetGroupID: "user:" + atLimit + "界"},
			"user group bidi":   {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn, TargetGroupID: "user:a\u202eb"},
			"user group blank":  {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn, TargetGroupID: "user:  "},
			"merge user long":   {AccountID: correctionAccount, Kind: AWSGroupMerge, GroupID: "tag:app=checkout", TargetGroupID: "user:" + atLimit + "界"},
			"merge no target":   {AccountID: correctionAccount, Kind: AWSGroupMerge, GroupID: "tag:app=checkout"},
			"merge name":        {AccountID: correctionAccount, Kind: AWSGroupMerge, GroupID: "tag:app=checkout", TargetGroupID: "stack:baseline", Name: "x"},
			"move no member":    {AccountID: correctionAccount, Kind: AWSGroupMove, TargetGroupID: "tag:app=checkout"},
			"move group":        {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn, GroupID: "vpc:vpc-1", TargetGroupID: "tag:app=checkout"},
			"move not arn":      {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: "vol-0abc", TargetGroupID: "tag:app=checkout"},
			"move other acct":   {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: "arn:aws:ec2:eu-south-2:123400000004:volume/vol-1", TargetGroupID: "tag:app=checkout"},
			"move arn space":    {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn + " x", TargetGroupID: "tag:app=checkout"},
			"move arn too long": {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn + strings.Repeat("a", MaxAWSGroupMemberARN), TargetGroupID: "tag:app=checkout"},
			"move no target":    {AccountID: correctionAccount, Kind: AWSGroupMove, MemberARN: arn},
			"created by nul":    {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", Name: "x", CreatedBy: "a\x00b"},
		}) {
			if _, err := store.PutAWSGroupCorrection(ctx, scope, correction); !errors.Is(err, ErrInvalidAWSGroupCorrection) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		for name, key := range ordered(map[string]AWSGroupCorrectionKey{
			"bad kind":     {AccountID: correctionAccount, Kind: "split", GroupID: "tag:app=checkout"},
			"move group":   {AccountID: correctionAccount, Kind: AWSGroupMove, GroupID: "tag:app=checkout", MemberARN: arn},
			"rename arn":   {AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "tag:app=checkout", MemberARN: arn},
			"bad account":  {AccountID: "x", Kind: AWSGroupRename, GroupID: "tag:app=checkout"},
			"empty rename": {AccountID: correctionAccount, Kind: AWSGroupRename},
		}) {
			if _, err := store.DeleteAWSGroupCorrection(ctx, scope, key); !errors.Is(err, ErrInvalidAWSGroupCorrection) {
				t.Errorf("delete %s: err = %v", name, err)
			}
		}
		if _, err := store.AWSGroupCorrections(ctx, scope, "not-an-account"); !errors.Is(err, ErrInvalidAWSGroupCorrection) {
			t.Errorf("read bad account: err = %v", err)
		}
		record("after refused", must(store.AWSGroupCorrections(ctx, scope, correctionAccount)))
	})
}

// One account holds a bounded number of corrections: a new one beyond the
// bound is refused, replacing an existing one still works, and another
// account is not affected. The bound is lowered so every backend runs it
// quickly.
func TestTablesParityAWSGroupCorrectionLimit(t *testing.T) {
	saved := awsGroupCorrectionLimit
	awsGroupCorrectionLimit = 40
	t.Cleanup(func() { awsGroupCorrectionLimit = saved })
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSGroupCorrectionsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent}}
		at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
		for index := range awsGroupCorrectionLimit {
			mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
				GroupID: "prefix:g" + strconv.Itoa(index) + "-", Name: "n", CreatedAt: at})))
		}
		_, err := store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename, GroupID: "vpc:vpc-1", Name: "n"})
		if !errors.Is(err, ErrAWSGroupCorrectionLimit) {
			t.Fatalf("over the limit = %v", err)
		}
		record("replace at limit", errOnly(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
			GroupID: "prefix:g0-", Name: "renamed", CreatedAt: at})) == nil)
		record("other account", errOnly(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: "123400000004", Kind: AWSGroupRename,
			GroupID: "vpc:vpc-1", Name: "n", CreatedAt: at})) == nil)
		record("count", len(must(store.AWSGroupCorrections(ctx, scope, correctionAccount))))
	})
}

// Writers racing for the account's last free correction never take it
// twice: exactly one new correction gets in, on every backend.
func TestTablesParityAWSGroupCorrectionLimitUnderRace(t *testing.T) {
	saved := awsGroupCorrectionLimit
	awsGroupCorrectionLimit = 10
	t.Cleanup(func() { awsGroupCorrectionLimit = saved })
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSGroupCorrectionsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent}}
		at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
		for index := range awsGroupCorrectionLimit - 1 {
			mustDo(t, errOnly(store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
				GroupID: "prefix:g" + strconv.Itoa(index) + "-", Name: "n", CreatedAt: at})))
		}
		const writers = 12
		results := make(chan error, writers)
		start := make(chan struct{})
		for index := range writers {
			go func() {
				<-start
				_, err := store.PutAWSGroupCorrection(ctx, scope, AWSGroupCorrection{AccountID: correctionAccount, Kind: AWSGroupRename,
					GroupID: "prefix:race" + strconv.Itoa(index) + "-", Name: "n", CreatedAt: at})
				results <- err
			}()
		}
		close(start)
		accepted, refused := 0, 0
		for range writers {
			switch err := <-results; {
			case err == nil:
				accepted++
			case errors.Is(err, ErrAWSGroupCorrectionLimit):
				refused++
			default:
				t.Errorf("racing write = %v", err)
			}
		}
		if accepted != 1 || refused != writers-1 {
			t.Fatalf("accepted %d, refused %d", accepted, refused)
		}
		record("held", len(must(store.AWSGroupCorrections(ctx, scope, correctionAccount))))
	})
}

// The group correction migration is schema only. Its table keys one row
// per organization, account, kind, group and member, refuses an unknown
// kind and a row with no time, and fills the unused columns with the empty
// string.
func TestAWSGroupCorrectionsMigrationSchema(t *testing.T) {
	text, err := os.ReadFile("migrations/042_aws_group_corrections.sql")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE)\b`).Match(stripSQLComments(string(text))) {
		t.Fatalf("migration carries a data statement:\n%s", text)
	}
	orgs := newTablesOrgs()
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			insert := func(org, kind, group, member, created string) error {
				_, err := backend.raw.Exec(`INSERT INTO aws_group_corrections (org_id, account_id, kind, group_id, member_arn, created_at) VALUES ('` +
					org + `', '` + correctionAccount + `', '` + kind + `', '` + group + `', '` + member + `', ` + created + `)`)
				return err
			}
			at := "'2026-11-22T10:00:00Z'"
			if err := insert(orgs.ent, "rename", "tag:app=checkout", "", at); err != nil {
				t.Fatal(err)
			}
			if err := insert(orgs.ent, "rename", "tag:app=checkout", "", at); err == nil {
				t.Fatal("a second rename of one group was stored")
			}
			if err := insert(orgs.ent, "merge", "tag:app=checkout", "", at); err != nil {
				t.Fatalf("a merge of the renamed group: %v", err)
			}
			if err := insert(orgs.def, "rename", "tag:app=checkout", "", at); err != nil {
				t.Fatalf("same rename under another organization: %v", err)
			}
			if err := insert(orgs.ent, "move", "", "arn:aws:s3:::a", at); err != nil {
				t.Fatalf("a move: %v", err)
			}
			if err := insert(orgs.ent, "move", "", "arn:aws:s3:::a", at); err == nil {
				t.Fatal("a second move of one member was stored")
			}
			if err := insert(orgs.ent, "split", "tag:app=x", "", at); err == nil {
				t.Fatal("an unknown kind was stored")
			}
			if err := insert(orgs.ent, "rename", "vpc:vpc-1", "", "NULL"); err == nil {
				t.Fatal("a row with no time was stored")
			}
			var target, name, createdBy string
			if err := backend.raw.QueryRow(`SELECT target_group_id, name, created_by FROM aws_group_corrections WHERE org_id = '`+orgs.def+`'`).Scan(&target, &name, &createdBy); err != nil {
				t.Fatal(err)
			}
			if target != "" || name != "" || createdBy != "" {
				t.Fatalf("defaults = %q %q %q", target, name, createdBy)
			}
			if backend.postgres {
				var columns int
				if err := backend.raw.QueryRow(`SELECT count(*) FROM information_schema.key_column_usage k
 JOIN information_schema.table_constraints c ON c.constraint_name = k.constraint_name AND c.table_name = k.table_name
 WHERE k.table_name = 'aws_group_corrections' AND c.constraint_type = 'PRIMARY KEY'
 AND k.column_name IN ('org_id', 'account_id', 'kind', 'group_id', 'member_arn')`).Scan(&columns); err != nil {
					t.Fatal(err)
				}
				if columns != 5 {
					t.Fatalf("primary key columns = %d, want org_id, account_id, kind, group_id and member_arn", columns)
				}
			}
		})
	}
}
