package probe

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigateway"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/configservice"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/health"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// probeFunc performs one read and returns the number of items in the page.
type probeFunc func(context.Context, env) (int, error)

// probes is keyed by catalog ID. A catalog entry without an implementation
// is reported as not checked rather than silently skipped.
var probes = map[string]probeFunc{
	"api-gateway":    probeAPIGateway,
	"auto-scaling":   probeAutoScaling,
	"aws-health":     probeHealth,
	"cloudformation": probeCloudFormation,
	"cloudfront":     probeCloudFront,
	"cloudtrail":     probeCloudTrail,
	"cloudwatch":     probeCloudWatch,
	"config":         probeConfig,
	"dynamodb":       probeDynamoDB,
	"ec2":            probeEC2,
	"ecs":            probeECS,
	"eks":            probeEKS,
	"elasticache":    probeElastiCache,
	"elbv2":          probeELBv2,
	"eventbridge":    probeEventBridge,
	"iam":            probeIAM,
	"kinesis":        probeKinesis,
	"lambda":         probeLambda,
	"rds":            probeRDS,
	"route53":        probeRoute53,
	"s3":             probeS3,
	"sns":            probeSNS,
	"sqs":            probeSQS,
	"step-functions": probeStepFunctions,
}

func probeAPIGateway(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("api-gateway")
	if err != nil {
		return 0, err
	}
	api := apigateway.NewFromConfig(environment.config(), func(options *apigateway.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.GetRestApis(ctx, &apigateway.GetRestApisInput{Limit: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Items), nil
}

func probeAutoScaling(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("auto-scaling")
	if err != nil {
		return 0, err
	}
	api := autoscaling.NewFromConfig(environment.config(), func(options *autoscaling.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{MaxRecords: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.AutoScalingGroups), nil
}

func probeHealth(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("aws-health")
	if err != nil {
		return 0, err
	}
	api := health.NewFromConfig(environment.config(), func(options *health.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeEvents(ctx, &health.DescribeEventsInput{MaxResults: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Events), nil
}

func probeCloudFormation(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("cloudformation")
	if err != nil {
		return 0, err
	}
	api := cloudformation.NewFromConfig(environment.config(), func(options *cloudformation.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{})
	if err != nil {
		return 0, err
	}
	return len(output.Stacks), nil
}

func probeCloudFront(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("cloudfront")
	if err != nil {
		return 0, err
	}
	api := cloudfront.NewFromConfig(environment.config(), func(options *cloudfront.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListDistributions(ctx, &cloudfront.ListDistributionsInput{MaxItems: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	if output.DistributionList == nil {
		return 0, nil
	}
	return len(output.DistributionList.Items), nil
}

func probeCloudTrail(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("cloudtrail")
	if err != nil {
		return 0, err
	}
	api := cloudtrail.NewFromConfig(environment.config(), func(options *cloudtrail.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.LookupEvents(ctx, &cloudtrail.LookupEventsInput{MaxResults: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Events), nil
}

func probeCloudWatch(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("cloudwatch")
	if err != nil {
		return 0, err
	}
	api := cloudwatch.NewFromConfig(environment.config(), func(options *cloudwatch.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{MaxRecords: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.MetricAlarms) + len(output.CompositeAlarms), nil
}

func probeConfig(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("config")
	if err != nil {
		return 0, err
	}
	api := configservice.NewFromConfig(environment.config(), func(options *configservice.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeConfigurationRecorders(ctx, &configservice.DescribeConfigurationRecordersInput{})
	if err != nil {
		return 0, err
	}
	return len(output.ConfigurationRecorders), nil
}

func probeDynamoDB(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("dynamodb")
	if err != nil {
		return 0, err
	}
	api := dynamodb.NewFromConfig(environment.config(), func(options *dynamodb.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.TableNames), nil
}

func probeEC2(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("ec2")
	if err != nil {
		return 0, err
	}
	api := ec2.NewFromConfig(environment.config(), func(options *ec2.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(5)})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, reservation := range output.Reservations {
		count += len(reservation.Instances)
	}
	return count, nil
}

func probeECS(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("ecs")
	if err != nil {
		return 0, err
	}
	api := ecs.NewFromConfig(environment.config(), func(options *ecs.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListClusters(ctx, &ecs.ListClustersInput{MaxResults: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.ClusterArns), nil
}

func probeEKS(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("eks")
	if err != nil {
		return 0, err
	}
	api := eks.NewFromConfig(environment.config(), func(options *eks.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListClusters(ctx, &eks.ListClustersInput{MaxResults: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Clusters), nil
}

func probeElastiCache(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("elasticache")
	if err != nil {
		return 0, err
	}
	api := elasticache.NewFromConfig(environment.config(), func(options *elasticache.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeCacheClusters(ctx, &elasticache.DescribeCacheClustersInput{MaxRecords: aws.Int32(20)})
	if err != nil {
		return 0, err
	}
	return len(output.CacheClusters), nil
}

func probeELBv2(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("elbv2")
	if err != nil {
		return 0, err
	}
	api := elasticloadbalancingv2.NewFromConfig(environment.config(), func(options *elasticloadbalancingv2.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeLoadBalancers(ctx, &elasticloadbalancingv2.DescribeLoadBalancersInput{PageSize: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.LoadBalancers), nil
}

func probeEventBridge(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("eventbridge")
	if err != nil {
		return 0, err
	}
	api := eventbridge.NewFromConfig(environment.config(), func(options *eventbridge.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListRules(ctx, &eventbridge.ListRulesInput{Limit: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Rules), nil
}

func probeIAM(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("iam")
	if err != nil {
		return 0, err
	}
	api := iam.NewFromConfig(environment.config(), func(options *iam.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListRoles(ctx, &iam.ListRolesInput{MaxItems: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Roles), nil
}

func probeKinesis(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("kinesis")
	if err != nil {
		return 0, err
	}
	api := kinesis.NewFromConfig(environment.config(), func(options *kinesis.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListStreams(ctx, &kinesis.ListStreamsInput{Limit: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.StreamNames), nil
}

func probeLambda(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("lambda")
	if err != nil {
		return 0, err
	}
	api := lambda.NewFromConfig(environment.config(), func(options *lambda.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListFunctions(ctx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.Functions), nil
}

func probeRDS(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("rds")
	if err != nil {
		return 0, err
	}
	api := rds.NewFromConfig(environment.config(), func(options *rds.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{MaxRecords: aws.Int32(20)})
	if err != nil {
		return 0, err
	}
	return len(output.DBInstances), nil
}

func probeRoute53(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("route53")
	if err != nil {
		return 0, err
	}
	api := route53.NewFromConfig(environment.config(), func(options *route53.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListHealthChecks(ctx, &route53.ListHealthChecksInput{MaxItems: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.HealthChecks), nil
}

func probeS3(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("s3")
	if err != nil {
		return 0, err
	}
	api := s3.NewFromConfig(environment.config(), func(options *s3.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return 0, err
	}
	return len(output.Buckets), nil
}

func probeSNS(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("sns")
	if err != nil {
		return 0, err
	}
	api := sns.NewFromConfig(environment.config(), func(options *sns.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListTopics(ctx, &sns.ListTopicsInput{})
	if err != nil {
		return 0, err
	}
	return len(output.Topics), nil
}

func probeSQS(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("sqs")
	if err != nil {
		return 0, err
	}
	api := sqs.NewFromConfig(environment.config(), func(options *sqs.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListQueues(ctx, &sqs.ListQueuesInput{MaxResults: aws.Int32(1)})
	if err != nil {
		return 0, err
	}
	return len(output.QueueUrls), nil
}

func probeStepFunctions(ctx context.Context, environment env) (int, error) {
	endpoint, client, err := environment.pin("step-functions")
	if err != nil {
		return 0, err
	}
	api := sfn.NewFromConfig(environment.config(), func(options *sfn.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
	})
	output, err := api.ListStateMachines(ctx, &sfn.ListStateMachinesInput{MaxResults: 1})
	if err != nil {
		return 0, err
	}
	return len(output.StateMachines), nil
}
