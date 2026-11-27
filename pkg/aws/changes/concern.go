package changes

import "strings"

// Concern is what a change touches that a reader of the estate cares about
// first: who can reach it from outside, or who can act in the account.
type Concern string

const (
	// ConcernExposure is a change to what can be reached and from where:
	// network rules, public access, resource policies, listeners and
	// public endpoints.
	ConcernExposure Concern = "exposure"
	// ConcernIdentity is a change to who can act: users, roles, groups,
	// policies, keys and single sign-on assignments.
	ConcernIdentity Concern = "identity"
)

// identitySources are the event sources whose every write changes who can
// act.
var identitySources = map[string]bool{
	"iam":           true,
	"sso":           true,
	"sso-directory": true,
	"identitystore": true,
}

// exposureEvents lists only the writes that change what can be reached
// whatever their parameters; a write that exposes something only with
// some parameters (a grant, a queue attribute, a database setting) is
// left out rather than shown as exposure every time.
//
// exposureEvents are the writes, by event source, that change what can be
// reached: network rules and routes to the internet, public access and
// resource policies, listeners, public endpoints and function URLs.
var exposureEvents = map[string]map[string]bool{
	"ec2": set("AuthorizeSecurityGroupIngress", "RevokeSecurityGroupIngress", "AuthorizeSecurityGroupEgress",
		"RevokeSecurityGroupEgress", "ModifySecurityGroupRules", "UpdateSecurityGroupRuleDescriptionsIngress",
		"CreateNetworkAclEntry", "ReplaceNetworkAclEntry", "DeleteNetworkAclEntry", "AttachInternetGateway",
		"CreateRoute", "ReplaceRoute", "AssociateAddress", "ModifyNetworkInterfaceAttribute",
		"ModifySnapshotAttribute", "ModifyImageAttribute", "DisableSnapshotBlockPublicAccess", "DisableImageBlockPublicAccess",
		"ModifyVpcEndpointServicePermissions"),
	"s3": set("PutBucketPolicy", "DeleteBucketPolicy", "PutBucketAcl", "PutObjectAcl", "PutBucketPublicAccessBlock",
		"DeleteBucketPublicAccessBlock", "PutPublicAccessBlock", "DeletePublicAccessBlock", "PutBucketWebsite",
		"PutAccountPublicAccessBlock", "DeleteAccountPublicAccessBlock", "PutBucketOwnershipControls", "PutAccessPointPolicy"),
	"elasticloadbalancing": set("CreateListener", "ModifyListener", "CreateLoadBalancer", "SetSecurityGroups", "SetSubnets"),
	"lambda": set("AddPermission", "RemovePermission", "AddLayerVersionPermission", "RemoveLayerVersionPermission", "CreateFunctionUrlConfig",
		"UpdateFunctionUrlConfig", "DeleteFunctionUrlConfig"),
	"kms":            set("PutKeyPolicy"),
	"ecr":            set("SetRepositoryPolicy", "DeleteRepositoryPolicy"),
	"sns":            set("SetTopicAttributes", "AddPermission"),
	"sqs":            set("AddPermission"),
	"secretsmanager": set("PutResourcePolicy", "DeleteResourcePolicy"),
	"eks":            set("UpdateClusterConfig", "CreateAccessEntry", "AssociateAccessPolicy"),
	"rds":            set("ModifyDBSnapshotAttribute", "ModifyDBClusterSnapshotAttribute"),
	"apigateway":     set("CreateRestApi", "UpdateRestApiPolicy"),
	"cloudfront":     set("CreateDistribution", "CreateDistributionWithTags", "UpdateDistribution"),
	"ram":            set("CreateResourceShare", "AssociateResourceShare", "UpdateResourceShare"),
}

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// sourceOfService maps the change list's service IDs that differ from the
// event source's first label back to it.
var sourceOfService = map[string]string{
	"elbv2":           "elasticloadbalancing",
	"secrets-manager": "secretsmanager",
	"api-gateway":     "apigateway",
}

// ConcernOf classifies a change by its event name, in the change list's
// "source:Action" form ("iam:CreateUser") or as the bare action with the
// change's service ID ("iam", "elbv2"). It returns "" for a change that
// touches neither exposure nor identity.
func ConcernOf(service, eventName string) Concern {
	source, action, ok := strings.Cut(eventName, ":")
	if !ok {
		source, action = service, eventName
		if mapped, known := sourceOfService[service]; known {
			source = mapped
		}
	}
	if source == "" || action == "" {
		return ""
	}
	if identitySources[source] {
		return ConcernIdentity
	}
	if exposureEvents[source][baseAction(action)] {
		return ConcernExposure
	}
	return ""
}

// baseAction drops the API version CloudTrail appends to some event names:
// Lambda's AddPermission20150331v2 and CloudFront's
// UpdateDistribution2020_05_31 read as AddPermission and
// UpdateDistribution.
func baseAction(action string) string {
	end := len(action)
	for end > 0 && (action[end-1] >= '0' && action[end-1] <= '9' || action[end-1] == '_' || action[end-1] == 'v') {
		end--
	}
	if end < len(action)-4 {
		return BaseEventName(action[:end])
	}
	return BaseEventName(action)
}
