package changes

import "testing"

// Real CloudTrail event names, as the change list stores them, classify
// by what they change: Lambda and CloudFront append an API version to
// theirs, the rest do not; every write by the identity services is
// identity; writes that leave reach and access alone have no concern.
func TestConcernOfRealCloudTrailEventNames(t *testing.T) {
	for _, tc := range []struct {
		service, event string
		want           Concern
	}{
		// Lambda, versioned.
		{"lambda", "AddPermission20150331v2", ConcernExposure},
		{"lambda", "RemovePermission20150331v2", ConcernExposure},
		{"lambda", "lambda:RemovePermission20150331v2", ConcernExposure},
		{"lambda", "CreateFunctionUrlConfig20211031", ConcernExposure},
		{"lambda", "UpdateFunctionUrlConfig20211031", ConcernExposure},
		{"lambda", "AddLayerVersionPermission20181031", ConcernExposure},
		{"lambda", "RemoveLayerVersionPermission20181031", ConcernExposure},
		{"lambda", "UpdateFunctionConfiguration20150331v2", ""},
		{"lambda", "UpdateFunctionCode20150331v2", ""},
		{"lambda", "CreateFunction20150331", ""},
		{"lambda", "PublishVersion20150331", ""},
		{"lambda", "UpdateAlias20150331", ""},
		{"lambda", "PutFunctionConcurrency20171031", ""},
		{"lambda", "TagResource20170331v2", ""},
		// CloudFront, versioned.
		{"cloudfront", "CreateDistribution2020_05_31", ConcernExposure},
		{"cloudfront", "CreateDistributionWithTags2020_05_31", ConcernExposure},
		{"cloudfront", "UpdateDistribution2020_05_31", ConcernExposure},
		{"cloudfront", "UpdateDistribution2019_03_26", ConcernExposure},
		{"cloudfront", "cloudfront:UpdateDistribution2020_05_31", ConcernExposure},
		{"cloudfront", "CreateInvalidation2020_05_31", ""},
		{"cloudfront", "TagResource2020_05_31", ""},
		{"cloudfront", "CreateCachePolicy2020_05_31", ""},
		// Network and public access.
		{"ec2", "AuthorizeSecurityGroupIngress", ConcernExposure},
		{"ec2", "RevokeSecurityGroupEgress", ConcernExposure},
		{"ec2", "CreateRoute", ConcernExposure},
		{"ec2", "AttachInternetGateway", ConcernExposure},
		{"ec2", "ModifySnapshotAttribute", ConcernExposure},
		{"ec2", "CreateTags", ""},
		{"ec2", "StartInstances", ""},
		{"ec2", "CreateSecurityGroup", ""},
		{"s3", "PutBucketPolicy", ConcernExposure},
		{"s3", "DeleteBucketPublicAccessBlock", ConcernExposure},
		{"s3", "PutBucketAcl", ConcernExposure},
		{"s3", "PutBucketTagging", ""},
		{"s3", "PutBucketVersioning", ""},
		{"s3", "PutObject", ""},
		{"elbv2", "CreateListener", ConcernExposure},
		{"elasticloadbalancing", "ModifyListener", ConcernExposure},
		{"elbv2", "RegisterTargets", ""},
		{"kms", "PutKeyPolicy", ConcernExposure},
		{"kms", "Encrypt", ""},
		{"kms", "CreateGrant", ""},
		{"secrets-manager", "PutResourcePolicy", ConcernExposure},
		{"secretsmanager", "PutSecretValue", ""},
		{"ecr", "SetRepositoryPolicy", ConcernExposure},
		{"ecr", "PutImage", ""},
		{"sqs", "AddPermission", ConcernExposure},
		{"sqs", "SetQueueAttributes", ""},
		{"rds", "ModifyDBSnapshotAttribute", ConcernExposure},
		{"rds", "ModifyDBInstance", ""},
		{"ram", "CreateResourceShare", ConcernExposure},
		// Identity: every write by the identity services.
		{"iam", "CreateAccessKey", ConcernIdentity},
		{"iam", "iam:AttachRolePolicy", ConcernIdentity},
		{"iam", "UpdateAssumeRolePolicy", ConcernIdentity},
		{"iam", "PutRolePolicy", ConcernIdentity},
		{"sso", "sso:CreateAccountAssignment", ConcernIdentity},
		{"identitystore", "CreateGroupMembership", ConcernIdentity},
		// Not identity: assuming a role or signing in changes nobody's
		// access.
		{"sts", "AssumeRole", ""},
		{"sts", "sts:GetCallerIdentity", ""},
		{"signin", "ConsoleLogin", ""},
		// Malformed names.
		{"", "", ""},
		{"lambda", "20150331v2", ""},
		{"ec2", "ec2:", ""},
	} {
		if got := ConcernOf(tc.service, tc.event); got != tc.want {
			t.Errorf("ConcernOf(%q, %q) = %q; want %q", tc.service, tc.event, got, tc.want)
		}
	}
}
