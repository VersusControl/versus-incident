package snapshot

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Relation kinds survive the store on every backend. A configured relation
// is stored in the shape rows had before kinds existed (type and target
// only), and such a row, written as an older release wrote it, reads back
// as configured.
func TestRelationKindsRoundTripAndOldRowsReadAsConfigured(t *testing.T) {
	// The row reader itself, before any later clean-up, reads an old
	// relation as configured.
	old := resourceOf(storage.AWSResource{ARN: "arn:aws:sqs:us-east-1:111122223333:orders",
		Relations: []storage.AWSRelation{{Type: string(model.RelationDeadLetter), Target: "arn:aws:sqs:us-east-1:111122223333:orders-dlq"}}})
	if len(old.Relations) != 1 || old.Relations[0].Kind != model.RelationConfigured {
		t.Fatalf("old row relations = %+v", old.Relations)
	}
	eachBackend(t, func(t *testing.T) {
		store, provider, _ := newStore(t)
		holder := instance("i-1")
		target := model.ARN("aws", "ec2", region, account, "instance/i-2")
		holder.Relations = []model.Relation{
			{Type: model.RelationAttachedTo, Target: target, Kind: model.RelationConfigured, Source: "ec2:DescribeInstances"},
			{Type: model.RelationAliasOf, Target: "web-1.us-east-1.elb.amazonaws.com", Kind: model.RelationInferred, Source: "route53:ListResourceRecordSets"},
			{Type: model.RelationMayAccess, Target: "123400000004", Kind: model.RelationKindMayAccess, Source: "s3:GetBucketPolicy",
				Access: model.AccessRead, Pattern: "prod-*", Count: 910},
			{Type: model.RelationTrusts, Target: model.OrgTarget("o-a1b2c3d4e5"), Kind: model.RelationKindMayAccess, Source: "iam:ListRoles"},
		}
		if _, err := store.Commit(unit("ec2"), Candidate{Resources: []model.Resource{holder, instance("i-2")}}); err != nil {
			t.Fatal(err)
		}
		read := func(store *Store) []model.Relation {
			t.Helper()
			result, err := store.Resources(context.Background(), Query{Account: account, Service: "ec2"})
			if err != nil {
				t.Fatal(err)
			}
			for _, resource := range result.Resources {
				if resource.ARN == holder.ARN {
					return resource.Relations
				}
			}
			t.Fatalf("holder not read: %+v", result.Resources)
			return nil
		}
		want := model.Sanitize(holder).Relations
		if got := read(New(provider, tenancy.DefaultOrgScope())); !reflect.DeepEqual(got, want) {
			t.Fatalf("relations read back:\n got %+v\nwant %+v", got, want)
		}

		row := resourceRow(model.Sanitize(holder))
		for _, relation := range row.Relations {
			data, _ := json.Marshal(relation)
			if relation.Type == string(model.RelationAttachedTo) && strings.Contains(string(data), `"kind"`) {
				t.Fatalf("configured relation stored with its kind: %s", data)
			}
		}

		if !strings.HasSuffix(t.Name(), "/postgres") {
			return
		}
		db := provider.inventoryProvider.(storage.SQLAccessor).DB()
		old := `[{"type":"attached_to","target":"` + target + `"},{"type":"in_vpc","target":"arn:aws:ec2:us-east-1:111122223333:vpc/vpc-1"}]`
		result, err := db.Exec(`UPDATE aws_resources SET relations = $1::jsonb WHERE arn = $2 AND valid_to IS NULL`, old, holder.ARN)
		if err != nil {
			t.Fatal(err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			t.Fatalf("old row update touched %d rows", count)
		}
		got := read(New(provider, tenancy.DefaultOrgScope()))
		if len(got) != 2 {
			t.Fatalf("old relations = %+v", got)
		}
		for _, relation := range got {
			if relation.Kind != model.RelationConfigured || relation.Source != "" || relation.Count != 0 {
				t.Fatalf("old relation = %+v", relation)
			}
		}
	})
}
