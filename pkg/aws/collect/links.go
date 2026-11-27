package collect

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/ram"
	ramtypes "github.com/aws/aws-sdk-go-v2/service/ram/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// The reads behind cross-account links: transit gateway attachments, DNS
// records that point at load balancers and distributions, resource shares,
// the organization an account belongs to, and the resource policies of
// buckets, keys and repositories. Each is a list or describe call; policy
// documents and record values are reduced before anything is stored.

// Fan-out caps of the link reads.
const (
	maxHostedZones  = 100
	maxRecordPages  = 20
	maxKeys         = 100
	maxRepositories = 200
)

// Unit services of the link reads that have no probe row of their own.
const (
	serviceTransitGateway = "transit-gateway"
	serviceRAM            = "ram"
	serviceOrganizations  = "organizations"
	serviceKMS            = "kms"
	serviceECR            = "ecr"
	serviceS3Policy       = model.PolicyEntryService
)

// AWS operations relations are read from, named in the relation's Source.
const (
	sourceRecordSets  = "route53:ListResourceRecordSets"
	sourceAttachments = "ec2:DescribeTransitGatewayAttachments"
	sourceShares      = "ram:GetResourceShareAssociations"
	sourceShared      = "ram:ListResources"
	sourceTrust       = "iam:ListRoles"
	sourceBucket      = "s3:GetBucketPolicy"
	sourceKey         = "kms:GetKeyPolicy"
	sourceRepository  = "ecr:GetRepositoryPolicy"
)

// ---- Route 53 records ----

// dnsTargetHost reports whether a host name is one a record may link to: a
// load balancer or a CloudFront distribution. Every other record value is
// dropped, never stored.
func dnsTargetHost(name string) bool {
	return strings.HasSuffix(name, ".elb.amazonaws.com") || strings.HasSuffix(name, ".elb.amazonaws.com.cn") ||
		strings.HasSuffix(name, ".cloudfront.net") || (strings.Contains(name, ".elb.") && strings.HasSuffix(name, ".amazonaws.com"))
}

