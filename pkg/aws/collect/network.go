package collect

import (
	"context"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// collectELB lists load balancers, target groups and target health. Target
// health is summarised per target group so a 5xx alarm can be read next to
// "3 of 4 targets unhealthy".
func collectELB(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("elbv2", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := elasticloadbalancingv2.NewFromConfig(c.env.config(c.endpointRegion), func(options *elasticloadbalancingv2.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	balancers := make(map[string]int)
	var marker *string
	for {
		page, err := api.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{PageSize: aws.Int32(400), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, balancer := range page.LoadBalancers {
			arn := deref(balancer.LoadBalancerArn)
			state := ""
			if balancer.State != nil {
				state = string(balancer.State.Code)
			}
			resource := c.newResource(arn, "load-balancer", deref(balancer.LoadBalancerName), state)
			resource.Attributes = map[string]string{
				model.AttrID:      lastSegment(arn, ":loadbalancer/"),
				model.AttrScheme:  string(balancer.Scheme),
				model.AttrDNSName: deref(balancer.DNSName),
				model.AttrVPC:     deref(balancer.VpcId),
			}
			if vpc := deref(balancer.VpcId); vpc != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", vpc)})
			}
			for _, zone := range balancer.AvailabilityZones {
				if subnet := deref(zone.SubnetId); subnet != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", subnet)})
				}
			}
			for _, group := range balancer.SecurityGroups {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", group)})
			}
			if balancer.Scheme == elbtypes.LoadBalancerSchemeEnumInternetFacing {
				out.flags = append(out.flags, c.flag(arn, model.FlagPublicExposure, "Load balancer is internet-facing"))
			}
			balancers[arn] = len(out.resources)
			out.resources = append(out.resources, resource)
		}
		if marker = page.NextMarker; deref(marker) == "" {
			break
		}
	}

	var groups []elbtypes.TargetGroup
	marker = nil
	for {
		page, err := api.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{PageSize: aws.Int32(400), Marker: marker})
		if err != nil {
			return result{}, err
		}
		groups = append(groups, page.TargetGroups...)
		if marker = page.NextMarker; deref(marker) == "" {
			break
		}
	}
	for index, group := range groups {
		arn := deref(group.TargetGroupArn)
		resource := c.newResource(arn, "target-group", deref(group.TargetGroupName), "")
		resource.Attributes = map[string]string{model.AttrID: lastSegment(arn, ":")}
		if vpc := deref(group.VpcId); vpc != "" {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", vpc)})
		}
		for _, balancer := range group.LoadBalancerArns {
			if position, ok := balancers[balancer]; ok {
				out.resources[position].Relations = append(out.resources[position].Relations, model.Relation{Type: model.RelationRoutesTo, Target: arn})
			}
		}
		if index >= maxTargetGroupHealth {
			if index == maxTargetGroupHealth {
				out.gaps = append(out.gaps, c.capGap("DescribeTargetHealth"))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		health, err := api.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(arn)})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("DescribeTargetHealth", err))
			out.resources = append(out.resources, resource)
			continue
		}
		total, unhealthy := 0, 0
		reason := ""
		for _, description := range health.TargetHealthDescriptions {
			if description.Target == nil {
				continue
			}
			total++
			target := deref(description.Target.Id)
			switch group.TargetType {
			case elbtypes.TargetTypeEnumInstance:
				target = c.ec2ARN("instance", target)
			}
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationRoutesTo, Target: target})
			if description.TargetHealth == nil {
				continue
			}
			if description.TargetHealth.State == elbtypes.TargetHealthStateEnumUnhealthy {
				unhealthy++
				if reason == "" {
					reason = string(description.TargetHealth.Reason)
				}
			}
		}
		state := "healthy"
		if total == 0 {
			state = "empty"
		}
		if unhealthy > 0 {
			state = "unhealthy"
			status := model.HealthWarning
			if unhealthy == total {
				status = model.HealthCritical
			}
			summary := strconv.Itoa(unhealthy) + " of " + strconv.Itoa(total) + " targets unhealthy"
			if reason != "" {
				summary += " (" + plainReason(reason) + ")"
			}
			out.health = append(out.health, c.healthItem(arn, model.SourceTargetHealth, status, summary, unknownSince, arn))
		}
		resource.State = state
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

// plainReason renders an ELB reason code such as Target.ResponseCodeMismatch
// as "response code mismatch".
func plainReason(reason string) string {
	reason = strings.TrimPrefix(strings.TrimPrefix(reason, "Target."), "Elb.")
	words := splitCamel(reason)
	return strings.ToLower(strings.Join(words, " "))
}

