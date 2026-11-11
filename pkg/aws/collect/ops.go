package collect

import (
	"context"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/health"
	healthtypes "github.com/aws/aws-sdk-go-v2/service/health/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/servicecatalogappregistry"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// alarmTarget resolves the resource a metric alarm watches from its
// namespace and dimensions.
func (c *call) alarmTarget(dimensions []cwtypes.Dimension) []string {
	values := make(map[string]string, len(dimensions))
	for _, dimension := range dimensions {
		values[deref(dimension.Name)] = deref(dimension.Value)
	}
	var targets []string
	add := func(arn string) { targets = append(targets, arn) }
	if value := values["LoadBalancer"]; value != "" {
		add(c.arn("elasticloadbalancing", "loadbalancer/"+value))
	}
	if value := values["TargetGroup"]; value != "" {
		add(c.arn("elasticloadbalancing", value))
	}
	if value := values["InstanceId"]; value != "" {
		add(c.ec2ARN("instance", value))
	}
	if value := values["DBInstanceIdentifier"]; value != "" {
		add(c.arn("rds", "db:"+value))
	}
	if value := values["DBClusterIdentifier"]; value != "" {
		add(c.arn("rds", "cluster:"+value))
	}
	if value := values["FunctionName"]; value != "" {
		add(c.arn("lambda", "function:"+value))
	}
	if value := values["QueueName"]; value != "" {
		add(c.arn("sqs", value))
	}
	if value := values["TableName"]; value != "" {
		add(c.arn("dynamodb", "table/"+value))
	}
	if cluster, service := values["ClusterName"], values["ServiceName"]; cluster != "" && service != "" {
		add(c.arn("ecs", "service/"+cluster+"/"+service))
	}
	if value := values["CacheClusterId"]; value != "" {
		add(c.arn("elasticache", "cluster:"+value))
	}
	if value := values["AutoScalingGroupName"]; value != "" {
		add(c.arn("autoscaling", "autoScalingGroup:*:autoScalingGroupName/"+value))
	}
	return targets
}

var compositeChild = regexp.MustCompile(`(?:ALARM|OK|INSUFFICIENT_DATA)\(\s*"?([^")]+?)"?\s*\)`)

// collectCloudWatch reads metric and composite alarms. An alarm in ALARM is a
// health signal on the resource it watches; a composite in ALARM names the
// children that are in ALARM.
func collectCloudWatch(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("cloudwatch", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := cloudwatch.NewFromConfig(c.env.config(c.endpointRegion), func(options *cloudwatch.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var metrics []cwtypes.MetricAlarm
	var composites []cwtypes.CompositeAlarm
	var token *string
	for {
		page, err := api.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{
			AlarmTypes: []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm},
			MaxRecords: aws.Int32(100),
			NextToken:  token,
		})
		if err != nil {
			return result{}, err
		}
		metrics = append(metrics, page.MetricAlarms...)
		composites = append(composites, page.CompositeAlarms...)
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}

	var out result
	inAlarm := make(map[string]string)
	byName := make(map[string]string)
	for _, alarm := range metrics {
		name := deref(alarm.AlarmName)
		arn := deref(alarm.AlarmArn)
		byName[name] = arn
		resource := c.newResource(arn, "alarm", name, strings.ToLower(string(alarm.StateValue)))
		resource.Attributes = map[string]string{model.AttrID: name}
		dimensions := alarm.Dimensions
		for _, query := range alarm.Metrics {
			if query.MetricStat != nil && query.MetricStat.Metric != nil {
				dimensions = append(dimensions, query.MetricStat.Metric.Dimensions...)
			}
		}
		targets := c.alarmTarget(dimensions)
		for _, target := range targets {
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationMonitors, Target: target})
		}
		out.resources = append(out.resources, resource)
		if alarm.StateValue != cwtypes.StateValueAlarm {
			continue
		}
		inAlarm[arn] = name
		subject := ""
		if len(targets) > 0 {
			subject = targets[0]
		}
		summary := "Alarm " + name + " is in ALARM"
		if metric := deref(alarm.MetricName); metric != "" {
			summary += " (" + metric + ")"
		}
		out.health = append(out.health, c.healthItem(subject, model.SourceAlarm, model.HealthCritical, summary, derefTime(alarm.StateUpdatedTimestamp), arn))
	}
	for _, alarm := range composites {
		name := deref(alarm.AlarmName)
		arn := deref(alarm.AlarmArn)
		resource := c.newResource(arn, "composite-alarm", name, strings.ToLower(string(alarm.StateValue)))
		resource.Attributes = map[string]string{model.AttrID: name}
		var children []string
		for _, match := range compositeChild.FindAllStringSubmatch(deref(alarm.AlarmRule), -1) {
			child := strings.TrimSpace(match[1])
			if !strings.HasPrefix(child, "arn:") {
				child = byName[child]
			}
			if child == "" {
				continue
			}
			resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationMonitors, Target: child})
			if _, firing := inAlarm[child]; firing {
				children = append(children, child)
			}
		}
		out.resources = append(out.resources, resource)
		if alarm.StateValue != cwtypes.StateValueAlarm {
			continue
		}
		item := c.healthItem("", model.SourceAlarm, model.HealthCritical, "Composite alarm "+name+" is in ALARM", derefTime(alarm.StateUpdatedTimestamp), arn)
		item.Children = children
		out.health = append(out.health, item)
	}
	return out, nil
}

