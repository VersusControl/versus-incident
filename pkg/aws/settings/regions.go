package settings

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// DiscoverRegions lists the account's regions and marks where resources were
// found, then stores the answer on the draft.
func (conn *Service) DiscoverRegions(ctx context.Context, org, draftID string) ([]RegionStatus, error) {
	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return nil, err
	}
	if draft.RoleARN == "" {
		return nil, ErrInvalidArguments
	}
	credentials, err := conn.credentials(ctx, draft.RoleARN, draft.ExternalID, defaultHomeRegion)
	if err != nil {
		return nil, err
	}
	accountID := draft.AccountID
	if accountID == "" {
		accountID = accountIDFromRoleARN(draft.RoleARN)
	}
	regions, err := conn.discoverAccountRegions(ctx, accountID, credentials)
	if err != nil {
		return nil, err
	}
	draft.Regions = regions
	if draft.Status == DraftStarted {
		draft.Status = DraftVerified
	}
	if err := conn.store.SaveDraft(org, draft); err != nil {
		return nil, err
	}
	if draft.AccountID != "" {
		if health, healthErr := conn.store.GetHealth(org, draft.AccountID); healthErr == nil {
			health.Regions = regions
			_ = conn.store.SaveHealth(org, health)
		}
	}
	return regions, nil
}

// discoverAccountRegions lists the account's regions and checks resource
// presence. Every call goes through the shared per-account, per-region
// limiter, so discovery is counted against the API-use meter and backs off
// after a throttling reply like any other read.
func (conn *Service) discoverAccountRegions(ctx context.Context, accountID string, credentials aws.CredentialsProvider) ([]RegionStatus, error) {
	var output *ec2.DescribeRegionsOutput
	err := conn.meteredCall(ctx, accountID, defaultHomeRegion, func(callCtx context.Context) error {
		client := ec2.NewFromConfig(conn.awsConfig(credentials, defaultHomeRegion, "ec2."+defaultHomeRegion+".amazonaws.com"))
		var callErr error
		output, callErr = client.DescribeRegions(callCtx, &ec2.DescribeRegionsInput{AllRegions: aws.Bool(true)})
		return callErr
	})
	if err != nil {
		return nil, err
	}
	if output == nil {
		return nil, awsauth.ErrNetwork
	}
	regions := make([]RegionStatus, 0, len(output.Regions))
	for _, region := range output.Regions {
		name := aws.ToString(region.RegionName)
		if awsauth.ValidateRegion(name) != nil {
			continue
		}
		status := strings.ToLower(aws.ToString(region.OptInStatus))
		enabled := status != "not-opted-in"
		regions = append(regions, RegionStatus{Region: name, Enabled: enabled, OptInStatus: status})
	}
	if len(regions) == 0 {
		return nil, awsauth.ErrNetwork
	}
	for index := range regions {
		if !regions[index].Enabled {
			continue
		}
		regions[index].ResourcesFound = conn.resourcesInRegion(ctx, accountID, credentials, regions[index].Region)
	}
	sort.Slice(regions, func(left, right int) bool { return regions[left].Region < regions[right].Region })
	return regions, nil
}

func (conn *Service) resourcesInRegion(ctx context.Context, accountID string, credentials aws.CredentialsProvider, region string) bool {
	suffix := ".amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = ".amazonaws.com.cn"
	}
	found := false
	checks := []func(context.Context) (bool, error){
		func(callCtx context.Context) (bool, error) {
			client := ec2.NewFromConfig(conn.awsConfig(credentials, region, "ec2."+region+suffix))
			output, err := client.DescribeInstances(callCtx, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(5)})
			return err == nil && output != nil && len(output.Reservations) > 0, err
		},
		func(callCtx context.Context) (bool, error) {
			client := lambda.NewFromConfig(conn.awsConfig(credentials, region, "lambda."+region+suffix))
			output, err := client.ListFunctions(callCtx, &lambda.ListFunctionsInput{MaxItems: aws.Int32(5)})
			return err == nil && output != nil && len(output.Functions) > 0, err
		},
		func(callCtx context.Context) (bool, error) {
			client := ecs.NewFromConfig(conn.awsConfig(credentials, region, "ecs."+region+suffix))
			output, err := client.ListClusters(callCtx, &ecs.ListClustersInput{MaxResults: aws.Int32(5)})
			return err == nil && output != nil && len(output.ClusterArns) > 0, err
		},
		func(callCtx context.Context) (bool, error) {
			client := rds.NewFromConfig(conn.awsConfig(credentials, region, "rds."+region+suffix))
			output, err := client.DescribeDBInstances(callCtx, &rds.DescribeDBInstancesInput{})
			return err == nil && output != nil && len(output.DBInstances) > 0, err
		},
	}
	for _, check := range checks {
		hit := false
		err := conn.meteredCall(ctx, accountID, region, func(callCtx context.Context) error {
			var callErr error
			hit, callErr = check(callCtx)
			return callErr
		})
		if err == nil && hit {
			found = true
			break
		}
	}
	return found
}

// meteredCall gates one AWS call behind the shared limiter and meters it. A
// throttling reply pauses the account and region; the returned error is always
// in the closed vocabulary so callers never render provider text.
func (conn *Service) meteredCall(ctx context.Context, accountID, region string, call func(context.Context) error) error {
	if conn.limiter != nil {
		if err := conn.limiter.Wait(ctx, accountID, region); err != nil {
			if errors.Is(err, limit.ErrBackoff) {
				return awsauth.ErrThrottled
			}
			return awsauth.ErrTimeout
		}
	}
	err := call(ctx)
	typed := typedAWSError(err)
	if conn.limiter != nil {
		switch {
		case errors.Is(typed, awsauth.ErrThrottled):
			conn.limiter.Throttled(accountID, region)
		case err == nil:
			conn.limiter.Succeeded(accountID, region)
		}
	}
	return typed
}

// typedAWSError maps a provider or transport failure onto the closed
// vocabulary. It keeps the specific code (region disabled, throttled, timeout)
// instead of collapsing every failure into a generic one.
func typedAWSError(err error) error {
	if err == nil {
		return nil
	}
	if code := awsauth.ErrorCode(err); code != "" {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return awsauth.ErrTimeout
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "RegionDisabledException", "RegionDisabled":
			return awsauth.ErrRegionDisabled
		case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException", "SlowDown":
			return awsauth.ErrThrottled
		case "RequestTimeout", "RequestTimeoutException":
			return awsauth.ErrTimeout
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "AuthorizationError":
			return awsauth.ErrTrustDenied
		}
	}
	return awsauth.ErrNetwork
}

// enabledRegions is the sorted list of regions the account may use.
func enabledRegions(regions []RegionStatus) []string {
	enabled := make([]string, 0, len(regions))
	for _, region := range regions {
		if region.Enabled {
			enabled = append(enabled, region.Region)
		}
	}
	sort.Strings(enabled)
	return enabled
}

func (conn *Service) awsConfig(credentials aws.CredentialsProvider, region, host string) aws.Config {
	return aws.Config{
		Region:           region,
		Credentials:      credentials,
		HTTPClient:       pinnedHostClient{client: conn.httpClient, host: host},
		RetryMaxAttempts: 1,
	}
}

// pinnedHostClient refuses any request that leaves the expected AWS host and
// then delegates to the injected transport. It keeps plaintext and redirects
// out of the region discovery path.
type pinnedHostClient struct {
	client aws.HTTPClient
	host   string
}

func (client pinnedHostClient) Do(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.URL.Hostname() != client.host {
		return nil, awsauth.ErrNetwork
	}
	return client.client.Do(request)
}
