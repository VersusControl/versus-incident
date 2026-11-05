// Package awsauth resolves the AWS identity Versus uses to read a customer
// account. It hardens the ambient credential chain (no process, SSO, or
// endpoint overrides), validates role and region identifiers, and assumes a
// customer reader role with an external ID. Failures collapse into a closed
// vocabulary so callers can render fix text without echoing provider messages.
package awsauth

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

const (
	defaultSessionDuration = 15 * time.Minute
	minimumSessionDuration = 15 * time.Minute
	maximumSessionDuration = 12 * time.Hour
	requestTimeout         = 10 * time.Second
)

// RoleSpec describes the reader role Versus assumes in a customer account.
type RoleSpec struct {
	RoleARN     string
	ExternalID  string
	SessionName string
	Duration    time.Duration
}

var (
	// ErrNoServerIdentity means the host has no usable AWS identity or its
	// credential configuration was rejected.
	ErrNoServerIdentity = errors.New("no_server_identity")
	// ErrRoleNotFound means the reader role does not exist in the account.
	ErrRoleNotFound = errors.New("role_not_found")
	// ErrTrustDenied means the role trust policy refused the Versus principal.
	ErrTrustDenied = errors.New("trust_denied")
	// ErrExternalIDMismatch means the presented external ID is not accepted.
	ErrExternalIDMismatch = errors.New("external_id_mismatch")
	// ErrSessionDurationInvalid means the requested session lifetime is out of
	// the range AWS accepts.
	ErrSessionDurationInvalid = errors.New("session_duration_invalid")
	// ErrRegionDisabled means the region is disabled or unavailable for the
	// account.
	ErrRegionDisabled = errors.New("region_disabled")
	// ErrThrottled means AWS asked Versus to slow down.
	ErrThrottled = errors.New("throttled")
	// ErrTimeout means the request exceeded its deadline.
	ErrTimeout = errors.New("timeout")
	// ErrNetwork means the request did not reach AWS or the reply could not be
	// understood.
	ErrNetwork = errors.New("network")
)

// errorTargets is the closed vocabulary, in precedence order.
var errorTargets = []error{
	ErrNoServerIdentity,
	ErrRoleNotFound,
	ErrTrustDenied,
	ErrExternalIDMismatch,
	ErrSessionDurationInvalid,
	ErrRegionDisabled,
	ErrThrottled,
	ErrTimeout,
	ErrNetwork,
}

// ErrorCode returns the closed-vocabulary code for a failure, or "" when the
// error is not one of the known conditions. It never returns provider text.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	for _, target := range errorTargets {
		if errors.Is(err, target) {
			return target.Error()
		}
	}
	return ""
}

var (
	roleARNPattern = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::([0-9]{12}):role(?:/[A-Za-z0-9+=,.@_-]+)+$`)
	regionPattern  = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]+$`)
	externalIDMax  = 1224
	sessionNameMax = 64
)

// ValidateRoleARN accepts the three commercial partitions, a 12-digit account,
// and a non-empty role path. Malformed input is rejected without echoing it.
func ValidateRoleARN(value string) error {
	if len(value) > 2048 || !roleARNPattern.MatchString(value) {
		return errors.New("invalid role ARN")
	}
	return nil
}

// ValidateRegion accepts a commercial region name. Partition-specific regions
// such as ISO are not supported.
func ValidateRegion(value string) error {
	if len(value) > 32 || !regionPattern.MatchString(value) {
		return errors.New("invalid region")
	}
	return nil
}

func partitionForRegion(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}

func partitionForRoleARN(roleARN string) string {
	parts := strings.SplitN(roleARN, ":", 4)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

func validateRoleRegion(roleARN, region string) error {
	if partitionForRoleARN(roleARN) != partitionForRegion(region) {
		return errors.New("role ARN partition does not match region")
	}
	return nil
}

func validSessionName(value string) bool {
	if value == "" || len(value) > sessionNameMax {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			strings.ContainsRune("+=,.@-_", character):
		default:
			return false
		}
	}
	return true
}

func roleSessionName(spec RoleSpec) string {
	if validSessionName(spec.SessionName) {
		return spec.SessionName
	}
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	var builder strings.Builder
	builder.WriteString("versus")
	for _, character := range host {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			strings.ContainsRune("+=,.@-_", character):
			builder.WriteRune(character)
		default:
			builder.WriteByte('-')
		}
	}
	name := strings.TrimRight(builder.String(), "-")
	if len(name) > sessionNameMax {
		name = name[:sessionNameMax]
	}
	return name
}