// collectCloudFormation lists stacks with their status and, for the first
// stacks, their member resources, which drive stack membership and the
// CloudFormation application source.
func collectCloudFormation(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("cloudformation", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := cloudformation.NewFromConfig(c.env.config(c.endpointRegion), func(options *cloudformation.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var stacks []cfntypes.Stack
	var token *string
	for {
		page, err := api.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{NextToken: token})
		if err != nil {
			return result{}, err
		}
		stacks = append(stacks, page.Stacks...)
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	var out result
	for index, item := range stacks {
		arn := deref(item.StackId)
		name := deref(item.StackName)
		status := string(item.StackStatus)
		resource := c.newResource(arn, "stack", name, strings.ToLower(status))
		resource.Attributes = map[string]string{model.AttrID: name}
		tags := make(map[string]string, len(item.Tags))
		for _, tag := range item.Tags {
			if key := deref(tag.Key); key != "" {
				tags[key] = deref(tag.Value)
			}
		}
		c.applyTags(&resource, tags)
		since := derefTime(item.LastUpdatedTime)
		if since.IsZero() {
			since = derefTime(item.CreationTime)
		}
		words := strings.ToLower(strings.ReplaceAll(status, "_", " "))
		switch {
		case strings.HasSuffix(status, "_FAILED"):
			out.health = append(out.health, c.healthItem(arn, model.SourceStackStatus, model.HealthCritical, "Stack "+name+": "+words, since, arn))
		case strings.Contains(status, "ROLLBACK"), strings.HasSuffix(status, "_IN_PROGRESS"):
			out.health = append(out.health, c.healthItem(arn, model.SourceStackStatus, model.HealthWarning, "Stack "+name+": "+words, since, arn))
		}
		if index >= maxStacksDescribed {
			if index == maxStacksDescribed {
				out.gaps = append(out.gaps, c.capGap("ListStackResources"))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		token = nil
		for {
			page, err := api.ListStackResources(ctx, &cloudformation.ListStackResourcesInput{StackName: aws.String(arn), NextToken: token})
			if err != nil {
				out.gaps = append(out.gaps, c.detailGap("ListStackResources", err))
				break
			}
			for _, member := range page.StackResourceSummaries {
				physical := deref(member.PhysicalResourceId)
				if physical == "" {
					continue
				}
				if deref(member.ResourceType) == "AWS::SQS::Queue" {
					physical = lastSegment(physical, "/")
				}
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationAssociated, Target: physical})
			}
			if token = page.NextToken; deref(token) == "" {
				break
			}
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

// collectAWSHealth reads open and upcoming AWS Health events and the
// resources they affect.
func collectAWSHealth(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("health", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := health.NewFromConfig(c.env.config(c.endpointRegion), func(options *health.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	page, err := api.DescribeEvents(ctx, &health.DescribeEventsInput{
		Filter:     &healthtypes.EventFilter{EventStatusCodes: []healthtypes.EventStatusCode{healthtypes.EventStatusCodeOpen, healthtypes.EventStatusCodeUpcoming}},
		MaxResults: aws.Int32(100),
	})
	if err != nil {
		return result{}, err
	}
	var out result
	for index, event := range page.Events {
		arn := deref(event.Arn)
		region := deref(event.Region)
		status := model.HealthWarning
		if event.EventTypeCategory == healthtypes.EventTypeCategoryIssue {
			status = model.HealthCritical
		}
		summary := "AWS Health: " + strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(deref(event.EventTypeCode), "AWS_"), "_", " "))
		var affected []string
		if index < maxHealthEvents {
			entities, err := api.DescribeAffectedEntities(ctx, &health.DescribeAffectedEntitiesInput{
				Filter:     &healthtypes.EntityFilter{EventArns: []string{arn}},
				MaxResults: aws.Int32(100),
			})
			if err != nil {
				out.gaps = append(out.gaps, c.detailGap("DescribeAffectedEntities", err))
			} else {
				for _, entity := range entities.Entities {
					value := deref(entity.EntityValue)
					switch {
					case strings.HasPrefix(value, "arn:"):
						affected = append(affected, value)
					case strings.HasPrefix(value, "i-") && region != "":
						affected = append(affected, c.regionalARN("ec2", region, "instance/"+value))
					}
				}
			}
		} else if index == maxHealthEvents {
			out.gaps = append(out.gaps, c.capGap("DescribeAffectedEntities"))
		}
		if len(affected) == 0 {
			item := c.healthItem("", model.SourceAWSHealth, status, summary, derefTime(event.StartTime), arn)
			item.Region = regionOr(region, c.unit.Region)
			out.health = append(out.health, item)
			continue
		}
		for _, target := range affected {
			item := c.healthItem(target, model.SourceAWSHealth, status, summary, derefTime(event.StartTime), arn)
			item.Region = regionOr(region, c.unit.Region)
			out.health = append(out.health, item)
		}
	}
	return out, nil
}

func regionOr(region, fallback string) string {
	if region == "" || region == "global" {
		return fallback
	}
	return region
}

// collectTags reads every tag set in the region through the tagging API so
// services whose list calls omit tags still get them. Values are redacted
// unless allowlisted.
func collectTags(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("tagging", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := resourcegroupstaggingapi.NewFromConfig(c.env.config(c.endpointRegion), func(options *resourcegroupstaggingapi.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{ResourcesPerPage: aws.Int32(100), PaginationToken: token})
		if err != nil {
			return result{}, err
		}
		for _, mapping := range page.ResourceTagMappingList {
			arn := deref(mapping.ResourceARN)
			if arn == "" {
				continue
			}
			tags := make(map[string]string, len(mapping.Tags))
			for _, tag := range mapping.Tags {
				if key := deref(tag.Key); key != "" {
					tags[key] = deref(tag.Value)
				}
			}
			resource := c.newResource(arn, "tags", "", "")
			c.applyTags(&resource, tags)
			out.resources = append(out.resources, resource)
		}
		if token = page.PaginationToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

// collectAppRegistry reads AppRegistry applications and the stacks and
// resources associated with them: the first application source.
func collectAppRegistry(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("appregistry", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := servicecatalogappregistry.NewFromConfig(c.env.config(c.endpointRegion), func(options *servicecatalogappregistry.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.ListApplications(ctx, &servicecatalogappregistry.ListApplicationsInput{MaxResults: aws.Int32(25), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, application := range page.Applications {
			resource := c.newResource(deref(application.Arn), "application", deref(application.Name), "")
			resource.Attributes = map[string]string{model.AttrApplicationID: deref(application.Id)}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	for index := range out.resources {
		if index >= maxApplications {
			out.gaps = append(out.gaps, c.capGap("ListAssociatedResources"))
			break
		}
		token = nil
		for {
			page, err := api.ListAssociatedResources(ctx, &servicecatalogappregistry.ListAssociatedResourcesInput{
				Application: aws.String(out.resources[index].Attributes[model.AttrApplicationID]),
				MaxResults:  aws.Int32(25),
				NextToken:   token,
			})
			if err != nil {
				out.gaps = append(out.gaps, c.detailGap("ListAssociatedResources", err))
				break
			}
			for _, associated := range page.Resources {
				if arn := deref(associated.Arn); arn != "" {
					out.resources[index].Relations = append(out.resources[index].Relations, model.Relation{Type: model.RelationAssociated, Target: arn})
				}
			}
			if token = page.NextToken; deref(token) == "" {
				break
			}
		}
	}
	return out, nil
}