func splitCamel(value string) []string {
	var words []string
	start := 0
	for index := 1; index < len(value); index++ {
		if value[index] >= 'A' && value[index] <= 'Z' && !(value[index-1] >= 'A' && value[index-1] <= 'Z') {
			words = append(words, value[start:index])
			start = index
		}
	}
	return append(words, value[start:])
}

// collectRDS lists database instances and clusters, plus database events
// from the last hour as health signals.
func collectRDS(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("rds", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := rds.NewFromConfig(c.env.config(c.endpointRegion), func(options *rds.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var marker *string
	for {
		page, err := api.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{MaxRecords: aws.Int32(100), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, instance := range page.DBInstances {
			arn := deref(instance.DBInstanceArn)
			id := deref(instance.DBInstanceIdentifier)
			resource := c.newResource(arn, "db-instance", id, deref(instance.DBInstanceStatus))
			resource.Attributes = map[string]string{
				model.AttrID:     id,
				model.AttrEngine: deref(instance.Engine),
				model.AttrSize:   deref(instance.DBInstanceClass),
				model.AttrZone:   deref(instance.AvailabilityZone),
			}
			if cluster := deref(instance.DBClusterIdentifier); cluster != "" {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationMemberOf, Target: c.arn("rds", "cluster:"+cluster)})
			}
			if group := instance.DBSubnetGroup; group != nil {
				if vpc := deref(group.VpcId); vpc != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInVPC, Target: c.ec2ARN("vpc", vpc)})
					resource.Attributes[model.AttrVPC] = vpc
				}
				for _, subnet := range group.Subnets {
					if id := deref(subnet.SubnetIdentifier); id != "" {
						resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationInSubnet, Target: c.ec2ARN("subnet", id)})
					}
				}
			}
			for _, group := range instance.VpcSecurityGroups {
				if id := deref(group.VpcSecurityGroupId); id != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", id)})
				}
			}
			tags := make(map[string]string, len(instance.TagList))
			for _, tag := range instance.TagList {
				if key := deref(tag.Key); key != "" {
					tags[key] = deref(tag.Value)
				}
			}
			c.applyTags(&resource, tags)
			if derefBool(instance.PubliclyAccessible) {
				out.flags = append(out.flags, c.flag(arn, model.FlagPublicExposure, "Database is publicly accessible"))
			}
			if !derefBool(instance.StorageEncrypted) {
				out.flags = append(out.flags, c.flag(arn, model.FlagEncryptionOff, "Database storage is not encrypted"))
			}
			if status := deref(instance.DBInstanceStatus); status != "" && status != "available" {
				out.health = append(out.health, c.healthItem(arn, model.SourceDatabaseEvent, model.HealthWarning, "Database status: "+strings.ReplaceAll(status, "-", " "), unknownSince, ""))
			}
			out.resources = append(out.resources, resource)
		}
		if marker = page.Marker; deref(marker) == "" {
			break
		}
	}

	marker = nil
	for {
		page, err := api.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{MaxRecords: aws.Int32(100), Marker: marker})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("DescribeDBClusters", err))
			break
		}
		for _, cluster := range page.DBClusters {
			arn := deref(cluster.DBClusterArn)
			id := deref(cluster.DBClusterIdentifier)
			resource := c.newResource(arn, "db-cluster", id, deref(cluster.Status))
			resource.Attributes = map[string]string{model.AttrID: id, model.AttrEngine: deref(cluster.Engine)}
			tags := make(map[string]string, len(cluster.TagList))
			for _, tag := range cluster.TagList {
				if key := deref(tag.Key); key != "" {
					tags[key] = deref(tag.Value)
				}
			}
			c.applyTags(&resource, tags)
			if !derefBool(cluster.StorageEncrypted) {
				out.flags = append(out.flags, c.flag(arn, model.FlagEncryptionOff, "Database cluster storage is not encrypted"))
			}
			out.resources = append(out.resources, resource)
		}
		if marker = page.Marker; deref(marker) == "" {
			break
		}
	}

	start := c.now.Add(-c.eventWindow())
	events, err := api.DescribeEvents(ctx, &rds.DescribeEventsInput{StartTime: &start, MaxRecords: aws.Int32(100)})
	if err != nil {
		out.gaps = append(out.gaps, c.detailGap("DescribeEvents", err))
		return out, nil
	}
	for _, event := range events.Events {
		arn := deref(event.SourceArn)
		if arn == "" {
			continue
		}
		categories := append([]string(nil), event.EventCategories...)
		status := model.HealthWarning
		for _, category := range categories {
			if category == "failure" || category == "failover" {
				status = model.HealthCritical
			}
		}
		summary := "Database event"
		if len(categories) > 0 {
			summary = "Database event: " + strings.Join(categories, ", ")
		}
		out.health = append(out.health, c.healthItem(arn, model.SourceDatabaseEvent, status, summary, derefTime(event.Date), ""))
	}
	return out, nil
}
