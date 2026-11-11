package collect

import (
	"context"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func ec2Client(c *call) (*ec2.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("ec2", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return ec2.NewFromConfig(c.env.config(c.endpointRegion), func(options *ec2.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

func ec2Tags(tags []ec2types.Tag) map[string]string {
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

// nameTag is the Name tag value, used as the display name only while the
// Name tag is allowlisted; otherwise, or without one, the fallback ID.
func (c *call) nameTag(tags []ec2types.Tag, fallback string) string {
	if !c.allowlist[model.NameTagKey] {
		return fallback
	}
	for _, tag := range tags {
		if deref(tag.Key) == model.NameTagKey && deref(tag.Value) != "" {
			return deref(tag.Value)
		}
	}
	return fallback
}

func (c *call) ec2ARN(kind, id string) string {
	return c.arn("ec2", kind+"/"+id)
}

// collectEC2 lists instances and their status checks. A failed status read
// keeps the inventory and names the gap.
func collectEC2(ctx context.Context, c *call) (result, error) {
	api, err := ec2Client(c)
	if err != nil {
		return result{}, err
	}
	var out result
	var token *string
	for {
		page, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, reservation := range page.Reservations {
			for _, instance := range reservation.Instances {
				id := deref(instance.InstanceId)
				if id == "" {
					continue
				}
				state := ""
				if instance.State != nil {
					state = string(instance.State.Name)
				}
				resource := c.newResource(c.ec2ARN("instance", id), "instance", c.nameTag(instance.Tags, id), state)
				resource.Attributes = map[string]string{
					model.AttrID:   id,
					model.AttrVPC:  deref(instance.VpcId),
					model.AttrSize: string(instance.InstanceType),
				}
				if instance.Placement != nil {
					resource.Attributes[model.AttrZone] = deref(instance.Placement.AvailabilityZone)
				}
				if vpc := deref(instance.VpcId); vpc != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", vpc)})
				}
				if subnet := deref(instance.SubnetId); subnet != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
				}
				for _, group := range instance.SecurityGroups {
					if id := deref(group.GroupId); id != "" {
						resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", id)})
					}
				}
				c.applyTags(&resource, ec2Tags(instance.Tags))
				if deref(instance.PublicIpAddress) != "" {
					resource.Attributes[model.AttrPublicIP] = "yes"
					out.flags = append(out.flags, c.flag(resource.ARN, model.FlagPublicExposure, "Instance has a public IP address"))
				}
				out.resources = append(out.resources, resource)
			}
		}
		token = page.NextToken
		if deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("DescribeInstanceStatus", err))
			break
		}
		for _, status := range page.InstanceStatuses {
			arn := c.ec2ARN("instance", deref(status.InstanceId))
			system := status.SystemStatus != nil && status.SystemStatus.Status == ec2types.SummaryStatusImpaired
			instance := status.InstanceStatus != nil && status.InstanceStatus.Status == ec2types.SummaryStatusImpaired
			switch {
			case system && instance:
				out.health = append(out.health, c.healthItem(arn, model.SourceInstanceStatus, model.HealthCritical, "System and instance status checks failing", unknownSince, ""))
			case system:
				out.health = append(out.health, c.healthItem(arn, model.SourceInstanceStatus, model.HealthCritical, "System status check failing", unknownSince, ""))
			case instance:
				out.health = append(out.health, c.healthItem(arn, model.SourceInstanceStatus, model.HealthCritical, "Instance status check failing", unknownSince, ""))
			}
			for _, event := range status.Events {
				out.health = append(out.health, c.healthItem(arn, model.SourceInstanceStatus, model.HealthWarning,
					"Scheduled event: "+strings.ReplaceAll(string(event.Code), "-", " "), derefTime(event.NotBefore), ""))
			}
		}
		token = page.NextToken
		if deref(token) == "" {
			break
		}
	}
	return out, nil
}

// adminPorts are the ports a world-open ingress rule must not expose.
var adminPorts = []int32{22, 3389}

func openToWorld(permission ec2types.IpPermission) bool {
	for _, ipRange := range permission.IpRanges {
		if deref(ipRange.CidrIp) == "0.0.0.0/0" {
			return true
		}
	}
	for _, ipRange := range permission.Ipv6Ranges {
		if deref(ipRange.CidrIpv6) == "::/0" {
			return true
		}
	}
	return false
}

func exposesAdminPort(permission ec2types.IpPermission) (int32, bool) {
	protocol := deref(permission.IpProtocol)
	if protocol != "tcp" && protocol != "-1" && protocol != "6" {
		return 0, false
	}
	if protocol == "-1" {
		return adminPorts[0], true
	}
	from, to := derefInt32(permission.FromPort), derefInt32(permission.ToPort)
	for _, port := range adminPorts {
		if from <= port && port <= to {
			return port, true
		}
	}
	return 0, false
}

// collectVPC lists the network: VPCs, subnets, route tables, gateways,
// security groups, network interfaces and endpoints.
func collectVPC(ctx context.Context, c *call) (result, error) {
	api, err := ec2Client(c)
	if err != nil {
		return result{}, err
	}
	var out result
	inVPC := func(resource *model.Resource, vpc string) {
		if vpc != "" {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", vpc)})
			if resource.Attributes == nil {
				resource.Attributes = map[string]string{}
			}
			resource.Attributes[model.AttrVPC] = vpc
		}
	}

	var token *string
	for {
		page, err := api.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, vpc := range page.Vpcs {
			id := deref(vpc.VpcId)
			resource := c.newResource(c.ec2ARN("vpc", id), "vpc", c.nameTag(vpc.Tags, id), string(vpc.State))
			resource.Attributes = map[string]string{model.AttrID: id}
			c.applyTags(&resource, ec2Tags(vpc.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, subnet := range page.Subnets {
			id := deref(subnet.SubnetId)
			resource := c.newResource(c.ec2ARN("subnet", id), "subnet", c.nameTag(subnet.Tags, id), string(subnet.State))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrZone: deref(subnet.AvailabilityZone)}
			inVPC(&resource, deref(subnet.VpcId))
			c.applyTags(&resource, ec2Tags(subnet.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{MaxResults: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, table := range page.RouteTables {
			id := deref(table.RouteTableId)
			resource := c.newResource(c.ec2ARN("route-table", id), "route-table", c.nameTag(table.Tags, id), "")
			resource.Attributes = map[string]string{model.AttrID: id}
			inVPC(&resource, deref(table.VpcId))
			for _, association := range table.Associations {
				if subnet := deref(association.SubnetId); subnet != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: c.ec2ARN("subnet", subnet)})
				}
			}
			for _, route := range table.Routes {
				if gateway := deref(route.GatewayId); strings.HasPrefix(gateway, "igw-") {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationRoutesTo, Target: c.ec2ARN("internet-gateway", gateway)})
				}
				if nat := deref(route.NatGatewayId); nat != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationRoutesTo, Target: c.ec2ARN("natgateway", nat)})
				}
			}
			c.applyTags(&resource, ec2Tags(table.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, gateway := range page.InternetGateways {
			id := deref(gateway.InternetGatewayId)
			resource := c.newResource(c.ec2ARN("internet-gateway", id), "internet-gateway", c.nameTag(gateway.Tags, id), "")
			resource.Attributes = map[string]string{model.AttrID: id}
			for _, attachment := range gateway.Attachments {
				if vpc := deref(attachment.VpcId); vpc != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: c.ec2ARN("vpc", vpc)})
				}
			}
			c.applyTags(&resource, ec2Tags(gateway.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, gateway := range page.NatGateways {
			id := deref(gateway.NatGatewayId)
			resource := c.newResource(c.ec2ARN("natgateway", id), "nat-gateway", c.nameTag(gateway.Tags, id), string(gateway.State))
			resource.Attributes = map[string]string{model.AttrID: id}
			inVPC(&resource, deref(gateway.VpcId))
			if subnet := deref(gateway.SubnetId); subnet != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
			}
			c.applyTags(&resource, ec2Tags(gateway.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, group := range page.SecurityGroups {
			id := deref(group.GroupId)
			resource := c.newResource(c.ec2ARN("security-group", id), "security-group", deref(group.GroupName), "")
			resource.Attributes = map[string]string{model.AttrID: id}
			inVPC(&resource, deref(group.VpcId))
			c.applyTags(&resource, ec2Tags(group.Tags))
			for _, permission := range group.IpPermissions {
				if !openToWorld(permission) {
					continue
				}
				if port, ok := exposesAdminPort(permission); ok {
					out.flags = append(out.flags, c.flag(resource.ARN, model.FlagAdminPortOpen, "Port "+strconv.Itoa(int(port))+" is open to the internet"))
					break
				}
			}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, networkInterface := range page.NetworkInterfaces {
			id := deref(networkInterface.NetworkInterfaceId)
			resource := c.newResource(c.ec2ARN("network-interface", id), "network-interface", id, string(networkInterface.Status))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrZone: deref(networkInterface.AvailabilityZone)}
			inVPC(&resource, deref(networkInterface.VpcId))
			if subnet := deref(networkInterface.SubnetId); subnet != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
			}
			for _, group := range networkInterface.Groups {
				if groupID := deref(group.GroupId); groupID != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", groupID)})
				}
			}
			if networkInterface.Attachment != nil {
				if instance := deref(networkInterface.Attachment.InstanceId); instance != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: c.ec2ARN("instance", instance)})
				}
			}
			if networkInterface.Association != nil && deref(networkInterface.Association.PublicIp) != "" {
				resource.Attributes[model.AttrPublicIP] = "yes"
			}
			c.applyTags(&resource, ec2Tags(networkInterface.TagSet))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	token = nil
	for {
		page, err := api.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, endpoint := range page.VpcEndpoints {
			id := deref(endpoint.VpcEndpointId)
			resource := c.newResource(c.ec2ARN("vpc-endpoint", id), "vpc-endpoint", deref(endpoint.ServiceName), string(endpoint.State))
			resource.Attributes = map[string]string{model.AttrID: id}
			inVPC(&resource, deref(endpoint.VpcId))
			for _, subnet := range endpoint.SubnetIds {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
			}
			c.applyTags(&resource, ec2Tags(endpoint.Tags))
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

func collectAutoScaling(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("autoscaling", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := autoscaling.NewFromConfig(c.env.config(c.endpointRegion), func(options *autoscaling.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{MaxRecords: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, group := range page.AutoScalingGroups {
			out.resources = append(out.resources, c.autoScalingGroup(group, &out))
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

func (c *call) autoScalingGroup(group autoscalingtypes.AutoScalingGroup, out *result) model.Resource {
	name := deref(group.AutoScalingGroupName)
	arn := deref(group.AutoScalingGroupARN)
	if arn == "" {
		arn = c.arn("autoscaling", "autoScalingGroup:*:autoScalingGroupName/"+name)
	}
	resource := c.newResource(arn, "auto-scaling-group", name, deref(group.Status))
	running := 0
	for _, instance := range group.Instances {
		id := deref(instance.InstanceId)
		resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationContains, Target: c.ec2ARN("instance", id)})
		if instance.LifecycleState == autoscalingtypes.LifecycleStateInService {
			running++
		}
		if strings.EqualFold(deref(instance.HealthStatus), "Unhealthy") {
			out.health = append(out.health, c.healthItem(c.ec2ARN("instance", id), model.SourceInstanceStatus, model.HealthWarning,
				"Auto Scaling marked the instance unhealthy", unknownSince, arn))
		}
	}
	for _, subnet := range strings.Split(deref(group.VPCZoneIdentifier), ",") {
		if subnet = strings.TrimSpace(subnet); subnet != "" {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
		}
	}
	for _, targetGroup := range group.TargetGroupARNs {
		resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAttachedTo, Target: targetGroup})
	}
	resource.Attributes = map[string]string{
		model.AttrID:      name,
		model.AttrDesired: strconv.Itoa(int(derefInt32(group.DesiredCapacity))),
		model.AttrRunning: strconv.Itoa(running),
	}
	tags := make(map[string]string, len(group.Tags))
	for _, tag := range group.Tags {
		if key := deref(tag.Key); key != "" {
			tags[key] = deref(tag.Value)
		}
	}
	c.applyTags(&resource, tags)
	return resource
}
