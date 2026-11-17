package collect

import (
	"context"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// collectECS lists clusters and their services. A service's primary
// deployment gives its rollout state, which becomes a health signal while a
// rollout is in progress or has failed.
func collectECS(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("ecs", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := ecs.NewFromConfig(c.env.config(c.endpointRegion), func(options *ecs.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})

	var clusters []string
	var token *string
	for {
		page, err := api.ListClusters(ctx, &ecs.ListClustersInput{MaxResults: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		clusters = append(clusters, page.ClusterArns...)
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	var out result
	if len(clusters) > maxClusters {
		out.gaps = append(out.gaps, c.capGap("DescribeServices"))
		clusters = clusters[:maxClusters]
	}
	if len(clusters) == 0 {
		return out, nil
	}

	described, err := api.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: clusters, Include: []ecstypes.ClusterField{ecstypes.ClusterFieldTags}})
	if err != nil {
		return result{}, err
	}
	for _, cluster := range described.Clusters {
		resource := c.newResource(deref(cluster.ClusterArn), "cluster", deref(cluster.ClusterName), strings.ToLower(deref(cluster.Status)))
		resource.Attributes = map[string]string{model.AttrID: deref(cluster.ClusterName)}
		c.applyTags(&resource, ecsTags(cluster.Tags))
		out.resources = append(out.resources, resource)
	}

	for _, cluster := range clusters {
		var serviceARNs []string
		token = nil
		for {
			page, err := api.ListServices(ctx, &ecs.ListServicesInput{Cluster: aws.String(cluster), MaxResults: aws.Int32(100), NextToken: token})
			if err != nil {
				return result{}, err
			}
			serviceARNs = append(serviceARNs, page.ServiceArns...)
			if token = page.NextToken; deref(token) == "" {
				break
			}
		}
		for start := 0; start < len(serviceARNs); start += maxServicesPerCall {
			end := min(start+maxServicesPerCall, len(serviceARNs))
			page, err := api.DescribeServices(ctx, &ecs.DescribeServicesInput{
				Cluster:  aws.String(cluster),
				Services: serviceARNs[start:end],
				Include:  []ecstypes.ServiceField{ecstypes.ServiceFieldTags},
			})
			if err != nil {
				return result{}, err
			}
			for _, service := range page.Services {
				out.resources = append(out.resources, c.ecsService(cluster, service, &out))
			}
		}
	}
	return out, nil
}

func ecsTags(tags []ecstypes.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if key := deref(tag.Key); key != "" {
			out[key] = deref(tag.Value)
		}
	}
	return out
}

// taskDefinitionLabel turns a task definition ARN into family:revision.
func taskDefinitionLabel(arn string) string {
	return lastSegment(arn, "/")
}

func (c *call) ecsService(cluster string, service ecstypes.Service, out *result) model.Resource {
	arn := deref(service.ServiceArn)
	name := deref(service.ServiceName)
	resource := c.newResource(arn, "service", name, strings.ToLower(deref(service.Status)))
	resource.Attributes = map[string]string{
		model.AttrID:      name,
		model.AttrTaskDef: taskDefinitionLabel(deref(service.TaskDefinition)),
		model.AttrDesired: strconv.Itoa(int(service.DesiredCount)),
		model.AttrRunning: strconv.Itoa(int(service.RunningCount)),
	}
	resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationMemberOf, Target: cluster})
	for _, balancer := range service.LoadBalancers {
		if targetGroup := deref(balancer.TargetGroupArn); targetGroup != "" {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: targetGroup})
		}
	}
	if service.NetworkConfiguration != nil && service.NetworkConfiguration.AwsvpcConfiguration != nil {
		for _, subnet := range service.NetworkConfiguration.AwsvpcConfiguration.Subnets {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
		}
		for _, group := range service.NetworkConfiguration.AwsvpcConfiguration.SecurityGroups {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", group)})
		}
	}
	c.applyTags(&resource, ecsTags(service.Tags))

	for _, deployment := range service.Deployments {
		if deref(deployment.Status) != "PRIMARY" {
			continue
		}
		label := taskDefinitionLabel(deref(deployment.TaskDefinition))
		resource.Attributes[model.AttrRollout] = strings.ToLower(string(deployment.RolloutState))
		since := derefTime(deployment.UpdatedAt)
		if since.IsZero() {
			since = derefTime(deployment.CreatedAt)
		}
		progress := strconv.Itoa(int(deployment.RunningCount)) + " of " + strconv.Itoa(int(deployment.DesiredCount)) + " tasks running"
		switch deployment.RolloutState {
		case ecstypes.DeploymentRolloutStateFailed:
			out.health = append(out.health, c.healthItem(arn, model.SourceRollout, model.HealthCritical,
				"Rollout of "+label+" failed ("+progress+")", since, deref(deployment.Id)))
		case ecstypes.DeploymentRolloutStateInProgress:
			out.health = append(out.health, c.healthItem(arn, model.SourceRollout, model.HealthWarning,
				"Rollout of "+label+" in progress ("+progress+")", since, deref(deployment.Id)))
		default:
			if deployment.RunningCount < deployment.DesiredCount {
				out.health = append(out.health, c.healthItem(arn, model.SourceRollout, model.HealthWarning,
					"Fewer tasks running than desired ("+progress+")", since, deref(deployment.Id)))
			}
		}
	}
	return resource
}

