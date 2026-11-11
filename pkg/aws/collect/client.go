package collect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// maxPageBytes bounds one collector response. Collectors ask for full pages,
// so the cap is wider than a probe's, but still finite.
const maxPageBytes = 16 * 1024 * 1024

// Closed error codes a unit failure records. They extend the awsauth
// vocabulary with the two probe outcomes a collector can also meet.
const (
	CodeDenied    = "denied"
	CodeNotOnPlan = "not_on_plan"
	CodeInvalid   = "invalid_snapshot"
	CodeStorage   = "storage"
	// CodeNotInRegion: the service has no endpoint in the region (its host
	// does not resolve, or AWS answers that the region is unsupported).
	CodeNotInRegion   = "not_available_in_region"
	codeThrottledWord = "throttled"
)

// errLocalBackoff is returned when the shared limiter is pausing this account
// and region after a throttling reply. It is reported as throttled.
var errLocalBackoff = errors.New("throttled")

// regionalHosts maps a client to the DNS label of its regional endpoint.
var regionalHosts = map[string]string{
	"apigateway":     "apigateway",
	"appregistry":    "servicecatalog-appregistry",
	"autoscaling":    "autoscaling",
	"cloudformation": "cloudformation",
	"cloudtrail":     "cloudtrail",
	"cloudwatch":     "monitoring",
	"dynamodb":       "dynamodb",
	"ec2":            "ec2",
	"ecs":            "ecs",
	"eks":            "eks",
	"elasticache":    "elasticache",
	"elbv2":          "elasticloadbalancing",
	"events":         "events",
	"kinesis":        "kinesis",
	"lambda":         "lambda",
	"rds":            "rds",
	"s3":             "s3",
	"sfn":            "states",
	"sns":            "sns",
	"sqs":            "sqs",
	"tagging":        "tagging",
}

// globalHosts maps a client to its single global endpoint.
var globalHosts = map[string]string{
	"cloudfront": "cloudfront.amazonaws.com",
	"health":     "health.us-east-1.amazonaws.com",
	"iam":        "iam.amazonaws.com",
	"route53":    "route53.amazonaws.com",
	"s3-global":  "s3.amazonaws.com",
}

// env carries one account's credentials, transport and shared limiter into a
// unit. Region is the limiter region: the unit's region, or us-east-1 for a
// global service.
type env struct {
	account     string
	region      string
	credentials aws.CredentialsProvider
	httpClient  aws.HTTPClient
	limiter     *limit.Manager
	retries     int
	sleep       func(context.Context, time.Duration) error
}

