package collect

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// s3Client builds an S3 client pinned to the global endpoint, or to a
// bucket's regional endpoint in path style so the pinned host never changes
// with the bucket name.
func s3Client(c *call, bucketRegion string) (*s3.Client, error) {
	client, signing := "s3-global", c.endpointRegion
	if bucketRegion != "" && awsauth.ValidateRegion(bucketRegion) == nil {
		client, signing = "s3", bucketRegion
	}
	endpoint, transport, stack, err := c.env.endpoint(client, bucketRegion)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(c.env.config(signing), func(options *s3.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = transport
		options.RetryMaxAttempts = 1
		options.UsePathStyle = true
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// collectS3 lists buckets and, for the first buckets, the three metadata
// reads behind the public-exposure and encryption flags. Object contents and
// policy documents are never read.
func collectS3(ctx context.Context, c *call) (result, error) {
	api, err := s3Client(c, "")
	if err != nil {
		return result{}, err
	}
	var out result
	var token *string
	type bucket struct{ name, region string }
	var buckets []bucket
	for {
		page, err := api.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1000), ContinuationToken: token})
		if err != nil {
			return result{}, err
		}
		for _, item := range page.Buckets {
			buckets = append(buckets, bucket{name: deref(item.Name), region: deref(item.BucketRegion)})
		}
		if token = page.ContinuationToken; deref(token) == "" {
			break
		}
	}
	for index, item := range buckets {
		resource := c.newResource(model.ARN(c.partition, "s3", "", "", item.name), "bucket", item.name, "")
		resource.Attributes = map[string]string{model.AttrID: item.name, model.AttrBucketRegion: item.region}
		if item.region != "" {
			resource.Region = item.region
		}
		if index >= maxBucketDetails {
			if index == maxBucketDetails {
				out.gaps = append(out.gaps, c.capGap("GetPublicAccessBlock"))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		detail, err := s3Client(c, item.region)
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("GetPublicAccessBlock", err))
			out.resources = append(out.resources, resource)
			continue
		}
		flag := func(kind model.SecurityFlagKind, summary string) {
			flagged := c.flag(resource.ARN, kind, summary)
			flagged.Region = resource.Region
			out.flags = append(out.flags, flagged)
		}

		blocked := false
		block, err := detail.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String(item.name)})
		switch {
		case err == nil && block.PublicAccessBlockConfiguration != nil:
			settings := block.PublicAccessBlockConfiguration
			blocked = derefBool(settings.BlockPublicAcls) && derefBool(settings.IgnorePublicAcls) &&
				derefBool(settings.BlockPublicPolicy) && derefBool(settings.RestrictPublicBuckets)
		case err != nil && apiErrorCode(err) == "NoSuchPublicAccessBlockConfiguration":
		case err != nil:
			out.gaps = append(out.gaps, c.detailGap("GetPublicAccessBlock", err))
			out.resources = append(out.resources, resource)
			continue
		}

		public := false
		status, err := detail.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String(item.name)})
		switch {
		case err == nil && status.PolicyStatus != nil:
			public = derefBool(status.PolicyStatus.IsPublic)
		case err != nil && apiErrorCode(err) == "NoSuchBucketPolicy":
		case err != nil:
			out.gaps = append(out.gaps, c.detailGap("GetBucketPolicyStatus", err))
		}
		switch {
		case public:
			flag(model.FlagPublicExposure, "Bucket policy makes the bucket public")
		case !blocked:
			flag(model.FlagPublicExposure, "Public access is not fully blocked")
		}

		if _, err := detail.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String(item.name)}); err != nil {
			if apiErrorCode(err) == "ServerSideEncryptionConfigurationNotFoundError" {
				flag(model.FlagEncryptionOff, "Bucket has no default encryption")
			} else {
				out.gaps = append(out.gaps, c.detailGap("GetBucketEncryption", err))
			}
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

func collectCloudFront(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("cloudfront", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := cloudfront.NewFromConfig(c.env.config(c.endpointRegion), func(options *cloudfront.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var marker *string
	for {
		page, err := api.ListDistributions(ctx, &cloudfront.ListDistributionsInput{MaxItems: aws.Int32(100), Marker: marker})
		if err != nil {
			return result{}, err
		}
		list := page.DistributionList
		if list == nil {
			break
		}
		for _, distribution := range list.Items {
			id := deref(distribution.Id)
			arn := deref(distribution.ARN)
			if arn == "" {
				arn = c.arn("cloudfront", "distribution/"+id)
			}
			resource := c.newResource(arn, "distribution", deref(distribution.DomainName), deref(distribution.Status))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrDNSName: deref(distribution.DomainName)}
			if distribution.Origins != nil {
				for _, origin := range distribution.Origins.Items {
					if domain := deref(origin.DomainName); domain != "" {
						resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationOrigin, Target: domain})
					}
				}
			}
			out.resources = append(out.resources, resource)
		}
		if marker = list.NextMarker; !derefBool(list.IsTruncated) || deref(marker) == "" {
			break
		}
	}
	return out, nil
}

func collectRoute53(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("route53", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := route53.NewFromConfig(c.env.config(c.endpointRegion), func(options *route53.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var marker *string
	for {
		page, err := api.ListHealthChecks(ctx, &route53.ListHealthChecksInput{MaxItems: aws.Int32(100), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, check := range page.HealthChecks {
			id := deref(check.Id)
			name := id
			if check.HealthCheckConfig != nil && deref(check.HealthCheckConfig.FullyQualifiedDomainName) != "" {
				name = deref(check.HealthCheckConfig.FullyQualifiedDomainName)
			}
			resource := c.newResource(model.ARN(c.partition, "route53", "", "", "healthcheck/"+id), "health-check", name, "")
			resource.Attributes = map[string]string{model.AttrID: id}
			out.resources = append(out.resources, resource)
		}
		if marker = page.NextMarker; !page.IsTruncated || deref(marker) == "" {
			break
		}
	}
	return out, nil
}

// collectIAM lists roles by name and path only; policies are never read.
func collectIAM(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("iam", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := iam.NewFromConfig(c.env.config(c.endpointRegion), func(options *iam.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var marker *string
	for {
		page, err := api.ListRoles(ctx, &iam.ListRolesInput{MaxItems: aws.Int32(1000), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, role := range page.Roles {
			name := deref(role.RoleName)
			resource := c.newResource(deref(role.Arn), "role", name, "")
			resource.Attributes = map[string]string{model.AttrID: name}
			out.resources = append(out.resources, resource)
		}
		if marker = page.Marker; !page.IsTruncated || deref(marker) == "" {
			break
		}
	}
	return out, nil
}
