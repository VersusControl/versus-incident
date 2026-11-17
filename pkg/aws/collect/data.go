package collect

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func collectDynamoDB(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("dynamodb", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := dynamodb.NewFromConfig(c.env.config(c.endpointRegion), func(options *dynamodb.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var start *string
	for {
		page, err := api.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(100), ExclusiveStartTableName: start})
		if err != nil {
			return result{}, err
		}
		for _, name := range page.TableNames {
			resource := c.newResource(c.arn("dynamodb", "table/"+name), "table", name, "")
			resource.Attributes = map[string]string{model.AttrID: name}
			out.resources = append(out.resources, resource)
		}
		if start = page.LastEvaluatedTableName; deref(start) == "" {
			break
		}
	}
	return out, nil
}

func collectElastiCache(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("elasticache", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := elasticache.NewFromConfig(c.env.config(c.endpointRegion), func(options *elasticache.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var marker *string
	for {
		page, err := api.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{MaxRecords: aws.Int32(100), Marker: marker})
		if err != nil {
			return result{}, err
		}
		for _, cluster := range page.CacheClusters {
			id := deref(cluster.CacheClusterId)
			arn := deref(cluster.ARN)
			if arn == "" {
				arn = c.arn("elasticache", "cluster:"+id)
			}
			status := deref(cluster.CacheClusterStatus)
			resource := c.newResource(arn, "cache-cluster", id, status)
			resource.Attributes = map[string]string{
				model.AttrID:     id,
				model.AttrEngine: deref(cluster.Engine),
				model.AttrSize:   deref(cluster.CacheNodeType),
				model.AttrZone:   deref(cluster.PreferredAvailabilityZone),
			}
			for _, group := range cluster.SecurityGroups {
				if groupID := deref(group.SecurityGroupId); groupID != "" {
					resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationSecurityGroup, Target: c.ec2ARN("security-group", groupID)})
				}
			}
			if !derefBool(cluster.AtRestEncryptionEnabled) {
				out.flags = append(out.flags, c.flag(arn, model.FlagEncryptionOff, "Cache data at rest is not encrypted"))
			}
			if status != "" && status != "available" {
				out.health = append(out.health, c.healthItem(arn, model.SourceDatabaseEvent, model.HealthWarning, "Cache status: "+status, unknownSince, ""))
			}
			out.resources = append(out.resources, resource)
		}
		if marker = page.Marker; deref(marker) == "" {
			break
		}
	}
	return out, nil
}

// queueARN derives a queue ARN from its URL
// (https://sqs.<region>.amazonaws.com/<account>/<name>).
func (c *call) queueARN(url string) (string, string) {
	name := lastSegment(url, "/")
	return c.arn("sqs", name), name
}

// queueAttributes are the only queue attributes requested: identifiers and
// encryption settings. Policies are never read.
var queueAttributes = []sqstypes.QueueAttributeName{
	sqstypes.QueueAttributeNameQueueArn,
	sqstypes.QueueAttributeNameKmsMasterKeyId,
	sqstypes.QueueAttributeNameSqsManagedSseEnabled,
	sqstypes.QueueAttributeNameRedrivePolicy,
}

func collectSQS(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("sqs", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := sqs.NewFromConfig(c.env.config(c.endpointRegion), func(options *sqs.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var urls []string
	var token *string
	for {
		page, err := api.ListQueues(ctx, &sqs.ListQueuesInput{MaxResults: aws.Int32(1000), NextToken: token})
		if err != nil {
			return result{}, err
		}
		urls = append(urls, page.QueueUrls...)
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	var out result
	for index, url := range urls {
		arn, name := c.queueARN(url)
		resource := c.newResource(arn, "queue", name, "")
		resource.Attributes = map[string]string{model.AttrID: name}
		if index >= maxQueueDetails {
			if index == maxQueueDetails {
				out.gaps = append(out.gaps, c.capGap("GetQueueAttributes"))
			}
			out.resources = append(out.resources, resource)
			continue
		}
		attributes, err := api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url), AttributeNames: queueAttributes})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("GetQueueAttributes", err))
			out.resources = append(out.resources, resource)
			continue
		}
		values := attributes.Attributes
		if queueARN := values[string(sqstypes.QueueAttributeNameQueueArn)]; queueARN != "" {
			resource.ARN = queueARN
		}
		if values[string(sqstypes.QueueAttributeNameKmsMasterKeyId)] == "" && values[string(sqstypes.QueueAttributeNameSqsManagedSseEnabled)] != "true" {
			out.flags = append(out.flags, c.flag(resource.ARN, model.FlagEncryptionOff, "Queue messages are not encrypted at rest"))
		}
		if policy := values[string(sqstypes.QueueAttributeNameRedrivePolicy)]; policy != "" {
			var redrive struct {
				DeadLetterTargetArn string `json:"deadLetterTargetArn"`
			}
			if json.Unmarshal([]byte(policy), &redrive) == nil && strings.HasPrefix(redrive.DeadLetterTargetArn, "arn:") {
				resource.Relations = append(resource.Relations, model.Relation{Type: model.RelationDeadLetter, Target: redrive.DeadLetterTargetArn})
			}
		}
		out.resources = append(out.resources, resource)
	}
	return out, nil
}