// host returns the exact host a client may reach in endpointRegion.
func host(client, endpointRegion string) (string, bool) {
	if fixed, ok := globalHosts[client]; ok {
		return fixed, true
	}
	prefix, ok := regionalHosts[client]
	if !ok || awsauth.ValidateRegion(endpointRegion) != nil {
		return "", false
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(endpointRegion, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return prefix + "." + endpointRegion + "." + suffix, true
}

// config builds an SDK configuration without loading ambient providers.
func (environment env) config(signingRegion string) aws.Config {
	return aws.Config{
		Region:           signingRegion,
		Credentials:      awsauth.MarkCredentialFailures(environment.credentials),
		HTTPClient:       environment.httpClient,
		RetryMaxAttempts: 1,
	}
}

// endpoint pins a client to its exact host over TLS and returns the base
// endpoint, the pinned transport and the limiter middleware.
func (environment env) endpoint(client, endpointRegion string) (string, aws.HTTPClient, []func(*middleware.Stack) error, error) {
	exact, ok := host(client, endpointRegion)
	if !ok {
		return "", nil, nil, awsauth.ErrNetwork
	}
	pinned := pinnedHTTPClient{client: environment.httpClient, host: exact}
	return "https://" + exact, pinned, []func(*middleware.Stack) error{environment.limitStack}, nil
}

// limitStack installs the shared limiter around every operation call so no
// AWS request, including a retry, can bypass the per-account, per-region
// budget or the API-use meter.
func (environment env) limitStack(stack *middleware.Stack) error {
	return stack.Initialize.Add(limitMiddleware{environment: environment}, middleware.Before)
}

type limitMiddleware struct {
	environment env
}

func (limitMiddleware) ID() string { return "VersusSharedLimiter" }

func (handler limitMiddleware) HandleInitialize(ctx context.Context, input middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
	environment := handler.environment
	attempts := environment.retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var (
		output   middleware.InitializeOutput
		metadata middleware.Metadata
		err      error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		if environment.limiter != nil {
			if waitErr := environment.limiter.Wait(ctx, environment.account, environment.region); waitErr != nil {
				if !errors.Is(waitErr, limit.ErrBackoff) {
					return output, metadata, waitErr
				}
				err = errLocalBackoff
				if attempt+1 < attempts && environment.pause(ctx, time.Duration(attempt+1)*2*time.Second) == nil {
					continue
				}
				return output, metadata, err
			}
		}
		output, metadata, err = next.HandleInitialize(ctx, input)
		if err != nil && isThrottle(err) {
			pause := 2 * time.Second
			if environment.limiter != nil {
				pause = environment.limiter.Throttled(environment.account, environment.region)
			}
			if attempt+1 < attempts && environment.pause(ctx, pause) == nil {
				continue
			}
			return output, metadata, err
		}
		if environment.limiter != nil {
			environment.limiter.Succeeded(environment.account, environment.region)
		}
		return output, metadata, err
	}
	return output, metadata, err
}

func (environment env) pause(ctx context.Context, duration time.Duration) error {
	if environment.sleep != nil {
		return environment.sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isThrottle(err error) bool {
	if errors.Is(err, errLocalBackoff) {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException",
			"SlowDown", "RequestThrottled", "RequestThrottledException", "PriorRequestNotComplete":
			return true
		}
	}
	return false
}

// errorCode maps a failure onto the closed vocabulary. Provider text is read
// only to tell an expired token from a disabled region, and is never kept or
// returned.
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if isThrottle(err) {
		return codeThrottledWord
	}
	if credentialsRejected(err) {
		return awsauth.ErrNoServerIdentity.Error()
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "AuthorizationError",
			"AuthorizationErrorException", "NotAuthorized", "Forbidden", "MissingAuthenticationTokenException",
			"UnauthorizedException":
			return CodeDenied
		case "SubscriptionRequiredException":
			return CodeNotOnPlan
		case "AuthFailure", "OptInRequired", "UnrecognizedClientException", "InvalidClientTokenId",
			"RegionDisabledException", "RegionDisabled":
			return awsauth.ErrRegionDisabled.Error()
		case "RequestTimeout", "RequestTimeoutException":
			return awsauth.ErrTimeout.Error()
		}
	}
	if awsauth.ServiceNotInRegion(err) {
		return CodeNotInRegion
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return awsauth.ErrTimeout.Error()
	}
	if code := awsauth.ErrorCode(err); code != "" {
		return code
	}
	return awsauth.ErrNetwork.Error()
}

// credentialsRejected reports a failure of the server's credentials rather
// than of a permission or a region: AWS rejected an expired, revoked or
// malformed token or key, or the signer could not load credentials at all.
// It reads as no_server_identity, the code the health check gives the same
// condition. An unrecognised token is ambiguous (AWS answers it for a
// disabled opt-in region too), so only an explicitly expired one counts.
func credentialsRejected(err error) bool {
	var signing *v4.SigningError
	if errors.As(err, &signing) || errors.Is(err, awsauth.ErrNoServerIdentity) {
		return true
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.ErrorCode() {
	case "ExpiredToken", "ExpiredTokenException", "InvalidAccessKeyId", "SignatureDoesNotMatch", "MissingAuthenticationToken":
		return true
	case "UnrecognizedClientException", "InvalidClientTokenId", "AuthFailure":
		return strings.Contains(strings.ToLower(apiError.ErrorMessage()), "expired")
	}
	return false
}

// apiErrorCode returns the provider error code without its message, for the
// few "not configured" answers that are data rather than failures.
func apiErrorCode(err error) string {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode()
	}
	return ""
}

// pinnedHTTPClient rejects any request that leaves the expected host and caps
// the response body.
type pinnedHTTPClient struct {
	client aws.HTTPClient
	host   string
}

func (pinned pinnedHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != pinned.host {
		return nil, awsauth.ErrNetwork
	}
	response, err := pinned.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxPageBytes {
		response.Body.Close()
		return nil, awsauth.ErrNetwork
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPageBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxPageBytes {
		return nil, awsauth.ErrNetwork
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