// recordName is the record name as a reader writes it: Route 53 answers a
// wildcard label octal-escaped and every name with a trailing dot.
func recordName(name string) string {
	name = strings.ReplaceAll(name, `\052`, "*")
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

func route53Client(c *call) (*route53.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("route53", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return route53.NewFromConfig(c.env.config(c.endpointRegion), func(options *route53.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// ---- Transit gateways ----

// collectTransitGateways lists the transit gateways the account owns or is
// shared, and the attachments it can see: every one on the owner, its own
// on a spoke. An attachment is keyed by the account that owns the attached
// network, so the owner's and the spoke's reads of it are one resource.
func collectTransitGateways(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := ec2Client(c)
	if err != nil {
		return out, err
	}
	var token *string
	for {
		page, err := api.DescribeTransitGateways(ctx, &ec2.DescribeTransitGatewaysInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return out, err
		}
		for _, gateway := range page.TransitGateways {
			id, owner := deref(gateway.TransitGatewayId), deref(gateway.OwnerId)
			arn := deref(gateway.TransitGatewayArn)
			if arn == "" {
				arn = model.ARN(c.partition, "ec2", c.unit.Region, owner, "transit-gateway/"+id)
			}
			resource := c.newResource(arn, "transit-gateway", id, string(gateway.State))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrOwnerAccount: owner}
			if owner == c.unit.Account {
				c.applyTags(&resource, ec2Tags(gateway.Tags))
			}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	token = nil
	for {
		c.listed("DescribeTransitGatewayAttachments")
		page, err := api.DescribeTransitGatewayAttachments(ctx, &ec2.DescribeTransitGatewayAttachmentsInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("DescribeTransitGatewayAttachments", err))
			break
		}
		for _, attachment := range page.TransitGatewayAttachments {
			id := deref(attachment.TransitGatewayAttachmentId)
			networkOwner, gatewayOwner := deref(attachment.ResourceOwnerId), deref(attachment.TransitGatewayOwnerId)
			if networkOwner == "" {
				networkOwner = c.unit.Account
			}
			if gatewayOwner == "" {
				gatewayOwner = networkOwner
			}
			region := c.unit.Region
			resource := c.newResource(model.ARN(c.partition, "ec2", region, networkOwner, "transit-gateway-attachment/"+id),
				"transit-gateway-attachment", id, string(attachment.State))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrOwnerAccount: networkOwner}
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedToTransit,
				Target: model.ARN(c.partition, "ec2", region, gatewayOwner, "transit-gateway/"+deref(attachment.TransitGatewayId)),
				Kind:   model.RelationConfigured, Source: sourceAttachments})
			if network := deref(attachment.ResourceId); network != "" {
				var target string
				switch attachment.ResourceType {
				case "vpc":
					target = model.ARN(c.partition, "ec2", region, networkOwner, "vpc/"+network)
				case "peering":
					target = model.ARN(c.partition, "ec2", region, networkOwner, "transit-gateway/"+network)
				case "vpn":
					target = model.ARN(c.partition, "ec2", region, networkOwner, "vpn-connection/"+network)
				}
				if target != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: target,
						Kind: model.RelationConfigured, Source: sourceAttachments})
				}
			}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

// ---- Resource shares ----

func ramClient(c *call) (*ram.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("ram", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return ram.NewFromConfig(c.env.config(c.endpointRegion), func(options *ram.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// shareTarget is the relation target of a share principal: an account ID
// stays, an organization or organizational unit becomes the organization.
func shareTarget(principal string) string {
	if accountIDPattern.MatchString(principal) {
		return principal
	}
	if org := orgIDPattern.FindString(principal); org != "" && strings.Contains(principal, ":organizations:") {
		return model.OrgTarget(org)
	}
	return principal
}

// collectRAM reads the resource shares the account owns, with who they are
// shared with, and the shares it receives. Three calls, no share names: a
// share is linked by its ARN, which names its owner.
func collectRAM(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := ramClient(c)
	if err != nil {
		return out, err
	}
	shares := map[string]*model.Resource{}
	var order []string
	share := func(arn string) *model.Resource {
		if resource, ok := shares[arn]; ok {
			return resource
		}
		owner := accountFromARN(arn)
		resource := c.newResource(arn, "resource-share", model.ResourceID(arn), "")
		resource.Attributes = map[string]string{model.AttrID: model.ResourceID(arn), model.AttrOwnerAccount: owner}
		if owner != c.unit.Account {
			// A received share: the account reads itself as a principal.
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSharedVia, Target: c.unit.Account,
				Kind: model.RelationConfigured, Source: sourceShared})
		}
		shares[arn] = &resource
		order = append(order, arn)
		return &resource
	}
	for _, owner := range []ramtypes.ResourceOwner{ramtypes.ResourceOwnerSelf, ramtypes.ResourceOwnerOtherAccounts} {
		var token *string
		for {
			page, err := api.ListResources(ctx, &ram.ListResourcesInput{ResourceOwner: owner, MaxResults: aws.Int32(500), NextToken: token})
			if err != nil {
				return out, err
			}
			for _, item := range page.Resources {
				arn := deref(item.ResourceShareArn)
				if arn == "" {
					continue
				}
				holder := share(arn)
				if target := deref(item.Arn); target != "" {
					holder.Relations = append(holder.Relations, model.Relation{Type: model.RelationAssociated, Target: target,
						Kind: model.RelationConfigured, Source: sourceShared})
				}
			}
			if token = page.NextToken; deref(token) == "" {
				break
			}
		}
	}
	var token *string
	for {
		c.listed("GetResourceShareAssociations")
		page, err := api.GetResourceShareAssociations(ctx, &ram.GetResourceShareAssociationsInput{
			AssociationType: ramtypes.ResourceShareAssociationTypePrincipal, MaxResults: aws.Int32(500), NextToken: token})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("GetResourceShareAssociations", err))
			break
		}
		for _, association := range page.ResourceShareAssociations {
			arn, principal := deref(association.ResourceShareArn), deref(association.AssociatedEntity)
			if arn == "" || principal == "" || accountFromARN(arn) != c.unit.Account {
				continue
			}
			holder := share(arn)
			holder.Relations = append(holder.Relations, model.Relation{Type: model.RelationSharedVia, Target: shareTarget(principal),
				Kind: model.RelationConfigured, Source: sourceShares})
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	for _, arn := range order {
		out.resources = append(out.resources, *shares[arn])
	}
	return out, nil
}

// accountFromARN is the account field of an ARN, empty when it has none.
func accountFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || !accountIDPattern.MatchString(parts[4]) {
		return ""
	}
	return parts[4]
}

// ---- Organization ----

// collectOrganization reads the organization the account belongs to: its
// ID and its management account ID, nothing else. An account outside any
// organization answers "not in use", which is a successful empty read.
func collectOrganization(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("organizations", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := organizations.NewFromConfig(c.env.config(c.endpointRegion), func(options *organizations.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	answer, err := api.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if notSetAnswer(err) {
		return result{}, nil
	}
	if err != nil {
		return result{}, err
	}
	if answer.Organization == nil || deref(answer.Organization.Id) == "" {
		return result{}, nil
	}
	id, management := deref(answer.Organization.Id), deref(answer.Organization.MasterAccountId)
	arn := deref(answer.Organization.Arn)
	if arn == "" {
		arn = model.ARN(c.partition, "organizations", "", management, "organization/"+id)
	}
	resource := c.newResource(arn, "organization", id, "")
	resource.Attributes = map[string]string{model.AttrID: id, model.AttrOrganizationID: id}
	if accountIDPattern.MatchString(management) {
		resource.Attributes[model.AttrManagementAccount] = management
	}
	return result{resources: []model.Resource{resource}}, nil
}

// ---- Resource policies ----

// collectBucketPolicies reads each bucket's policy and keeps the grants to
// other principals as one policy entry per bucket; the document itself is
// never stored. Bucket policy reads are metered, so this unit runs on the
// policy cadence, not with the inventory.
func collectBucketPolicies(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := s3Client(c, "")
	if err != nil {
		return out, err
	}
	type bucket struct{ name, region string }
	var buckets []bucket
	var token *string
	for {
		page, err := api.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1000), ContinuationToken: token})
		if err != nil {
			return out, err
		}
		for _, item := range page.Buckets {
			buckets = append(buckets, bucket{name: deref(item.Name), region: deref(item.BucketRegion)})
		}
		if token = page.ContinuationToken; deref(token) == "" {
			break
		}
	}
	for index, item := range buckets {
		if index >= maxBucketDetails {
			out.gaps = append(out.gaps, c.capGap("GetBucketPolicy", maxBucketDetails, len(buckets)))
			break
		}
		c.tried("GetBucketPolicy", model.ARN(c.partition, "s3", "", "", item.name))
		detail, err := s3Client(c, item.region)
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("GetBucketPolicy", err))
			continue
		}
		policy, err := detail.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(item.name)})
		switch {
		case notSetAnswer(err):
			continue
		case err != nil:
			out.gaps = append(out.gaps, c.detailGap("GetBucketPolicy", err))
			continue
		}
		relations := grantRelations(model.RelationMayAccess, sourceBucket, reducePolicy(deref(policy.Policy), c.unit.Account, item.name, nil))
		if len(relations) == 0 {
			continue
		}
		subject := model.ARN(c.partition, "s3", "", "", item.name)
		resource := c.newResource(subject+"#policy", model.ResourcePolicyType, item.name, "")
		resource.Attributes = map[string]string{model.AttrSubject: subject}
		if item.region != "" {
			resource.Region = item.region
		}
		resource.Relations = relations
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

func kmsClient(c *call) (*kms.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("kms", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return kms.NewFromConfig(c.env.config(c.endpointRegion), func(options *kms.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// collectKMS lists keys and reads each key's policy: one call per key, the
// fewest that read every policy. An AWS managed key's policy lets in only
// its own account, so it reduces to no grant. Key requests are metered, so
// this unit runs on the policy cadence.
func collectKMS(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := kmsClient(c)
	if err != nil {
		return out, err
	}
	var keys []kmstypes.KeyListEntry
	var marker *string
	for {
		page, err := api.ListKeys(ctx, &kms.ListKeysInput{Limit: aws.Int32(1000), Marker: marker})
		if err != nil {
			return out, err
		}
		keys = append(keys, page.Keys...)
		if marker = page.NextMarker; !page.Truncated || deref(marker) == "" {
			break
		}
	}
	for index, key := range keys {
		id := deref(key.KeyId)
		arn := deref(key.KeyArn)
		if arn == "" {
			arn = c.arn("kms", "key/"+id)
		}
		resource := c.newResource(arn, "key", id, "")
		resource.Attributes = map[string]string{model.AttrID: id}
		if index >= maxKeys {
			if index == maxKeys {
				out.gaps = append(out.gaps, c.capGap("GetKeyPolicy", maxKeys, len(keys)))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		c.tried("GetKeyPolicy", arn)
		policy, err := api.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{KeyId: aws.String(id), PolicyName: aws.String("default")})
		switch {
		case notSetAnswer(err), apiErrorCode(err) == "NotFoundException":
			// No policy, or a key deleted since it was listed.
		case err != nil:
			out.gaps = append(out.gaps, c.detailGap("GetKeyPolicy", err))
		default:
			resource.Relations = grantRelations(model.RelationMayAccess, sourceKey, reducePolicy(deref(policy.Policy), c.unit.Account, "key/"+id, nil))
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

func ecrClient(c *call) (*ecr.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("ecr", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return ecr.NewFromConfig(c.env.config(c.endpointRegion), func(options *ecr.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// collectECR lists repositories and reads each repository's policy.
func collectECR(ctx context.Context, c *call) (result, error) {
	var out result
	api, err := ecrClient(c)
	if err != nil {
		return out, err
	}
	type repository struct{ arn, name string }
	var repositories []repository
	var token *string
	for {
		page, err := api.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return out, err
		}
		for _, item := range page.Repositories {
			repositories = append(repositories, repository{arn: deref(item.RepositoryArn), name: deref(item.RepositoryName)})
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	for index, item := range repositories {
		arn := item.arn
		if arn == "" {
			arn = c.arn("ecr", "repository/"+item.name)
		}
		resource := c.newResource(arn, "repository", item.name, "")
		resource.Attributes = map[string]string{model.AttrID: item.name}
		if index >= maxRepositories {
			if index == maxRepositories {
				out.gaps = append(out.gaps, c.capGap("GetRepositoryPolicy", maxRepositories, len(repositories)))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		c.tried("GetRepositoryPolicy", arn)
		policy, err := api.GetRepositoryPolicy(ctx, &ecr.GetRepositoryPolicyInput{RepositoryName: aws.String(item.name)})
		switch {
		case notSetAnswer(err):
		case err != nil:
			out.gaps = append(out.gaps, c.detailGap("GetRepositoryPolicy", err))
		default:
			resource.Relations = grantRelations(model.RelationMayAccess, sourceRepository,
				reducePolicy(deref(policy.PolicyText), c.unit.Account, "repository/"+item.name, nil))
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}
