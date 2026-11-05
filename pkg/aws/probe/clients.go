package probe

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// maxResponseBytes bounds one probe page. Every probe requests the smallest
// page AWS serves, so a larger body is a signal to stop rather than parse.
const maxResponseBytes = 512 * 1024

// regionalPrefixes maps a catalog ID to the DNS label of its regional
// endpoint. Global services are listed separately.
var regionalPrefixes = map[string]string{
	"api-gateway":    "apigateway",
	"auto-scaling":   "autoscaling",
	"cloudformation": "cloudformation",
	"cloudtrail":     "cloudtrail",
	"cloudwatch":     "monitoring",
	"config":         "config",
	"dynamodb":       "dynamodb",
	"ec2":            "ec2",
	"ecs":            "ecs",
	"eks":            "eks",
	"elasticache":    "elasticache",
	"elbv2":          "elasticloadbalancing",
	"eventbridge":    "events",
	"kinesis":        "kinesis",
	"lambda":         "lambda",
	"rds":            "rds",
	"sns":            "sns",
	"sqs":            "sqs",
	"step-functions": "states",
}

// globalHosts maps a catalog ID to its single global endpoint.
var globalHosts = map[string]string{
	"aws-health": "health.us-east-1.amazonaws.com",
	"cloudfront": "cloudfront.amazonaws.com",
	"iam":        "iam.amazonaws.com",
	"route53":    "route53.amazonaws.com",
	"s3":         "s3.amazonaws.com",
}

// endpointHost returns the exact host a probe may reach.
func endpointHost(serviceID, region string) (string, bool) {
	if host, ok := globalHosts[serviceID]; ok {
		return host, true
	}
	prefix, ok := regionalPrefixes[serviceID]
	if !ok {
		return "", false
	}
	if awsauth.ValidateRegion(region) != nil {
		return "", false
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return prefix + "." + region + "." + suffix, true
}

// env carries the account's credentials and transport into one probe.
type env struct {
	credentials aws.CredentialsProvider
	httpClient  aws.HTTPClient
	region      string
}

// config builds the client configuration without loading ambient providers.
// Credentials always come from the caller's assumed reader role.
func (environment env) config() aws.Config {
	return aws.Config{
		Region:           environment.region,
		Credentials:      environment.credentials,
		HTTPClient:       environment.httpClient,
		RetryMaxAttempts: 1,
	}
}

// pin resolves the service endpoint and wraps the transport so a call can
// only reach that exact host over TLS.
func (environment env) pin(serviceID string) (string, aws.HTTPClient, error) {
	host, ok := endpointHost(serviceID, environment.region)
	if !ok {
		return "", nil, awsauth.ErrNetwork
	}
	return "https://" + host, pinnedHTTPClient{client: environment.httpClient, host: host}, nil
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
	if response.ContentLength > maxResponseBytes {
		response.Body.Close()
		return nil, awsauth.ErrNetwork
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, awsauth.ErrNetwork
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