// collectEKS lists clusters and node groups, metadata only.
func collectEKS(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("eks", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := eks.NewFromConfig(c.env.config(c.endpointRegion), func(options *eks.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var names []string
	var token *string
	for {
		page, err := api.ListClusters(ctx, &eks.ListClustersInput{MaxResults: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		names = append(names, page.Clusters...)
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	var out result
	if len(names) > maxClusters {
		out.gaps = append(out.gaps, c.capGap("DescribeCluster"))
		names = names[:maxClusters]
	}
	nodegroups := 0
	for _, name := range names {
		described, err := api.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("DescribeCluster", err))
			continue
		}
		cluster := described.Cluster
		if cluster == nil {
			continue
		}
		arn := deref(cluster.Arn)
		if arn == "" {
			arn = c.arn("eks", "cluster/"+name)
		}
		resource := c.newResource(arn, "cluster", name, strings.ToLower(string(cluster.Status)))
		resource.Attributes = map[string]string{model.AttrID: name, model.AttrEngine: "kubernetes " + deref(cluster.Version)}
		if vpc := cluster.ResourcesVpcConfig; vpc != nil {
			if id := deref(vpc.VpcId); id != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", id)})
				resource.Attributes[model.AttrVPC] = id
			}
			for _, subnet := range vpc.SubnetIds {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
			}
			for _, group := range vpc.SecurityGroupIds {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", group)})
			}
			if vpc.EndpointPublicAccess {
				for _, cidr := range vpc.PublicAccessCidrs {
					if cidr == "0.0.0.0/0" {
						out.flags = append(out.flags, c.flag(arn, model.FlagPublicExposure, "Cluster API endpoint is reachable from the internet"))
						break
					}
				}
			}
		}
		c.applyTags(&resource, cluster.Tags)
		out.resources = append(out.resources, resource)

		token = nil
		for {
			page, err := api.ListNodegroups(ctx, &eks.ListNodegroupsInput{ClusterName: aws.String(name), MaxResults: aws.Int32(100), NextToken: token})
			if err != nil {
				out.gaps = append(out.gaps, c.detailGap("ListNodegroups", err))
				break
			}
			for _, group := range page.Nodegroups {
				if nodegroups >= maxNodegroups {
					out.gaps = append(out.gaps, c.capGap("DescribeNodegroup"))
					break
				}
				nodegroups++
				detail, err := api.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(name), NodegroupName: aws.String(group)})
				if err != nil || detail.Nodegroup == nil {
					out.gaps = append(out.gaps, c.detailGap("DescribeNodegroup", err))
					continue
				}
				nodegroup := detail.Nodegroup
				member := c.newResource(deref(nodegroup.NodegroupArn), "node-group", group, strings.ToLower(string(nodegroup.Status)))
				member.Attributes = map[string]string{model.AttrID: group}
				if scaling := nodegroup.ScalingConfig; scaling != nil {
					member.Attributes[model.AttrDesired] = strconv.Itoa(int(derefInt32(scaling.DesiredSize)))
				}
				member.Relations = append(member.Relations, model.Relation{Type: model.RelationMemberOf, Target: arn})
				for _, subnet := range nodegroup.Subnets {
					member.Relations = append(member.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
				}
				c.applyTags(&member, nodegroup.Tags)
				if member.ARN != "" {
					out.resources = append(out.resources, member)
				}
			}
			if token = page.NextToken; deref(token) == "" {
				break
			}
		}
	}
	return out, nil
}

// collectLambda lists functions and their event source mappings. Function
// environment variables are never read into the model.
func collectLambda(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("lambda", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := lambda.NewFromConfig(c.env.config(c.endpointRegion), func(options *lambda.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	byARN := make(map[string]int)
	var marker *string
	for {
		page, err := api.ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(50), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, function := range page.Functions {
			arn := deref(function.FunctionArn)
			name := deref(function.FunctionName)
			resource := c.newResource(arn, "function", name, strings.ToLower(string(function.State)))
			resource.Attributes = map[string]string{model.AttrID: name, model.AttrRuntime: string(function.Runtime)}
			if vpc := function.VpcConfig; vpc != nil {
				if id := deref(vpc.VpcId); id != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", id)})
				}
				for _, subnet := range vpc.SubnetIds {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
				}
				for _, group := range vpc.SecurityGroupIds {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", group)})
				}
			}
			if function.DeadLetterConfig != nil && deref(function.DeadLetterConfig.TargetArn) != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationDeadLetter, Target: deref(function.DeadLetterConfig.TargetArn)})
			}
			byARN[arn] = len(out.resources)
			out.resources = append(out.resources, resource)
		}
		if marker = page.NextMarker; deref(marker) == "" {
			break
		}
	}

	marker = nil
	for {
		page, err := api.ListEventSourceMappings(ctx, &lambda.ListEventSourceMappingsInput{MaxItems: aws.Int32(100), Marker: marker})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("ListEventSourceMappings", err))
			break
		}
		for _, mapping := range page.EventSourceMappings {
			function := deref(mapping.FunctionArn)
			// A mapping may name a version or alias; relate it to the base function.
			if parts := strings.Split(function, ":"); len(parts) > 7 {
				function = strings.Join(parts[:7], ":")
			}
			if position, ok := byARN[function]; ok && deref(mapping.EventSourceArn) != "" {
				out.resources[position].Relations = append(out.resources[position].Relations,
					model.Relation{Type: model.RelationTriggeredBy, Target: deref(mapping.EventSourceArn)})
			}
		}
		if marker = page.NextMarker; deref(marker) == "" {
			break
		}
	}
	return out, nil
}