// collectSNS lists topics and their subscriptions. Only subscriptions whose
// endpoint is an ARN become relations; email, SMS and URL endpoints are
// personal or external and are never kept.
func collectSNS(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("sns", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := sns.NewFromConfig(c.env.config(c.endpointRegion), func(options *sns.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	topics := make(map[string]int)
	var token *string
	for {
		page, err := api.ListTopics(ctx, &sns.ListTopicsInput{NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, topic := range page.Topics {
			arn := deref(topic.TopicArn)
			name := lastSegment(arn, ":")
			resource := c.newResource(arn, "topic", name, "")
			resource.Attributes = map[string]string{model.AttrID: name}
			topics[arn] = len(out.resources)
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	token = nil
	for {
		page, err := api.ListSubscriptions(ctx, &sns.ListSubscriptionsInput{NextToken: token})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("ListSubscriptions", err))
			break
		}
		for _, subscription := range page.Subscriptions {
			endpoint := deref(subscription.Endpoint)
			position, ok := topics[deref(subscription.TopicArn)]
			if !ok || !strings.HasPrefix(endpoint, "arn:") {
				continue
			}
			out.resources[position].Relations = append(out.resources[position].Relations, model.Relation{Type: model.RelationSubscribes, Target: endpoint})
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

func collectEventBridge(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("events", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := eventbridge.NewFromConfig(c.env.config(c.endpointRegion), func(options *eventbridge.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.ListRules(ctx, &eventbridge.ListRulesInput{Limit: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, rule := range page.Rules {
			name := deref(rule.Name)
			resource := c.newResource(deref(rule.Arn), "rule", name, strings.ToLower(string(rule.State)))
			resource.Attributes = map[string]string{model.AttrID: name}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	for index := range out.resources {
		if index >= maxRuleTargets {
			out.gaps = append(out.gaps, c.capGap("ListTargetsByRule"))
			break
		}
		targets, err := api.ListTargetsByRule(ctx, &eventbridge.ListTargetsByRuleInput{Rule: aws.String(out.resources[index].Name), Limit: aws.Int32(100)})
		if err != nil {
			out.gaps = append(out.gaps, c.detailGap("ListTargetsByRule", err))
			continue
		}
		for _, target := range targets.Targets {
			if arn := deref(target.Arn); arn != "" {
				out.resources[index].Relations = append(out.resources[index].Relations, model.Relation{Type: model.RelationTargets, Target: arn})
			}
		}
	}
	return out, nil
}

func collectStepFunctions(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("sfn", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := sfn.NewFromConfig(c.env.config(c.endpointRegion), func(options *sfn.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.ListStateMachines(ctx, &sfn.ListStateMachinesInput{MaxResults: 100, NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, machine := range page.StateMachines {
			name := deref(machine.Name)
			resource := c.newResource(deref(machine.StateMachineArn), "state-machine", name, "")
			resource.Attributes = map[string]string{model.AttrID: name, model.AttrEngine: strings.ToLower(string(machine.Type))}
			out.resources = append(out.resources, resource)
		}
		if token = page.NextToken; deref(token) == "" {
			break
		}
	}
	return out, nil
}

func collectKinesis(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("kinesis", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := kinesis.NewFromConfig(c.env.config(c.endpointRegion), func(options *kinesis.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var token *string
	for {
		page, err := api.ListStreams(ctx, &kinesis.ListStreamsInput{Limit: aws.Int32(100), NextToken: token})
		if err != nil {
			return result{}, err
		}
		for _, summary := range page.StreamSummaries {
			name := deref(summary.StreamName)
			arn := deref(summary.StreamARN)
			if arn == "" {
				arn = c.arn("kinesis", "stream/"+name)
			}
			resource := c.newResource(arn, "stream", name, strings.ToLower(string(summary.StreamStatus)))
			resource.Attributes = map[string]string{model.AttrID: name}
			out.resources = append(out.resources, resource)
		}
		if len(page.StreamSummaries) == 0 {
			for _, name := range page.StreamNames {
				resource := c.newResource(c.arn("kinesis", "stream/"+name), "stream", name, "")
				resource.Attributes = map[string]string{model.AttrID: name}
				out.resources = append(out.resources, resource)
			}
		}
		if token = page.NextToken; deref(token) == "" || !derefBool(page.HasMoreStreams) {
			break
		}
	}
	return out, nil
}

func collectAPIGateway(ctx context.Context, c *call) (result, error) {
	endpoint, client, stack, err := c.env.endpoint("apigateway", c.endpointRegion)
	if err != nil {
		return result{}, err
	}
	api := apigateway.NewFromConfig(c.env.config(c.endpointRegion), func(options *apigateway.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	})
	var out result
	var position *string
	for {
		page, err := api.GetRestApis(ctx, &apigateway.GetRestApisInput{Limit: aws.Int32(500), Position: position})
		if err != nil {
			return result{}, err
		}
		for _, item := range page.Items {
			id := deref(item.Id)
			resource := c.newResource(model.ARN(c.partition, "apigateway", c.unit.Region, "", "/restapis/"+id), "rest-api", deref(item.Name), "")
			resource.Attributes = map[string]string{model.AttrID: id}
			c.applyTags(&resource, item.Tags)
			out.resources = append(out.resources, resource)
		}
		if position = page.Position; deref(position) == "" {
			break
		}
	}
	return out, nil
}
