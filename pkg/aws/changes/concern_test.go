package changes

import "testing"

// Changes to who can act read as identity, changes to what can be reached
// read as exposure, whether the event name carries its source or the
// change's service ID names it; everything else has no concern.
func TestConcernOfClassifiesExposureAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		service, event string
		want           Concern
	}{
		{"iam", "iam:CreateUser", ConcernIdentity},
		{"iam", "DeleteRole", ConcernIdentity},
		{"", "sso:CreateAccountAssignment", ConcernIdentity},
		{"ec2", "ec2:AuthorizeSecurityGroupIngress", ConcernExposure},
		{"s3", "PutBucketPolicy", ConcernExposure},
		{"elbv2", "CreateListener", ConcernExposure},
		{"elbv2", "elasticloadbalancing:ModifyListener", ConcernExposure},
		{"secrets-manager", "PutResourcePolicy", ConcernExposure},
		{"lambda", "lambda:AddPermission20150331v2", ConcernExposure},
		{"lambda", "CreateFunctionUrlConfig20211031", ConcernExposure},
		{"lambda", "DeleteFunctionUrlConfig20211031", ConcernExposure},
		{"lambda", "AddLayerVersionPermission20181031", ConcernExposure},
		{"lambda", "UpdateFunctionCode20150331v2", ""},
		{"cloudfront", "cloudfront:UpdateDistribution2020_05_31", ConcernExposure},
		{"cloudfront", "CreateDistribution2020_05_31", ConcernExposure},
		{"cloudfront", "CreateDistributionWithTags2020_05_31", ConcernExposure},
		{"lambda", "RemoveLayerVersionPermission20181031", ConcernExposure},
		{"ec2", "DisableSnapshotBlockPublicAccess", ConcernExposure},
		{"ec2", "DisableImageBlockPublicAccess", ConcernExposure},
		{"ec2", "ModifyVpcEndpointServicePermissions", ConcernExposure},
		{"s3", "PutAccessPointPolicy", ConcernExposure},
		// Configuration that exposes something only with some parameters
		// is not shown as exposure every time.
		{"kms", "CreateGrant", ""},
		{"apigateway", "CreateDeployment", ""},
		{"api-gateway", "CreateDeployment", ""},
		{"rds", "ModifyDBInstance", ""},
		{"ec2", "ModifyInstanceAttribute", ""},
		{"sqs", "SetQueueAttributes", ""},
		{"ec2", "ec2:RunInstances", ""},
		{"lambda", "UpdateFunctionCode", ""},
		{"", "CreateUser", ""},
		{"iam", "", ""},
	} {
		if got := ConcernOf(tc.service, tc.event); got != tc.want {
			t.Errorf("ConcernOf(%q, %q) = %q; want %q", tc.service, tc.event, got, tc.want)
		}
	}
}