func validExternalID(value string) bool {
	if len(value) < 2 || len(value) > externalIDMax {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func roleDuration(value time.Duration) time.Duration {
	if value == 0 {
		return defaultSessionDuration
	}
	return value
}

func validateRoleSpec(spec RoleSpec, region string) error {
	if err := ValidateRoleARN(spec.RoleARN); err != nil {
		return err
	}
	if err := ValidateRegion(region); err != nil {
		return err
	}
	if err := validateRoleRegion(spec.RoleARN, region); err != nil {
		return err
	}
	duration := roleDuration(spec.Duration)
	if duration < minimumSessionDuration || duration > maximumSessionDuration || duration%time.Second != 0 {
		return ErrSessionDurationInvalid
	}
	if spec.ExternalID != "" && !validExternalID(spec.ExternalID) {
		return ErrExternalIDMismatch
	}
	if spec.SessionName != "" && !validSessionName(spec.SessionName) {
		return errors.New("invalid session name")
	}
	return nil
}

// NewHTTPClient returns the pinned credential and STS transport: no ambient
// proxy, TLS 1.2 minimum, a bounded timeout, and no redirect following.
func NewHTTPClient() aws.HTTPClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func stsEndpoint(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "https://sts." + region + ".amazonaws.com.cn"
	}
	return "https://sts." + region + ".amazonaws.com"
}

// stsAPI is the subset of the STS client this package uses. Tests inject a
// fake to assert the request shape without touching the network.
type stsAPI interface {
	stscreds.AssumeRoleAPIClient
	stscreds.AssumeRoleWithWebIdentityAPIClient
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

func newSTSClient(region string, provider aws.CredentialsProvider, client aws.HTTPClient) stsAPI {
	endpoint := stsEndpoint(region)
	return sts.New(sts.Options{
		Region:           region,
		Credentials:      provider,
		HTTPClient:       client,
		BaseEndpoint:     &endpoint,
		RetryMaxAttempts: 1,
	})
}

// mappedProvider translates provider failures into the closed vocabulary so a
// caller never sees AWS text.
type mappedProvider struct{ provider aws.CredentialsProvider }

func (provider mappedProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	credentials, err := provider.provider.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, classifyError(err)
	}
	return credentials, nil
}

// AssumeRole returns a provider for the customer reader role. The base
// provider is the Versus principal resolved by BaseProvider.
func AssumeRole(ctx context.Context, base aws.CredentialsProvider, spec RoleSpec, region string) (aws.CredentialsProvider, error) {
	return assumeRole(ctx, base, spec, region, NewHTTPClient())
}

func assumeRole(_ context.Context, base aws.CredentialsProvider, spec RoleSpec, region string, client aws.HTTPClient) (aws.CredentialsProvider, error) {
	if base == nil {
		return nil, ErrNoServerIdentity
	}
	if err := validateRoleSpec(spec, region); err != nil {
		return nil, err
	}
	if err := rejectAWSEndpointEnvironment(); err != nil {
		return nil, ErrNoServerIdentity
	}
	service := newSTSClient(region, base, client)
	return assumeRoleFrom(service, spec), nil
}

func assumeRoleFrom(service stscreds.AssumeRoleAPIClient, spec RoleSpec) aws.CredentialsProvider {
	return mappedProvider{aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(service, spec.RoleARN, func(options *stscreds.AssumeRoleOptions) {
		if spec.ExternalID != "" {
			options.ExternalID = aws.String(spec.ExternalID)
		}
		options.RoleSessionName = roleSessionName(spec)
		options.Duration = roleDuration(spec.Duration)
	}))}
}

// CallerIdentity returns the Versus principal ARN and its account ID.
func CallerIdentity(ctx context.Context, cfg aws.Config) (arn string, accountID string, err error) {
	if err := ValidateRegion(cfg.Region); err != nil {
		return "", "", err
	}
	if cfg.Credentials == nil {
		return "", "", ErrNoServerIdentity
	}
	if err := rejectAWSEndpointEnvironment(); err != nil {
		return "", "", ErrNoServerIdentity
	}
	client := cfg.HTTPClient
	if client == nil {
		client = NewHTTPClient()
	}
	return callerIdentity(ctx, newSTSClient(cfg.Region, cfg.Credentials, client))
}

type callerIdentityAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

func callerIdentity(ctx context.Context, service callerIdentityAPI) (string, string, error) {
	output, err := service.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", classifyError(err)
	}
	if output == nil || aws.ToString(output.Arn) == "" || len(aws.ToString(output.Account)) != 12 {
		return "", "", ErrNoServerIdentity
	}
	return aws.ToString(output.Arn), aws.ToString(output.Account), nil
}

// classifyError maps a provider or transport failure onto the closed
// vocabulary. The returned error carries only the code.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	for _, target := range errorTargets {
		if errors.Is(err, target) {
			return target
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrTimeout
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return classifyAPIError(apiError)
	}
	var netError net.Error
	if errors.As(err, &netError) {
		if netError.Timeout() {
			return ErrTimeout
		}
		return ErrNetwork
	}
	return ErrNetwork
}

func classifyAPIError(apiError smithy.APIError) error {
	code := apiError.ErrorCode()
	message := strings.ToLower(apiError.ErrorMessage())
	switch code {
	case "RegionDisabledException", "RegionDisabled":
		return ErrRegionDisabled
	case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException", "SlowDown":
		return ErrThrottled
	case "RequestTimeout", "RequestTimeoutException":
		return ErrTimeout
	case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException", "MissingAuthenticationToken", "NoCredentialProviders":
		return ErrNoServerIdentity
	case "AccessDenied", "AccessDeniedException":
		if strings.Contains(message, "external id") || strings.Contains(message, "externalid") {
			return ErrExternalIDMismatch
		}
		if strings.Contains(message, "cannot be found") || strings.Contains(message, "not found") || strings.Contains(message, "no such") {
			return ErrRoleNotFound
		}
		return ErrTrustDenied
	case "InvalidParameterValue", "ValidationError", "InvalidInput":
		if strings.Contains(message, "duration") {
			return ErrSessionDurationInvalid
		}
		return ErrTrustDenied
	}
	return ErrNetwork
}

// AssumeRoleWithHTTPClient is AssumeRole with an injected transport. Production
// callers use AssumeRole; tests point the transport at a deterministic fixture.
// It exists so a caller can prove the assume path end to end without reaching
// AWS, and it keeps the same validation and closed error vocabulary.
func AssumeRoleWithHTTPClient(ctx context.Context, base aws.CredentialsProvider, spec RoleSpec, region string, client aws.HTTPClient) (aws.CredentialsProvider, error) {
	if client == nil {
		client = NewHTTPClient()
	}
	return assumeRole(ctx, base, spec, region, client)
}
