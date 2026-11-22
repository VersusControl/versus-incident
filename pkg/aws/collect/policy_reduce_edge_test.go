package collect

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// A trust open to every principal but narrowed by an organization
// condition is one organization link, whichever organization key or case
// the condition uses; with no narrowing it is anyone.
func TestTrustNarrowedByOrganizationCondition(t *testing.T) {
	trust := func(condition string) string {
		statement := `{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"`
		if condition != "" {
			statement += `,"Condition":` + condition
		}
		return url.QueryEscape(`{"Version":"2012-10-17","Statement":[` + statement + `}]}`)
	}
	org := model.OrgTarget("o-a1b2c3d4e5")
	for name, tc := range map[string]struct {
		condition string
		want      string
	}{
		"organization ID":          {`{"StringEquals":{"aws:PrincipalOrgID":"o-a1b2c3d4e5"}}`, org},
		"organization ID any case": {`{"StringEquals":{"AWS:principalorgid":["o-a1b2c3d4e5"]}}`, org},
		"organization path":        {`{"ForAnyValue:StringLike":{"aws:PrincipalOrgPaths":["o-a1b2c3d4e5/r-ab12/ou-ab12-11111111/*"]}}`, org},
		"principal account":        {`{"StringEquals":{"aws:PrincipalAccount":"123400000004"}}`, "123400000004"},
		"not narrowing":            {`{"StringEquals":{"aws:SourceAccount":"123400000004"}}`, model.AnyPrincipal},
		"no condition":             {"", model.AnyPrincipal},
	} {
		t.Run(name, func(t *testing.T) {
			grants := reducePolicy(trust(tc.condition), policyOwner, "", assumeActions)
			if len(grants) != 1 || grants[0].principal != tc.want {
				t.Fatalf("grants = %+v, want principal %q", grants, tc.want)
			}
			relations := grantRelations(model.RelationTrusts, sourceTrust, grants)
			want := model.Relation{Type: model.RelationTrusts, Target: tc.want, Kind: model.RelationKindMayAccess, Source: sourceTrust}
			if !reflect.DeepEqual(relations, []model.Relation{want}) {
				t.Fatalf("relations = %+v", relations)
			}
		})
	}
}

// IAM unique IDs of deleted users, roles and policies stay as the ID in a
// resource policy; an ID-like string that is not one is dropped.
func TestResourcePolicyKeepsUnknownPrincipalIDs(t *testing.T) {
	document := `{"Statement":[{"Effect":"Allow","Principal":{"AWS":["AIDAJQABLZS4A3QDU576Q","AROAEXAMPLEDELETED003","AIDAshort"]},` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::exports/*"}]}`
	got := reducePolicy(document, policyOwner, "exports", nil)
	want := []policyGrant{{principal: "AIDAJQABLZS4A3QDU576Q", access: model.AccessRead}, {principal: "AROAEXAMPLEDELETED003", access: model.AccessRead}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grants = %+v, want %+v", got, want)
	}
}

// What is kept of a policy is the principal, the access class and the
// resource pattern. Statement IDs, actions, condition keys and values,
// ExternalIds and denied principals never reach a relation.
func TestReducedPolicyCarriesNoDocumentText(t *testing.T) {
	documents := map[string]struct {
		document, subject string
		keep              func([]string) bool
		kind              model.RelationType
	}{
		"bucket": {
			document: `{"Version":"2012-10-17","Id":"marker-policy-id","Statement":[{"Sid":"marker-sid-bucket","Effect":"Allow",` +
				`"Principal":{"AWS":"arn:aws:iam::123400000004:role/etl"},"Action":["s3:GetObject"],"Resource":"arn:aws:s3:::exports/prod-*",` +
				`"Condition":{"StringEquals":{"aws:SourceVpce":"vpce-marker0001","s3:ExistingObjectTag/marker-tag":"marker-tag-value"}}},` +
				`{"Sid":"marker-sid-deny","Effect":"Deny","NotPrincipal":{"AWS":"arn:aws:iam::123400000099:role/marker-denied"},"Action":"s3:*","Resource":"*"}]}`,
			subject: "exports", kind: model.RelationMayAccess,
		},
		"key": {
			document: `{"Statement":[{"Sid":"marker-sid-key","Effect":"Allow","Principal":{"AWS":"*"},"Action":["kms:Decrypt"],"Resource":"*",` +
				`"Condition":{"StringEquals":{"kms:CallerAccount":"123400000004","kms:ViaService":"marker-service.eu-south-2.amazonaws.com",` +
				`"kms:EncryptionContext:marker-context":"marker-context-value"}}}]}`,
			subject: "key/0d0d", kind: model.RelationMayAccess,
		},
		"trust": {
			document: url.QueryEscape(`{"Statement":[{"Sid":"marker-sid-trust","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::555500006666:root"},` +
				`"Action":"sts:AssumeRole","Condition":{"StringEquals":{"sts:ExternalId":"marker-external-id-value"}}}]}`),
			keep: assumeActions, kind: model.RelationTrusts,
		},
	}
	for name, tc := range documents {
		t.Run(name, func(t *testing.T) {
			relations := grantRelations(tc.kind, sourceBucket, reducePolicy(tc.document, policyOwner, tc.subject, tc.keep))
			if len(relations) != 1 {
				t.Fatalf("relations = %+v", relations)
			}
			stored := model.Sanitize(model.Resource{ARN: "arn:aws:s3:::exports#policy", Relations: relations,
				Attributes: map[string]string{model.AttrSubject: "arn:aws:s3:::exports", "policy": tc.document}})
			data, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			for _, leaked := range []string{"marker", "Statement", "Condition", "Sid", "kms:", "s3:GetObject", "sts:", "vpce-", "%7B"} {
				if strings.Contains(string(data), leaked) {
					t.Errorf("stored policy entry carries %q: %s", leaked, data)
				}
			}
		})
	}
}

// The unit report of a sync that read trust, key, bucket and repository
// policies carries no policy text or ExternalId either.
func TestEstateSyncReportCarriesNoPolicyText(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioEstateHubSpoke)
	for _, account := range []string{"123400000003", "123400000004"} {
		report := mustSync(t, h, Target{AccountID: account, Regions: []string{"eu-south-2"}, Credentials: testCredentials(account)})
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data) + fmt.Sprintf("%+v", report)
		for _, leaked := range []string{fakeaws.EstateExternalIDValue, "Statement", "sts:ExternalId", "aws:SourceArn", "kms:ViaService",
			"Principal", "%22Statement"} {
			if strings.Contains(text, leaked) {
				t.Errorf("%s report carries %q", account, leaked)
			}
		}
	}
}

// A read of a policy, an ACL or the grants is a read of metadata, not a
// change; a wildcard that covers policy or ACL changes is admin.
func TestAccessClassReadsOfPoliciesAndWildcardWrites(t *testing.T) {
	for _, tc := range []struct {
		actions []string
		want    string
	}{
		{[]string{"s3:GetBucketPolicy"}, model.AccessRead},
		{[]string{"s3:GetBucketAcl", "s3:GetObjectAcl"}, model.AccessRead},
		{[]string{"kms:GetKeyPolicy", "kms:ListGrants"}, model.AccessRead},
		{[]string{"ecr:GetRepositoryPolicy"}, model.AccessRead},
		{[]string{"s3:Put*"}, model.AccessAdmin},
		{[]string{"ecr:Set*"}, model.AccessAdmin},
		{[]string{"s3:PutObject*"}, model.AccessAdmin},
		{[]string{"s3:PutBucket*"}, model.AccessAdmin},
		{[]string{"kms:PutKey*"}, model.AccessAdmin},
		{[]string{"ecr:SetRepository*"}, model.AccessAdmin},
		{[]string{"kms:CreateG*"}, model.AccessAdmin},
	} {
		if got := accessClass(tc.actions, nil); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.actions, got, tc.want)
		}
	}
}
